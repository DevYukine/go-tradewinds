package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/config"
)

// Module provides the Agent implementation to the fx DI container.
var Module = fx.Module("agent",
	fx.Provide(NewAgent),
)

// Agent makes trading decisions. Implementations can be heuristic-based,
// LLM-powered, or use custom ML models. The bot's strategy layer calls
// these methods at each decision point.
type Agent interface {
	// Name returns a human-readable identifier for this agent.
	Name() string

	// DecideTradeAction is called when a ship docks. It receives the full game
	// state and must return what to buy/sell and where to sail next.
	DecideTradeAction(ctx context.Context, req TradeDecisionRequest) (*TradeDecision, error)

	// DecideFleetAction is called periodically. It can recommend buying ships,
	// upgrading warehouses, or other capital decisions.
	DecideFleetAction(ctx context.Context, req FleetDecisionRequest) (*FleetDecision, error)

	// DecideMarketAction is called when evaluating P2P market opportunities.
	DecideMarketAction(ctx context.Context, req MarketDecisionRequest) (*MarketDecision, error)
}

// NewAgent creates the appropriate Agent implementation based on config.
func NewAgent(cfg *config.Config, logger *zap.Logger) (Agent, error) {
	return newAgentFromType(cfg.Agent.Type, cfg, logger)
}

// newAgentFromType builds an agent of the given type, allowing recursive
// construction for composite agents.
func newAgentFromType(agentType string, cfg *config.Config, logger *zap.Logger) (Agent, error) {
	switch agentType {
	case "heuristic", "":
		return NewHeuristicAgent(logger), nil

	case "llm":
		provider, err := newLLMProvider(cfg)
		if err != nil {
			logger.Warn("failed to create LLM provider, falling back to heuristic",
				zap.Error(err),
			)
			return NewHeuristicAgent(logger), nil
		}
		return NewLLMAgent(provider, cfg.Agent.LLMModel, cfg.Agent.LLMMaxTokens, logger), nil

	case "composite":
		fast, err := newAgentFromType(cfg.Agent.CompositeFastAgent, cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("create composite fast agent: %w", err)
		}
		slow, err := newAgentFromType(cfg.Agent.CompositeSlowAgent, cfg, logger)
		if err != nil {
			return nil, fmt.Errorf("create composite slow agent: %w", err)
		}
		return NewCompositeAgent(fast, slow, logger), nil

	default:
		logger.Warn("unknown agent type, falling back to heuristic",
			zap.String("configured_type", agentType),
		)
		return NewHeuristicAgent(logger), nil
	}
}

// NewAgentFromParams creates an agent from explicit settings (not global config).
// Used for per-company agent overrides. Falls back to heuristic on any error.
func NewAgentFromParams(agentType, provider, model, apiKey string, maxTokens int, logger *zap.Logger) Agent {
	if agentType == "" || agentType == "heuristic" {
		return NewHeuristicAgent(logger)
	}

	if agentType == "llm" {
		llmProvider, err := newLLMProviderFromParams(provider, model, apiKey, maxTokens)
		if err != nil {
			logger.Warn("failed to create per-company LLM provider, falling back to heuristic",
				zap.String("provider", provider),
				zap.Error(err),
			)
			return NewHeuristicAgent(logger)
		}
		return NewLLMAgent(llmProvider, model, maxTokens, logger)
	}

	logger.Warn("unknown per-company agent type, falling back to heuristic",
		zap.String("agent_type", agentType),
	)
	return NewHeuristicAgent(logger)
}

// newLLMProviderFromParams creates an LLMProvider from explicit parameters.
func newLLMProviderFromParams(provider, model, apiKey string, maxTokens int) (LLMProvider, error) {
	switch provider {
	case "claude":
		if apiKey == "" {
			return nil, fmt.Errorf("API key is required for claude provider")
		}
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		return NewClaudeProvider(apiKey, model, maxTokens), nil

	case "openai":
		if apiKey == "" {
			return nil, fmt.Errorf("API key is required for openai provider")
		}
		if model == "" {
			model = "gpt-4o"
		}
		return NewOpenAIProvider(apiKey, model, maxTokens), nil

	case "openrouter":
		if apiKey == "" {
			return nil, fmt.Errorf("API key is required for openrouter provider")
		}
		if model == "" {
			model = "anthropic/claude-sonnet-4"
		}
		return NewOpenRouterProvider(apiKey, model, maxTokens), nil

	case "ollama":
		if model == "" {
			model = "llama3"
		}
		return NewOllamaProvider(model), nil

	default:
		return nil, fmt.Errorf("unknown LLM provider: %q", provider)
	}
}

// newLLMProvider creates the appropriate LLMProvider based on config.
func newLLMProvider(cfg *config.Config) (LLMProvider, error) {
	apiKey := cfg.Agent.APIKeyForProvider(cfg.Agent.LLMProvider)
	return newLLMProviderFromParams(cfg.Agent.LLMProvider, cfg.Agent.LLMModel, apiKey, cfg.Agent.LLMMaxTokens)
}

// --- Snapshot Types ---
// These provide a serializable view of game state for agent decision-making.

// CompanySnapshot is a point-in-time view of a company's finances.
type CompanySnapshot struct {
	ID          uuid.UUID `json:"id"`
	Treasury    int64     `json:"treasury"`
	Reputation  int64     `json:"reputation"`
	TotalUpkeep int64     `json:"total_upkeep"`
}

// ShipSnapshot is a point-in-time view of a ship's state.
type ShipSnapshot struct {
	ID           uuid.UUID   `json:"id"`
	Name         string      `json:"name"`
	Status       string      `json:"status"` // "docked" or "traveling"
	PortID       *uuid.UUID  `json:"port_id"`
	Cargo        []CargoItem `json:"cargo"`
	Capacity     int         `json:"capacity"`
	Speed        int         `json:"speed"`
	Upkeep       int         `json:"upkeep"`        // Per-cycle upkeep cost.
	PassengerCap int         `json:"passenger_cap"` // Max passenger groups from ship type.
	ArrivesAt    *time.Time  `json:"arrives_at"`
	IdleTicks    int         `json:"idle_ticks"` // Consecutive "wait" ticks while docked.
	ShipType     string      `json:"ship_type"`
}

// PassengerInfo describes an available or boarded passenger group.
type PassengerInfo struct {
	ID                uuid.UUID `json:"id"`
	Count             int       `json:"count"`
	Bid               int       `json:"bid"` // Payment on delivery.
	OriginPortID      uuid.UUID `json:"origin_port_id"`
	DestinationPortID uuid.UUID `json:"destination_port_id"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// CargoItem represents a quantity of a good on a ship.
type CargoItem struct {
	GoodID   uuid.UUID `json:"good_id"`
	Quantity int       `json:"quantity"`
}

// WarehouseSnapshot is a point-in-time view of a warehouse.
type WarehouseSnapshot struct {
	ID       uuid.UUID       `json:"id"`
	PortID   uuid.UUID       `json:"port_id"`
	Level    int             `json:"level"`
	Capacity int             `json:"capacity"`
	Items    []WarehouseItem `json:"items"`
}

// WarehouseItem represents a quantity of a good in a warehouse.
type WarehouseItem struct {
	GoodID   uuid.UUID `json:"good_id"`
	Quantity int       `json:"quantity"`
}

// PricePoint records observed buy/sell prices for a good at a port.
type PricePoint struct {
	PortID     uuid.UUID `json:"port_id"`
	GoodID     uuid.UUID `json:"good_id"`
	BuyPrice   int       `json:"buy_price"`
	SellPrice  int       `json:"sell_price"`
	ObservedAt time.Time `json:"observed_at"`
}

// PortInfo is a simplified port representation for agent decisions.
type PortInfo struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Code       string    `json:"code"`
	IsHub      bool      `json:"is_hub"`
	TaxRateBps int       `json:"tax_rate_bps"`
}

// RouteInfo is a simplified route representation for agent decisions.
type RouteInfo struct {
	ID       uuid.UUID `json:"id"`
	FromID   uuid.UUID `json:"from_id"`
	ToID     uuid.UUID `json:"to_id"`
	Distance float64   `json:"distance"`
}

// ShipTypeInfo describes an available ship type for purchase decisions.
type ShipTypeInfo struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Capacity     int       `json:"capacity"`
	Speed        int       `json:"speed"`
	Upkeep       int       `json:"upkeep"`
	BasePrice    int       `json:"base_price"`
	PassengerCap int       `json:"passenger_cap"` // Maximum number of passenger groups this ship can carry.
}

// TradeLogEntry is a recent trade for context in decision-making.
type TradeLogEntry struct {
	Action    string    `json:"action"`
	GoodID    uuid.UUID `json:"good_id"`
	PortID    uuid.UUID `json:"port_id"`
	Quantity  int       `json:"quantity"`
	UnitPrice int       `json:"unit_price"`
	CreatedAt time.Time `json:"created_at"`
}

// RoutePerformanceEntry is a historical buy→sell route result used to
// bias destination scoring toward routes that have been profitable.
type RoutePerformanceEntry struct {
	FromPortID uuid.UUID `json:"from_port_id"`
	ToPortID   uuid.UUID `json:"to_port_id"`
	GoodID     uuid.UUID `json:"good_id"`
	Profit     int       `json:"profit"`
	Quantity   int       `json:"quantity"`
	CreatedAt  time.Time `json:"created_at"`
}

// TradeOpportunity represents a cross-port trade opportunity discovered by
// the ProfitAnalyzer. Passed to agents to guide idle ships toward profit.
type TradeOpportunity struct {
	BuyPortID  uuid.UUID `json:"buy_port_id"`
	SellPortID uuid.UUID `json:"sell_port_id"`
	GoodID     uuid.UUID `json:"good_id"`
	BuyPrice   int       `json:"buy_price"`
	SellPrice  int       `json:"sell_price"`
	Profit     int       `json:"profit"`
	Distance   float64   `json:"distance"`
	Score      float64   `json:"score"`
}

// Constraints defines safety boundaries for trading decisions.
type Constraints struct {
	TreasuryFloor int64 `json:"treasury_floor"` // Minimum treasury to maintain (2x upkeep).
	MaxSpend      int64 `json:"max_spend"`       // Maximum to spend on a single trade.
}

// --- Trade Decision ---

// TradeDecisionRequest contains everything an agent needs to decide a trade.
type TradeDecisionRequest struct {
	StrategyHint        string               `json:"strategy_hint"`        // "arbitrage", "bulk_hauler", "market_maker" — guides agent behavior.
	Company             CompanySnapshot         `json:"company"`
	Ship                ShipSnapshot            `json:"ship"`
	AllShips            []ShipSnapshot          `json:"all_ships"`
	Warehouses          []WarehouseSnapshot     `json:"warehouses"`
	PriceCache          []PricePoint            `json:"price_cache"`
	Routes              []RouteInfo             `json:"routes"`
	Ports               []PortInfo              `json:"ports"`
	RecentTrades        []TradeLogEntry         `json:"recent_trades"`
	RouteHistory        []RoutePerformanceEntry `json:"route_history"`        // Recent route performance data for learning.
	Constraints         Constraints             `json:"constraints"`
	AvailablePassengers []PassengerInfo         `json:"available_passengers"` // Passengers at the current port looking for transport.
	BoardedPassengers   []PassengerInfo       `json:"boarded_passengers"`   // Passengers already on this ship.
	PortOrders          []MarketOrder         `json:"port_orders"`          // P2P orders at the current port (for filling opportunities).
	OwnOrders           []MarketOrder         `json:"own_orders"`           // This company's active orders (to avoid self-fill).
	TopOpportunities    []TradeOpportunity    `json:"top_opportunities"`    // Top global trade opportunities from ProfitAnalyzer.
	ClaimedRoutes       []string              `json:"claimed_routes"`       // Routes claimed by other ships via Coordinator.
	Params              map[string]float64    `json:"params"`               // Tunable parameters from optimizer (nil = use defaults).
}

// SellOrder instructs the bot to sell a good at the current port.
type SellOrder struct {
	GoodID   uuid.UUID `json:"good_id"`
	Quantity int       `json:"quantity"`
}

// BuyOrder instructs the bot to buy a good and load it onto a destination.
type BuyOrder struct {
	GoodID      uuid.UUID `json:"good_id"`
	Quantity    int       `json:"quantity"`
	Destination uuid.UUID `json:"destination"` // Ship or warehouse ID.
}

// WarehouseTransfer describes a cargo transfer between a ship and a warehouse.
type WarehouseTransfer struct {
	WarehouseID uuid.UUID `json:"warehouse_id"`
	GoodID      uuid.UUID `json:"good_id"`
	Quantity    int       `json:"quantity"`
}

// TradeDecision is the agent's response to a trade decision request.
type TradeDecision struct {
	Action          string              `json:"action"`           // "buy_and_sail", "sell_and_buy", "wait", "dock"
	SellOrders      []SellOrder         `json:"sell_orders"`      // What to sell at current port.
	BuyOrders       []BuyOrder          `json:"buy_orders"`       // What to buy before departing.
	FillOrders      []FillOrder         `json:"fill_orders"`      // P2P orders to fill at the current port.
	WarehouseLoads  []WarehouseTransfer `json:"warehouse_loads"`  // Load goods from warehouse onto ship.
	WarehouseStores []WarehouseTransfer `json:"warehouse_stores"` // Store goods from ship into warehouse.
	BoardPassengers []uuid.UUID         `json:"board_passengers"` // Passenger IDs to board before departing.
	SailTo          *uuid.UUID          `json:"sail_to"`          // Destination port (nil = stay docked).
	Reasoning       string              `json:"reasoning"`        // Human-readable explanation.
	Confidence      float64             `json:"confidence"`       // 0.0-1.0, used by optimizer to weight decisions.
}

// --- Fleet Decision ---

// FleetDecisionRequest contains state needed for capital investment decisions.
type FleetDecisionRequest struct {
	StrategyHint  string              `json:"strategy_hint"`  // "arbitrage", "bulk_hauler", "market_maker" — guides ship selection.
	Company       CompanySnapshot     `json:"company"`
	Ships         []ShipSnapshot      `json:"ships"`
	Warehouses    []WarehouseSnapshot `json:"warehouses"`
	ShipTypes     []ShipTypeInfo      `json:"ship_types"`
	PriceCache    []PricePoint        `json:"price_cache"`
	ShipyardPorts []uuid.UUID              `json:"shipyard_ports"` // Port IDs that have shipyards (not all ports do).
	Ports         []PortInfo               `json:"ports"`          // Port details including tax rates for purchase cost calculation.
	RouteHistory  []RoutePerformanceEntry  `json:"route_history"`  // Recent route performance for warehouse placement.
}

// ShipPurchase describes a ship to buy at a specific port.
type ShipPurchase struct {
	ShipTypeID uuid.UUID `json:"ship_type_id"`
	PortID     uuid.UUID `json:"port_id"`
}

// WarehouseAction describes a scaling action for an existing warehouse.
type WarehouseAction struct {
	WarehouseID uuid.UUID `json:"warehouse_id"`
	Action      string    `json:"action"` // "grow", "shrink", "demolish"
}

// FleetDecision is the agent's response to a fleet decision request.
type FleetDecision struct {
	BuyShips         []ShipPurchase   `json:"buy_ships"`
	SellShips        []uuid.UUID      `json:"sell_ships"`         // Ship IDs to decommission (sell back to game).
	BuyWarehouses    []uuid.UUID      `json:"buy_warehouses"`     // Port IDs to build warehouses at.
	WarehouseActions []WarehouseAction `json:"warehouse_actions"`  // Grow/shrink/demolish existing warehouses.
	Reasoning        string           `json:"reasoning"`
}

// --- Market Decision ---

// MarketDecisionRequest contains state for P2P market decisions.
type MarketDecisionRequest struct {
	Company    CompanySnapshot     `json:"company"`
	OpenOrders []MarketOrder       `json:"open_orders"`
	OwnOrders  []MarketOrder       `json:"own_orders"`
	PriceCache []PricePoint        `json:"price_cache"`
	Warehouses []WarehouseSnapshot `json:"warehouses"`
}

// MarketOrder represents a P2P market order.
type MarketOrder struct {
	ID        uuid.UUID `json:"id"`
	PortID    uuid.UUID `json:"port_id"`
	GoodID    uuid.UUID `json:"good_id"`
	Side      string    `json:"side"` // "buy" or "sell"
	Price     int       `json:"price"`
	Remaining int       `json:"remaining"`
}

// NewMarketOrder describes a new order to post.
type NewMarketOrder struct {
	PortID uuid.UUID `json:"port_id"`
	GoodID uuid.UUID `json:"good_id"`
	Side   string    `json:"side"`
	Price  int       `json:"price"`
	Total  int       `json:"total"`
}

// FillOrder describes an existing order to fill.
type FillOrder struct {
	OrderID  uuid.UUID `json:"order_id"`
	Quantity int       `json:"quantity"`
}

// MarketDecision is the agent's response to a market decision request.
type MarketDecision struct {
	PostOrders   []NewMarketOrder `json:"post_orders"`
	FillOrders   []FillOrder      `json:"fill_orders"`
	CancelOrders []uuid.UUID      `json:"cancel_orders"`
	Reasoning    string           `json:"reasoning"`
}

