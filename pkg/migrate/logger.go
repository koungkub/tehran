package migrate

import "github.com/rs/zerolog"

// gooseLogger adapts goose's logger interface onto *zerolog.Logger, so a
// migration run lands in the same stream and format as everything else the
// process logs, and this package exposes no logging type of its own.
//
// goose offers two mutually exclusive logging options: WithSlog, which takes a
// *slog.Logger and emits attributes, and WithLogger, which takes this Printf and
// Fatalf pair. Only the second can be satisfied by a zerolog logger, so goose's
// fields — source, direction, duration_seconds, state, current_version — arrive
// inside the message rather than beside it. goose renders a separate string for
// this path, so nothing is lost from the account of what ran; it is just not
// queryable by field.
type gooseLogger struct {
	log *zerolog.Logger
}

var _ interface {
	Printf(string, ...any)
	Fatalf(string, ...any)
} = gooseLogger{}

// Printf carries every line goose emits during a run. Info is the right level:
// goose gates all of it behind WithVerbose, and what comes through is the record
// of a schema change rather than a diagnostic.
func (g gooseLogger) Printf(format string, v ...any) {
	g.log.Info().Msgf(format, v...)
}

// Fatalf logs at error and returns, rather than exiting.
//
// goose only calls it from its own cmd/goose binary, never from the Provider API
// this package drives, so in practice it is unreachable from here. It is
// implemented as a log because the alternative is a library calling os.Exit on
// its caller's behalf, which would take the process down with no chance to
// report what was applied — the very thing Up goes to some length to preserve.
func (g gooseLogger) Fatalf(format string, v ...any) {
	g.log.Error().Msgf(format, v...)
}
