package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
)

// TestSetupRoutesOtelErrorsToTheLogger covers what WithErrorLogger is for: OTel
// reports its own failures through otel.Handle, and left alone they reach the
// stream as level-less lines that no filter can select.
func TestSetupRoutesOtelErrorsToTheLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	// The handler is a process global and cannot be uninstalled, so point it
	// somewhere harmless rather than leaving this test's buffer live for whatever
	// runs next.
	t.Cleanup(func() {
		discard := zerolog.New(io.Discard)
		otel.SetErrorHandler(otelErrorHandler{log: &discard})
	})

	if _, err := Setup(context.Background(), Config{ServiceName: "tehran-test"},
		WithErrorLogger(&logger)); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	otel.Handle(errors.New("traces export: connection refused"))

	got := map[string]any{}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", buf.String(), err)
	}
	// Warn, not error: a collector being down loses telemetry and fails no
	// request, and the batch processor reports it once per failed export for as
	// long as the outage lasts.
	if got["level"] != zerolog.WarnLevel.String() {
		t.Errorf("level = %v, want %v", got["level"], zerolog.WarnLevel)
	}
	if got["error"] != "traces export: connection refused" {
		t.Errorf("error = %v, want the error OTel reported: %v", got["error"], got)
	}
}

func TestSetupShutdown(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := Config{
			Enabled:        enabled,
			Endpoint:       "localhost:4317",
			Insecure:       true,
			SampleRatio:    1.0,
			ServiceName:    "tehran-test",
			ServiceVersion: "test",
		}
		tel, err := Setup(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Setup(enabled=%v): %v", enabled, err)
		}
		families, err := tel.PromRegistry.Gather()
		if err != nil {
			t.Fatalf("gather metrics: %v", err)
		}
		if len(families) == 0 {
			t.Error("registry has no metric families, expected at least the Go collector")
		}
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown(enabled=%v): %v", enabled, err)
		}
	}
}
