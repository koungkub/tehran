package telemetry

import (
	"context"
	"testing"

	"github.com/koungkub/tehran/internal/platform/config"
)

func TestSetupShutdown(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Otel{
			Enabled:     enabled,
			Endpoint:    "localhost:4317",
			Insecure:    true,
			SampleRatio: 1.0,
			ServiceName: "tehran-test",
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
