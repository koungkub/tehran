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
//
// DefaultRequestTimeout and DefaultShutdownTimeout are chosen together, not
// independently: see the invariant documented on Config.ShutdownTimeout. The
// chain 15s < 20s, and 20s + ops' 5s < lifecycle's 30s, is what makes a SIGTERM
// during ordinary traffic drain cleanly and exit zero.
const (
	DefaultMaxRequestBytes   = 4 << 20 // 4 MiB
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultRequestTimeout    = 15 * time.Second
	DefaultShutdownTimeout   = 20 * time.Second
	DefaultIdleTimeout       = 120 * time.Second
	DefaultKeepaliveInterval = 30 * time.Second
	DefaultWriteByteTimeout  = 30 * time.Second
	// DefaultMaxConcurrentStreams matches the standard library's own current
	// default, so setting it changes nothing today; it is pinned explicitly only
	// so that a change to that default cannot silently move this server's limit.
	// The stdlib source carries a TODO about lowering it to 100.
	DefaultMaxConcurrentStreams = 250
)

// DefaultName labels the server in logs and to a supervisor. Override it with
// WithName when a process runs more than one.
const DefaultName = "rpc"

// Config describes the server's listener and limits.
//
// The mapstructure tags are inert metadata: they let a viper-based service nest
// this struct directly into its own configuration without a conversion layer.
//
// The timeouts here follow one rule: a timeout that bounds a total duration
// eventually kills a healthy stream, so on a server that may speak streaming
// RPCs the safe ones are those that bound a *lack of progress*. That is why
// net/http's WriteTimeout is never set — it covers the whole ServeHTTP lifetime,
// a streaming response included — and why its protective role is taken by
// WriteByteTimeout, KeepaliveInterval and IdleTimeout instead.
//
// Two gaps that leaves are covered by exceptions to that rule, because nothing
// progress-based can reach them: RequestTimeout for a handler that hangs, and
// ReadTimeout for a body that never finishes arriving. Both are scoped so they
// cannot end a healthy stream — RequestTimeout skips streaming handlers, and
// ReadTimeout is opt-in because it is unsafe for client-streaming specifically.
type Config struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	// ShutdownTimeout bounds how long Serve waits for in-flight requests to
	// drain once its context is cancelled.
	//
	// It must cover the longest a request can legitimately take, or the drain is
	// not graceful: http.Server.Shutdown only closes connections once they go
	// idle and does not cancel request contexts, so exceeding this timeout means
	// Serve returns an error while handlers are still running, and the process
	// exits non-zero on an ordinary deploy. The whole chain to respect is
	//
	//	ReadTimeout + RequestTimeout <= ShutdownTimeout
	//	ShutdownTimeout + (other components' drains) <= supervisor's timeout
	//	supervisor's timeout <= the orchestrator's grace period
	//
	// The first line is additive because the two phases are sequential: Connect
	// reads and decodes the request message, and only then runs the handler.
	//
	// A streaming RPC in mid-flight never goes idle on its own, so a server with
	// long-lived streams must raise this or cancel those streams itself.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// MaxRequestBytes caps the decompressed size of a request message.
	MaxRequestBytes int `mapstructure:"max_request_bytes"`
	// ReadHeaderTimeout bounds how long a client may take to send its headers.
	//
	// It is an HTTP/1.1 guard only. Go's HTTP/2 server never reads this field —
	// it uses its own hardcoded handshake timeouts instead — so it does not
	// protect the h2c path that gRPC clients use. See ReadTimeout for the
	// equivalent there.
	//
	// Keep it at or below ReadTimeout whenever that one is on. net/http arms the
	// header phase with this value and swaps to the ReadTimeout deadline only once
	// the headers are read, so a larger value here lets the header phase outlive
	// the whole-request budget and leaves the body phase starting on a deadline
	// that has already passed.
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
	// ReadTimeout bounds how long one request's body may take to arrive. It is
	// off by default, and it is the only setting here that is not safe for every
	// service, so it is opt-in.
	//
	// Nothing else covers the body-read phase. Connect decodes the request
	// message before the interceptor chain runs, so RequestTimeout's clock has
	// not started yet; the HTTP/2 idle timer is stopped as soon as a stream
	// opens; WriteByteTimeout has nothing to write; and a peer that answers
	// keepalive PINGs while dribbling its body defeats KeepaliveInterval. Left
	// at zero, a slow-body client can pin a stream and its handler goroutine
	// indefinitely.
	//
	// It is narrower than its net/http reputation suggests. Under HTTP/2 it is
	// armed per stream after the headers are read and only closes that stream's
	// request body, so it is safe for unary and server-streaming RPCs. It is not
	// safe for client-streaming or bidi, where a long upload is the point — set
	// it only if no registered module serves one.
	ReadTimeout time.Duration `mapstructure:"read_timeout"`
	// IdleTimeout reaps a kept-alive connection that has no request in flight at
	// all. Safe with streaming for that reason: it cannot fire while a stream is
	// open.
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
	// KeepaliveInterval is how long the connection may go without a single frame
	// arriving before the server sends an HTTP/2 PING to check the peer is still
	// there. Without it, a peer that vanishes with no FIN — a partition, a
	// crashed host, a NAT dropping the flow — holds its streams and their
	// goroutines until the OS gives up on the socket, which can take hours. It
	// maps to http.HTTP2Config.SendPingTimeout; the reply deadline is that
	// struct's PingTimeout default of 15s.
	KeepaliveInterval time.Duration `mapstructure:"keepalive_interval"`
	// WriteByteTimeout closes a connection whose writes stop making progress —
	// typically a client that opened a stream and stopped reading. It is the
	// streaming-safe counterpart to WriteTimeout: the clock starts only once
	// there is data to write and is extended by every byte written, so it ends a
	// stalled response without touching a slow but advancing one.
	WriteByteTimeout time.Duration `mapstructure:"write_byte_timeout"`
	// RequestTimeout is a per-RPC deadline applied to unary handlers that arrive
	// with no deadline of their own, so a handler that hangs cannot occupy a
	// stream forever. None of the timeouts above cover that case: the peer is
	// healthy so it answers PINGs, a stream is open so IdleTimeout cannot fire,
	// and a handler that has produced no output yet has no data available to
	// write, so WriteByteTimeout has not even started counting.
	//
	// A deadline the client sent (Connect-Timeout-Ms, grpc-timeout) always wins,
	// and streaming handlers are exempt because they are meant to run long.
	//
	// Like every field here a non-positive value means "use the default", so
	// there is no way to switch this off: a unary handler with no deadline at all
	// is the failure mode it exists to prevent. A service with legitimately long
	// unary RPCs raises the value instead.
	//
	// It works by cancelling the handler's context, so it bounds a handler that
	// observes that context — which anything doing I/O through a database driver
	// or an HTTP client does. A handler that ignores its context entirely still
	// runs to completion; no server-side timeout can preempt one without leaking
	// the goroutine it left behind.
	RequestTimeout time.Duration `mapstructure:"request_timeout"`
	// MaxConcurrentStreams caps the streams a single connection may have open at
	// once. Note "connection", not "client": nothing here limits how many
	// connections one peer may open, so this is not on its own a bound on what
	// one caller can occupy. The default matches the standard library's, so
	// lowering it is the only way to make it bite.
	MaxConcurrentStreams int `mapstructure:"max_concurrent_streams"`
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
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	if c.KeepaliveInterval <= 0 {
		c.KeepaliveInterval = DefaultKeepaliveInterval
	}
	if c.WriteByteTimeout <= 0 {
		c.WriteByteTimeout = DefaultWriteByteTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = DefaultRequestTimeout
	}
	if c.MaxConcurrentStreams <= 0 {
		c.MaxConcurrentStreams = DefaultMaxConcurrentStreams
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
	http *http.Server
	// mux is the bare router, kept because http.Handler wraps it in middleware:
	// tests assert on routing, which needs the mux rather than the wrapper.
	mux             *http.ServeMux
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
	// it sees that context. The timeout interceptor comes after both so that a
	// deadline it imposes is reported on the rpc log line and recorded on the
	// span, and before a caller's so their interceptors run under the deadline
	// too.
	interceptors := append(
		[]connect.Interceptor{
			otelInterceptor,
			newLoggingInterceptor(o.log),
			newTimeoutInterceptor(cfg.RequestTimeout),
		},
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

	// Health and reflection are mounted without handlerOpts, so they carry no
	// interceptors and are deliberately absent from the logs — a health check
	// every few seconds is not worth a line each. accountedFor is what stops the
	// rejection middleware reporting them as never served.
	checker := grpchealth.NewStaticChecker(serviceNames...)
	healthPath, healthHandler := grpchealth.NewHandler(checker)
	mux.Handle(healthPath, accountedFor(healthHandler))

	reflector := grpcreflect.NewStaticReflector(
		append(serviceNames, grpchealth.HealthV1ServiceName)...,
	)
	for _, mount := range []func() (string, http.Handler){
		func() (string, http.Handler) { return grpcreflect.NewHandlerV1(reflector) },
		func() (string, http.Handler) { return grpcreflect.NewHandlerV1Alpha(reflector) },
	} {
		path, handler := mount()
		mux.Handle(path, accountedFor(handler))
	}

	// gRPC clients speak HTTP/2 without TLS (h2c); plain net/http only
	// negotiates HTTP/2 over TLS unless unencrypted HTTP/2 is opted into.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)

	return &Server{
		log:             o.log,
		name:            o.name,
		mux:             mux,
		shutdownTimeout: cfg.ShutdownTimeout,
		http: &http.Server{
			Addr: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
			// The mux is wrapped so that a request which never reaches a handler
			// is still reported; see newRejectionLogger.
			Handler:   newRejectionLogger(mux, o.log),
			Protocols: protocols,
			// No WriteTimeout: it bounds the whole ServeHTTP lifetime and so would
			// abort a healthy streaming response. ReadTimeout is off unless the
			// service opts in, because it is unsafe for client-streaming only.
			// See Config for the reasoning behind each of these.
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
			ReadTimeout:       cfg.ReadTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			HTTP2: &http.HTTP2Config{
				SendPingTimeout:      cfg.KeepaliveInterval,
				WriteByteTimeout:     cfg.WriteByteTimeout,
				MaxConcurrentStreams: cfg.MaxConcurrentStreams,
			},
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

// Handler returns the server's whole handler: every module, health and
// reflection handler mounted, behind the middleware that reports requests which
// never reach one. It is the seam for tests, which can serve it through httptest
// instead of binding a port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.http.Addr }
