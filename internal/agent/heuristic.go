package agent

import (
	"context"

	"go.uber.org/zap"
)

// HeuristicAgent uses hand-coded rules for trading decisions.
// The actual heuristics are implemented per-strategy in the strategy package;
// this agent serves as the default decision-maker that strategies delegate to.
type HeuristicAgent struct {
	logger *zap.Logger
}

// NewHeuristicAgent creates a new heuristic-based agent.
func NewHeuristicAgent(logger *zap.Logger) *HeuristicAgent {
	return &HeuristicAgent{
		logger: logger.Named("heuristic_agent"),
	}
}

func (a *HeuristicAgent) Name() string { return "heuristic" }

func (a *HeuristicAgent) DecideTradeAction(_ context.Context, _ TradeDecisionRequest) (*TradeDecision, error) {
	return &TradeDecision{
		Action:     "wait",
		Reasoning:  "no trade heuristic implemented yet",
		Confidence: 0,
	}, nil
}

func (a *HeuristicAgent) DecideFleetAction(_ context.Context, _ FleetDecisionRequest) (*FleetDecision, error) {
	return &FleetDecision{
		Reasoning: "no fleet heuristic implemented yet",
	}, nil
}

func (a *HeuristicAgent) DecideMarketAction(_ context.Context, _ MarketDecisionRequest) (*MarketDecision, error) {
	return &MarketDecision{
		Reasoning: "no market heuristic implemented yet",
	}, nil
}

func (a *HeuristicAgent) EvaluateStrategy(_ context.Context, _ StrategyEvalRequest) (*StrategyEvaluation, error) {
	return &StrategyEvaluation{
		Reasoning: "no strategy evaluation heuristic implemented yet",
	}, nil
}
