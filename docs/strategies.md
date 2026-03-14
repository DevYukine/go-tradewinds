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

`StrategyContext` provides: Client, State, World, PriceCache, Agent, Logger, Events, DB.

## Registry (`internal/strategy/registry.go`)

Maps names to factory functions:
- `"arbitrage"` → `NewArbitrage`
- `"bulk_hauler"` → `NewBulkHauler`
- `"market_maker"` → `NewMarketMaker`

## Base Strategy (`internal/strategy/base.go`)

Shared logic used by all strategies.

### On-Demand Price Scanning
- `ensurePortPrices(ctx, port)` — Checks if port prices are stale (>3 min) or missing, fetches fresh buy/sell quotes on demand using `PriorityNormal`. Called automatically by `buildTradeRequestWithPassengers` so ships never make trade decisions with missing price data.

### Request Builders
- `buildTradeRequest(ship, port)` — Assembles TradeDecisionRequest from state, includes `Params` map from `CompanyState.Params`
- `buildTradeRequestWithPassengers(ctx, ship, port)` — Calls `ensurePortPrices`, then extends with available/boarded passengers and P2P orders
- `buildFleetRequest()` — Assembles FleetDecisionRequest

### Trade Execution
- `executeSells(ctx, ship, sells)` — Batch quote + execute sells at port
- `executeBuys(ctx, ship, buys)` — Batch quote + execute buys, treasury floor check, destination routing
- `sendShipToPort(ctx, ship, destPortID)` — Find route, send transit, update local state immediately

### Fleet Execution
- `executeFleetDecision(ctx, decision)` — Sell ships, buy ships (with fallback), buy warehouses
- `tryBuyShip(ctx, purchase)` — Check shipyard inventory, try exact type then cheapest, try multiple ports

### Logging
- `logAgentDecision(type, req, resp, reasoning, confidence, latency)` — Save to AgentDecisionLog
- `recordTrade(exec)` — Save to TradeLog, call `recordRoutePerformance` on sells
- `recordRoutePerformance(sell)` — Match with recent buy of same good, create RoutePerformance record

### Snapshot Converters
- `shipToSnapshot(ship, world)` — ShipState → agent.ShipSnapshot (enriched with ship type info)
- `warehouseToSnapshot(wh)` — WarehouseState → agent.WarehouseSnapshot

## Arbitrage Strategy (`internal/strategy/arbitrage.go`)

Fast buy-low-sell-high across ports.

- `OnShipArrival`: Build trade request with passengers → agent decides → execute sells → buys → board passengers → sail
- `OnTick`: Fleet evaluation every 3 min (configurable via `FleetEvalIntervalSec` param)

## Bulk Hauler Strategy (`internal/strategy/bulk_hauler.go`)

High-volume trading with large ships.

- Same flow as arbitrage but agent favors high-value goods and large ship capacity
- Fleet eval every 3 min (configurable via `FleetEvalIntervalSec` param)

## Market Maker Strategy (`internal/strategy/market_maker.go`)

P2P market trading + NPC trading.

- `OnShipArrival`: Same NPC trade flow as arbitrage
- `OnTick`: Fleet eval (3 min) + market eval (1 min) — both configurable via params
- `evaluateMarket`: Fetch all open orders + own orders → agent decides → fill orders, post new orders, cancel stale orders

## Configurable Parameters

All strategies read timing intervals from `CompanyState.Params` (set by the optimizer's parameter tuner). If params are nil, hardcoded defaults are used.

| Parameter | Default | Used By |
|-----------|---------|---------|
| `FleetEvalIntervalSec` | 180 (3 min) | All strategies |
| `MarketEvalIntervalSec` | 60 (1 min) | Market Maker |
| `MinMarginPct` | 0.15 (15%) | Heuristic agent trade decisions |
| `PassengerWeight` | 2.0 | Heuristic agent destination scoring |
| `PassengerDestBonus` | 3.0 | Heuristic agent passenger selection |
| `SpeculativeTradeEnabled` | false | Heuristic agent fallback behavior |

## Profitability Guards

The heuristic agent enforces several guards to prevent money-losing trades:
- **Minimum margin**: trades must exceed `MinMarginPct` (default 15%) of buy price
- **Sell-side tax**: profit calculation includes both buy and sell port taxes
- **No speculative buying**: when no profitable trade exists, ships sail empty toward passenger revenue (not buying speculative cargo)
- **P2P fill threshold**: 7% minimum margin for filling player orders

## Smart Selling

Instead of dumping all cargo at the current port, the agent evaluates each cargo
item against reachable destinations. Cargo is held (not sold) when a destination
offers >20% better net sell price after taxes. Held cargo:
- Reduces available ship capacity for new buys
- Adds a scoring bonus to the destination it should be carried to
- Is automatically sold when the ship arrives at the better port

## Destination Scoring

Trade decisions evaluate **all** reachable destinations, not just the single
best individual opportunity. For each destination, the agent simulates filling
the entire ship with profitable goods and computes a composite score:

| Factor | Arbitrage Weight | Bulk Hauler Weight |
|--------|------------------|--------------------|
| Total achievable cargo profit | ÷ distance | absolute |
| Passenger revenue at destination | ÷ distance × passengerWeight | ÷ distance × passengerWeight |
| Held cargo profit gain | ÷ distance | absolute |
| Route history bonus (avg past profit) | ÷ distance | absolute |

### Passenger Override

After choosing a destination, the agent checks if passenger revenue alone
(weighted by `PassengerWeight`) exceeds the expected trade **profit** from the
buy orders. If so, the destination is overridden to the best passenger port.

## Route Performance History

The `buildTradeRequestWithPassengers` function loads the last 24h of
`RoutePerformance` records (up to 50) for the company. The heuristic agent
uses these to compute an average-profit-per-trade bonus for each destination
from the current port, biasing decisions toward historically profitable routes.
