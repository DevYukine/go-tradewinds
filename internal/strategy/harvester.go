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
	harvesterOrderScanInterval = 10 * time.Second
	harvesterFleetInterval     = 3 * time.Minute
	harvesterStockThreshold    = 3 // ships' worth of goods to signal stocked
)

// Harvester extracts profit from the feeder/harvester scheme. It pre-stocks
// goods at the target port (via warehouse), then fills feeder P2P buy orders
// at inflated prices. Runs a supply chain loop between non-target and target
// ports to keep inventory flowing.
type Harvester struct {
	baseStrategy
	lastFleetEval    time.Time
	lastOrderScan    time.Time
	warehouseBackoff backoffState
}

// NewHarvester creates a new Harvester strategy instance.
func NewHarvester(ctx bot.StrategyContext) (bot.Strategy, error) {
	h := &Harvester{
		lastFleetEval: time.Now(),
	}
	h.name = "harvester"
	if err := h.Init(ctx); err != nil {
		return nil, err
	}

	initScheme(ctx.World)
	return h, nil
}

func (h *Harvester) Name() string { return "harvester" }

// OnShipArrival handles the harvester's supply chain loop:
// - At non-target ports: sell cargo for profit, buy cheap goods, sail to target
// - At target port: store cargo in warehouse (don't sell), buy more, then sail
//   out to resupply
func (h *Harvester) OnShipArrival(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	target := schemeTargetPort()
	if target == uuid.Nil {
		return nil
	}

	if port.ID != target {
		return h.handleNonTargetPort(ctx, ship, port, target)
	}
	return h.handleTargetPort(ctx, ship, port)
}

// handleNonTargetPort sells cargo for profit, buys cheap goods, then sails to
// the target port with a full hold.
func (h *Harvester) handleNonTargetPort(ctx context.Context, ship *bot.ShipState, port *api.Port, target uuid.UUID) error {
	// Sell any cargo at NPC prices — profit from wherever.
	sells := h.buildSellAllCargo(ship)
	if err := h.executeSells(ctx, ship, sells); err != nil {
		if api.IsBankrupt(err) {
			return err
		}
		h.logger.Warn("harvester sell failed", zap.Error(err))
	}

	// Buy cheapest goods to bring to target port.
	h.ensurePortPrices(ctx, port)
	buys := h.buildBuyOrders(port.ID, ship)
	if err := h.executeBuys(ctx, ship, buys, &target); err != nil {
		if api.IsBankrupt(err) {
			return err
		}
		h.logger.Warn("harvester buy failed", zap.Error(err))
	}

	// Sail to target port with goods.
	h.logger.Debug("harvester: sailing to target port with stock",
		zap.String("ship", ship.Ship.Name),
		zap.String("target", target.String()),
	)
	return h.sendShipToPort(ctx, ship, target)
}

// handleTargetPort stores cargo in warehouse (don't sell to NPC), buys more
// goods into remaining capacity, then sails to a resupply port.
func (h *Harvester) handleTargetPort(ctx context.Context, ship *bot.ShipState, port *api.Port) error {
	// Store ship cargo into warehouse — DON'T sell to NPC.
	warehouseID := h.findWarehouseAt(port.ID)
	if warehouseID != uuid.Nil {
		stores := h.buildWarehouseStores(ship, warehouseID)
		h.executeWarehouseStores(ctx, ship, stores)
	}

	// Buy more goods from NPC into remaining ship capacity.
	h.ensurePortPrices(ctx, port)
	buys := h.buildBuyOrders(port.ID, ship)
	if err := h.executeBuys(ctx, ship, buys, nil); err != nil {
		if api.IsBankrupt(err) {
			return err
		}
		h.logger.Warn("harvester NPC buy at target failed", zap.Error(err))
	}

	// If we still have cargo, store it too.
	if warehouseID != uuid.Nil {
		stores := h.buildWarehouseStores(ship, warehouseID)
		h.executeWarehouseStores(ctx, ship, stores)
	}

	// Update stocking status.
	h.updateStockedStatus(port.ID)

	// Sail to a different port to buy more goods and bring them back.
	resupply := h.pickResupplyPort(port.ID)
	if resupply != uuid.Nil {
		h.logger.Debug("harvester: sailing to resupply",
			zap.String("ship", ship.Ship.Name),
			zap.String("resupply_port", resupply.String()),
		)
		return h.sendShipToPort(ctx, ship, resupply)
	}

	return nil
}

// OnTick scans for feeder orders to fill, evaluates fleet, and checks stocking.
func (h *Harvester) OnTick(ctx context.Context, _ *bot.CompanyState) error {
	target := schemeTargetPort()
	if target == uuid.Nil {
		return nil
	}

	// Fast order scan — fill feeder orders immediately.
	if schemeIsStocked() && time.Since(h.lastOrderScan) >= harvesterOrderScanInterval {
		h.lastOrderScan = time.Now()
		h.scanAndFillOrders(ctx, target)
	}

	// Update stocked status every tick.
	h.updateStockedStatus(target)

	// Fleet evaluation — buy ships and warehouses.
	h.ctx.State.RLock()
	shipCount := len(h.ctx.State.Ships)
	h.ctx.State.RUnlock()

	if (shipCount == 0 || time.Since(h.lastFleetEval) >= harvesterFleetInterval) && h.fleetEvalBackoff.ready() {
		h.lastFleetEval = time.Now()
		h.evalFleet(ctx, target)
	}

	return nil
}

// scanAndFillOrders finds feeder buy orders at the target port and fills them
// from warehouse inventory.
func (h *Harvester) scanAndFillOrders(ctx context.Context, targetPort uuid.UUID) {
	orders, err := h.ctx.Client.ListOrders(ctx, api.OrderFilters{
		PortIDs: []uuid.UUID{targetPort},
		Side:    "buy",
	})
	if err != nil {
		h.logger.Debug("harvester: failed to list orders", zap.Error(err))
		return
	}

	companyID := h.ctx.State.CompanyID
	warehouseID := h.findWarehouseAt(targetPort)
	if warehouseID == uuid.Nil {
		return
	}

	// Build warehouse stock map.
	stock := h.getWarehouseStock(warehouseID)

	filled := 0
	for _, order := range orders {
		// Skip own orders.
		if order.CompanyID == companyID {
			continue
		}
		if order.Remaining <= 0 {
			continue
		}

		// Check if we have this good in warehouse.
		available := stock[order.GoodID]
		if available <= 0 {
			continue
		}

		fillQty := order.Remaining
		if fillQty > available {
			fillQty = available
		}

		_, err := h.ctx.Client.FillOrder(ctx, order.ID, fillQty)
		if err != nil {
			h.logger.Debug("harvester: failed to fill order",
				zap.String("order_id", order.ID.String()),
				zap.Error(err),
			)
			continue
		}

		stock[order.GoodID] -= fillQty
		filled++

		// Estimate profit: order price vs NPC buy price.
		npcBuyPrice := 0
		if pp, ok := h.ctx.PriceCache.Get(targetPort, order.GoodID); ok {
			npcBuyPrice = pp.BuyPrice
		}
		profit := (order.Price - npcBuyPrice) * fillQty

		goodName := order.GoodID.String()[:8]
		if g := h.ctx.World.GetGood(order.GoodID); g != nil {
			goodName = g.Name
		}
		portName := targetPort.String()
		if p := h.ctx.World.GetPort(targetPort); p != nil {
			portName = p.Name
		}

		h.logger.Trade("harvester: filled feeder order",
			zap.String("order_id", order.ID.String()),
			zap.String("good", goodName),
			zap.Int("quantity", fillQty),
			zap.Int("order_price", order.Price),
			zap.Int("npc_buy_price", npcBuyPrice),
			zap.Int("est_profit", profit),
		)

		h.ctx.DB.Create(&db.P2POrderLog{
			CompanyID:  h.ctx.State.CompanyDBID(),
			OrderID:    order.ID.String(),
			OrderType:  "fill",
			GoodID:     order.GoodID.String(),
			GoodName:   goodName,
			PortID:     targetPort.String(),
			PortName:   portName,
			Quantity:   fillQty,
			Price:      order.Price,
			TotalValue: order.Price * fillQty,
			Strategy:   h.name,
			AgentName:  h.ctx.Agent.Name(),
		})
	}

	if filled > 0 {
		// Refresh warehouse inventory after fills.
		h.refreshWarehouseInventories(ctx, map[uuid.UUID]bool{warehouseID: true})
		h.ctx.Events.Emit(bot.EventTrade)
	}
}

// updateStockedStatus checks total goods at the target port (warehouse +
// docked ship cargo) and updates the stocked signal.
func (h *Harvester) updateStockedStatus(targetPort uuid.UUID) {
	totalGoods := 0

	// Count warehouse inventory at target port.
	warehouseID := h.findWarehouseAt(targetPort)
	if warehouseID != uuid.Nil {
		stock := h.getWarehouseStock(warehouseID)
		for _, qty := range stock {
			totalGoods += qty
		}
	}

	// Count cargo on ships docked at target port.
	h.ctx.State.RLock()
	for _, ss := range h.ctx.State.Ships {
		if ss.Ship.Status == "docked" && ss.Ship.PortID != nil && *ss.Ship.PortID == targetPort {
			totalGoods += ss.UsedCapacity()
		}
	}
	h.ctx.State.RUnlock()

	// Threshold: average ship capacity * harvesterStockThreshold.
	threshold := h.avgShipCapacity() * harvesterStockThreshold

	wasStocked := schemeIsStocked()
	nowStocked := totalGoods >= threshold

	if nowStocked != wasStocked {
		schemeSetStocked(nowStocked)
		h.logger.Info("harvester: stocked status changed",
			zap.Bool("stocked", nowStocked),
			zap.Int("total_goods", totalGoods),
			zap.Int("threshold", threshold),
		)
	}
}

// evalFleet buys warehouses and ships for the supply chain.
func (h *Harvester) evalFleet(ctx context.Context, targetPort uuid.UUID) {
	// Buy warehouse at target port if missing.
	if h.findWarehouseAt(targetPort) == uuid.Nil && h.warehouseBackoff.ready() {
		wh, err := h.ctx.Client.BuyWarehouse(ctx, targetPort)
		if err != nil {
			h.warehouseBackoff.fail()
			h.logger.Warn("harvester: failed to buy warehouse at target",
				zap.String("port_id", targetPort.String()),
				zap.Error(err),
			)
		} else {
			h.warehouseBackoff.succeed()
			h.logger.Info("harvester: purchased warehouse at target",
				zap.String("warehouse_id", wh.ID.String()),
			)
			portName := targetPort.String()
			if p := h.ctx.World.GetPort(targetPort); p != nil {
				portName = p.Name
			}
			h.ctx.DB.Create(&db.WarehouseEventLog{
				CompanyID:   h.ctx.State.CompanyDBID(),
				WarehouseID: wh.ID.String(),
				PortID:      targetPort.String(),
				PortName:    portName,
				EventType:   "purchase",
				Level:       wh.Level,
				Strategy:    h.name,
				AgentName:   h.ctx.Agent.Name(),
			})
			h.ctx.State.AddWarehouse(*wh)
			h.ctx.Events.Emit(bot.EventWarehouse)
		}
	}

	// Buy warehouse at next port for pre-positioning.
	nextPort := schemeNextPort()
	if nextPort != uuid.Nil && h.findWarehouseAt(nextPort) == uuid.Nil && h.warehouseBackoff.ready() {
		wh, err := h.ctx.Client.BuyWarehouse(ctx, nextPort)
		if err != nil {
			h.logger.Debug("harvester: failed to buy warehouse at next port", zap.Error(err))
		} else {
			h.logger.Info("harvester: purchased warehouse at next port",
				zap.String("warehouse_id", wh.ID.String()),
			)
			portName := nextPort.String()
			if p := h.ctx.World.GetPort(nextPort); p != nil {
				portName = p.Name
			}
			h.ctx.DB.Create(&db.WarehouseEventLog{
				CompanyID:   h.ctx.State.CompanyDBID(),
				WarehouseID: wh.ID.String(),
				PortID:      nextPort.String(),
				PortName:    portName,
				EventType:   "purchase",
				Level:       wh.Level,
				Strategy:    h.name,
				AgentName:   h.ctx.Agent.Name(),
			})
			h.ctx.State.AddWarehouse(*wh)
			h.ctx.Events.Emit(bot.EventWarehouse)
		}
	}

	// Buy ships aggressively — target 6+ for the supply chain.
	h.ctx.State.RLock()
	shipCount := len(h.ctx.State.Ships)
	h.ctx.State.RUnlock()

	if shipCount < 6 && h.shipBuyBackoff.ready() {
		// Find the largest cargo capacity ship type.
		var bestType uuid.UUID
		bestCapacity := 0
		for _, st := range h.ctx.World.ShipTypes {
			if st.Capacity > bestCapacity {
				bestCapacity = st.Capacity
				bestType = st.ID
			}
		}

		if bestType != uuid.Nil {
			portID := uuid.Nil
			if len(h.ctx.World.ShipyardPorts) > 0 {
				portID = h.ctx.World.ShipyardPorts[0]
			}
			if portID != uuid.Nil {
				ship := h.tryBuyShip(ctx, agent.ShipPurchase{
					ShipTypeID: bestType,
					PortID:     portID,
				})
				if ship != nil {
					h.shipBuyBackoff.succeed()
					h.logger.Info("harvester: purchased ship",
						zap.String("ship_id", ship.ID.String()),
						zap.String("name", ship.Name),
					)
				} else {
					h.shipBuyBackoff.fail()
				}
			}
		}
	}
}

// --- Helpers ---

// buildSellAllCargo creates sell orders for all cargo on the ship.
func (h *Harvester) buildSellAllCargo(ship *bot.ShipState) []agent.SellOrder {
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

// buildBuyOrders finds the cheapest goods at a port and builds buy orders to
// fill the ship's remaining capacity.
func (h *Harvester) buildBuyOrders(portID uuid.UUID, ship *bot.ShipState) []agent.BuyOrder {
	goods := h.ctx.World.Goods
	shipType := h.ctx.World.GetShipType(ship.Ship.ShipTypeID)
	if shipType == nil {
		return nil
	}

	capacity := shipType.Capacity - ship.UsedCapacity()
	if capacity <= 0 {
		return nil
	}

	type goodPrice struct {
		goodID uuid.UUID
		price  int
	}
	var available []goodPrice
	for _, g := range goods {
		pp, ok := h.ctx.PriceCache.Get(portID, g.ID)
		if ok && pp.BuyPrice > 0 {
			available = append(available, goodPrice{goodID: g.ID, price: pp.BuyPrice})
		}
	}
	if len(available) == 0 {
		return nil
	}

	// Sort cheapest first.
	for i := 0; i < len(available); i++ {
		for j := i + 1; j < len(available); j++ {
			if available[j].price < available[i].price {
				available[i], available[j] = available[j], available[i]
			}
		}
	}

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

// buildWarehouseStores creates warehouse transfer orders for all ship cargo.
func (h *Harvester) buildWarehouseStores(ship *bot.ShipState, warehouseID uuid.UUID) []agent.WarehouseTransfer {
	var stores []agent.WarehouseTransfer
	for _, c := range ship.Cargo {
		if c.Quantity > 0 {
			stores = append(stores, agent.WarehouseTransfer{
				WarehouseID: warehouseID,
				GoodID:      c.GoodID,
				Quantity:    c.Quantity,
			})
		}
	}
	return stores
}

// findWarehouseAt returns the warehouse ID at the given port, or uuid.Nil.
func (h *Harvester) findWarehouseAt(portID uuid.UUID) uuid.UUID {
	h.ctx.State.RLock()
	defer h.ctx.State.RUnlock()

	for _, w := range h.ctx.State.Warehouses {
		if w.Warehouse.PortID == portID {
			return w.Warehouse.ID
		}
	}
	return uuid.Nil
}

// getWarehouseStock returns a map of goodID → quantity for the given warehouse.
func (h *Harvester) getWarehouseStock(warehouseID uuid.UUID) map[uuid.UUID]int {
	stock := make(map[uuid.UUID]int)
	h.ctx.State.RLock()
	if wh, ok := h.ctx.State.Warehouses[warehouseID]; ok {
		for _, item := range wh.Inventory {
			stock[item.GoodID] = item.Quantity
		}
	}
	h.ctx.State.RUnlock()
	return stock
}

// pickResupplyPort finds the best non-target port to buy goods from. Prefers
// ports with shipyards (so the ship can also buy ships), or just the nearest
// different port.
func (h *Harvester) pickResupplyPort(currentPort uuid.UUID) uuid.UUID {
	target := schemeTargetPort()

	// Prefer shipyard ports for resupply (dual-purpose trips).
	for _, portID := range h.ctx.World.ShipyardPorts {
		if portID != target && portID != currentPort {
			return portID
		}
	}

	// Fall back to any port that isn't the target.
	for _, port := range h.ctx.World.Ports {
		if port.ID != target && port.ID != currentPort {
			return port.ID
		}
	}

	return uuid.Nil
}

// avgShipCapacity returns the average cargo capacity across owned ships, or a
// sensible default if no ships are owned yet.
func (h *Harvester) avgShipCapacity() int {
	h.ctx.State.RLock()
	defer h.ctx.State.RUnlock()

	if len(h.ctx.State.Ships) == 0 {
		return 100 // sensible default
	}

	total := 0
	for _, ss := range h.ctx.State.Ships {
		if st := h.ctx.World.GetShipType(ss.Ship.ShipTypeID); st != nil {
			total += st.Capacity
		}
	}
	return total / len(h.ctx.State.Ships)
}
