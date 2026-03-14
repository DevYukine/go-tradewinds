package optimizer

import (
	"context"
	"sort"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

const (
	defaultEvalInterval = 30 * time.Minute

	// minPeriodsBeforeSwitch requires a strategy to underperform for this many
	// consecutive evaluation periods before triggering a reallocation.
	minPeriodsBeforeSwitch = 2

	// minCompaniesPerStrategy is the minimum to maintain for statistical validity.
	minCompaniesPerStrategy = 2
)

// Module provides the optimizer Engine to the fx DI container.
var Module = fx.Module("optimizer",
	fx.Provide(NewEngine),
	fx.Invoke(RegisterEngine),
)

// Engine evaluates strategy performance across all companies and recommends
// reallocations when one strategy statistically outperforms another.
type Engine struct {
	gormDB   *gorm.DB
	agent    agent.Agent
	logger   *zap.Logger
	interval time.Duration

	// underperformCount tracks consecutive periods a strategy has underperformed.
	underperformCount map[string]int

	// lowUtilCount tracks consecutive periods where rate limit utilization was low.
	lowUtilCount int

	// highUtilCount tracks consecutive periods where rate limit utilization was high.
	highUtilCount int
}

// NewEngine creates a new optimization engine.
func NewEngine(gormDB *gorm.DB, agnt agent.Agent, logger *zap.Logger) *Engine {
	return &Engine{
		gormDB:            gormDB,
		agent:             agnt,
		logger:            logger.Named("optimizer"),
		interval:          defaultEvalInterval,
		underperformCount: make(map[string]int),
	}
}

// RegisterEngine hooks the optimizer into the fx lifecycle.
func RegisterEngine(lc fx.Lifecycle, e *Engine) {
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go e.run(ctx)
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// run is the main evaluation loop.
func (e *Engine) run(ctx context.Context) {
	// Wait before first evaluation to let companies collect data.
	select {
	case <-ctx.Done():
		return
	case <-time.After(e.interval):
	}

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		e.evaluate(ctx)

		select {
		case <-ctx.Done():
			e.logger.Info("optimizer stopped")
			return
		case <-ticker.C:
		}
	}
}

// evaluate runs one evaluation cycle: collect metrics, aggregate, score, decide.
func (e *Engine) evaluate(ctx context.Context) {
	e.logger.Info("running optimization evaluation")

	since := time.Now().Add(-e.interval)

	// 1. Collect per-company metrics.
	var companies []db.CompanyRecord
	e.gormDB.Where("status = ?", "running").Find(&companies)

	if len(companies) == 0 {
		e.logger.Debug("no running companies, skipping evaluation")
		return
	}

	metrics := make([]companyMetrics, len(companies))
	for i, c := range companies {
		metrics[i] = collectCompanyMetrics(e.gormDB, c.ID, c.Strategy, since)
	}

	// 2. Aggregate by strategy.
	stats := aggregateByStrategy(metrics)

	// 3. Record strategy metrics to DB.
	e.recordStrategyMetrics(stats)

	// 4. Log results.
	for _, s := range stats {
		e.logger.Info("strategy performance",
			zap.String("strategy", s.StrategyName),
			zap.Int("companies", s.CompanyCount),
			zap.Int("trades", s.TotalTrades),
			zap.Float64("mean_profit_per_hour", s.MeanProfit),
			zap.Float64("std_dev", s.StdDevProfit),
			zap.Float64("ci_low", s.ConfidenceLow),
			zap.Float64("ci_high", s.ConfidenceHigh),
			zap.Float64("win_rate", s.MeanWinRate),
			zap.Float64("score", s.Score),
		)
	}

	// 5. Check for reallocation opportunities.
	e.checkReallocations(ctx, stats)

	// 6. Ask agent for strategy evaluation.
	e.agentEvaluation(ctx, stats)
}

// checkReallocations looks for statistically significant underperformance.
func (e *Engine) checkReallocations(_ context.Context, stats []strategyStats) {
	if len(stats) < 2 {
		return
	}

	// Sort by score descending.
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Score > stats[j].Score
	})

	best := stats[0]
	worst := stats[len(stats)-1]

	// Check if worst strategy's CI upper bound is below best's CI lower bound.
	if worst.ConfidenceHigh < best.ConfidenceLow {
		e.underperformCount[worst.StrategyName]++
		e.logger.Warn("strategy underperforming",
			zap.String("worst", worst.StrategyName),
			zap.String("best", best.StrategyName),
			zap.Int("consecutive_periods", e.underperformCount[worst.StrategyName]),
			zap.Float64("worst_ci_high", worst.ConfidenceHigh),
			zap.Float64("best_ci_low", best.ConfidenceLow),
		)

		if e.underperformCount[worst.StrategyName] >= minPeriodsBeforeSwitch {
			if worst.CompanyCount > minCompaniesPerStrategy {
				e.logger.Info("recommending reallocation",
					zap.String("from", worst.StrategyName),
					zap.String("to", best.StrategyName),
				)
				// Actual reallocation would be executed via the Manager.
				// For now, log the recommendation.
			} else {
				e.logger.Warn("would reallocate but strategy at minimum company count",
					zap.String("strategy", worst.StrategyName),
					zap.Int("count", worst.CompanyCount),
				)
			}
		}
	} else {
		// Reset underperform counter if not statistically significant.
		e.underperformCount[worst.StrategyName] = 0
	}
}

// agentEvaluation asks the AI agent for strategic recommendations.
func (e *Engine) agentEvaluation(ctx context.Context, stats []strategyStats) {
	req := agent.StrategyEvalRequest{
		Metrics: toAgentMetrics(stats),
	}

	evaluation, err := e.agent.EvaluateStrategy(ctx, req)
	if err != nil {
		e.logger.Error("agent strategy evaluation failed", zap.Error(err))
		return
	}

	if evaluation.Reasoning != "" {
		e.logger.Info("agent strategy evaluation",
			zap.String("reasoning", evaluation.Reasoning),
		)
	}

	if evaluation.SwitchTo != nil {
		e.logger.Info("agent recommends strategy switch",
			zap.String("switch_to", *evaluation.SwitchTo),
		)
	}
}

// recordStrategyMetrics persists aggregated metrics to the database.
func (e *Engine) recordStrategyMetrics(stats []strategyStats) {
	now := time.Now()
	periodStart := now.Add(-e.interval)

	for _, s := range stats {
		var totalProfit, totalLoss int64
		for _, c := range s.Companies {
			totalProfit += c.TotalProfit
			totalLoss += c.TotalLoss
		}

		avgProfitPerTrade := 0.0
		if s.TotalTrades > 0 {
			avgProfitPerTrade = float64(totalProfit-totalLoss) / float64(s.TotalTrades)
		}

		metric := db.StrategyMetric{
			StrategyName:      s.StrategyName,
			CompanyCount:      s.CompanyCount,
			TradesExecuted:    s.TotalTrades,
			TotalProfit:       totalProfit,
			TotalLoss:         totalLoss,
			AvgProfitPerTrade: avgProfitPerTrade,
			StdDevProfit:      s.StdDevProfit,
			WinRate:           s.MeanWinRate,
			ConfidenceLow:     s.ConfidenceLow,
			ConfidenceHigh:    s.ConfidenceHigh,
			PeriodStart:       periodStart,
			PeriodEnd:         now,
		}

		if err := e.gormDB.Create(&metric).Error; err != nil {
			e.logger.Error("failed to record strategy metric",
				zap.String("strategy", s.StrategyName),
				zap.Error(err),
			)
		}
	}
}
