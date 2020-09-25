package runner

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/dynamic"
	"github.com/jhump/protoreflect/dynamic/grpcdynamic"
	"go.uber.org/multierr"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/metadata"
)

// ConditionChecker tells worker whether to run
type ConditionChecker func(string, error, int, time.Duration) bool

// Worker is used for doing a single stream of requests in parallel
type Worker struct {
	stub grpcdynamic.Stub
	mtd  *desc.MethodDescriptor

	config *RunConfig

	reqCounter *int64
	workerID   string

	// cached messages only for binary
	cachedMessages []*dynamic.Message

	// non-binary json optimization
	arrayJSONData []string

	done chan bool

	mu     sync.RWMutex
	active bool
}

func (w *Worker) isActive() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.active
}

func (w *Worker) setActive(v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = v
}

func (w *Worker) runWorker(cond ConditionChecker, stopOnCond bool) error {
	var err, rErr error

	start := time.Now()
	n := 0

	for {
		select {
		case <-w.done:
			return nil
		default:
			if cond(w.workerID, rErr, n, time.Since(start)) {
				reqNum := atomic.AddInt64(w.reqCounter, 1)
				n++

				rErr := w.makeRequest(reqNum)

				err = multierr.Append(err, rErr)
			} else if stopOnCond {
				return err
			}
		}
	}
}

func (w *Worker) runWorkerAsync(cond ConditionChecker, stopOnCond bool) error {

	start := time.Now()
	n := 0
	wc := 0
	g := new(errgroup.Group)

	for {
		select {
		case <-w.done:
			return g.Wait()
		default:
			if cond(w.workerID, nil, n, time.Since(start)) {
				reqNum := atomic.AddInt64(w.reqCounter, 1)
				n++
				wc++
				g.Go(func() error {
					return w.makeRequest(reqNum)
				})
			} else if stopOnCond {
				return g.Wait()
			}
		}
	}
}

func (w *Worker) makeRequest(reqNum int64) error {

	ctd := newCallTemplateData(w.mtd, w.config.funcs, w.workerID, reqNum)

	var inputs []*dynamic.Message
	var err error

	// try the optimized path for JSON data for non client-streaming
	if !w.config.binary && !w.mtd.IsClientStreaming() && len(w.arrayJSONData) > 0 {
		indx := int((reqNum - 1) % int64(len(w.arrayJSONData))) // we want to start from inputs[0] so dec reqNum
		if inputs, err = w.getMessages(ctd, []byte(w.arrayJSONData[indx])); err != nil {
			return err
		}
	} else {
		if inputs, err = w.getMessages(ctd, w.config.data); err != nil {
			return err
		}
	}

	mdMap, err := ctd.executeMetadata(string(w.config.metadata))
	if err != nil {
		return err
	}

	var reqMD *metadata.MD
	if len(mdMap) > 0 {
		md := metadata.New(mdMap)
		reqMD = &md
	} else {
		reqMD = &metadata.MD{}
	}

	if w.config.enableCompression {
		reqMD.Append("grpc-accept-encoding", gzip.Name)
	}

	ctx := context.Background()
	var cancel context.CancelFunc

	if w.config.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, w.config.timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	// include the metadata
	if reqMD != nil {
		ctx = metadata.NewOutgoingContext(ctx, *reqMD)
	}

	var callType string
	if w.config.hasLog {
		callType = "unary"
		if w.mtd.IsClientStreaming() && w.mtd.IsServerStreaming() {
			callType = "bidi"
		} else if w.mtd.IsServerStreaming() {
			callType = "server-streaming"
		} else if w.mtd.IsClientStreaming() {
			callType = "client-streaming"
		}

		w.config.log.Debugw("Making request", "workerID", w.workerID,
			"call type", callType, "call", w.mtd.GetFullyQualifiedName(),
			"input", inputs, "metadata", reqMD)
	}

	inputsLen := len(inputs)
	if inputsLen == 0 {
		return fmt.Errorf("no data provided for request")
	}
	inputIdx := int((reqNum - 1) % int64(inputsLen)) // we want to start from inputs[0] so dec reqNum
	unaryInput := inputs[inputIdx]

	// RPC errors are handled via stats handler
	if w.mtd.IsClientStreaming() && w.mtd.IsServerStreaming() {
		_ = w.makeBidiRequest(&ctx, inputs)
	} else if w.mtd.IsClientStreaming() {
		_ = w.makeClientStreamingRequest(&ctx, inputs)
	} else if w.mtd.IsServerStreaming() {
		_ = w.makeServerStreamingRequest(&ctx, unaryInput)
	} else {
		_ = w.makeUnaryRequest(&ctx, reqMD, unaryInput)
	}

	return err
}

func (w *Worker) getMessages(ctd *callTemplateData, inputData []byte) ([]*dynamic.Message, error) {
	var inputs []*dynamic.Message

	if w.cachedMessages != nil {
		return w.cachedMessages, nil
	}

	if !w.config.binary {
		data, err := ctd.executeData(string(inputData))
		if err != nil {
			return nil, err
		}
		inputs, err = createPayloadsFromJSON(string(data), w.mtd)
		if err != nil {
			return nil, err
		}
		// Json messages are not cached due to templating
	} else {
		var err error
		inputs, err = createPayloadsFromBin(inputData, w.mtd)
		if err != nil {
			return nil, err
		}

		w.cachedMessages = inputs
	}

	return inputs, nil
}

func (w *Worker) makeClientStreamingRequest(ctx *context.Context, input []*dynamic.Message) error {
	var str *grpcdynamic.ClientStream
	var err error
	var callOptions = []grpc.CallOption{}
	if w.config.enableCompression {
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
	}
	str, err = w.stub.InvokeRpcClientStream(*ctx, w.mtd, callOptions...)

	if err != nil && w.config.hasLog {
		w.config.log.Errorw("Invoke Client Streaming RPC call error: "+err.Error(), "workerID", w.workerID,
			"call type", "client-streaming",
			"call", w.mtd.GetFullyQualifiedName(), "error", err)
	}

	counter := 0

	for err == nil {
		inputLen := len(input)
		if input == nil || inputLen == 0 {
			res, closeErr := str.CloseAndReceive()

			if w.config.hasLog {
				w.config.log.Debugw("Close and receive", "workerID", w.workerID, "call type", "client-streaming",
					"call", w.mtd.GetFullyQualifiedName(),
					"response", res, "error", closeErr)
			}

			break
		}

		if counter == inputLen {
			res, closeErr := str.CloseAndReceive()

			if w.config.hasLog {
				w.config.log.Debugw("Close and receive", "workerID", w.workerID, "call type", "client-streaming",
					"call", w.mtd.GetFullyQualifiedName(),
					"response", res, "error", closeErr)
			}

			break
		}

		payload := input[counter]

		var wait <-chan time.Time
		if w.config.streamInterval > 0 {
			wait = time.Tick(w.config.streamInterval)
			<-wait
		}

		err = str.SendMsg(payload)

		if w.config.hasLog {
			w.config.log.Debugw("Send message", "workerID", w.workerID, "call type", "client-streaming",
				"call", w.mtd.GetFullyQualifiedName(),
				"payload", payload, "error", err)
		}

		if err == io.EOF {
			// We get EOF on send if the server says "go away"
			// We have to use CloseAndReceive to get the actual code
			res, closeErr := str.CloseAndReceive()

			if w.config.hasLog {
				w.config.log.Debugw("Close and receive", "workerID", w.workerID, "call type", "client-streaming",
					"call", w.mtd.GetFullyQualifiedName(),
					"response", res, "error", closeErr)
			}

			break
		}
		counter++
	}
	return nil
}

func (w *Worker) makeServerStreamingRequest(ctx *context.Context, input *dynamic.Message) error {
	var callOptions = []grpc.CallOption{}
	if w.config.enableCompression {
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
	}
	str, err := w.stub.InvokeRpcServerStream(*ctx, w.mtd, input, callOptions...)

	if err != nil && w.config.hasLog {
		w.config.log.Errorw("Invoke Server Streaming RPC call error: "+err.Error(), "workerID", w.workerID,
			"call type", "server-streaming",
			"call", w.mtd.GetFullyQualifiedName(),
			"input", input, "error", err)
	}

	for err == nil {
		res, err := str.RecvMsg()

		if w.config.hasLog {
			w.config.log.Debugw("Receive message", "workerID", w.workerID, "call type", "server-streaming",
				"call", w.mtd.GetFullyQualifiedName(),
				"response", res, "error", err)
		}

		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
	}

	return err
}

func (w *Worker) makeUnaryRequest(ctx *context.Context, reqMD *metadata.MD, input *dynamic.Message) error {
	var res proto.Message
	var resErr error
	var callOptions = []grpc.CallOption{}
	if w.config.enableCompression {
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
	}

	res, resErr = w.stub.InvokeRpc(*ctx, w.mtd, input, callOptions...)

	if w.config.hasLog {
		w.config.log.Debugw("Received response", "workerID", w.workerID, "call type", "unary",
			"call", w.mtd.GetFullyQualifiedName(),
			"input", input, "metadata", reqMD,
			"response", res, "error", resErr)
	}

	return resErr
}

func (w *Worker) makeBidiRequest(ctx *context.Context, input []*dynamic.Message) error {
	var str *grpcdynamic.BidiStream
	var err error
	var callOptions = []grpc.CallOption{}
	if w.config.enableCompression {
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
	}
	str, err = w.stub.InvokeRpcBidiStream(*ctx, w.mtd, callOptions...)

	if err != nil {
		if w.config.hasLog {
			w.config.log.Errorw("Invoke Bidi RPC call error: "+err.Error(),
				"workerID", w.workerID, "call type", "bidi",
				"call", w.mtd.GetFullyQualifiedName(), "error", err)
		}

		return err
	}

	counter := 0

	inputLen := len(input)

	recvDone := make(chan bool)

	if input == nil || inputLen == 0 {
		closeErr := str.CloseSend()

		if w.config.hasLog {
			w.config.log.Debugw("Close send", "workerID", w.workerID, "call type", "bidi",
				"call", w.mtd.GetFullyQualifiedName(), "error", closeErr)
		}

		return nil
	}

	go func() {
		for {
			res, err := str.RecvMsg()

			if w.config.hasLog {
				w.config.log.Debugw("Receive message", "workerID", w.workerID, "call type", "bidi",
					"call", w.mtd.GetFullyQualifiedName(),
					"response", res, "error", err)
			}

			if err != nil {
				close(recvDone)
				break
			}
		}
	}()

	for err == nil {
		if counter == inputLen {
			closeErr := str.CloseSend()

			if w.config.hasLog {
				w.config.log.Debugw("Close send", "workerID", w.workerID, "call type", "bidi",
					"call", w.mtd.GetFullyQualifiedName(), "error", closeErr)
			}

			break
		}

		payload := input[counter]

		var wait <-chan time.Time
		if w.config.streamInterval > 0 {
			wait = time.Tick(w.config.streamInterval)
			<-wait
		}

		err = str.SendMsg(payload)
		counter++

		if w.config.hasLog {
			w.config.log.Debugw("Send message", "workerID", w.workerID, "call type", "bidi",
				"call", w.mtd.GetFullyQualifiedName(),
				"payload", payload, "error", err)
		}
	}

	if err == nil {
		<-recvDone
	}

	return nil
}
