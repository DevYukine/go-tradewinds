package logging

import (
	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Module provides a configured *zap.Logger to the fx DI container.
var Module = fx.Module("logging",
	fx.Provide(NewLogger),
)

// NewLogger creates a development-configured zap logger with colored,
// human-readable output on stdout.
func NewLogger() (*zap.Logger, error) {
	cfg := zap.NewDevelopmentConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	cfg.OutputPaths = []string{"stdout"}
	cfg.ErrorOutputPaths = []string{"stdout"}

	return cfg.Build()
}
