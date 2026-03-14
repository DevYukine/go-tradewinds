package bot

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"go.uber.org/zap"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/api"
	"github.com/DevYukine/go-tradewinds/internal/db"
	"gorm.io/gorm"
)

const (
	// economyRefreshInterval is how often to poll the economy endpoint.
	economyRefreshInterval = 60 * time.Second

	// economyJitterMax is the maximum random offset added to the economy ticker
	// to prevent all companies from refreshing at the same instant.
	economyJitterMax = 10 * time.Second
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

	strategyCh chan Strategy // Receives new strategy assignments from the optimizer.
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
		strategyCh: make(chan Strategy, 1),
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

	r.logger.Info("company state initialized",
		zap.Int64("treasury", econ.Treasury),
		zap.Int("ships", len(ships)),
		zap.Int("warehouses", len(warehouses)),
	)

	return nil
}

// handleEvent processes an SSE event from the company event stream.
func (r *CompanyRunner) handleEvent(ctx context.Context, event api.SSEEvent) {
	switch event.Type {
	case "ship_docked":
		r.handleShipDocked(ctx, event.Data)
	case "ship_set_sail":
		r.handleShipSetSail(event.Data)
	case "ship_bought":
		r.handleShipBought(ctx, event.Data)
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

	// Refresh ship state from API.
	ship, err := r.client.GetShip(ctx, docked.ShipID)
	if err != nil {
		r.logger.Error("failed to refresh ship after docking", zap.Error(err))
		return
	}

	cargo, err := r.client.GetShipInventory(ctx, docked.ShipID)
	if err != nil {
		r.logger.Error("failed to fetch ship cargo after docking", zap.Error(err))
		return
	}

	r.state.mu.Lock()
	if ss, ok := r.state.Ships[docked.ShipID]; ok {
		ss.Ship = *ship
		ss.Cargo = cargo
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

	if err := r.strategy.OnShipArrival(ctx, shipState, port); err != nil {
		r.logger.Error("strategy OnShipArrival failed",
			zap.String("ship_id", docked.ShipID.String()),
			zap.Error(err),
		)
	}
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

// handleShipBought updates state when a new ship is purchased.
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
}

// handleTick runs periodic tasks: economy refresh, P&L snapshot, strategy tick.
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

	// Delegate to strategy for periodic work.
	if err := r.strategy.OnTick(ctx, r.state); err != nil {
		r.logger.Error("strategy OnTick failed", zap.Error(err))
	}
}

// recordPnLSnapshot writes a P&L snapshot to the database.
func (r *CompanyRunner) recordPnLSnapshot() {
	r.state.mu.RLock()
	snapshot := db.PnLSnapshot{
		CompanyID: r.dbRecord.ID,
		Treasury:  r.state.Treasury,
		ShipCount: len(r.state.Ships),
	}
	r.state.mu.RUnlock()

	if err := r.gormDB.Create(&snapshot).Error; err != nil {
		r.logger.Warn("failed to record P&L snapshot", zap.Error(err))
	}
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

// DBRecord returns the database record for this company.
func (r *CompanyRunner) DBRecord() *db.CompanyRecord {
	return r.dbRecord
}
