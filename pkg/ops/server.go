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
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

// Defaults substituted by New for zero-valued Config fields.
const (
	DefaultReadHeaderTimeout = 10 * time.Second
	DefaultShutdownTimeout   = 5 * time.Second
	// DefaultIdleTimeout matches pkg/connectrpc's. Everything that talks to this
	// port — a Prometheus scraper, an orchestrator's probes — reconnects on a
	// schedule and keeps its connections alive between polls, so without a bound
	// a peer that goes away without a FIN leaves one behind for good.
	DefaultIdleTimeout = 120 * time.Second
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
	// IdleTimeout reaps a kept-alive connection with no request in flight.
	//
	// It has to be set explicitly: net/http falls back to ReadTimeout when this
	// is zero, and nothing here sets one, so leaving it out means no idle timeout
	// at all rather than a conservative one. Scrapers and probes are the only
	// callers and they are all keep-alive clients, so the connections that leak
	// that way are exactly the ones this port collects.
	IdleTimeout time.Duration `mapstructure:"idle_timeout"`
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	if c.ReadHeaderTimeout <= 0 {
		c.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = DefaultIdleTimeout
	}
	return c
}

// Server exposes the operational endpoints and owns its own lifecycle. It
// satisfies lifecycle.Component, so a supervisor can sequence it against other
// components without this package importing one.
type Server struct {
	http            *http.Server
	log             *zerolog.Logger
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
			IdleTimeout:       cfg.IdleTimeout,
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
		s.log.Info().Ctx(ctx).Str("addr", s.Addr()).Msg(s.name + " server listening")
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

	s.log.Info().Ctx(ctx).Dur("timeout", s.shutdownTimeout).
		Msg(s.name + " server shutting down")
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
