package telemetry

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// These measure the logging stack rather than the terminal or the disk, so
// everything writes to io.Discard.
//
// The pair that matters is Enabled against EnabledWithCaller. Resolving a call
// site costs several times the rest of a four-field record put together, which is
// why LogConfig.Caller exists and why it is off by default. The Disabled case is
// here to show there is nothing to win there: a dropped line was already free.

func benchLogger(b *testing.B, cfg LogConfig) *zerolog.Logger {
	b.Helper()
	logger, err := NewLogger(cfg, WithWriter(io.Discard))
	if err != nil {
		b.Fatalf("NewLogger: %v", err)
	}
	return logger
}

func rpcLine(ctx context.Context, logger *zerolog.Logger) {
	logger.Info().Ctx(ctx).
		Str("procedure", "/greeter.v1.GreeterService/SayHello").
		Str("peer", "10.1.2.3:54221").
		Dur("duration", 1200*time.Microsecond).
		Str("code", "ok").
		Msg("rpc")
}

// BenchmarkEnabled is the shape every call site in this repository uses.
func BenchmarkEnabled(b *testing.B) {
	logger := benchLogger(b, LogConfig{Level: "info", Format: "json"})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		rpcLine(ctx, logger)
	}
}

// BenchmarkEnabledWithCaller is the same line with call-site resolution on. The
// difference between this and BenchmarkEnabled is the whole cost of the caller
// field.
func BenchmarkEnabledWithCaller(b *testing.B) {
	logger := benchLogger(b, LogConfig{Level: "info", Format: "json", Caller: true})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		rpcLine(ctx, logger)
	}
}

// BenchmarkEnabledWithSpan prices the correlation itself: two hex encodings and
// the context lookup that finds them.
func BenchmarkEnabledWithSpan(b *testing.B) {
	logger := benchLogger(b, LogConfig{Level: "info", Format: "json"})
	ctx := benchSpanContext()
	b.ReportAllocs()
	for b.Loop() {
		rpcLine(ctx, logger)
	}
}

// BenchmarkDisabled is a line dropped by the level. zerolog returns a nil event
// and every chained field is a no-op on it.
func BenchmarkDisabled(b *testing.B) {
	logger := benchLogger(b, LogConfig{Level: "info", Format: "json"})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		logger.Debug().Ctx(ctx).
			Str("statement", "SELECT * FROM widgets WHERE id = ?").
			Dur("duration", 400*time.Microsecond).
			Int64("rows", 1).
			Str("table", "widgets").
			Msg("sql")
	}
}

// BenchmarkWith measures a request-scoped logger: five fields attached once, then
// one line. zerolog pre-serialises the attached set, so they cost a copy per
// record rather than a re-encode.
func BenchmarkWith(b *testing.B) {
	base := benchLogger(b, LogConfig{Level: "info", Format: "json"})
	logger := base.With().
		Str("service", "tehran").
		Str("env", "prod").
		Str("region", "ap-southeast-1").
		Str("instance", "tehran-7d9f4b6c8-x2jkl").
		Str("version", "1.4.2").
		Logger()
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		logger.Info().Ctx(ctx).Str("code", "ok").Msg("rpc")
	}
}

// benchSpanContext returns a context carrying a valid span context, built by hand
// so the benchmark does not measure the tracing SDK.
func benchSpanContext() context.Context {
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		SpanID:     trace.SpanID{0xb7, 0xad, 0x6b, 0x71, 0x69, 0x20, 0x33, 0x31},
		TraceFlags: trace.FlagsSampled,
	})
	return trace.ContextWithSpanContext(context.Background(), sc)
}
