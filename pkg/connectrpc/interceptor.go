package connectrpc

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
)

// loggingInterceptor emits one log line per RPC: procedure, peer, duration and
// connect code.
type loggingInterceptor struct {
	log *slog.Logger
}

var _ connect.Interceptor = (*loggingInterceptor)(nil)

func newLoggingInterceptor(log *slog.Logger) *loggingInterceptor {
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
	attrs := []slog.Attr{
		slog.String("procedure", procedure),
		slog.String("peer", peer),
		slog.Duration("duration", duration),
	}
	if err != nil {
		attrs = append(attrs,
			slog.String("code", connect.CodeOf(err).String()),
			slog.String("error", err.Error()),
		)
		i.log.LogAttrs(ctx, slog.LevelWarn, "rpc", attrs...)
		return
	}
	i.log.LogAttrs(ctx, slog.LevelInfo, "rpc", append(attrs, slog.String("code", "ok"))...)
}

// newRecoverHandler turns a panicking RPC into CodeInternal instead of killing
// the process, logging the panic value and stack.
func newRecoverHandler(log *slog.Logger) func(context.Context, connect.Spec, http.Header, any) error {
	return func(ctx context.Context, spec connect.Spec, _ http.Header, recovered any) error {
		log.LogAttrs(ctx, slog.LevelError, "rpc panic",
			slog.String("procedure", spec.Procedure),
			slog.Any("panic", recovered),
			slog.String("stack", string(debug.Stack())),
		)
		return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
}
