package telemetry

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/rs/zerolog"
)

// LogConfig describes the application logger. See Config for why the
// mapstructure tags are here.
type LogConfig struct {
	// Level is a zerolog level name: trace, debug, info, warn, error, fatal or
	// panic.
	Level string `mapstructure:"level"`
	// Format is "console" for colored human-readable output, anything else for
	// JSON.
	Format string `mapstructure:"format"`
}

// LogOption configures NewLogger. The set is closed: only this package can
// define one.
type LogOption func(*logOptions)

type logOptions struct {
	out io.Writer
}

// WithWriter sends log output somewhere other than stderr.
func WithWriter(w io.Writer) LogOption {
	return func(o *logOptions) {
		if w != nil {
			o.out = w
		}
	}
}

// NewLogger builds the application logger: JSON in production, colored console
// output for local development. Records emitted with a context that carries an
// active span also carry trace_id and span_id, so logs join traces, and each
// record carries the call site that produced it.
//
// The returned logger is installed as the slog default, which also routes the
// standard library's log package through it.
//
// slog is the front end and zerolog the back end. Fields therefore follow
// zerolog's naming — level, message, time — because renaming them means
// assigning to zerolog's package-level variables, which would reach into every
// other use of zerolog in the importing process.
func NewLogger(cfg LogConfig, opts ...LogOption) (*slog.Logger, error) {
	level, err := zerolog.ParseLevel(cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", cfg.Level, err)
	}

	o := logOptions{out: os.Stderr}
	for _, opt := range opts {
		opt(&o)
	}

	w := o.out
	if cfg.Format == "console" {
		w = zerolog.ConsoleWriter{Out: o.out, TimeFormat: time.RFC3339}
	}

	// Deliberately no .Timestamp(): the slog handler emits the record's own
	// time, and a timestamp hook here would duplicate the field.
	zl := zerolog.New(w).Level(level)

	// The correlation wrapper must stay outermost — it is what keeps trace_id,
	// span_id and caller at the top level of the record.
	logger := slog.New(NewCorrelationHandler(zerolog.NewSlogHandler(zl)))
	slog.SetDefault(logger)
	return logger, nil
}
