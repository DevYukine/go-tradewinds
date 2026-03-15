# Optimizer

Evaluates strategy performance, records metrics for the dashboard, recovers inactive companies, and tunes trading parameters. Does **not** switch strategies — companies stay on their configured strategy.

## Engine (`internal/optimizer/engine.go`)

### Configuration
- `defaultEvalInterval = 10 min` — Evaluation cycle
- `metricsLookback = 2 hours` — How far back to look for trade data

### Evaluation Cycle (`evaluate`)

1. Collect per-company metrics from trade logs **and passenger logs**
2. Aggregate by strategy (mean, std dev, 95% CI, score)
3. Record strategy metrics to DB (for dashboard)
4. Log results
5. **Recover inactive companies** — ALL ships docked + 0 trades + 0 passenger revenue → `ForceDispatch()` to re-kick idle ships
6. **Parameter tuning** — Evaluate active experiments, then start new ones

### Inactive Recovery (`recoverInactiveCompanies`)
- Criteria: ALL ships docked AND 0 trades AND 0 passenger revenue in the 2-hour lookback
- Action: Calls `runner.ForceDispatch()` to re-dispatch idle ships through the trade decision loop
- Does NOT swap strategy — the strategy stays, ships just get re-evaluated
- `ForceDispatch()` is a non-blocking signal via a buffered channel on `CompanyRunner`

### Constructor
```go
func NewEngine(gormDB *gorm.DB, logger *zap.Logger, manager *bot.Manager) *Engine
```
No agent or registry dependencies — the optimizer only tunes parameters and recovers inactive companies.

## Metrics (`internal/optimizer/metrics.go`)

### Per-Company Metrics (`companyMetrics`)
| Field | Description |
|-------|-------------|
| `TradesExecuted` | Total trades in eval window |
| `TotalProfit` | Sum of sell revenue + passenger revenue (raw) |
| `TotalLoss` | Sum of buy costs (raw) |
| `PassengerRevenue` | Sum of passenger bids in eval window |
| `WinRate` | sells / total trades |
| `ProfitPerHour` | Decay-weighted net profit (incl. passengers) / hours |
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
Recent trades (1 min) get weight ~0.95. Trades 14 min old get ~0.5. Applied to ProfitPerHour calculation including passenger revenue.

### Strategy Stats (`strategyStats`)
| Field | Description |
|-------|-------------|
| `MeanProfit` | Average profit/hour across companies |
| `StdDevProfit` | Standard deviation of profit/hour |
| `ConfidenceLow/High` | 95% CI using t≈2.0 |
| `MeanWinRate` | Average win rate |
| `MeanTradesPerHour` | Average trading velocity |
| `MeanCapacityUtil` | Average capacity utilization |
| `Score` | Net profit per hour (`MeanProfit`) |

### Score
```
Score = MeanProfit
```
Simple net profit per hour. The tuner just needs a relative comparison between strategies.

## Parameter Tuner (`internal/optimizer/tuner.go`)

Self-learning system that experiments with per-company trading parameters.

### Tunable Parameters
| Parameter | Min | Max | Step% | Description |
|-----------|-----|-----|-------|-------------|
| `MinMarginPct` | 0.03 | 0.30 | 15% | Minimum profit margin to accept a trade |
| `PassengerWeight` | 0.5 | 10.0 | 20% | Weight of passenger revenue in destination scoring |
| `PassengerDestBonus` | 1.5 | 10.0 | 15% | Multiplier for passengers heading to chosen destination |
| `FleetEvalIntervalSec` | 60 | 600 | 20% | How often to evaluate fleet decisions |
| `MarketEvalIntervalSec` | 30 | 300 | 20% | How often to evaluate P2P market |

### Experiment Flow
1. **Pick target**: worst-performing company by profit/hour
2. **Pick param**: least-recently-tuned parameter for that company
3. **Adjust**: change by ±step% (alternating direction each experiment)
4. **Record**: save to `ParamExperimentLog` with status "active"
5. **Wait**: `minExperimentAge = 30 min` (3 eval cycles at 10-min intervals)
6. **Evaluate**: compare profit before/after
   - Better → mark "completed", propagate to peers on same strategy
   - Worse → mark "reverted", restore old value

### Propagation
When an experiment succeeds, the winning parameter value is applied to all other companies running the same strategy.

### DB Models
- `CompanyParams` — per-company tunable parameters (1:1 with CompanyRecord)
- `ParamExperimentLog` — experiment history with status tracking
