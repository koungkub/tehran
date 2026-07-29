// Package telemetry wires OpenTelemetry tracing, Prometheus-bridged metrics,
// and a structured logger whose records carry the active trace identifiers.
//
// It is application-agnostic: Config carries everything the pipeline needs,
// including the service version, so nothing here reaches back into a
// particular service's configuration or build-info package.
package telemetry

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
)

// Config describes the telemetry pipeline.
//
// The mapstructure tags are inert metadata: they let a viper-based service nest
// this struct directly into its own configuration without a conversion layer,
// and give every service that does so the same configuration keys.
type Config struct {
	ServiceName string `mapstructure:"service_name"`
	// ServiceVersion stamps service.version onto every span and metric. It
	// comes from the binary's build stamp (ldflags) rather than a config file,
	// so the application assigns it after loading configuration. Empty means
	// the attribute is omitted.
	ServiceVersion string `mapstructure:"-"`
	// Enabled gates the OTLP trace exporter only. Metrics are pull-based, so
	// they remain available on the Prometheus registry either way.
	Enabled     bool    `mapstructure:"enabled"`
	Endpoint    string  `mapstructure:"endpoint"`
	Insecure    bool    `mapstructure:"insecure"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
}

// Telemetry holds the configured providers. The concrete SDK types are exposed
// rather than the interfaces because callers need Shutdown, and because handing
// them to a consumer that wants only the interface costs nothing.
type Telemetry struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	// PromRegistry is meant to be served on a /metrics endpoint. The OTel
	// Prometheus exporter is pull-based: it writes into this registry, so the
	// same instance must be handed to promhttp.
	PromRegistry *prometheus.Registry
}

// Option configures Setup. The set is closed: only this package can define one.
type Option func(*setupOptions)

type setupOptions struct {
	errLog *zerolog.Logger
}

// WithErrorLogger routes the errors OpenTelemetry reports about *itself* — a span
// export that failed, a flush that timed out — into log instead of leaving them
// to OTel's default.
//
// That default is worth understanding, because it is what makes this option
// necessary. otel.Handle with no handler installed falls through to the
// package-level log.Print (otel internal/errorhandler). NewLogger points the
// standard library's log package at the application logger, so the line does
// arrive in the log stream — but through zerolog.Logger.Write, which emits a
// level-less event. The result is an export failure that no level filter can
// select, carrying a caller that names Go's own log/log.go rather than anything
// in OTel.
//
// Setting this is therefore mostly about making those lines selectable. Two
// limitations survive it, both from otel.ErrorHandler being Handle(error) and
// nothing more:
//
//   - No trace correlation. There is no context to read a span from, so these
//     lines never carry trace_id, whatever the call site was doing.
//   - The caller, when log.caller is on, names this package's handler. It is the
//     same value for every OTel error, so it identifies nothing.
//
// Note also that OTel delegates errors to a handler only on the *first*
// SetErrorHandler call (delegateErrorHandlerOnce). An application that installs
// its own handler as well gets one of the two silently, depending on order.
func WithErrorLogger(log *zerolog.Logger) Option {
	return func(o *setupOptions) {
		if log != nil {
			o.errLog = log
		}
	}
}

// otelErrorHandler reports OTel's own failures on the application logger.
//
// Warn, not error: every otel.Handle call site in the trace SDK is an export, a
// flush or a shutdown, so what it reports is telemetry being lost, never a
// request failing. It is also unbounded — the batch processor calls it once per
// failed export, so a collector that is simply down produces one of these every
// batch interval for as long as the outage lasts. At error level that is a page
// for something no on-call rotation can act on; at warn it is still selectable,
// which is the whole point of installing this.
type otelErrorHandler struct{ log *zerolog.Logger }

var _ otel.ErrorHandler = otelErrorHandler{}

func (h otelErrorHandler) Handle(err error) {
	h.log.Warn().Err(err).Msg("otel")
}

// Setup builds the tracer and meter providers and installs them (plus W3C
// trace-context propagation) as the OTel globals. When cfg.Enabled is false
// the tracer provider has no exporter, so spans are dropped; metrics stay on
// since they are scraped, not pushed.
func Setup(ctx context.Context, cfg Config, opts ...Option) (*Telemetry, error) {
	var o setupOptions
	for _, opt := range opts {
		opt(&o)
	}
	// Installed before anything else here, unlike the provider globals at the
	// bottom: an exporter or a reader that fails while being built reports it
	// through otel.Handle, and this is the only window in which that would
	// otherwise go unlabelled.
	if o.errLog != nil {
		otel.SetErrorHandler(otelErrorHandler{log: o.errLog})
	}

	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	tracerOpts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	}
	if cfg.Enabled {
		exporterOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
		}
		exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
		if err != nil {
			return nil, fmt.Errorf("create otlp trace exporter: %w", err)
		}
		tracerOpts = append(tracerOpts, sdktrace.WithBatcher(exporter))
	}
	tracerProvider := sdktrace.NewTracerProvider(tracerOpts...)

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	promExporter, err := otelprom.New(otelprom.WithRegisterer(registry))
	if err != nil {
		// The tracer provider is already running by this point, and when
		// cfg.Enabled it owns an OTLP exporter holding a gRPC connection and a
		// batch processor goroutine. Returning the error alone would strand both
		// for the life of the process, which matters most to the caller that
		// treats a telemetry failure as survivable and carries on without it.
		return nil, errors.Join(
			fmt.Errorf("create prometheus exporter: %w", err),
			tracerProvider.Shutdown(ctx),
		)
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return &Telemetry{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
		PromRegistry:   registry,
	}, nil
}

// Shutdown flushes pending spans and metrics.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	return errors.Join(
		t.TracerProvider.Shutdown(ctx),
		t.MeterProvider.Shutdown(ctx),
	)
}
