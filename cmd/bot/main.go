package main

import (
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/cache"
	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"github.com/DevYukine/go-tradewinds/internal/logging"
	"github.com/DevYukine/go-tradewinds/internal/optimizer"
	"github.com/DevYukine/go-tradewinds/internal/server"
	"github.com/DevYukine/go-tradewinds/internal/strategy"
)

func main() {
	fx.New(
		fx.StartTimeout(90*time.Second),
		fx.WithLogger(func(log *zap.Logger) fxevent.Logger {
			zapLogger := fxevent.ZapLogger{Logger: log}
			zapLogger.UseLogLevel(zap.DebugLevel)
			return &zapLogger
		}),
		config.Module,    // Provides *Config
		logging.Module,   // Provides *zap.Logger
		db.Module,        // Provides *gorm.DB
		cache.Module,     // Provides *RedisCache for state persistence
		agent.Module,     // Provides agent.Agent
		strategy.Module,  // Provides bot.Registry
		bot.Module,       // Provides *Manager, starts company runners
		optimizer.Module, // Provides *optimizer.Engine, runs periodic evaluation
		server.Module,    // Provides API server for dashboard
	).Run()
}
