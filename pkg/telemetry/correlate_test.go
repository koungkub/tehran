package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"testing/slogtest"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// newCapture returns a logger wrapping a JSON handler, plus a function decoding
// whatever was written as a nested map.
func newCapture() (*slog.Logger, func(*testing.T) map[string]any) {
	var buf bytes.Buffer
	logger := slog.New(NewCorrelationHandler(slog.NewJSONHandler(&buf, nil)))
	return logger, func(t *testing.T) map[string]any {
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

// TestCorrelationHandlerConformance runs the standard library's handler
// conformance suite, which is what verifies the group and attribute replay in
// Handle behaves exactly like an unwrapped handler.
func TestCorrelationHandlerConformance(t *testing.T) {
	var buf bytes.Buffer
	slogtest.Run(t,
		func(*testing.T) slog.Handler {
			buf.Reset()
			return NewCorrelationHandler(slog.NewJSONHandler(&buf, nil))
		},
		func(t *testing.T) map[string]any {
			t.Helper()
			got := map[string]any{}
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("decode %q: %v", buf.String(), err)
			}
			return got
		},
	)
}

func TestCorrelationHandlerStampsSpanIDs(t *testing.T) {
	ctx, spanCtx := startSpan(t)
	logger, decode := newCapture()

	logger.InfoContext(ctx, "hello", "k", "v")

	got := decode(t)
	if got[TraceIDKey] != spanCtx.TraceID().String() {
		t.Errorf("%s = %v, want %v", TraceIDKey, got[TraceIDKey], spanCtx.TraceID())
	}
	if got[SpanIDKey] != spanCtx.SpanID().String() {
		t.Errorf("%s = %v, want %v", SpanIDKey, got[SpanIDKey], spanCtx.SpanID())
	}
	if got["k"] != "v" {
		t.Errorf("the record's own attribute was lost: %v", got)
	}
}

func TestCorrelationHandlerWithoutSpan(t *testing.T) {
	logger, decode := newCapture()

	logger.InfoContext(context.Background(), "hello")

	got := decode(t)
	if _, ok := got[TraceIDKey]; ok {
		t.Errorf("%s present without an active span: %v", TraceIDKey, got)
	}
	if _, ok := got[SpanIDKey]; ok {
		t.Errorf("%s present without an active span: %v", SpanIDKey, got)
	}
}

// TestCorrelationHandlerSurvivesWith covers the re-wrapping in WithAttrs: a
// logger derived with With must still correlate.
func TestCorrelationHandlerSurvivesWith(t *testing.T) {
	ctx, spanCtx := startSpan(t)
	logger, decode := newCapture()

	logger.With("service", "tehran").InfoContext(ctx, "hello")

	got := decode(t)
	if got[TraceIDKey] != spanCtx.TraceID().String() {
		t.Errorf("%s = %v, want %v", TraceIDKey, got[TraceIDKey], spanCtx.TraceID())
	}
	if got["service"] != "tehran" {
		t.Errorf("the derived attribute was lost: %v", got)
	}
}

// TestCorrelationHandlerKeepsIDsOutOfGroups is the reason Handle replays
// qualifiers instead of pushing them into the inner handler. A log backend joins
// on a top-level trace_id; nested under a group it would be g.trace_id and the
// correlation would silently stop working.
func TestCorrelationHandlerKeepsIDsOutOfGroups(t *testing.T) {
	ctx, spanCtx := startSpan(t)
	logger, decode := newCapture()

	logger.WithGroup("g").With("k", "v").InfoContext(ctx, "hello", "x", "y")

	got := decode(t)
	if got[TraceIDKey] != spanCtx.TraceID().String() {
		t.Errorf("%s = %v, want it at the top level; got record %v", TraceIDKey, got[TraceIDKey], got)
	}
	group, ok := got["g"].(map[string]any)
	if !ok {
		t.Fatalf("group g is missing or not an object: %v", got)
	}
	if group[TraceIDKey] != nil {
		t.Errorf("%s leaked into group g: %v", TraceIDKey, group)
	}
	if group["k"] != "v" || group["x"] != "y" {
		t.Errorf("group g = %v, want both k and x inside it", group)
	}
}

func TestCorrelationHandlerNestedGroups(t *testing.T) {
	ctx, _ := startSpan(t)
	logger, decode := newCapture()

	logger.WithGroup("a").WithGroup("b").InfoContext(ctx, "hello", "x", "y")

	got := decode(t)
	if got[TraceIDKey] == nil {
		t.Errorf("%s missing from the top level: %v", TraceIDKey, got)
	}
	a, ok := got["a"].(map[string]any)
	if !ok {
		t.Fatalf("group a is missing: %v", got)
	}
	b, ok := a["b"].(map[string]any)
	if !ok {
		t.Fatalf("group b is missing from a: %v", a)
	}
	if b["x"] != "y" {
		t.Errorf("a.b = %v, want x inside it", b)
	}
}

// TestCorrelationHandlerAddsCaller covers what the back end drops: zerolog's
// slog handler ignores Record.PC, so the call site is only present because this
// handler resolves it.
func TestCorrelationHandlerAddsCaller(t *testing.T) {
	logger, decode := newCapture()

	logger.InfoContext(context.Background(), "hello") // <- the line reported below

	got := decode(t)
	caller, ok := got[CallerKey].(string)
	if !ok {
		t.Fatalf("%s missing from the record: %v", CallerKey, got)
	}
	if !strings.HasPrefix(caller, "telemetry/correlate_test.go:") {
		t.Errorf("%s = %q, want it to point at this test file", CallerKey, caller)
	}
}

func TestCorrelationHandlerKeepsCallerOutOfGroups(t *testing.T) {
	logger, decode := newCapture()

	logger.WithGroup("g").InfoContext(context.Background(), "hello", "x", "y")

	got := decode(t)
	if _, ok := got[CallerKey]; !ok {
		t.Errorf("%s is not at the top level: %v", CallerKey, got)
	}
}

// TestCorrelationHandlerNoCallerWithoutPC guards the fast path: a record built
// by hand has no PC, and inventing a call site for it would be a lie.
func TestCorrelationHandlerNoCallerWithoutPC(t *testing.T) {
	logger, decode := newCapture()

	if err := logger.Handler().Handle(
		context.Background(),
		slog.NewRecord(time.Now(), slog.LevelInfo, "hello", 0),
	); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	if got := decode(t); got[CallerKey] != nil {
		t.Errorf("%s = %v, want it absent when the record has no PC", CallerKey, got[CallerKey])
	}
}
