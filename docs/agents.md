# Decision Agents

Agents make all trading decisions. The strategy layer calls agents and executes their responses.

## Agent Interface (`internal/agent/agent.go`)

```go
type Agent interface {
    Name() string
    DecideTradeAction(ctx, TradeDecisionRequest) (*TradeDecision, error)
    DecideFleetAction(ctx, FleetDecisionRequest) (*FleetDecision, error)
    DecideMarketAction(ctx, MarketDecisionRequest) (*MarketDecision, error)
    EvaluateStrategy(ctx, StrategyEvalRequest) (*StrategyEvaluation, error)
}
```

Config selects agent type via `AGENT_TYPE` env var: `"heuristic"` (default), `"llm"`, `"composite"`.

## HeuristicAgent (`internal/agent/heuristic.go`)

Hand-coded rules adapting to strategy hints.

### Trade Decisions (`DecideTradeAction`)

1. **Sell** all cargo at current port
2. Build price index and reachable ports map
3. Build port tax index from `req.Ports` (TaxRateBps)
4. If budget <= 0: sell cargo, sail to closest port
5. Delegate to strategy-specific method:

#### Arbitrage (`decideArbitrageTrade`)
- Score: `netProfit / distance + passengerBonus`
- Net profit: `sellPrice - buyPrice - (buyPrice * taxBps / 10000)`
- Passenger bonus: `passengerRevByDest[destID] / distance * 0.5`
- Pick best destination, then **multi-good fill**: collect all profitable goods for that dest, sort by profit/unit desc, greedily fill `ship.Capacity`

#### Bulk Hauler (`decideBulkHaulerTrade`)
- Score: `netProfit * qty + passengerBonus`
- Same multi-good fill as arbitrage but scored by total profit
- Uses `ship.Capacity` instead of hardcoded limits

#### Speculative (`speculativeTrade`)
- Triggered when no profitable arbitrage exists
- Scans price cache: for each buyable good at current port, find highest sell price at any reachable port
- Picks good+destination with highest margin
- Falls back to closest port with no cargo if nothing found
- Confidence: 0.4

6. **Board passengers** heading to chosen destination (or any reachable port)
   - Score: `bidPerHead / distance`, 2x bonus for matching destination
   - Fill up to `ship.PassengerCap`

### Fleet Decisions (`DecideFleetAction`)

Order of evaluation:
1. **Warehouse purchase** — Only if `numShips >= 3`, `treasury > upkeep * 10`, `< 2 warehouses`, and a port has `>= 2 docked ships`
2. **Ship decommission** — If `treasury < upkeep * 5` and `> 1 ship`: sell worst ship (arbitrage→slowest, bulk→smallest, market→most expensive)
3. **Ship purchase** — Strategy-specific preference:
   - Arbitrage: fastest ship
   - Bulk hauler: largest capacity (max fleet: 3)
   - Market maker: cheapest ship
   - Max fleet: 5 (3 for bulk hauler)
   - Safety margin: `newUpkeep * 3`

### Market Decisions (`DecideMarketAction`)
- Fill underpriced sell orders (profit >= 10% margin)
- Fill overpriced buy orders (same margin)
- Cancel stale own orders (outpriced by NPCs)
- Post new buy orders at `buyPrice + spread/4` (if spread > 20%, max 5 active orders)

### Strategy Evaluation (`EvaluateStrategy`)
- Switch if worst strategy losing money and best is profitable
- Switch if best > 2x worst profit/hour

## LLM Agent (`internal/agent/llm.go`)

Delegates decisions to an LLM (Claude, OpenAI, or Ollama). Falls back to HeuristicAgent on any error.

- Trade/Fleet/Market/Strategy decisions serialized as JSON → LLM → parsed JSON response
- System prompts per decision type
- Strips code fences from responses

## Composite Agent (`internal/agent/composite.go`)

Routes decisions between fast (heuristic) and slow (LLM) agents:
- `DecideTradeAction` → Fast (time-sensitive)
- `DecideFleetAction` → Slow (fallback to fast)
- `DecideMarketAction` → Slow (fallback to fast)
- `EvaluateStrategy` → Slow (fallback to fast)

## Key Types

### Request Types
- `TradeDecisionRequest` — Ship, company, price cache, routes, ports (with TaxRateBps), constraints, passengers
- `FleetDecisionRequest` — Ships, warehouses, ship types, shipyard ports
- `MarketDecisionRequest` — Open/own orders, price cache, warehouses
- `StrategyEvalRequest` — Strategy metrics array

### Response Types
- `TradeDecision` — SellOrders, BuyOrders, BoardPassengers, SailTo, Confidence
- `FleetDecision` — BuyShips, SellShips, BuyWarehouses
- `MarketDecision` — FillOrders, PostOrders, CancelOrders
- `StrategyEvaluation` — ParamChanges, SwitchTo

### Snapshot Types
- `ShipSnapshot` — ID, Name, Status, PortID, Cargo, Capacity, Speed, PassengerCap
- `CompanySnapshot` — ID, Treasury, Reputation, TotalUpkeep
- `WarehouseSnapshot` — ID, PortID, Level, Capacity, Items
- `PricePoint` — PortID, GoodID, BuyPrice, SellPrice, ObservedAt
- `PortInfo` — ID, Name, Code, IsHub, TaxRateBps
- `RouteInfo` — ID, FromID, ToID, Distance
- `ShipTypeInfo` — ID, Name, Capacity, Speed, Upkeep, BasePrice, PassengerCap
- `PassengerInfo` — ID, Count, Bid, OriginPortID, DestinationPortID, ExpiresAt
- `Constraints` — TreasuryFloor (2x upkeep), MaxSpend
