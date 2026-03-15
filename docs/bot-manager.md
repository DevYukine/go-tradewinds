# Bot Manager & Company Runner

## Manager (`internal/bot/manager.go`)

Orchestrates all company runners with shared resources.

### Shared Resources
- **baseClient** — Single HTTP client with JWT auth token
- **rateLimiter** — Fixed window (60s reset), 900 req/min, priority-based
- **worldData** — `WorldCache` with ports, goods, routes, ship types
- **priceCache** — `PriceCache` with NPC buy/sell prices (updated by scanner)
- **coordinator** — `Coordinator` for route deconfliction and passenger claiming across companies
- **agent** — AI decision agent (heuristic/LLM/composite)
- **registry** — Strategy factories

### Startup Flow
1. Load config, authenticate player
2. Fetch world data (ports, goods, routes, ship types)
3. Create/resume companies from DB using `StrategyAllocation` config
4. Spawn `CompanyRunner` goroutine per company
5. Spawn `PriceScanner` goroutine (shared)
6. Spawn world data refresher (every 5 min, discovers new ports/routes/goods)

### Public Methods
- `AddCompany(strategy)` — Dynamically create and start a new company
- `PauseCompany(gameID)` — Stop runner, mark paused in DB
- `GetRunner(gameID)` — Lookup runner by game ID
- `CompanyCount()` — Active company count
- `RateLimiter()`, `BaseClient()`, `WorldData()`, `PriceCache()` — Accessors

## CompanyRunner (`internal/bot/company_runner.go`)

Manages the lifecycle of a single company. Receives the shared `Coordinator` for passenger claim coordination.

### Main Loop (`Run`)
```
select {
  case <-ctx.Done():        → shutdown
  case event := <-eventCh:  → handleEvent (SSE)
  case <-ticker.C:          → handleTick (30s + jitter)
  case newStrategy := <-ch: → swapStrategy
}
```

### SSE Event Handling
- **ship_docked** → Refresh ship state from API, call `dispatchWithRetry`
- **ship_set_sail** → Update ship status to "traveling"
- **ship_bought** → Add ship to state, dispatch if docked

### Tick Handler (`handleTick`)
1. Refresh economy (treasury, upkeep)
2. Record P&L snapshot (treasury, revenue, costs, capacity utilization)
3. Call `strategy.OnTick()` (fleet evals, market evals)
4. Dispatch idle docked ships

### Dispatch Retry (Phase 1f)
- Retries `OnShipArrival` up to 3 times with 3s backoff
- Respects context cancellation between retries
- Marks ship in `dispatchedShips` map on success

### Ship Dedup (Phase 1g)
- `dispatchedShips map[uuid.UUID]time.Time` tracks recently dispatched ships
- `dispatchIdleShips` skips ships dispatched within last 60s
- Entries older than 60s are cleaned on each tick

### P&L Snapshots
Records to `db.PnLSnapshot`:
- Treasury, revenue, costs, net P&L, ship count
- `AvgCapacityUtil` — `sum(cargo) / sum(capacity)` across all ships

## CompanyState (`internal/bot/state.go`)

Thread-safe in-memory state per company.

### Fields
- `CompanyID`, `Treasury`, `Reputation`, `TotalUpkeep`
- `Ships map[uuid.UUID]*ShipState` — Ship + cargo
- `Warehouses map[uuid.UUID]*WarehouseState` — Warehouse + inventory
- `mu sync.RWMutex`

### Key Methods
- `UpdateEconomy(econ)` — Refresh financials
- `UpdateShips(ships)` — Replace ship roster
- `DockedShips()` — All ships with status "docked"
- `TreasuryFloor()` — `TotalUpkeep * 2`
- `ShipState.UsedCapacity()` — Sum of cargo quantities
- `RemoveWarehouse(id)` — Remove a demolished warehouse from state

## Scaler (`internal/bot/scaler.go`)

Calculates safe company counts based on rate limit budget.

- `perCompanyCostPerMinute = 5.0` estimated API calls
- `sharedOverheadPerMinute = 6.0` fixed cost (scanner, world)
- `targetUtilization = 0.80` — 20% headroom
- Min 2 companies per strategy for statistical comparison
- Assigns diversified home ports (round-robin)

## Coordinator (`internal/bot/coordinator.go`)

Route deconfliction and passenger claiming across all companies.

- **Route deconfliction**: Filters claimed routes from trade requests so companies don't compete with each other on the same routes
- **Passenger claiming**: Coordinates passenger sniping across companies to avoid multiple companies racing for the same passenger group
- **ClaimedRoutes**: Exposed via `TradeDecisionRequest.ClaimedRoutes` so agents can avoid self-competition
- Shared by all `CompanyRunner` instances via the `StrategyContext`

## Scanner (`internal/bot/scanner.go`)

Single goroutine rotating through all ports, fetching NPC prices.

- Batch quotes per port (all goods)
- Updates shared `PriceCache`
- Saves `PriceObservation` records to DB
- Adaptive interval: 8s normal, 30s at high utilization (>85%)
- **On-demand scanning**: When `RefreshWorldData` discovers new ports, the manager immediately triggers `ScanPorts()` to populate prices before the regular scan cycle reaches them. This ensures newly discovered ports can be evaluated as trade destinations right away.

## WorldCache (`internal/bot/world.go`)

World data with indexed lookups. Loaded at startup and refreshed every 5 minutes
to discover new ports, routes, and goods added to the game at runtime. All access
is thread-safe via RWMutex.

- `GetPort/Good/ShipType/Route(id)` — O(1) lookup
- `FindRoute(from, to)` — Direct route between ports
- `RoutesFrom(portID)` — All departing routes
- `GetPortAtIndex(idx)` — Safe indexed access for scanner
- `Snapshot()` — Returns copies of all slices for API handlers
- `RefreshWorldData(ctx, client, logger)` — Fetches and merges new entries, returns newly discovered ports for immediate price scanning
- `ToAgentPorts/Routes/ShipTypes()` — Convert to agent types
