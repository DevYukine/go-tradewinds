package bot

import (
	"context"

	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
)

// Strategy defines the trading behavior for a company. Strategies gather game
// state and delegate decisions to an Agent, then execute the agent's response.
type Strategy interface {
	// Name returns the strategy identifier (e.g., "arbitrage", "bulk_hauler").
	Name() string

	// Init sets up the strategy with its dependencies. Called once before the
	// company runner's main loop starts.
	Init(ctx StrategyContext) error

	// OnShipArrival is called when a ship docks at a port. The strategy should
	// gather state, call the agent, and execute the resulting trade decisions.
	OnShipArrival(ctx context.Context, ship *ShipState, port *api.Port) error

	// OnTick is called periodically (every ~60s). Used for fleet management,
	// economy checks, and non-trade-related decisions.
	OnTick(ctx context.Context, state *CompanyState) error

	// Shutdown is called when the company runner is stopping. Clean up resources.
	Shutdown() error
}

// StrategyContext provides all dependencies a strategy needs to operate.
type StrategyContext struct {
	Client     *api.Client
	State      *CompanyState
	World      *WorldCache
	PriceCache *PriceCache
	Agent      agent.Agent
	Logger     *CompanyLogger
	Events     *EventBroadcaster
	DB         *gorm.DB
}

// StrategyFactory creates a new Strategy instance. Each factory is registered
// in the Registry under the strategy's name.
type StrategyFactory func(ctx StrategyContext) (Strategy, error)

// Registry maps strategy names to their factory functions.
// Populated by the strategy package and injected into the bot via fx.
type Registry map[string]StrategyFactory
