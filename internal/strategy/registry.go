package strategy

import (
	"go.uber.org/fx"

	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// Module provides the strategy Registry. The price scanner is started by the
// bot Manager after world data is loaded, to avoid lifecycle ordering issues.
var Module = fx.Module("strategy",
	fx.Provide(NewRegistry),
)

// NewRegistry creates the strategy registry with all available strategy factories.
func NewRegistry() bot.Registry {
	return bot.Registry{
		"arbitrage":        NewArbitrage,
		"bulk_hauler":      NewBulkHauler,
		"market_maker":     NewMarketMaker,
		"passenger_sniper": NewPassengerSniper,
		"feeder":           NewFeeder,
		"harvester":        NewHarvester,
	}
}
