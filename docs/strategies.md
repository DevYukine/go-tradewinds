# Trading Strategies

All strategies inherit from `baseStrategy` and delegate decisions to an Agent.

## Strategy Interface (`internal/bot/strategy.go`)

```go
type Strategy interface {
    Name() string
    Init(ctx StrategyContext) error
    OnShipArrival(ctx, ship *ShipState, port *api.Port) error
    OnTick(ctx, state *CompanyState) error
    Shutdown() error
}
```

`StrategyContext` provides: Client, State, World, PriceCache, ProfitAnalyzer, Agent, Logger, Events, DB, Coordinator.

## Registry (`internal/strategy/registry.go`)

Maps names to factory functions:
- `"arbitrage"` → `NewArbitrage`
- `"bulk_hauler"` → `NewBulkHauler`
- `"market_maker"` → `NewMarketMaker`
- `"passenger_sniper"` → `NewPassengerSniper`
- `"feeder"` → `NewFeeder`
- `"harvester"` → `NewHarvester`

## Base Strategy (`internal/strategy/base.go`)

Shared logic used by all strategies.

### On-Demand Price Scanning
- `ensurePortPrices(ctx, port)` — Checks if port prices are stale (>90s) or missing, fetches fresh buy/sell quotes on demand using `PriorityNormal`. Called automatically by `buildTradeRequestWithPassengers` so ships never make trade decisions with missing price data.

### Request Builders
- `buildTradeRequest(ship, port)` — Assembles TradeDecisionRequest from state, includes `Params` map from `CompanyState.Params`
- `buildTradeRequestWithPassengers(ctx, ship, port)` — Calls `ensurePortPrices`, then extends with available/boarded passengers and P2P orders
- `buildFleetRequest()` — Assembles FleetDecisionRequest

### Trade Execution
- `executeSells(ctx, ship, sells)` — Batch quote + execute sells at port
- `executeBuys(ctx, ship, buys)` — Batch quote + execute buys, treasury floor check, destination routing
- `sendShipToPort(ctx, ship, destPortID)` — Find route, send transit, update local state immediately

### Fleet Execution
- `executeFleetDecision(ctx, decision)` — Sell ships, buy ships (with fallback), buy warehouses, execute warehouse actions (grow/shrink/demolish)
- `tryBuyShip(ctx, purchase)` — Check shipyard inventory, try exact type then cheapest, try multiple ports

### Logging
- `logAgentDecision(type, req, resp, reasoning, confidence, latency)` — Save to AgentDecisionLog
- `recordTrade(exec)` — Save to TradeLog, call `recordRoutePerformance` on sells
- `recordRoutePerformance(sell)` — Match with recent buy of same good, create RoutePerformance record

### Snapshot Converters
- `shipToSnapshot(ship, world)` — ShipState → agent.ShipSnapshot (enriched with ship type info)
- `warehouseToSnapshot(wh)` — WarehouseState → agent.WarehouseSnapshot

## Passenger Sniping via World Events

The `CompanyRunner` subscribes to the public world SSE stream (`/world/events`) and listens for `passenger_request_created` events. When a new passenger group appears at a port:

1. Finds the best idle docked ship at the passenger's origin port
2. Prefers "passenger ships" (cargo capacity ≤ 60, has passenger slots) over cargo ships
3. Among same type, prefers ships idle longer
4. **Boards the passenger directly** via a single `BoardPassenger` API call — skips the full trade pipeline (price scanning, agent decision, etc.) to win the race against other players
5. If boarding succeeds, dispatches the ship through the normal strategy flow to sell cargo and choose the best destination (which now accounts for the boarded passenger)
6. If boarding fails (another player got it first), logs at debug level and moves on — no wasted API calls on the full trade pipeline

### Passenger Ship Spreading

Small/fast passenger ships relocate to uncovered ports (where no other company ships are docked) after just 1 idle tick — maximizing geographic coverage for sniping. Priority: uncovered hubs > uncovered non-hubs > covered hubs. This is handled in `speculativeTrade` in the heuristic agent.

### Immediate Fleet Purchase on Startup

When a company has 0 ships, the fleet evaluation interval is bypassed entirely — all strategies trigger fleet eval on the very first tick to purchase ships immediately.

## Arbitrage Strategy (`internal/strategy/arbitrage.go`)

Fast buy-low-sell-high across ports.

- `OnShipArrival`: Build trade request with passengers → agent decides → execute sells → buys → board passengers → sail
- `OnTick`: Fleet evaluation every 3 min (configurable via `FleetEvalIntervalSec` param)

## Bulk Hauler Strategy (`internal/strategy/bulk_hauler.go`)

High-volume trading with large ships.

- Same flow as arbitrage but agent favors high-value goods and large ship capacity
- Fleet eval every 3 min (configurable via `FleetEvalIntervalSec` param)

## Market Maker Strategy (`internal/strategy/market_maker.go`)

P2P market trading + NPC trading. Not included in default allocation but still available.

- `OnShipArrival`: Same NPC trade flow as arbitrage
- `OnTick`: Fleet eval (3 min) + market eval (1 min) — both configurable via params
- `evaluateMarket`: Fetch all open orders + own orders → agent decides → fill orders, post new orders, cancel stale orders

## Passenger Sniper Strategy (`internal/strategy/passenger_sniper.go`)

Pure passenger revenue focus — tax-free, low-cost, high frequency.

- **Ship preference**: Cheapest ships with passenger slots, max fleet 12. Sells highest-upkeep ships when decommissioning.
- **Geographic spreading**: Ships spread across ports for maximum passenger coverage
- **Coordinator integration**: Uses `Coordinator` for passenger claim coordination across companies to avoid conflicts
- `OnShipArrival`: Same flow as arbitrage with passenger-optimized scoring
- `OnTick`: Fleet eval every 3 min (configurable via `FleetEvalIntervalSec` param)

## Feeder Strategy (`internal/strategy/feeder.go`)

Bailout exploitation — drains treasury to trigger bankruptcy bailouts. No agent involvement; all decisions are hardcoded.

- **Purpose**: Spend money as fast as possible by buying and selling goods at the same port (net loss from spread + double tax), then post inflated P2P buy orders for the harvester to fill.
- **Coordination**: Uses shared `schemeState` (package-level) to coordinate with harvester on target port and stocking status.
- `OnShipArrival`: If not at target port → sail there. At target port → sell all cargo → buy cheapest goods → check treasury for rotation.
- `OnTick`:
  - When `schemeIsStocked()` is true: post P2P buy orders at 1.75x NPC price every 15s (1 per good, budget = treasury / numGoods). Skips goods with existing orders.
  - Cancel stale orders at non-target ports after rotation.
  - Fleet eval: buys up to 3 cheap ships for faster treasury drain.
- **Bankruptcy cycle**: Ships buy → sell back at same port → net loss from spread + double tax → treasury drains → bankrupt → bailed out → repeat.

## Harvester Strategy (`internal/strategy/harvester.go`)

Profit extraction — fills inflated feeder P2P orders from pre-stocked warehouse inventory. No agent involvement.

- **Purpose**: Pre-stock goods at the target port warehouse, then fill feeder buy orders at inflated prices for profit.
- **Supply chain loop**: Sail to non-target port → buy cheap goods → sail to target → store in warehouse → repeat until stocked.
- `OnShipArrival`:
  - Non-target port: sell cargo at NPC prices, buy cheap goods, sail to target port.
  - Target port: store cargo in warehouse (don't sell to NPC), buy more, store again, then sail to resupply port.
- `OnTick` (three periodic tasks):
  - **Order scan (10s)**: When stocked, list buy orders at target port, fill from warehouse stock. Skip own company's orders. Logs estimated profit.
  - **Stocking check (every tick)**: Count warehouse + docked ship cargo. If ≥ 3 ships' worth → `schemeSetStocked(true)`. If depleted → `schemeSetStocked(false)`.
  - **Fleet eval (3 min)**: Buy warehouses at target + next port. Buy ships aggressively (target 6+, largest cargo capacity type).
- **Anti-snipe**: Feeders don't post orders until harvester signals stocked; harvester fills from pre-positioned warehouse inventory within 10s.

### Scheme Coordination (`internal/strategy/scheme.go`)

Shared state for feeder/harvester coordination (package-level, no DB):
- **Port rotation**: Sorted list of all ports. `schemeAdvancePort()` uses atomic CAS.
- **Stocking signal**: `schemeStocked` atomic bool. Harvester sets true when stocked; feeders check before posting P2P orders. Resets on port advance.
- **Phase flow**: Stocking → Extraction → Rotation → repeat.

### Configuration

```
STRATEGY_ALLOCATION="feeder:6,harvester:1"
```

## Configurable Parameters

All strategies read timing intervals from `CompanyState.Params` (set by the optimizer's parameter tuner). If params are nil, hardcoded defaults are used.

| Parameter | Default | Used By |
|-----------|---------|---------|
| `FleetEvalIntervalSec` | 180 (3 min) | All strategies |
| `MarketEvalIntervalSec` | 60 (1 min) | Market Maker |
| `MinMarginPct` | 0.08 (8%) | Heuristic agent trade decisions |
| `PassengerWeight` | 5.0 | Heuristic agent destination scoring |
| `PassengerDestBonus` | 5.0 | Heuristic agent passenger selection |
| `SpeculativeTradeEnabled` | true | Heuristic agent fallback behavior |

## Profitability Guards

The heuristic agent enforces several guards to prevent money-losing trades:
- **Cost basis tracking**: `ShipState.CargoCosts` records the weighted-average buy price (including tax) for each good on each ship. `CargoItem.BuyPrice` carries this to the agent. Costs are recorded in `executeBuys` and cleared in `executeSells`.
- **Loss prevention**: `buildSmartSellOrders` refuses to sell cargo at a loss when `BuyPrice > 0` and `currentNet < BuyPrice`. The cargo is held and routed to the best destination above cost. After 3+ idle ticks, cargo is force-liquidated to free capacity.
- **Minimum margin**: trades must exceed `MinMarginPct` (default 8%) of buy price
- **Sell-side tax**: profit calculation includes both buy and sell port taxes
- **Idle relocation**: after 2+ idle ticks (~60s), ships relocate to the nearest hub port or opportunity port instead of sitting idle. Hub ports are preferred because they have more trade variety.
- **Speculative sailing**: when enabled (default), ships sail to opportunity buy ports from the ProfitAnalyzer when no local trade is profitable
- **Cargo hold threshold**: ships only hold cargo for a better destination if the destination offers >20% better net price after travel upkeep
- **P2P fill threshold**: 5% minimum margin for filling player orders

## ProfitAnalyzer (`internal/bot/profit_analyzer.go`)

Background analyzer that evaluates all cross-port trade opportunities using cached
price data. Maintains a ranked list of the top 50 opportunities (by profit/distance).

- **Recompute**: Called after each full scanner cycle. Iterates all price pairs to find profitable (buy port → sell port) routes.
- **Idle ship routing**: When a ship has no local trades, passengers, or cargo, the agent checks the ProfitAnalyzer for the best reachable buy port and sails there with purpose.
- **Destination scoring bonus**: Destinations that are sell ports of top opportunities receive a scoring bonus.
- **Idle tick tracking**: Ships track consecutive "wait" actions via `ShipState.IdleTicks`. Reset to 0 on any trade/sail action.

## Smart Selling

Instead of dumping all cargo at the current port, the agent evaluates each cargo
item against reachable destinations. Cargo is held in two scenarios:
1. **Loss prevention**: if `BuyPrice > 0` and selling here would lose money (`currentNet < BuyPrice`), the cargo is held and routed to the best profitable destination. After 3+ idle ticks, held cargo is force-liquidated.
2. **Better destination**: cargo is held when a reachable destination offers >20% better net sell price after taxes and travel upkeep.

Held cargo:
- Reduces available ship capacity for new buys
- Adds a scoring bonus to the destination it should be carried to
- Is automatically sold when the ship arrives at the better port

## Destination Scoring

Trade decisions evaluate **all** reachable destinations, not just the single
best individual opportunity. For each destination, the agent simulates filling
the entire ship with profitable goods and computes a composite score:

All strategies now use **profit-per-minute** scoring:
```
score = (cargoProfit - travelUpkeep) / totalTripMinutes
totalTripMinutes = distance / speed + 2
```

| Factor | Weight |
|--------|--------|
| Total achievable cargo profit minus travel upkeep | ÷ totalTripMinutes |
| Warehouse sell profit (goods at current port sellable at dest) | ÷ totalTripMinutes |
| Passenger revenue at destination | ÷ totalTripMinutes × passengerWeight |
| Held cargo profit gain | ÷ totalTripMinutes |
| Route history bonus (avg past profit) | ÷ totalTripMinutes |

### Passenger Override

After choosing a destination, the agent checks if passenger revenue alone
(weighted by `PassengerWeight`) exceeds **half** of the expected trade **profit**
from the buy orders. If so, the destination is overridden to the best passenger
port. This aggressive override ensures passengers — the most reliable revenue
source — are rarely ignored in favor of marginal cargo trades.

## Route Performance History

The `buildTradeRequestWithPassengers` function loads the last 24h of
`RoutePerformance` records (up to 50) for the company. The heuristic agent
uses these to compute an average-profit-per-trade bonus for each destination
from the current port, biasing decisions toward historically profitable routes.

## Warehouse Operations

`warehouseOps` runs after the main trade decision and handles loading/storing goods.

### Load Priority over Low-ROI Buys
Warehouse goods are already paid for (sunk cost), so loading them is almost always
more profitable per-unit than buying new NPC goods. When warehouse load candidates
exist but no ship capacity remains after buy orders:
- Buy orders are sorted by per-unit profit ascending
- Warehouse candidates are compared against the worst buy orders
- If a warehouse load is more profitable, the buy order is displaced (removed)
- Displacement is capped at 50% of planned buy capacity to keep the ship useful
- This ensures warehouse goods don't rot while ships fill up with marginal NPC buys

### Warehouse Sell Profit in Destination Scoring
Destination scoring in both `decideArbitrageTrade` and `decideBulkHaulerTrade`
now includes warehouse sell profit: for each destination candidate, the agent
checks if warehouse goods at the current port can be profitably sold there.
This makes ships naturally choose destinations that allow warehouse inventory
to be offloaded.
