package main

import (
	"errors"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"
)

// Called after config.Load so logging also honors the local dotenv file.
func newLogger() (*zap.Logger, error) {
	var level zapcore.Level
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "", "info":
		level = zapcore.InfoLevel
	case "debug":
		level = zapcore.DebugLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		return nil, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	colorPolicy := os.Getenv("LOG_COLOR")
	switch colorPolicy {
	case "", "auto", "always", "never":
	default:
		return nil, errors.New("LOG_COLOR must be auto, always, or never")
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	color := !noColor && (colorPolicy == "always" ||
		(colorPolicy != "never" && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(os.Stderr.Fd()))))
	stderr := zapcore.Lock(zapcore.AddSync(os.Stderr))
	core := zapcore.NewCore(newConsoleEncoder(color), stderr, level)
	return zap.New(core, zap.ErrorOutput(stderr), zap.AddStacktrace(zapcore.ErrorLevel)), nil
}
