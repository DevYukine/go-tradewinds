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
	"github.com/DevYukine/go-tradewinds/internal/cache"
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

	// passengerShipCapacityThreshold is the maximum cargo capacity for a ship
	// to be considered a "passenger ship" eligible for passenger sniping.
	// Small/fast ships with low cargo capacity but high passenger capacity
	// are ideal for dedicated passenger runs.
	passengerShipCapacityThreshold = 60
)

// CompanyRunner manages the lifecycle of a single company: subscribing to
// events, running the strategy loop, and recording metrics.
type CompanyRunner struct {
	client      *api.Client
	gormDB      *gorm.DB
	redis       *cache.RedisCache
	world       *WorldCache
	priceCache  *PriceCache
	state       *CompanyState
	strategy    Strategy
	agent       agent.Agent
	logger      *CompanyLogger
	dbRecord    *db.CompanyRecord
	coordinator *Coordinator

	events          *EventBroadcaster
	strategyCh      chan strategySwap       // Receives new strategy assignments from the optimizer.
	dispatchCh      chan struct{}           // Receives forced dispatch signals from the optimizer.
	dispatchedShips map[uuid.UUID]time.Time // Tracks recently dispatched ships.
	bankrupt        bool                   // True when the game API reports the company as bankrupt.
}

// strategySwap bundles a new strategy with the reason it was selected.
type strategySwap struct {
	Strategy Strategy
	Reason   string
}

// NewCompanyRunner creates a runner for a single company.
func NewCompanyRunner(
	client *api.Client,
	gormDB *gorm.DB,
	redis *cache.RedisCache,
	world *WorldCache,
	priceCache *PriceCache,
	state *CompanyState,
	strategy Strategy,
	ag agent.Agent,
	logger *CompanyLogger,
	dbRecord *db.CompanyRecord,
	events *EventBroadcaster,
	coordinator *Coordinator,
) *CompanyRunner {
	return &CompanyRunner{
		client:          client,
		gormDB:          gormDB,
		redis:           redis,
		world:           world,
		priceCache:      priceCache,
		state:           state,
		strategy:        strategy,
		agent:           ag,
		logger:          logger,
		dbRecord:        dbRecord,
		coordinator:     coordinator,
		events:          events,
		strategyCh:      make(chan strategySwap, 1),
		dispatchCh:      make(chan struct{}, 1),
		dispatchedShips: make(map[uuid.UUID]time.Time),
	}
}

// Run is the main loop for a company. It blocks until the context is cancelled.
func (r *CompanyRunner) Run(ctx context.Context) {
	r.logger.Debug("company runner starting",
		zap.String("strategy", r.strategy.Name()),
	)

	if err := r.initState(ctx); err != nil {
		if api.IsBankrupt(err) {
			r.enterBankruptcy()
			// Continue to main loop — ticker will poll for recovery.
		} else {
			r.logger.Error("failed to initialize company state", zap.Error(err))
			return
		}
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

	// Subscribe to world events for passenger sniping.
	// When a passenger_request_created event arrives, we immediately dispatch
	// any idle ship docked at the origin port to grab the passenger.
	worldEventCh := make(chan api.SSEEvent, 64)
	worldStream := r.client.SubscribeWorldEvents(ctx, func(event api.SSEEvent) {
		// Only forward passenger events to avoid flooding the channel.
		if event.Type == "passenger_request_created" {
			select {
			case worldEventCh <- event:
			default:
				// Drop if full — passenger sniping is best-effort.
			}
		}
	})
	defer worldStream.Close()

	// Jittered economy ticker.
	jitter := time.Duration(rand.Int64N(int64(economyJitterMax)))
	ticker := time.NewTicker(economyRefreshInterval + jitter)
	defer ticker.Stop()

	r.logger.Debug("company runner ready, entering main loop")

	// Immediately dispatch any docked ships — don't wait for the first tick.
	if !r.bankrupt {
		r.dispatchIdleShips(ctx)
	}

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

		case event := <-worldEventCh:
			r.handleWorldEvent(ctx, event)

		case <-ticker.C:
			r.handleTick(ctx)

		case swap := <-r.strategyCh:
			r.swapStrategy(ctx, swap.Strategy, swap.Reason)

		case <-r.dispatchCh:
			r.dispatchIdleShips(ctx)
		}
	}
}

// SwapStrategy sends a new strategy to the runner's strategy channel.
// Called by the optimizer when rebalancing.
func (r *CompanyRunner) SwapStrategy(s Strategy, reason string) {
	select {
	case r.strategyCh <- strategySwap{Strategy: s, Reason: reason}:
	default:
		r.logger.Warn("strategy swap channel full, skipping")
	}
}

// ForceDispatch signals the runner to re-dispatch all idle docked ships.
// Called by the optimizer to recover inactive companies without swapping strategy.
func (r *CompanyRunner) ForceDispatch() {
	select {
	case r.dispatchCh <- struct{}{}:
	default:
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

	// Fetch cargo for each ship and restore cost tracking from Redis.
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

		// Restore cargo costs from Redis.
		if r.redis != nil {
			costs := r.redis.LoadCargoCosts(ctx, r.state.CompanyID.String(), ship.ID.String())
			if len(costs) > 0 {
				r.state.Lock()
				if ss, ok := r.state.Ships[ship.ID]; ok {
					for goodIDStr, cost := range costs {
						goodID, err := uuid.Parse(goodIDStr)
						if err != nil {
							continue
						}
						// Only restore cost for goods still on the ship.
						for _, c := range cargo {
							if c.GoodID == goodID && c.Quantity > 0 {
								ss.CargoCosts[goodID] = cost
								break
							}
						}
					}
				}
				r.state.Unlock()
			}
		}
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

	// Load or create tunable params from DB.
	// Use a two-step lookup to avoid duplicate key errors when multiple
	// runners start concurrently and race on FirstOrCreate.
	var params db.CompanyParams
	if err := r.gormDB.Where("company_id = ?", r.dbRecord.ID).First(&params).Error; err != nil {
		// Not found — create with defaults.
		params = db.CompanyParams{
			CompanyID:               r.dbRecord.ID,
			MinMarginPct:            0.05,
			PassengerWeight:         8.0,
			SpeculativeTradeEnabled: true,
			MarketEvalIntervalSec:   60,
			FleetEvalIntervalSec:    180,
			PassengerDestBonus:      8.0,
		}
		if err := r.gormDB.Create(&params).Error; err != nil {
			// Another runner may have created it — try loading again.
			if err2 := r.gormDB.Where("company_id = ?", r.dbRecord.ID).First(&params).Error; err2 != nil {
				r.logger.Warn("failed to load company params, using defaults", zap.Error(err2))
			}
		}
	} else {
		// Migrate existing params: if still using old conservative defaults,
		// update to aggressive values for better trading throughput.
		updates := map[string]any{}
		if params.MinMarginPct >= 0.15 {
			updates["min_margin_pct"] = 0.05
			params.MinMarginPct = 0.05
		}
		if params.PassengerWeight < 8.0 {
			updates["passenger_weight"] = 8.0
			params.PassengerWeight = 8.0
		}
		if !params.SpeculativeTradeEnabled {
			updates["speculative_trade_enabled"] = true
			params.SpeculativeTradeEnabled = true
		}
		if params.PassengerDestBonus < 8.0 {
			updates["passenger_dest_bonus"] = 8.0
			params.PassengerDestBonus = 8.0
		}
		if len(updates) > 0 {
			r.gormDB.Model(&params).Updates(updates)
			r.logger.Info("migrated company params to aggressive trading defaults",
				zap.Any("updates", updates),
			)
		}
	}
	r.state.mu.Lock()
	r.state.Params = &params
	r.state.mu.Unlock()

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

	// Mark state as fully initialized so API handlers can safely use live values.
	r.state.mu.Lock()
	r.state.Initialized = true
	r.state.mu.Unlock()

	// Sync treasury/reputation to DB immediately so they survive a restart
	// before the first recordPnLSnapshot tick.
	r.gormDB.Model(r.dbRecord).Updates(map[string]any{
		"treasury":   econ.Treasury,
		"reputation": econ.Reputation,
	})

	// Notify SSE listeners that fresh data is available.
	r.events.Emit(EventEconomyTick)

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

	r.logger.Debug("P&L counters seeded from database",
		zap.Int64("initial_treasury", r.state.InitialTreasury),
		zap.Int64("trade_rev", totalRev),
		zap.Int64("trade_costs", totalCosts),
		zap.Int64("passenger_rev", passengerRev),
	)
}

// handleEvent processes an SSE event from the company event stream.
func (r *CompanyRunner) handleEvent(ctx context.Context, event api.SSEEvent) {
	// While bankrupt, ignore events — all API calls would fail with 401.
	if r.bankrupt {
		return
	}

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
		r.logger.Debug("unhandled SSE event type", zap.String("type", event.Type))
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

	// Ignore events for ships no longer in state (e.g., sold ships whose
	// stale SSE docked events arrive after the sale completed).
	if r.state.GetShip(docked.ShipID) == nil {
		r.logger.Debug("ignoring ship_docked event for unknown/sold ship",
			zap.String("ship_id", docked.ShipID.String()),
		)
		return
	}

	// Refresh ship state from API.
	ship, err := r.client.GetShip(ctx, docked.ShipID)
	if err != nil {
		r.logger.Warn("failed to refresh ship after docking", zap.Error(err))
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
		r.logger.Warn("failed to fetch ship cargo after docking", zap.Error(err))
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
		r.logger.Warn("unknown port in ship_docked event",
			zap.String("port_id", docked.PortID.String()),
		)
		return
	}

	r.events.Emit(EventShipDocked)
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
	r.events.Emit(EventShipSailed)
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
	r.events.Emit(EventShipBought)

	// Trigger first trade for newly bought ship if it's docked.
	if ship.Status == "docked" && ship.PortID != nil {
		r.dispatchDockedShip(ctx, bought.ShipID)
	}
}

// handleWorldEvent processes an SSE event from the public world event stream.
func (r *CompanyRunner) handleWorldEvent(ctx context.Context, event api.SSEEvent) {
	if r.bankrupt {
		return
	}

	switch event.Type {
	case "passenger_request_created":
		r.handlePassengerCreated(ctx, event.Data)
	}
}

// handlePassengerCreated reacts to a new passenger group appearing at a port.
// Instead of going through the full trade decision pipeline (which costs 5+
// API calls and agent latency), we board the passenger directly and then
// dispatch the ship so the strategy can decide the best destination.
func (r *CompanyRunner) handlePassengerCreated(ctx context.Context, data json.RawMessage) {
	pax, err := api.ParsePassengerRequestCreatedEvent(data)
	if err != nil {
		r.logger.Debug("failed to parse passenger_request_created event", zap.Error(err))
		return
	}

	// Find idle docked ships at the passenger's origin port.
	// Prefer small/fast "passenger ships" (low cargo capacity) over large cargo ships.
	r.state.mu.RLock()
	var bestShip *ShipState
	bestIsPassengerShip := false
	for _, ss := range r.state.Ships {
		if ss.Ship.Status != "docked" || ss.Ship.PortID == nil {
			continue
		}
		if *ss.Ship.PortID != pax.OriginPortID {
			continue
		}
		// Check if this is a passenger-type ship (small cargo, fast).
		st := r.world.GetShipType(ss.Ship.ShipTypeID)
		isPassengerShip := st != nil && st.Capacity <= passengerShipCapacityThreshold && st.Passengers > 0

		// Prefer passenger ships over cargo ships. Among same type, prefer
		// ships that have been idle longer.
		if bestShip == nil ||
			(isPassengerShip && !bestIsPassengerShip) ||
			(isPassengerShip == bestIsPassengerShip && ss.IdleTicks > bestShip.IdleTicks) {
			bestShip = ss
			bestIsPassengerShip = isPassengerShip
		}
	}
	r.state.mu.RUnlock()

	if bestShip == nil {
		return
	}

	// Skip if this ship was recently dispatched (avoid re-dispatch spam).
	if lastDispatched, ok := r.dispatchedShips[bestShip.Ship.ID]; ok {
		if time.Since(lastDispatched) < 2*time.Second {
			return
		}
	}

	port := r.world.GetPort(pax.OriginPortID)
	if port == nil {
		return
	}

	// Check coordinator to avoid multiple companies chasing same passenger.
	if r.coordinator != nil && !r.coordinator.ClaimPassenger(pax.ID, r.state.CompanyID.String()) {
		return
	}

	// Board immediately — this is a race against other players.
	// Skip the full trade pipeline (ensurePortPrices, agent decision, etc.)
	// and call the board API directly. One API call instead of 6+.
	boarded, err := r.client.BoardPassenger(ctx, pax.ID, bestShip.Ship.ID)
	if err != nil {
		// Expected when another player beats us — not an error worth warning about.
		r.logger.Debug("passenger snipe failed (likely taken by competitor)",
			zap.String("passenger_id", pax.ID.String()),
			zap.String("ship", bestShip.Ship.Name),
			zap.Error(err),
		)
		return
	}

	destName := pax.DestinationPortID.String()
	if p := r.world.GetPort(pax.DestinationPortID); p != nil {
		destName = p.Name
	}
	originName := port.Name

	r.logger.Trade("sniped passenger",
		zap.String("passenger_id", pax.ID.String()),
		zap.Int("bid", boarded.Bid),
		zap.Int("count", boarded.Count),
		zap.String("ship", bestShip.Ship.Name),
		zap.String("origin", originName),
		zap.String("destination", destName),
	)

	// Log to database.
	r.gormDB.Create(&db.PassengerLog{
		CompanyID:           r.dbRecord.ID,
		PassengerID:         pax.ID.String(),
		Count:               boarded.Count,
		Bid:                 boarded.Bid,
		OriginPortID:        pax.OriginPortID.String(),
		OriginPortName:      originName,
		DestinationPortID:   pax.DestinationPortID.String(),
		DestinationPortName: destName,
		ShipID:              bestShip.Ship.ID.String(),
		ShipName:            bestShip.Ship.Name,
		Strategy:            r.strategy.Name(),
		AgentName:           r.agent.Name(),
	})

	// Now dispatch the ship so the strategy can sell cargo, pick up more
	// passengers, and choose the best destination (which should now favor
	// the boarded passenger's destination).
	r.dispatchedShips[bestShip.Ship.ID] = time.Now()
	r.dispatchWithRetry(ctx, bestShip, port)
}

// handleTick runs periodic tasks: economy refresh, P&L snapshot, strategy tick,
// and dispatches idle docked ships that haven't been sent on a trade run.
func (r *CompanyRunner) handleTick(ctx context.Context) {
	// Refresh economy.
	econ, err := r.client.GetEconomy(ctx)
	if err != nil {
		if api.IsBankrupt(err) {
			r.enterBankruptcy()
			return
		}
		r.logger.Warn("economy refresh failed", zap.Error(err))
	} else {
		// If we were bankrupt and the economy call succeeded, the admin
		// has bailed us out — but only resume if treasury is enough to
		// cover at least 2 upkeep cycles. Otherwise we'll just go bankrupt
		// again on the first trade attempt.
		if r.bankrupt {
			minTreasury := r.state.TotalUpkeep * 2
			if minTreasury < 1000 {
				minTreasury = 1000
			}
			if econ.Treasury >= minTreasury {
				r.exitBankruptcy(econ)
			}
		}
		r.state.UpdateEconomy(econ)
		r.events.Emit(EventEconomyTick)
	}

	// While bankrupt, only poll for recovery — skip all trading activity.
	if r.bankrupt {
		return
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
		r.logger.Warn("strategy OnTick failed", zap.Error(err))
	}

	// Dispatch any idle docked ships that SSE events might have missed.
	r.dispatchIdleShips(ctx)
}

// enterBankruptcy marks the company as bankrupt and stops all trading.
// The runner continues polling GetEconomy to detect an admin bailout.
func (r *CompanyRunner) enterBankruptcy() {
	if r.bankrupt {
		return
	}

	r.bankrupt = true
	r.logger.Error("BANKRUPTCY DETECTED — company has gone bankrupt, all trading suspended",
		zap.String("company", r.dbRecord.Name),
		zap.String("ticker", r.dbRecord.Ticker),
		zap.Int64("last_known_treasury", r.state.Treasury),
	)
	r.logger.Warn("admin must inject funds via game admin panel to resume operations")

	// Update DB status so the dashboard shows bankruptcy.
	r.gormDB.Model(r.dbRecord).Update("status", "bankrupt")
	r.events.Emit(EventEconomyTick)
}

// exitBankruptcy restores a bankrupt company to normal operations after
// an admin bailout (treasury injection).
func (r *CompanyRunner) exitBankruptcy(econ *api.CompanyEconomy) {
	r.bankrupt = false
	r.logger.Info("BANKRUPTCY RECOVERED — admin bailout detected, resuming trading",
		zap.String("company", r.dbRecord.Name),
		zap.Int64("new_treasury", econ.Treasury),
	)

	// Restore DB status.
	r.gormDB.Model(r.dbRecord).Update("status", "running")
	r.state.UpdateEconomy(econ)
	r.events.Emit(EventEconomyTick)
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

	r.logger.Debug("dispatching newly purchased ship",
		zap.String("ship", shipState.Ship.Name),
		zap.String("port", port.Name),
	)

	r.dispatchWithRetry(ctx, shipState, port)
}

// dispatchWithRetry attempts to dispatch a ship up to 5 times with exponential backoff.
func (r *CompanyRunner) dispatchWithRetry(ctx context.Context, ship *ShipState, port *api.Port) {
	const maxAttempts = 5
	backoff := 2 * time.Second

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := r.strategy.OnShipArrival(ctx, ship, port); err == nil {
			r.dispatchedShips[ship.Ship.ID] = time.Now()
			return
		} else {
			// If the error is a bankruptcy signal, enter bankruptcy immediately
			// and stop retrying — all further API calls will also fail.
			if api.IsBankrupt(err) {
				r.enterBankruptcy()
				return
			}
			r.logger.Warn("dispatch attempt failed",
				zap.String("ship_id", ship.Ship.ID.String()),
				zap.Int("attempt", attempt+1),
				zap.Duration("next_backoff", backoff),
				zap.Error(err),
			)
		}
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2 // 2s → 4s → 8s → 16s
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
func (r *CompanyRunner) swapStrategy(ctx context.Context, newStrategy Strategy, reason string) {
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
		Events:     r.events,
		DB:         r.gormDB,
		Redis:      r.redis,
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

	// Log strategy change event.
	r.gormDB.Create(&db.StrategyChangeLog{
		CompanyID:    r.dbRecord.ID,
		FromStrategy: oldName,
		ToStrategy:   newStrategy.Name(),
		Reason:       reason,
	})

	// Update DB record.
	r.gormDB.Model(r.dbRecord).Update("strategy", newStrategy.Name())
}

// Logger returns the company logger for external consumers (e.g., SSE streaming).
func (r *CompanyRunner) Logger() *CompanyLogger {
	return r.logger
}

// Events returns the event broadcaster for external consumers (e.g., SSE streaming).
func (r *CompanyRunner) Events() *EventBroadcaster {
	return r.events
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

// AgentName returns the name of the agent driving this company.
func (r *CompanyRunner) AgentName() string {
	return r.agent.Name()
}

// State returns the in-memory company state for external consumers (e.g., API server).
func (r *CompanyRunner) State() *CompanyState {
	return r.state
}

// Client returns the API client for this company (e.g., for proxying game API calls).
func (r *CompanyRunner) Client() *api.Client {
	return r.client
}
