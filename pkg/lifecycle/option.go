package lifecycle

import (
	"os"
	"syscall"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// Option configures a Supervisor. The set is closed: only this package can
// define one.
type Option func(*options)

type options struct {
	log            *zerolog.Logger
	components     []Component
	beforeShutdown []func()
	signals        []os.Signal
}

func newOptions(opts []Option) options {
	o := options{
		log: &zlog.Logger,
		// Handling termination signals is the common case, so it is the default;
		// WithSignals with no arguments opts out.
		signals: []os.Signal{syscall.SIGINT, syscall.SIGTERM},
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithLogger sets the logger used for lifecycle events. Defaults to zerolog's
// package-level logger.
func WithLogger(log *zerolog.Logger) Option {
	return func(o *options) {
		if log != nil {
			o.log = log
		}
	}
}

// WithComponents registers components. Registration order is start order, and
// shutdown runs in reverse — so register the components others depend on first,
// and they will be the last to stop. Repeated calls accumulate.
func WithComponents(components ...Component) Option {
	return func(o *options) {
		o.components = append(o.components, components...)
	}
}

// BeforeShutdown registers a function to run once shutdown begins and before any
// component is stopped. Use it to flip a readiness flag: every port is still
// open at that point, so a load balancer can observe the instance leaving
// service before anything stops accepting connections.
//
// Hooks run in registration order, on the goroutine that called Run, and they
// spend Config.ShutdownTimeout: the clock starts before the first one, so
// whatever they take is taken from the components' drain. Being on Run's
// goroutine, a hook that never returns is not preempted by that timeout either —
// it stops the shutdown where it stands. Keep them to setting a flag.
func BeforeShutdown(hooks ...func()) Option {
	return func(o *options) {
		for _, h := range hooks {
			if h != nil {
				o.beforeShutdown = append(o.beforeShutdown, h)
			}
		}
	}
}

// WithSignals replaces the signals that trigger shutdown. Calling it with no
// arguments installs no signal handler at all, leaving Run to shut down only
// when its context is cancelled — which is what a test or an embedded use
// wants.
func WithSignals(signals ...os.Signal) Option {
	return func(o *options) {
		o.signals = signals
	}
}
