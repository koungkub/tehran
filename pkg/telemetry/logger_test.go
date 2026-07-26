package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestNewLoggerJSONShape pins the log schema the zerolog back end produces:
// zerolog's field names (level, message, time), plus the three attributes the
// correlation handler contributes.
func TestNewLoggerJSONShape(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "info", Format: "json"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	ctx, spanCtx := startSpan(t)
	logger.InfoContext(ctx, "served", "procedure", "/Echo")

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}

	for key, want := range map[string]any{
		"level":     "info",
		"message":   "served",
		"procedure": "/Echo",
		TraceIDKey:  spanCtx.TraceID().String(),
		SpanIDKey:   spanCtx.SpanID().String(),
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v (record: %v)", key, got[key], want, got)
		}
	}
	if got["time"] == nil {
		t.Errorf("time is missing: %v", got)
	}
	if caller, _ := got[CallerKey].(string); !strings.HasPrefix(caller, "telemetry/logger_test.go:") {
		t.Errorf("%s = %q, want it to point at this test file", CallerKey, caller)
	}
	// The record's own time is emitted once, not once by the handler and again
	// by a zerolog timestamp hook.
	if n := strings.Count(buf.String(), `"time":`); n != 1 {
		t.Errorf("time appears %d times, want 1: %s", n, buf.String())
	}
}

// TestNewLoggerFlattensGroups documents a real difference from slog's own JSON
// handler: zerolog flattens groups into dotted keys instead of nesting objects.
// The correlation attributes must stay outside that flattening.
func TestNewLoggerFlattensGroups(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "info", Format: "json"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	ctx, _ := startSpan(t)
	logger.WithGroup("req").InfoContext(ctx, "served", "path", "/Echo")

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if got["req.path"] != "/Echo" {
		t.Errorf(`want the grouped attribute flattened to "req.path": %v`, got)
	}
	if got[TraceIDKey] == nil {
		t.Errorf("%s was swallowed by the group: %v", TraceIDKey, got)
	}
	if got["req."+TraceIDKey] != nil {
		t.Errorf("%s was prefixed with the group name: %v", TraceIDKey, got)
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "warn", Format: "json"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.InfoContext(context.Background(), "dropped")
	if buf.Len() != 0 {
		t.Errorf("info was emitted at level warn: %s", buf.String())
	}

	logger.WarnContext(context.Background(), "kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("warn was not emitted at level warn: %s", buf.String())
	}
}

func TestNewLoggerConsoleFormat(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "info", Format: "console"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.InfoContext(context.Background(), "served")

	out := buf.String()
	if !strings.Contains(out, "served") {
		t.Errorf("console output does not contain the message: %q", out)
	}
	if json.Valid(bytes.TrimSpace(buf.Bytes())) {
		t.Errorf("console format produced JSON: %q", out)
	}
}

func TestNewLoggerFormats(t *testing.T) {
	for _, format := range []string{"json", "console"} {
		logger, err := NewLogger(LogConfig{Level: "info", Format: format})
		if err != nil {
			t.Fatalf("NewLogger(%q): %v", format, err)
		}
		// The correlation wrapper must be outermost for the ids and caller to
		// stay top-level; NewLogger is the only place that ordering is decided.
		if _, ok := logger.Handler().(*CorrelationHandler); !ok {
			t.Errorf("NewLogger(%q) handler is %T, want *CorrelationHandler", format, logger.Handler())
		}
	}
}

func TestNewLoggerRejectsBadLevel(t *testing.T) {
	if _, err := NewLogger(LogConfig{Level: "chatty"}); err == nil {
		t.Error("NewLogger accepted an invalid level")
	}
}
