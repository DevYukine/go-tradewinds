package strategy

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

const (
	feederOrderInterval = 15 * time.Second
	feederFleetInterval = 3 * time.Minute
)

// Feeder drains treasury as fast as possible by buying and selling goods at the
// same port (net loss from spread + tax). When the harvester signals stocked,
// feeders post inflated P2P buy orders that the harvester fills for profit.
// Feeders go bankrupt → bailed out → repeat.
type Feeder struct {
	baseStrategy
	lastOrderPost time.Time
	lastFleetEval time.Time
}

// NewFeeder creates a new Feeder strategy instance.
func NewFeeder(ctx bot.StrategyContext) (bot.Strategy, error) {
	f := &Feeder{
		lastFleetEval: time.Now(),
	}
	f.name = "feeder"
	if err := f.Init(ctx); err != nil {
		return nil, err
	}

	initScheme(ctx.World)
	return f, nil
}

func (f *Feeder) Name() string { return "feeder" }

// OnShipArrival buys cheap goods and sells them back at the same port to drain
// treasury through spread + tax losses.
func (f *Feeder) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	target := schemeTargetPort()
	if target == uuid.Nil {
		return nil
	}

	// If not at target port, sail there.
	if port.ID != target {
		f.logger.Debug("feeder: ship not at target, redirecting",
			zap.String("ship", ship.Ship.Name),
			zap.String("target", target.String()),
		)
		return f.sendShipToPort(ctx, ship, target)
	}

	// At target port: sell any existing cargo first.
	sells := f.buildSellAllCargo(ship)
	if err := f.executeSells(ctx, ship, sells); err != nil {
		if api.IsBankrupt(err) {
			return err
		}
		f.logger.Warn("feeder sell failed", zap.Error(err))
	}

	// Refresh prices and buy cheapest goods to fill ship.
	targetPort := f.ctx.World.GetPort(target)
	if targetPort != nil {
		f.ensurePortPrices(ctx, targetPort)
	}

	buys := f.buildBuyOrders(target, ship)
	if err := f.executeBuys(ctx, ship, buys, nil); err != nil {
		if api.IsBankrupt(err) {
			return err
		}
		f.logger.Warn("feeder buy failed", zap.Error(err))
	}

	// Check if treasury is depleted enough to trigger rotation.
	f.ctx.State.RLock()
	treasury := f.ctx.State.Treasury
	upkeep := f.ctx.State.TotalUpkeep
	f.ctx.State.RUnlock()

	if upkeep > 0 && treasury < upkeep {
		newTarget := schemeAdvancePort()
		f.logger.Info("feeder: treasury depleted, rotating port",
			zap.Int64("treasury", treasury),
			zap.Int64("upkeep", upkeep),
			zap.String("new_target", newTarget.String()),
		)
	}

	return nil
}

// OnTick posts inflated P2P buy orders when the harvester is stocked, and
// manages fleet purchases.
func (f *Feeder) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	target := schemeTargetPort()
	if target == uuid.Nil {
		return nil
	}

	// Cancel stale orders at non-target ports.
	f.cancelStaleOrders(ctx, target)

	// Post P2P buy orders only when harvester is stocked.
	if schemeIsStocked() && time.Since(f.lastOrderPost) >= feederOrderInterval {
		f.lastOrderPost = time.Now()
		f.postInflatedOrders(ctx, target)
	}

	// Fleet evaluation — buy ships to drain money faster.
	f.ctx.State.RLock()
	shipCount := len(f.ctx.State.Ships)
	f.ctx.State.RUnlock()

	if (shipCount == 0 || time.Since(f.lastFleetEval) >= feederFleetInterval) && f.shipBuyBackoff.ready() {
		f.lastFleetEval = time.Now()
		f.evalFleet(ctx)
	}

	return nil
}

// buildSellAllCargo creates sell orders for all cargo on the ship.
func (f *Feeder) buildSellAllCargo(ship *bot.ShipState) []agent.SellOrder {
	var sells []agent.SellOrder
	for _, c := range ship.Cargo {
		if c.Quantity > 0 {
			sells = append(sells, agent.SellOrder{
				GoodID:   c.GoodID,
				Quantity: c.Quantity,
			})
		}
	}
	return sells
}

// buildBuyOrders finds the cheapest goods at the port and builds buy orders
// to fill the ship's capacity.
func (f *Feeder) buildBuyOrders(portID uuid.UUID, ship *bot.ShipState) []agent.BuyOrder {
	goods := f.ctx.World.Goods
	shipType := f.ctx.World.GetShipType(ship.Ship.ShipTypeID)
	if shipType == nil {
		return nil
	}

	capacity := shipType.Capacity - ship.UsedCapacity()
	if capacity <= 0 {
		return nil
	}

	// Find goods with buy prices and sort by cheapest.
	type goodPrice struct {
		goodID uuid.UUID
		price  int
	}
	var available []goodPrice
	for _, g := range goods {
		pp, ok := f.ctx.PriceCache.Get(portID, g.ID)
		if ok && pp.BuyPrice > 0 {
			available = append(available, goodPrice{goodID: g.ID, price: pp.BuyPrice})
		}
	}
	if len(available) == 0 {
		return nil
	}

	// Sort cheapest first to maximize quantity.
	for i := 0; i < len(available); i++ {
		for j := i + 1; j < len(available); j++ {
			if available[j].price < available[i].price {
				available[i], available[j] = available[j], available[i]
			}
		}
	}

	// Distribute capacity across cheapest goods.
	var buys []agent.BuyOrder
	remaining := capacity
	perGood := remaining / len(available)
	if perGood < 1 {
		perGood = 1
	}

	for _, gp := range available {
		qty := perGood
		if qty > remaining {
			qty = remaining
		}
		if qty <= 0 {
			break
		}
		buys = append(buys, agent.BuyOrder{
			GoodID:   gp.goodID,
			Quantity: qty,
		})
		remaining -= qty
	}

	return buys
}

// postInflatedOrders posts P2P buy orders at 1.75x NPC sell price for the
// harvester to fill.
func (f *Feeder) postInflatedOrders(ctx context.Context, targetPort uuid.UUID) {
	goods := f.ctx.World.Goods
	if len(goods) == 0 {
		return
	}

	ownCompanyID := f.ctx.State.CompanyID
	f.ctx.State.RLock()
	treasury := f.ctx.State.Treasury
	existingOrders := make(map[uuid.UUID]bool)
	for _, o := range f.ctx.State.Orders {
		if o.CompanyID == ownCompanyID && o.PortID == targetPort && o.Side == "buy" {
			existingOrders[o.GoodID] = true
		}
	}
	f.ctx.State.RUnlock()

	if treasury <= 0 {
		return
	}

	budget := treasury / int64(len(goods))
	if budget <= 0 {
		return
	}

	for _, g := range goods {
		// Skip goods we already have an order for.
		if existingOrders[g.ID] {
			continue
		}

		// Get NPC sell price (what NPCs sell to us = buy price).
		pp, ok := f.ctx.PriceCache.Get(targetPort, g.ID)
		if !ok || pp.BuyPrice <= 0 {
			continue
		}

		npcBuyPrice := pp.BuyPrice

		// Post at 1.75x NPC price — makes it profitable only for our harvester.
		inflatedPrice := int(float64(npcBuyPrice) * 1.75)
		if inflatedPrice <= 0 {
			continue
		}

		total := int(budget) / inflatedPrice
		if total <= 0 {
			continue
		}

		order, err := f.ctx.Client.PostOrder(ctx, api.CreateOrderRequest{
			PortID: targetPort,
			GoodID: g.ID,
			Side:   "buy",
			Price:  inflatedPrice,
			Total:  total,
		})
		if err != nil {
			f.logger.Debug("feeder: failed to post P2P order",
				zap.String("good_id", g.ID.String()),
				zap.Error(err),
			)
			continue
		}

		goodName := g.Name
		portName := targetPort.String()
		if p := f.ctx.World.GetPort(targetPort); p != nil {
			portName = p.Name
		}

		f.logger.Trade("feeder: posted inflated buy order",
			zap.String("order_id", order.ID.String()),
			zap.String("good", goodName),
			zap.String("port", portName),
			zap.Int("price", inflatedPrice),
			zap.Int("total", total),
			zap.Int("npc_price", npcBuyPrice),
		)

		f.ctx.DB.Create(&db.P2POrderLog{
			CompanyID:  f.ctx.State.CompanyDBID(),
			OrderID:    order.ID.String(),
			OrderType:  "post",
			GoodID:     g.ID.String(),
			GoodName:   goodName,
			PortID:     targetPort.String(),
			PortName:   portName,
			Quantity:   total,
			Price:      inflatedPrice,
			TotalValue: inflatedPrice * total,
			Strategy:   f.name,
			AgentName:  f.ctx.Agent.Name(),
		})
	}
}

// cancelStaleOrders cancels any P2P orders at non-target ports.
func (f *Feeder) cancelStaleOrders(ctx context.Context, targetPort uuid.UUID) {
	companyID := f.ctx.State.CompanyID
	f.ctx.State.RLock()
	var toCancel []uuid.UUID
	for _, o := range f.ctx.State.Orders {
		if o.CompanyID == companyID && o.PortID != targetPort {
			toCancel = append(toCancel, o.ID)
		}
	}
	f.ctx.State.RUnlock()

	for _, orderID := range toCancel {
		if err := f.ctx.Client.CancelOrder(ctx, orderID); err != nil {
			f.logger.Debug("feeder: failed to cancel stale order",
				zap.String("order_id", orderID.String()),
				zap.Error(err),
			)
			continue
		}
		f.logger.Info("feeder: cancelled stale order at old port",
			zap.String("order_id", orderID.String()),
		)

		f.ctx.DB.Create(&db.P2POrderLog{
			CompanyID: f.ctx.State.CompanyDBID(),
			OrderID:   orderID.String(),
			OrderType: "cancel",
			Strategy:  f.name,
			AgentName: f.ctx.Agent.Name(),
		})
	}
}

// evalFleet buys cheap ships to speed up treasury drain.
func (f *Feeder) evalFleet(ctx context.Context) {
	f.ctx.State.RLock()
	shipCount := len(f.ctx.State.Ships)
	f.ctx.State.RUnlock()

	// Feeders want just 2-3 ships — enough to cycle buy/sell.
	if shipCount >= 3 {
		return
	}

	// Find the cheapest ship type.
	var cheapestType uuid.UUID
	cheapestPrice := int(^uint(0) >> 1) // max int
	for _, st := range f.ctx.World.ShipTypes {
		if st.BasePrice < cheapestPrice {
			cheapestPrice = st.BasePrice
			cheapestType = st.ID
		}
	}

	if cheapestType == uuid.Nil {
		return
	}

	// Try to buy at any shipyard port.
	portID := uuid.Nil
	if len(f.ctx.World.ShipyardPorts) > 0 {
		portID = f.ctx.World.ShipyardPorts[0]
	}
	if portID == uuid.Nil {
		return
	}

	ship := f.tryBuyShip(ctx, agent.ShipPurchase{
		ShipTypeID: cheapestType,
		PortID:     portID,
	})
	if ship != nil {
		f.shipBuyBackoff.succeed()
		f.logger.Info("feeder: purchased ship",
			zap.String("ship_id", ship.ID.String()),
			zap.String("name", ship.Name),
		)
	} else {
		f.shipBuyBackoff.fail()
	}
}
