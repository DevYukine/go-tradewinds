package strategy

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// BulkHauler focuses on high-volume luxury goods using large ships (galleons).
// Same decision flow as Arbitrage but the agent is expected to favor high-value
// goods and larger ship purchases.
type BulkHauler struct {
	baseStrategy
	lastFleetEval time.Time
}

// NewBulkHauler creates a new BulkHauler strategy instance.
func NewBulkHauler(ctx bot.StrategyContext) (bot.Strategy, error) {
	b := &BulkHauler{}
	b.name = "bulk_hauler"
	if err := b.Init(ctx); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *BulkHauler) Name() string { return "bulk_hauler" }

func (b *BulkHauler) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	b.logger.Info("bulk_hauler: ship arrived, evaluating trade",
		zap.String("ship", ship.Ship.Name),
		zap.String("port", port.Name),
	)

	req := b.buildTradeRequestWithPassengers(ctx, ship, port)

	start := time.Now()
	decision, err := b.ctx.Agent.DecideTradeAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		b.logger.Error("agent trade decision failed", zap.Error(err))
		return err
	}

	b.logger.Agent("trade decision",
		zap.String("action", decision.Action),
		zap.String("reasoning", decision.Reasoning),
		zap.Float64("confidence", decision.Confidence),
	)
	b.logAgentDecision("trade", req, decision, decision.Reasoning, decision.Confidence, latency)

	switch decision.Action {
	case "sell_and_buy", "buy_and_sail":
		if err := b.executeSells(ctx, ship, decision.SellOrders); err != nil {
			b.logger.Error("sell execution failed", zap.Error(err))
		}
		if err := b.executeBuys(ctx, ship, decision.BuyOrders); err != nil {
			b.logger.Error("buy execution failed", zap.Error(err))
		}
		b.boardPassengers(ctx, ship, decision.BoardPassengers)
		if decision.SailTo != nil {
			if err := b.sendShipToPort(ctx, ship, *decision.SailTo); err != nil {
				b.logger.Error("transit failed", zap.Error(err))
			}
		}
	case "wait", "dock":
		b.logger.Info("agent decided to wait",
			zap.String("reasoning", decision.Reasoning),
		)
	}

	return nil
}

func (b *BulkHauler) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	if time.Since(b.lastFleetEval) < fleetEvalInterval {
		return nil
	}
	b.lastFleetEval = time.Now()

	req := b.buildFleetRequest()

	start := time.Now()
	decision, err := b.ctx.Agent.DecideFleetAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		b.logger.Error("agent fleet decision failed", zap.Error(err))
		return nil
	}

	if len(decision.BuyShips) > 0 || len(decision.BuyWarehouses) > 0 || len(decision.SellShips) > 0 {
		b.logger.Agent("fleet decision",
			zap.String("reasoning", decision.Reasoning),
		)
		b.logAgentDecision("fleet", req, decision, decision.Reasoning, 0, latency)
		b.executeFleetDecision(ctx, decision)
	}

	return nil
}
