package strategy

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// PassengerSniper focuses on tax-free passenger revenue with geographic
// coverage. Ships spread across ports to maximize passenger sniping
// opportunities. Cargo trading is opportunistic — only high-margin goods.
type PassengerSniper struct {
	baseStrategy
	lastFleetEval time.Time
}

// NewPassengerSniper creates a new PassengerSniper strategy instance.
func NewPassengerSniper(ctx bot.StrategyContext) (bot.Strategy, error) {
	p := &PassengerSniper{
		lastFleetEval: time.Now(),
	}
	p.name = "passenger_sniper"
	if err := p.Init(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *PassengerSniper) Name() string { return "passenger_sniper" }

func (p *PassengerSniper) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	p.logger.Debug("passenger_sniper: ship arrived, evaluating trade",
		zap.String("ship", ship.Ship.Name),
		zap.String("port", port.Name),
	)

	req := p.buildTradeRequestWithPassengers(ctx, ship, port)

	start := time.Now()
	decision, err := p.ctx.Agent.DecideTradeAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		p.logger.Warn("agent trade decision failed", zap.Error(err))
		return err
	}

	p.logger.Agent("trade decision",
		zap.String("action", decision.Action),
		zap.String("reasoning", decision.Reasoning),
		zap.Float64("confidence", decision.Confidence),
		zap.Duration("latency", latency),
	)
	p.logAgentDecision("trade", req, decision, decision.Reasoning, decision.Confidence, latency)

	switch decision.Action {
	case "sell_and_buy", "buy_and_sail":
		if err := p.executeSells(ctx, ship, decision.SellOrders); err != nil {
			if api.IsBankrupt(err) {
				return err
			}
			p.logger.Warn("sell execution failed", zap.Error(err))
		}
		p.executeFills(ctx, port.ID, decision.FillOrders)
		p.executeWarehouseLoads(ctx, ship, decision.WarehouseLoads)
		if err := p.executeBuys(ctx, ship, decision.BuyOrders, decision.SailTo); err != nil {
			if api.IsBankrupt(err) {
				return err
			}
			p.logger.Warn("buy execution failed", zap.Error(err))
		}
		p.executeWarehouseStores(ctx, ship, decision.WarehouseStores)
		p.boardPassengers(ctx, ship, decision.BoardPassengers)
		p.ctx.State.Lock()
		if ss := p.ctx.State.Ships[ship.Ship.ID]; ss != nil {
			ss.IdleTicks = 0
		}
		p.ctx.State.Unlock()
		if decision.SailTo != nil {
			if err := p.sendShipToPort(ctx, ship, *decision.SailTo); err != nil {
				if api.IsBankrupt(err) {
					return err
				}
				p.logger.Warn("transit failed", zap.Error(err))
			}
		}

	case "wait", "dock":
		p.logger.Debug("agent decided to wait",
			zap.String("reasoning", decision.Reasoning),
		)
		if err := p.executeSells(ctx, ship, decision.SellOrders); err != nil {
			if api.IsBankrupt(err) {
				return err
			}
			p.logger.Warn("sell execution failed", zap.Error(err))
		}
		p.executeFills(ctx, port.ID, decision.FillOrders)
		p.executeWarehouseLoads(ctx, ship, decision.WarehouseLoads)
		p.executeWarehouseStores(ctx, ship, decision.WarehouseStores)
		p.boardPassengers(ctx, ship, decision.BoardPassengers)
		p.ctx.State.Lock()
		if ss := p.ctx.State.Ships[ship.Ship.ID]; ss != nil {
			ss.IdleTicks++
		}
		p.ctx.State.Unlock()

	default:
		p.logger.Warn("unknown trade action from agent",
			zap.String("action", decision.Action),
		)
	}

	return nil
}

func (p *PassengerSniper) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	fleetInterval := fleetEvalInterval
	p.ctx.State.RLock()
	shipCount := len(p.ctx.State.Ships)
	if p.ctx.State.Params != nil && p.ctx.State.Params.FleetEvalIntervalSec > 0 {
		fleetInterval = time.Duration(p.ctx.State.Params.FleetEvalIntervalSec) * time.Second
	}
	p.ctx.State.RUnlock()

	if shipCount > 0 && time.Since(p.lastFleetEval) < fleetInterval {
		return nil
	}
	if !p.fleetEvalBackoff.ready() {
		return nil
	}
	p.lastFleetEval = time.Now()

	req := p.buildFleetRequest()

	start := time.Now()
	decision, err := p.ctx.Agent.DecideFleetAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		p.fleetEvalBackoff.fail()
		p.logger.Warn("agent fleet decision failed",
			zap.Error(err),
			zap.Duration("backoff", p.fleetEvalBackoff.delay),
		)
		return nil
	}
	p.fleetEvalBackoff.succeed()

	if len(decision.BuyShips) > 0 || len(decision.BuyWarehouses) > 0 || len(decision.SellShips) > 0 {
		p.logger.Agent("fleet decision",
			zap.String("reasoning", decision.Reasoning),
			zap.Int("ships_to_buy", len(decision.BuyShips)),
			zap.Int("ships_to_sell", len(decision.SellShips)),
			zap.Int("warehouses_to_buy", len(decision.BuyWarehouses)),
		)
		p.logAgentDecision("fleet", req, decision, decision.Reasoning, 0, latency)
		p.executeFleetDecision(ctx, decision)
	}

	return nil
}
