// Package connectrpc wires a ConnectRPC application server. One server speaks
// the Connect, gRPC, and gRPC-Web protocols on a single port, so plain HTTP
// clients and gRPC clients are both served without a second listener.
//
// It is transport infrastructure only: it knows nothing about individual domain
// modules, which register themselves through the Module interface. Telemetry
// arrives as the OpenTelemetry provider interfaces rather than as a concrete
// setup, so this package stays independent of how tracing and metrics are
// configured.
package connectrpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"connectrpc.com/otelconnect"
)

// Defaults substituted by New for zero-valued Config fields.
const (
	DefaultMaxRequestBytes   = 4 << 20 // 4 MiB
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultShutdownTimeout   = 10 * time.Second
)

// DefaultName labels the server in logs and to a supervisor. Override it with
// WithName when a process runs more than one.
const DefaultName = "rpc"

// Config describes the server's listener and limits.
//
// The mapstructure tags are inert metadata: they let a viper-based service nest
// this struct directly into its own configuration without a conversion layer.
type Config struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	// ShutdownTimeout bounds how long Serve waits for in-flight requests to
	// drain once its context is cancelled.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// MaxRequestBytes caps the decompressed size of a request message.
	MaxRequestBytes int `mapstructure:"max_request_bytes"`
	// ReadHeaderTimeout bounds how long a client may take to send its headers.
	// There is deliberately no read timeout: it would abort streaming RPCs.
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.MaxRequestBytes <= 0 {
		c.MaxRequestBytes = DefaultMaxRequestBytes
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	return c
}

// Module is a bounded context that exposes ConnectRPC handlers. Each module
// mounts its handlers on the shared mux and returns the fully-qualified gRPC
// service names it serves, so New can register them for health and reflection
// without depending on any specific domain package.
//
// A module satisfies this interface structurally; it never needs to import this
// package.
type Module interface {
	Register(mux *http.ServeMux, opts ...connect.HandlerOption) []string
}

// Server is a ConnectRPC server that owns its own lifecycle. It satisfies
// lifecycle.Component, so a supervisor can sequence it against other components
// without this package importing one.
type Server struct {
	http            *http.Server
	log             *slog.Logger
	name            string
	shutdownTimeout time.Duration
}

// New builds the RPC server: the modules' Connect handlers plus gRPC health and
// reflection. Unencrypted HTTP/2 is enabled so plain-text gRPC clients (h2c)
// work. Every option has a working default, so New(cfg, WithModules(m)) is a
// complete call.
func New(cfg Config, opts ...Option) (*Server, error) {
	cfg = cfg.withDefaults()
	o := newOptions(opts)

	otelInterceptor, err := otelconnect.NewInterceptor(
		otelconnect.WithTracerProvider(o.tracerProvider),
		otelconnect.WithMeterProvider(o.meterProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("create otel interceptor: %w", err)
	}

	// Connect makes the first interceptor in the slice the outermost one, so
	// order matters twice over here. The otel interceptor must come before the
	// logging one: it is what puts the request's span on the context, and the
	// logging interceptor can only stamp trace_id and span_id onto its line if
	// it sees that context. Built-ins come before a caller's, so their work is
	// covered by the log line and the span.
	interceptors := append(
		[]connect.Interceptor{otelInterceptor, newLoggingInterceptor(o.log)},
		o.interceptors...,
	)
	handlerOpts := []connect.HandlerOption{
		connect.WithInterceptors(interceptors...),
		connect.WithRecover(newRecoverHandler(o.log)),
		connect.WithReadMaxBytes(cfg.MaxRequestBytes),
	}

	mux := http.NewServeMux()
	var serviceNames []string
	for _, m := range o.modules {
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

	return &Server{
		log:             o.log,
		name:            o.name,
		shutdownTimeout: cfg.ShutdownTimeout,
		http: &http.Server{
			Addr:      net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
			Handler:   mux,
			Protocols: protocols,
			// No ReadTimeout: it would abort long-lived streaming RPCs.
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		},
	}, nil
}

// Serve listens and blocks until the server stops. Cancelling ctx starts a
// graceful shutdown bounded by Config.ShutdownTimeout, after which Serve
// returns; a clean stop returns nil.
//
// Together with Name this satisfies lifecycle.Component, so a supervisor can
// sequence this server against others; running it directly on a cancellable
// context works just as well.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.LogAttrs(ctx, slog.LevelInfo, s.name+" server listening",
			slog.String("addr", s.Addr()))
		err := s.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("%s server: %w", s.name, err)
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err // Failed to bind, or stopped by a direct Shutdown call.
	case <-ctx.Done():
	}

	s.log.LogAttrs(ctx, slog.LevelInfo, s.name+" server shutting down",
		slog.Duration("timeout", s.shutdownTimeout))
	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to drain within.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	return errors.Join(s.Shutdown(shutdownCtx), <-errCh)
}

// Shutdown stops the server, draining in-flight requests until ctx is done.
// Serve calls it on cancellation, so call it directly only when driving the
// lifecycle by hand.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown %s server: %w", s.name, err)
	}
	return nil
}

// Name identifies the server to a supervisor and labels its log lines.
func (s *Server) Name() string { return s.name }

// Handler returns the mux with every module, health and reflection handler
// mounted. It is the seam for tests, which can serve it through httptest
// instead of binding a port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.http.Addr }
