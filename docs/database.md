# Database Models & Retention

## Connection (`internal/db/db.go`)

PostgreSQL via GORM with auto-migration.
- Max 25 open connections, 5 idle, 5-minute max lifetime
- Config via `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`, `DB_SSLMODE`

## Models (`internal/db/models.go`)

### CompanyRecord
Bot-managed company state.
| Field | Type | Notes |
|-------|------|-------|
| GameID | string | Unique, game-assigned ID |
| Name, Ticker | string | Company identity |
| HomePortID | string | Starting port |
| Strategy | string | Current strategy name |
| Status | string | "running", "paused", "bankrupt", or "stopped" |
| Treasury, Reputation | int64 | Latest known values |

### TradeLog
Every buy/sell executed.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK to CompanyRecord |
| Action | string | "buy" or "sell" |
| GoodID, GoodName | string | Traded good |
| PortID, PortName | string | Trade location |
| Quantity, UnitPrice, TotalPrice | int | Trade details |
| TaxPaid | int | Tax amount (from API response) |
| ShipID, ShipName | string | Which ship executed the trade |
| Source | string | "port_buy", "port_sell", "warehouse_load", "p2p_fill" |
| DestPortID, DestPortName | string | Intended sell destination (for buy trades) |
| Matched | bool | Whether this buy has been FIFO-matched to a sell |
| Strategy, AgentName | string | Decision context |
| Indexed | company_id + created_at |

### PnLSnapshot
Periodic treasury snapshots.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| Treasury | int64 | Current balance |
| TotalCosts, TotalRev | int64 | Cumulative (TotalRev includes passenger revenue) |
| PassengerRev | int64 | Cumulative passenger bid revenue |
| NetPnL | int64 | Rev - Costs |
| ShipCount | int | Fleet size |
| AvgCapacityUtil | float64 | cargo/capacity ratio |

### PassengerLog
Every passenger boarding executed.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK to CompanyRecord |
| PassengerID | string | Game-assigned passenger group ID |
| Count | int | Number of passengers |
| Bid | int | Revenue earned |
| OriginPortID, OriginPortName | string | Boarding port |
| DestinationPortID, DestinationPortName | string | Destination port |
| ShipID, ShipName | string | Carrying ship |
| Strategy, AgentName | string | Decision context |
| Indexed | company_id + created_at |

### RoutePerformance
Completed buy→sell cycle profitability (FIFO matching).
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| FromPortID, ToPortID | string | Route |
| GoodID | string | Traded good |
| BuyPrice, SellPrice | int | Per-unit prices |
| Quantity | int | Units traded |
| Profit | int | sell_total - buy_total |
| Strategy | string | Active strategy |

### ShipEventLog
Ship purchases and sales.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| ShipID, ShipName | string | Ship identity |
| ShipType | string | Ship type name |
| EventType | string | "purchase" or "sale" |
| Price | int | Purchase/sale price |
| Treasury | int | Treasury after event |
| PortID, PortName | string | Transaction location |
| Strategy, AgentName | string | Decision context |
| Retention | permanent |

### WarehouseEventLog
Warehouse purchases, stores, and loads.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| WarehouseID | string | Warehouse identity |
| PortID, PortName | string | Warehouse location |
| EventType | string | "purchase", "store", or "load" |
| GoodID, GoodName | string | Good involved (empty for purchases) |
| Quantity | int | Units transferred |
| Level | int | Warehouse level (for purchase events) |
| Strategy, AgentName | string | Decision context |
| Retention | permanent |

### P2POrderLog
P2P market order activity.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| OrderID | string | Market order ID |
| OrderType | string | "post", "fill", or "cancel" |
| GoodID, GoodName | string | Good involved |
| PortID, PortName | string | Market location |
| Quantity, Price, TotalValue | int | Order details |
| Strategy, AgentName | string | Decision context |
| Retention | permanent |

### StrategyChangeLog
Strategy swaps by the optimizer.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| FromStrategy, ToStrategy | string | Strategy names |
| Reason | string | Optimizer reason or "manual" |
| Retention | permanent |

### QuoteFailureLog
Failed quote attempts for analysis.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| ShipID | string | Ship that attempted the trade |
| GoodID, GoodName | string | Good involved |
| PortID, PortName | string | Port location |
| Action | string | "buy" or "sell" |
| Quantity | int | Attempted quantity |
| ExpPrice, ActPrice | int | Expected vs actual price |
| Reason | string | "price_moved", "out_of_stock", "api_error" |
| Strategy | string | Active strategy |
| Retention | 7 days |

### StrategyMetric
Aggregated per-strategy performance per eval period.
| Field | Type | Notes |
|-------|------|-------|
| StrategyName | string | Strategy identifier |
| CompanyCount | int | Companies running it |
| TradesExecuted | int | Total trades |
| TotalProfit, TotalLoss | int64 | Aggregated |
| AvgProfitPerTrade | float64 | Net / trades |
| StdDevProfit | float64 | Profit/hour std dev |
| WinRate | float64 | Sell ratio |
| ConfidenceLow/High | float64 | 95% CI |
| PeriodStart/End | time.Time | Eval window |

### Other Models
- **CompanyLog** — Log entries (level, message) for dashboard streaming
- **PriceObservation** — NPC prices per port+good
- **AgentDecisionLog** — Full request/response/reasoning for each decision

## Migrations (`internal/db/db.go`)

SQL migrations via [goose](https://github.com/pressly/goose) with embedded filesystem (`//go:embed migrations/*.sql`).
- Migrations run automatically on startup via `goose.Up()`
- Migration files live in `internal/db/migrations/` using `-- +goose Up` / `-- +goose Down` annotations
- All migrations use `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` for idempotency

## Retention Pruning (`internal/db/retention.go`)

Background goroutine, checks every 1 hour:
| Table | Max Age |
|-------|---------|
| CompanyLog | 1 day |
| PriceObservation | 7 days |
| AgentDecisionLog | 30 days |
| QuoteFailureLog | 7 days |
| All other tables | permanent (no pruning) |
