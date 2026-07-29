package database

import (
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
	"gorm.io/gorm"
)

// Option configures a DB. The set is closed: only this package can define one.
type Option func(*options)

type options struct {
	name           string
	log            *zerolog.Logger
	dialector      gorm.Dialector
	plugins        []gorm.Plugin
	tracerProvider trace.TracerProvider
	meterProvider  metric.MeterProvider
}

func newOptions(opts []Option) options {
	// Defaults keep Open usable with no options at all: logging goes wherever
	// zerolog's package-level logger points, and the no-op providers make the
	// instrumentation inert rather than absent.
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

// WithName labels the pool in its log lines, in its metric attributes, and
// through Name wherever a readiness check reports on it. Defaults to
// DefaultName; worth setting when a process opens more than one pool.
func WithName(name string) Option {
	return func(o *options) {
		if name != "" {
			o.name = name
		}
	}
}

// WithLogger sets the logger for statements, slow queries and connection
// events. Defaults to zerolog's package-level logger.
//
// Statement lines carry the trace of the request that caused them only if the
// query ran with a context — gorm.WithContext(ctx) — since that is where the
// span comes from.
func WithLogger(log *zerolog.Logger) Option {
	return func(o *options) {
		if log != nil {
			o.log = log
		}
	}
}

// WithDialector opens through a dialector built by the caller instead of one
// derived from Config.Driver, which is what makes this package usable with a
// driver it does not import: sqlite, a cloud connector, a Cloud SQL socket, or
// sqlmock in a test.
//
// It bypasses DSN construction entirely, so Config's connection fields are
// ignored — the pool, logging and instrumentation settings still apply.
func WithDialector(d gorm.Dialector) Option {
	return func(o *options) {
		if d != nil {
			o.dialector = d
		}
	}
}

// WithPlugins registers GORM plugins after the built-in instrumentation, so
// their callbacks run inside its spans. Repeated calls accumulate.
func WithPlugins(plugins ...gorm.Plugin) Option {
	return func(o *options) { o.plugins = append(o.plugins, plugins...) }
}

// WithTracerProvider sets the provider statement spans are recorded against.
// Defaults to a no-op provider.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(o *options) {
		if tp != nil {
			o.tracerProvider = tp
		}
	}
}

// WithMeterProvider sets the provider the statement-duration histogram and the
// pool gauges are recorded against. Defaults to a no-op provider.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(o *options) {
		if mp != nil {
			o.meterProvider = mp
		}
	}
}
