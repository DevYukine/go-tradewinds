package db

import (
	"time"
)

// CompanyRecord tracks each bot-managed company in the game.
type CompanyRecord struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	GameID     string    `gorm:"uniqueIndex;not null" json:"game_id"`
	Name       string    `gorm:"not null" json:"name"`
	Ticker     string    `gorm:"not null;size:5" json:"ticker"`
	HomePortID string    `gorm:"not null" json:"home_port_id"`
	Strategy   string    `gorm:"not null" json:"strategy"`
	Status     string    `gorm:"not null;default:running;index" json:"status"`
	Treasury   int64     `json:"treasury"`
	Reputation int64     `json:"reputation"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// TradeLog records every trade executed by the bot.
type TradeLog struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	CompanyID    uint      `gorm:"index:idx_trade_company_time;index:idx_trade_company_action;index:idx_trade_buy_lookup;not null" json:"company_id"`
	Action       string    `gorm:"not null;size:4;index:idx_trade_company_action;index:idx_trade_buy_lookup" json:"action"`
	GoodID       string    `gorm:"not null;index:idx_trade_buy_lookup" json:"good_id"`
	GoodName     string    `gorm:"not null" json:"good_name"`
	PortID       string    `gorm:"not null" json:"port_id"`
	PortName     string    `gorm:"not null" json:"port_name"`
	Quantity     int       `gorm:"not null" json:"quantity"`
	UnitPrice    int       `gorm:"not null" json:"unit_price"`
	TotalPrice   int       `gorm:"not null" json:"total_price"`
	TaxPaid      int       `json:"tax_paid"`
	ShipID       string    `json:"ship_id"`
	ShipName     string    `json:"ship_name"`
	Source       string    `json:"source"`         // "port_buy", "port_sell", "warehouse_load", "p2p_fill"
	DestPortID   string    `json:"dest_port_id"`   // Intended sell destination (for buy trades)
	DestPortName string    `json:"dest_port_name"` // Destination port name
	Matched      bool      `json:"matched"`        // Whether this buy has been matched to a sell (FIFO)
	Strategy     string    `gorm:"not null" json:"strategy"`
	AgentName    string    `json:"agent_name"`
	CreatedAt    time.Time `gorm:"index:idx_trade_company_time;not null" json:"created_at"`
}

// PnLSnapshot stores periodic profit/loss snapshots per company.
type PnLSnapshot struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	CompanyID  uint      `gorm:"index:idx_pnl_company_time;not null" json:"company_id"`
	Treasury   int64     `gorm:"not null" json:"treasury"`
	TotalCosts      int64     `json:"total_costs"`
	TotalRev        int64     `json:"total_rev"`
	PassengerRev    int64     `json:"passenger_rev"`
	ShipCosts       int64     `json:"ship_costs"`
	NetPnL          int64     `json:"net_pnl"`
	ShipCount       int       `json:"ship_count"`
	AvgCapacityUtil float64   `json:"avg_capacity_util"`
	CreatedAt       time.Time `gorm:"index:idx_pnl_company_time;not null" json:"created_at"`
}

// ShipEventLog records ship purchases and sales.
type ShipEventLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CompanyID uint      `gorm:"index" json:"company_id"`
	ShipID    string    `json:"ship_id"`
	ShipName  string    `json:"ship_name"`
	ShipType  string    `json:"ship_type"`
	EventType string    `json:"event_type"` // "purchase", "sale"
	Price     int       `json:"price"`
	Treasury  int       `json:"treasury"` // Treasury after event
	PortID    string    `json:"port_id"`
	PortName  string    `json:"port_name"`
	Strategy  string    `json:"strategy"`
	AgentName string    `json:"agent_name"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// WarehouseEventLog records warehouse purchases, stores, and loads.
type WarehouseEventLog struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	CompanyID   uint      `gorm:"index" json:"company_id"`
	WarehouseID string    `json:"warehouse_id"`
	PortID      string    `json:"port_id"`
	PortName    string    `json:"port_name"`
	EventType   string    `json:"event_type"` // "purchase", "store", "load"
	GoodID      string    `json:"good_id"`
	GoodName    string    `json:"good_name"`
	Quantity    int       `json:"quantity"`
	Level       int       `json:"level"` // Warehouse level (for purchase events)
	Strategy    string    `json:"strategy"`
	AgentName   string    `json:"agent_name"`
	CreatedAt   time.Time `gorm:"index" json:"created_at"`
}

// P2POrderLog records P2P market order activity.
type P2POrderLog struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	CompanyID  uint      `gorm:"index" json:"company_id"`
	OrderID    string    `json:"order_id"`
	OrderType  string    `json:"order_type"` // "post", "fill", "cancel"
	GoodID     string    `json:"good_id"`
	GoodName   string    `json:"good_name"`
	PortID     string    `json:"port_id"`
	PortName   string    `json:"port_name"`
	Quantity   int       `json:"quantity"`
	Price      int       `json:"price"`
	TotalValue int       `json:"total_value"`
	Strategy   string    `json:"strategy"`
	AgentName  string    `json:"agent_name"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// StrategyChangeLog records strategy swaps by the optimizer.
type StrategyChangeLog struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	CompanyID    uint      `gorm:"index" json:"company_id"`
	FromStrategy string    `json:"from_strategy"`
	ToStrategy   string    `json:"to_strategy"`
	Reason       string    `json:"reason"` // Optimizer reason or "manual"
	CreatedAt    time.Time `gorm:"index" json:"created_at"`
}

// QuoteFailureLog records failed quote attempts for analysis.
type QuoteFailureLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CompanyID uint      `gorm:"index" json:"company_id"`
	ShipID    string    `json:"ship_id"`
	GoodID    string    `json:"good_id"`
	GoodName  string    `json:"good_name"`
	PortID    string    `json:"port_id"`
	PortName  string    `json:"port_name"`
	Action    string    `json:"action"` // "buy" or "sell"
	Quantity  int       `json:"quantity"`
	ExpPrice  int       `json:"exp_price"` // Expected price
	ActPrice  int       `json:"act_price"` // Actual quoted price (0 if no quote)
	Reason    string    `json:"reason"`    // "price_moved", "out_of_stock", "api_error"
	Strategy  string    `json:"strategy"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// StrategyMetric tracks per-strategy performance aggregated across companies.
type StrategyMetric struct {
	ID                uint      `gorm:"primaryKey" json:"id"`
	StrategyName      string    `gorm:"index;not null" json:"strategy_name"`
	CompanyCount      int       `gorm:"not null" json:"company_count"`
	TradesExecuted    int       `json:"trades_executed"`
	TotalProfit       int64     `json:"total_profit"`
	TotalLoss         int64     `json:"total_loss"`
	AvgProfitPerTrade float64   `json:"avg_profit_per_trade"`
	StdDevProfit      float64   `json:"std_dev_profit"`
	WinRate           float64   `json:"win_rate"`
	ConfidenceLow     float64   `json:"confidence_low"`
	ConfidenceHigh    float64   `json:"confidence_high"`
	PeriodStart       time.Time `json:"period_start"`
	PeriodEnd         time.Time `json:"period_end"`
	CreatedAt         time.Time `json:"created_at"`
}

// CompanyLog stores log lines for dashboard streaming and historical view.
type CompanyLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	CompanyID uint      `gorm:"index:idx_log_company_time;not null" json:"company_id"`
	Level     string    `gorm:"not null;size:10" json:"level"`
	Message   string    `gorm:"type:text;not null" json:"message"`
	CreatedAt time.Time `gorm:"index:idx_log_company_time;not null" json:"created_at"`
}

// PriceObservation records NPC prices for trend analysis.
type PriceObservation struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	PortID    string    `gorm:"index:idx_price_port_good;not null" json:"port_id"`
	GoodID    string    `gorm:"index:idx_price_port_good;not null" json:"good_id"`
	BuyPrice  int       `gorm:"not null" json:"buy_price"`
	SellPrice int       `gorm:"not null" json:"sell_price"`
	CreatedAt time.Time `gorm:"index;not null" json:"created_at"`
}

// AgentDecisionLog records every decision made by an agent for analysis and replay.
type AgentDecisionLog struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	CompanyID    uint      `gorm:"index:idx_decision_company_time;not null" json:"company_id"`
	AgentName    string    `gorm:"not null" json:"agent_name"`
	DecisionType string    `gorm:"not null;size:20" json:"decision_type"`
	Request      string    `gorm:"type:text" json:"request"`
	Response     string    `gorm:"type:text" json:"response"`
	Reasoning    string    `gorm:"type:text" json:"reasoning"`
	Confidence   float64   `json:"confidence"`
	LatencyMs    int64     `json:"latency_ms"`
	Outcome      string    `gorm:"size:10" json:"outcome"`
	OutcomeValue int64     `json:"outcome_value"`
	CreatedAt    time.Time `gorm:"index:idx_decision_company_time;not null" json:"created_at"`
}

// RoutePerformance records the profit of completed buy→sell trade cycles per route.
type RoutePerformance struct {
	ID         uint      `gorm:"primaryKey" json:"id"`
	CompanyID  uint      `gorm:"index:idx_route_company_time;not null" json:"company_id"`
	FromPortID string    `gorm:"not null" json:"from_port_id"`
	ToPortID   string    `gorm:"not null" json:"to_port_id"`
	GoodID     string    `gorm:"not null" json:"good_id"`
	BuyPrice   int       `gorm:"not null" json:"buy_price"`
	SellPrice  int       `gorm:"not null" json:"sell_price"`
	Quantity   int       `gorm:"not null" json:"quantity"`
	Profit     int       `gorm:"not null" json:"profit"`
	Strategy   string    `gorm:"not null" json:"strategy"`
	CreatedAt  time.Time `gorm:"index:idx_route_company_time;not null" json:"created_at"`
}

// PassengerLog records every passenger boarding executed by the bot.
type PassengerLog struct {
	ID                  uint      `gorm:"primaryKey" json:"id"`
	CompanyID           uint      `gorm:"index:idx_passenger_company_time;not null" json:"company_id"`
	PassengerID         string    `gorm:"not null" json:"passenger_id"`
	Count               int       `gorm:"not null" json:"count"`
	Bid                 int       `gorm:"not null" json:"bid"`
	OriginPortID        string    `gorm:"not null" json:"origin_port_id"`
	OriginPortName      string    `gorm:"not null" json:"origin_port_name"`
	DestinationPortID   string    `gorm:"not null" json:"destination_port_id"`
	DestinationPortName string    `gorm:"not null" json:"destination_port_name"`
	ShipID              string    `gorm:"not null" json:"ship_id"`
	ShipName            string    `gorm:"not null" json:"ship_name"`
	Strategy            string    `gorm:"not null" json:"strategy"`
	AgentName           string    `json:"agent_name"`
	CreatedAt           time.Time `gorm:"index:idx_passenger_company_time;not null" json:"created_at"`
}

// CompanyParams stores tunable trading parameters per company.
// The optimizer adjusts these through experiments.
type CompanyParams struct {
	ID                      uint      `gorm:"primaryKey" json:"id"`
	CompanyID               uint      `gorm:"uniqueIndex;not null" json:"company_id"`
	MinMarginPct            float64   `gorm:"not null;default:0.05" json:"min_margin_pct"`
	PassengerWeight         float64   `gorm:"not null;default:5.0" json:"passenger_weight"`
	SpeculativeTradeEnabled bool      `gorm:"not null;default:true" json:"speculative_trade_enabled"`
	MarketEvalIntervalSec   int       `gorm:"not null;default:60" json:"market_eval_interval_sec"`
	FleetEvalIntervalSec    int       `gorm:"not null;default:180" json:"fleet_eval_interval_sec"`
	PassengerDestBonus      float64   `gorm:"not null;default:5.0" json:"passenger_dest_bonus"`
	AgentType               string    `gorm:"not null;default:heuristic;size:20" json:"agent_type"`   // "heuristic", "llm", "composite"
	LLMProvider             string    `gorm:"size:20" json:"llm_provider"`                             // "claude", "openai", "ollama"
	LLMModel                string    `gorm:"size:100" json:"llm_model"`                               // e.g. "claude-sonnet-4-20250514", "gpt-4o"
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

// ParamExperimentLog records optimizer parameter tuning experiments.
type ParamExperimentLog struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	CompanyID    uint      `gorm:"index:idx_experiment_company;not null" json:"company_id"`
	ParamName    string    `gorm:"not null" json:"param_name"`
	OldValue     float64   `gorm:"not null" json:"old_value"`
	NewValue     float64   `gorm:"not null" json:"new_value"`
	ProfitBefore float64   `json:"profit_before"`
	ProfitAfter  float64   `json:"profit_after"`
	Status       string    `gorm:"not null;default:active;size:20;index" json:"status"` // active, completed, reverted
	CreatedAt    time.Time `gorm:"index;not null" json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// AllModels returns all GORM models for auto-migration.
func AllModels() []any {
	return []any{
		&CompanyRecord{},
		&TradeLog{},
		&PnLSnapshot{},
		&StrategyMetric{},
		&CompanyLog{},
		&PriceObservation{},
		&AgentDecisionLog{},
		&RoutePerformance{},
		&PassengerLog{},
		&CompanyParams{},
		&ParamExperimentLog{},
		&ShipEventLog{},
		&WarehouseEventLog{},
		&P2POrderLog{},
		&StrategyChangeLog{},
		&QuoteFailureLog{},
	}
}
