package db

import (
	"fmt"
	"strings"

	"go.uber.org/zap"
)

// gooseLogger adapts a *zap.Logger to the goose.Logger interface.
// Trailing newlines are trimmed because goose format strings often
// include them, which would show up as literal \n in zap output.
type gooseLogger struct {
	logger *zap.Logger
}

func (g *gooseLogger) Printf(format string, v ...interface{}) {
	g.logger.Info(fmt.Sprintf(strings.TrimRight(format, "\n"), v...))
}

func (g *gooseLogger) Fatalf(format string, v ...interface{}) {
	g.logger.Fatal(fmt.Sprintf(strings.TrimRight(format, "\n"), v...))
}
