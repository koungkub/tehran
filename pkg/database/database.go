// Package database opens an instrumented GORM connection to a SQL database and
// owns the connection pool in front of it.
//
// It is persistence infrastructure only: it knows nothing about models,
// migrations or repositories, which belong to the domains that define them. What
// it does own is everything a service gets wrong when each domain opens its own
// handle — pool sizing, a startup connect that cannot hang forever, a readiness
// probe, per-statement logs that join the request's trace, spans and metrics.
//
// Telemetry arrives as the OpenTelemetry provider interfaces rather than as a
// concrete setup, so this package stays independent of how tracing and metrics
// are configured, and works with none at all.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Drivers Config.Driver understands. Anything else needs WithDialector.
const (
	DriverPostgres = "postgres"
	DriverMySQL    = "mysql"
)

// Defaults substituted by Open for zero-valued Config fields.
const (
	DefaultDriver = DriverPostgres
	// DefaultMaxOpenConns is per process, and every replica opens its own pool:
	// the server's max_connections has to cover MaxOpenConns × replicas, plus
	// whatever migrations and operators use.
	DefaultMaxOpenConns = 25
	// DefaultConnMaxLifetime rotates connections so that a failover, a DNS
	// change or a rolling restart of the database is picked up without
	// restarting this process.
	DefaultConnMaxLifetime = 30 * time.Minute
	DefaultConnMaxIdleTime = 5 * time.Minute
	DefaultConnectTimeout  = 5 * time.Second
	// DefaultSlowQueryThreshold matches GORM's own, and is the point at which a
	// statement is logged at warn level rather than debug.
	DefaultSlowQueryThreshold = 200 * time.Millisecond
)

// DefaultName labels the pool in logs, in its metric attributes, and to a
// readiness check. Override it with WithName when a process opens more than one.
const DefaultName = "database"

// ErrMissingDatabase is returned by Open when Config carries neither a DSN nor
// enough to build one.
var ErrMissingDatabase = errors.New("database: dsn or database name required")

// Config describes the connection and the pool in front of it.
//
// The mapstructure tags are inert metadata: they let a viper-based service nest
// this struct directly into its own configuration without a conversion layer.
//
// The pool settings are the ones worth being deliberate about. database/sql
// hands out connections and blocks callers when there are none left, so the pool
// is a queue in front of the database: too small and requests wait behind each
// other, too large and the database runs out of connections or spends its time
// context-switching. A request waiting for a connection is bounded by its own
// context — the RPC server's per-request deadline covers it — so a saturated pool
// shows up as deadline_exceeded on the caller and as
// db.client.connection.wait.count climbing here.
type Config struct {
	// Driver selects the dialector: postgres or mysql. Anything else — sqlite,
	// a cloud proxy, a test double — is passed in with WithDialector instead,
	// which ignores this field and every field below down to MaxOpenConns.
	Driver string `mapstructure:"driver"`
	// DSN is used verbatim when set, and the host/user/password fields are
	// ignored. Use it for the connection strings a platform hands you whole
	// (a Kubernetes secret, a managed-database URL) rather than picking them
	// apart to reassemble here.
	DSN  string `mapstructure:"dsn"`
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
	User string `mapstructure:"user"`
	// Password belongs in the environment, not in a config file that is checked
	// in. Nothing here ever logs it: errors and log lines carry Target(), which
	// is credential-free by construction.
	Password string `mapstructure:"password"`
	Database string `mapstructure:"database"`
	// SSLMode is PostgreSQL's sslmode parameter (disable, require,
	// verify-full, …). Empty leaves it out of the DSN entirely, so the driver's
	// own default applies rather than one chosen here.
	SSLMode string `mapstructure:"ssl_mode"`
	// MaxOpenConns caps connections in use and idle together. It is the pool's
	// real limit; see the note on Config for what happens when it is reached.
	MaxOpenConns int `mapstructure:"max_open_conns"`
	// MaxIdleConns caps the connections kept open when nothing is using them.
	// It defaults to MaxOpenConns, deliberately: database/sql closes any
	// connection returned to a full idle set, so a lower value makes a service
	// at its peak open and close connections continuously — the default of 2 in
	// database/sql itself is the classic cause of that churn.
	MaxIdleConns int `mapstructure:"max_idle_conns"`
	// ConnMaxLifetime bounds a connection's total age. Keep it well under any
	// idle-connection timeout the database or a proxy in front of it enforces,
	// so this side closes connections first rather than discovering them dead.
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	// ConnMaxIdleTime closes a connection that has gone unused, releasing
	// server-side resources during quiet periods.
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
	// ConnectTimeout bounds the startup ping Open performs and any later Ping
	// whose caller supplied no deadline, and — when the DSN is built from the
	// fields above rather than supplied whole — is passed to the driver as its
	// own connect timeout, which matters long after startup because the pool
	// opens connections lazily as load grows.
	ConnectTimeout time.Duration `mapstructure:"connect_timeout"`
	// SlowQueryThreshold is the duration above which a statement is logged at
	// warn level. Like every duration here, a non-positive value means "use the
	// default"; there is no way to switch it off, because a slow query that
	// nothing reports is the failure this exists to surface.
	SlowQueryThreshold time.Duration `mapstructure:"slow_query_threshold"`
	// PrepareStmt caches a prepared statement per query, which is faster and is
	// what most services want — but it breaks against a connection pooler in
	// transaction or statement mode (PgBouncer, RDS Proxy), where consecutive
	// statements are not guaranteed the same server-side session. Off by
	// default for that reason.
	PrepareStmt bool `mapstructure:"prepare_stmt"`
	// IncludeQueryValues puts bound parameter values into log lines and span
	// attributes. Off by default because those values are the data itself:
	// e-mail addresses, tokens, national identifiers. Left off, both carry the
	// statement with its placeholders intact, which is what a query plan needs
	// anyway. Turn it on in local development, not in production.
	IncludeQueryValues bool `mapstructure:"include_query_values"`
}

func (c Config) withDefaults() Config {
	if c.Driver == "" {
		c.Driver = DefaultDriver
	}
	if c.MaxOpenConns <= 0 {
		c.MaxOpenConns = DefaultMaxOpenConns
	}
	if c.MaxIdleConns <= 0 {
		c.MaxIdleConns = c.MaxOpenConns
	}
	if c.ConnMaxLifetime <= 0 {
		c.ConnMaxLifetime = DefaultConnMaxLifetime
	}
	if c.ConnMaxIdleTime <= 0 {
		c.ConnMaxIdleTime = DefaultConnMaxIdleTime
	}
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = DefaultConnectTimeout
	}
	if c.SlowQueryThreshold <= 0 {
		c.SlowQueryThreshold = DefaultSlowQueryThreshold
	}
	return c
}

// Target names the server and database without credentials, for logs, errors and
// span attributes. A DSN that is not shaped like a URL yields just the driver
// name — this never returns anything that could contain a password.
func (c Config) Target() string {
	c = c.withDefaults()
	if c.DSN != "" {
		host, port, database := dsnServer(c.DSN)
		if host == "" {
			return c.Driver
		}
		target := c.Driver + "://" + host
		if port > 0 {
			target += ":" + strconv.Itoa(port)
		}
		if database != "" {
			target += "/" + database
		}
		return target
	}
	if c.Database == "" {
		return c.Driver
	}
	return c.Driver + "://" + c.address() + "/" + c.Database
}

// address is the host:port to dial, with the port left off when none is
// configured so that the driver's own default applies. Joining a zero port
// instead would dial port 0 — a connection refused where "5432" was meant.
func (c Config) address() string {
	if c.Port <= 0 {
		return c.Host
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// dsnServer reads the server and database back out of a DSN — and only out of
// one actually shaped like a URL.
//
// url.Parse accepts far more than URLs. A libpq key/value DSN
// ("host=… password=…") parses with no error at all and lands whole, password
// included, in Path; a MySQL DSN parses to an opaque with nothing usable in it.
// Taking either at face value would publish the credentials as a span attribute,
// so a parse that yields no host means "nothing safe to read here" rather than
// "use what came back".
func dsnServer(dsn string) (host string, port int, database string) {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "", 0, ""
	}
	port, _ = strconv.Atoi(u.Port())
	return u.Hostname(), port, strings.TrimPrefix(u.Path, "/")
}

// dsn builds the driver DSN from the individual fields, escaping as that driver
// requires: a password with an @ or a space in it is common enough that
// hand-assembling the string is a bug waiting to happen.
func (c Config) dsn() (string, error) {
	if c.DSN != "" {
		return c.DSN, nil
	}
	if c.Database == "" {
		return "", ErrMissingDatabase
	}
	// Both drivers substitute their own default port for an address that carries
	// none, which is what makes leaving Port unset mean "the usual one" rather
	// than "port 0".
	addr := c.address()

	switch c.Driver {
	case DriverPostgres:
		u := url.URL{Scheme: "postgres", Host: addr, Path: "/" + c.Database}
		if c.User != "" {
			u.User = url.UserPassword(c.User, c.Password)
		}
		q := url.Values{}
		if c.SSLMode != "" {
			q.Set("sslmode", c.SSLMode)
		}
		// libpq-style connect_timeout is whole seconds, and 0 means "no
		// timeout" — round up so a sub-second setting cannot disable it.
		q.Set("connect_timeout", strconv.Itoa(max(1, int(math.Ceil(c.ConnectTimeout.Seconds())))))
		u.RawQuery = q.Encode()
		return u.String(), nil

	case DriverMySQL:
		m := mysqldriver.NewConfig()
		m.Net, m.Addr, m.DBName = "tcp", addr, c.Database
		m.User, m.Passwd = c.User, c.Password
		m.Timeout = c.ConnectTimeout
		// Without ParseTime the driver hands back []byte for DATETIME columns
		// and every time.Time field in a model fails to scan. UTC keeps the
		// values independent of wherever the process happens to run.
		m.ParseTime, m.Loc = true, time.UTC
		return m.FormatDSN(), nil
	}
	return "", fmt.Errorf("database: unsupported driver %q (use %q, %q, or WithDialector)",
		c.Driver, DriverPostgres, DriverMySQL)
}

func dialectorFor(driver, dsn string) (gorm.Dialector, error) {
	switch driver {
	case DriverPostgres:
		return postgres.Open(dsn), nil
	case DriverMySQL:
		return mysql.Open(dsn), nil
	}
	return nil, fmt.Errorf("database: unsupported driver %q", driver)
}

// DB is an open pool and the GORM handle over it.
//
// It is deliberately not a lifecycle.Component: there is nothing to serve, and a
// component would be stopped in the middle of the shutdown sequence while
// handlers that still need the database are draining. Close it after the
// supervisor returns instead, so the pool outlives every user of it.
type DB struct {
	gorm   *gorm.DB
	sql    *sql.DB
	log    *slog.Logger
	name   string
	target string
	// pingTimeout bounds a Ping whose caller supplied no deadline. See Ping.
	pingTimeout time.Duration
	unregister  func() error
}

// Open connects, applies the pool settings, registers instrumentation, and
// verifies the connection with a ping bounded by Config.ConnectTimeout.
//
// A failed ping is a failed Open: the pool is closed before returning, so a
// service that decides to carry on without a database is not left holding one.
// ctx bounds only the connect — the returned DB outlives it.
func Open(ctx context.Context, cfg Config, opts ...Option) (*DB, error) {
	cfg = cfg.withDefaults()
	o := newOptions(opts)

	dialector := o.dialector
	if dialector == nil {
		dsn, err := cfg.dsn()
		if err != nil {
			return nil, err
		}
		if dialector, err = dialectorFor(cfg.Driver, dsn); err != nil {
			return nil, err
		}
	}

	gormDB, err := gorm.Open(dialector, &gorm.Config{
		Logger:      newLogger(o.log, cfg),
		PrepareStmt: cfg.PrepareStmt,
		// GORM's own ping takes no context, so it can hang for as long as the
		// driver's dial does. Open does it below, under a deadline.
		DisableAutomaticPing: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Target(), err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.Target(), err)
	}
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	db := &DB{
		gorm:        gormDB,
		sql:         sqlDB,
		log:         o.log,
		name:        o.name,
		target:      cfg.Target(),
		pingTimeout: cfg.ConnectTimeout,
	}

	// The dialector's own name, not Config.Driver: a connection opened through
	// WithDialector ignores that field, so reporting it would label a SQLite or a
	// Spanner connection as whatever Config happened to default to.
	driver := dialector.Name()
	if err := registerInstrumentation(gormDB, cfg, driver, o); err != nil {
		return nil, errors.Join(fmt.Errorf("instrument %s: %w", db.name, err), db.Close())
	}
	unregister, err := registerPoolMetrics(sqlDB, cfg, driver, o)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("instrument %s: %w", db.name, err), db.Close())
	}
	db.unregister = unregister

	for _, p := range o.plugins {
		if err := gormDB.Use(p); err != nil {
			return nil, errors.Join(fmt.Errorf("use plugin %s: %w", p.Name(), err), db.Close())
		}
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := db.Ping(pingCtx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	o.log.LogAttrs(ctx, slog.LevelInfo, db.name+" connected",
		slog.String("target", db.target),
		slog.Int("max_open_conns", cfg.MaxOpenConns),
		slog.Duration("conn_max_lifetime", cfg.ConnMaxLifetime),
	)
	return db, nil
}

// Gorm returns the handle repositories run their queries through. Use the
// context-carrying form — gorm.WithContext(ctx) — or statements produce spans
// and log lines with no trace to join.
func (db *DB) Gorm() *gorm.DB { return db.gorm }

// SQL returns the underlying pool, for the queries GORM is the wrong tool for
// and for anything that needs database/sql directly.
func (db *DB) SQL() *sql.DB { return db.sql }

// Ping verifies a connection can be obtained and used. It is meant to be handed
// to ops.WithReadyCheck as-is.
//
// It reports the pool's health as a caller experiences it, which is why it does
// not bypass the pool: with every connection busy it waits for one to come free.
// That is also why it imposes Config.ConnectTimeout on a context that carries no
// deadline of its own — which an HTTP request's context does not. Without that, a
// readiness probe against an exhausted pool or an unresponsive server would hang
// in the handler instead of answering 503, and an orchestrator waiting on a reply
// it never gets cannot tell "not ready" from "not answering".
func (db *DB) Ping(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok && db.pingTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, db.pingTimeout)
		defer cancel()
	}
	if err := db.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("ping %s: %w", db.target, err)
	}
	return nil
}

// Stats reports the pool's current state.
func (db *DB) Stats() sql.DBStats { return db.sql.Stats() }

// Name identifies the pool in logs, in metric attributes, and to a readiness
// check.
func (db *DB) Name() string { return db.name }

// Close stops the metric callback and closes the pool. In-flight queries are not
// cancelled — database/sql waits for the connections they hold to be returned —
// so call it once nothing is using the database, after a supervisor's shutdown
// sequence has finished rather than as part of it.
func (db *DB) Close() error {
	var errs []error
	if db.unregister != nil {
		if err := db.unregister(); err != nil {
			errs = append(errs, fmt.Errorf("unregister %s metrics: %w", db.name, err))
		}
		db.unregister = nil
	}
	if err := db.sql.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close %s: %w", db.name, err))
	}
	return errors.Join(errs...)
}
