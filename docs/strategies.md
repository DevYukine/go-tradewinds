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

`StrategyContext` provides: Client, State, World, PriceCache, Agent, Logger, DB.

## Registry (`internal/strategy/registry.go`)

Maps names to factory functions:
- `"arbitrage"` → `NewArbitrage`
- `"bulk_hauler"` → `NewBulkHauler`
- `"market_maker"` → `NewMarketMaker`

## Base Strategy (`internal/strategy/base.go`)

Shared logic used by all strategies.

### Request Builders
- `buildTradeRequest(ship, port)` — Assembles TradeDecisionRequest from state
- `buildTradeRequestWithPassengers(ctx, ship, port)` — Extends with available/boarded passengers
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
- `OnTick`: Fleet evaluation every 5 minutes

## Bulk Hauler Strategy (`internal/strategy/bulk_hauler.go`)

High-volume trading with large ships.

- Same flow as arbitrage but agent favors high-value goods and large ship capacity
- Fleet eval every 5 minutes

## Market Maker Strategy (`internal/strategy/market_maker.go`)

P2P market trading + NPC trading.

- `OnShipArrival`: Same NPC trade flow as arbitrage
- `OnTick`: Fleet eval (5 min) + market eval (2 min)
- `evaluateMarket`: Fetch all open orders + own orders → agent decides → fill orders, post new orders, cancel stale orders
