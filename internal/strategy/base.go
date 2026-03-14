package strategy

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// baseStrategy provides shared logic used by all strategy implementations:
// building decision requests, executing trade/fleet decisions, safety checks,
// and logging agent decisions to the database.
type baseStrategy struct {
	ctx    bot.StrategyContext
	name   string
	logger *bot.CompanyLogger
}

func (b *baseStrategy) Init(ctx bot.StrategyContext) error {
	b.ctx = ctx
	b.logger = ctx.Logger
	return nil
}

func (b *baseStrategy) Shutdown() error { return nil }

// --- Decision Request Builders ---

// buildTradeRequest assembles a TradeDecisionRequest from current state.
func (b *baseStrategy) buildTradeRequest(ship *bot.ShipState, port *api.Port) agent.TradeDecisionRequest {
	state := b.ctx.State

	state.RLock()
	defer state.RUnlock()

	world := b.ctx.World

	// Build ship snapshots.
	allShips := make([]agent.ShipSnapshot, 0, len(state.Ships))
	for _, s := range state.Ships {
		allShips = append(allShips, shipToSnapshot(s, world))
	}

	// Build warehouse snapshots.
	warehouses := make([]agent.WarehouseSnapshot, 0, len(state.Warehouses))
	for _, w := range state.Warehouses {
		warehouses = append(warehouses, warehouseToSnapshot(w))
	}

	return agent.TradeDecisionRequest{
		StrategyHint: b.name,
		Company: agent.CompanySnapshot{
			ID:          state.CompanyID,
			Treasury:    state.Treasury,
			Reputation:  state.Reputation,
			TotalUpkeep: state.TotalUpkeep,
		},
		Ship:       shipToSnapshot(ship, world),
		AllShips:    allShips,
		Warehouses: warehouses,
		PriceCache: b.ctx.PriceCache.All(),
		Routes:     world.ToAgentRoutes(),
		Ports:      world.ToAgentPorts(),
		Constraints: agent.Constraints{
			TreasuryFloor: state.TotalUpkeep * 2,
			MaxSpend:      state.Treasury - state.TotalUpkeep*2,
		},
	}
}

// buildFleetRequest assembles a FleetDecisionRequest from current state.
func (b *baseStrategy) buildFleetRequest() agent.FleetDecisionRequest {
	state := b.ctx.State

	state.RLock()
	defer state.RUnlock()

	ships := make([]agent.ShipSnapshot, 0, len(state.Ships))
	for _, s := range state.Ships {
		ships = append(ships, shipToSnapshot(s, b.ctx.World))
	}

	warehouses := make([]agent.WarehouseSnapshot, 0, len(state.Warehouses))
	for _, w := range state.Warehouses {
		warehouses = append(warehouses, warehouseToSnapshot(w))
	}

	return agent.FleetDecisionRequest{
		StrategyHint:  b.name,
		Company: agent.CompanySnapshot{
			ID:          state.CompanyID,
			Treasury:    state.Treasury,
			Reputation:  state.Reputation,
			TotalUpkeep: state.TotalUpkeep,
		},
		Ships:         ships,
		Warehouses:    warehouses,
		ShipTypes:     b.ctx.World.ToAgentShipTypes(),
		PriceCache:    b.ctx.PriceCache.All(),
		ShipyardPorts: b.ctx.World.ShipyardPorts,
	}
}

// buildTradeRequestWithPassengers extends buildTradeRequest by fetching
// available passengers at the current port and boarded passengers on the ship.
func (b *baseStrategy) buildTradeRequestWithPassengers(ctx context.Context, ship *bot.ShipState, port *api.Port) agent.TradeDecisionRequest {
	req := b.buildTradeRequest(ship, port)

	// Only fetch passengers if the ship type supports them.
	if req.Ship.PassengerCap <= 0 {
		return req
	}

	// Fetch available passengers at this port.
	available, err := b.ctx.Client.ListPassengers(ctx, api.PassengerFilters{
		Status: "available",
		PortID: port.ID.String(),
	})
	if err != nil {
		b.logger.Debug("failed to fetch available passengers", zap.Error(err))
	} else {
		for _, p := range available {
			req.AvailablePassengers = append(req.AvailablePassengers, agent.PassengerInfo{
				ID:                p.ID,
				Count:             p.Count,
				Bid:               p.Bid,
				OriginPortID:      p.OriginPortID,
				DestinationPortID: p.DestinationPortID,
				ExpiresAt:         p.ExpiresAt,
			})
		}
	}

	// Fetch passengers already boarded on this ship.
	boarded, err := b.ctx.Client.ListPassengers(ctx, api.PassengerFilters{
		Status: "boarded",
		ShipID: ship.Ship.ID.String(),
	})
	if err != nil {
		b.logger.Debug("failed to fetch boarded passengers", zap.Error(err))
	} else {
		for _, p := range boarded {
			req.BoardedPassengers = append(req.BoardedPassengers, agent.PassengerInfo{
				ID:                p.ID,
				Count:             p.Count,
				Bid:               p.Bid,
				OriginPortID:      p.OriginPortID,
				DestinationPortID: p.DestinationPortID,
				ExpiresAt:         p.ExpiresAt,
			})
		}
	}

	return req
}

// --- Passenger Boarding ---

// boardPassengers boards the specified passengers onto the ship.
func (b *baseStrategy) boardPassengers(ctx context.Context, ship *bot.ShipState, passengerIDs []uuid.UUID) {
	for _, pid := range passengerIDs {
		passenger, err := b.ctx.Client.BoardPassenger(ctx, pid, ship.Ship.ID)
		if err != nil {
			b.logger.Warn("failed to board passenger",
				zap.String("passenger_id", pid.String()),
				zap.Error(err),
			)
			continue
		}

		originName := passenger.OriginPortID.String()
		if p := b.ctx.World.GetPort(passenger.OriginPortID); p != nil {
			originName = p.Name
		}
		destName := passenger.DestinationPortID.String()
		if p := b.ctx.World.GetPort(passenger.DestinationPortID); p != nil {
			destName = p.Name
		}

		b.logger.Trade("boarded passengers",
			zap.String("passenger_id", pid.String()),
			zap.Int("count", passenger.Count),
			zap.Int("bid", passenger.Bid),
			zap.String("destination", destName),
		)

		// Log passenger boarding to database.
		plog := db.PassengerLog{
			CompanyID:           b.ctx.State.CompanyDBID(),
			PassengerID:         pid.String(),
			Count:               passenger.Count,
			Bid:                 passenger.Bid,
			OriginPortID:        passenger.OriginPortID.String(),
			OriginPortName:      originName,
			DestinationPortID:   passenger.DestinationPortID.String(),
			DestinationPortName: destName,
			ShipID:              ship.Ship.ID.String(),
			ShipName:            ship.Ship.Name,
			Strategy:            b.name,
			AgentName:           b.ctx.Agent.Name(),
		}
		if err := b.ctx.DB.Create(&plog).Error; err != nil {
			b.logger.Warn("failed to log passenger boarding", zap.Error(err))
		}

		// Update cumulative counter for P&L snapshots.
		b.ctx.State.AddPassengerRevenue(int64(passenger.Bid))

		// Track boarded passenger count on the ship.
		ship.PassengerCount += passenger.Count
	}
}

// --- Trade Execution ---

// executeSells sells cargo from a ship at the current port.
func (b *baseStrategy) executeSells(ctx context.Context, ship *bot.ShipState, sells []agent.SellOrder) error {
	if len(sells) == 0 {
		return nil
	}

	portID := ship.Ship.PortID
	if portID == nil {
		return nil
	}

	// Build batch quote requests for all sells.
	quoteReqs := make([]api.QuoteRequest, 0, len(sells))
	for _, sell := range sells {
		quoteReqs = append(quoteReqs, api.QuoteRequest{
			PortID:   *portID,
			GoodID:   sell.GoodID,
			Action:   "sell",
			Quantity: sell.Quantity,
		})
	}

	results, err := b.ctx.Client.BatchQuotes(ctx, quoteReqs)
	if err != nil {
		return err
	}

	// Execute successful quotes.
	var execReqs []api.ExecuteQuoteRequest
	for _, r := range results {
		if r.Status == "success" && r.Token != "" {
			execReqs = append(execReqs, api.ExecuteQuoteRequest{
				Token: r.Token,
				Destinations: []api.Destination{{
					Type:     "ship",
					ID:       ship.Ship.ID,
					Quantity: r.Quote.Quantity,
				}},
			})
		}
	}

	if len(execReqs) == 0 {
		return nil
	}

	execResults, err := b.ctx.Client.BatchExecuteQuotes(ctx, execReqs)
	if err != nil {
		return err
	}

	for _, r := range execResults {
		if r.Status == "success" && r.Execution != nil {
			b.logger.Trade("sold cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution)
		}
	}

	return nil
}

// executeBuys buys cargo and loads it onto the ship at the current port.
func (b *baseStrategy) executeBuys(ctx context.Context, ship *bot.ShipState, buys []agent.BuyOrder) error {
	if len(buys) == 0 {
		return nil
	}

	portID := ship.Ship.PortID
	if portID == nil {
		return nil
	}

	// Safety check: don't spend below treasury floor.
	floor := b.ctx.State.TreasuryFloor()
	b.ctx.State.RLock()
	available := b.ctx.State.Treasury - floor
	b.ctx.State.RUnlock()

	if available <= 0 {
		b.logger.Warn("treasury too low to buy, skipping",
			zap.Int64("available", available),
			zap.Int64("floor", floor),
		)
		return nil
	}

	quoteReqs := make([]api.QuoteRequest, 0, len(buys))
	for _, buy := range buys {
		quoteReqs = append(quoteReqs, api.QuoteRequest{
			PortID:   *portID,
			GoodID:   buy.GoodID,
			Action:   "buy",
			Quantity: buy.Quantity,
		})
	}

	results, err := b.ctx.Client.BatchQuotes(ctx, quoteReqs)
	if err != nil {
		return err
	}

	// Filter quotes that would exceed treasury floor.
	var execReqs []api.ExecuteQuoteRequest
	for _, r := range results {
		if r.Status != "success" || r.Token == "" || r.Quote == nil {
			continue
		}

		if int64(r.Quote.TotalPrice) > available {
			b.logger.Warn("skipping buy: would exceed treasury floor",
				zap.Int("cost", r.Quote.TotalPrice),
				zap.Int64("available", available),
			)
			continue
		}

		dest := ship.Ship.ID
		// Use explicit destination if provided.
		for _, buy := range buys {
			if buy.GoodID == r.Quote.GoodID && buy.Destination != uuid.Nil {
				dest = buy.Destination
				break
			}
		}

		execReqs = append(execReqs, api.ExecuteQuoteRequest{
			Token: r.Token,
			Destinations: []api.Destination{{
				Type:     "ship",
				ID:       dest,
				Quantity: r.Quote.Quantity,
			}},
		})

		available -= int64(r.Quote.TotalPrice)
	}

	if len(execReqs) == 0 {
		return nil
	}

	execResults, err := b.ctx.Client.BatchExecuteQuotes(ctx, execReqs)
	if err != nil {
		return err
	}

	for _, r := range execResults {
		if r.Status == "success" && r.Execution != nil {
			b.logger.Trade("bought cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution)
		}
	}

	return nil
}

// sendShipToPort sends a ship to the specified destination port.
func (b *baseStrategy) sendShipToPort(ctx context.Context, ship *bot.ShipState, destPortID uuid.UUID) error {
	if ship.Ship.PortID == nil {
		return nil
	}

	route := b.ctx.World.FindRoute(*ship.Ship.PortID, destPortID)
	if route == nil {
		// Route missing from cache — fetch directly from API.
		b.logger.Warn("route not in cache, fetching from API",
			zap.String("from", ship.Ship.PortID.String()),
			zap.String("to", destPortID.String()),
		)
		fetched, err := b.ctx.Client.ListRoutes(ctx, api.RouteFilters{
			FromID: ship.Ship.PortID,
			ToID:   &destPortID,
		})
		if err != nil || len(fetched) == 0 {
			b.logger.Warn("no route found even from API",
				zap.String("from", ship.Ship.PortID.String()),
				zap.String("to", destPortID.String()),
				zap.Error(err),
			)
			return nil
		}
		route = &fetched[0]
		b.ctx.World.AddRoute(*route)
	}

	updated, err := b.ctx.Client.SendTransit(ctx, ship.Ship.ID, route.ID)
	if err != nil {
		return err
	}

	// Immediately update local state so the ship isn't re-dispatched by
	// dispatchIdleShips before the SSE event arrives.
	b.ctx.State.Lock()
	if ss := b.ctx.State.Ships[ship.Ship.ID]; ss != nil {
		ss.Ship.Status = "traveling"
		ss.Ship.PortID = nil
		ss.Ship.RouteID = &route.ID
		ss.Ship.ArrivingAt = updated.ArrivingAt
	}
	b.ctx.State.Unlock()

	destPort := b.ctx.World.GetPort(destPortID)
	destName := destPortID.String()
	if destPort != nil {
		destName = destPort.Name
	}

	b.logger.Info("ship sent to port",
		zap.String("ship", ship.Ship.Name),
		zap.String("destination", destName),
		zap.Float64("distance", route.Distance),
	)

	return nil
}

// executeFleetDecision handles ship purchases, ship sales, and warehouse construction.
func (b *baseStrategy) executeFleetDecision(ctx context.Context, decision *agent.FleetDecision) {
	// Sell ships first to free up upkeep budget.
	for _, shipID := range decision.SellShips {
		// Find the ship's current port.
		ship, err := b.ctx.Client.GetShip(ctx, shipID)
		if err != nil {
			b.logger.Warn("failed to get ship for sale",
				zap.String("ship_id", shipID.String()),
				zap.Error(err),
			)
			continue
		}
		if ship.PortID == nil {
			b.logger.Warn("cannot sell ship that is not docked",
				zap.String("ship_id", shipID.String()),
				zap.String("status", ship.Status),
			)
			continue
		}
		// Find shipyard at the ship's port.
		shipyard, err := b.ctx.Client.FindShipyard(ctx, *ship.PortID)
		if err != nil || shipyard == nil {
			b.logger.Warn("no shipyard at ship's port, cannot sell",
				zap.String("ship_id", shipID.String()),
				zap.String("port_id", ship.PortID.String()),
				zap.Error(err),
			)
			continue
		}
		resp, err := b.ctx.Client.SellShip(ctx, shipyard.ID, shipID)
		if err != nil {
			b.logger.Warn("failed to sell ship",
				zap.String("ship_id", shipID.String()),
				zap.Error(err),
			)
			continue
		}
		b.logger.Trade("sold ship",
			zap.String("ship_id", shipID.String()),
			zap.Int("price", resp.Price),
		)
	}

	for _, purchase := range decision.BuyShips {
		ship := b.tryBuyShip(ctx, purchase)
		if ship != nil {
			b.logger.Info("purchased new ship",
				zap.String("ship_id", ship.ID.String()),
				zap.String("name", ship.Name),
			)
		}
	}

	for _, portID := range decision.BuyWarehouses {
		wh, err := b.ctx.Client.BuyWarehouse(ctx, portID)
		if err != nil {
			b.logger.Error("failed to buy warehouse",
				zap.String("port_id", portID.String()),
				zap.Error(err),
			)
			continue
		}

		b.logger.Info("purchased warehouse",
			zap.String("warehouse_id", wh.ID.String()),
			zap.String("port_id", portID.String()),
		)
	}
}

// tryBuyShip attempts to purchase a ship at the given port. It checks the
// shipyard inventory first and picks the best affordable ship. Falls back to
// other shipyard ports if the primary port has no affordable stock.
func (b *baseStrategy) tryBuyShip(ctx context.Context, purchase agent.ShipPurchase) *api.Ship {
	// Check how much we can spend (treasury minus safety margin for upkeep).
	b.ctx.State.RLock()
	treasury := b.ctx.State.Treasury
	upkeep := b.ctx.State.TotalUpkeep
	b.ctx.State.RUnlock()

	// Keep a safety margin of 3x upkeep so we don't go bankrupt.
	safetyMargin := upkeep * 3
	// Also account for ~5% tax on purchase.
	maxSpend := treasury - safetyMargin
	if maxSpend <= 0 {
		b.logger.Info("treasury too low to buy ship",
			zap.Int64("treasury", treasury),
			zap.Int64("safety_margin", safetyMargin),
		)
		return nil
	}

	// Try the requested port first, then fall back to other shipyard ports.
	portsToTry := []uuid.UUID{purchase.PortID}
	for _, portID := range b.ctx.World.ShipyardPorts {
		if portID != purchase.PortID {
			portsToTry = append(portsToTry, portID)
		}
	}

	for _, portID := range portsToTry {
		shipyard, err := b.ctx.Client.FindShipyard(ctx, portID)
		if err != nil || shipyard == nil {
			continue
		}

		inventory, err := b.ctx.Client.GetShipyardInventory(ctx, shipyard.ID)
		if err != nil {
			b.logger.Warn("failed to fetch shipyard inventory",
				zap.String("port_id", portID.String()),
				zap.Error(err),
			)
			continue
		}

		if len(inventory) == 0 {
			b.logger.Debug("shipyard has no inventory",
				zap.String("port_id", portID.String()),
			)
			continue
		}

		// Sort inventory by cost (cheapest first) so we pick affordable ships.
		sort.Slice(inventory, func(i, j int) bool {
			return inventory[i].Cost < inventory[j].Cost
		})

		// First try to find the exact requested ship type that we can afford.
		var chosenItem *api.ShipyardInventoryItem
		for i := range inventory {
			if inventory[i].ShipTypeID == purchase.ShipTypeID {
				// Add ~5% tax buffer to cost check.
				costWithTax := int64(float64(inventory[i].Cost) * 1.06)
				if costWithTax <= maxSpend {
					chosenItem = &inventory[i]
					break
				}
			}
		}

		// Fall back to the cheapest affordable ship of any type.
		if chosenItem == nil {
			for i := range inventory {
				costWithTax := int64(float64(inventory[i].Cost) * 1.06)
				if costWithTax <= maxSpend {
					chosenItem = &inventory[i]
					b.logger.Info("desired ship type not in stock or too expensive, using cheapest affordable alternative",
						zap.String("wanted_type", purchase.ShipTypeID.String()),
						zap.String("using_type", chosenItem.ShipTypeID.String()),
						zap.Int("cost", chosenItem.Cost),
					)
					break
				}
			}
		}

		if chosenItem == nil {
			b.logger.Debug("no affordable ships at shipyard",
				zap.String("port_id", portID.String()),
				zap.Int64("max_spend", maxSpend),
				zap.Int("cheapest_available", inventory[0].Cost),
			)
			continue
		}

		ship, err := b.ctx.Client.BuyShip(ctx, shipyard.ID, chosenItem.ShipTypeID)
		if err != nil {
			b.logger.Error("failed to buy ship",
				zap.String("ship_type_id", chosenItem.ShipTypeID.String()),
				zap.String("port_id", portID.String()),
				zap.Int("cost", chosenItem.Cost),
				zap.Error(err),
			)
			continue
		}

		// Track ship purchase cost for P&L (includes ~5% port tax).
		b.ctx.State.AddShipCost(int64(chosenItem.Cost))

		return ship
	}

	b.logger.Warn("could not buy ship at any shipyard port",
		zap.Int64("max_spend", maxSpend),
	)
	return nil
}

// --- Agent Decision Logging ---

// logAgentDecision records an agent decision to the database.
func (b *baseStrategy) logAgentDecision(
	decisionType string,
	request any,
	response any,
	reasoning string,
	confidence float64,
	latency time.Duration,
) {
	reqJSON, _ := json.Marshal(request)
	respJSON, _ := json.Marshal(response)

	log := db.AgentDecisionLog{
		CompanyID:    b.ctx.State.CompanyDBID(),
		AgentName:    b.ctx.Agent.Name(),
		DecisionType: decisionType,
		Request:      string(reqJSON),
		Response:     string(respJSON),
		Reasoning:    reasoning,
		Confidence:   confidence,
		LatencyMs:    latency.Milliseconds(),
	}

	if err := b.ctx.DB.Create(&log).Error; err != nil {
		b.logger.Warn("failed to log agent decision", zap.Error(err))
	}
}

// --- Trade Logging ---

// recordTrade writes a trade to the database.
func (b *baseStrategy) recordTrade(exec *api.TradeExecution) {
	portName := exec.PortID.String()
	if p := b.ctx.World.GetPort(exec.PortID); p != nil {
		portName = p.Name
	}
	goodName := exec.GoodID.String()
	if g := b.ctx.World.GetGood(exec.GoodID); g != nil {
		goodName = g.Name
	}

	trade := db.TradeLog{
		CompanyID:  b.ctx.State.CompanyDBID(),
		Action:     exec.Action,
		GoodID:     exec.GoodID.String(),
		GoodName:   goodName,
		PortID:     exec.PortID.String(),
		PortName:   portName,
		Quantity:   exec.Quantity,
		UnitPrice:  exec.UnitPrice,
		TotalPrice: exec.TotalPrice,
		Strategy:   b.name,
		AgentName:  b.ctx.Agent.Name(),
	}

	if err := b.ctx.DB.Create(&trade).Error; err != nil {
		b.logger.Warn("failed to log trade", zap.Error(err))
	}

	// Update cumulative counters for P&L snapshots.
	if exec.Action == "sell" {
		b.ctx.State.AddTradeRevenue(int64(exec.TotalPrice))
		b.recordRoutePerformance(exec)
	} else {
		b.ctx.State.AddTradeCost(int64(exec.TotalPrice))
	}
}

// recordRoutePerformance finds the most recent buy of the same good and
// records the profit for the completed buy→sell cycle.
func (b *baseStrategy) recordRoutePerformance(sell *api.TradeExecution) {
	// Find the most recent buy of the same good.
	var buyTrade db.TradeLog
	err := b.ctx.DB.Where("company_id = ? AND action = ? AND good_id = ? AND created_at > ?",
		b.ctx.State.CompanyDBID(), "buy", sell.GoodID.String(), time.Now().Add(-24*time.Hour)).
		Order("created_at DESC").First(&buyTrade).Error
	if err != nil {
		return // No matching buy found.
	}

	profit := sell.TotalPrice - buyTrade.TotalPrice

	rp := db.RoutePerformance{
		CompanyID:  b.ctx.State.CompanyDBID(),
		FromPortID: buyTrade.PortID,
		ToPortID:   sell.PortID.String(),
		GoodID:     sell.GoodID.String(),
		BuyPrice:   buyTrade.UnitPrice,
		SellPrice:  sell.UnitPrice,
		Quantity:   sell.Quantity,
		Profit:     profit,
		Strategy:   b.name,
	}

	if err := b.ctx.DB.Create(&rp).Error; err != nil {
		b.logger.Warn("failed to record route performance", zap.Error(err))
	}
}

// --- Snapshot Converters ---

func shipToSnapshot(s *bot.ShipState, world *bot.WorldCache) agent.ShipSnapshot {
	cargo := make([]agent.CargoItem, len(s.Cargo))
	for i, c := range s.Cargo {
		cargo[i] = agent.CargoItem{
			GoodID:   c.GoodID,
			Quantity: c.Quantity,
		}
	}

	snap := agent.ShipSnapshot{
		ID:        s.Ship.ID,
		Name:      s.Ship.Name,
		Status:    s.Ship.Status,
		PortID:    s.Ship.PortID,
		Cargo:     cargo,
		ArrivesAt: s.Ship.ArrivingAt,
	}

	// Enrich with ship type info if available.
	if st := world.GetShipType(s.Ship.ShipTypeID); st != nil {
		snap.Capacity = st.Capacity
		snap.Speed = st.Speed
		snap.PassengerCap = st.Passengers
	}

	return snap
}

func warehouseToSnapshot(w *bot.WarehouseState) agent.WarehouseSnapshot {
	items := make([]agent.WarehouseItem, len(w.Inventory))
	for i, item := range w.Inventory {
		items[i] = agent.WarehouseItem{
			GoodID:   item.GoodID,
			Quantity: item.Quantity,
		}
	}

	return agent.WarehouseSnapshot{
		ID:       w.Warehouse.ID,
		PortID:   w.Warehouse.PortID,
		Level:    w.Warehouse.Level,
		Capacity: w.Warehouse.Capacity,
		Items:    items,
	}
}
