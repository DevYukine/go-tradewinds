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
	a.logger.Debug("arbitrage: ship arrived, evaluating trade",
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
		a.logger.Warn("agent trade decision failed", zap.Error(err))
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
			a.logger.Warn("sell execution failed", zap.Error(err))
		}
		// Fill P2P orders.
		a.executeFills(ctx, *ship.Ship.PortID, decision.FillOrders)
		// Then buy.
		if err := a.executeBuys(ctx, ship, decision.BuyOrders); err != nil {
			a.logger.Warn("buy execution failed", zap.Error(err))
		}
		// Board passengers.
		a.boardPassengers(ctx, ship, decision.BoardPassengers)
		// Reset idle ticks on active trade.
		a.ctx.State.Lock()
		if ss := a.ctx.State.Ships[ship.Ship.ID]; ss != nil {
			ss.IdleTicks = 0
		}
		a.ctx.State.Unlock()
		// Then sail.
		if decision.SailTo != nil {
			if err := a.sendShipToPort(ctx, ship, *decision.SailTo); err != nil {
				a.logger.Warn("transit failed", zap.Error(err))
			}
		}

	case "wait", "dock":
		a.logger.Debug("agent decided to wait",
			zap.String("reasoning", decision.Reasoning),
		)
		// Execute any sells even when waiting.
		if err := a.executeSells(ctx, ship, decision.SellOrders); err != nil {
			a.logger.Warn("sell execution failed", zap.Error(err))
		}
		a.executeFills(ctx, *ship.Ship.PortID, decision.FillOrders)
		a.boardPassengers(ctx, ship, decision.BoardPassengers)
		// Track idle ticks for the ship.
		a.ctx.State.Lock()
		if ss := a.ctx.State.Ships[ship.Ship.ID]; ss != nil {
			ss.IdleTicks++
		}
		a.ctx.State.Unlock()

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
	if !a.fleetEvalBackoff.ready() {
		return nil
	}
	a.lastFleetEval = time.Now()

	req := a.buildFleetRequest()

	start := time.Now()
	decision, err := a.ctx.Agent.DecideFleetAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		a.fleetEvalBackoff.fail()
		a.logger.Warn("agent fleet decision failed",
			zap.Error(err),
			zap.Duration("backoff", a.fleetEvalBackoff.delay),
		)
		return nil
	}
	a.fleetEvalBackoff.succeed()

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
