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
| Status | string | "running" or "paused" |
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
| TaxPaid | int | Tax amount |
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
Completed buy→sell cycle profitability.
| Field | Type | Notes |
|-------|------|-------|
| CompanyID | uint | FK |
| FromPortID, ToPortID | string | Route |
| GoodID | string | Traded good |
| BuyPrice, SellPrice | int | Per-unit prices |
| Quantity | int | Units traded |
| Profit | int | sell_total - buy_total |
| Strategy | string | Active strategy |

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
- **InventorySnapshot** — Cargo/warehouse state over time
- **AgentDecisionLog** — Full request/response/reasoning for each decision

## Retention Pruning (`internal/db/retention.go`)

Background goroutine, checks every 1 hour:
| Table | Max Age |
|-------|---------|
| CompanyLog | 1 day |
| PriceObservation | 7 days |
| AgentDecisionLog | 30 days |

`AllModels()` returns all models for auto-migration.
