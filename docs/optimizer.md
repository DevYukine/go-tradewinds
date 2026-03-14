# Optimizer

Evaluates strategy performance and reallocates companies between strategies.

## Engine (`internal/optimizer/engine.go`)

### Configuration
- `defaultEvalInterval = 15 min` — Evaluation cycle
- `minPeriodsBeforeSwitch = 2` — Consecutive underperform periods before swap (30 min total)
- `minCompaniesPerStrategy = 2` — Minimum for statistical validity
- `lowUtilThreshold = 0.50` — Scale up below this
- `highUtilThreshold = 0.90` — Scale down above this
- `utilPeriodsBeforeScale = 3` — Consecutive periods before scaling (45 min)

### Evaluation Cycle (`evaluate`)

1. Collect per-company metrics from trade logs
2. Aggregate by strategy (mean, std dev, 95% CI, score)
3. Record strategy metrics to DB
4. Log results
5. **Check inactive companies** — 0 trades + docked ships → swap to best strategy
6. **Check reallocations** — Worst CI_high < best CI_low for 2 periods → swap worst company
7. **Dynamic scaling** — Low utilization → add company; high utilization → pause worst
8. **Agent evaluation** — Ask agent for strategy recommendations

### Inactivity Detection (`checkInactiveCompanies`)
- Companies with 0 trades and docked ships are "stalled"
- Swaps to best-performing strategy to break the stall
- Only one swap per evaluation

### Reallocation Logic (`checkReallocations`)
- Tracks `underperformCount` per strategy
- Increments when worst CI_high < best CI_low
- Resets when CIs overlap (recovery)
- After `minPeriodsBeforeSwitch` consecutive periods: swap worst company in underperforming strategy to best strategy
- Won't reduce below `minCompaniesPerStrategy`

### Dynamic Scaling (`checkDynamicScaling`)
- **Scale up**: `lowUtilCount >= 3` → add company to best strategy (respects max from config)
- **Scale down**: `highUtilCount >= 3` → pause worst company in worst strategy
- Counters reset when utilization returns to healthy range

### Agent Evaluation (`agentEvaluation`)
- Calls `agent.EvaluateStrategy()` with converted metrics
- Logs parameter change recommendations
- Executes strategy switch recommendations

## Metrics (`internal/optimizer/metrics.go`)

### Per-Company Metrics (`companyMetrics`)
| Field | Description |
|-------|-------------|
| `TradesExecuted` | Total trades in eval window |
| `TotalProfit` | Sum of sell revenue (raw) |
| `TotalLoss` | Sum of buy costs (raw) |
| `WinRate` | sells / total trades |
| `ProfitPerHour` | Decay-weighted net profit / hours |
| `AvgTradeProfit` | Net profit / trade count |
| `TradesPerHour` | Trades / hours |
| `CapacityUtil` | From latest PnL snapshot |

### Decay Weighting
```go
func decayWeight(tradeTime, now time.Time) float64 {
    age := now.Sub(tradeTime).Minutes()
    return math.Exp(-0.05 * age) // Half-life ~14 min
}
```
Recent trades (1 min) get weight ~0.95. Trades 14 min old get ~0.5. Applied to ProfitPerHour calculation.

### Strategy Stats (`strategyStats`)
| Field | Description |
|-------|-------------|
| `MeanProfit` | Average profit/hour across companies |
| `StdDevProfit` | Standard deviation of profit/hour |
| `ConfidenceLow/High` | 95% CI using t≈2.0 |
| `MeanWinRate` | Average win rate |
| `MeanTradesPerHour` | Average trading velocity |
| `MeanCapacityUtil` | Average capacity utilization |
| `Score` | Composite score |

### Composite Score Formula
```
Score = 0.35 * CI_lower
      + 0.25 * mean_profit_per_hour
      + 0.20 * mean_win_rate
      + 0.10 * mean_trades_per_hour
      + 0.10 * mean_capacity_util
```
