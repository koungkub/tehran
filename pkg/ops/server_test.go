package ops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/koungkub/tehran/pkg/lifecycle"
)

// The server is meant to be driven by a supervisor. Asserting that here rather
// than in the package proper keeps the production code free of the dependency.
var _ lifecycle.Component = (*Server)(nil)

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthzAlwaysOK(t *testing.T) {
	// Liveness must not depend on readiness: a draining instance is still alive,
	// and reporting otherwise would get it killed instead of drained.
	srv := New(Config{}, WithReadyCheck("down", func(context.Context) error {
		return errors.New("nope")
	}))

	if rec := get(t, srv, "/healthz"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyzWithoutChecks(t *testing.T) {
	srv := New(Config{})

	rec := get(t, srv, "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("/readyz = %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Errorf("/readyz body = %q, want %q", body, "ok")
	}
}

func TestReadyzRunsAllChecks(t *testing.T) {
	var ran []string
	srv := New(Config{},
		WithReadyCheck("first", func(context.Context) error {
			ran = append(ran, "first")
			return nil
		}),
		WithReadyCheck("second", func(context.Context) error {
			ran = append(ran, "second")
			return nil
		}),
	)

	if rec := get(t, srv, "/readyz"); rec.Code != http.StatusOK {
		t.Errorf("/readyz = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(ran) != 2 {
		t.Errorf("checks run = %v, want both", ran)
	}
}

func TestReadyzNamesTheFailingCheck(t *testing.T) {
	srv := New(Config{},
		WithReadyCheck("healthy", func(context.Context) error { return nil }),
		WithReadyCheck("drain", func(context.Context) error { return errors.New("draining") }),
	)

	rec := get(t, srv, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "drain") {
		t.Errorf("/readyz body = %q, want it to name the drain check", body)
	}
	if !strings.Contains(body, "draining") {
		t.Errorf("/readyz body = %q, want it to include the check's error", body)
	}
}

func TestMetricsServesRegistry(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "tehran_test_total"})
	registry.MustRegister(counter)
	counter.Inc()

	srv := New(Config{}, WithRegistry(registry))

	rec := get(t, srv, "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "tehran_test_total 1") {
		t.Errorf("/metrics body does not contain the registered counter:\n%s", rec.Body.String())
	}
}

func TestMetricsUnmountedWithoutRegistry(t *testing.T) {
	srv := New(Config{})

	if rec := get(t, srv, "/metrics"); rec.Code != http.StatusNotFound {
		t.Errorf("/metrics = %d, want %d when no registry is configured", rec.Code, http.StatusNotFound)
	}
}

func TestServeStopsOnContextCancel(t *testing.T) {
	srv := New(Config{Host: "127.0.0.1", ShutdownTimeout: 2 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve after cancel = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", got.ShutdownTimeout, DefaultShutdownTimeout)
	}
	if got.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got.ReadHeaderTimeout, DefaultReadHeaderTimeout)
	}
	if got.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", got.IdleTimeout, DefaultIdleTimeout)
	}
}

// TestIdleTimeoutIsSetOnTheServer pins the one timeout here that cannot be left
// to net/http. A zero IdleTimeout falls back to ReadTimeout, and nothing sets
// one, so an unset field means no idle timeout at all rather than a conservative
// default — and every caller on this port is a keep-alive client.
func TestIdleTimeoutIsSetOnTheServer(t *testing.T) {
	srv := New(Config{})
	if srv.http.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v", srv.http.IdleTimeout, DefaultIdleTimeout)
	}
	srv = New(Config{IdleTimeout: 7 * time.Second})
	if srv.http.IdleTimeout != 7*time.Second {
		t.Errorf("IdleTimeout = %v, want the configured 7s", srv.http.IdleTimeout)
	}
}
