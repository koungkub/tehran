package server

import (
	"context"
	"errors"
	"net/http"
	"runtime/debug"
	"time"

	"connectrpc.com/connect"
	"go.uber.org/zap"
)

// loggingInterceptor emits one log line per RPC: procedure, peer, duration
// and connect code.
type loggingInterceptor struct {
	log *zap.Logger
}

var _ connect.Interceptor = (*loggingInterceptor)(nil)

func newLoggingInterceptor(log *zap.Logger) *loggingInterceptor {
	return &loggingInterceptor{log: log}
}

func (i *loggingInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		res, err := next(ctx, req)
		i.logResult(req.Spec().Procedure, req.Peer().Addr, time.Since(start), err)
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
		i.logResult(conn.Spec().Procedure, conn.Peer().Addr, time.Since(start), err)
		return err
	}
}

func (i *loggingInterceptor) logResult(procedure, peer string, duration time.Duration, err error) {
	fields := []zap.Field{
		zap.String("procedure", procedure),
		zap.String("peer", peer),
		zap.Duration("duration", duration),
	}
	if err != nil {
		fields = append(fields, zap.String("code", connect.CodeOf(err).String()), zap.Error(err))
		i.log.Warn("rpc", fields...)
		return
	}
	i.log.Info("rpc", append(fields, zap.String("code", "ok"))...)
}

// newRecoverHandler turns a panicking RPC into CodeInternal instead of
// killing the process, logging the panic value and stack.
func newRecoverHandler(log *zap.Logger) func(context.Context, connect.Spec, http.Header, any) error {
	return func(_ context.Context, spec connect.Spec, _ http.Header, recovered any) error {
		log.Error("rpc panic",
			zap.String("procedure", spec.Procedure),
			zap.Any("panic", recovered),
			zap.ByteString("stack", debug.Stack()),
		)
		return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
}
