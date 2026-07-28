package migrate

import (
	"log/slog"

	"github.com/pressly/goose/v3"
)

// Option configures a Migrator. The set is closed: only this package can define
// one.
type Option func(*options)

type options struct {
	log          *slog.Logger
	goMigrations []*goose.Migration
}

func newOptions(opts []Option) options {
	// The default keeps New usable with no options at all, sending migration
	// records wherever slog's default handler points.
	o := options{log: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// WithLogger sets the logger migrations are reported through — one record per
// migration, carrying its source, direction and duration. Defaults to
// slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(o *options) {
		if log != nil {
			o.log = log
		}
	}
}

// WithGoMigrations registers migrations written in Go, for the changes SQL cannot
// express: a backfill that has to parse a column, re-encrypt a field, or call
// something outside the database. Build them with goose.NewGoMigration, whose
// version has to agree with the surrounding .sql filenames since the two are
// merged into one ordered sequence.
//
// It is the alternative to goose's package-level registry, which a Migrator also
// picks up: passing them in keeps the set explicit and per-Migrator, rather than
// depending on which packages an import graph happened to pull in for their init
// functions. Repeated calls accumulate.
func WithGoMigrations(migrations ...*goose.Migration) Option {
	return func(o *options) { o.goMigrations = append(o.goMigrations, migrations...) }
}
