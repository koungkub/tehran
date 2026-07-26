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

// Setup builds the tracer and meter providers and installs them (plus W3C
// trace-context propagation) as the OTel globals. When cfg.Enabled is false
// the tracer provider has no exporter, so spans are dropped; metrics stay on
// since they are scraped, not pushed.
func Setup(ctx context.Context, cfg Config) (*Telemetry, error) {
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
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
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
