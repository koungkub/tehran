// Package lifecycle starts and stops the long-running parts of an application
// in a defined order.
//
// A Supervisor runs each Component concurrently, waits for a shutdown signal or
// for any component to exit, and then stops them in the reverse of the order
// they were registered — one at a time, waiting for each to finish draining
// before starting on the next. Registering the components other components
// depend on first therefore keeps them alive longest: an ops server registered
// ahead of an RPC server keeps answering probes for the whole time the RPC
// server is draining.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"
)

// DefaultShutdownTimeout bounds the whole ordered shutdown, as a backstop
// against a component that will not stop.
const DefaultShutdownTimeout = 30 * time.Second

// ErrNoComponents is returned by Run when nothing was registered.
var ErrNoComponents = errors.New("lifecycle: no components registered")

// Component is a long-running part of an application: a server, a queue
// consumer, a scheduler.
//
// Serve must block until the component stops. Cancelling its context must begin
// a graceful shutdown, after which Serve must return — returning nil means it
// stopped cleanly. A component that bounds its own drain (as the servers in this
// repository do, via their ShutdownTimeout) keeps that bound; the supervisor's
// timeout is only a backstop over the whole sequence.
//
// Serve returning before it is asked to stop is treated as a reason to shut the
// whole application down, whether it returns an error or not.
type Component interface {
	// Name identifies the component in logs and in shutdown errors.
	Name() string
	Serve(ctx context.Context) error
}

// Config describes the supervisor. See pkg/connectrpc for why the mapstructure
// tag is here.
type Config struct {
	// ShutdownTimeout bounds the entire ordered shutdown. It should exceed the
	// sum of the components' own drain timeouts, since it exists to catch a
	// component that ignores its context rather than to cut drains short.
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	return c
}

// Supervisor owns the run and shutdown sequence of a set of components.
type Supervisor struct {
	log             *slog.Logger
	components      []Component
	beforeShutdown  []func()
	signals         []os.Signal
	shutdownTimeout time.Duration
}

// New builds a supervisor. Registration order is start order, and shutdown runs
// in reverse.
func New(cfg Config, opts ...Option) *Supervisor {
	cfg = cfg.withDefaults()
	o := newOptions(opts)
	return &Supervisor{
		log:             o.log,
		components:      o.components,
		beforeShutdown:  o.beforeShutdown,
		signals:         o.signals,
		shutdownTimeout: cfg.ShutdownTimeout,
	}
}

// task is a component while it is running.
type task struct {
	component Component
	cancel    context.CancelFunc
	// done carries Serve's return value. It is buffered so a component that has
	// already exited can be collected without blocking.
	done chan error
}

// Run starts every component and blocks until a shutdown signal arrives, ctx is
// cancelled, or a component exits. It then stops the components in reverse
// registration order and returns every error it collected, joined.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.components) == 0 {
		return ErrNoComponents
	}

	if len(s.signals) > 0 {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, s.signals...)
		defer stop()
	}

	// Components get contexts detached from ctx, so that cancelling ctx does not
	// stop all of them at once — sequencing them is the supervisor's job.
	base := context.WithoutCancel(ctx)
	running := make([]*task, len(s.components))
	exited := make(chan int, len(s.components))

	for i, c := range s.components {
		componentCtx, cancel := context.WithCancel(base)
		t := &task{component: c, cancel: cancel, done: make(chan error, 1)}
		running[i] = t

		s.log.LogAttrs(ctx, slog.LevelInfo, "component starting",
			slog.String("component", c.Name()))
		go func() {
			t.done <- c.Serve(componentCtx)
			exited <- i
		}()
	}
	// Release every context on any exit path, including a panic or an early
	// return from the timeout backstop.
	defer func() {
		for _, t := range running {
			t.cancel()
		}
	}()

	select {
	case <-ctx.Done():
		s.log.LogAttrs(ctx, slog.LevelInfo, "shutdown signalled")
	case i := <-exited:
		s.log.LogAttrs(ctx, slog.LevelWarn, "component exited early, shutting down",
			slog.String("component", running[i].component.Name()))
	}

	// Hooks run before anything is stopped: this is where readiness is flipped
	// off, so load balancers stop routing here while every port is still open.
	for _, hook := range s.beforeShutdown {
		hook()
	}

	return s.stop(ctx, running)
}

// stop drains the components in reverse registration order, one at a time.
func (s *Supervisor) stop(ctx context.Context, running []*task) error {
	deadline := time.NewTimer(s.shutdownTimeout)
	defer deadline.Stop()

	var errs []error
	for i, t := range slices.Backward(running) {
		name := t.component.Name()
		t.cancel()

		select {
		case err := <-t.done:
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
			s.log.LogAttrs(ctx, slog.LevelInfo, "component stopped",
				slog.String("component", name))
		case <-deadline.C:
			// Name everything still running, not just the one being waited on:
			// they are all still holding resources.
			stuck := make([]string, 0, i+1)
			for _, r := range running[:i+1] {
				stuck = append(stuck, r.component.Name())
			}
			errs = append(errs, fmt.Errorf(
				"lifecycle: shutdown timed out after %s, still running: %s",
				s.shutdownTimeout, strings.Join(stuck, ", "),
			))
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}
