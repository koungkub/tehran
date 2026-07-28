// Package config loads the application configuration with the precedence
// hardcoded defaults < TOML config file < environment variables < CLI flags.
//
// The sections are the library modules' own Config types rather than
// re-declarations of them: pkg/connectrpc, pkg/ops, pkg/database and
// pkg/telemetry each own their fields and TOML keys, and this package only
// decides how they nest, where they are read from, and what the defaults are.
// That part is specific to this service, which is why config cannot itself live
// in pkg. Where a section needs a setting that is the service's own decision
// rather than the module's, it wraps the module's Config instead of editing it —
// see Database.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/koungkub/tehran/pkg/connectrpc"
	"github.com/koungkub/tehran/pkg/database"
	"github.com/koungkub/tehran/pkg/lifecycle"
	"github.com/koungkub/tehran/pkg/migrate"
	"github.com/koungkub/tehran/pkg/ops"
	"github.com/koungkub/tehran/pkg/telemetry"
)

const envPrefix = "TEHRAN"

// Config is the whole of this service's configuration. Each section is the
// owning library module's own type.
type Config struct {
	Server    connectrpc.Config   `mapstructure:"server"`
	Ops       ops.Config          `mapstructure:"ops"`
	Database  Database            `mapstructure:"database"`
	Migrate   migrate.Config      `mapstructure:"migrate"`
	Lifecycle lifecycle.Config    `mapstructure:"lifecycle"`
	Otel      telemetry.Config    `mapstructure:"otel"`
	Log       telemetry.LogConfig `mapstructure:"log"`
}

// Database is the database section: the library module's own Config, plus the one
// decision that belongs to the service rather than to the connection.
type Database struct {
	// Enabled is read here and not by pkg/database, because whether a service
	// needs a database at all is not a property of the database. The api command
	// serves greeter, which has no repository, so it starts without one; the
	// first domain that persists anything turns this on.
	Enabled bool `mapstructure:"enabled"`
	// Squashed so the library's keys sit directly under [database] rather than
	// under a nested table of their own.
	database.Config `mapstructure:",squash"`
}

// flagBindings maps viper keys to CLI flag names. A bound flag overrides the
// other layers only when it was actually passed on the command line. Only host
// and port are bindable; all other settings come from the file or TEHRAN_* env.
var flagBindings = map[string]string{
	"server.host": "host",
	"server.port": "port",
}

// Load builds the configuration from the four layers. configFile is the
// --config value ("" means search ./config.toml then /etc/tehran/config.toml,
// and a missing file is not an error). flags may be nil.
func Load(configFile string, flags *pflag.FlagSet) (*Config, error) {
	v := viper.New()

	// Layer 1 — defaults. Every key must have one: viper.Unmarshal only walks
	// registered keys, so a key without a default would be invisible to
	// env-only overrides via AutomaticEnv. That holds for the keys the pkg
	// modules declare too: those modules substitute their own fallback for a
	// zero value, but only a registered key can be overridden at all.
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.shutdown_timeout", connectrpc.DefaultShutdownTimeout)
	v.SetDefault("server.max_request_bytes", connectrpc.DefaultMaxRequestBytes)
	v.SetDefault("server.read_header_timeout", connectrpc.DefaultReadHeaderTimeout)
	// Off by default, and deliberately so: unsafe for client-streaming RPCs. It
	// still needs registering, or it could not be switched on from a file or the
	// environment at all.
	v.SetDefault("server.read_timeout", time.Duration(0))
	v.SetDefault("server.idle_timeout", connectrpc.DefaultIdleTimeout)
	v.SetDefault("server.keepalive_interval", connectrpc.DefaultKeepaliveInterval)
	v.SetDefault("server.write_byte_timeout", connectrpc.DefaultWriteByteTimeout)
	v.SetDefault("server.request_timeout", connectrpc.DefaultRequestTimeout)
	v.SetDefault("server.max_concurrent_streams", connectrpc.DefaultMaxConcurrentStreams)
	v.SetDefault("ops.host", "0.0.0.0")
	v.SetDefault("ops.port", 9090)
	v.SetDefault("ops.shutdown_timeout", ops.DefaultShutdownTimeout)
	v.SetDefault("ops.read_header_timeout", ops.DefaultReadHeaderTimeout)
	// The database section is off until a domain here persists something, but
	// every key still needs registering: an unregistered key cannot be set from
	// the environment at all, and the credentials are exactly what arrives that
	// way rather than through the file.
	v.SetDefault("database.enabled", false)
	v.SetDefault("database.driver", database.DefaultDriver)
	v.SetDefault("database.dsn", "")
	v.SetDefault("database.host", "127.0.0.1")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.user", "")
	v.SetDefault("database.password", "")
	v.SetDefault("database.database", "")
	v.SetDefault("database.ssl_mode", "")
	v.SetDefault("database.max_open_conns", database.DefaultMaxOpenConns)
	// Zero means "follow max_open_conns", which is what the library substitutes.
	v.SetDefault("database.max_idle_conns", 0)
	v.SetDefault("database.conn_max_lifetime", database.DefaultConnMaxLifetime)
	v.SetDefault("database.conn_max_idle_time", database.DefaultConnMaxIdleTime)
	v.SetDefault("database.connect_timeout", database.DefaultConnectTimeout)
	v.SetDefault("database.slow_query_threshold", database.DefaultSlowQueryThreshold)
	v.SetDefault("database.prepare_stmt", false)
	v.SetDefault("database.include_query_values", false)
	// The migrate section drives the `db` command and is read by nothing the api
	// server does. dialect is registered empty rather than at the library's own
	// default, because internal/app/db falls back to database.driver: a default
	// here would let the two disagree, and a version table built for the wrong
	// dialect is discovered on the first migration rather than in review.
	v.SetDefault("migrate.dialect", "")
	v.SetDefault("migrate.table_name", migrate.DefaultTableName)
	v.SetDefault("migrate.lock_mode", migrate.DefaultLockMode)
	v.SetDefault("migrate.lock_id", migrate.DefaultLockID)
	v.SetDefault("migrate.lock_wait", migrate.DefaultLockWait)
	v.SetDefault("migrate.allow_out_of_order", false)
	// Off, like the library's own default: a bound low enough to catch a hung
	// migration also cuts a legitimate index build short. Registered so it can be
	// set for a service that knows its migrations are all small.
	v.SetDefault("migrate.timeout", time.Duration(0))
	v.SetDefault("lifecycle.shutdown_timeout", lifecycle.DefaultShutdownTimeout)
	v.SetDefault("otel.enabled", true)
	v.SetDefault("otel.endpoint", "localhost:4317")
	v.SetDefault("otel.insecure", true)
	v.SetDefault("otel.sample_ratio", 1.0)
	v.SetDefault("otel.service_name", "tehran")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "json")

	// Layer 2 — TOML config file.
	if configFile != "" {
		v.SetConfigFile(configFile)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("toml")
		v.AddConfigPath(".")
		v.AddConfigPath("/etc/tehran")
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if configFile != "" || !errors.As(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	// Layer 3 — environment variables: server.port -> TEHRAN_SERVER_PORT.
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Layer 4 — CLI flags.
	if flags != nil {
		for key, name := range flagBindings {
			if f := flags.Lookup(name); f != nil {
				if err := v.BindPFlag(key, f); err != nil {
					return nil, fmt.Errorf("bind flag %s: %w", name, err)
				}
			}
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	return &cfg, nil
}
