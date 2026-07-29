package telemetry

import (
	"bytes"
	"encoding/json"
	stdlog "log"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestNewLoggerJSONShape pins the log schema: zerolog's field names (level,
// message, time), plus the correlation the hook contributes.
func TestNewLoggerJSONShape(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "info", Format: "json", Caller: true}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	ctx, spanCtx := startSpan(t)
	logger.Info().Ctx(ctx).Str("procedure", "/Echo").Msg("served")

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
	// zerolog's own format: the full path recorded at build time, not shortened.
	if caller, _ := got[CallerKey].(string); !strings.Contains(caller, "telemetry/logger_test.go:") {
		t.Errorf("%s = %q, want it to point at this test file", CallerKey, caller)
	}
	// The timestamp is emitted once. zerolog.New installs no timestamp hook, so
	// this exists to catch a second one being added alongside the explicit
	// .Timestamp() in NewLogger.
	if n := strings.Count(buf.String(), `"time":`); n != 1 {
		t.Errorf("time appears %d times, want 1: %s", n, buf.String())
	}
}

// TestCallerKeyMatchesZerolog guards the one constant this package duplicates:
// zerolog writes the caller field under its own package-level name, and CallerKey
// exists only so a consumer has one place to read every key from.
func TestCallerKeyMatchesZerolog(t *testing.T) {
	if CallerKey != zerolog.CallerFieldName {
		t.Errorf("CallerKey = %q, but zerolog writes %q", CallerKey, zerolog.CallerFieldName)
	}
}

// TestNewLoggerOmitsCallerByDefault pins the default that decides what a log line
// costs: resolving a call site is several times the cost of the rest of a record,
// so it is off unless configuration asks for it.
func TestNewLoggerOmitsCallerByDefault(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "info", Format: "json"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.Info().Msg("served")

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if got[CallerKey] != nil {
		t.Errorf("%s = %v, want it absent unless LogConfig.Caller is set", CallerKey, got[CallerKey])
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewLogger(LogConfig{Level: "warn", Format: "json"}, WithWriter(&buf))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	logger.Info().Msg("dropped")
	if buf.Len() != 0 {
		t.Errorf("info was emitted at level warn: %s", buf.String())
	}

	logger.Warn().Msg("kept")
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

	logger.Info().Msg("served")

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
		if _, err := NewLogger(LogConfig{Level: "info", Format: format}); err != nil {
			t.Fatalf("NewLogger(%q): %v", format, err)
		}
	}
}

// TestNewLoggerRoutesTheStandardLibrary covers what replaced slog.SetDefault:
// anything logging through the standard library's log package has to end up in
// the same stream, or a dependency's output disappears.
func TestNewLoggerRoutesTheStandardLibrary(t *testing.T) {
	var buf bytes.Buffer
	if _, err := NewLogger(LogConfig{Level: "info", Format: "json"}, WithWriter(&buf)); err != nil {
		t.Fatalf("NewLogger: %v", err)
	}

	stdlog.Print("from the standard library")

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	if got["message"] != "from the standard library" {
		t.Errorf("message = %v, want the standard library's line: %v", got["message"], got)
	}
	// zerolog.Logger.Write emits a level-less event, so these lines carry no
	// level field. Worth knowing before filtering a stream on one.
	if got["level"] != nil {
		t.Errorf("level = %v, want none: Write emits a level-less event", got["level"])
	}
}

func TestNewLoggerRejectsBadLevel(t *testing.T) {
	if _, err := NewLogger(LogConfig{Level: "chatty"}); err == nil {
		t.Error("NewLogger accepted an invalid level")
	}
}
