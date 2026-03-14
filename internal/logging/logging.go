package logging

import (
	"strings"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/DevYukine/go-tradewinds/internal/config"
)

// Module provides a configured *zap.Logger to the fx DI container.
var Module = fx.Module("logging",
	fx.Provide(NewLogger),
)

// NewLogger creates a zap logger configured from the application config.
func NewLogger(cfg *config.Config) (*zap.Logger, error) {
	zapCfg := zap.NewDevelopmentConfig()
	zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	zapCfg.OutputPaths = []string{"stdout"}
	zapCfg.Level = zap.NewAtomicLevelAt(parseLogLevel(cfg.LogLevel))
	zapCfg.Development = cfg.Env == "development"

	return zapCfg.Build()
}

// parseLogLevel converts a string log level to a zapcore.Level.
func parseLogLevel(level string) zapcore.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zapcore.DebugLevel
	case "info":
		return zapcore.InfoLevel
	case "warn":
		return zapcore.WarnLevel
	case "error":
		return zapcore.ErrorLevel
	default:
		return zapcore.DebugLevel
	}
}
