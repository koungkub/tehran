// Package config loads the application configuration with the precedence
// hardcoded defaults < TOML config file < environment variables < CLI flags.
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const envPrefix = "TEHRAN"

type Config struct {
	Server Server `mapstructure:"server"`
	Ops    Ops    `mapstructure:"ops"`
	Otel   Otel   `mapstructure:"otel"`
	Log    Log    `mapstructure:"log"`
}

type Server struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
}

type Ops struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type Otel struct {
	Enabled     bool    `mapstructure:"enabled"`
	Endpoint    string  `mapstructure:"endpoint"`
	Insecure    bool    `mapstructure:"insecure"`
	SampleRatio float64 `mapstructure:"sample_ratio"`
	ServiceName string  `mapstructure:"service_name"`
}

type Log struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
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
	// env-only overrides via AutomaticEnv.
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("ops.host", "0.0.0.0")
	v.SetDefault("ops.port", 9090)
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
