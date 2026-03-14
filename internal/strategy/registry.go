package strategy

import (
	"context"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// Module provides the strategy Registry and starts the shared price scanner.
var Module = fx.Module("strategy",
	fx.Provide(NewRegistry),
	fx.Invoke(RegisterScanner),
)

// NewRegistry creates the strategy registry with all available strategy factories.
func NewRegistry() bot.Registry {
	return bot.Registry{
		"arbitrage":    NewArbitrage,
		"bulk_hauler":  NewBulkHauler,
		"market_maker": NewMarketMaker,
	}
}

// RegisterScanner starts the shared price scanner via fx lifecycle.
// The scanner runs as a single goroutine using the base client (no company
// header needed for batch quotes) and feeds the shared PriceCache.
func RegisterScanner(lc fx.Lifecycle, m *bot.Manager, logger *zap.Logger) {
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			world := m.WorldData()
			if world == nil {
				logger.Warn("world data not loaded yet, scanner will not start")
				return nil
			}

			scanner := NewScanner(
				m.BaseClient(),
				world,
				m.PriceCache(),
				m.RateLimiter(),
				m.DB(),
				logger,
			)
			go scanner.Run(ctx)
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}
