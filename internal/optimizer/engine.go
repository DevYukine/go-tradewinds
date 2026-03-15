package optimizer

import (
	"context"
	"sort"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

const (
	defaultEvalInterval = 5 * time.Minute

	// metricsLookback is how far back to look for trade data. Using a longer
	// window than the eval interval prevents noisy swaps from short dry spells
	// (e.g. ships in transit produce 0 trades in a 5-min window).
	metricsLookback = 30 * time.Minute

	// minPeriodsBeforeSwitch requires a strategy to underperform for this many
	// consecutive evaluation periods before triggering a reallocation.
	// At 5-min intervals, 3 periods = 15 minutes of sustained underperformance.
	minPeriodsBeforeSwitch = 3

	// swapCooldown prevents multiple swaps on the same company in quick succession.
	swapCooldown = 20 * time.Minute

	// minCompaniesPerStrategy is the minimum to maintain for statistical validity.
	minCompaniesPerStrategy = 2

	// lowUtilThreshold is the utilization below which we consider scaling up.
	lowUtilThreshold = 0.50

	// highUtilThreshold is the utilization above which we consider scaling down.
	highUtilThreshold = 0.90

	// utilPeriodsBeforeScale requires utilization to be consistently low/high
	// for this many consecutive evaluation periods before scaling.
	utilPeriodsBeforeScale = 2
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
	manager  *bot.Manager
	registry bot.Registry
	tuner    *ParameterTuner

	// underperformCount tracks consecutive periods a strategy has underperformed.
	underperformCount map[string]int

	// lastSwap tracks when each company was last swapped, preventing rapid oscillation.
	lastSwap map[uint]time.Time

	// lowUtilCount tracks consecutive periods where rate limit utilization was low.
	lowUtilCount int

	// highUtilCount tracks consecutive periods where rate limit utilization was high.
	highUtilCount int
}

// NewEngine creates a new optimization engine.
func NewEngine(gormDB *gorm.DB, agnt agent.Agent, logger *zap.Logger, manager *bot.Manager, registry bot.Registry) *Engine {
	e := &Engine{
		gormDB:            gormDB,
		agent:             agnt,
		logger:            logger.Named("optimizer"),
		interval:          defaultEvalInterval,
		manager:           manager,
		registry:          registry,
		underperformCount: make(map[string]int),
		lastSwap:          make(map[uint]time.Time),
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

// evaluate runs one evaluation cycle: collect metrics, aggregate, score, decide.
func (e *Engine) evaluate(ctx context.Context) {
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

	// 3. Record strategy metrics to DB.
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

	// 4.5. Check for inactive/stalled companies.
	e.checkInactiveCompanies(ctx, metrics, stats)

	// 5. Check for reallocation opportunities.
	e.checkReallocations(ctx, stats)

	// 6. Dynamic company scaling based on rate limit utilization.
	e.checkDynamicScaling(ctx, stats)

	// 7. Ask agent for strategy evaluation.
	e.agentEvaluation(ctx, stats)

	// 8. Parameter tuning: evaluate active experiments, then start new ones.
	e.tuner.EvaluateExperiments(metrics)
	e.tuner.RunExperiment(metrics)
}

// checkDynamicScaling adjusts the number of active companies based on rate
// limit utilization. If utilization is consistently low (<50% for 3 periods),
// it adds a company to the best strategy. If utilization is consistently high
// (>90% for 3 periods), it pauses the worst company.
func (e *Engine) checkDynamicScaling(ctx context.Context, stats []strategyStats) {
	utilization := e.manager.RateLimiter().Utilization()

	e.logger.Debug("rate limit utilization check",
		zap.Float64("utilization", utilization),
		zap.Int("low_util_periods", e.lowUtilCount),
		zap.Int("high_util_periods", e.highUtilCount),
	)

	if utilization < lowUtilThreshold {
		e.lowUtilCount++
		e.highUtilCount = 0
	} else if utilization > highUtilThreshold {
		e.highUtilCount++
		e.lowUtilCount = 0
	} else {
		// Utilization is in the healthy range — reset both counters.
		e.lowUtilCount = 0
		e.highUtilCount = 0
	}

	// Scale up: add a company to the best-performing strategy.
	if e.lowUtilCount >= utilPeriodsBeforeScale && len(stats) > 0 {
		e.lowUtilCount = 0

		// Check we haven't exceeded the configured maximum.
		maxCompanies := e.manager.Cfg().TotalCompanies()
		currentCount := e.manager.CompanyCount()
		if currentCount >= maxCompanies {
			e.logger.Debug("utilization low but already at max configured companies",
				zap.Int("current", currentCount),
				zap.Int("max", maxCompanies),
			)
			return
		}

		// Find the best-performing strategy.
		sort.Slice(stats, func(i, j int) bool {
			return stats[i].Score > stats[j].Score
		})
		bestStrategy := stats[0].StrategyName

		gameID, err := e.manager.AddCompany(ctx, bestStrategy)
		if err != nil {
			e.logger.Error("failed to scale up — add company",
				zap.String("strategy", bestStrategy),
				zap.Error(err),
			)
			return
		}

		e.logger.Info("optimizer scaled up: added company to best strategy",
			zap.String("strategy", bestStrategy),
			zap.String("game_id", gameID),
			zap.Float64("utilization", utilization),
			zap.Int("new_total", e.manager.CompanyCount()),
		)
	}

	// Scale down: pause the worst-performing company.
	if e.highUtilCount >= utilPeriodsBeforeScale {
		e.highUtilCount = 0

		// Don't scale below minimum viable count.
		if e.manager.CompanyCount() <= len(stats) {
			e.logger.Debug("utilization high but already at minimum company count",
				zap.Int("count", e.manager.CompanyCount()),
			)
			return
		}

		// Find the worst-performing strategy with more than minimum companies.
		sort.Slice(stats, func(i, j int) bool {
			return stats[i].Score < stats[j].Score
		})

		for _, s := range stats {
			if s.CompanyCount <= minCompaniesPerStrategy || len(s.Companies) == 0 {
				continue
			}

			// Find the worst company in this strategy.
			worstCompany := s.Companies[0]
			for _, c := range s.Companies[1:] {
				if c.ProfitPerHour < worstCompany.ProfitPerHour {
					worstCompany = c
				}
			}

			var dbRecord db.CompanyRecord
			if err := e.gormDB.First(&dbRecord, worstCompany.CompanyID).Error; err != nil {
				e.logger.Error("failed to find company for scale-down",
					zap.Uint("company_id", worstCompany.CompanyID),
					zap.Error(err),
				)
				continue
			}

			if err := e.manager.PauseCompany(dbRecord.GameID); err != nil {
				e.logger.Error("failed to pause company for scale-down",
					zap.String("game_id", dbRecord.GameID),
					zap.Error(err),
				)
				continue
			}

			e.logger.Info("optimizer scaled down: paused worst company",
				zap.String("company", dbRecord.Name),
				zap.String("strategy", s.StrategyName),
				zap.Float64("profit_per_hour", worstCompany.ProfitPerHour),
				zap.Float64("utilization", utilization),
				zap.Int("remaining", e.manager.CompanyCount()),
			)
			break // Only pause one company per evaluation.
		}
	}
}

// checkInactiveCompanies detects stalled companies (0 trades with docked ships)
// and triggers a strategy swap to break the stall.
func (e *Engine) checkInactiveCompanies(ctx context.Context, metrics []companyMetrics, stats []strategyStats) {
	if len(stats) < 2 {
		return
	}

	// Find best strategy to swap to.
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Score > stats[j].Score
	})
	bestStrategy := stats[0].StrategyName

	for _, m := range metrics {
		// With a 30-minute lookback, 0 trades is a meaningful signal of a stall
		// (not just ships in transit for a few minutes).
		if m.TradesExecuted > 0 || m.PassengerRevenue > 0 {
			continue
		}

		// Check swap cooldown.
		if !e.canSwap(m.CompanyID) {
			continue
		}

		// Check if company has docked ships by looking up its state.
		var dbRecord db.CompanyRecord
		if err := e.gormDB.First(&dbRecord, m.CompanyID).Error; err != nil {
			continue
		}

		runner := e.manager.GetRunner(dbRecord.GameID)
		if runner == nil {
			continue
		}

		// Check if any ships are docked.
		dockedShips := runner.State().DockedShips()
		if len(dockedShips) == 0 {
			continue
		}

		// Company is stalled — has docked ships but no trades.
		// Skip if already on the best strategy.
		if m.Strategy == bestStrategy {
			continue
		}

		e.logger.Info("inactive company detected, triggering strategy swap",
			zap.Uint("company_id", m.CompanyID),
			zap.String("current_strategy", m.Strategy),
			zap.String("swap_to", bestStrategy),
			zap.Int("docked_ships", len(dockedShips)),
		)

		factory, ok := e.registry[bestStrategy]
		if !ok {
			continue
		}

		stratCtx := bot.StrategyContext{
			Client:     e.manager.BaseClient().ForCompany(dbRecord.GameID),
			State:      runner.State(),
			World:      e.manager.WorldData(),
			PriceCache: e.manager.PriceCache(),
			Agent:      e.agent,
			Logger:     runner.Logger(),
			DB:         e.gormDB,
		}

		newStrategy, err := factory(stratCtx)
		if err != nil {
			e.logger.Error("failed to create strategy for inactive swap",
				zap.Error(err),
			)
			continue
		}

		runner.SwapStrategy(newStrategy, "inactive company with docked ships")
		e.recordSwap(m.CompanyID)
		break // Only swap one company per evaluation.
	}
}

// checkReallocations looks for statistically significant underperformance
// and executes strategy swaps on the worst-performing company.
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
		e.logger.Info("strategy underperforming",
			zap.String("worst", worst.StrategyName),
			zap.String("best", best.StrategyName),
			zap.Int("consecutive_periods", e.underperformCount[worst.StrategyName]),
			zap.Float64("worst_ci_high", worst.ConfidenceHigh),
			zap.Float64("best_ci_low", best.ConfidenceLow),
		)

		if e.underperformCount[worst.StrategyName] >= minPeriodsBeforeSwitch {
			if worst.CompanyCount > minCompaniesPerStrategy {
				e.executeReallocation(worst, best)
			} else {
				e.logger.Info("would reallocate but strategy at minimum company count",
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

// executeReallocation finds the worst-performing company in the underperforming
// strategy, creates a new strategy instance of the best strategy, and swaps it.
func (e *Engine) executeReallocation(worst, best strategyStats) {
	// Find the worst-performing company within the underperforming strategy.
	if len(worst.Companies) == 0 {
		e.logger.Error("no companies found in worst strategy during reallocation",
			zap.String("strategy", worst.StrategyName),
		)
		return
	}

	// Find the worst-performing company that isn't on cooldown.
	var worstCompany *companyMetrics
	candidates := make([]companyMetrics, len(worst.Companies))
	copy(candidates, worst.Companies)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ProfitPerHour < candidates[j].ProfitPerHour
	})
	for i := range candidates {
		if e.canSwap(candidates[i].CompanyID) {
			worstCompany = &candidates[i]
			break
		}
	}
	if worstCompany == nil {
		e.logger.Debug("all companies in worst strategy on swap cooldown",
			zap.String("strategy", worst.StrategyName),
		)
		return
	}

	// Look up the runner by finding the company's game ID from the DB record.
	var dbRecord db.CompanyRecord
	if err := e.gormDB.First(&dbRecord, worstCompany.CompanyID).Error; err != nil {
		e.logger.Error("failed to find company DB record for reallocation",
			zap.Uint("company_id", worstCompany.CompanyID),
			zap.Error(err),
		)
		return
	}

	runner := e.manager.GetRunner(dbRecord.GameID)
	if runner == nil {
		e.logger.Warn("runner not found for company, skipping reallocation",
			zap.String("game_id", dbRecord.GameID),
			zap.String("strategy", worst.StrategyName),
		)
		return
	}

	// Get the strategy factory for the best strategy from the registry.
	factory, ok := e.registry[best.StrategyName]
	if !ok {
		e.logger.Error("no strategy factory registered for target strategy",
			zap.String("strategy", best.StrategyName),
		)
		return
	}

	// Build the strategy context from the manager's shared resources.
	stratCtx := bot.StrategyContext{
		Client:     e.manager.BaseClient().ForCompany(dbRecord.GameID),
		State:      runner.State(),
		World:      e.manager.WorldData(),
		PriceCache: e.manager.PriceCache(),
		Agent:      e.agent,
		Logger:     runner.Logger(),
		DB:         e.gormDB,
	}

	newStrategy, err := factory(stratCtx)
	if err != nil {
		e.logger.Error("failed to create new strategy instance for reallocation",
			zap.String("target_strategy", best.StrategyName),
			zap.String("company", dbRecord.Name),
			zap.Error(err),
		)
		return
	}

	// Send the new strategy to the runner via its swap channel.
	runner.SwapStrategy(newStrategy, "reallocation: underperforming vs "+best.StrategyName)

	e.logger.Info("optimizer executed strategy reallocation",
		zap.String("company", dbRecord.Name),
		zap.String("game_id", dbRecord.GameID),
		zap.String("from_strategy", worst.StrategyName),
		zap.String("to_strategy", best.StrategyName),
		zap.Float64("company_profit_per_hour", worstCompany.ProfitPerHour),
		zap.Float64("worst_strategy_mean", worst.MeanProfit),
		zap.Float64("best_strategy_mean", best.MeanProfit),
	)

	e.recordSwap(worstCompany.CompanyID)

	// Reset the underperform counter after a successful swap.
	e.underperformCount[worst.StrategyName] = 0
}

// agentEvaluation asks the AI agent for strategic recommendations and applies
// any recommended parameter changes or strategy switches.
func (e *Engine) agentEvaluation(ctx context.Context, stats []strategyStats) {
	req := agent.StrategyEvalRequest{
		Metrics: toAgentMetrics(stats),
	}

	evaluation, err := e.agent.EvaluateStrategy(ctx, req)
	if err != nil {
		e.logger.Warn("agent strategy evaluation failed", zap.Error(err))
		return
	}

	if evaluation.Reasoning != "" {
		e.logger.Info("agent strategy evaluation",
			zap.String("reasoning", evaluation.Reasoning),
		)
	}

	// Apply parameter changes if the agent recommended any.
	if len(evaluation.ParamChanges) > 0 {
		e.applyParamChanges(evaluation.ParamChanges)
	}

	// Execute agent-recommended strategy switch.
	if evaluation.SwitchTo != nil {
		e.applyAgentSwitch(stats, *evaluation.SwitchTo)
	}
}

// applyParamChanges applies agent-recommended parameter changes to all running
// companies. Values are validated against tunable parameter bounds before applying.
func (e *Engine) applyParamChanges(changes map[string]any) {
	// Build a bounds lookup for validation.
	bounds := make(map[string]paramDef, len(tunableParams))
	for _, p := range tunableParams {
		bounds[p.Name] = p
	}

	var companies []db.CompanyRecord
	e.gormDB.Where("status = ?", "running").Find(&companies)

	for param, rawValue := range changes {
		def, known := bounds[param]
		if !known {
			e.logger.Warn("agent recommended unknown parameter, ignoring",
				zap.String("parameter", param),
				zap.Any("value", rawValue),
			)
			continue
		}

		// Convert to float64.
		var value float64
		switch v := rawValue.(type) {
		case float64:
			value = v
		case float32:
			value = float64(v)
		case int:
			value = float64(v)
		case int64:
			value = float64(v)
		default:
			e.logger.Warn("agent parameter value has unsupported type, ignoring",
				zap.String("parameter", param),
				zap.Any("value", rawValue),
			)
			continue
		}

		// Clamp to valid range.
		if value < def.Min {
			value = def.Min
		}
		if value > def.Max {
			value = def.Max
		}

		e.logger.Info("applying agent parameter change",
			zap.String("parameter", param),
			zap.Float64("value", value),
			zap.Int("companies", len(companies)),
		)

		for _, c := range companies {
			e.tuner.setCompanyParam(c.ID, param, value)
		}
	}
}

// applyAgentSwitch executes a strategy switch recommended by the agent.
// It finds the worst-performing company of the least-performing strategy
// (that is not the target) and swaps it to the recommended strategy.
func (e *Engine) applyAgentSwitch(stats []strategyStats, targetStrategy string) {
	e.logger.Info("agent recommends strategy switch",
		zap.String("switch_to", targetStrategy),
	)

	// Verify the target strategy exists in the registry.
	factory, ok := e.registry[targetStrategy]
	if !ok {
		e.logger.Warn("agent recommended unknown strategy, ignoring",
			zap.String("strategy", targetStrategy),
		)
		return
	}

	// Sort by score ascending to find the worst strategy that isn't the target.
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Score < stats[j].Score
	})

	var source *strategyStats
	for i := range stats {
		if stats[i].StrategyName != targetStrategy && stats[i].CompanyCount > minCompaniesPerStrategy {
			source = &stats[i]
			break
		}
	}

	if source == nil {
		e.logger.Warn("no eligible source strategy for agent-recommended switch",
			zap.String("target", targetStrategy),
		)
		return
	}

	// Find the worst company in the source strategy that isn't on cooldown.
	if len(source.Companies) == 0 {
		return
	}

	agentCandidates := make([]companyMetrics, len(source.Companies))
	copy(agentCandidates, source.Companies)
	sort.Slice(agentCandidates, func(i, j int) bool {
		return agentCandidates[i].ProfitPerHour < agentCandidates[j].ProfitPerHour
	})
	var worstCompany *companyMetrics
	for i := range agentCandidates {
		if e.canSwap(agentCandidates[i].CompanyID) {
			worstCompany = &agentCandidates[i]
			break
		}
	}
	if worstCompany == nil {
		e.logger.Debug("all companies on cooldown for agent switch")
		return
	}

	var dbRecord db.CompanyRecord
	if err := e.gormDB.First(&dbRecord, worstCompany.CompanyID).Error; err != nil {
		e.logger.Error("failed to find company for agent switch",
			zap.Uint("company_id", worstCompany.CompanyID),
			zap.Error(err),
		)
		return
	}

	runner := e.manager.GetRunner(dbRecord.GameID)
	if runner == nil {
		e.logger.Warn("runner not found for agent switch, skipping",
			zap.String("game_id", dbRecord.GameID),
		)
		return
	}

	stratCtx := bot.StrategyContext{
		Client:     e.manager.BaseClient().ForCompany(dbRecord.GameID),
		State:      runner.State(),
		World:      e.manager.WorldData(),
		PriceCache: e.manager.PriceCache(),
		Agent:      e.agent,
		Logger:     runner.Logger(),
		DB:         e.gormDB,
	}

	newStrategy, err := factory(stratCtx)
	if err != nil {
		e.logger.Error("failed to create strategy for agent switch",
			zap.String("strategy", targetStrategy),
			zap.Error(err),
		)
		return
	}

	runner.SwapStrategy(newStrategy, "agent-recommended strategy switch")

	e.logger.Info("optimizer executed agent-recommended strategy switch",
		zap.String("company", dbRecord.Name),
		zap.String("game_id", dbRecord.GameID),
		zap.String("from_strategy", source.StrategyName),
		zap.String("to_strategy", targetStrategy),
	)

	e.recordSwap(worstCompany.CompanyID)
}

// canSwap checks whether a company is eligible for a strategy swap based on
// the cooldown timer. Returns false if the company was swapped too recently.
func (e *Engine) canSwap(companyID uint) bool {
	if last, ok := e.lastSwap[companyID]; ok {
		if time.Since(last) < swapCooldown {
			return false
		}
	}
	return true
}

// recordSwap marks a company as recently swapped.
func (e *Engine) recordSwap(companyID uint) {
	e.lastSwap[companyID] = time.Now()
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
