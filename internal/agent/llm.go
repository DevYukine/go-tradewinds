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
Given the current game state as JSON, decide what to buy, sell, and where to sail.

Respond with ONLY a valid JSON object matching this schema:
{
  "action": "buy_and_sail" | "sell_and_buy" | "wait" | "dock",
  "sell_orders": [{"good_id": "uuid", "quantity": int}],
  "buy_orders": [{"good_id": "uuid", "quantity": int, "destination": "uuid"}],
  "sail_to": "uuid" | null,
  "reasoning": "string",
  "confidence": 0.0-1.0
}

Maximize profit by exploiting price differences between ports. Consider:
- Current cargo and what can be sold profitably at this port
- Price differences between ports for arbitrage opportunities
- Distance and travel time to destinations
- Available budget (treasury constraints)
- Ship capacity

Do NOT include any text outside the JSON object.`

const fleetSystemPrompt = `You are a trading bot AI managing a fleet in the Tradewinds maritime game.
Given the current company state as JSON, decide whether to buy ships or warehouses.

Respond with ONLY a valid JSON object matching this schema:
{
  "buy_ships": [{"ship_type_id": "uuid", "port_id": "uuid"}],
  "buy_warehouses": ["port_uuid"],
  "reasoning": "string"
}

Consider:
- Current treasury and upkeep costs
- Fleet size and composition
- Available ship types and their cost/benefit
- Whether expanding will increase profits enough to justify upkeep

Do NOT include any text outside the JSON object.`

const marketSystemPrompt = `You are a trading bot AI managing P2P market orders in the Tradewinds game.
Given the current market state as JSON, decide which orders to post, fill, or cancel.

Respond with ONLY a valid JSON object matching this schema:
{
  "post_orders": [{"port_id": "uuid", "good_id": "uuid", "side": "buy"|"sell", "price": int, "total": int}],
  "fill_orders": [{"order_id": "uuid", "quantity": int}],
  "cancel_orders": ["uuid"],
  "reasoning": "string"
}

Consider:
- Current market prices vs NPC prices for profit opportunities
- Warehouse inventory for fulfillment
- Company treasury for purchasing power

Do NOT include any text outside the JSON object.`

const strategySystemPrompt = `You are a trading bot AI evaluating strategy performance in the Tradewinds game.
Given performance metrics as JSON, recommend parameter adjustments or strategy switches.

Respond with ONLY a valid JSON object matching this schema:
{
  "param_changes": {"key": value},
  "switch_to": "strategy_name" | null,
  "reasoning": "string"
}

Consider:
- Win rate and profitability of each strategy
- Whether a strategy switch could improve performance
- Gradual parameter tuning over dramatic changes

Do NOT include any text outside the JSON object.`
