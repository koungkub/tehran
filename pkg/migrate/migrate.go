// Package migrate applies versioned SQL migrations to a database, over
// pressly/goose.
//
// It is deliberately not part of package database, and not something a server
// does on the way up. A migration runs from a separate process invocation: it
// needs a role that may ALTER, it must not race the other replicas starting
// beside it, its failure has to fail a deploy step rather than crash-loop a
// service, and rolling an image back must not roll a schema back with it. What
// this package owns is the part that is easy to get wrong once that decision is
// made — mutual exclusion between concurrent runners, a bound on the whole run,
// and reporting what was applied when something fails halfway.
//
// It takes an open *sql.DB rather than opening one, so the pool, its timeouts
// and its instrumentation stay the caller's decision — package database is one
// way to supply it, and is not required.
//
// Migrations are read from an fs.FS, which is what lets them be embedded into
// the binary that applies them. goose globs the root of that filesystem and does
// not recurse, so the .sql files have to sit at the root: an embed.FS declared
// alongside them already is, and a subdirectory needs fs.Sub.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"time"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/rs/zerolog"
)

// Dialects with a lock implementation behind them. Config.Dialect accepts any
// name goose knows — sqlite3, clickhouse, mssql and the rest — but LockMode
// above LockModeNone is only implementable on Postgres today.
const (
	DialectPostgres = string(goose.DialectPostgres)
	DialectMySQL    = string(goose.DialectMySQL)
)

// Lock modes. See Config.LockMode.
const (
	// LockModeSession takes a PostgreSQL session-level advisory lock, held on
	// one connection for the duration of the run and released by the server if
	// the process dies. It is the right choice against a database.
	LockModeSession = "session"
	// LockModeTable takes a lease on a row in a lock table, refreshed by a
	// heartbeat. It costs a table and a background goroutine, and it is what
	// works through a connection pooler in transaction mode, where consecutive
	// statements are not guaranteed the same session and an advisory lock taken
	// on one connection is therefore invisible from the next.
	LockModeTable = "table"
	// LockModeNone runs unlocked. Two runners starting together will both try to
	// apply the same migration.
	LockModeNone = "none"
)

// Defaults substituted by New for zero-valued Config fields.
const (
	DefaultDialect   = DialectPostgres
	DefaultLockMode  = LockModeSession
	DefaultTableName = goose.DefaultTablename
	// DefaultLockID is goose's own, so a schema already managed by the goose CLI
	// is locked against the same identifier rather than beside it.
	DefaultLockID = lock.DefaultLockID
	// DefaultLockWait is how long a runner waits for a lock another runner holds.
	// It has to cover that other runner's whole migration, not just its own
	// start-up: two replicas of a migration Job racing is the case this exists
	// for, and the loser waiting five minutes is the correct outcome.
	DefaultLockWait = 5 * time.Minute
)

// lockRetryPeriod is the interval between lock attempts. goose expresses a lock
// timeout as a period and a retry count rather than a duration, so Config.LockWait
// is divided by this to produce the count.
const lockRetryPeriod = 5 * time.Second

// Config describes where the version table lives, how concurrent runners are
// kept apart, and what bounds a run.
//
// The mapstructure tags are inert metadata: they let a viper-based service nest
// this struct directly into its own configuration without a conversion layer.
type Config struct {
	// Dialect selects the version-table SQL: postgres, mysql, sqlite3, and the
	// other names goose understands. It is not the same choice as the driver the
	// pool was opened with, but it has to agree with it — pass the driver name
	// through rather than configuring this twice.
	Dialect string `mapstructure:"dialect"`
	// TableName holds one row per applied version. Renaming it after the first
	// migration has run orphans the history, and the next run will try to apply
	// everything again.
	TableName string `mapstructure:"table_name"`
	// LockMode decides how two runners starting at the same time are kept apart:
	// session, table, or none. Anything above none currently requires Postgres,
	// because goose implements no locker for another dialect — see LockModeNone
	// for what running without one means.
	LockMode string `mapstructure:"lock_mode"`
	// LockID identifies the lock. Two services migrating two schemas in the same
	// PostgreSQL database share the advisory-lock namespace, which is per
	// database and not per schema, so they serialise against each other unless
	// they are given different IDs.
	LockID int64 `mapstructure:"lock_id"`
	// LockWait bounds how long this runner waits for a lock somebody else holds
	// before giving up. It is rounded up to a multiple of five seconds, which is
	// the retry interval.
	LockWait time.Duration `mapstructure:"lock_wait"`
	// AllowOutOfOrder applies a migration whose version is below the highest one
	// already applied, rather than refusing. Off, deliberately: two branches
	// merging with interleaved versions is exactly the case where applying
	// quietly leaves two environments with the same version number and a
	// different schema.
	AllowOutOfOrder bool `mapstructure:"allow_out_of_order"`
	// Timeout bounds a whole Up or Down, lock wait included, and is off by
	// default — unlike every other duration here, where a non-positive value
	// means "use the default". A concurrent index build on a large table
	// legitimately runs for an hour, and a ceiling low enough to be useful
	// against a hung migration is also low enough to cut that one off partway.
	// The right backstop is usually the deadline on the job that runs this.
	Timeout time.Duration `mapstructure:"timeout"`
}

func (c Config) withDefaults() Config {
	if c.Dialect == "" {
		c.Dialect = DefaultDialect
	}
	if c.TableName == "" {
		c.TableName = DefaultTableName
	}
	if c.LockMode == "" {
		c.LockMode = DefaultLockMode
	}
	if c.LockID == 0 {
		c.LockID = DefaultLockID
	}
	if c.LockWait <= 0 {
		c.LockWait = DefaultLockWait
	}
	return c
}

// lockRetries converts LockWait into the retry count goose wants, rounding up so
// that a wait shorter than one period still gets one attempt.
func (c Config) lockRetries() uint64 {
	return max(1, uint64(math.Ceil(float64(c.LockWait)/float64(lockRetryPeriod))))
}

// lockOption builds the provider option for LockMode, or nil for LockModeNone.
//
// The table locker is built without a logger: goose's lock.WithTableLogger takes
// a *slog.Logger and has no other form, so there is nothing to hand it here. It
// then installs its own error-level text handler onto stderr, which means a
// contended table lease reports outside this service's log stream and format.
// That is confined to LockModeTable — LockModeSession, the default, has no
// logger at all — and to errors.
func (c Config) lockOption() (goose.ProviderOption, error) {
	switch c.LockMode {
	case LockModeNone:
		return nil, nil

	case LockModeSession:
		if c.Dialect != DialectPostgres {
			return nil, unlockableError(c.Dialect, c.LockMode)
		}
		locker, err := lock.NewPostgresSessionLocker(
			lock.WithLockID(c.LockID),
			lock.WithLockTimeout(uint64(lockRetryPeriod/time.Second), c.lockRetries()),
		)
		if err != nil {
			return nil, fmt.Errorf("migrate: session locker: %w", err)
		}
		return goose.WithSessionLocker(locker), nil

	case LockModeTable:
		if c.Dialect != DialectPostgres {
			return nil, unlockableError(c.Dialect, c.LockMode)
		}
		locker, err := lock.NewPostgresTableLocker(
			lock.WithTableName(c.TableName+"_lock"),
			lock.WithTableLockID(c.LockID),
			lock.WithTableLockTimeout(lockRetryPeriod, c.lockRetries()),
		)
		if err != nil {
			return nil, fmt.Errorf("migrate: table locker: %w", err)
		}
		return goose.WithLocker(locker), nil
	}
	return nil, fmt.Errorf("migrate: unknown lock_mode %q (use %q, %q or %q)",
		c.LockMode, LockModeSession, LockModeTable, LockModeNone)
}

// unlockableError refuses rather than silently downgrading to LockModeNone. A
// service that has to run unlocked should say so in its configuration, because
// the consequence — two runners applying the same migration — is not something
// the next person reading that configuration can infer from its absence.
func unlockableError(dialect, mode string) error {
	return fmt.Errorf(
		"migrate: lock_mode %q needs dialect %q, not %q: goose implements no locker for it, "+
			"so set lock_mode to %q and make sure only one migration runs at a time",
		mode, DialectPostgres, dialect, LockModeNone)
}

// Applied describes one migration that ran.
type Applied struct {
	Version int64
	// Source is the migration's path within the filesystem it came from, and is
	// empty for a Go migration registered by hand.
	Source    string
	Direction string
	Duration  time.Duration
	// Empty reports a migration that was recorded but carried no statements: a
	// placeholder, or a Go migration with a nil function.
	Empty bool
}

// Source is a migration a Migrator knows about, applied or not.
type Source struct {
	Version int64
	// Path is the migration's path within the filesystem it came from, and is
	// empty for a Go migration registered by hand.
	Path string
}

// Status is one migration's state — known on the filesystem, recorded in the
// database, or both.
type Status struct {
	Version int64
	Source  string
	Applied bool
	// AppliedAt is the zero time for a migration that has not been applied.
	AppliedAt time.Time
}

// Migrator applies and reports on migrations.
//
// It has no Close: the pool was opened by the caller and is closed by the
// caller, which also means one pool can serve a migration and whatever runs
// after it in the same process.
type Migrator struct {
	provider *goose.Provider
	// reader is the same set of migrations over the same pool with locking
	// switched off, and is what the read-only methods use.
	//
	// It exists because goose enables or disables locking per provider and then
	// takes the lock inside Status and GetDBVersion — which are exactly the calls
	// somebody makes *while* a migration is running, and which would therefore
	// block for as long as lock_wait allows. Whether to lock is not a property of
	// a provider; it is a property of the operation, and reads do not need it.
	// They can see a schema mid-migration instead, which for a diagnostic beats a
	// five-minute wait.
	reader  *goose.Provider
	log     *zerolog.Logger
	timeout time.Duration
	target  string
}

// New builds a Migrator over an open pool and a filesystem of migrations.
//
// db is not closed by anything here. fsys is globbed at its root and not walked,
// so `//go:embed *.sql` declared beside the migrations is ready to pass in and a
// subdirectory needs fs.Sub first — a filesystem with no migration in its root
// is an error rather than a run that applies nothing.
func New(db *sql.DB, fsys fs.FS, cfg Config, opts ...Option) (*Migrator, error) {
	if db == nil {
		return nil, errors.New("migrate: nil *sql.DB")
	}
	if fsys == nil {
		return nil, errors.New("migrate: nil fs.FS")
	}
	cfg = cfg.withDefaults()
	o := newOptions(opts)

	// Shared by both providers below. Everything that decides *what* the
	// migrations are belongs here; only locking differs between them.
	baseOpts := []goose.ProviderOption{
		// WithLogger rather than WithSlog: this package's logger is a
		// *zerolog.Logger and goose's structured option takes a *slog.Logger
		// only, so the two are bridged by the Printf adapter below. The cost is
		// that goose's own fields — source, direction, duration_seconds, state —
		// arrive as part of a formatted message rather than as attributes, since
		// goose renders a separate legacy string for this path. The lines still
		// land in whatever stream and format the service already logs to, which
		// is the part that matters for a deploy's record.
		goose.WithLogger(gooseLogger{log: o.log}),
		// Not a knob. goose gates every log line on this flag, so without it
		// WithLogger above is wired to nothing and a migration run is silent —
		// and applying a schema change is a rare, irreversible event whose record
		// is the only account of what a deploy did to the database.
		goose.WithVerbose(true),
		goose.WithTableName(cfg.TableName),
		goose.WithAllowOutofOrder(cfg.AllowOutOfOrder),
	}
	if len(o.goMigrations) > 0 {
		baseOpts = append(baseOpts, goose.WithGoMigrations(o.goMigrations...))
	}
	lockOpt, err := cfg.lockOption()
	if err != nil {
		return nil, err
	}

	writerOpts := baseOpts
	if lockOpt != nil {
		// A fresh slice: appending to baseOpts in place would put the lock option
		// into the reader's options too, on any call where baseOpts had spare
		// capacity — which is the whole bug this is here to avoid.
		writerOpts = append(append([]goose.ProviderOption{}, baseOpts...), lockOpt)
	}

	provider, err := newProvider(cfg, db, fsys, writerOpts)
	if err != nil {
		return nil, err
	}
	reader, err := newProvider(cfg, db, fsys, baseOpts)
	if err != nil {
		return nil, err
	}

	return &Migrator{
		provider: provider,
		reader:   reader,
		log:      o.log,
		timeout:  cfg.Timeout,
		target:   cfg.Dialect + "/" + cfg.TableName,
	}, nil
}

func newProvider(cfg Config, db *sql.DB, fsys fs.FS, opts []goose.ProviderOption) (*goose.Provider, error) {
	provider, err := goose.NewProvider(goose.Dialect(cfg.Dialect), db, fsys, opts...)
	if err != nil {
		if errors.Is(err, goose.ErrNoMigrations) {
			// The overwhelmingly likely cause, and one goose cannot see: goose
			// globs the root of the filesystem it is given, so an embed.FS
			// declared with a directory prefix looks empty from in there.
			return nil, fmt.Errorf(
				"migrate: %w in the root of the filesystem given "+
					"(goose globs *.sql at the root and does not recurse: use fs.Sub for a subdirectory)", err)
		}
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return provider, nil
}

// Up applies every pending migration, in version order.
//
// A migration that fails leaves the ones before it applied, and those are
// returned alongside the error rather than discarded: what a half-finished run
// actually did is the first thing anybody needs to know, and goose reports it
// only through the error.
func (m *Migrator) Up(ctx context.Context) ([]Applied, error) {
	ctx, cancel := m.bound(ctx)
	defer cancel()
	return appliedFrom(m.provider.Up(ctx))
}

// UpTo applies pending migrations no higher than version, which is what a deploy
// pinned to a particular schema version uses.
func (m *Migrator) UpTo(ctx context.Context, version int64) ([]Applied, error) {
	ctx, cancel := m.bound(ctx)
	defer cancel()
	return appliedFrom(m.provider.UpTo(ctx, version))
}

// Down rolls back the highest applied migration.
//
// Rolling a schema back is not the inverse of rolling code back, and it is
// rarely the right move against a live database: the down of a dropped column
// restores the column, not the data that was in it. Prefer a new forward
// migration.
func (m *Migrator) Down(ctx context.Context) (Applied, error) {
	ctx, cancel := m.bound(ctx)
	defer cancel()
	result, err := m.provider.Down(ctx)
	if err != nil {
		return Applied{}, fmt.Errorf("migrate: %w", err)
	}
	return appliedOf(result), nil
}

// DownTo rolls back until the database is at version. Read Down first.
func (m *Migrator) DownTo(ctx context.Context, version int64) ([]Applied, error) {
	ctx, cancel := m.bound(ctx)
	defer cancel()
	return appliedFrom(m.provider.DownTo(ctx, version))
}

// Status lists every migration known on the filesystem or recorded in the
// database, in version order.
//
// It takes no lock, deliberately, which is what lets it report on a run that is
// in progress rather than queueing behind it for as long as LockWait allows. The
// cost is that a run in progress may be caught half way, with a version applied a
// moment ago still reading as pending.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	statuses, err := m.reader.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	out := make([]Status, 0, len(statuses))
	for _, s := range statuses {
		st := Status{
			AppliedAt: s.AppliedAt,
			Applied:   s.State == goose.StateApplied,
		}
		if s.Source != nil {
			st.Version, st.Source = s.Source.Version, s.Source.Path
		}
		out = append(out, st)
	}
	return out, nil
}

// Version reports the highest version recorded in the database, or 0 when none
// is. It is the highest, not the last applied: an out-of-order run leaves those
// two different. Like Status, it takes no lock.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	version, err := m.reader.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate: %w", err)
	}
	return version, nil
}

// HasPending reports whether anything is left to apply. Like Status it takes no
// lock, so it is safe to call from something that must not block — a deploy gate
// that refuses to roll out code ahead of its schema, for instance.
func (m *Migrator) HasPending(ctx context.Context) (bool, error) {
	pending, err := m.reader.HasPending(ctx)
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return pending, nil
}

// Sources lists the migrations this Migrator knows about, applied or not, in
// version order. It touches no database.
func (m *Migrator) Sources() []Source {
	sources := m.reader.ListSources()
	out := make([]Source, 0, len(sources))
	for _, s := range sources {
		out = append(out, Source{Version: s.Version, Path: s.Path})
	}
	return out
}

// bound applies Config.Timeout. It always returns a cancel func, so callers can
// defer it unconditionally.
func (m *Migrator) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, m.timeout)
}

// appliedFrom converts goose's results, and digs the successful ones out of a
// PartialError: goose returns a nil slice alongside it, so the error is the only
// place the migrations that did land are recorded.
func appliedFrom(results []*goose.MigrationResult, err error) ([]Applied, error) {
	if partial, ok := errors.AsType[*goose.PartialError](err); ok {
		return appliedSlice(partial.Applied), fmt.Errorf("migrate: %w", err)
	}
	if err != nil {
		return appliedSlice(results), fmt.Errorf("migrate: %w", err)
	}
	return appliedSlice(results), nil
}

func appliedSlice(results []*goose.MigrationResult) []Applied {
	out := make([]Applied, 0, len(results))
	for _, r := range results {
		out = append(out, appliedOf(r))
	}
	return out
}

func appliedOf(r *goose.MigrationResult) Applied {
	if r == nil {
		return Applied{}
	}
	a := Applied{
		Direction: r.Direction,
		Duration:  r.Duration,
		Empty:     r.Empty,
	}
	if r.Source != nil {
		a.Version, a.Source = r.Source.Version, r.Source.Path
	}
	return a
}
