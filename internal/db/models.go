package db

import (
	"time"
)

// CompanyRecord tracks each bot-managed company in the game.
type CompanyRecord struct {
	ID         uint   `gorm:"primaryKey"`
	GameID     string `gorm:"uniqueIndex;not null"` // Game UUID of the company.
	Name       string `gorm:"not null"`
	Ticker     string `gorm:"not null;size:5"`
	HomePortID string `gorm:"not null"`
	Strategy   string `gorm:"not null"`              // Current strategy name.
	Status     string `gorm:"not null;default:running"` // running, paused, error, bankrupt.
	Treasury   int64
	Reputation int64
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TradeLog records every trade executed by the bot.
type TradeLog struct {
	ID         uint      `gorm:"primaryKey"`
	CompanyID  uint      `gorm:"index:idx_trade_company_time;not null"`
	Action     string    `gorm:"not null;size:4"` // "buy" or "sell".
	GoodID     string    `gorm:"not null"`
	GoodName   string    `gorm:"not null"`
	PortID     string    `gorm:"not null"`
	PortName   string    `gorm:"not null"`
	Quantity   int       `gorm:"not null"`
	UnitPrice  int       `gorm:"not null"`
	TotalPrice int       `gorm:"not null"`
	TaxPaid    int
	Strategy   string    `gorm:"not null"`
	AgentName  string    // Which agent made this decision.
	CreatedAt  time.Time `gorm:"index:idx_trade_company_time;not null"`
}

// PnLSnapshot stores periodic profit/loss snapshots per company.
type PnLSnapshot struct {
	ID         uint      `gorm:"primaryKey"`
	CompanyID  uint      `gorm:"index:idx_pnl_company_time;not null"`
	Treasury   int64     `gorm:"not null"`
	TotalCosts int64     // Cumulative upkeep + taxes.
	TotalRev   int64     // Cumulative trade revenue.
	NetPnL     int64     // Treasury - initial deposit.
	ShipCount  int
	CreatedAt  time.Time `gorm:"index:idx_pnl_company_time;not null"`
}

// InventorySnapshot tracks cargo and warehouse state over time.
type InventorySnapshot struct {
	ID        uint      `gorm:"primaryKey"`
	CompanyID uint      `gorm:"index:idx_inv_company_time;not null"`
	Location  string    `gorm:"not null"` // "ship:<uuid>" or "warehouse:<uuid>".
	GoodID    string    `gorm:"not null"`
	GoodName  string    `gorm:"not null"`
	Quantity  int       `gorm:"not null"`
	CreatedAt time.Time `gorm:"index:idx_inv_company_time;not null"`
}

// StrategyMetric tracks per-strategy performance aggregated across companies.
type StrategyMetric struct {
	ID                uint      `gorm:"primaryKey"`
	StrategyName      string    `gorm:"index;not null"`
	CompanyCount      int       `gorm:"not null"` // How many companies ran this strategy.
	TradesExecuted    int
	TotalProfit       int64
	TotalLoss         int64
	AvgProfitPerTrade float64
	StdDevProfit      float64 // Standard deviation across companies.
	WinRate           float64
	ConfidenceLow     float64 // 95% CI lower bound on profit/hour.
	ConfidenceHigh    float64 // 95% CI upper bound.
	PeriodStart       time.Time
	PeriodEnd         time.Time
	CreatedAt         time.Time
}

// CompanyLog stores log lines for dashboard streaming and historical view.
type CompanyLog struct {
	ID        uint      `gorm:"primaryKey"`
	CompanyID uint      `gorm:"index:idx_log_company_time;not null"`
	Level     string    `gorm:"not null;size:10"` // info, warn, error, trade, event, optimizer, agent.
	Message   string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index:idx_log_company_time;not null"`
}

// PriceObservation records NPC prices for trend analysis.
type PriceObservation struct {
	ID        uint      `gorm:"primaryKey"`
	PortID    string    `gorm:"index:idx_price_port_good;not null"`
	GoodID    string    `gorm:"index:idx_price_port_good;not null"`
	BuyPrice  int       `gorm:"not null"`
	SellPrice int       `gorm:"not null"`
	CreatedAt time.Time `gorm:"not null"`
}

// AgentDecisionLog records every decision made by an agent for analysis and replay.
type AgentDecisionLog struct {
	ID           uint      `gorm:"primaryKey"`
	CompanyID    uint      `gorm:"index:idx_decision_company_time;not null"`
	AgentName    string    `gorm:"not null"`
	DecisionType string    `gorm:"not null;size:20"` // trade, fleet, market, strategy_eval.
	Request      string    `gorm:"type:text"`        // JSON-serialized request (game state snapshot).
	Response     string    `gorm:"type:text"`        // JSON-serialized decision.
	Reasoning    string    `gorm:"type:text"`        // Agent's explanation.
	Confidence   float64
	LatencyMs    int64
	Outcome      string `gorm:"size:10"` // profit, loss, neutral (filled in later).
	OutcomeValue int64
	CreatedAt    time.Time `gorm:"index:idx_decision_company_time;not null"`
}

// AllModels returns all GORM models for auto-migration.
func AllModels() []any {
	return []any{
		&CompanyRecord{},
		&TradeLog{},
		&PnLSnapshot{},
		&InventorySnapshot{},
		&StrategyMetric{},
		&CompanyLog{},
		&PriceObservation{},
		&AgentDecisionLog{},
	}
}
