// Package db is the composition root for the `db` command: it opens a pool sized
// for one migration, points pkg/migrate at the embedded migrations, and owns both
// until the command returns.
//
// It is a sibling of internal/app/api rather than a part of it, because a
// migration is a different process invocation with different needs — see
// pkg/migrate's package comment for why it is not something the server does on
// the way up.
package db

import (
	"context"
	"errors"
	"log/slog"

	"github.com/koungkub/tehran/internal/config"
	"github.com/koungkub/tehran/internal/migrations"
	"github.com/koungkub/tehran/pkg/database"
	"github.com/koungkub/tehran/pkg/migrate"
	"github.com/koungkub/tehran/pkg/telemetry"
)

// poolName labels this pool in its log lines and metric attributes, so a
// migration's connections are not mistaken for the API server's.
const poolName = "migrate"

// poolConns is what a migration needs open at once. Two, not one: a session-level
// advisory lock is held on its own connection for the whole run, so the statements
// need a second, and a table lock's heartbeat wants the same. One is not merely
// slow but deadlocked, and goose guards against it explicitly — a pool of exactly
// one with a session locker configured is rejected rather than hung.
//
// More would not help: migrations are strictly sequential, and every connection a
// migration Job holds is one the running service cannot.
const poolConns = 2

// App is the wired-up db command: a pool and the migrator over it.
type App struct {
	log      *slog.Logger
	pool     *database.DB
	migrator *migrate.Migrator
}

// New opens the pool and builds the migrator. Nothing is applied here.
//
// It ignores config.Database.Enabled, deliberately. That flag answers whether the
// api server needs a database, and the deploy order is the other way round: the
// schema is migrated before the release that reads it, so requiring the flag
// would mean turning it on one release early, in a config change with no code
// behind it. A section with no database name still fails here, from
// database.ErrMissingDatabase.
func New(ctx context.Context, cfg *config.Config) (*App, error) {
	log, err := telemetry.NewLogger(cfg.Log)
	if err != nil {
		return nil, err
	}

	// No OTLP pipeline, unlike the api command. A migration is the step a deploy
	// is gated on, so it gets the smallest possible set of things that can fail
	// before the schema changes — and a process that exits in two seconds is a
	// poor fit for a batching exporter anyway. goose reports every migration
	// through the logger above, which is where a job's output is read from.
	poolCfg := cfg.Database.Config
	poolCfg.MaxOpenConns, poolCfg.MaxIdleConns = poolConns, poolConns
	// Prepared-statement caching buys nothing here — no statement runs twice —
	// and DDL is exactly where a cached plan against a table this run is
	// rewriting is worth avoiding.
	poolCfg.PrepareStmt = false

	pool, err := database.Open(ctx, poolCfg,
		database.WithName(poolName),
		database.WithLogger(log),
	)
	if err != nil {
		return nil, err
	}

	migrateCfg := cfg.Migrate
	if migrateCfg.Dialect == "" {
		// The dialect follows the driver the pool was opened with rather than
		// being configured twice. Set [migrate].dialect only for a driver
		// pkg/database does not name — the two are then the caller's to keep
		// consistent.
		migrateCfg.Dialect = poolCfg.Driver
	}

	migrator, err := migrate.New(pool.SQL(), migrations.FS(), migrateCfg,
		migrate.WithLogger(log),
	)
	if err != nil {
		// The pool is already open, so dropping the App here would leak it.
		return nil, errors.Join(err, pool.Close())
	}

	return &App{log: log, pool: pool, migrator: migrator}, nil
}

// Migrate applies every pending migration, or every one up to version when it is
// positive.
//
// The migrations that were applied are returned even when the error is non-nil: a
// run that failed half way leaves the ones before it in place, and that is the
// first thing anybody diagnosing it needs.
func (a *App) Migrate(ctx context.Context, version int64) ([]migrate.Applied, error) {
	if version > 0 {
		return a.migrator.UpTo(ctx, version)
	}
	return a.migrator.Up(ctx)
}

// Status lists every migration on the filesystem or in the version table.
func (a *App) Status(ctx context.Context) ([]migrate.Status, error) {
	return a.migrator.Status(ctx)
}

// Version reports the highest version recorded in the database, and whether
// anything is still pending — the pair a deploy gate needs, and cheaper to read
// together than to ask for twice.
func (a *App) Version(ctx context.Context) (version int64, pending bool, err error) {
	if version, err = a.migrator.Version(ctx); err != nil {
		return 0, false, err
	}
	pending, err = a.migrator.HasPending(ctx)
	return version, pending, err
}

// Close closes the pool. The migrator holds nothing of its own: it was handed the
// pool rather than opening one, which is also why it has no Close to call here.
func (a *App) Close() error { return a.pool.Close() }
