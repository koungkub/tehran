package ops

import (
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

// Option configures a Server. The set is closed: only this package can define
// one.
type Option func(*options)

// readyCheck is a named readiness condition.
type readyCheck struct {
	name string
	fn   func(context.Context) error
}

type options struct {
	name        string
	log         *zerolog.Logger
	registry    *prometheus.Registry
	readyChecks []readyCheck
}

func newOptions(opts []Option) options {
	o := options{name: DefaultName, log: &zlog.Logger}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithName labels the server in its own log lines and, through Name, wherever a
// supervisor reports on it. Defaults to DefaultName.
func WithName(name string) Option {
	return func(o *options) {
		if name != "" {
			o.name = name
		}
	}
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

// WithRegistry serves registry on /metrics. Without it the endpoint is not
// mounted at all, which is honest: a 404 is easier to diagnose than an empty
// scrape.
func WithRegistry(registry *prometheus.Registry) Option {
	return func(o *options) { o.registry = registry }
}

// WithReadyCheck registers a condition /readyz must satisfy. Repeated calls
// accumulate and all checks must pass; the first failure names itself in the
// 503 body. With no checks registered /readyz always reports ready.
//
// The check receives the probe request's context, so it must be fast and must
// honour cancellation. Typical uses are a drain flag the application flips at
// the start of shutdown, or a dependency's own health call.
func WithReadyCheck(name string, fn func(context.Context) error) Option {
	return func(o *options) {
		if fn != nil {
			o.readyChecks = append(o.readyChecks, readyCheck{name: name, fn: fn})
		}
	}
}
