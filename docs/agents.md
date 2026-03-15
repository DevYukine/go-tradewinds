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

1. **Smart Selling** — Do NOT blindly sell all cargo. For each cargo item, check if a reachable destination offers >30% better net sell price after taxes. If so, HOLD that cargo for the better port. Only sell cargo best sold here or with no better destination.
2. Build price index and reachable ports map (bidirectional route lookup)
3. Build port tax index from `req.Ports` (TaxRateBps)
4. **P2P Order Fills** — Check port orders for profitable fills (>5% margin after taxes). Requires warehouse at port. Filters out own orders to avoid self-fill.
5. If budget <= 0: sell cargo, sail to closest port
6. Delegate to strategy-specific method:

#### Arbitrage (`decideArbitrageTrade`)
- **Destination-level scoring**: For each reachable destination, simulate filling remaining ship capacity with all profitable goods sorted by profit/unit
- **Passenger-only destinations**: Ports with passenger revenue but no cargo profit are also scored as candidates
- **Opportunity sell-port bonus**: Destinations that are sell ports of top ProfitAnalyzer opportunities get a scoring bonus
- Profit calculation: `sellPrice - buyPrice - buyTax - sellTax` (both sides taxed)
- Minimum margin: `profit >= buyPrice * MinMarginPct` (default 5%)
- Score: `totalCargoProfit / distance + passengerRevenue / distance * PassengerWeight + heldCargoGain / distance + routeHistoryBonus + opportunityBonus`
- PassengerWeight default: 8.0, PassengerDestBonus default: 8.0

#### Bulk Hauler (`decideBulkHaulerTrade`)
- Same destination-level simulation as arbitrage
- Same passenger-only destinations and opportunity sell-port bonus
- Score: `totalCargoProfit + passengerRevenue / distance * PassengerWeight + heldCargoGain + routeHistoryBonus + opportunityBonus`
- Uses `ship.Capacity` instead of hardcoded limits

#### Warehouse Operations (`warehouseOps`)
- Integrated into arbitrage and bulk hauler trade decisions (not market_maker)
- **LOAD (warehouse → ship)**: When docked at a port with a warehouse, check inventory for goods profitable to sell at the destination (or any reachable port). Load onto ship to fill remaining capacity. No buy tax since goods are already owned.
- **STORE (buy → warehouse)**: Low-priority fallback, only during idle/speculative decisions (confidence ≤ 0.5). Buy cheap goods at the current port and store in warehouse when sell margin ≥ 15% at any known port. Limited to 25% of available budget to avoid over-investing.
- Store destinations use `BuyOrder.Destination = warehouse_id` with `type: "warehouse"` in the API call

#### Speculative (`speculativeTrade`)
- Triggered when no profitable trade meets minimum margin
- Priority: (1) sail to best passenger revenue destination, (2) sail to ProfitAnalyzer opportunity buy port, (3) after 2+ idle ticks → relocate to nearest hub port or opportunity sell port, (4) first tick only → wait briefly for price refresh
- Hub ports are preferred for relocation because they have more goods and routes
- Confidence: 0.3–0.5

7. **Passenger Override** — After choosing a destination, if passenger revenue (weighted by PassengerWeight) exceeds **quarter** of expected trade profit, override destination to best passenger port (very aggressive override)
8. **Budget=0 passenger routing** — When treasury is exhausted, sail to best passenger destination (not just closest port) since passengers are pure revenue
9. **Board passengers** heading to chosen destination (or any reachable port)
   - Score: `bidPerHead / distance`, PassengerDestBonus (default 8.0) for matching destination
   - Fill up to `ship.PassengerCap`
9. **Route History** — `route_history` in request contains recent buy→sell results. Average profit per trade for each (from→to) pair is added as bonus to destination scoring

### Fleet Decisions (`DecideFleetAction`)

Order of evaluation:
1. **Warehouse purchase** — Only if `numShips >= 3`, `treasury > upkeep * 10 cycles`, `< 2 warehouses`, and a port has `>= 2 docked ships`
2. **Ship decommission** — If `treasury < totalUpkeep * reserveCycles` and `> 1 ship`: sell worst value ship (highest upkeep relative to capacity). Reserve cycles scale with fleet size: base 5 cycles (25h), +1 cycle per 4 ships (bulk_hauler: per 2, market_maker: per 5). Sold ships are immediately removed from state to prevent race conditions. Ship must have empty cargo hold (checked via API) before selling.
3. **Ship purchase** — Strategy-specific preference:
   - Arbitrage: fastest ship with passenger capacity bonus (`speed + passengerCap/5`)
   - Bulk hauler: largest capacity, passenger capacity as tiebreaker (max fleet: 3)
   - Market maker: cheapest upkeep ship
   - Max fleet: 5 (3 for bulk hauler)
   - Safety: `ship_cost * (1 + port_tax_rate) + (current_upkeep + new_upkeep) * 5 cycles` (tax from port's `tax_rate_bps`)
   - **Backoff**: When all shipyards are out of stock, exponential backoff (30s→1m→…→30m) before retrying. Shipyard inventory refills ~every 30 minutes.
   - **Note**: Upkeep is charged per 5-hour cycle, not hourly. All reserve calculations use cycles.

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
- `MinMarginPct`: 0.03–0.50 (default 0.05) — minimum profit margin as fraction of buy price
- `PassengerWeight`: 0.5–20.0 (default 8.0) — passenger revenue weight in destination scoring
- `PassengerDestBonus`: 1.0–20.0 (default 8.0) — bonus for destination-matching passengers
- `FleetEvalIntervalSec`: 60–600 (default 180)
- `MarketEvalIntervalSec`: 30–300 (default 60)
- `SpeculativeTradeEnabled`: true (default) — allow sailing to opportunity ports when no local trade is profitable

## LLM Agent (`internal/agent/llm.go`)

Delegates decisions to an LLM (Claude, OpenAI, or Ollama). Returns errors to the caller on failure (no silent fallback).

- Trade/Fleet/Market/Strategy decisions serialized as JSON → LLM → parsed JSON response
- **Data-driven prompts**: The LLM receives full game state, game mechanics explanations, data field descriptions, and hard constraints — but reasons autonomously about trade-offs and strategy rather than following prescribed formulas or thresholds
- System prompts per decision type:
  - **Trade**: Game mechanics (taxes, routes, passengers, P2P), data dictionary for all input fields, hard constraints (treasury floor, reachability, warehouse requirement, no self-fill), goals (maximize profit across all revenue streams, avoid empty sailing)
  - **Fleet**: Ship purchase/sell/warehouse mechanics, shipyard port constraint, treasury sustainability goals
  - **Market**: P2P order mechanics, warehouse requirement, NPC price baseline for comparison
  - **Strategy**: Available strategies and parameter ranges, performance-based evaluation
- LLM errors are returned to the strategy layer (no silent heuristic fallback)
- **Exponential backoff**: Trade dispatch retries 5× (2s→4s→8s→16s), fleet/market eval failures back off 30s→1m→…→30m before retrying
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
- `TradeDecisionRequest` — Ship, company, price cache, routes, ports (with TaxRateBps), constraints, passengers, TopOpportunities (from ProfitAnalyzer)
- `FleetDecisionRequest` — Ships, warehouses, ship types, shipyard ports
- `MarketDecisionRequest` — Open/own orders, price cache, warehouses
- `StrategyEvalRequest` — Strategy metrics array

### Response Types
- `TradeDecision` — SellOrders, BuyOrders, WarehouseLoads, WarehouseStores, BoardPassengers, SailTo, Confidence
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
