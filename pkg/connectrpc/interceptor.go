package connectrpc

import (
	"context"
	"errors"
	"net/http"
	"os"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"github.com/rs/zerolog"
)

// loggingInterceptor emits one log line per RPC: procedure, peer, duration and
// connect code.
type loggingInterceptor struct {
	log *zerolog.Logger
}

var _ connect.Interceptor = (*loggingInterceptor)(nil)

func newLoggingInterceptor(log *zerolog.Logger) *loggingInterceptor {
	return &loggingInterceptor{log: log}
}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		res, err := next(ctx, req)
		i.logResult(ctx, req.Spec().Procedure, req.Peer().Addr, time.Since(start), err)
		return res, err
	}
}

func (i *loggingInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *loggingInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		start := time.Now()
		err := next(ctx, conn)
		i.logResult(ctx, conn.Spec().Procedure, conn.Peer().Addr, time.Since(start), err)
		return err
	}
}

// logResult takes ctx and passes it to the logger: that is what lets a
// correlating handler stamp the request's trace and span ids onto the line.
func (i *loggingInterceptor) logResult(
	ctx context.Context,
	procedure, peer string,
	duration time.Duration,
	err error,
) {
	// Tell the surrounding middleware this request is accounted for, so it does
	// not report it a second time as a rejection.
	markAccounted(ctx)

	// One event, whichever way this went: the level is chosen first because
	// creating a second one would emit a second line.
	e := i.log.Info()
	if err != nil {
		e = i.log.Warn()
	}
	e.Ctx(ctx).
		Str("procedure", procedure).
		Str("peer", peer).
		Dur("duration", duration)
	if err != nil {
		e.Str("code", codeOf(err).String()).Str("error", err.Error())
	} else {
		e.Str("code", "ok")
	}
	e.Msg("rpc")
}

// codeOf classifies an RPC error the way Connect's protocol layer eventually
// will.
//
// connect.CodeOf reports CodeUnknown for a bare context.Canceled or
// context.DeadlineExceeded. Connect does map those to CodeCanceled and
// CodeDeadlineExceeded for the response, but in its protocol layer — which runs
// only after the whole interceptor chain has returned — and the helper that does
// it is unexported. Calling CodeOf here would therefore log every client
// disconnect and every timeout as "unknown" while the caller correctly receives
// "canceled" or "deadline_exceeded".
func codeOf(err error) connect.Code {
	// An error that already carries a code keeps it: a handler that deliberately
	// returned, say, CodeInvalidArgument wrapping a cancellation meant the former.
	// This mirrors the already-wrapped check in connect's own wrapIfContextError.
	if coded, ok := errors.AsType[*connect.Error](err); ok {
		return coded.Code()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.CodeCanceled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return connect.CodeDeadlineExceeded
	}
	return connect.CodeOf(err)
}

// timeoutInterceptor bounds how long a unary handler may run when the client
// did not ask for a deadline of its own.
//
// It exists because the connection-level timeouts cannot cover a hung handler:
// the peer is healthy so it answers keepalive PINGs, a stream is open so
// IdleTimeout cannot fire, and a handler that has written nothing has no data
// available to write, so WriteByteTimeout never starts counting. A context
// deadline is also the better instrument than net/http's WriteTimeout would be,
// because it cancels the handler's work and returns a real CodeDeadlineExceeded
// to the caller rather than tearing the connection down underneath it.
type timeoutInterceptor struct {
	timeout time.Duration
}

var _ connect.Interceptor = (*timeoutInterceptor)(nil)

func newTimeoutInterceptor(timeout time.Duration) *timeoutInterceptor {
	return &timeoutInterceptor{timeout: timeout}
}

func (i *timeoutInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		// A deadline the client sent always wins: Connect has already turned
		// Connect-Timeout-Ms or grpc-timeout into one by this point, and this is
		// only a backstop for callers that set none.
		if _, ok := ctx.Deadline(); ok {
			return next(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, i.timeout)
		defer cancel()
		return next(ctx, req)
	}
}

func (i *timeoutInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler deliberately imposes nothing: a stream is meant to run
// long, so a total-duration deadline would eventually end a healthy one.
func (i *timeoutInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// newRecoverHandler turns a panicking RPC into CodeInternal instead of killing
// the process, logging the panic value and stack.
func newRecoverHandler(log *zerolog.Logger) func(context.Context, connect.Spec, http.Header, any) error {
	return func(ctx context.Context, spec connect.Spec, _ http.Header, recovered any) error {
		log.Error().Ctx(ctx).
			Str("procedure", spec.Procedure).
			Interface("panic", recovered).
			Str("stack", string(debug.Stack())).
			Msg("rpc panic")
		return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
}
