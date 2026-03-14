package agent

import (
	"context"

	"go.uber.org/zap"
)

// CompositeAgent routes decisions between a fast agent (for time-sensitive
// operations like trade decisions) and a slow agent (for strategic decisions
// like fleet management and strategy evaluation). If the slow agent fails,
// the fast agent is used as a fallback.
type CompositeAgent struct {
	fast   Agent
	slow   Agent
	logger *zap.Logger
}

// NewCompositeAgent creates a composite agent that routes between fast and slow agents.
func NewCompositeAgent(fast, slow Agent, logger *zap.Logger) *CompositeAgent {
	return &CompositeAgent{
		fast:   fast,
		slow:   slow,
		logger: logger.Named("composite_agent"),
	}
}

func (a *CompositeAgent) Name() string {
	return "composite:" + a.fast.Name() + "+" + a.slow.Name()
}

// DecideTradeAction is time-sensitive and routes to the fast agent.
func (a *CompositeAgent) DecideTradeAction(ctx context.Context, req TradeDecisionRequest) (*TradeDecision, error) {
	return a.fast.DecideTradeAction(ctx, req)
}

// DecideFleetAction is strategic and routes to the slow agent.
// Falls back to fast on error.
func (a *CompositeAgent) DecideFleetAction(ctx context.Context, req FleetDecisionRequest) (*FleetDecision, error) {
	decision, err := a.slow.DecideFleetAction(ctx, req)
	if err != nil {
		a.logger.Warn("slow agent fleet decision failed, falling back to fast",
			zap.Error(err),
		)
		return a.fast.DecideFleetAction(ctx, req)
	}
	return decision, nil
}

// DecideMarketAction is strategic and routes to the slow agent.
// Falls back to fast on error.
func (a *CompositeAgent) DecideMarketAction(ctx context.Context, req MarketDecisionRequest) (*MarketDecision, error) {
	decision, err := a.slow.DecideMarketAction(ctx, req)
	if err != nil {
		a.logger.Warn("slow agent market decision failed, falling back to fast",
			zap.Error(err),
		)
		return a.fast.DecideMarketAction(ctx, req)
	}
	return decision, nil
}

// EvaluateStrategy is strategic and routes to the slow agent.
// Falls back to fast on error.
func (a *CompositeAgent) EvaluateStrategy(ctx context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error) {
	evaluation, err := a.slow.EvaluateStrategy(ctx, req)
	if err != nil {
		a.logger.Warn("slow agent strategy eval failed, falling back to fast",
			zap.Error(err),
		)
		return a.fast.EvaluateStrategy(ctx, req)
	}
	return evaluation, nil
}
