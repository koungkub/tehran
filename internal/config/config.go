// Package config loads the application configuration with the precedence
// hardcoded defaults < TOML config file < environment variables < CLI flags.
//
// The sections are the library modules' own Config types rather than
// re-declarations of them: pkg/connectrpc, pkg/ops and pkg/telemetry each own
// their fields and TOML keys, and this package only decides how they nest, where
// they are read from, and what the defaults are. That part is specific to this
// service, which is why config cannot itself live in pkg.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/koungkub/tehran/pkg/connectrpc"
	"github.com/koungkub/tehran/pkg/lifecycle"
	"github.com/koungkub/tehran/pkg/ops"
	"github.com/koungkub/tehran/pkg/telemetry"
)

const envPrefix = "TEHRAN"

// Config is the whole of this service's configuration. Each section is the
// owning library module's own type.
type Config struct {
	Server    connectrpc.Config   `mapstructure:"server"`
	Ops       ops.Config          `mapstructure:"ops"`
	Lifecycle lifecycle.Config    `mapstructure:"lifecycle"`
	Otel      telemetry.Config    `mapstructure:"otel"`
	Log       telemetry.LogConfig `mapstructure:"log"`
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
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("server.max_request_bytes", connectrpc.DefaultMaxRequestBytes)
	v.SetDefault("server.read_header_timeout", connectrpc.DefaultReadHeaderTimeout)
	v.SetDefault("ops.host", "0.0.0.0")
	v.SetDefault("ops.port", 9090)
	v.SetDefault("ops.shutdown_timeout", ops.DefaultShutdownTimeout)
	v.SetDefault("ops.read_header_timeout", ops.DefaultReadHeaderTimeout)
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
