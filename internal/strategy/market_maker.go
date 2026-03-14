package strategy

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
)

const (
	// marketEvalInterval is how often the market maker evaluates P2P orders.
	marketEvalInterval = 2 * time.Minute
)

// MarketMaker operates on the P2P market by posting buy/sell orders and
// filling other players' orders for profit. Also trades with NPCs when
// ships arrive at ports.
type MarketMaker struct {
	baseStrategy
	lastFleetEval  time.Time
	lastMarketEval time.Time
}

// NewMarketMaker creates a new MarketMaker strategy instance.
func NewMarketMaker(ctx bot.StrategyContext) (bot.Strategy, error) {
	m := &MarketMaker{}
	m.name = "market_maker"
	if err := m.Init(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *MarketMaker) Name() string { return "market_maker" }

func (m *MarketMaker) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	m.logger.Info("market_maker: ship arrived, evaluating trade",
		zap.String("ship", ship.Ship.Name),
		zap.String("port", port.Name),
	)

	req := m.buildTradeRequest(ship, port)

	start := time.Now()
	decision, err := m.ctx.Agent.DecideTradeAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		m.logger.Error("agent trade decision failed", zap.Error(err))
		return err
	}

	m.logger.Agent("trade decision",
		zap.String("action", decision.Action),
		zap.String("reasoning", decision.Reasoning),
	)
	m.logAgentDecision("trade", req, decision, decision.Reasoning, decision.Confidence, latency)

	switch decision.Action {
	case "sell_and_buy", "buy_and_sail":
		if err := m.executeSells(ctx, ship, decision.SellOrders); err != nil {
			m.logger.Error("sell execution failed", zap.Error(err))
		}
		if err := m.executeBuys(ctx, ship, decision.BuyOrders); err != nil {
			m.logger.Error("buy execution failed", zap.Error(err))
		}
		if decision.SailTo != nil {
			if err := m.sendShipToPort(ctx, ship, *decision.SailTo); err != nil {
				m.logger.Error("transit failed", zap.Error(err))
			}
		}
	case "wait", "dock":
		m.logger.Info("agent decided to wait",
			zap.String("reasoning", decision.Reasoning),
		)
	}

	return nil
}

func (m *MarketMaker) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	// Evaluate market opportunities periodically.
	if time.Since(m.lastMarketEval) >= marketEvalInterval {
		m.lastMarketEval = time.Now()
		m.evaluateMarket(ctx)
	}

	// Evaluate fleet decisions less frequently.
	if time.Since(m.lastFleetEval) >= fleetEvalInterval {
		m.lastFleetEval = time.Now()
		m.evaluateFleet(ctx)
	}

	return nil
}

// evaluateMarket asks the agent about P2P market opportunities.
func (m *MarketMaker) evaluateMarket(ctx context.Context) {
	// Fetch open orders.
	openOrders, err := m.ctx.Client.ListOrders(ctx, api.OrderFilters{})
	if err != nil {
		m.logger.Warn("failed to fetch market orders", zap.Error(err))
		return
	}

	// Convert to agent types.
	agentOrders := make([]agent.MarketOrder, len(openOrders))
	for i, o := range openOrders {
		agentOrders[i] = agent.MarketOrder{
			ID:        o.ID,
			PortID:    o.PortID,
			GoodID:    o.GoodID,
			Side:      o.Side,
			Price:     o.Price,
			Remaining: o.Remaining,
		}
	}

	m.ctx.State.RLock()
	warehouses := make([]agent.WarehouseSnapshot, 0, len(m.ctx.State.Warehouses))
	for _, w := range m.ctx.State.Warehouses {
		warehouses = append(warehouses, warehouseToSnapshot(w))
	}
	m.ctx.State.RUnlock()

	req := agent.MarketDecisionRequest{
		Company: agent.CompanySnapshot{
			ID:          m.ctx.State.CompanyID,
			Treasury:    m.ctx.State.Treasury,
			Reputation:  m.ctx.State.Reputation,
			TotalUpkeep: m.ctx.State.TotalUpkeep,
		},
		OpenOrders: agentOrders,
		PriceCache: m.ctx.PriceCache.All(),
		Warehouses: warehouses,
	}

	start := time.Now()
	decision, err := m.ctx.Agent.DecideMarketAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		m.logger.Error("agent market decision failed", zap.Error(err))
		return
	}

	m.logAgentDecision("market", req, decision, decision.Reasoning, 0, latency)

	// Execute market decisions.
	for _, order := range decision.PostOrders {
		posted, err := m.ctx.Client.PostOrder(ctx, api.CreateOrderRequest{
			PortID: order.PortID,
			GoodID: order.GoodID,
			Side:   order.Side,
			Price:  order.Price,
			Total:  order.Total,
		})
		if err != nil {
			m.logger.Error("failed to post market order", zap.Error(err))
			continue
		}
		m.logger.Trade("posted market order",
			zap.String("order_id", posted.ID.String()),
			zap.String("side", order.Side),
			zap.Int("price", order.Price),
			zap.Int("total", order.Total),
		)
	}

	for _, fill := range decision.FillOrders {
		_, err := m.ctx.Client.FillOrder(ctx, fill.OrderID, fill.Quantity)
		if err != nil {
			m.logger.Error("failed to fill market order",
				zap.String("order_id", fill.OrderID.String()),
				zap.Error(err),
			)
			continue
		}
		m.logger.Trade("filled market order",
			zap.String("order_id", fill.OrderID.String()),
			zap.Int("quantity", fill.Quantity),
		)
	}

	for _, orderID := range decision.CancelOrders {
		if err := m.ctx.Client.CancelOrder(ctx, orderID); err != nil {
			m.logger.Error("failed to cancel order",
				zap.String("order_id", orderID.String()),
				zap.Error(err),
			)
			continue
		}
		m.logger.Info("cancelled market order",
			zap.String("order_id", orderID.String()),
		)
	}
}

func (m *MarketMaker) evaluateFleet(ctx context.Context) {
	req := m.buildFleetRequest()

	start := time.Now()
	decision, err := m.ctx.Agent.DecideFleetAction(ctx, req)
	latency := time.Since(start)

	if err != nil {
		m.logger.Error("agent fleet decision failed", zap.Error(err))
		return
	}

	if len(decision.BuyShips) > 0 || len(decision.BuyWarehouses) > 0 {
		m.logger.Agent("fleet decision",
			zap.String("reasoning", decision.Reasoning),
		)
		m.logAgentDecision("fleet", req, decision, decision.Reasoning, 0, latency)
		m.executeFleetDecision(ctx, decision)
	}
}
