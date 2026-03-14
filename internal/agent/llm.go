package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// LLMAgent delegates trading decisions to a large-language model via an
// LLMProvider. On any error (timeout, malformed JSON, etc.) it falls back
// to a HeuristicAgent so the bot never stalls.
type LLMAgent struct {
	provider  LLMProvider
	model     string
	maxTokens int
	logger    *zap.Logger
	fallback  *HeuristicAgent
}

// NewLLMAgent creates an LLM-backed agent with the given provider.
func NewLLMAgent(provider LLMProvider, model string, maxTokens int, logger *zap.Logger) *LLMAgent {
	return &LLMAgent{
		provider:  provider,
		model:     model,
		maxTokens: maxTokens,
		logger:    logger.Named("llm_agent"),
		fallback:  NewHeuristicAgent(logger),
	}
}

func (a *LLMAgent) Name() string { return "llm:" + a.model }

// ---------------------------------------------------------------------------
// DecideTradeAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideTradeAction(ctx context.Context, req TradeDecisionRequest) (*TradeDecision, error) {
	var decision TradeDecision
	if err := a.callLLM(ctx, "trade", tradeSystemPrompt, req, &decision); err != nil {
		a.logger.Warn("LLM trade decision failed, falling back to heuristic",
			zap.Error(err),
		)
		return a.fallback.DecideTradeAction(ctx, req)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// DecideFleetAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideFleetAction(ctx context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	var decision FleetDecision
	if err := a.callLLM(ctx, "fleet", fleetSystemPrompt, req, &decision); err != nil {
		a.logger.Warn("LLM fleet decision failed, falling back to heuristic",
			zap.Error(err),
		)
		return a.fallback.DecideFleetAction(ctx, req)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// DecideMarketAction
// ---------------------------------------------------------------------------

func (a *LLMAgent) DecideMarketAction(ctx context.Context, req MarketDecisionRequest) (*MarketDecision, error) {
	var decision MarketDecision
	if err := a.callLLM(ctx, "market", marketSystemPrompt, req, &decision); err != nil {
		a.logger.Warn("LLM market decision failed, falling back to heuristic",
			zap.Error(err),
		)
		return a.fallback.DecideMarketAction(ctx, req)
	}
	return &decision, nil
}

// ---------------------------------------------------------------------------
// EvaluateStrategy
// ---------------------------------------------------------------------------

func (a *LLMAgent) EvaluateStrategy(ctx context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error) {
	var evaluation StrategyEvaluation
	if err := a.callLLM(ctx, "strategy", strategySystemPrompt, req, &evaluation); err != nil {
		a.logger.Warn("LLM strategy evaluation failed, falling back to heuristic",
			zap.Error(err),
		)
		return a.fallback.EvaluateStrategy(ctx, req)
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

const tradeSystemPrompt = `You are a trading bot AI for a maritime trading game called Tradewinds.
A ship has just docked at a port. Given the full game state as JSON, decide what to sell, buy, which P2P orders to fill, which passengers to board, and where to sail next.

## Strategy Behavior
The "strategy_hint" field tells you which strategy this company uses:
- "arbitrage": Maximize profit per distance. Score destinations by (profit / distance). Prefer fast ships and quick turnarounds.
- "bulk_hauler": Maximize absolute profit per trip. Score destinations by total achievable profit regardless of distance. Prefer large-capacity ships.
- "market_maker": Same NPC trading as arbitrage, but also fills P2P market orders when profitable.

## Decision Process (follow this order)

### 1. Smart Selling
Do NOT blindly sell all cargo. For each cargo item, check if a reachable destination offers a significantly better (>20%) net sell price after taxes. If so, HOLD that cargo — do not sell it. Held cargo reduces available capacity for new buys but should be carried to the better port.
Only sell cargo that is best sold HERE or has no better destination.

### 2. Fill P2P Orders
Check "port_orders" for profitable fill opportunities. Ignore orders in "own_orders" (self-fill). Only fill orders where the margin exceeds 7% after taxes. You need a warehouse at this port to fill orders — check "warehouses". Include fills in "fill_orders".

### 3. Buy Cargo
Evaluate ALL reachable destinations, not just one. For each destination, simulate filling the remaining ship capacity with profitable goods sorted by profit per unit. A trade is profitable only if:
  sell_price - buy_price - buy_tax - sell_tax > buy_price * MinMarginPct
MinMarginPct defaults to 0.15 (15%) but check "params" for overrides.
Port taxes are in "ports[].tax_rate_bps" (basis points, e.g., 500 = 5%). Tax = price * bps / 10000.

### 4. Score Destinations
For each destination, compute a composite score:
- Arbitrage: (total_cargo_profit / distance) + (passenger_revenue / distance * PassengerWeight) + (held_cargo_gain / distance) + route_history_bonus
- Bulk hauler: total_cargo_profit + (passenger_revenue / distance * PassengerWeight) + held_cargo_gain + route_history_bonus
PassengerWeight defaults to 2.0 but check "params" for overrides.

### 5. Passenger Override
After choosing a destination, check if passenger revenue alone (weighted by PassengerWeight) exceeds the expected trade PROFIT (not cost) from buy orders. If so, override the destination to the best passenger port. PassengerDestBonus defaults to 3.0 for destination-matching passengers.

### 6. Route History
"route_history" contains recent buy→sell results for routes. Compute average profit per trade for each (from_port → to_port) pair and add as a bonus to destination scoring.

### 7. No Profitable Trade
If no trade meets the minimum margin, do NOT buy speculatively. Instead, sail to the destination with the highest passenger revenue, or the nearest port if no passengers are available.

## Tunable Parameters (from "params" field, use defaults if absent)
- MinMarginPct: 0.15 (minimum profit margin as fraction of buy price)
- PassengerWeight: 2.0 (multiplier for passenger revenue in scoring)
- PassengerDestBonus: 3.0 (bonus for passengers heading to the chosen destination)
- SpeculativeTradeEnabled: false (if true, allow buying without guaranteed profit)

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

const fleetSystemPrompt = `You are a trading bot AI managing a fleet in the Tradewinds maritime game.
Given the current company state as JSON, decide whether to buy ships, sell ships, or buy warehouses.

## Ship Buying Rules
- Only buy if treasury can cover: ship_cost * 1.06 (6% tax) + (current_upkeep + new_ship_upkeep) * 24 hours safety margin
- Strategy preferences:
  - "arbitrage": Prefer fastest ships (high speed)
  - "bulk_hauler": Prefer largest ships (high capacity)
  - "market_maker": Prefer cheapest upkeep ships
- Only buy at ports with shipyards ("shipyard_ports" in request)

## Ship Selling (Decommission) Rules
- If treasury < (total_upkeep * reserve_hours), sell the worst-performing ship to reduce burn
- Reserve hours scale with fleet size: small fleets (1-3) need 30h, medium (4-10) need 24h, large (10+) need 20h
- Sell the ship with the worst value ratio (highest upkeep relative to capacity)
- Include ship IDs to sell in "sell_ships"

## Warehouse Rules
- Only buy warehouses if: treasury > 10x hourly upkeep, fleet has 3+ ships, and company has fewer than 2 warehouses
- Prefer ports where multiple ships frequently dock

## Response Schema
Respond with ONLY a valid JSON object:
{
  "buy_ships": [{"ship_type_id": "uuid", "port_id": "uuid"}],
  "sell_ships": ["ship_uuid"],
  "buy_warehouses": ["port_uuid"],
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`

const marketSystemPrompt = `You are a trading bot AI managing P2P market orders in the Tradewinds game.
Given the current market state as JSON, decide which orders to fill, post, or cancel.

## Filling Orders
- Only fill orders at ports where the company has a warehouse (check "warehouses")
- Filter out orders in "own_orders" to avoid self-filling
- Sell-side fills: only fill if order price > NPC sell price * 1.10 (10% min margin)
- Buy-side fills: only fill if NPC buy price > order price * 1.07 (7% min margin)
- Account for port taxes (tax_rate_bps) in all profit calculations

## Posting New Orders
- Maximum 5 active orders at a time (check "own_orders" count)
- Only post at ports where the company has a warehouse
- Sell orders: price should be 20%+ above NPC buy price at that port
- Buy orders: price should be 20%+ below NPC sell price at that port
- Spread should be attractive enough to get filled but still profitable

## Cancelling Orders
- Cancel orders where NPC prices have moved past your order price (no longer competitive)
- Cancel orders that have been sitting unfilled for too long

## Response Schema
Respond with ONLY a valid JSON object:
{
  "post_orders": [{"port_id": "uuid", "good_id": "uuid", "side": "buy"|"sell", "price": int, "total": int}],
  "fill_orders": [{"order_id": "uuid", "quantity": int}],
  "cancel_orders": ["order_uuid"],
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`

const strategySystemPrompt = `You are a trading bot AI evaluating strategy performance in the Tradewinds game.
Given performance metrics as JSON, recommend parameter adjustments or strategy switches.

## Strategy Switch Rules
- Available strategies: "arbitrage", "bulk_hauler", "market_maker"
- Switch when the best strategy's profit/hour is 1.5x better than the current one
- Also switch if the current strategy has negative profit (losing money)
- Only recommend switching to a strategy that is actually performing well

## Parameter Tuning
Valid tunable parameters and their ranges:
- MinMarginPct: 0.05 - 0.50 (minimum profit margin, default 0.15)
- PassengerWeight: 0.5 - 5.0 (passenger revenue weight in scoring, default 2.0)
- PassengerDestBonus: 1.0 - 10.0 (bonus for destination-matching passengers, default 3.0)
- FleetEvalIntervalSec: 60 - 600 (seconds between fleet evaluations, default 180)
- MarketEvalIntervalSec: 30 - 300 (seconds between market evaluations, default 60)

Adjust parameters gradually (10-20% at a time). Do not make dramatic changes.

## Response Schema
Respond with ONLY a valid JSON object:
{
  "param_changes": {"MinMarginPct": 0.18, "PassengerWeight": 2.5},
  "switch_to": "strategy_name" | null,
  "reasoning": "string"
}

Do NOT include any text outside the JSON object.`
