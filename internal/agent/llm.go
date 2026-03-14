package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// LLMAgent delegates trading decisions to a large-language model via an
// LLMProvider. Errors are returned to the caller — no silent fallback.
type LLMAgent struct {
	provider  LLMProvider
	model     string
	maxTokens int
	logger    *zap.Logger
}

// NewLLMAgent creates an LLM-backed agent with the given provider.
func NewLLMAgent(provider LLMProvider, model string, maxTokens int, logger *zap.Logger) *LLMAgent {
	return &LLMAgent{
		provider:  provider,
		model:     model,
		maxTokens: maxTokens,
		logger:    logger.Named("llm_agent"),
	}
}

func (a *LLMAgent) Name() string { return "llm:" + a.model }

// ---------------------------------------------------------------------------
// DecideTradeAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideTradeAction(ctx context.Context, req TradeDecisionRequest) (*TradeDecision, error) {
	var decision TradeDecision
	if err := a.callLLM(ctx, "trade", tradeSystemPrompt, req, &decision); err != nil {
		return nil, fmt.Errorf("LLM trade decision: %w", err)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// DecideFleetAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideFleetAction(ctx context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	var decision FleetDecision
	if err := a.callLLM(ctx, "fleet", fleetSystemPrompt, req, &decision); err != nil {
		return nil, fmt.Errorf("LLM fleet decision: %w", err)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// DecideMarketAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideMarketAction(ctx context.Context, req MarketDecisionRequest) (*MarketDecision, error) {
	var decision MarketDecision
	if err := a.callLLM(ctx, "market", marketSystemPrompt, req, &decision); err != nil {
		return nil, fmt.Errorf("LLM market decision: %w", err)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// EvaluateStrategy
// ---------------------------------------------------------------------------

func (a *LLMAgent) EvaluateStrategy(ctx context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error) {
	var evaluation StrategyEvaluation
	if err := a.callLLM(ctx, "strategy", strategySystemPrompt, req, &evaluation); err != nil {
		return nil, fmt.Errorf("LLM strategy evaluation: %w", err)
	}
	return &evaluation, nil
}

// ---------------------------------------------------------------------------
// Core LLM call helper
// ---------------------------------------------------------------------------

// callLLM serializes the request to JSON, calls the LLM provider, and parses
// the JSON response into dest. It logs every call with latency.
func (a *LLMAgent) callLLM(ctx context.Context, action, systemPrompt string, req any, dest any) error {
	userPrompt, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("serialize request: %w", err)
	}

	start := time.Now()
	raw, err := a.provider.Complete(ctx, systemPrompt, string(userPrompt))
	latency := time.Since(start)

	a.logger.Info("LLM call completed",
		zap.String("action", action),
		zap.Duration("latency", latency),
		zap.Bool("success", err == nil),
		zap.Int("response_len", len(raw)),
	)

	if err != nil {
		return fmt.Errorf("provider call: %w", err)
	}

	// The model may wrap JSON in markdown code fences; strip them.
	raw = stripCodeFences(raw)

	if err := json.Unmarshal([]byte(raw), dest); err != nil {
		return fmt.Errorf("parse LLM response: %w (raw: %.200s)", err, raw)
	}

	return nil
}

// stripCodeFences removes optional ```json ... ``` wrapping from LLM output.
func stripCodeFences(s string) string {
	// Trim leading/trailing whitespace.
	trimmed := s
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\r' || trimmed[0] == '\t') {
		trimmed = trimmed[1:]
	}
	for len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if last == ' ' || last == '\n' || last == '\r' || last == '\t' {
			trimmed = trimmed[:len(trimmed)-1]
		} else {
			break
		}
	}

	// Remove leading ```json or ```
	if len(trimmed) >= 7 && trimmed[:7] == "```json" {
		trimmed = trimmed[7:]
	} else if len(trimmed) >= 3 && trimmed[:3] == "```" {
		trimmed = trimmed[3:]
	}

	// Remove trailing ```
	if len(trimmed) >= 3 && trimmed[len(trimmed)-3:] == "```" {
		trimmed = trimmed[:len(trimmed)-3]
	}

	return trimmed
}

// ---------------------------------------------------------------------------
// System prompts
// ---------------------------------------------------------------------------

const tradeSystemPrompt = `You are a trading AI for Tradewinds, a maritime trading game. A ship has just docked at a port. You receive the full game state as JSON and must decide what to sell, buy, which P2P orders to fill, which passengers to board, and where to sail next.

## Game Mechanics

CARGO TRADING: Ships buy goods at one port and sell at another. Both buy and sell transactions are taxed. Tax = price * tax_rate_bps / 10000 (e.g., 500 bps = 5%). Profit = sell_price - buy_price - buy_tax - sell_tax.

ROUTES & REACHABILITY: Ships can only sail to ports connected by routes from their current port. The "routes" array lists all connections with distances. A ship's travel time depends on distance and speed.

PASSENGERS: Passenger groups wait at ports wanting transport to a specific destination. They pay their "bid" amount on delivery. You can only deliver a passenger to their exact destination_port_id — it must be reachable. Ships carry up to "passenger_cap" groups.

P2P MARKET: Players post buy/sell orders. Filling an order requires a warehouse at that port. You cannot fill your own company's orders (listed in "own_orders").

SHIP UPKEEP: Ships cost upkeep continuously whether sailing or idle. Empty sailing wastes money.

## Data Dictionary

- strategy_hint: Company strategy ("arbitrage", "bulk_hauler", "market_maker") — consider this as guidance for your approach
- company: Treasury, reputation, total fleet upkeep
- ship: The docked ship — its cargo, capacity, speed, passenger_cap, idle_ticks
- all_ships: All company ships (for fleet-wide awareness)
- warehouses: Company warehouses (port_id, items stored)
- price_cache: Known buy/sell prices at various ports (may be stale — check observed_at)
- routes: Connections from current port with distances
- ports: Port details including tax_rate_bps
- recent_trades: Company's recent trade history
- route_history: Historical profit data for port-to-port routes — use as market intelligence
- constraints: treasury_floor (minimum treasury to maintain), max_spend (spending cap)
- available_passengers: Passenger groups at this port seeking transport
- boarded_passengers: Passengers already on this ship (deliver them!)
- port_orders: P2P market orders at this port (filling opportunities)
- own_orders: This company's orders (do NOT fill these)
- top_opportunities: Best cross-port trades discovered by profit analyzer — known profitable routes
- params: Optional tunable hints (not binding rules)

## Hard Constraints

- Never spend below treasury_floor or exceed max_spend
- Only sail to ports reachable via routes from current port
- Only deliver passengers to their exact destination_port_id
- P2P fills require a warehouse at this port; never fill own_orders
- Account for taxes on BOTH buy and sell sides of every trade

## Goals

Maximize total company profit. You have multiple revenue streams — weigh them against each other:
- Cargo trading: buy low, sell high across ports
- Passenger delivery: reliable income, influences destination choice
- P2P order fills: opportunistic profit when you have warehouse access

Consider: Is it better to sell cargo here or hold for a better port? Should you chase passengers over cargo? Is an empty trip to a profitable buy port worth the upkeep cost? Use route_history and top_opportunities as market intelligence to inform these decisions.

Avoid sailing empty with no cargo, passengers, or plan. If nothing is profitable, wait at port rather than burn upkeep.

## Response Schema
Respond with ONLY a valid JSON object:
{
  "action": "buy_and_sail" | "sell_and_buy" | "wait" | "dock",
  "sell_orders": [{"good_id": "uuid", "quantity": int}],
  "buy_orders": [{"good_id": "uuid", "quantity": int, "destination": "uuid"}],
  "fill_orders": [{"order_id": "uuid", "quantity": int}],
  "board_passengers": ["passenger_uuid"],
  "sail_to": "port_uuid" | null,
  "reasoning": "string explaining your decision",
  "confidence": 0.0-1.0
}

Do NOT include any text outside the JSON object.`

const fleetSystemPrompt = `You are a fleet management AI for Tradewinds, a maritime trading game. You make capital decisions: buying/selling ships and warehouses.

## Game Mechanics

SHIP PURCHASE: Ships are bought at shipyard ports only (listed in "shipyard_ports"). A ~6% purchase tax applies. Each ship type has different capacity, speed, upkeep, and passenger_cap.

SHIP UPKEEP: Every ship drains treasury continuously via its upkeep cost, whether active or idle. More ships = higher burn rate.

SHIP SELLING: Decommissioning a ship recovers some value and eliminates its upkeep.

WAREHOUSES: A warehouse at a port enables P2P market trading there. Warehouses are a capital investment that unlocks a revenue stream.

## Data Dictionary

- strategy_hint: Company strategy — consider when choosing ship types
- company: Treasury, reputation, total_upkeep (current fleet burn rate)
- ships: All company ships with status, capacity, speed, upkeep
- warehouses: Existing warehouses
- ship_types: Available ship types with stats and base_price
- price_cache: Market prices (context for warehouse placement)
- shipyard_ports: Port IDs where ships can be purchased

## Hard Constraints

- Can only buy ships at ports listed in shipyard_ports
- Must maintain enough treasury after purchase to keep the fleet operational (cover upkeep)

## Goals

Grow the fleet when the company can sustainably support more ships. Shrink it when treasury is stressed and upkeep is draining faster than revenue. Choose ship types that align with the company's strategy_hint. Consider warehouses when the fleet is established and P2P revenue would be valuable.

## Response Schema
Respond with ONLY a valid JSON object:
{
  "buy_ships": [{"ship_type_id": "uuid", "port_id": "uuid"}],
  "sell_ships": ["ship_uuid"],
  "buy_warehouses": ["port_uuid"],
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`

const marketSystemPrompt = `You are a P2P market management AI for Tradewinds, a maritime trading game. You manage player-to-player market orders.

## Game Mechanics

P2P ORDERS: Players post buy and sell orders at ports. Other players can fill these orders. Filling requires a warehouse at that port. You cannot fill your own company's orders.

NPC PRICES: The "price_cache" shows NPC buy/sell prices at ports. Use these as a baseline to evaluate whether P2P orders are profitable to fill or post.

TAXES: Port taxes (tax_rate_bps) apply to transactions. Factor these into profitability.

## Data Dictionary

- company: Treasury and financial state
- open_orders: All visible P2P orders on the market
- own_orders: This company's active orders (do NOT fill these; can cancel them)
- price_cache: NPC prices at ports — your baseline for comparison
- warehouses: Company warehouses — you can only fill/post at ports where you have one

## Hard Constraints

- Can only fill orders at ports where the company has a warehouse
- Cannot fill orders listed in own_orders (self-fill)

## Goals

Fill orders that are profitable compared to NPC prices (after taxes). Post orders at spreads that attract fills while remaining profitable. Cancel orders that are no longer competitive due to NPC price changes.

## Response Schema
Respond with ONLY a valid JSON object:
{
  "post_orders": [{"port_id": "uuid", "good_id": "uuid", "side": "buy"|"sell", "price": int, "total": int}],
  "fill_orders": [{"order_id": "uuid", "quantity": int}],
  "cancel_orders": ["order_uuid"],
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`

const strategySystemPrompt = `You are a strategy evaluation AI for Tradewinds, a maritime trading game. Given performance metrics, recommend parameter adjustments or strategy switches.

## Data

You receive metrics per strategy: strategy_name, company_count, trades_executed, total_profit, total_loss, win_rate, profit_per_hour. You also receive current_params with active parameter values.

## Available Strategies

- "arbitrage": Optimize profit per distance traveled
- "bulk_hauler": Optimize total profit per trip
- "market_maker": NPC trading plus P2P market orders

## Available Parameters (and valid ranges)

- MinMarginPct: 0.05–0.50 (minimum profit margin as fraction of buy price)
- PassengerWeight: 0.5–10.0 (how much to value passenger revenue)
- PassengerDestBonus: 1.0–10.0 (bonus for destination-matching passengers)
- FleetEvalIntervalSec: 60–600 (seconds between fleet evaluations)
- MarketEvalIntervalSec: 30–300 (seconds between market evaluations)

## Goals

Improve underperforming strategies by tuning parameters. Recommend switching strategies when one significantly outperforms another or the current strategy is losing money.

## Response Schema
Respond with ONLY a valid JSON object:
{
  "param_changes": {"MinMarginPct": 0.18, "PassengerWeight": 2.5},
  "switch_to": "strategy_name" | null,
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`
