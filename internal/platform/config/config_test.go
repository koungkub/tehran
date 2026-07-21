package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
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
		if cfg.Server.ShutdownTimeout != 10*time.Second {
			t.Errorf("shutdown_timeout = %v, want 10s", cfg.Server.ShutdownTimeout)
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
