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

// --- On-Demand Price Scanning ---

// ensurePortPrices checks if the price cache for a port is stale or missing
// and fetches fresh buy/sell quotes on demand. This prevents ships from
// arriving at unscanned ports and finding no profitable trades.
func (b *baseStrategy) ensurePortPrices(ctx context.Context, port *api.Port) {
	const stalePriceThreshold = 3 * time.Minute

	age := b.ctx.PriceCache.PortAge(port.ID)
	if age < stalePriceThreshold {
		return
	}

	b.logger.Debug("port prices stale or missing, scanning on demand",
		zap.String("port", port.Name),
		zap.Duration("age", age),
	)

	goods := b.ctx.World.Goods

	// Fetch buy quotes for all goods at this port.
	buyReqs := make([]api.QuoteRequest, len(goods))
	for i, good := range goods {
		buyReqs[i] = api.QuoteRequest{
			PortID:   port.ID,
			GoodID:   good.ID,
			Action:   "buy",
			Quantity: 1,
		}
	}

	buyResults, err := b.ctx.Client.BatchQuotesWithPriority(ctx, buyReqs, api.PriorityNormal)
	if err != nil {
		b.logger.Warn("on-demand buy quote scan failed", zap.String("port", port.Name), zap.Error(err))
		return
	}

	// Fetch sell quotes for all goods at this port.
	sellReqs := make([]api.QuoteRequest, len(goods))
	for i, good := range goods {
		sellReqs[i] = api.QuoteRequest{
			PortID:   port.ID,
			GoodID:   good.ID,
			Action:   "sell",
			Quantity: 1,
		}
	}

	sellResults, err := b.ctx.Client.BatchQuotesWithPriority(ctx, sellReqs, api.PriorityNormal)
	if err != nil {
		b.logger.Warn("on-demand sell quote scan failed", zap.String("port", port.Name), zap.Error(err))
		return
	}

	updated := 0
	for i, good := range goods {
		var buyPrice, sellPrice int
		if i < len(buyResults) && buyResults[i].Status == "success" && buyResults[i].Quote != nil {
			buyPrice = buyResults[i].Quote.UnitPrice
		}
		if i < len(sellResults) && sellResults[i].Status == "success" && sellResults[i].Quote != nil {
			sellPrice = sellResults[i].Quote.UnitPrice
		}
		if buyPrice > 0 || sellPrice > 0 {
			b.ctx.PriceCache.Set(port.ID, good.ID, buyPrice, sellPrice)
			updated++
		}
	}

	b.logger.Debug("on-demand port scan complete",
		zap.String("port", port.Name),
		zap.Int("prices_updated", updated),
	)
}

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

	// Build tunable params map from CompanyParams.
	var params map[string]float64
	if state.Params != nil {
		params = map[string]float64{
			"minMarginPct":           state.Params.MinMarginPct,
			"passengerWeight":        state.Params.PassengerWeight,
			"passengerDestBonus":     state.Params.PassengerDestBonus,
			"speculativeTradeEnabled": 0.0,
		}
		if state.Params.SpeculativeTradeEnabled {
			params["speculativeTradeEnabled"] = 1.0
		}
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
		Params:     params,
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
// available passengers at the current port, boarded passengers on the ship,
// and P2P market orders at the current port for fill opportunities.
func (b *baseStrategy) buildTradeRequestWithPassengers(ctx context.Context, ship *bot.ShipState, port *api.Port) agent.TradeDecisionRequest {
	// Ensure we have fresh prices for this port before making trade decisions.
	b.ensurePortPrices(ctx, port)

	req := b.buildTradeRequest(ship, port)

	// Fetch recent route performance history for destination scoring.
	var routePerfs []db.RoutePerformance
	if err := b.ctx.DB.Where("company_id = ? AND created_at > ?",
		b.ctx.State.CompanyDBID(), time.Now().Add(-24*time.Hour)).
		Order("created_at DESC").Limit(50).Find(&routePerfs).Error; err != nil {
		b.logger.Debug("failed to fetch route history", zap.Error(err))
	} else {
		for _, rp := range routePerfs {
			fromID, _ := uuid.Parse(rp.FromPortID)
			toID, _ := uuid.Parse(rp.ToPortID)
			goodID, _ := uuid.Parse(rp.GoodID)
			req.RouteHistory = append(req.RouteHistory, agent.RoutePerformanceEntry{
				FromPortID: fromID,
				ToPortID:   toID,
				GoodID:     goodID,
				Profit:     rp.Profit,
				Quantity:   rp.Quantity,
				CreatedAt:  rp.CreatedAt,
			})
		}
	}

	// Fetch P2P orders at this port for fill opportunities.
	portOrders, err := b.ctx.Client.ListOrders(ctx, api.OrderFilters{
		PortIDs: []uuid.UUID{port.ID},
	})
	if err != nil {
		b.logger.Debug("failed to fetch port orders", zap.Error(err))
	} else {
		companyID := b.ctx.State.CompanyID
		for _, o := range portOrders {
			mo := agent.MarketOrder{
				ID:        o.ID,
				PortID:    o.PortID,
				GoodID:    o.GoodID,
				Side:      o.Side,
				Price:     o.Price,
				Remaining: o.Remaining,
			}
			req.PortOrders = append(req.PortOrders, mo)
			if o.CompanyID == companyID {
				req.OwnOrders = append(req.OwnOrders, mo)
			}
		}
	}

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
	if len(passengerIDs) > 0 {
		b.ctx.Events.Emit(bot.EventPassenger)
	}
}

// --- P2P Order Fills ---

// executeFills fills P2P market orders at the current port.
func (b *baseStrategy) executeFills(ctx context.Context, fills []agent.FillOrder) {
	for _, fill := range fills {
		_, err := b.ctx.Client.FillOrder(ctx, fill.OrderID, fill.Quantity)
		if err != nil {
			b.logger.Warn("failed to fill market order",
				zap.String("order_id", fill.OrderID.String()),
				zap.Int("quantity", fill.Quantity),
				zap.Error(err),
			)
			continue
		}
		b.logger.Trade("filled market order",
			zap.String("order_id", fill.OrderID.String()),
			zap.Int("quantity", fill.Quantity),
		)
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

	traded := false
	for _, r := range execResults {
		if r.Status == "success" && r.Execution != nil {
			b.logger.Trade("sold cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution)
			traded = true
		}
	}
	if traded {
		b.ctx.Events.Emit(bot.EventTrade)
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
		b.logger.Debug("treasury too low to buy, skipping",
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
			b.logger.Debug("skipping buy: would exceed treasury floor",
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

	traded := false
	for _, r := range execResults {
		if r.Status == "success" && r.Execution != nil {
			b.logger.Trade("bought cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution)
			traded = true
		}
	}
	if traded {
		b.ctx.Events.Emit(bot.EventTrade)
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
		b.logger.Debug("route not in cache, fetching from API",
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
		b.ctx.Events.Emit(bot.EventShipSold)
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
			b.logger.Warn("failed to buy warehouse",
				zap.String("port_id", portID.String()),
				zap.Error(err),
			)
			continue
		}

		b.logger.Info("purchased warehouse",
			zap.String("warehouse_id", wh.ID.String()),
			zap.String("port_id", portID.String()),
		)
		b.ctx.Events.Emit(bot.EventWarehouse)
	}
}

// tryBuyShip attempts to purchase a ship at the given port. It checks the
// shipyard inventory first and picks the best affordable ship. Falls back to
// other shipyard ports if the primary port has no affordable stock.
//
// Safety: uses POST-purchase upkeep (current + new ship) in the 24h reserve
// check, and refreshes economy state after purchase so subsequent fleet evals
// see up-to-date treasury/upkeep figures.
func (b *baseStrategy) tryBuyShip(ctx context.Context, purchase agent.ShipPurchase) *api.Ship {
	// Refresh economy before checking — ensures we have fresh treasury/upkeep
	// after any recent purchases by this or other strategies.
	if econ, err := b.ctx.Client.GetEconomy(ctx); err == nil {
		b.ctx.State.UpdateEconomy(econ)
	}

	b.ctx.State.RLock()
	treasury := b.ctx.State.Treasury
	upkeep := b.ctx.State.TotalUpkeep
	b.ctx.State.RUnlock()

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

		// canAffordShip checks if we can buy a ship and still maintain 24h of
		// post-purchase upkeep (current upkeep + new ship's upkeep).
		canAffordShip := func(item *api.ShipyardInventoryItem) bool {
			costWithTax := int64(float64(item.Cost) * 1.06)
			// Look up the new ship's upkeep from world data.
			newShipUpkeep := int64(0)
			if st := b.ctx.World.GetShipType(item.ShipTypeID); st != nil {
				newShipUpkeep = int64(st.Upkeep)
			}
			postPurchaseUpkeep := upkeep + newShipUpkeep
			safetyMargin := postPurchaseUpkeep * 24
			return treasury >= costWithTax+safetyMargin
		}

		// First try to find the exact requested ship type that we can afford.
		var chosenItem *api.ShipyardInventoryItem
		for i := range inventory {
			if inventory[i].ShipTypeID == purchase.ShipTypeID {
				if canAffordShip(&inventory[i]) {
					chosenItem = &inventory[i]
					break
				}
			}
		}

		// Fall back to the cheapest affordable ship of any type.
		if chosenItem == nil {
			for i := range inventory {
				if canAffordShip(&inventory[i]) {
					chosenItem = &inventory[i]
					b.logger.Debug("desired ship type not in stock or too expensive, using cheapest affordable alternative",
						zap.String("wanted_type", purchase.ShipTypeID.String()),
						zap.String("using_type", chosenItem.ShipTypeID.String()),
						zap.Int("cost", chosenItem.Cost),
					)
					break
				}
			}
		}

		if chosenItem == nil {
			newShipUpkeep := int64(0)
			if st := b.ctx.World.GetShipType(inventory[0].ShipTypeID); st != nil {
				newShipUpkeep = int64(st.Upkeep)
			}
			b.logger.Debug("no affordable ships at shipyard",
				zap.String("port_id", portID.String()),
				zap.Int64("treasury", treasury),
				zap.Int64("post_purchase_reserve_needed", (upkeep+newShipUpkeep)*24),
				zap.Int("cheapest_available", inventory[0].Cost),
			)
			continue
		}

		ship, err := b.ctx.Client.BuyShip(ctx, shipyard.ID, chosenItem.ShipTypeID)
		if err != nil {
			b.logger.Warn("failed to buy ship, trying next port",
				zap.String("ship_type_id", chosenItem.ShipTypeID.String()),
				zap.String("port_id", portID.String()),
				zap.Int("cost", chosenItem.Cost),
				zap.Error(err),
			)
			continue
		}

		// Give the ship a fun FFXIV-themed name.
		name := bot.GenerateShipName()
		if renamed, err := b.ctx.Client.RenameShip(ctx, ship.ID, name); err != nil {
			b.logger.Warn("failed to name ship", zap.Error(err))
		} else {
			ship = renamed
			b.logger.Info("christened new ship",
				zap.String("name", name),
				zap.String("ship_id", ship.ID.String()),
			)
			// Update local state so the dashboard picks up the new name
			// immediately instead of waiting for the next SSE refresh.
			b.ctx.State.Lock()
			if ss := b.ctx.State.Ships[ship.ID]; ss != nil {
				ss.Ship.Name = name
			}
			b.ctx.State.Unlock()
		}

		// Track ship purchase cost for P&L (includes ~5% port tax).
		b.ctx.State.AddShipCost(int64(chosenItem.Cost))

		// Refresh economy so subsequent fleet evals see updated upkeep/treasury.
		if econ, err := b.ctx.Client.GetEconomy(ctx); err == nil {
			b.ctx.State.UpdateEconomy(econ)
		}

		return ship
	}

	b.logger.Warn("could not buy ship at any shipyard port",
		zap.Int64("treasury", treasury),
		zap.Int64("upkeep_24h", upkeep*24),
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
