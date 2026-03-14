package strategy

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
)

const (
	// fleetEvalInterval is how often strategies evaluate fleet decisions.
	fleetEvalInterval = 3 * time.Minute
)

// Arbitrage implements a buy-low-sell-high strategy across ports.
// On ship arrival: sell cargo at a profit, buy cheap goods, sail to the
// best destination. Delegates all decisions to the Agent interface.
type Arbitrage struct {
	baseStrategy
	lastFleetEval time.Time
}

// NewArbitrage creates a new Arbitrage strategy instance.
func NewArbitrage(ctx bot.StrategyContext) (bot.Strategy, error) {
	a := &Arbitrage{}
	a.name = "arbitrage"
	if err := a.Init(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *Arbitrage) Name() string { return "arbitrage" }

// OnShipArrival is called when a ship docks. It gathers state, asks the agent
// for a trade decision, and executes the result.
func (a *Arbitrage) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	a.logger.Info("arbitrage: ship arrived, evaluating trade",
		zap.String("ship", ship.Ship.Name),
		zap.String("port", port.Name),
	)

	// Build decision request with full game state including passengers.
	req := a.buildTradeRequestWithPassengers(ctx, ship, port)

	// Ask the agent.
	start := time.Now()
	decision, err := a.ctx.Agent.DecideTradeAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		a.logger.Error("agent trade decision failed", zap.Error(err))
		return err
	}

	a.logger.Agent("trade decision",
		zap.String("action", decision.Action),
		zap.String("reasoning", decision.Reasoning),
		zap.Float64("confidence", decision.Confidence),
		zap.Duration("latency", latency),
	)

	// Log to DB for analysis.
	a.logAgentDecision("trade", req, decision, decision.Reasoning, decision.Confidence, latency)

	// Execute the decision.
	switch decision.Action {
	case "sell_and_buy", "buy_and_sail":
		// Sell first.
		if err := a.executeSells(ctx, ship, decision.SellOrders); err != nil {
			a.logger.Error("sell execution failed", zap.Error(err))
		}
		// Fill P2P orders.
		a.executeFills(ctx, decision.FillOrders)
		// Then buy.
		if err := a.executeBuys(ctx, ship, decision.BuyOrders); err != nil {
			a.logger.Error("buy execution failed", zap.Error(err))
		}
		// Board passengers.
		a.boardPassengers(ctx, ship, decision.BoardPassengers)
		// Then sail.
		if decision.SailTo != nil {
			if err := a.sendShipToPort(ctx, ship, *decision.SailTo); err != nil {
				a.logger.Error("transit failed", zap.Error(err))
			}
		}

	case "wait", "dock":
		a.logger.Info("agent decided to wait",
			zap.String("reasoning", decision.Reasoning),
		)

	default:
		a.logger.Warn("unknown trade action from agent",
			zap.String("action", decision.Action),
		)
	}

	return nil
}

// OnTick handles periodic fleet management decisions.
func (a *Arbitrage) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	fleetInterval := fleetEvalInterval
	a.ctx.State.RLock()
	if a.ctx.State.Params != nil && a.ctx.State.Params.FleetEvalIntervalSec > 0 {
		fleetInterval = time.Duration(a.ctx.State.Params.FleetEvalIntervalSec) * time.Second
	}
	a.ctx.State.RUnlock()

	if time.Since(a.lastFleetEval) < fleetInterval {
		return nil
	}
	a.lastFleetEval = time.Now()

	req := a.buildFleetRequest()

	start := time.Now()
	decision, err := a.ctx.Agent.DecideFleetAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		a.logger.Error("agent fleet decision failed", zap.Error(err))
		return nil
	}

	if len(decision.BuyShips) > 0 || len(decision.BuyWarehouses) > 0 || len(decision.SellShips) > 0 {
		a.logger.Agent("fleet decision",
			zap.String("reasoning", decision.Reasoning),
			zap.Int("ships_to_buy", len(decision.BuyShips)),
			zap.Int("ships_to_sell", len(decision.SellShips)),
			zap.Int("warehouses_to_buy", len(decision.BuyWarehouses)),
		)
		a.logAgentDecision("fleet", req, decision, decision.Reasoning, 0, latency)
		a.executeFleetDecision(ctx, decision)
	}

	return nil
}
