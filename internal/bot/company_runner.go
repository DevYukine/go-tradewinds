package bot

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"go.uber.org/zap"

	"github.com/google/uuid"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"gorm.io/gorm"
)

const (
	// economyRefreshInterval is how often to poll the economy endpoint and
	// dispatch idle ships. Shorter intervals ensure ships don't sit idle if
	// SSE events are missed.
	economyRefreshInterval = 30 * time.Second

	// economyJitterMax is the maximum random offset added to the economy ticker
	// to prevent all companies from refreshing at the same instant.
	economyJitterMax = 15 * time.Second
)

// CompanyRunner manages the lifecycle of a single company: subscribing to
// events, running the strategy loop, and recording metrics.
type CompanyRunner struct {
	client     *api.Client
	gormDB     *gorm.DB
	world      *WorldCache
	priceCache *PriceCache
	state      *CompanyState
	strategy   Strategy
	agent      agent.Agent
	logger     *CompanyLogger
	dbRecord   *db.CompanyRecord

	strategyCh      chan Strategy          // Receives new strategy assignments from the optimizer.
	dispatchedShips map[uuid.UUID]time.Time // Tracks recently dispatched ships.
}

// NewCompanyRunner creates a runner for a single company.
func NewCompanyRunner(
	client *api.Client,
	gormDB *gorm.DB,
	world *WorldCache,
	priceCache *PriceCache,
	state *CompanyState,
	strategy Strategy,
	ag agent.Agent,
	logger *CompanyLogger,
	dbRecord *db.CompanyRecord,
) *CompanyRunner {
	return &CompanyRunner{
		client:     client,
		gormDB:     gormDB,
		world:      world,
		priceCache: priceCache,
		state:      state,
		strategy:   strategy,
		agent:      ag,
		logger:     logger,
		dbRecord:   dbRecord,
		strategyCh:      make(chan Strategy, 1),
		dispatchedShips: make(map[uuid.UUID]time.Time),
	}
}

// Run is the main loop for a company. It blocks until the context is cancelled.
func (r *CompanyRunner) Run(ctx context.Context) {
	r.logger.Info("company runner starting",
		zap.String("strategy", r.strategy.Name()),
	)

	if err := r.initState(ctx); err != nil {
		r.logger.Error("failed to initialize company state", zap.Error(err))
		return
	}

	// Subscribe to company SSE events (long-lived, no rate limit cost).
	eventCh := make(chan api.SSEEvent, 32)
	stream := r.client.SubscribeCompanyEvents(ctx, func(event api.SSEEvent) {
		select {
		case eventCh <- event:
		default:
			r.logger.Warn("SSE event channel full, dropping event",
				zap.String("type", event.Type),
			)
		}
	})
	defer stream.Close()

	// Jittered economy ticker.
	jitter := time.Duration(rand.Int64N(int64(economyJitterMax)))
	ticker := time.NewTicker(economyRefreshInterval + jitter)
	defer ticker.Stop()

	r.logger.Info("company runner ready, entering main loop")

	// Immediately dispatch any docked ships — don't wait for the first tick.
	r.dispatchIdleShips(ctx)

	for {
		select {
		case <-ctx.Done():
			r.logger.Info("company runner shutting down")
			if err := r.strategy.Shutdown(); err != nil {
				r.logger.Error("strategy shutdown error", zap.Error(err))
			}
			return

		case event := <-eventCh:
			r.handleEvent(ctx, event)

		case <-ticker.C:
			r.handleTick(ctx)

		case newStrategy := <-r.strategyCh:
			r.swapStrategy(ctx, newStrategy)
		}
	}
}

// SwapStrategy sends a new strategy to the runner's strategy channel.
// Called by the optimizer when rebalancing.
func (r *CompanyRunner) SwapStrategy(s Strategy) {
	select {
	case r.strategyCh <- s:
	default:
		r.logger.Warn("strategy swap channel full, skipping")
	}
}

// initState fetches the company's current ships, warehouses, and economy
// from the API and populates the in-memory state.
func (r *CompanyRunner) initState(ctx context.Context) error {
	// Fetch economy.
	econ, err := r.client.GetEconomy(ctx)
	if err != nil {
		return err
	}
	r.state.UpdateEconomy(econ)

	// Fetch ships.
	ships, err := r.client.ListShips(ctx)
	if err != nil {
		return err
	}
	r.state.UpdateShips(ships)

	// Rename any ships that don't have an FFXIV-themed name yet.
	for _, ship := range ships {
		if IsFFXIVName(ship.Name) {
			continue
		}
		name := GenerateShipName()
		renamed, err := r.client.RenameShip(ctx, ship.ID, name)
		if err != nil {
			r.logger.Warn("failed to rename legacy ship",
				zap.String("ship_id", ship.ID.String()),
				zap.String("old_name", ship.Name),
				zap.Error(err),
			)
			continue
		}
		r.logger.Info("renamed legacy ship",
			zap.String("old_name", ship.Name),
			zap.String("new_name", renamed.Name),
		)
		r.state.Lock()
		if ss := r.state.Ships[ship.ID]; ss != nil {
			ss.Ship.Name = renamed.Name
		}
		r.state.Unlock()
	}

	// Fetch cargo for each ship.
	for _, ship := range ships {
		cargo, err := r.client.GetShipInventory(ctx, ship.ID)
		if err != nil {
			r.logger.Warn("failed to fetch ship cargo",
				zap.String("ship_id", ship.ID.String()),
				zap.Error(err),
			)
			continue
		}
		r.state.SetShipCargo(ship.ID, cargo)
	}

	// Fetch boarded passenger counts for ships with passenger capacity.
	if r.world != nil {
		for _, ship := range ships {
			st := r.world.GetShipType(ship.ShipTypeID)
			if st == nil || st.Passengers <= 0 {
				continue
			}
			boarded, err := r.client.ListPassengers(ctx, api.PassengerFilters{
				Status: "boarded",
				ShipID: ship.ID.String(),
			})
			if err != nil {
				r.logger.Debug("failed to fetch boarded passengers",
					zap.String("ship_id", ship.ID.String()),
					zap.Error(err),
				)
				continue
			}
			count := 0
			for _, p := range boarded {
				count += p.Count
			}
			r.state.SetShipPassengers(ship.ID, count)
		}
	}

	// Fetch warehouses.
	warehouses, err := r.client.ListWarehouses(ctx)
	if err != nil {
		return err
	}
	r.state.UpdateWarehouses(warehouses)

	// Fetch warehouse inventories.
	for _, wh := range warehouses {
		inv, err := r.client.GetWarehouseInventory(ctx, wh.ID)
		if err != nil {
			r.logger.Warn("failed to fetch warehouse inventory",
				zap.String("warehouse_id", wh.ID.String()),
				zap.Error(err),
			)
			continue
		}
		r.state.SetWarehouseInventory(wh.ID, inv)
	}

	// Strategy is already initialized by the factory in the manager.
	// No need to re-init here.

	// Fetch active P2P orders.
	orders, err := r.client.ListOrders(ctx, api.OrderFilters{})
	if err != nil {
		r.logger.Warn("failed to fetch initial orders", zap.Error(err))
	} else {
		r.state.UpdateOrders(orders)
	}

	// Capture initial treasury before seeding P&L counters.
	r.state.mu.Lock()
	r.state.InitialTreasury = econ.Treasury
	r.state.mu.Unlock()

	// Seed cumulative P&L counters from DB so incremental tracking is accurate.
	r.seedPnLCounters()

	r.logger.Info("company state initialized",
		zap.Int64("treasury", econ.Treasury),
		zap.Int("ships", len(ships)),
		zap.Int("warehouses", len(warehouses)),
	)

	return nil
}

// seedPnLCounters loads cumulative trade/passenger totals from the database
// once at startup so recordPnLSnapshot can track them incrementally.
// Also seeds InitialTreasury from the earliest P&L snapshot (or current
// treasury if no history exists).
func (r *CompanyRunner) seedPnLCounters() {
	var totalRev, totalCosts, passengerRev int64

	r.gormDB.Model(&db.TradeLog{}).
		Where("company_id = ? AND action = ?", r.dbRecord.ID, "sell").
		Select("COALESCE(SUM(total_price), 0)").Scan(&totalRev)

	r.gormDB.Model(&db.TradeLog{}).
		Where("company_id = ? AND action = ?", r.dbRecord.ID, "buy").
		Select("COALESCE(SUM(total_price), 0)").Scan(&totalCosts)

	r.gormDB.Model(&db.PassengerLog{}).
		Where("company_id = ?", r.dbRecord.ID).
		Select("COALESCE(SUM(bid), 0)").Scan(&passengerRev)

	// Use the earliest P&L snapshot's treasury as the initial value so
	// lifetime P&L survives restarts. Fall back to current treasury.
	var firstSnapshot db.PnLSnapshot
	err := r.gormDB.Where("company_id = ?", r.dbRecord.ID).
		Order("id ASC").First(&firstSnapshot).Error

	r.state.mu.Lock()
	r.state.CumTradeRev = totalRev
	r.state.CumTradeCosts = totalCosts
	r.state.CumPassengerRev = passengerRev
	if err == nil {
		r.state.InitialTreasury = firstSnapshot.Treasury
	}
	// else: keep the value set from econ.Treasury in initState
	r.state.pnlInitialized = true
	r.state.mu.Unlock()

	r.logger.Info("P&L counters seeded from database",
		zap.Int64("initial_treasury", r.state.InitialTreasury),
		zap.Int64("trade_rev", totalRev),
		zap.Int64("trade_costs", totalCosts),
		zap.Int64("passenger_rev", passengerRev),
	)
}

// handleEvent processes an SSE event from the company event stream.
func (r *CompanyRunner) handleEvent(ctx context.Context, event api.SSEEvent) {
	switch event.Type {
	case "ship_docked":
		r.handleShipDocked(ctx, event.Data)
	case "ship_set_sail", "ship_transit_started":
		r.handleShipSetSail(event.Data)
	case "ship_bought":
		r.handleShipBought(ctx, event.Data)
	case "ledger_entry":
		// Treasury is refreshed periodically via economy poll; no action needed.
	default:
		r.logger.Warn("unhandled SSE event type", zap.String("type", event.Type))
	}
}

// handleShipDocked processes a ship arrival event and triggers the strategy.
func (r *CompanyRunner) handleShipDocked(ctx context.Context, data json.RawMessage) {
	docked, err := api.ParseShipDockedEvent(data)
	if err != nil {
		r.logger.Error("failed to parse ship_docked event", zap.Error(err))
		return
	}

	r.logger.Event("ship docked",
		zap.String("ship_id", docked.ShipID.String()),
		zap.String("port_id", docked.PortID.String()),
	)

	// Refresh ship state from API.
	ship, err := r.client.GetShip(ctx, docked.ShipID)
	if err != nil {
		r.logger.Error("failed to refresh ship after docking", zap.Error(err))
		// Still mark the ship as docked from the event data so it doesn't
		// stay stuck in "traveling" status forever.
		r.state.mu.Lock()
		if ss, ok := r.state.Ships[docked.ShipID]; ok {
			ss.Ship.Status = "docked"
			ss.Ship.PortID = &docked.PortID
			ss.Ship.RouteID = nil
			ss.Ship.ArrivingAt = nil
			ss.PassengerCount = 0
		}
		r.state.mu.Unlock()
		return
	}

	cargo, err := r.client.GetShipInventory(ctx, docked.ShipID)
	if err != nil {
		r.logger.Error("failed to fetch ship cargo after docking", zap.Error(err))
		// Update status from the full ship response even without cargo.
		r.state.mu.Lock()
		if ss, ok := r.state.Ships[docked.ShipID]; ok {
			ss.Ship = *ship
			ss.PassengerCount = 0
		} else {
			r.state.Ships[docked.ShipID] = &ShipState{Ship: *ship}
		}
		r.state.mu.Unlock()
		return
	}

	r.state.mu.Lock()
	if ss, ok := r.state.Ships[docked.ShipID]; ok {
		ss.Ship = *ship
		ss.Cargo = cargo
		ss.PassengerCount = 0
	} else {
		r.state.Ships[docked.ShipID] = &ShipState{Ship: *ship, Cargo: cargo}
	}
	r.state.mu.Unlock()

	shipState := r.state.GetShip(docked.ShipID)
	if shipState == nil {
		return
	}

	port := r.world.GetPort(docked.PortID)
	if port == nil {
		r.logger.Error("unknown port in ship_docked event",
			zap.String("port_id", docked.PortID.String()),
		)
		return
	}

	r.dispatchWithRetry(ctx, shipState, port)
}

// handleShipSetSail updates state when a ship departs.
func (r *CompanyRunner) handleShipSetSail(data json.RawMessage) {
	sailed, err := api.ParseShipSetSailEvent(data)
	if err != nil {
		r.logger.Error("failed to parse ship_set_sail event", zap.Error(err))
		return
	}

	r.logger.Event("ship set sail",
		zap.String("ship_id", sailed.ShipID.String()),
		zap.String("route_id", sailed.RouteID.String()),
	)

	r.state.mu.Lock()
	if ss, ok := r.state.Ships[sailed.ShipID]; ok {
		ss.Ship.Status = "traveling"
		ss.Ship.RouteID = &sailed.RouteID
		ss.Ship.PortID = nil
	}
	r.state.mu.Unlock()
}

// handleShipBought updates state when a new ship is purchased and triggers
// the first trade if the ship is docked.
func (r *CompanyRunner) handleShipBought(ctx context.Context, data json.RawMessage) {
	bought, err := api.ParseShipBoughtEvent(data)
	if err != nil {
		r.logger.Error("failed to parse ship_bought event", zap.Error(err))
		return
	}

	r.logger.Event("ship bought",
		zap.String("ship_id", bought.ShipID.String()),
		zap.String("ship_type_id", bought.ShipTypeID.String()),
	)

	// Fetch the new ship's details.
	ship, err := r.client.GetShip(ctx, bought.ShipID)
	if err != nil {
		r.logger.Error("failed to fetch new ship details", zap.Error(err))
		return
	}

	r.state.mu.Lock()
	r.state.Ships[bought.ShipID] = &ShipState{Ship: *ship}
	r.state.mu.Unlock()

	// Trigger first trade for newly bought ship if it's docked.
	if ship.Status == "docked" && ship.PortID != nil {
		r.dispatchDockedShip(ctx, bought.ShipID)
	}
}

// handleTick runs periodic tasks: economy refresh, P&L snapshot, strategy tick,
// and dispatches idle docked ships that haven't been sent on a trade run.
func (r *CompanyRunner) handleTick(ctx context.Context) {
	// Refresh economy.
	econ, err := r.client.GetEconomy(ctx)
	if err != nil {
		r.logger.Warn("economy refresh failed", zap.Error(err))
	} else {
		r.state.UpdateEconomy(econ)
	}

	// Record P&L snapshot.
	r.recordPnLSnapshot()

	// Refresh active P2P orders & cancel expired ones.
	r.refreshOrders(ctx)

	// Refresh any ships stuck in "traveling" past their arrival time.
	// This happens when a ship_docked SSE event is lost or GetShip fails.
	r.refreshStaleShips(ctx)

	// Delegate to strategy for periodic work (fleet evals, market evals).
	if err := r.strategy.OnTick(ctx, r.state); err != nil {
		r.logger.Error("strategy OnTick failed", zap.Error(err))
	}

	// Dispatch any idle docked ships that SSE events might have missed.
	r.dispatchIdleShips(ctx)
}

// refreshStaleShips detects ships stuck in "traveling" status past their
// ArrivingAt time and refreshes them from the API. This recovers from lost
// ship_docked SSE events or failed GetShip calls.
func (r *CompanyRunner) refreshStaleShips(ctx context.Context) {
	now := time.Now()

	r.state.mu.RLock()
	var stale []uuid.UUID
	for id, ss := range r.state.Ships {
		if ss.Ship.Status == "traveling" && ss.Ship.ArrivingAt != nil && now.After(ss.Ship.ArrivingAt.Add(10*time.Second)) {
			stale = append(stale, id)
		}
	}
	r.state.mu.RUnlock()

	for _, shipID := range stale {
		ship, err := r.client.GetShip(ctx, shipID)
		if err != nil {
			r.logger.Debug("failed to refresh stale ship", zap.String("ship_id", shipID.String()), zap.Error(err))
			continue
		}

		r.state.mu.Lock()
		if ss, ok := r.state.Ships[shipID]; ok {
			ss.Ship = *ship
		}
		r.state.mu.Unlock()

		if ship.Status == "docked" && ship.PortID != nil {
			r.logger.Info("recovered stale ship",
				zap.String("ship", ship.Name),
				zap.String("ship_id", shipID.String()),
			)
		}
	}
}

// dispatchIdleShips checks for docked ships and triggers OnShipArrival
// to ensure ships don't sit idle (e.g., after purchase or missed SSE events).
func (r *CompanyRunner) dispatchIdleShips(ctx context.Context) {
	// Clean old entries.
	now := time.Now()
	for id, t := range r.dispatchedShips {
		if now.Sub(t) > 60*time.Second {
			delete(r.dispatchedShips, id)
		}
	}

	docked := r.state.DockedShips()
	for _, ship := range docked {
		if ship.Ship.PortID == nil {
			continue
		}

		// Skip recently dispatched ships.
		if lastDispatched, ok := r.dispatchedShips[ship.Ship.ID]; ok {
			if now.Sub(lastDispatched) < 60*time.Second {
				continue
			}
		}

		port := r.world.GetPort(*ship.Ship.PortID)
		if port == nil {
			continue
		}

		r.logger.Debug("dispatching idle docked ship",
			zap.String("ship", ship.Ship.Name),
			zap.String("port", port.Name),
		)

		r.dispatchWithRetry(ctx, ship, port)
	}
}

// dispatchDockedShip triggers a trade evaluation for a single docked ship.
func (r *CompanyRunner) dispatchDockedShip(ctx context.Context, shipID uuid.UUID) {
	shipState := r.state.GetShip(shipID)
	if shipState == nil || shipState.Ship.PortID == nil {
		return
	}

	port := r.world.GetPort(*shipState.Ship.PortID)
	if port == nil {
		return
	}

	r.logger.Info("dispatching newly purchased ship",
		zap.String("ship", shipState.Ship.Name),
		zap.String("port", port.Name),
	)

	r.dispatchWithRetry(ctx, shipState, port)
}

// dispatchWithRetry attempts to dispatch a ship up to 3 times with backoff.
func (r *CompanyRunner) dispatchWithRetry(ctx context.Context, ship *ShipState, port *api.Port) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := r.strategy.OnShipArrival(ctx, ship, port); err == nil {
			r.dispatchedShips[ship.Ship.ID] = time.Now()
			return
		} else {
			r.logger.Warn("dispatch attempt failed",
				zap.String("ship_id", ship.Ship.ID.String()),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
	r.logger.Error("all dispatch attempts failed",
		zap.String("ship_id", ship.Ship.ID.String()),
	)
}

// recordPnLSnapshot writes a P&L snapshot to the database.
func (r *CompanyRunner) recordPnLSnapshot() {
	r.state.mu.RLock()
	treasury := r.state.Treasury
	reputation := r.state.Reputation
	shipCount := len(r.state.Ships)
	tradeRev := r.state.CumTradeRev
	passengerRev := r.state.CumPassengerRev
	initialTreasury := r.state.InitialTreasury
	shipCosts := r.state.CumShipCosts

	// Compute capacity utilization.
	var totalCargo, totalCapacity int
	for _, ship := range r.state.Ships {
		for _, c := range ship.Cargo {
			totalCargo += c.Quantity
		}
		if st := r.world.GetShipType(ship.Ship.ShipTypeID); st != nil {
			totalCapacity += st.Capacity
		}
	}
	r.state.mu.RUnlock()

	avgCapUtil := 0.0
	if totalCapacity > 0 {
		avgCapUtil = float64(totalCargo) / float64(totalCapacity)
	}

	// Total revenue is trade sells + passenger boardings.
	totalRev := tradeRev + passengerRev

	// Derive total costs from the treasury identity:
	//   current_treasury = initial_treasury + all_revenue - all_costs
	// This captures trade buys, ship purchases, warehouse purchases, upkeep, and taxes.
	totalCosts := initialTreasury + totalRev - treasury
	if totalCosts < 0 {
		totalCosts = 0 // Guard against timing skew.
	}

	// Net P&L is what the company has gained/lost since bot start.
	netPnL := treasury - initialTreasury

	snapshot := db.PnLSnapshot{
		CompanyID:       r.dbRecord.ID,
		Treasury:        treasury,
		TotalRev:        totalRev,
		TotalCosts:      totalCosts,
		PassengerRev:    passengerRev,
		NetPnL:          netPnL,
		ShipCount:       shipCount,
		ShipCosts:       shipCosts,
		AvgCapacityUtil: avgCapUtil,
	}

	if err := r.gormDB.Create(&snapshot).Error; err != nil {
		r.logger.Warn("failed to record P&L snapshot", zap.Error(err))
	}

	// Keep the CompanyRecord in sync so /api/companies returns fresh values.
	r.gormDB.Model(r.dbRecord).Updates(map[string]any{
		"treasury":   treasury,
		"reputation": reputation,
	})
}

// swapStrategy replaces the current strategy with a new one (from the optimizer).
func (r *CompanyRunner) swapStrategy(ctx context.Context, newStrategy Strategy) {
	oldName := r.strategy.Name()

	if err := r.strategy.Shutdown(); err != nil {
		r.logger.Warn("old strategy shutdown error", zap.Error(err))
	}

	stratCtx := StrategyContext{
		Client:     r.client,
		State:      r.state,
		World:      r.world,
		PriceCache: r.priceCache,
		Agent:      r.agent,
		Logger:     r.logger,
		DB:         r.gormDB,
	}

	if err := newStrategy.Init(stratCtx); err != nil {
		r.logger.Error("new strategy init failed, keeping old strategy",
			zap.String("new_strategy", newStrategy.Name()),
			zap.Error(err),
		)
		// Re-init old strategy.
		_ = r.strategy.Init(stratCtx)
		return
	}

	r.strategy = newStrategy

	r.logger.Info("strategy swapped",
		zap.String("old", oldName),
		zap.String("new", newStrategy.Name()),
	)

	// Update DB record.
	r.gormDB.Model(r.dbRecord).Update("strategy", newStrategy.Name())
}

// Logger returns the company logger for external consumers (e.g., SSE streaming).
func (r *CompanyRunner) Logger() *CompanyLogger {
	return r.logger
}

// refreshOrders fetches active orders, updates state, and auto-cancels expired ones.
func (r *CompanyRunner) refreshOrders(ctx context.Context) {
	orders, err := r.client.ListOrders(ctx, api.OrderFilters{})
	if err != nil {
		r.logger.Debug("order refresh failed", zap.Error(err))
		return
	}
	r.state.UpdateOrders(orders)

	now := time.Now()
	for _, o := range orders {
		if o.ExpiresAt != nil && now.After(*o.ExpiresAt) {
			if err := r.client.CancelOrder(ctx, o.ID); err != nil {
				r.logger.Debug("failed to cancel expired order",
					zap.String("order_id", o.ID.String()),
					zap.Error(err),
				)
			} else {
				r.state.RemoveOrder(o.ID)
				r.logger.Info("cancelled expired P2P order",
					zap.String("order_id", o.ID.String()),
				)
			}
		}
	}
}

// DBRecord returns the database record for this company.
func (r *CompanyRunner) DBRecord() *db.CompanyRecord {
	return r.dbRecord
}

// State returns the in-memory company state for external consumers (e.g., API server).
func (r *CompanyRunner) State() *CompanyState {
	return r.state
}

// Client returns the API client for this company (e.g., for proxying game API calls).
func (r *CompanyRunner) Client() *api.Client {
	return r.client
}
