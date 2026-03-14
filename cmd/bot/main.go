package main

import (
	"go.uber.org/fx"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"github.com/DevYukine/go-tradewinds/internal/logging"
	"github.com/DevYukine/go-tradewinds/internal/strategy"
)

func main() {
	fx.New(
		config.Module,   // Provides *Config
		logging.Module,  // Provides *zap.Logger
		db.Module,       // Provides *gorm.DB
		agent.Module,    // Provides agent.Agent
		strategy.Module, // Provides bot.Registry
		bot.Module,      // Provides *Manager, starts company runners
		// Step 4 will add: optimizer.Module
		// Step 5 will add: server.Module
	).Run()
}
