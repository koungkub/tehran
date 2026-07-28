package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/koungkub/tehran/pkg/connectrpc"
	"github.com/koungkub/tehran/pkg/database"
	"github.com/koungkub/tehran/pkg/migrate"
)

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func apiFlags() *pflag.FlagSet {
	fs := pflag.NewFlagSet("api", pflag.ContinueOnError)
	fs.String("host", "0.0.0.0", "")
	fs.Int("port", 8080, "")
	return fs
}

func TestLoadPrecedence(t *testing.T) {
	t.Run("defaults only", func(t *testing.T) {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 8080 {
			t.Errorf("port = %d, want default 8080", cfg.Server.Port)
		}
		// The library constant, not a literal: a literal here would keep passing
		// while config pinned a stale value the library had moved on from.
		if cfg.Server.ShutdownTimeout != connectrpc.DefaultShutdownTimeout {
			t.Errorf("shutdown_timeout = %v, want %v",
				cfg.Server.ShutdownTimeout, connectrpc.DefaultShutdownTimeout)
		}
	})

	t.Run("file overrides default", func(t *testing.T) {
		path := writeTOML(t, "[server]\nport = 9000\nshutdown_timeout = \"3s\"\n")
		cfg, err := Load(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9000 {
			t.Errorf("port = %d, want 9000 from file", cfg.Server.Port)
		}
		if cfg.Server.ShutdownTimeout != 3*time.Second {
			t.Errorf("shutdown_timeout = %v, want 3s from file", cfg.Server.ShutdownTimeout)
		}
		if cfg.Ops.Port != 9090 {
			t.Errorf("ops.port = %d, want default 9090", cfg.Ops.Port)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		path := writeTOML(t, "[server]\nport = 9000\n")
		t.Setenv("TEHRAN_SERVER_PORT", "9001")
		cfg, err := Load(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9001 {
			t.Errorf("port = %d, want 9001 from env", cfg.Server.Port)
		}
	})

	t.Run("env overrides default without file", func(t *testing.T) {
		t.Setenv("TEHRAN_LOG_LEVEL", "debug")
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Log.Level != "debug" {
			t.Errorf("log.level = %q, want debug from env", cfg.Log.Level)
		}
	})

	t.Run("changed flag overrides env and file", func(t *testing.T) {
		path := writeTOML(t, "[server]\nport = 9000\n")
		t.Setenv("TEHRAN_SERVER_PORT", "9001")
		fs := apiFlags()
		if err := fs.Set("port", "9002"); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path, fs)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9002 {
			t.Errorf("port = %d, want 9002 from flag", cfg.Server.Port)
		}
	})

	t.Run("unchanged flag does not shadow env", func(t *testing.T) {
		t.Setenv("TEHRAN_SERVER_PORT", "9001")
		cfg, err := Load("", apiFlags())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.Port != 9001 {
			t.Errorf("port = %d, want 9001 from env (flag not passed)", cfg.Server.Port)
		}
	})

	t.Run("explicit missing config file is an error", func(t *testing.T) {
		if _, err := Load(filepath.Join(t.TempDir(), "nope.toml"), nil); err == nil {
			t.Error("want error for missing explicit config file")
		}
	})
}

// TestServerTimeoutsLoad covers every timeout key on the RPC server. Each one
// needs a SetDefault to exist as far as viper is concerned, so a key added to
// connectrpc.Config but not registered here would silently stay at its zero
// value from a file and be invisible to TEHRAN_* env overrides entirely.
func TestServerTimeoutsLoad(t *testing.T) {
	t.Run("defaults match the library", func(t *testing.T) {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"shutdown_timeout", cfg.Server.ShutdownTimeout, connectrpc.DefaultShutdownTimeout},
			{"read_header_timeout", cfg.Server.ReadHeaderTimeout, connectrpc.DefaultReadHeaderTimeout},
			{"read_timeout", cfg.Server.ReadTimeout, time.Duration(0)},
			{"idle_timeout", cfg.Server.IdleTimeout, connectrpc.DefaultIdleTimeout},
			{"keepalive_interval", cfg.Server.KeepaliveInterval, connectrpc.DefaultKeepaliveInterval},
			{"write_byte_timeout", cfg.Server.WriteByteTimeout, connectrpc.DefaultWriteByteTimeout},
			{"request_timeout", cfg.Server.RequestTimeout, connectrpc.DefaultRequestTimeout},
			{"max_concurrent_streams", cfg.Server.MaxConcurrentStreams, connectrpc.DefaultMaxConcurrentStreams},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("file overrides each one", func(t *testing.T) {
		path := writeTOML(t, `
[server]
read_header_timeout = "1s"
idle_timeout = "2s"
keepalive_interval = "3s"
write_byte_timeout = "4s"
request_timeout = "5s"
max_concurrent_streams = 6
read_timeout = "7s"
shutdown_timeout = "8s"
`)
		cfg, err := Load(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"read_header_timeout", cfg.Server.ReadHeaderTimeout, time.Second},
			{"idle_timeout", cfg.Server.IdleTimeout, 2 * time.Second},
			{"keepalive_interval", cfg.Server.KeepaliveInterval, 3 * time.Second},
			{"write_byte_timeout", cfg.Server.WriteByteTimeout, 4 * time.Second},
			{"request_timeout", cfg.Server.RequestTimeout, 5 * time.Second},
			{"max_concurrent_streams", cfg.Server.MaxConcurrentStreams, 6},
			{"read_timeout", cfg.Server.ReadTimeout, 7 * time.Second},
			{"shutdown_timeout", cfg.Server.ShutdownTimeout, 8 * time.Second},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v from file", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("env overrides a timeout", func(t *testing.T) {
		t.Setenv("TEHRAN_SERVER_REQUEST_TIMEOUT", "12s")
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.RequestTimeout != 12*time.Second {
			t.Errorf("request_timeout = %v, want 12s from env", cfg.Server.RequestTimeout)
		}
	})
}

// TestDatabaseSectionLoads covers the squashed section: the library's keys have
// to land directly under [database] alongside the service's own enabled flag, or
// every one of them silently stays at its zero value.
func TestDatabaseSectionLoads(t *testing.T) {
	t.Run("defaults match the library", func(t *testing.T) {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Database.Enabled {
			t.Error("database.enabled = true by default, want false: the api command must start without a database")
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"driver", cfg.Database.Driver, database.DefaultDriver},
			{"max_open_conns", cfg.Database.MaxOpenConns, database.DefaultMaxOpenConns},
			{"conn_max_lifetime", cfg.Database.ConnMaxLifetime, database.DefaultConnMaxLifetime},
			{"conn_max_idle_time", cfg.Database.ConnMaxIdleTime, database.DefaultConnMaxIdleTime},
			{"connect_timeout", cfg.Database.ConnectTimeout, database.DefaultConnectTimeout},
			{"slow_query_threshold", cfg.Database.SlowQueryThreshold, database.DefaultSlowQueryThreshold},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("file fills the section", func(t *testing.T) {
		path := writeTOML(t, `
[database]
enabled = true
driver = "mysql"
host = "db.internal"
port = 3306
user = "svc"
database = "billing"
ssl_mode = "verify-full"
max_open_conns = 40
max_idle_conns = 10
conn_max_lifetime = "10m"
connect_timeout = "2s"
slow_query_threshold = "50ms"
prepare_stmt = true
`)
		cfg, err := Load(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"enabled", cfg.Database.Enabled, true},
			{"driver", cfg.Database.Driver, database.DriverMySQL},
			{"host", cfg.Database.Host, "db.internal"},
			{"port", cfg.Database.Port, 3306},
			{"user", cfg.Database.User, "svc"},
			{"database", cfg.Database.Database, "billing"},
			{"ssl_mode", cfg.Database.SSLMode, "verify-full"},
			{"max_open_conns", cfg.Database.MaxOpenConns, 40},
			{"max_idle_conns", cfg.Database.MaxIdleConns, 10},
			{"conn_max_lifetime", cfg.Database.ConnMaxLifetime, 10 * time.Minute},
			{"connect_timeout", cfg.Database.ConnectTimeout, 2 * time.Second},
			{"slow_query_threshold", cfg.Database.SlowQueryThreshold, 50 * time.Millisecond},
			{"prepare_stmt", cfg.Database.PrepareStmt, true},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v from file", tc.name, tc.got, tc.want)
			}
		}
	})

	// The password is the one setting that must be settable this way and only
	// this way, which is why its key is registered with an empty default and left
	// out of config.toml.
	t.Run("env supplies the password", func(t *testing.T) {
		t.Setenv("TEHRAN_DATABASE_PASSWORD", "from-the-environment")
		cfg, err := Load(repoConfig, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Database.Password != "from-the-environment" {
			t.Errorf("password = %q, want it from TEHRAN_DATABASE_PASSWORD", cfg.Database.Password)
		}
	})
}

// TestDatabaseConnectTimeoutFitsARequest guards another relationship no single
// package can see. The pool opens connections lazily, so a request that arrives
// when none is free waits for a new one to be dialled — out of its own deadline.
// A connect timeout at or above the request timeout means such a request cannot
// succeed: it times out while the connection it needs is still being made.
func TestDatabaseConnectTimeoutFitsARequest(t *testing.T) {
	cfg, err := Load(repoConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	if connect, request := cfg.Database.ConnectTimeout, cfg.Server.RequestTimeout; connect >= request {
		t.Errorf("database.connect_timeout (%v) >= server.request_timeout (%v): "+
			"a request that has to open a connection exhausts its deadline doing so",
			connect, request)
	}
}

// repoConfig is the config.toml the binary actually reads, relative to this
// package's directory — which is what `go test` makes the working directory.
//
// The path has to be explicit. Load("") searches "." and /etc/tehran, and "."
// during a test is internal/config, so an empty path silently falls back to the
// hardcoded defaults and asserts nothing about the shipped file. Those defaults
// are already covered by connectrpc's own TestConfigDefaults.
const repoConfig = "../../config.toml"

// TestShippedConfigIsFound fails loudly if repoConfig stops resolving, so that
// moving or renaming the file cannot quietly turn the coherence test below back
// into an assertion about defaults.
func TestShippedConfigIsFound(t *testing.T) {
	cfg, err := Load(repoConfig, nil)
	if err != nil {
		t.Fatalf("load %s: %v", repoConfig, err)
	}
	// A value the hardcoded defaults do not produce: defaults say "json".
	if cfg.Log.Format != "console" {
		t.Errorf("log.format = %q, want %q from %s: the file was not read",
			cfg.Log.Format, "console", repoConfig)
	}
}

// TestShutdownTimeoutsAreCoherent guards a relationship between three
// independently-owned settings that nothing else can enforce.
//
// http.Server.Shutdown only closes a connection once it goes idle, and it never
// cancels a handler's context. So if a request may run longer than the drain it
// is given, an ordinary SIGTERM cuts that request off mid-flight, Serve returns
// an error, and the process exits non-zero on every deploy that happens to catch
// a slow request. Each timeout is defensible alone; only together are they
// wrong, which is exactly what no single package's own tests would notice.
//
// It reads the shipped config.toml rather than the defaults, because the values
// that can actually break a deploy are the ones in that file.
func TestShutdownTimeoutsAreCoherent(t *testing.T) {
	cfg, err := Load(repoConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, ops, sup := cfg.Server.ShutdownTimeout, cfg.Ops.ShutdownTimeout, cfg.Lifecycle.ShutdownTimeout

	// Body read and handler run in sequence, so the worst case for one request is
	// the sum. read_timeout is 0 when off, which reduces this to request_timeout.
	if worst := cfg.Server.ReadTimeout + cfg.Server.RequestTimeout; worst > srv {
		t.Errorf("server.read_timeout + server.request_timeout (%v) > server.shutdown_timeout (%v): "+
			"a request still running when the drain expires is cut off and the process exits non-zero",
			worst, srv)
	}
	// Components drain one at a time in reverse registration order, so the
	// supervisor's backstop has to cover their sum, not their maximum.
	if srv+ops > sup {
		t.Errorf("server (%v) + ops (%v) drains exceed lifecycle.shutdown_timeout (%v): "+
			"the backstop fires while a component is still draining normally", srv, ops, sup)
	}
	// net/http arms the header phase with read_header_timeout and only swaps to
	// the read_timeout deadline once the headers are read (server.go: hdrDeadline
	// then wholeReqDeadline). A header timeout longer than the whole-request one
	// therefore lets the header phase outlive the budget that is supposed to bound
	// it, and leaves the body phase starting on an already-expired deadline. Only
	// checked when read_timeout is on: off, there is no whole-request budget to
	// overrun. HTTP/1.1 only, since h2 ignores read_header_timeout entirely.
	if hdr, read := cfg.Server.ReadHeaderTimeout, cfg.Server.ReadTimeout; read > 0 && hdr > read {
		t.Errorf("server.read_header_timeout (%v) > server.read_timeout (%v): "+
			"on HTTP/1.1 the header phase outlives the whole-request deadline", hdr, read)
	}
}

// TestMigrateSectionLoads covers the section the `db` command reads. Every key
// needs a SetDefault to exist as far as viper is concerned, so one added to
// migrate.Config but not registered there would stay at its zero value even when
// a file sets it.
func TestMigrateSectionLoads(t *testing.T) {
	t.Run("defaults match the library", func(t *testing.T) {
		cfg, err := Load("", nil)
		if err != nil {
			t.Fatal(err)
		}
		// dialect is the one key deliberately left empty rather than defaulted:
		// internal/app/db falls back to database.driver, and a value here would
		// let a version table be built for a dialect the pool does not speak.
		if cfg.Migrate.Dialect != "" {
			t.Errorf("migrate.dialect = %q by default, want empty so it follows database.driver",
				cfg.Migrate.Dialect)
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"table_name", cfg.Migrate.TableName, migrate.DefaultTableName},
			{"lock_mode", cfg.Migrate.LockMode, migrate.DefaultLockMode},
			{"lock_id", cfg.Migrate.LockID, migrate.DefaultLockID},
			{"lock_wait", cfg.Migrate.LockWait, migrate.DefaultLockWait},
			{"allow_out_of_order", cfg.Migrate.AllowOutOfOrder, false},
			{"timeout", cfg.Migrate.Timeout, time.Duration(0)},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("file fills the section", func(t *testing.T) {
		path := writeTOML(t, `
[migrate]
dialect = "mysql"
table_name = "schema_version"
lock_mode = "none"
lock_id = 12345
lock_wait = "90s"
allow_out_of_order = true
timeout = "10m"
`)
		cfg, err := Load(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name      string
			got, want any
		}{
			{"dialect", cfg.Migrate.Dialect, migrate.DialectMySQL},
			{"table_name", cfg.Migrate.TableName, "schema_version"},
			{"lock_mode", cfg.Migrate.LockMode, migrate.LockModeNone},
			{"lock_id", cfg.Migrate.LockID, int64(12345)},
			{"lock_wait", cfg.Migrate.LockWait, 90 * time.Second},
			{"allow_out_of_order", cfg.Migrate.AllowOutOfOrder, true},
			{"timeout", cfg.Migrate.Timeout, 10 * time.Minute},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v from file", tc.name, tc.got, tc.want)
			}
		}
	})
}

// TestMigrateLockModeSuitsTheDriver guards a relationship that spans two sections
// of the shipped file and is checked nowhere else until a migration actually runs
// — which is during a deploy.
//
// goose implements a locker for PostgreSQL only, so pkg/migrate refuses any
// lock_mode above "none" on another dialect rather than starting up silently
// unlocked. Switching database.driver to mysql and leaving lock_mode at its
// default is therefore a working configuration for the api server and a broken
// one for `tehran db migrate`.
func TestMigrateLockModeSuitsTheDriver(t *testing.T) {
	cfg, err := Load(repoConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The same fallback internal/app/db applies.
	dialect := cfg.Migrate.Dialect
	if dialect == "" {
		dialect = cfg.Database.Driver
	}
	if cfg.Migrate.LockMode != migrate.LockModeNone && dialect != migrate.DialectPostgres {
		t.Errorf("migrate.lock_mode = %q with dialect %q: goose implements no locker for it, "+
			"so `db migrate` would refuse to start — set migrate.lock_mode = %q",
			cfg.Migrate.LockMode, dialect, migrate.LockModeNone)
	}
}
