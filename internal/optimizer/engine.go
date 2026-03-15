package optimizer

import (
	"context"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

const (
	defaultEvalInterval = 10 * time.Minute

	// metricsLookback is how far back to look for trade data. A 2-hour window
	// gives ships plenty of time to complete round trips and avoids false
	// inactivity signals from ships in transit.
	metricsLookback = 2 * time.Hour
)

// Module provides the optimizer Engine to the fx DI container.
var Module = fx.Module("optimizer",
	fx.Provide(NewEngine),
	fx.Invoke(RegisterEngine),
)

// Engine evaluates strategy performance across all companies, records metrics
// for the dashboard, tunes parameters, and recovers inactive companies.
type Engine struct {
	gormDB   *gorm.DB
	logger   *zap.Logger
	interval time.Duration
	manager  *bot.Manager
	tuner    *ParameterTuner
}

// NewEngine creates a new optimization engine.
func NewEngine(gormDB *gorm.DB, logger *zap.Logger, manager *bot.Manager) *Engine {
	e := &Engine{
		gormDB:   gormDB,
		logger:   logger.Named("optimizer"),
		interval: defaultEvalInterval,
		manager:  manager,
	}
	e.tuner = NewParameterTuner(gormDB, manager, logger.Named("tuner"))
	return e
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

// evaluate runs one evaluation cycle: collect metrics, aggregate, record,
// recover inactive companies, and tune parameters.
func (e *Engine) evaluate(_ context.Context) {
	e.logger.Debug("running optimization evaluation")

	since := time.Now().Add(-metricsLookback)

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

	// 3. Record strategy metrics to DB (for dashboard).
	e.recordStrategyMetrics(stats)

	// 4. Log results.
	for _, s := range stats {
		e.logger.Debug("strategy performance",
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

	// 5. Recover inactive companies by re-dispatching ships (no strategy swap).
	e.recoverInactiveCompanies(metrics)

	// 6. Parameter tuning: evaluate active experiments, then start new ones.
	e.tuner.EvaluateExperiments(metrics)
	e.tuner.RunExperiment(metrics)
}

// recoverInactiveCompanies detects stalled companies (ALL ships docked, 0
// trades, 0 passenger revenue over the lookback window) and re-dispatches
// their idle ships through the trade decision loop. Does NOT swap strategy.
func (e *Engine) recoverInactiveCompanies(metrics []companyMetrics) {
	for _, m := range metrics {
		if m.TradesExecuted > 0 || m.PassengerRevenue > 0 {
			continue
		}

		// Look up the runner.
		var dbRecord db.CompanyRecord
		if err := e.gormDB.First(&dbRecord, m.CompanyID).Error; err != nil {
			continue
		}

		runner := e.manager.GetRunner(dbRecord.GameID)
		if runner == nil {
			continue
		}

		// Only recover if ALL ships are docked (ships in transit will
		// eventually dock and trigger normal dispatch).
		state := runner.State()
		dockedShips := state.DockedShips()
		state.RLock()
		allShips := len(state.Ships)
		state.RUnlock()
		if allShips == 0 || len(dockedShips) != allShips {
			continue
		}

		e.logger.Info("inactive company detected, re-dispatching idle ships",
			zap.Uint("company_id", m.CompanyID),
			zap.String("strategy", m.Strategy),
			zap.Int("docked_ships", len(dockedShips)),
		)

		runner.ForceDispatch()
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
			e.logger.Warn("failed to record strategy metric",
				zap.String("strategy", s.StrategyName),
				zap.Error(err),
			)
		}
	}
}
