package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// newCapture returns a logger carrying the correlation hook, plus a function
// decoding whatever was written.
func newCapture() (*zerolog.Logger, func(*testing.T) map[string]any) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf).Hook(correlationHook{})
	return &logger, func(t *testing.T) map[string]any {
		t.Helper()
		got := map[string]any{}
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("decode %q: %v", buf.String(), err)
		}
		return got
	}
}

func startSpan(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	t.Cleanup(func() { span.End() })
	return ctx, span.SpanContext()
}

func TestCorrelationHookStampsSpanIDs(t *testing.T) {
	ctx, spanCtx := startSpan(t)
	logger, decode := newCapture()

	logger.Info().Ctx(ctx).Str("k", "v").Msg("hello")

	got := decode(t)
	if got[TraceIDKey] != spanCtx.TraceID().String() {
		t.Errorf("%s = %v, want %v", TraceIDKey, got[TraceIDKey], spanCtx.TraceID())
	}
	if got[SpanIDKey] != spanCtx.SpanID().String() {
		t.Errorf("%s = %v, want %v", SpanIDKey, got[SpanIDKey], spanCtx.SpanID())
	}
	if got["k"] != "v" {
		t.Errorf("the record's own field was lost: %v", got)
	}
}

func TestCorrelationHookWithoutSpan(t *testing.T) {
	logger, decode := newCapture()

	logger.Info().Ctx(context.Background()).Msg("hello")

	got := decode(t)
	if _, ok := got[TraceIDKey]; ok {
		t.Errorf("%s present without an active span: %v", TraceIDKey, got)
	}
	if _, ok := got[SpanIDKey]; ok {
		t.Errorf("%s present without an active span: %v", SpanIDKey, got)
	}
}

// TestCorrelationHookNeedsAContext is the contract callers have to know about:
// correlation is read from the event's context, so an event created without one
// carries no ids. It is a documented limitation rather than a bug, and it is the
// reason every call site in this repository passes Ctx.
func TestCorrelationHookNeedsAContext(t *testing.T) {
	ctx, _ := startSpan(t)
	logger, decode := newCapture()

	_ = ctx // deliberately not passed to the event
	logger.Info().Msg("hello")

	if got := decode(t); got[TraceIDKey] != nil {
		t.Errorf("%s = %v, want it absent when no context reached the event",
			TraceIDKey, got[TraceIDKey])
	}
}

// TestCorrelationHookSurvivesWith covers a derived logger: fields attached with
// With are held by the logger, and the hook still has to run for each record.
func TestCorrelationHookSurvivesWith(t *testing.T) {
	ctx, spanCtx := startSpan(t)
	var buf bytes.Buffer
	base := zerolog.New(&buf).Hook(correlationHook{})
	logger := base.With().Str("service", "tehran").Logger()

	logger.Info().Ctx(ctx).Msg("hello")

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if got[TraceIDKey] != spanCtx.TraceID().String() {
		t.Errorf("%s = %v, want %v", TraceIDKey, got[TraceIDKey], spanCtx.TraceID())
	}
	if got["service"] != "tehran" {
		t.Errorf("the derived field was lost: %v", got)
	}
}
