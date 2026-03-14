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

	// EvaluateStrategy is called by the optimizer. Given performance metrics,
	// the agent can recommend parameter adjustments or strategy switches.
	EvaluateStrategy(ctx context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error)
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
	ID          uuid.UUID
	Treasury    int64
	Reputation  int64
	TotalUpkeep int64
}

// ShipSnapshot is a point-in-time view of a ship's state.
type ShipSnapshot struct {
	ID           uuid.UUID
	Name         string
	Status       string // "docked" or "traveling"
	PortID       *uuid.UUID
	Cargo        []CargoItem
	Capacity     int
	Speed        int
	PassengerCap int // Max passenger groups from ship type.
	ArrivesAt    *time.Time
}

// PassengerInfo describes an available or boarded passenger group.
type PassengerInfo struct {
	ID                uuid.UUID
	Count             int
	Bid               int // Payment on delivery.
	OriginPortID      uuid.UUID
	DestinationPortID uuid.UUID
	ExpiresAt         time.Time
}

// CargoItem represents a quantity of a good on a ship.
type CargoItem struct {
	GoodID   uuid.UUID
	Quantity int
}

// WarehouseSnapshot is a point-in-time view of a warehouse.
type WarehouseSnapshot struct {
	ID       uuid.UUID
	PortID   uuid.UUID
	Level    int
	Capacity int
	Items    []WarehouseItem
}

// WarehouseItem represents a quantity of a good in a warehouse.
type WarehouseItem struct {
	GoodID   uuid.UUID
	Quantity int
}

// PricePoint records observed buy/sell prices for a good at a port.
type PricePoint struct {
	PortID     uuid.UUID
	GoodID     uuid.UUID
	BuyPrice   int
	SellPrice  int
	ObservedAt time.Time
}

// PortInfo is a simplified port representation for agent decisions.
type PortInfo struct {
	ID         uuid.UUID
	Name       string
	Code       string
	IsHub      bool
	TaxRateBps int
}

// RouteInfo is a simplified route representation for agent decisions.
type RouteInfo struct {
	ID       uuid.UUID
	FromID   uuid.UUID
	ToID     uuid.UUID
	Distance float64
}

// ShipTypeInfo describes an available ship type for purchase decisions.
type ShipTypeInfo struct {
	ID           uuid.UUID
	Name         string
	Capacity     int
	Speed        int
	Upkeep       int
	BasePrice    int
	PassengerCap int // Maximum number of passenger groups this ship can carry.
}

// TradeLogEntry is a recent trade for context in decision-making.
type TradeLogEntry struct {
	Action    string
	GoodID    uuid.UUID
	PortID    uuid.UUID
	Quantity  int
	UnitPrice int
	CreatedAt time.Time
}

// RoutePerformanceEntry is a historical buy→sell route result used to
// bias destination scoring toward routes that have been profitable.
type RoutePerformanceEntry struct {
	FromPortID uuid.UUID
	ToPortID   uuid.UUID
	GoodID     uuid.UUID
	Profit     int
	Quantity   int
	CreatedAt  time.Time
}

// Constraints defines safety boundaries for trading decisions.
type Constraints struct {
	TreasuryFloor int64 // Minimum treasury to maintain (2x upkeep).
	MaxSpend      int64 // Maximum to spend on a single trade.
}

// --- Trade Decision ---

// TradeDecisionRequest contains everything an agent needs to decide a trade.
type TradeDecisionRequest struct {
	StrategyHint        string // "arbitrage", "bulk_hauler", "market_maker" — guides agent behavior.
	Company             CompanySnapshot
	Ship                ShipSnapshot
	AllShips            []ShipSnapshot
	Warehouses          []WarehouseSnapshot
	PriceCache          []PricePoint
	Routes              []RouteInfo
	Ports               []PortInfo
	RecentTrades        []TradeLogEntry
	RouteHistory        []RoutePerformanceEntry // Recent route performance data for learning.
	Constraints         Constraints
	AvailablePassengers []PassengerInfo // Passengers at the current port looking for transport.
	BoardedPassengers   []PassengerInfo // Passengers already on this ship.
	PortOrders          []MarketOrder   // P2P orders at the current port (for filling opportunities).
	OwnOrders           []MarketOrder   // This company's active orders (to avoid self-fill).
	Params              map[string]float64 // Tunable parameters from optimizer (nil = use defaults).
}

// SellOrder instructs the bot to sell a good at the current port.
type SellOrder struct {
	GoodID   uuid.UUID
	Quantity int
}

// BuyOrder instructs the bot to buy a good and load it onto a destination.
type BuyOrder struct {
	GoodID      uuid.UUID
	Quantity    int
	Destination uuid.UUID // Ship or warehouse ID.
}

// TradeDecision is the agent's response to a trade decision request.
type TradeDecision struct {
	Action          string      // "buy_and_sail", "sell_and_buy", "wait", "dock"
	SellOrders      []SellOrder // What to sell at current port.
	BuyOrders       []BuyOrder  // What to buy before departing.
	FillOrders      []FillOrder // P2P orders to fill at the current port.
	BoardPassengers []uuid.UUID // Passenger IDs to board before departing.
	SailTo          *uuid.UUID  // Destination port (nil = stay docked).
	Reasoning       string      // Human-readable explanation.
	Confidence      float64     // 0.0-1.0, used by optimizer to weight decisions.
}

// --- Fleet Decision ---

// FleetDecisionRequest contains state needed for capital investment decisions.
type FleetDecisionRequest struct {
	StrategyHint  string // "arbitrage", "bulk_hauler", "market_maker" — guides ship selection.
	Company       CompanySnapshot
	Ships         []ShipSnapshot
	Warehouses    []WarehouseSnapshot
	ShipTypes     []ShipTypeInfo
	PriceCache    []PricePoint
	ShipyardPorts []uuid.UUID // Port IDs that have shipyards (not all ports do).
}

// ShipPurchase describes a ship to buy at a specific port.
type ShipPurchase struct {
	ShipTypeID uuid.UUID
	PortID     uuid.UUID
}

// FleetDecision is the agent's response to a fleet decision request.
type FleetDecision struct {
	BuyShips      []ShipPurchase
	SellShips     []uuid.UUID // Ship IDs to decommission (sell back to game).
	BuyWarehouses []uuid.UUID // Port IDs to build warehouses at.
	Reasoning     string
}

// --- Market Decision ---

// MarketDecisionRequest contains state for P2P market decisions.
type MarketDecisionRequest struct {
	Company    CompanySnapshot
	OpenOrders []MarketOrder
	OwnOrders  []MarketOrder
	PriceCache []PricePoint
	Warehouses []WarehouseSnapshot
}

// MarketOrder represents a P2P market order.
type MarketOrder struct {
	ID        uuid.UUID
	PortID    uuid.UUID
	GoodID    uuid.UUID
	Side      string // "buy" or "sell"
	Price     int
	Remaining int
}

// NewMarketOrder describes a new order to post.
type NewMarketOrder struct {
	PortID uuid.UUID
	GoodID uuid.UUID
	Side   string
	Price  int
	Total  int
}

// FillOrder describes an existing order to fill.
type FillOrder struct {
	OrderID  uuid.UUID
	Quantity int
}

// MarketDecision is the agent's response to a market decision request.
type MarketDecision struct {
	PostOrders   []NewMarketOrder
	FillOrders   []FillOrder
	CancelOrders []uuid.UUID
	Reasoning    string
}

// --- Strategy Evaluation ---

// StrategyMetrics holds aggregated performance data for a strategy.
type StrategyMetrics struct {
	StrategyName   string
	CompanyCount   int
	TradesExecuted int
	TotalProfit    int64
	TotalLoss      int64
	WinRate        float64
	ProfitPerHour  float64
}

// StrategyEvalRequest provides metrics for the agent to evaluate strategies.
type StrategyEvalRequest struct {
	Metrics       []StrategyMetrics
	CurrentParams map[string]any
}

// StrategyEvaluation is the agent's recommendation for strategy changes.
type StrategyEvaluation struct {
	ParamChanges map[string]any
	SwitchTo     *string // Recommend switching strategy (nil = keep current).
	Reasoning    string
}
