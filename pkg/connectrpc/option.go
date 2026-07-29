package connectrpc

import (
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Option configures a Server. The set is closed: only this package can define
// one.
type Option func(*options)

type options struct {
	name             string
	log              *zerolog.Logger
	modules          []Module
	interceptors     []connect.Interceptor
	tracerProvider   trace.TracerProvider
	meterProvider    metric.MeterProvider
	trustRemoteSpans bool
}

func newOptions(opts []Option) options {
	// Defaults keep New usable with no options at all: logging goes wherever
	// zerolog's package-level logger points, and the no-op providers make the otel
	// interceptor inert rather than absent.
	o := options{
		name:           DefaultName,
		log:            &zlog.Logger,
		tracerProvider: tracenoop.NewTracerProvider(),
		meterProvider:  metricnoop.NewMeterProvider(),
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithName labels the server in its own log lines and, through Name, wherever a
// supervisor reports on it. Defaults to DefaultName; worth setting when a
// process runs more than one RPC server.
func WithName(name string) Option {
	return func(o *options) {
		if name != "" {
			o.name = name
		}
	}
}

// WithLogger sets the logger used for the per-RPC log line, panics, and
// lifecycle events. Defaults to zerolog's package-level logger.
func WithLogger(log *zerolog.Logger) Option {
	return func(o *options) {
		if log != nil {
			o.log = log
		}
	}
}

// WithModules adds bounded contexts whose handlers the server mounts. Repeated
// calls accumulate.
func WithModules(modules ...Module) Option {
	return func(o *options) { o.modules = append(o.modules, modules...) }
}

// WithInterceptors adds interceptors after the built-in logging, tracing and
// metrics ones, so they run inside them. Repeated calls accumulate.
func WithInterceptors(interceptors ...connect.Interceptor) Option {
	return func(o *options) { o.interceptors = append(o.interceptors, interceptors...) }
}

// WithTracerProvider sets the provider the otelconnect interceptor records
// spans against. Defaults to a no-op provider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) {
		if tp != nil {
			o.tracerProvider = tp
		}
	}
}

// WithMeterProvider sets the provider the otelconnect interceptor records RPC
// metrics against. Defaults to a no-op provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) {
		if mp != nil {
			o.meterProvider = mp
		}
	}
}

// WithTrustRemoteSpans makes an incoming traceparent the *parent* of this
// server's span, so one trace runs across the services that handled a request.
//
// Without it a caller's trace context is still read, but otelconnect starts a
// new root span and merely links to it — which means a distributed trace breaks
// at every hop, and the trace_id on this service's log lines is not the one the
// caller saw. That is otelconnect's default, and it is the right default for a
// server reachable by callers it does not control: a trace id arrives in a
// header, so an untrusted peer can choose it, joining its requests onto a trace
// of its choosing or inflating one without bound.
//
// Turn it on for a service whose peers are inside the same trust boundary — a
// mesh, a private network, an authenticated internal API — and leave it off at
// the edge. A service that does both wants it off, and the trace joined by the
// internal hop behind it instead.
func WithTrustRemoteSpans() Option {
	return func(o *options) { o.trustRemoteSpans = true }
}
