// Package ops serves the operational endpoints an orchestrator and a metrics
// scraper need: /metrics, /healthz and /readyz.
//
// It is deliberately separate from the application's own server. A service with
// no inbound RPC at all — a queue consumer, say — still has to expose probes
// and metrics, and a service that does have an RPC port wants these on a
// different one so they are not publicly routable.
package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Defaults substituted by New for zero-valued Config fields.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultShutdownTimeout   = 5 * time.Second
)

// DefaultName labels the server in logs and to a supervisor.
const DefaultName = "ops"

// Config describes the ops listener. See the equivalent in pkg/connectrpc for
// why the mapstructure tags are here.
type Config struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	// ShutdownTimeout bounds how long Serve waits for in-flight scrapes and
	// probes once its context is cancelled. It is short by design: nothing here
	// is long-running.
	ShutdownTimeout   time.Duration `mapstructure:"shutdown_timeout"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout"`
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	return c
}

// Server exposes the operational endpoints and owns its own lifecycle. It
// satisfies lifecycle.Component, so a supervisor can sequence it against other
// components without this package importing one.
type Server struct {
	http            *http.Server
	log             *slog.Logger
	name            string
	shutdownTimeout time.Duration
}

// New builds the ops server. With no options it still serves /healthz and a
// /readyz that reports ready.
func New(cfg Config, opts ...Option) *Server {
	cfg = cfg.withDefaults()
	o := newOptions(opts)

	mux := http.NewServeMux()
	if o.registry != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(o.registry, promhttp.HandlerOpts{}))
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeOK(w)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		// Liveness says the process is up; readiness says it should receive
		// traffic. Report the failing check by name so an operator reading the
		// probe output knows which one objected.
		for _, c := range o.readyChecks {
			if err := c.fn(r.Context()); err != nil {
				http.Error(w, c.name+": "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		writeOK(w)
	})

	return &Server{
		log:             o.log,
		name:            o.name,
		shutdownTimeout: cfg.ShutdownTimeout,
		http: &http.Server{
			Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
			Handler:           mux,
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		},
	}
}

func writeOK(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// Serve listens and blocks until the server stops. Cancelling ctx starts a
// graceful shutdown bounded by Config.ShutdownTimeout; a clean stop returns nil.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.LogAttrs(ctx, slog.LevelInfo, s.name+" server listening",
			slog.String("addr", s.Addr()))
		err := s.http.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("%s server: %w", s.name, err)
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.LogAttrs(ctx, slog.LevelInfo, s.name+" server shutting down",
		slog.Duration("timeout", s.shutdownTimeout))
	// A fresh context: ctx is already cancelled, and Shutdown needs a live
	// deadline to drain within.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	return errors.Join(s.Shutdown(shutdownCtx), <-errCh)
}

// Shutdown stops the server, draining in-flight requests until ctx is done.
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown %s server: %w", s.name, err)
	}
	return nil
}

// Name identifies the server to a supervisor and labels its log lines.
func (s *Server) Name() string { return s.name }

// Handler returns the mux, so tests can serve it through httptest instead of
// binding a port.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Addr is the host:port the server listens on.
func (s *Server) Addr() string { return s.http.Addr }
