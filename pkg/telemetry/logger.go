package telemetry

import (
	"fmt"
	"io"
	stdlog "log"
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
	// Caller adds the file:line that produced each record, through zerolog's own
	// caller hook.
	//
	// It is off by default because it is the most expensive thing on a log line by
	// a wide margin: resolving a call site costs roughly 800ns against 240ns for
	// the rest of a four-field record, so turning it on roughly quadruples what a
	// log line costs. It also earns very little here — every line this repository
	// emits already has a message unique to one call site, and the per-RPC lines
	// carry a `procedure` field that identifies the code better than a file:line
	// would. Turn it on locally, or in production once the volume is known to
	// afford it.
	//
	// The field holds zerolog's format, which is the *full* path recorded at build
	// time. Shortening it means assigning to zerolog.CallerMarshalFunc, a
	// package-level variable that would reach into every other use of zerolog in
	// the importing process — the same reason the field names are left alone.
	Caller bool `mapstructure:"caller"`
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
// active span also carry trace_id and span_id, so logs join traces.
//
// The returned logger is installed as the destination of the standard library's
// log package, so anything logging through that ends up in the same stream.
//
// Fields follow zerolog's naming — level, message, time — because renaming them
// means assigning to zerolog's package-level variables, which would reach into
// every other use of zerolog in the importing process.
func NewLogger(cfg LogConfig, opts ...LogOption) (*zerolog.Logger, error) {
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

	// Timestamp is explicit: zerolog.New installs no timestamp hook, so without
	// it a record carries no time at all.
	fields := zerolog.New(w).Level(level).With().Timestamp()
	if cfg.Caller {
		// zerolog's own caller hook, rather than resolving the frame in
		// correlationHook. A hook is handed no call site and would have to search
		// the stack for one, which costs about three times as much and is fragile:
		// the distance to the call site varies with which terminator was used and
		// with what the compiler inlined. zerolog resolves it from a fixed depth
		// that its own Msg/Msgf/Send/MsgFunc all share.
		fields = fields.Caller()
	}
	logger := fields.Logger().Hook(correlationHook{})

	// zerolog.Logger is an io.Writer that logs whatever is written to it, which
	// is what routes the standard library's log package here. The flags go
	// because the timestamp and the level are this logger's job, and leaving
	// them on would prefix the message with a second date.
	stdlog.SetFlags(0)
	stdlog.SetOutput(logger)

	return &logger, nil
}
