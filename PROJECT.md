# Tradewinds Bot — Architectural Plan

## Code Quality Standards

All code in this project must follow **best practices and conventions** for each language, framework, and library used:

- **Go**: Follow [Effective Go](https://go.dev/doc/effective_go), the Go Code Review Comments guide, and standard project layout conventions. Use idiomatic error handling, proper goroutine lifecycle management, meaningful variable names, and clear package boundaries. Avoid global state. Use interfaces for testability.
- **Nuxt 4 / Vue.js**: Follow the Vue.js Style Guide (Priority A & B rules), use Composition API with `<script setup>`, leverage Nuxt auto-imports and conventions. Keep components small and single-purpose. Use TypeScript strictly — no `any` types unless unavoidable.
- **TailwindCSS 4**: Use utility-first approach, extract repeated patterns into components (not `@apply` bloat). Follow the CSS-first config convention of TailwindCSS 4.
- **GORM**: Use proper model tagging, avoid raw SQL unless necessary, use scopes for reusable queries, handle migrations cleanly.
- **uber/fx**: One module per domain, explicit dependency declarations, use `fx.Lifecycle` for startup/shutdown ordering. No init() functions.
- **uber/zap**: Structured logging everywhere. Use child loggers with context fields. Never use `fmt.Println` or `log.Println`.
- **go-fiber**: Use middleware properly, group routes logically, return consistent JSON error responses.

**General principles:**
- Write clean, readable, and maintainable code above all else
- Prefer clarity over cleverness
- Name things descriptively — code should read like documentation
- Keep functions short and focused (single responsibility)
- Handle errors explicitly and meaningfully — no swallowed errors
- Write code that future contributors can understand without extra context

## Tech Stack

| Layer | Technology |
|-------|-----------|
| **Backend** | Go, go-fiber (HTTP framework), uber/fx (DI), uber/zap (logging) |
| **ORM** | GORM + PostgreSQL |
| **Config** | godotenv (.env) |
| **Dashboard** | Nuxt 4, Vue.js 3.5+, TailwindCSS 4, nuxt-charts, @nuxt/icon + @iconify |
| **Charting** | nuxt-charts (ApexCharts-based, Nuxt 4 compatible) |
| **Icons** | @nuxt/icon (Iconify-powered, built-in with Nuxt 4 ecosystem) |
| **AI Integration** | Pluggable agent interface — supports local heuristics, LLM APIs, or custom ML models |

## Multi-Company Architecture

The bot uses a **single game account** (one player login) with **multiple companies** created under that player. Each company operates as an independent trading entity with its own treasury, ships, warehouses, and strategy. This allows testing different strategies simultaneously while sharing a single authentication token.

**Key implications:**
- One JWT token from `POST /auth/login` is shared across all companies
- Each API call that requires company context uses the `tradewinds-company-id` header to specify which company
- Companies are created via `POST /companies` under the same player
- Each company has its own treasury, reputation, fleet, and warehouses — fully independent
- The bot spawns one goroutine per company, each with its own `api.Client` instance (sharing the same token but setting different `tradewinds-company-id` headers)

## Rate-Limit-Aware Company Sizing

**Known rate limit: 300 requests per 60 seconds (5 req/sec), per IP.**

All companies share a single IP, so all API calls across all companies count against this one budget. The bot must size its company count to stay comfortably within this limit — targeting **80% utilization (240 req/min)** as a safety margin to absorb bursts.

### API Budget System

A centralized `RateLimiter` enforces the 300/60s limit using a token bucket:

```go
type RateLimiter struct {
    maxPerMinute    int              // 300 (from game spec)
    maxPerSecond    float64          // 5.0
    tokens          float64          // Current available tokens (token bucket)
    lastRefill      time.Time
    usedThisWindow  int64            // Atomic counter for current 60s window
    windowStart     time.Time
    mu              sync.Mutex
    logger          *zap.Logger
}

func (rl *RateLimiter) Acquire(ctx context.Context, priority Priority) error
func (rl *RateLimiter) TryAcquire(priority Priority) bool
func (rl *RateLimiter) Utilization() float64  // usedThisWindow / 300
```

The limiter uses a sliding window counter (tracks requests in the current 60s window) combined with a token bucket (smooths out per-second bursts to ~5 req/sec). If the window counter reaches 270 (90%), only PriorityHigh requests are allowed through. At 295, all requests block until the window resets.

### Company Count Formula

The number of companies is determined by the 300/min budget divided by per-company API consumption:

```
Per-company API calls per minute (steady state):
  - SSE event stream:          0 (long-lived connection, not counted)
  - Economy refresh (60s):     1 call/min
  - Ship status checks:        ~1 call/min (via SSE, rarely polled)
  - Trade cycle (on arrival):  ~3 calls (quote + execute + transit) per ship arrival
  - Price scan contribution:   0 (shared scanner, not per-company)

  Estimate per company: ~3-5 API calls/min (steady state, 1-2 ships)
  Estimate per company: ~8-12 API calls/min (active, 3+ ships with frequent arrivals)

Shared API calls per minute:
  - Price scanner:             ~5 calls/min (15 ports / 3min cycle)
  - World cache refresh:       ~0.1 calls/min (every 10 min)
  - Total shared:              ~6 calls/min

Budget for companies (80% safety margin):
  available = 300 * 0.80 - 6 = 234 calls/min

Conservative estimate (worst case ~10 calls/min/company):
  max_companies = 234 / 10 = ~23 companies

Moderate estimate (typical ~5 calls/min/company):
  max_companies = 234 / 5 = ~46 companies

Default allocation (well within budget):
  arbitrage:3 + bulk_hauler:2 + market_maker:2 = 7 companies
  Estimated usage: 7 * 5 + 6 = ~41 calls/min (14% of budget)
  → Plenty of headroom for scaling up
```

With the default 7 companies, the bot uses only ~14% of the rate limit budget — leaving ample room for burst handling, price scanning, and future scaling. The `Manager` still validates on startup and caps if needed.

**Scaling guidance:**
- Up to ~20 companies is very safe (uses ~40% budget)
- 20-40 companies is moderate (uses 40-80%, may need to slow price scanner)
- 40+ companies requires careful tuning (reduce scan frequency, increase tick intervals)

### Staggered Scheduling

Even within budget, bursty behavior (e.g., 5 ships docking simultaneously) can spike. The bot mitigates this:
- **Jittered tickers**: Each company's periodic ticker has a random offset so they don't all fire at the same second
- **Trade queue**: Ship arrival trade cycles are enqueued through the `RateLimiter` rather than executed immediately — this serializes bursts across companies
- **Priority levels**: Price scanner and economy refreshes are low priority; trade executions (time-sensitive quotes) are high priority and jump the queue
- **Backpressure**: If a company's API call blocks on the rate limiter for >10s, it logs a warning and the optimizer considers reducing active companies

### Multiple Companies Per Strategy (Sample Size)

To get statistically meaningful comparisons, each strategy runs on **multiple companies**:

```env
# .env — Strategy allocation
# Format: strategy_name:count
# The bot creates this many companies per strategy
STRATEGY_ALLOCATION=arbitrage:3,bulk_hauler:2,market_maker:2
```

This means:
- 3 companies running `arbitrage` (e.g., TRD1, TRD2, TRD3)
- 2 companies running `bulk_hauler` (e.g., BLK1, BLK2)
- 2 companies running `market_maker` (e.g., MKT1, MKT2)
- Total: 7 companies (must fit within rate limit budget)

Companies within the same strategy are **not identical** — they start at different home ports to diversify route coverage. The optimizer aggregates metrics across all companies of a strategy to compute a statistically robust score (mean profit/hour, standard deviation, confidence intervals) rather than relying on a single data point.

If the rate limit budget is too small for the requested allocation, the bot scales down proportionally while maintaining at least 2 companies per strategy (minimum viable sample size). If even that doesn't fit, it falls back to 1 per strategy with a warning.

## AI Agent Integration Layer

### Design Philosophy

All trading decisions flow through a pluggable **Agent** interface. The default implementation uses hand-coded heuristics (the strategies described in Step 4), but the interface is designed so that any decision point can be delegated to an external AI agent (LLM, ML model, reinforcement learning, etc.) without changing the bot's core loop.

### Agent Interface (`internal/agent/agent.go`)

```go
// Agent makes trading decisions. Implementations can be heuristic, LLM-based, or ML-based.
type Agent interface {
    // Name returns a human-readable identifier for this agent
    Name() string

    // DecideTradeAction is called when a ship docks. It receives the full game state
    // and must return what to buy/sell and where to sail next.
    DecideTradeAction(ctx context.Context, req TradeDecisionRequest) (*TradeDecision, error)

    // DecideFleetAction is called periodically. It can recommend buying ships,
    // upgrading warehouses, or other capital decisions.
    DecideFleetAction(ctx context.Context, req FleetDecisionRequest) (*FleetDecision, error)

    // DecideMarketAction is called when evaluating P2P market opportunities.
    DecideMarketAction(ctx context.Context, req MarketDecisionRequest) (*MarketDecision, error)

    // EvaluateStrategy is called by the optimizer. Given performance metrics,
    // the agent can recommend parameter adjustments or strategy switches.
    EvaluateStrategy(ctx context.Context, req StrategyEvalRequest) (*StrategyEvaluation, error)
}
```

### Decision Request/Response Types

```go
// TradeDecisionRequest contains everything an agent needs to decide a trade
type TradeDecisionRequest struct {
    Company       CompanySnapshot     // Treasury, reputation, upkeep
    Ship          ShipSnapshot        // Current port, cargo, capacity, speed
    AllShips      []ShipSnapshot      // Fleet state (avoid conflicting routes)
    Warehouses    []WarehouseSnapshot // Warehouse locations and inventory
    PriceCache    []PricePoint        // Latest known prices across all ports
    RouteGraph    RouteGraph          // Port distances
    PortInfo      []PortInfo          // Tax rates, hub status
    RecentTrades  []TradeLogEntry     // Last N trades for context
    Constraints   Constraints         // Treasury floor, max spend, etc.
}

type TradeDecision struct {
    Action        string              // "buy_and_sail", "sell_and_buy", "wait", "dock"
    SellOrders    []SellOrder         // What to sell at current port (if carrying cargo)
    BuyOrders     []BuyOrder          // What to buy before departing
    SailTo        *uuid.UUID          // Destination port (nil = stay docked)
    Reasoning     string              // Human-readable explanation (for logging/dashboard)
    Confidence    float64             // 0.0-1.0, used by optimizer to weight decisions
}

type FleetDecisionRequest struct {
    Company       CompanySnapshot
    Ships         []ShipSnapshot
    Warehouses    []WarehouseSnapshot
    ShipTypes     []ShipTypeInfo      // Available ship types and costs
    PriceCache    []PricePoint
    Performance   StrategyMetrics     // How well current fleet is performing
}

type FleetDecision struct {
    BuyShips      []ShipPurchase      // Ship type + port to buy at
    SellShips     []uuid.UUID         // Ships to decommission (if supported)
    BuyWarehouses []uuid.UUID         // Ports to build warehouses at
    Reasoning     string
}

type MarketDecisionRequest struct {
    Company       CompanySnapshot
    OpenOrders    []MarketOrder       // Current P2P market state
    OwnOrders     []MarketOrder       // Our active orders
    PriceCache    []PricePoint        // NPC reference prices
    Warehouses    []WarehouseSnapshot
}

type MarketDecision struct {
    PostOrders    []NewMarketOrder    // Orders to post
    FillOrders    []FillOrder         // Other players' orders to fill
    CancelOrders  []uuid.UUID         // Our orders to cancel
    Reasoning     string
}

type StrategyEvalRequest struct {
    Metrics       []StrategyMetrics   // Performance data across all companies
    CurrentParams map[string]any      // Current strategy parameters
    PriceHistory  []PriceObservation  // Price trends
}

type StrategyEvaluation struct {
    ParamChanges  map[string]any      // Suggested parameter adjustments
    SwitchTo      *string             // Recommend switching strategy (nil = keep current)
    Reasoning     string
}
```

### Built-in Agent Implementations

```go
// HeuristicAgent — the default. Uses hand-coded rules (arbitrage matrix, etc.)
type HeuristicAgent struct { ... }

// LLMAgent — delegates decisions to an LLM API (Claude, GPT, etc.)
type LLMAgent struct {
    provider  LLMProvider
    model     string
    apiKey    string
    maxTokens int
    logger    *zap.Logger
}

// CompositeAgent — uses heuristics for time-sensitive decisions (trade execution)
// and LLM for strategic decisions (fleet management, strategy evaluation)
type CompositeAgent struct {
    fast Agent  // Used for DecideTradeAction (low latency required)
    slow Agent  // Used for DecideFleetAction, EvaluateStrategy (can be slower)
}

// ReplayAgent — replays recorded decisions from DB for backtesting
type ReplayAgent struct { ... }
```

### LLM Provider Interface

```go
// LLMProvider abstracts the LLM API call
type LLMProvider interface {
    Complete(ctx context.Context, prompt string, systemPrompt string) (string, error)
}

// Implementations:
type ClaudeProvider struct { apiKey, model string }
type OpenAIProvider struct { apiKey, model string }
type OllamaProvider struct { baseURL, model string }  // Local models
```

The `LLMAgent` serializes the decision request into a structured prompt with game context, sends it to the LLM, and parses the structured response (JSON). Failed or unparseable responses fall back to the `HeuristicAgent`.

### Agent Configuration

```env
# .env
# Agent type: "heuristic", "llm", "composite"
AGENT_TYPE=heuristic

# LLM settings (only if AGENT_TYPE=llm or composite)
LLM_PROVIDER=claude          # "claude", "openai", "ollama"
LLM_MODEL=claude-sonnet-4-6
LLM_API_KEY=sk-ant-...
LLM_MAX_TOKENS=4096

# Composite agent settings
COMPOSITE_FAST_AGENT=heuristic    # For time-sensitive trade decisions
COMPOSITE_SLOW_AGENT=llm          # For strategic decisions
```

### Integration Points

The Agent slots into the existing Strategy/CompanyRunner architecture:

```
CompanyRunner.Run()
  └─ strategy.OnShipArrival(ship, port)
       └─ agent.DecideTradeAction(ctx, request)  ← AGENT DECISION POINT
            └─ execute the returned TradeDecision via API client

  └─ strategy.OnTick(state)
       └─ agent.DecideFleetAction(ctx, request)   ← AGENT DECISION POINT
       └─ agent.DecideMarketAction(ctx, request)  ← AGENT DECISION POINT

  └─ optimizer.Evaluate()
       └─ agent.EvaluateStrategy(ctx, request)    ← AGENT DECISION POINT
```

The Strategy implementations become thin wrappers that:
1. Gather the current game state into a decision request
2. Call the Agent
3. Execute the Agent's response via the API client
4. Log the Agent's reasoning to the dashboard

This means switching from heuristic to AI-driven trading is a config change, not a code change.

### Agent Decision Logging

Every agent decision is logged with full context for analysis and replay:

```go
type AgentDecisionLog struct {
    ID          uint           `gorm:"primaryKey"`
    CompanyID   uint           `gorm:"index"`
    AgentName   string         // Which agent made this decision
    DecisionType string        // "trade", "fleet", "market", "strategy_eval"
    Request     string         // JSON-serialized request (full game state snapshot)
    Response    string         // JSON-serialized decision
    Reasoning   string         // Agent's explanation
    Confidence  float64
    LatencyMs   int64          // How long the agent took to decide
    Outcome     string         // Filled in later: "profit", "loss", "neutral"
    OutcomeValue int64         // Actual P&L from this decision
    CreatedAt   time.Time
}
```

This enables:
- Dashboard display of agent reasoning per trade
- Backtesting: replay past states through a new agent and compare decisions
- Training data: export decision logs as training data for ML models
- A/B testing: compare heuristic vs LLM decisions on the same game states

## Project Structure

```
tradewinds/
├── cmd/
│   └── bot/
│       └── main.go                 # Entry point: fx.New() wires everything
├── internal/
│   ├── config/
│   │   └── config.go               # .env loading, provides config to fx container
│   ├── api/
│   │   ├── client.go               # Base HTTP client (auth, headers, retries, rate limiting)
│   │   ├── ratelimiter.go          # Centralized token-bucket rate limiter with priority queue
│   │   ├── auth.go                 # POST /auth/register, /auth/login, /auth/revoke, GET /me
│   │   ├── company.go              # Company CRUD, economy, ledger
│   │   ├── world.go                # Ports, goods, routes, ship-types (public, no auth)
│   │   ├── trade.go                # NPC trade: quotes, execute, batch
│   │   ├── market.go               # P2P market: orders, fill, cancel, blended-price
│   │   ├── fleet.go                # Ships: list, detail, transit, inventory, transfer
│   │   ├── warehouse.go            # Warehouses: buy, list, inventory, grow, shrink, transfer
│   │   ├── shipyard.go             # Shipyard: find, inventory, purchase
│   │   ├── events.go               # SSE client for world + company event streams
│   │   └── models.go               # All request/response Go structs
│   ├── agent/
│   │   ├── agent.go                # Agent interface + decision types
│   │   ├── heuristic.go            # Default heuristic agent (rule-based)
│   │   ├── llm.go                  # LLM-powered agent (Claude, OpenAI, Ollama)
│   │   ├── composite.go            # Composite agent (fast heuristic + slow LLM)
│   │   ├── replay.go               # Replay agent for backtesting
│   │   └── provider/
│   │       ├── provider.go         # LLM provider interface
│   │       ├── claude.go           # Anthropic Claude provider
│   │       ├── openai.go           # OpenAI provider
│   │       └── ollama.go           # Local Ollama provider
│   ├── db/
│   │   ├── db.go                   # GORM connection setup, auto-migrate, fx provider
│   │   └── models.go               # GORM models (companies, trades, P&L, logs, decisions)
│   ├── bot/
│   │   ├── manager.go              # Multi-company manager: logs in once, spawns company goroutines
│   │   ├── company_runner.go       # Single company lifecycle: init, run strategy loop
│   │   ├── state.go                # Per-company in-memory state
│   │   └── scaler.go               # Company count calculator based on rate limit budget
│   ├── strategy/
│   │   ├── strategy.go             # Strategy interface + registry (delegates to Agent)
│   │   ├── arbitrage.go            # Arbitrage strategy (gathers state, calls Agent, executes)
│   │   ├── market_maker.go         # Market maker strategy
│   │   ├── bulk_hauler.go          # Bulk hauler strategy
│   │   └── scanner.go              # Shared price scanner (runs once, feeds all companies)
│   ├── optimizer/
│   │   ├── engine.go               # Auto-optimization engine with statistical analysis
│   │   └── metrics.go              # Strategy performance metrics + confidence intervals
│   └── server/
│       ├── server.go               # go-fiber app setup, middleware, fx provider
│       ├── handlers.go             # REST API handlers: companies, P&L, logs, charts, decisions
│       └── sse.go                  # SSE endpoint for live log streaming to dashboard
├── dashboard/                      # Nuxt 4 app (separate from Go)
│   ├── nuxt.config.ts
│   ├── package.json
│   ├── app.vue
│   ├── app/
│   │   ├── pages/
│   │   │   └── index.vue           # Main dashboard page
│   │   ├── components/
│   │   │   ├── CompanySidebar.vue
│   │   │   ├── CompanyDetail.vue
│   │   │   ├── PnLChart.vue
│   │   │   ├── InventoryChart.vue
│   │   │   ├── LiveLogs.vue
│   │   │   ├── StrategyComparison.vue
│   │   │   ├── OptimizerLog.vue
│   │   │   ├── AgentDecisions.vue   # Agent reasoning viewer
│   │   │   └── RateLimitGauge.vue   # API budget usage visualization
│   │   ├── composables/
│   │   │   ├── useCompanies.ts
│   │   │   ├── usePnL.ts
│   │   │   ├── useLogs.ts
│   │   │   └── useAgent.ts          # Agent decision log fetching
│   │   └── types/
│   │       └── index.ts
│   └── public/
│       └── favicon.ico
├── .env.example                    # Template for required env vars
├── go.mod
├── go.sum
├── PROJECT.md                      # This file
└── GAME.md                         # Game reference
```

---

## Step 1: Game Mechanics & API Wrapper

### 1.1 — API Models (`internal/api/models.go`)

Define Go structs for every request and response type documented in GAME.md. All IDs are `uuid.UUID`. All responses follow the `{ data: ... }` envelope pattern.

**Key structs:**

```
// Envelope
type APIResponse[T any] struct { Data T }
type APIError struct { Errors struct { Detail string } }

// Auth
RegisterRequest { Name, Email, Password, DiscordID? }
LoginRequest { Email, Password }
LoginResponse { Token string }
Player { ID, Name, Email, Enabled, InsertedAt }

// Company
CreateCompanyRequest { Name, Ticker, HomePortID }
Company { ID, Name, Ticker, Treasury, Reputation, Status, HomePortID }
CompanyEconomy { Treasury, Reputation, ShipUpkeep, WarehouseUpkeep, TotalUpkeep }
LedgerEntry { ID, Amount, Reason, ReferenceType, ReferenceID, OccurredAt, Meta }

// World
Port { ID, Name, Code, IsHub, TaxRateBps, CountryID, Traders[], OutgoingRoutes[] }
Good { ID, Name, Description, Category }
Route { ID, FromID, ToID, Distance }
ShipType { ID, Name, Capacity, Speed, Upkeep, BasePrice, Passengers, Description }

// Trade
QuoteRequest { PortID, GoodID, Action, Quantity }
Quote { Token, UnitPrice, TotalPrice, Quantity, ... }
ExecuteQuoteRequest { Token, Destinations[] { Type, ID, Quantity } }
TradeExecution { Action, Quantity, UnitPrice, TotalPrice }
BatchQuoteRequest { Requests []QuoteRequest }
ExecuteTradeRequest { PortID, GoodID, Action, Destinations[] }

// Market
Order { ID, CompanyID, PortID, GoodID, Side, Price, Total, Remaining, Status, PostedReputation, ExpiresAt }
CreateOrderRequest { PortID, GoodID, Side, Price, Total }
FillOrderRequest { Quantity }
BlendedPrice { BlendedPrice float64 }

// Fleet
Ship { ID, Name, Status, CompanyID, ShipTypeID, PortID?, RouteID?, ArrivingAt? }
Cargo { GoodID, Quantity }
TransitRequest { RouteID }
TransitLog { ID, ShipID, RouteID, DepartedAt, ArrivedAt? }
TransferToWarehouseRequest { WarehouseID, GoodID, Quantity }

// Warehouses
Warehouse { ID, Level, Capacity, PortID, CompanyID }
WarehouseInventory { ID, WarehouseID, GoodID, Quantity }
TransferToShipRequest { ShipID, GoodID, Quantity }

// Shipyard
Shipyard { ID, PortID }
ShipyardInventory { ID, ShipyardID, ShipTypeID, ShipID, Cost }
PurchaseShipRequest { ShipTypeID }

// Events (SSE)
SSEEvent { Type string, Data json.RawMessage }
ShipDockedEvent { Name, ShipID, PortID, CompanyID, CompanyName }
ShipSetSailEvent { Name, ShipID, RouteID, CompanyID, CompanyName }

// Trader
Trader { ID, Name }
TraderPosition { ID, TraderID, PortID, GoodID, StockBounds, PriceBounds }
```

### 1.2 — Centralized Rate Limiter (`internal/api/ratelimiter.go`)

All API calls across all companies flow through a single rate limiter enforcing the **300 requests / 60 seconds per IP** limit.

```go
type Priority int
const (
    PriorityHigh   Priority = 0  // Trade executions (quote expiry sensitive)
    PriorityNormal Priority = 1  // Ship transit, inventory checks
    PriorityLow    Priority = 2  // Price scanning, economy refresh
)

type RateLimiter struct {
    maxPerMinute    int             // 300 (hardcoded from game spec)
    maxPerSecond    float64         // 5.0 (300/60, for token bucket smoothing)
    tokens          float64         // Token bucket — refills at 5/sec
    lastRefill      time.Time
    usedThisWindow  int64           // Atomic counter for sliding 60s window
    windowStart     time.Time
    queue           [3]chan struct{} // Priority queues (high, normal, low)
    mu              sync.Mutex
    logger          *zap.Logger
    backoffUntil    time.Time       // Set on 429, blocks all requests until reset
}

func (rl *RateLimiter) Acquire(ctx context.Context, priority Priority) error
func (rl *RateLimiter) TryAcquire(priority Priority) bool
func (rl *RateLimiter) CurrentBudget() (used, max int)  // Exposed for scaler + dashboard
func (rl *RateLimiter) Utilization() float64              // 0.0-1.0, for dashboard gauge
```

**Throttle thresholds:**
- **0-270 used (0-90%)**: All priorities allowed freely
- **270-295 used (90-98%)**: Only PriorityHigh and PriorityNormal allowed; PriorityLow blocks
- **295-300 used (98-100%)**: Only PriorityHigh allowed (trade executions with expiring quotes)
- **300 reached**: All requests block until the 60s window rolls over
- **429 received**: Emergency backoff — all requests block until `Retry-After` elapses

### 1.3 — Base HTTP Client (`internal/api/client.go`)

The client is designed for the multi-company architecture: one shared JWT token, per-call company ID, all calls going through the shared rate limiter.

```go
type Client struct {
    baseURL     string
    httpClient  *http.Client
    token       string        // JWT shared across all companies
    companyID   string        // UUID — each CompanyRunner sets this on its own Client clone
    rateLimiter *RateLimiter  // Shared across all clients
    mu          sync.RWMutex
    logger      *zap.Logger
}

// ForCompany creates a lightweight copy with a specific company ID.
// Shares the same httpClient, token, AND rateLimiter.
func (c *Client) ForCompany(companyID string) *Client {
    return &Client{
        baseURL:     c.baseURL,
        httpClient:  c.httpClient,
        token:       c.token,
        companyID:   companyID,
        rateLimiter: c.rateLimiter,  // Same limiter — all companies share the budget
        logger:      c.logger.With(zap.String("company_id", companyID)),
    }
}
```

Features:
- All requests go through `do(ctx, method, path, body, result, priority)` which:
  - Calls `rateLimiter.Acquire(ctx, priority)` before making the request
  - Sets auth and company headers
  - Handles `{ data: ... }` response envelope
  - Calls `rateLimiter.UpdateFromResponse(resp)` after every response
  - On 429: sets `backoffUntil` and retries after the indicated delay
  - On 5xx: exponential backoff retry (max 3)
  - Logs every request/response via zap at debug level
- Cursor-based pagination helper that auto-fetches all pages

### 1.4 — Endpoint Modules

Each file covers one API domain. All methods accept a `context.Context` and `Priority`, and return `(result, error)`:

**`auth.go`**:
- `Register(ctx, req) (*Player, error)`
- `Login(ctx, email, password) (token, error)`
- `Revoke(ctx) error`
- `Me(ctx) (*Player, error)`

**`company.go`**:
- `CreateCompany(ctx, req) (*Company, error)`
- `ListMyCompanies(ctx) ([]Company, error)`
- `GetCompany(ctx) (*Company, error)`
- `GetEconomy(ctx) (*CompanyEconomy, error)`
- `GetLedger(ctx, cursor) ([]LedgerEntry, error)`

**`world.go`** (no auth needed):
- `ListPorts(ctx, filters) ([]Port, error)`
- `GetPort(ctx, id) (*Port, error)`
- `ListGoods(ctx, category) ([]Good, error)`
- `ListRoutes(ctx, filters) ([]Route, error)` — handles pagination
- `ListShipTypes(ctx) ([]ShipType, error)`

**`trade.go`**:
- `GetQuote(ctx, req) (*Quote, error)` — PriorityHigh
- `ExecuteQuote(ctx, req) (*TradeExecution, error)` — PriorityHigh
- `DirectTrade(ctx, req) (*TradeExecution, error)` — PriorityHigh
- `BatchQuotes(ctx, reqs) ([]QuoteResult, error)` — PriorityHigh
- `BatchExecuteQuotes(ctx, reqs) ([]ExecutionResult, error)` — PriorityHigh
- `ListTraders(ctx) ([]Trader, error)` — PriorityLow
- `ListTraderPositions(ctx, traderID) ([]TraderPosition, error)` — PriorityLow

**`market.go`**:
- `ListOrders(ctx, filters) ([]Order, error)`
- `PostOrder(ctx, req) (*Order, error)`
- `FillOrder(ctx, orderID, quantity) (*Order, error)` — PriorityHigh
- `CancelOrder(ctx, orderID) error`
- `GetBlendedPrice(ctx, portID, goodID, side, quantity) (float64, error)`

**`fleet.go`**:
- `ListShips(ctx) ([]Ship, error)`
- `GetShip(ctx, id) (*Ship, error)`
- `RenameShip(ctx, id, name) (*Ship, error)`
- `GetShipInventory(ctx, id) ([]Cargo, error)`
- `SendTransit(ctx, shipID, routeID) (*Ship, error)` — PriorityHigh
- `GetTransitLogs(ctx, shipID) ([]TransitLog, error)`
- `TransferToWarehouse(ctx, shipID, req) error`

**`warehouse.go`**:
- `BuyWarehouse(ctx, portID) (*Warehouse, error)`
- `ListWarehouses(ctx) ([]Warehouse, error)`
- `GetWarehouse(ctx, id) (*Warehouse, error)`
- `GetWarehouseInventory(ctx, id) ([]WarehouseInventory, error)`
- `GrowWarehouse(ctx, id) (*Warehouse, error)`
- `ShrinkWarehouse(ctx, id) (*Warehouse, error)`
- `TransferToShip(ctx, warehouseID, req) error`

**`shipyard.go`**:
- `FindShipyard(ctx, portID) (*Shipyard, error)`
- `GetShipyardInventory(ctx, shipyardID) ([]ShipyardInventory, error)`
- `BuyShip(ctx, shipyardID, shipTypeID) (*Ship, error)`

### 1.5 — SSE Event Client (`internal/api/events.go`)

```go
type EventStream struct {
    client   *Client
    onEvent  func(SSEEvent)
    cancel   context.CancelFunc
    logger   *zap.Logger
}

func (c *Client) SubscribeWorldEvents(ctx context.Context, handler func(SSEEvent)) *EventStream
func (c *Client) SubscribeCompanyEvents(ctx context.Context, handler func(SSEEvent)) *EventStream
```

- SSE connections are long-lived and do NOT count against rate limits
- Auto-reconnects on disconnect with backoff
- Each company subscribes to its own company event stream
- One shared world event stream feeds the `WorldCache`

---

## Step 2: Database Setup & Data Models

### 2.1 — Configuration (`internal/config/config.go`)

Load from `.env` file using `github.com/joho/godotenv`. Provides an `fx.Option` that supplies `*Config` to the DI container.

```env
# .env
DB_HOST=localhost
DB_PORT=5432
DB_USER=tradewinds
DB_PASSWORD=secret
DB_NAME=tradewinds
DB_SSLMODE=disable

BOT_BASE_URL=https://tradewinds.fly.dev
API_PORT=3001

# Single game account
PLAYER_EMAIL=player@example.com
PLAYER_PASSWORD=secretpass

# Strategy allocation — strategy_name:company_count
# Total companies must fit within rate limit budget (auto-capped if too many)
STRATEGY_ALLOCATION=arbitrage:3,bulk_hauler:2,market_maker:2

# Rate limit: 300 req/60s per IP (game spec). Override only if changed.
# RATE_LIMIT_PER_MINUTE=300

# AI Agent configuration
AGENT_TYPE=heuristic
# LLM_PROVIDER=claude
# LLM_MODEL=claude-sonnet-4-6
# LLM_API_KEY=sk-ant-...
# LLM_MAX_TOKENS=4096
# COMPOSITE_FAST_AGENT=heuristic
# COMPOSITE_SLOW_AGENT=llm
```

Config struct:
```go
type Config struct {
    DB                 DBConfig
    BaseURL            string
    APIPort            int
    PlayerEmail        string
    PlayerPassword     string
    StrategyAllocation []StrategyAlloc  // parsed from STRATEGY_ALLOCATION
    RateLimitPerMinute int              // default 300 (game spec)
    Agent              AgentConfig
}

type StrategyAlloc struct {
    Strategy string
    Count    int
}

type AgentConfig struct {
    Type              string  // "heuristic", "llm", "composite"
    LLMProvider       string
    LLMModel          string
    LLMAPIKey         string
    LLMMaxTokens      int
    CompositeFastAgent string
    CompositeSlowAgent string
}
```

### 2.2 — Database Connection (`internal/db/db.go`)

```go
func Module() fx.Option // provides *gorm.DB
```

- Uses `gorm.io/driver/postgres`
- Calls `db.AutoMigrate(...)` for all models on startup
- Sets connection pool limits (max open=25, max idle=5, lifetime=5min)
- Uses `fx.Lifecycle` hooks for connect on start and close on stop

### 2.3 — GORM Models (`internal/db/models.go`)

```go
// CompanyRecord tracks each bot-managed company
type CompanyRecord struct {
    ID          uint           `gorm:"primaryKey"`
    GameID      string         `gorm:"uniqueIndex"` // Game UUID of the company
    Name        string
    Ticker      string
    HomePortID  string
    Strategy    string         // Current strategy name
    Status      string         // "running", "paused", "error", "bankrupt"
    Treasury    int64          // Last known treasury
    Reputation  int64
    CreatedAt   time.Time
    UpdatedAt   time.Time
}

// TradeLog records every trade executed
type TradeLog struct {
    ID         uint           `gorm:"primaryKey"`
    CompanyID  uint           `gorm:"index"`
    Action     string         // "buy" or "sell"
    GoodID     string
    GoodName   string
    PortID     string
    PortName   string
    Quantity   int
    UnitPrice  int
    TotalPrice int
    TaxPaid    int
    Strategy   string
    AgentName  string         // Which agent made this decision
    CreatedAt  time.Time
}

// PnLSnapshot periodic profit/loss snapshots per company
type PnLSnapshot struct {
    ID         uint           `gorm:"primaryKey"`
    CompanyID  uint           `gorm:"index"`
    Treasury   int64
    TotalCosts int64
    TotalRev   int64
    NetPnL     int64
    ShipCount  int
    CreatedAt  time.Time
}

// InventorySnapshot tracks cargo/warehouse state over time
type InventorySnapshot struct {
    ID         uint           `gorm:"primaryKey"`
    CompanyID  uint           `gorm:"index"`
    Location   string         // "ship:<id>" or "warehouse:<id>"
    GoodID     string
    GoodName   string
    Quantity   int
    CreatedAt  time.Time
}

// StrategyMetric tracks per-strategy performance (aggregated across companies)
type StrategyMetric struct {
    ID                uint           `gorm:"primaryKey"`
    StrategyName      string         `gorm:"index"`
    CompanyCount      int            // How many companies ran this strategy
    TradesExecuted    int
    TotalProfit       int64
    TotalLoss         int64
    AvgProfitPerTrade float64
    StdDevProfit      float64        // Standard deviation across companies
    WinRate           float64
    ConfidenceLow     float64        // 95% CI lower bound on profit/hour
    ConfidenceHigh    float64        // 95% CI upper bound
    PeriodStart       time.Time
    PeriodEnd         time.Time
    CreatedAt         time.Time
}

// CompanyLog stores log lines for dashboard streaming
type CompanyLog struct {
    ID        uint           `gorm:"primaryKey"`
    CompanyID uint           `gorm:"index"`
    Level     string         // "info", "warn", "error", "trade", "event", "optimizer", "agent"
    Message   string
    CreatedAt time.Time      `gorm:"index"`
}

// PriceObservation records NPC prices for trend analysis
type PriceObservation struct {
    ID        uint           `gorm:"primaryKey"`
    PortID    string         `gorm:"index:idx_price_port_good"`
    GoodID    string         `gorm:"index:idx_price_port_good"`
    BuyPrice  int
    SellPrice int
    CreatedAt time.Time
}

// AgentDecisionLog records every decision made by an agent
type AgentDecisionLog struct {
    ID           uint           `gorm:"primaryKey"`
    CompanyID    uint           `gorm:"index"`
    AgentName    string
    DecisionType string         // "trade", "fleet", "market", "strategy_eval"
    Request      string         `gorm:"type:text"` // JSON snapshot of game state
    Response     string         `gorm:"type:text"` // JSON decision
    Reasoning    string         `gorm:"type:text"`
    Confidence   float64
    LatencyMs    int64
    Outcome      string         // Filled in later: "profit", "loss", "neutral"
    OutcomeValue int64
    CreatedAt    time.Time
}
```

**Retention:** Background goroutine prunes `CompanyLog` >24h, `PriceObservation` >7d, `AgentDecisionLog` >30d.

---

## Step 3: Core Bot & Concurrency

### 3.1 — Dependency Injection with uber/fx

```go
func main() {
    fx.New(
        config.Module,        // Provides *Config
        db.Module,            // Provides *gorm.DB
        logging.Module,       // Provides *zap.Logger
        agent.Module,         // Provides Agent (based on config)
        bot.Module,           // Provides *Manager
        strategy.Module,      // Provides strategy registry
        optimizer.Module,     // Provides *optimizer.Engine
        server.Module,        // Provides *fiber.App (API server)
    ).Run()
}
```

`fx.Lifecycle` hooks handle startup/shutdown ordering:
1. Config loads first
2. Logger initializes
3. DB connects + migrates
4. Agent initializes (connects to LLM if configured)
5. Bot manager starts (logs in, discovers rate limits, sizes companies, spawns goroutines)
6. Optimizer starts
7. Fiber server starts
8. On shutdown: reverse order

### 3.2 — Logging with uber/zap

```go
func Module() fx.Option {
    return fx.Provide(func() (*zap.Logger, error) {
        cfg := zap.NewProductionConfig()
        cfg.OutputPaths = []string{"stdout"}
        cfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
        return cfg.Build()
    })
}
```

- Each company gets a child logger: `logger.With(zap.String("company", name), zap.String("ticker", ticker), zap.String("strategy", strategy))`
- The `CompanyLogger` wraps zap to also persist to DB and broadcast to SSE

### 3.3 — Company Scaler (`internal/bot/scaler.go`)

Calculates the safe number of companies based on rate limit budget:

```go
type Scaler struct {
    rateLimiter *RateLimiter
    logger      *zap.Logger
}

type ScaledAllocation struct {
    Strategy     string
    Count        int      // Actual count (may be less than requested)
    HomePorts    []string // Diversified across ports
}

func (s *Scaler) CalculateAllocation(
    requested []StrategyAlloc,
    rateBudget float64,
) []ScaledAllocation {
    // 1. Calculate shared overhead (scanner, world cache)
    // 2. Calculate per-company steady-state API cost
    // 3. available = rateBudget - sharedOverhead
    // 4. maxCompanies = available / perCompanyCost
    // 5. If maxCompanies >= totalRequested: use as-is
    // 6. Else: scale down proportionally, minimum 2 per strategy (or 1 if budget is very tight)
    // 7. Assign diversified home ports to each company
}
```

Home port assignment ensures companies within the same strategy start at different ports:
- With 15 ports and 3 arbitrage companies → each starts at a different port
- This diversifies route coverage and avoids all companies competing for the same goods at the same port

### 3.4 — Multi-Company Manager (`internal/bot/manager.go`)

```go
type Manager struct {
    cfg         *config.Config
    db          *gorm.DB
    logger      *zap.Logger
    baseClient  *api.Client
    rateLimiter *api.RateLimiter
    worldData   *WorldCache
    priceCache  *PriceCache       // Shared across all companies
    agent       agent.Agent       // Shared agent instance
    companies   map[string]*CompanyRunner
    scaler      *Scaler
    optimizer   *optimizer.Engine
    mu          sync.RWMutex
    ctx         context.Context
    cancel      context.CancelFunc
}
```

**Lifecycle (registered via `fx.Lifecycle`):**
1. **OnStart**:
   a. Creates `RateLimiter` with the known 300 req/60s limit
   b. Creates base `api.Client` with shared rate limiter, logs in **once**
   c. Calls `GET /me` to verify auth
   d. Runs `Scaler.CalculateAllocation()` to determine company count (validates against 300/min budget)
   f. Fetches shared world data — stored in `WorldCache`
   g. Calls `GET /me/companies` to find existing companies
   h. For each company in the scaled allocation:
      - Checks if company exists (by ticker pattern: `{STRATEGY}{N}`, e.g., ARB1, ARB2, BLK1)
      - If not, creates via `POST /companies` with a jittered delay between creations
      - Creates company-scoped client: `baseClient.ForCompany(companyID)`
      - Spawns `go runner.Run(ctx)` with a random start delay (0-10s jitter)
   i. Starts shared price scanner goroutine (one for all companies)
   j. Starts one world SSE listener
2. **OnStop**: Cancels context, waits for all goroutines

### 3.5 — Company Runner (`internal/bot/company_runner.go`)

```go
type CompanyRunner struct {
    cfg        config.CompanyConfig
    client     *api.Client       // Company-scoped (shares token + rate limiter)
    db         *gorm.DB
    world      *WorldCache
    priceCache *PriceCache       // Shared, read-only
    state      *CompanyState
    strategy   strategy.Strategy
    agent      agent.Agent       // Shared agent instance
    logger     *CompanyLogger
    ctx        context.Context
    strategyCh chan strategy.Strategy
    dbRecord   *db.CompanyRecord
}
```

**Main loop (event-driven + periodic):**

```
func (r *CompanyRunner) Run(ctx context.Context):
    1. Initial setup: fetch ships, warehouses, inventory
    2. Subscribe to company SSE events (long-lived, no rate limit cost)
    3. Start periodic ticker (every 60s + jitter) for:
       - Refresh economy data (treasury, upkeep)
       - Take P&L snapshot
    4. Main select loop:
       case <-companyEvent:
           if ship_docked → strategy.OnShipArrival(ship, port)
           if trade_complete → log trade, update state
       case <-ticker:
           strategy.OnTick(state)
           record P&L snapshot
       case newStrategy := <-strategyCh:
           swap to new strategy assigned by optimizer
       case <-ctx.Done():
           clean shutdown
```

### 3.6 — Per-Company State (`internal/bot/state.go`)

```go
type CompanyState struct {
    CompanyID     uuid.UUID
    Treasury      int64
    Reputation    int64
    Ships         map[uuid.UUID]*ShipState
    Warehouses    map[uuid.UUID]*WarehouseState
    TotalUpkeep   int64
    LastEconomy   time.Time
    mu            sync.RWMutex
}

type ShipState struct {
    Ship      api.Ship
    Cargo     []api.Cargo
    Capacity  int
    Speed     int
    Status    string
    ArrivesAt *time.Time
    AssignedRoute *TradeRoute
}
```

### 3.7 — Company Logger

```go
type CompanyLogger struct {
    zap         *zap.Logger
    companyDBID uint
    db          *gorm.DB
    ringBuffer  *ring.Buffer[LogEntry]
    subscribers []chan LogEntry
    mu          sync.RWMutex
}
```

---

## Step 4: Strategy & Auto-Optimization Engine

### 4.1 — Strategy Interface (`internal/strategy/strategy.go`)

Strategies are now thin wrappers that gather state, call the Agent, and execute decisions:

```go
type Strategy interface {
    Name() string
    Init(ctx StrategyContext) error
    OnShipArrival(ship *ShipState, port Port) error
    OnTick(state *CompanyState) error
    Shutdown() error
}

type StrategyContext struct {
    Client     *api.Client
    State      *CompanyState
    World      *WorldCache
    PriceCache *PriceCache
    Agent      agent.Agent       // Pluggable decision maker
    Logger     *CompanyLogger
    DB         *gorm.DB
}
```

### 4.2 — Arbitrage Strategy (`internal/strategy/arbitrage.go`)

**Logic flow:**

1. **OnShipArrival(ship, port)**:
   - Gather full game state snapshot (prices, fleet, warehouses, routes)
   - Build `TradeDecisionRequest`
   - Call `agent.DecideTradeAction(ctx, request)`
   - Log the agent's reasoning
   - Execute the returned `TradeDecision`:
     - Sell cargo if instructed → batch quote + execute (PriorityHigh)
     - Buy new cargo if instructed → batch quote + execute (PriorityHigh)
     - Send ship to destination → transit (PriorityHigh)
   - Record `AgentDecisionLog` with the decision and state snapshot

2. **OnTick(state)**:
   - If sufficient time has passed, call `agent.DecideFleetAction(ctx, request)`
   - Execute fleet decisions (buy ships, warehouses)
   - Multi-ship coordination: mark routes as "claimed" in state

3. **Safety checks (always enforced, regardless of agent):**
   - Never spend below treasury floor (2x upcoming upkeep)
   - Never execute a trade that would result in a loss after taxes
   - Never send ship to a port where another ship is already heading with the same good

### 4.3 — Market Maker Strategy (`internal/strategy/market_maker.go`)

Same pattern: gathers state → calls `agent.DecideMarketAction()` → executes decisions.

### 4.4 — Bulk Hauler Strategy (`internal/strategy/bulk_hauler.go`)

Same pattern: gathers state → calls `agent.DecideTradeAction()` → executes with focus on luxury goods + galleons.

### 4.5 — Price Scanner (`internal/strategy/scanner.go`)

Shared across all companies — runs as a single goroutine to avoid duplicate API calls:

```go
type PriceCache struct {
    prices map[string]PricePoint  // key: "portID:goodID"
    mu     sync.RWMutex
}
```

- Rotates through all 15 ports, batch-quoting all goods
- Uses PriorityLow for all API calls (yields to trade executions)
- Full scan cycle: 15 API calls → 210 price points
- Scan interval adapts to rate limit pressure: speeds up when budget is available, slows down when utilization is high
- Only one company's client is used for scanning (the first arbitrage company, chosen at startup)

### 4.6 — Auto-Optimization Engine (`internal/optimizer/engine.go`)

The optimizer now has **multiple data points per strategy** (multiple companies) for statistical rigor.

```go
type Engine struct {
    db        *gorm.DB
    manager   *Manager
    agent     agent.Agent       // Can delegate evaluation to AI agent
    logger    *zap.Logger
    interval  time.Duration     // Default: 30 minutes
}
```

**Evaluation cycle (every 30 minutes):**

1. **Collect Metrics per Company** (`metrics.go`):
   - For each company, compute: trades, profit, loss, avg profit/trade, win rate, profit/hour
   - Query `PnLSnapshot` for treasury trend

2. **Aggregate per Strategy** (statistical analysis):
   - Group companies by strategy
   - Compute: mean profit/hour, standard deviation, 95% confidence interval
   - With 3 companies per strategy, the CI is meaningful; with 1 it's just a point estimate
   - Record as `StrategyMetric` with `CompanyCount`, `StdDevProfit`, `ConfidenceLow`, `ConfidenceHigh`

3. **Score Strategies** (using confidence intervals):
   - Primary: lower bound of 95% CI on profit/hour (conservative — rewards consistency)
   - Secondary: mean profit/hour, win rate
   - Score = `0.5 * CI_lower_bound + 0.3 * mean_profit_per_hour + 0.2 * win_rate`

4. **Reallocation Decisions**:
   - If a strategy's CI upper bound is below another strategy's CI lower bound for 2 consecutive periods → statistically significant underperformance
   - Auto-switch the worst company of the underperforming strategy to the top strategy
   - Call `agent.EvaluateStrategy()` for parameter tuning recommendations
   - **Constraints**:
     - Keep at least 2 companies per active strategy (preserve sample size)
     - If a strategy has only 2 companies and is underperforming, switch 1 but log a warning about reduced sample size
     - Never switch mid-trade-cycle
     - Rate limit aware: don't add companies if budget is >80% utilized

5. **Dynamic Company Scaling**:
   - If rate limit utilization is consistently <50% for 3 evaluation periods, the optimizer can recommend adding a new company to the best-performing strategy (up to the config maximum)
   - If utilization is consistently >90%, recommend pausing the worst-performing company

---

## Step 5: Web Dashboard

### 5.1 — Go-Fiber API Server (`internal/server/`)

```go
func Module() fx.Option {
    return fx.Options(
        fx.Provide(NewServer),
        fx.Invoke(RegisterRoutes),
    )
}

func NewServer(cfg *config.Config, logger *zap.Logger) *fiber.App {
    app := fiber.New(fiber.Config{
        AppName: "Tradewinds Bot API",
    })
    app.Use(cors.New())
    app.Use(fiberlogger.New())
    return app
}
```

**REST API routes:**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/companies` | List all companies with status, treasury, strategy |
| GET | `/api/companies/:id/pnl` | P&L time series |
| GET | `/api/companies/:id/trades` | Trade log (paginated) |
| GET | `/api/companies/:id/inventory` | Current inventory |
| GET | `/api/companies/:id/logs` | Historical logs (paginated) |
| GET | `/api/companies/:id/decisions` | Agent decision log with reasoning |
| GET | `/api/strategy-metrics` | Aggregated strategy metrics with CIs |
| GET | `/api/prices` | Latest price observations |
| GET | `/api/optimizer/log` | Optimizer decision history |
| GET | `/api/ratelimit` | Current rate limit utilization and budget |
| GET | `/api/health` | Bot health status |

**SSE endpoints:**

| Method | Path | Description |
|--------|------|-------------|
| GET | `/sse/logs/:id` | Live log stream for a company |
| GET | `/sse/pnl` | Live P&L updates for all companies |

### 5.2 — Nuxt 4 Dashboard (`dashboard/`)

**`nuxt.config.ts`:**
```ts
export default defineNuxtConfig({
    ssr: false,
    modules: [
        '@nuxtjs/tailwindcss',
        '@nuxt/icon',
        'nuxt-charts',
    ],
    runtimeConfig: {
        public: {
            apiBase: 'http://localhost:3001',
        },
    },
    icon: {
        serverBundle: 'remote',
    },
    compatibilityDate: '2025-01-01',
})
```

**`package.json` dependencies:**
```json
{
    "dependencies": {
        "nuxt": "^4.0.0",
        "vue": "^3.5.0",
        "@nuxtjs/tailwindcss": "^7.0.0",
        "@nuxt/icon": "^1.10.0",
        "nuxt-charts": "latest",
        "@iconify-json/mdi": "latest",
        "@iconify-json/heroicons": "latest",
        "@iconify-json/lucide": "latest",
        "@iconify/vue": "^4.0.0"
    }
}
```

### 5.3 — Dashboard Layout

```
┌──────────────────────────────────────────────────────────────┐
│  TRADEWINDS BOT DASHBOARD       [API: ████░░ 68%]  [● OK]  │
├──────────────┬───────────────────────────────────────────────┤
│  Companies   │  Selected Company Detail                      │
│              │                                               │
│  ARBITRAGE   │  ┌─ Header ──────────────────────────────┐   │
│  ● ARB1 3.2k │  │ ARB1 — Arbitrage Co 1                 │   │
│  ● ARB2 4.1k │  │ Strategy: arbitrage  Agent: heuristic  │   │
│  ● ARB3 2.8k │  │ Treasury: 3,201  Upkeep/cycle: 1,000  │   │
│              │  │ Ships: 2  Warehouses: 0                │   │
│  BULK HAULER │  └───────────────────────────────────────┘   │
│  ● BLK1 5.5k │                                               │
│  ● BLK2 6.2k │  ┌─ P&L Chart ──────────────────────────┐   │
│              │  │  [Line: treasury over time]            │   │
│  MARKET MAKER│  │  [Overlay: all ARB companies]          │   │
│  ● MKT1 1.9k │  └───────────────────────────────────────┘   │
│  ● MKT2 2.1k │                                               │
│              │  ┌─ Agent Decisions ─────────────────────┐   │
│              │  │  12:01 BUY 50 Grain @52 → Plymouth    │   │
│              │  │  "Best arb: +17/unit, 1.2h travel,    │   │
│              │  │   profit/hr=708. 2nd best was Wool    │   │
│              │  │   at +12/unit but 2.1h travel."       │   │
│              │  │  Confidence: 0.87                      │   │
│              │  └───────────────────────────────────────┘   │
│              │                                               │
│              │  ┌─ Live Logs ──────────────────────────┐    │
│              │  │  (SSE stream, color-coded)            │    │
│              │  └──────────────────────────────────────┘    │
├──────────────┴───────────────────────────────────────────────┤
│  Strategy Comparison (aggregated across companies)           │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  [Bar chart: mean profit/hr ± CI by strategy]         │   │
│  │  [Table: strategy, companies, trades, mean, σ, CI]    │   │
│  └──────────────────────────────────────────────────────┘   │
│  Optimizer: "ARB outperforms BLK (CI: 680-720 vs 410-510)  │
│   → Switching BLK2 to arbitrage strategy"                    │
└──────────────────────────────────────────────────────────────┘
```

### 5.4 — Components (under `app/components/`)

| Component | Description |
|-----------|-------------|
| `CompanySidebar.vue` | Groups companies by strategy. Status dots. Treasury. Click to select. Uses `<Icon name="mdi:ship" />`, `<Icon name="lucide:coins" />`. |
| `CompanyDetail.vue` | Header card: company name, strategy, agent type, treasury, upkeep, fleet size. |
| `PnLChart.vue` | `<Chart type="line">` — can overlay all companies of the same strategy for comparison. Auto-updates via SSE. |
| `InventoryChart.vue` | `<Chart type="bar">` stacked. Refreshes every 60s. |
| `LiveLogs.vue` | SSE-connected log stream. Color-coded by level. Pause toggle. |
| `AgentDecisions.vue` | Shows recent agent decisions with reasoning text, confidence score, and eventual outcome (profit/loss badge). Expandable to see full state snapshot. |
| `StrategyComparison.vue` | `<Chart type="bar">` with error bars (confidence intervals). Table with strategy name, company count, mean, std dev, CI bounds. |
| `OptimizerLog.vue` | Timeline of optimizer decisions with icons. |
| `RateLimitGauge.vue` | Circular gauge showing current API budget utilization against the 300/min limit (0-100%). Changes color: green <60%, yellow 60-80%, red >80%. Shows used/300 counter. |

### 5.5 — Composables (under `app/composables/`)

**`useCompanies.ts`:**
```ts
export function useCompanies() {
    const config = useRuntimeConfig()
    const companies = ref<Company[]>([])
    const selected = ref<Company | null>(null)
    const grouped = computed(() => groupByStrategy(companies.value))

    async function fetchCompanies() {
        companies.value = await $fetch<Company[]>(`${config.public.apiBase}/api/companies`)
    }

    return { companies, selected, grouped, fetchCompanies }
}
```

**`usePnL.ts`:**
```ts
export function usePnL(companyId: Ref<number>) {
    const config = useRuntimeConfig()
    const series = ref<PnLPoint[]>([])
    let eventSource: EventSource | null = null

    function connect() {
        eventSource = new EventSource(`${config.public.apiBase}/sse/pnl`)
        eventSource.onmessage = (e) => {
            const update = JSON.parse(e.data)
            if (update.company_id === companyId.value) {
                series.value.push(update)
            }
        }
    }

    onUnmounted(() => eventSource?.close())
    return { series, connect }
}
```

**`useLogs.ts`:**
```ts
export function useLogs(companyId: Ref<number>) {
    const config = useRuntimeConfig()
    const logs = ref<LogEntry[]>([])
    const paused = ref(false)
    let eventSource: EventSource | null = null

    function connect() {
        eventSource = new EventSource(`${config.public.apiBase}/sse/logs/${companyId.value}`)
        eventSource.onmessage = (e) => {
            if (!paused.value) {
                logs.value.push(JSON.parse(e.data))
                if (logs.value.length > 500) logs.value.shift()
            }
        }
    }

    onUnmounted(() => eventSource?.close())
    return { logs, paused, connect }
}
```

**`useAgent.ts`:**
```ts
export function useAgentDecisions(companyId: Ref<number>) {
    const config = useRuntimeConfig()
    const decisions = ref<AgentDecision[]>([])

    async function fetchDecisions(limit = 20) {
        decisions.value = await $fetch<AgentDecision[]>(
            `${config.public.apiBase}/api/companies/${companyId.value}/decisions`,
            { params: { limit } }
        )
    }

    return { decisions, fetchDecisions }
}
```

### 5.6 — TypeScript Types (`app/types/index.ts`)

```ts
interface Company {
    id: number
    game_id: string
    name: string
    ticker: string
    strategy: string
    status: 'running' | 'paused' | 'error' | 'bankrupt'
    treasury: number
    reputation: number
    agent_name: string
}

interface PnLPoint {
    timestamp: string
    treasury: number
    net_pnl: number
    ship_count: number
}

interface TradeLog {
    id: number
    action: 'buy' | 'sell'
    good_name: string
    port_name: string
    quantity: number
    unit_price: number
    total_price: number
    strategy: string
    agent_name: string
    created_at: string
}

interface LogEntry {
    level: 'info' | 'warn' | 'error' | 'trade' | 'event' | 'optimizer' | 'agent'
    message: string
    created_at: string
}

interface StrategyMetric {
    strategy_name: string
    company_count: number
    trades_executed: number
    total_profit: number
    avg_profit_per_trade: number
    std_dev_profit: number
    win_rate: number
    confidence_low: number
    confidence_high: number
    period_start: string
    period_end: string
}

interface AgentDecision {
    id: number
    agent_name: string
    decision_type: 'trade' | 'fleet' | 'market' | 'strategy_eval'
    reasoning: string
    confidence: number
    latency_ms: number
    outcome: 'profit' | 'loss' | 'neutral' | 'pending'
    outcome_value: number
    created_at: string
}

interface RateLimitStatus {
    max_per_minute: number
    current_utilization: number
    remaining: number
    active_companies: number
    budget_per_company: number
}
```

### 5.7 — TailwindCSS 4 Styling

CSS-first configuration (no `tailwind.config.ts`):

- Dark theme by default
- Color palette: navy/slate backgrounds, emerald for profit, rose for loss
- Cards: `bg-slate-800 rounded-lg shadow-lg border border-slate-700`
- Status: `bg-emerald-500/20 text-emerald-400` (running), `bg-yellow-500/20 text-yellow-400` (paused), `bg-rose-500/20 text-rose-400` (error)
- Strategy group headers in sidebar with subtle color coding
- Confidence bars: gradient from rose (low) to emerald (high)
- Rate limit gauge: green <60%, yellow 60-80%, rose >80%
- TailwindCSS 4's `@theme` directive for custom tokens

---

## Implementation Order

1. **Step 1**: API wrapper + models + rate limiter. Test against live API.
2. **Step 2**: Database models + config + fx wiring. Test migrations.
3. **Step 3**: Bot manager + company runners + scaler. Test with 1 company doing basic trade cycle.
4. **Step 4**: Strategies + agent interface + optimizer. Start with heuristic agent + arbitrage. Add LLM agent later.
5. **Step 5**: Dashboard (Nuxt 4) + Fiber API server. Can be built in parallel with Step 4.

**Go dependencies:**
- `github.com/gofiber/fiber/v2`
- `go.uber.org/fx`
- `go.uber.org/zap`
- `github.com/google/uuid`
- `github.com/joho/godotenv`
- `gorm.io/gorm`
- `gorm.io/driver/postgres`

**Dashboard dependencies:**
- `nuxt@^4.0.0`
- `vue@^3.5.0`
- `@nuxtjs/tailwindcss@^7.0.0`
- `@nuxt/icon@^1.10.0`
- `nuxt-charts@latest`
- `@iconify-json/mdi@latest`
- `@iconify-json/heroicons@latest`
- `@iconify-json/lucide@latest`
- `@iconify/vue@^4.0.0`

---

## Risk Mitigations

| Risk | Mitigation |
|------|------------|
| Bankruptcy from upkeep | Treasury floor: never trade if treasury < 2x upcoming upkeep |
| API rate limiting | Centralized token-bucket limiter enforcing 300 req/60s, priority queue (trades > transit > scanning), auto-scaled company count |
| Too many companies for budget | Scaler caps companies at startup; optimizer can pause companies if utilization >90% |
| Stale quotes | Quote tokens expire in 120s; trade executions get PriorityHigh in rate limiter |
| Price crashes (market shocks) | Re-scan prices after world events; agents receive price history for context |
| Strategy thrashing | Optimizer uses confidence intervals; requires statistically significant underperformance over 2 periods |
| Small sample size | Multiple companies per strategy; CI widens with fewer samples, making the optimizer more conservative |
| AI agent latency | Composite agent uses fast heuristic for time-sensitive trades, slow LLM for strategic decisions |
| AI agent failure | All LLM calls have timeout + fallback to heuristic agent; safety checks enforced regardless of agent |
| DB growth | Auto-prune: logs 24h, prices 7d, agent decisions 30d |
| Company isolation | Each company has its own client (shared token + rate limiter), own state, own goroutine |
| Token expiry | Single login in Manager; re-login on 401 and update all company clients |
