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

Config selects default agent type via `AGENT_TYPE` env var: `"heuristic"` (default), `"llm"`, `"composite"`.

### Per-Company Agent Override

Each company can use a different agent via `CompanyParams` fields (`agent_type`, `llm_provider`, `llm_model`).

**Strategy allocation format** supports agent hints:
```
# Default (heuristic agent):
STRATEGY_ALLOCATION=arbitrage:3,bulk_hauler:2,market_maker:2

# Mixed heuristic + LLM companies:
STRATEGY_ALLOCATION=arbitrage:3,bulk_hauler:2,market_maker:2,arbitrage/llm-openrouter:1,bulk_hauler/llm-openrouter:1,market_maker/llm-openrouter:1
```

**Per-provider API keys** (env vars):
- `CLAUDE_API_KEY` — for `claude` provider
- `OPENAI_API_KEY` — for `openai` provider
- `OPENROUTER_API_KEY` — for `openrouter` provider (routes to any model)
- `LLM_API_KEY` — fallback for any provider without a specific key

**LLM Providers** (`internal/agent/provider.go`):
- `claude` — Anthropic Messages API (default model: `claude-sonnet-4-20250514`)
- `openai` — OpenAI Chat Completions API (default model: `gpt-4o`)
- `openrouter` — OpenRouter API, OpenAI-compatible, routes to any model (default: `anthropic/claude-sonnet-4`)
- `ollama` — Local Ollama server (default model: `llama3`)

## HeuristicAgent (`internal/agent/heuristic.go`)

Hand-coded rules adapting to strategy hints.

### Trade Decisions (`DecideTradeAction`)

1. **Smart Selling** — Do NOT blindly sell all cargo. For each cargo item, check if a reachable destination offers >20% better net sell price after taxes. If so, HOLD that cargo for the better port. Only sell cargo best sold here or with no better destination.
2. Build price index and reachable ports map (bidirectional route lookup)
3. Build port tax index from `req.Ports` (TaxRateBps)
4. **P2P Order Fills** — Check port orders for profitable fills (>7% margin after taxes). Requires warehouse at port. Filters out own orders to avoid self-fill.
5. If budget <= 0: sell cargo, sail to closest port
6. Delegate to strategy-specific method:

#### Arbitrage (`decideArbitrageTrade`)
- **Destination-level scoring**: For each reachable destination, simulate filling remaining ship capacity with all profitable goods sorted by profit/unit
- Profit calculation: `sellPrice - buyPrice - buyTax - sellTax` (both sides taxed)
- Minimum margin: `profit >= buyPrice * MinMarginPct` (default 15%)
- Score: `totalCargoProfit / distance + passengerRevenue / distance * PassengerWeight + heldCargoGain / distance + routeHistoryBonus`
- PassengerWeight default: 2.0, PassengerDestBonus default: 3.0

#### Bulk Hauler (`decideBulkHaulerTrade`)
- Same destination-level simulation as arbitrage
- Score: `totalCargoProfit + passengerRevenue / distance * PassengerWeight + heldCargoGain + routeHistoryBonus`
- Uses `ship.Capacity` instead of hardcoded limits

#### Speculative (`speculativeTrade`)
- Triggered when no profitable trade meets minimum margin
- Does NOT buy cargo speculatively (unless `SpeculativeTradeEnabled` param is true)
- Sails to destination with highest passenger revenue, or nearest port if no passengers
- Confidence: 0.4

7. **Passenger Override** — After choosing a destination, if passenger revenue (weighted by PassengerWeight) exceeds expected trade profit, override destination to best passenger port
8. **Board passengers** heading to chosen destination (or any reachable port)
   - Score: `bidPerHead / distance`, PassengerDestBonus (default 3.0) for matching destination
   - Fill up to `ship.PassengerCap`
9. **Route History** — `route_history` in request contains recent buy→sell results. Average profit per trade for each (from→to) pair is added as bonus to destination scoring

### Fleet Decisions (`DecideFleetAction`)

Order of evaluation:
1. **Warehouse purchase** — Only if `numShips >= 3`, `treasury > upkeep * 10`, `< 2 warehouses`, and a port has `>= 2 docked ships`
2. **Ship decommission** — If `treasury < totalUpkeep * reserveHours` and `> 1 ship`: sell worst value ship (highest upkeep relative to capacity). Reserve hours scale with fleet size: small (1-3) = 30h, medium (4-10) = 24h, large (10+) = 20h. Sold ships are immediately removed from state to prevent race conditions.
3. **Ship purchase** — Strategy-specific preference:
   - Arbitrage: fastest ship (high speed)
   - Bulk hauler: largest capacity (max fleet: 3)
   - Market maker: cheapest upkeep ship
   - Max fleet: 5 (3 for bulk hauler)
   - Safety: `ship_cost * 1.06 (6% tax) + (current_upkeep + new_upkeep) * 24h`

### Market Decisions (`DecideMarketAction`)
- Fill sell orders: only if `orderPrice > NPC_sell_price * 1.10` (10% min margin)
- Fill buy orders: only if `NPC_buy_price > orderPrice * 1.07` (7% min margin)
- Requires warehouse at port for all fills; filters out own orders (no self-fill)
- Cancel stale own orders (outpriced by NPCs)
- Post new buy/sell orders (spread > 20%, max 5 active orders)
- Account for port taxes (tax_rate_bps) in all profit calculations

### Strategy Evaluation (`EvaluateStrategy`)
- Switch when best strategy's profit/hour is 1.5x better than current
- Switch when current strategy has negative profit (losing money)
- Only recommend switching to a strategy that is actually performing well

### Tunable Parameters (from `CompanyParams` / request `params` field)
- `MinMarginPct`: 0.05–0.50 (default 0.15) — minimum profit margin as fraction of buy price
- `PassengerWeight`: 0.5–5.0 (default 2.0) — passenger revenue weight in destination scoring
- `PassengerDestBonus`: 1.0–10.0 (default 3.0) — bonus for destination-matching passengers
- `FleetEvalIntervalSec`: 60–600 (default 180)
- `MarketEvalIntervalSec`: 30–300 (default 60)
- `SpeculativeTradeEnabled`: false (default) — allow buying without guaranteed profit

## LLM Agent (`internal/agent/llm.go`)

Delegates decisions to an LLM (Claude, OpenAI, or Ollama). Falls back to HeuristicAgent on any error.

- Trade/Fleet/Market/Strategy decisions serialized as JSON → LLM → parsed JSON response
- System prompts per decision type, fully aligned with heuristic agent features:
  - **Trade**: Smart selling (hold cargo for >20% better price), P2P order fills (>7% margin), destination-level scoring with strategy-specific formulas, route history bonus, passenger override, tunable params, tax calculations, minimum margins
  - **Fleet**: Strategy-specific ship preferences, reserve hours scaling, 24h safety margin, ship decommission by value ratio, warehouse rules
  - **Market**: Fill margin thresholds (10% sell-side, 7% buy-side), warehouse requirement, self-fill avoidance, max 5 active orders, cancellation of stale orders
  - **Strategy**: Parameter tuning with valid ranges, switch threshold (1.5x), loss detection
- Strips markdown code fences from responses
- Logs every call with action type, latency, success, and response length

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
