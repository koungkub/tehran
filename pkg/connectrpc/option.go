package connectrpc

import (
	"log/slog"

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
	name           string
	log            *slog.Logger
	modules        []Module
	interceptors   []connect.Interceptor
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
}

func newOptions(opts []Option) options {
	// Defaults keep New usable with no options at all: logging goes wherever
	// slog's default handler points, and the no-op providers make the otel
	// interceptor inert rather than absent.
	o := options{
		name:           DefaultName,
		log:            slog.Default(),
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
// lifecycle events. Defaults to slog.Default().
func WithLogger(log *slog.Logger) Option {
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
