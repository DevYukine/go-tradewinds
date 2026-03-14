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

	// Build ship snapshots.
	allShips := make([]agent.ShipSnapshot, 0, len(state.Ships))
	for _, s := range state.Ships {
		allShips = append(allShips, shipToSnapshot(s))
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
		Ship:       shipToSnapshot(ship),
		AllShips:    allShips,
		Warehouses: warehouses,
		PriceCache: b.ctx.PriceCache.All(),
		Routes:     b.ctx.World.ToAgentRoutes(),
		Ports:      b.ctx.World.ToAgentPorts(),
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
		ships = append(ships, shipToSnapshot(s))
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
		if r.Status == 200 && r.Token != "" {
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
		if r.Status == 200 && r.Execution != nil {
			b.logger.Trade("sold cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution, *portID, "sell")
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
		if r.Status != 200 || r.Token == "" || r.Quote == nil {
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
		if r.Status == 200 && r.Execution != nil {
			b.logger.Trade("bought cargo",
				zap.String("action", r.Execution.Action),
				zap.Int("quantity", r.Execution.Quantity),
				zap.Int("unit_price", r.Execution.UnitPrice),
				zap.Int("total", r.Execution.TotalPrice),
			)
			b.recordTrade(r.Execution, *portID, "buy")
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
		b.logger.Warn("no direct route found",
			zap.String("from", ship.Ship.PortID.String()),
			zap.String("to", destPortID.String()),
		)
		return nil
	}

	_, err := b.ctx.Client.SendTransit(ctx, ship.Ship.ID, route.ID)
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
	// Sell (decommission) ships first to free up upkeep budget.
	for _, shipID := range decision.SellShips {
		if err := b.ctx.Client.SellShip(ctx, shipID); err != nil {
			b.logger.Warn("failed to decommission ship (game may not support ship sales)",
				zap.String("ship_id", shipID.String()),
				zap.Error(err),
			)
			continue
		}
		b.logger.Trade("decommissioned ship",
			zap.String("ship_id", shipID.String()),
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
func (b *baseStrategy) recordTrade(exec *api.TradeExecution, portID uuid.UUID, action string) {
	port := b.ctx.World.GetPort(portID)
	portName := portID.String()
	if port != nil {
		portName = port.Name
	}

	trade := db.TradeLog{
		CompanyID:  b.ctx.State.CompanyDBID(),
		Action:     action,
		PortID:     portID.String(),
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
}

// --- Snapshot Converters ---

func shipToSnapshot(s *bot.ShipState) agent.ShipSnapshot {
	cargo := make([]agent.CargoItem, len(s.Cargo))
	for i, c := range s.Cargo {
		cargo[i] = agent.CargoItem{
			GoodID:   c.GoodID,
			Quantity: c.Quantity,
		}
	}

	snap := agent.ShipSnapshot{
		ID:       s.Ship.ID,
		Name:     s.Ship.Name,
		Status:   s.Ship.Status,
		PortID:   s.Ship.PortID,
		Cargo:    cargo,
		ArrivesAt: s.Ship.ArrivingAt,
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
