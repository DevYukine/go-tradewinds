package strategy

import (
	"go.uber.org/fx"

	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// Module provides the strategy Registry to the fx DI container.
// Strategy implementations are registered here; actual strategy logic
// will be added in Step 4.
var Module = fx.Module("strategy",
	fx.Provide(NewRegistry),
)

// NewRegistry creates the strategy registry with all available strategy factories.
// Currently returns an empty registry — strategy implementations (arbitrage,
// bulk_hauler, market_maker) will be registered in Step 4.
func NewRegistry() bot.Registry {
	return bot.Registry{
		// Step 4 will add:
		// "arbitrage":    arbitrage.New,
		// "bulk_hauler":  bulk_hauler.New,
		// "market_maker": market_maker.New,
	}
}
