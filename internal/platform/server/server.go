// Package server wires the ConnectRPC application server and the ops server.
// It is transport infrastructure only: it knows nothing about individual
// domain modules, which register themselves through the Module interface.
package server

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/otelconnect"
	"go.uber.org/zap"

	"github.com/koungkub/tehran/internal/platform/config"
	"github.com/koungkub/tehran/internal/platform/telemetry"
)

const maxRequestBytes = 4 << 20 // 4 MiB

// Module is a bounded context that exposes ConnectRPC handlers. Each module
// mounts its handlers on the shared mux and returns the fully-qualified gRPC
// service names it serves, so New can register them for health and reflection
// without depending on any specific domain package.
type Module interface {
	Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string
}

// New builds the RPC server: Connect handlers, gRPC health and reflection.
// Unencrypted HTTP/2 is enabled so plain-text gRPC clients (h2c) work.
func New(
	cfg config.Server,
	log *zap.Logger,
	tel *telemetry.Telemetry,
	modules ...Module,
) (*http.Server, error) {
	otelInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(tel.TracerProvider),
		otelconnect.WithMeterProvider(tel.MeterProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel interceptor: %w", err)
	}

	handlerOpts := []connect.HandlerOption{
		connect.WithInterceptors(newLoggingInterceptor(log), otelInterceptor),
		connect.WithRecover(newRecoverHandler(log)),
		connect.WithReadMaxBytes(maxRequestBytes),
	}

	mux := http.NewServeMux()
	var serviceNames []string
	for _, m := range modules {
		serviceNames = append(serviceNames, m.Register(mux, handlerOpts...)...)
	}

	checker := grpchealth.NewStaticChecker(serviceNames...)
	mux.Handle(grpchealth.NewHandler(checker))

	reflector := grpcreflect.NewStaticReflector(
		append(serviceNames, grpchealth.HealthV1ServiceName)...,
	)
	mux.Handle(grpcreflect.NewHandlerV1(reflector))
	mux.Handle(grpcreflect.NewHandlerV1Alpha(reflector))

	// gRPC clients speak HTTP/2 without TLS (h2c); plain net/http only
	// negotiates HTTP/2 over TLS unless unencrypted HTTP/2 is opted into.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &http.Server{
		Addr:      net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:   mux,
		Protocols: protocols,
		// No ReadTimeout: it would abort long-lived streaming RPCs.
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}
