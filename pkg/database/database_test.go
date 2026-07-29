package database

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/rs/zerolog"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// quiet keeps a test's own output to its assertions. Passing it explicitly also
// stops these tests from depending on zerolog's package-level logger.
func quiet() *zerolog.Logger {
	l := zerolog.New(io.Discard)
	return &l
}

// withMockDialector is WithDialector's reason for existing, used as intended: a
// driver this package does not import, here a mock standing in for a server.
func withMockDialector(conn *sql.DB) Option {
	return WithDialector(postgres.New(postgres.Config{Conn: conn}))
}

// openMock opens a DB over a mocked driver, so everything Open does around the
// connection — pool settings, instrumentation, the startup ping — is exercised
// without a database to talk to. The caller sets its expectations on the returned
// mock before triggering the work that should meet them.
func openMock(t *testing.T, cfg Config, opts ...Option) (*DB, sqlmock.Sqlmock) {
	t.Helper()
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing()

	opts = append([]Option{WithLogger(quiet()), withMockDialector(conn)}, opts...)
	db, err := Open(context.Background(), cfg, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		// Declared here rather than up front: sqlmock matches in order, so an
		// expectation set before the test's own would have to be met first.
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db, mock
}

func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	for _, tc := range []struct {
		name      string
		got, want any
	}{
		{"Driver", got.Driver, DefaultDriver},
		{"MaxOpenConns", got.MaxOpenConns, DefaultMaxOpenConns},
		{"ConnMaxLifetime", got.ConnMaxLifetime, DefaultConnMaxLifetime},
		{"ConnMaxIdleTime", got.ConnMaxIdleTime, DefaultConnMaxIdleTime},
		{"ConnectTimeout", got.ConnectTimeout, DefaultConnectTimeout},
		{"SlowQueryThreshold", got.SlowQueryThreshold, DefaultSlowQueryThreshold},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}

	// The idle limit follows the open limit rather than a constant of its own:
	// database/sql closes any connection returned to a full idle set, so a lower
	// idle limit makes a busy service churn connections continuously.
	if got.MaxIdleConns != got.MaxOpenConns {
		t.Errorf("MaxIdleConns = %d, want it to default to MaxOpenConns (%d)",
			got.MaxIdleConns, got.MaxOpenConns)
	}
	if got := (Config{MaxOpenConns: 4}).withDefaults(); got.MaxIdleConns != 4 {
		t.Errorf("MaxIdleConns = %d, want it to follow an explicit MaxOpenConns (4)", got.MaxIdleConns)
	}
	// database/sql caps the idle set at MaxOpenConns whatever it is told, so a
	// larger value is not a pool holding more idle connections — it is only a
	// number that disagrees with the pool, and db.client.connection.idle.max
	// reports it because database/sql exposes no way to read the real one back.
	if got := (Config{MaxOpenConns: 4, MaxIdleConns: 40}).withDefaults(); got.MaxIdleConns != 4 {
		t.Errorf("MaxIdleConns = %d, want it clamped to MaxOpenConns (4)", got.MaxIdleConns)
	}
}

// TestPostgresDSNEscapesCredentials is about the failure that makes
// hand-assembled DSNs a bad idea: a password is an arbitrary string, and a
// generated one routinely contains the characters that delimit the DSN itself.
func TestPostgresDSNEscapesCredentials(t *testing.T) {
	const password = "p@ss word/:?#1"
	cfg := Config{
		Driver: DriverPostgres, Host: "db.internal", Port: 5432,
		User: "svc", Password: password, Database: "billing", SSLMode: "verify-full",
	}.withDefaults()

	dsn, err := cfg.dsn()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse generated dsn: %v", err)
	}
	if got, _ := u.User.Password(); got != password {
		t.Errorf("password round-tripped as %q, want %q", got, password)
	}
	if got := u.User.Username(); got != "svc" {
		t.Errorf("user = %q, want svc", got)
	}
	if got := u.Host; got != "db.internal:5432" {
		t.Errorf("host = %q, want db.internal:5432", got)
	}
	if got := strings.TrimPrefix(u.Path, "/"); got != "billing" {
		t.Errorf("database = %q, want billing", got)
	}
	if got := u.Query().Get("sslmode"); got != "verify-full" {
		t.Errorf("sslmode = %q, want verify-full", got)
	}
	// The driver's own connect timeout, not just the one Open applies to its
	// ping: the pool opens connections lazily long after startup.
	if got := u.Query().Get("connect_timeout"); got != "5" {
		t.Errorf("connect_timeout = %q, want 5", got)
	}
}

// TestPostgresConnectTimeoutRoundsUp guards a value that means the opposite of
// what it looks like: connect_timeout is whole seconds, and 0 means "wait
// forever", so truncating a sub-second setting would disable the timeout.
func TestPostgresConnectTimeoutRoundsUp(t *testing.T) {
	cfg := Config{
		Driver: DriverPostgres, Database: "billing",
		ConnectTimeout: 100 * time.Millisecond,
	}.withDefaults()

	dsn, err := cfg.dsn()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse generated dsn: %v", err)
	}
	if got := u.Query().Get("connect_timeout"); got != "1" {
		t.Errorf("connect_timeout = %q, want 1: a sub-second value must not round to 0, which means no timeout", got)
	}
}

// TestDSNLeavesAnUnsetPortToTheDriver covers a value that reads as harmless and
// is not: joining a zero port produces "host:0", which dials port 0 rather than
// 5432, and the resulting "connection refused" says nothing about why.
func TestDSNLeavesAnUnsetPortToTheDriver(t *testing.T) {
	t.Run("postgres", func(t *testing.T) {
		cfg := Config{Driver: DriverPostgres, Host: "db.internal", Database: "billing"}.withDefaults()
		dsn, err := cfg.dsn()
		if err != nil {
			t.Fatalf("dsn: %v", err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse generated dsn: %v", err)
		}
		if u.Host != "db.internal" {
			t.Errorf("host = %q, want no port at all so the driver uses 5432", u.Host)
		}
	})

	t.Run("mysql", func(t *testing.T) {
		cfg := Config{Driver: DriverMySQL, Host: "db.internal", Database: "billing"}.withDefaults()
		dsn, err := cfg.dsn()
		if err != nil {
			t.Fatalf("dsn: %v", err)
		}
		got, err := mysqldriver.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("parse generated dsn: %v", err)
		}
		// go-sql-driver normalises a bare host to its own default port.
		if got.Addr != "db.internal:3306" {
			t.Errorf("addr = %q, want the driver's default port", got.Addr)
		}
	})

	t.Run("target", func(t *testing.T) {
		got := Config{Host: "db.internal", Database: "billing"}.Target()
		if got != "postgres://db.internal/billing" {
			t.Errorf("Target() = %q, want no :0 in it", got)
		}
	})
}

func TestMySQLDSN(t *testing.T) {
	const password = "p@ss/word:1"
	cfg := Config{
		Driver: DriverMySQL, Host: "db.internal", Port: 3306,
		User: "svc", Password: password, Database: "billing",
	}.withDefaults()

	dsn, err := cfg.dsn()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	got, err := mysqldriver.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parse generated dsn: %v", err)
	}
	if got.Passwd != password {
		t.Errorf("password round-tripped as %q, want %q", got.Passwd, password)
	}
	if got.Addr != "db.internal:3306" || got.DBName != "billing" {
		t.Errorf("addr/db = %q/%q, want db.internal:3306/billing", got.Addr, got.DBName)
	}
	// Without ParseTime the driver returns []byte for DATETIME and every
	// time.Time field in a model fails to scan.
	if !got.ParseTime {
		t.Error("ParseTime = false, want true: time.Time fields cannot scan without it")
	}
	if got.Loc != time.UTC {
		t.Errorf("Loc = %v, want UTC", got.Loc)
	}
	if got.Timeout != DefaultConnectTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultConnectTimeout)
	}
}

func TestDSNOverridesFields(t *testing.T) {
	cfg := Config{DSN: "postgres://elsewhere/other", Host: "ignored", Database: "ignored"}.withDefaults()

	got, err := cfg.dsn()
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	if got != "postgres://elsewhere/other" {
		t.Errorf("dsn = %q, want the configured DSN verbatim", got)
	}
}

func TestDSNErrors(t *testing.T) {
	t.Run("nothing to build from", func(t *testing.T) {
		_, err := Config{}.withDefaults().dsn()
		if !errors.Is(err, ErrMissingDatabase) {
			t.Errorf("err = %v, want ErrMissingDatabase", err)
		}
	})

	t.Run("unsupported driver", func(t *testing.T) {
		_, err := Config{Driver: "cassandra", Database: "billing"}.withDefaults().dsn()
		if err == nil {
			t.Fatal("dsn = nil error, want one naming the driver")
		}
		// The message has to point at the way out, or a service using a driver
		// this package does not import has no idea one exists.
		if !strings.Contains(err.Error(), "WithDialector") {
			t.Errorf("err = %q, want it to mention WithDialector", err)
		}
	})
}

// TestTargetNeverCarriesCredentials covers the string every log line and error in
// this package uses to name the server. It is the reason none of them can leak a
// password.
func TestTargetNeverCarriesCredentials(t *testing.T) {
	// Assembled rather than written out: a literal URL with credentials in it is
	// a hardcoded-secret finding, however fake the secret is.
	const secret = "hunter2"
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "from fields",
			cfg:  Config{Host: "db.internal", Port: 5432, User: "svc", Password: secret, Database: "billing"},
			want: "postgres://db.internal:5432/billing",
		},
		{
			name: "from a dsn",
			cfg:  Config{DSN: "postgres://svc:" + secret + "@db.internal:5432/billing?sslmode=require"},
			want: "postgres://db.internal:5432/billing",
		},
		{
			// A key/value DSN does not parse as a URL, so there is nothing safe
			// to pick out of it: report the driver and nothing else.
			name: "from a dsn that is not a url",
			cfg:  Config{DSN: "host=db.internal user=svc password=" + secret},
			want: "postgres",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.cfg.Target()
			if got != tc.want {
				t.Errorf("Target() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, secret) {
				t.Errorf("Target() = %q, want no password in it", got)
			}
		})
	}
}

func TestOpenAppliesPoolSettings(t *testing.T) {
	db, _ := openMock(t, Config{MaxOpenConns: 7})

	if got := db.Stats().MaxOpenConnections; got != 7 {
		t.Errorf("MaxOpenConnections = %d, want 7", got)
	}
	if got := db.Name(); got != DefaultName {
		t.Errorf("Name() = %q, want %q", got, DefaultName)
	}
	if db.Gorm() == nil || db.SQL() == nil {
		t.Error("Gorm() or SQL() is nil after a successful Open")
	}
}

// TestOpenClosesThePoolOnAFailedPing is the leak this ordering exists to prevent:
// gorm.Open has already opened a pool by the time the ping runs, so returning an
// error without closing it leaves the connections behind with no handle to reach
// them by.
func TestOpenClosesThePoolOnAFailedPing(t *testing.T) {
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	boom := errors.New("connection refused")
	mock.ExpectPing().WillReturnError(boom)
	mock.ExpectClose()

	db, err := Open(context.Background(), Config{},
		WithLogger(quiet()),
		WithDialector(postgres.New(postgres.Config{Conn: conn})),
	)
	if err == nil {
		t.Fatalf("Open = nil error, want one; db = %v", db)
	}
	if !errors.Is(err, boom) {
		t.Errorf("Open err = %v, want it to wrap the driver's error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the pool was not closed after the failed ping: %v", err)
	}
}

// TestOpenDoesNotHangOnAnUnreachableDatabase is why Open disables GORM's own
// ping: that one takes no context, so a database that accepts the connection and
// then stops responding would block startup for as long as the driver allows.
func TestOpenDoesNotHangOnAnUnreachableDatabase(t *testing.T) {
	conn, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatalf("new sqlmock: %v", err)
	}
	mock.ExpectPing().WillDelayFor(10 * time.Second)
	mock.ExpectClose()

	start := time.Now()
	if _, err := Open(context.Background(), Config{ConnectTimeout: 50 * time.Millisecond},
		WithLogger(quiet()),
		WithDialector(postgres.New(postgres.Config{Conn: conn})),
	); err == nil {
		t.Fatal("Open = nil error, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Open took %v, want it bounded by ConnectTimeout (50ms)", elapsed)
	}
}

func TestPingReportsTheTargetNotTheDSN(t *testing.T) {
	db, mock := openMock(t, Config{Host: "db.internal", Port: 5432, User: "svc", Password: "hunter2", Database: "billing"})
	mock.ExpectPing().WillReturnError(errors.New("server closed the connection"))

	err := db.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping = nil error, want one")
	}
	if !strings.Contains(err.Error(), "db.internal:5432/billing") {
		t.Errorf("Ping err = %q, want it to name the target", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("Ping err = %q, want no password in it", err)
	}
}

// TestPingBoundsADeadlinelessContext is about the readiness probe this is meant
// to be handed to. An HTTP request's context carries no deadline of its own, so
// without a bound here a probe against an exhausted pool or an unresponsive
// server would block in the handler for as long as the prober waited — and an
// orchestrator that gets no reply cannot tell "not ready" from "not answering".
func TestPingBoundsADeadlinelessContext(t *testing.T) {
	db, mock := openMock(t, Config{ConnectTimeout: 50 * time.Millisecond})
	mock.ExpectPing().WillDelayFor(10 * time.Second)

	start := time.Now()
	err := db.Ping(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Ping = nil error, want one once its own bound expires")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Ping took %v with no deadline in its context, want it bounded by ConnectTimeout (50ms)", elapsed)
	}
}

// A deadline the caller set always wins, in either direction — the same stance
// the RPC server takes on a timeout a client sent.
func TestPingKeepsTheCallersDeadline(t *testing.T) {
	db, mock := openMock(t, Config{ConnectTimeout: time.Hour})
	mock.ExpectPing().WillDelayFor(10 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := db.Ping(ctx); err == nil {
		t.Fatal("Ping = nil error, want the caller's deadline to end it")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Ping took %v, want the caller's 50ms deadline to win over ConnectTimeout", elapsed)
	}
}

// The GORM handle is what repositories are given, and a Config with no
// PrepareStmt must not quietly enable it: prepared-statement caching breaks
// against a pooler in transaction mode.
func TestPrepareStmtIsOffByDefault(t *testing.T) {
	db, _ := openMock(t, Config{})

	if db.Gorm().PrepareStmt {
		t.Error("PrepareStmt = true with a zero Config, want false: it breaks against PgBouncer in transaction mode")
	}
}

func TestPluginsAreRegistered(t *testing.T) {
	p := &countingPlugin{}
	db, _ := openMock(t, Config{}, WithPlugins(p))

	if p.used != 1 {
		t.Errorf("plugin initialised %d times, want 1", p.used)
	}
	if _, ok := db.Gorm().Plugins[p.Name()]; !ok {
		t.Errorf("plugin %q is not registered on the handle", p.Name())
	}
}

type countingPlugin struct{ used int }

func (p *countingPlugin) Name() string { return "counting" }

func (p *countingPlugin) Initialize(*gorm.DB) error {
	p.used++
	return nil
}
