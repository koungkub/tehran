package telemetry

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/koungkub/tehran/internal/platform/config"
)

// NewLogger builds the application logger: JSON in production, colored
// console output for local development. Redirects the stdlib log package.
func NewLogger(cfg config.Log) (*zap.Logger, error) {
	level, err := zapcore.ParseLevel(cfg.Level)
	if err != nil {
		return nil, fmt.Errorf("parse log level %q: %w", cfg.Level, err)
	}

	var zcfg zap.Config
	if cfg.Format == "console" {
		zcfg = zap.NewDevelopmentConfig()
		zcfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		zcfg = zap.NewProductionConfig()
		zcfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}
	zcfg.Level = zap.NewAtomicLevelAt(level)

	logger, err := zcfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	zap.RedirectStdLog(logger)
	return logger, nil
}
