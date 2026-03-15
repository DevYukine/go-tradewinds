package optimizer

import (
	"sort"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// paramDef defines a tunable parameter's bounds.
type paramDef struct {
	Name    string
	Min     float64
	Max     float64
	StepPct float64 // How much to adjust by (as fraction, e.g. 0.15 = 15%).
}

var tunableParams = []paramDef{
	{Name: "MinMarginPct", Min: 0.03, Max: 0.30, StepPct: 0.15},
	{Name: "PassengerWeight", Min: 0.5, Max: 10.0, StepPct: 0.20},
	{Name: "PassengerDestBonus", Min: 1.5, Max: 10.0, StepPct: 0.15},
	{Name: "FleetEvalIntervalSec", Min: 60, Max: 600, StepPct: 0.20},
	{Name: "MarketEvalIntervalSec", Min: 30, Max: 300, StepPct: 0.20},
}

// ParameterTuner runs parameter experiments on companies to find optimal settings.
type ParameterTuner struct {
	gormDB  *gorm.DB
	manager *bot.Manager
	logger  *zap.Logger
}

// NewParameterTuner creates a new parameter tuner.
func NewParameterTuner(gormDB *gorm.DB, manager *bot.Manager, logger *zap.Logger) *ParameterTuner {
	return &ParameterTuner{
		gormDB:  gormDB,
		manager: manager,
		logger:  logger,
	}
}

// minExperimentAge is how long an experiment must run before being evaluated.
// This ensures we have enough trade data to judge the parameter change.
const minExperimentAge = 20 * time.Minute

// EvaluateExperiments checks active experiments and completes or reverts them
// based on whether the company's profit improved. Experiments must run for at
// least minExperimentAge before being evaluated.
func (t *ParameterTuner) EvaluateExperiments(metrics []companyMetrics) {
	var active []db.ParamExperimentLog
	t.gormDB.Where("status = ?", "active").Find(&active)

	if len(active) == 0 {
		return
	}

	// Build profit lookup by company ID.
	profitByCompany := make(map[uint]float64, len(metrics))
	for _, m := range metrics {
		profitByCompany[m.CompanyID] = m.ProfitPerHour
	}

	for _, exp := range active {
		// Don't evaluate experiments that haven't run long enough.
		if time.Since(exp.CreatedAt) < minExperimentAge {
			t.logger.Debug("experiment too young to evaluate",
				zap.Uint("company_id", exp.CompanyID),
				zap.String("param", exp.ParamName),
				zap.Duration("age", time.Since(exp.CreatedAt)),
			)
			continue
		}

		profitAfter, ok := profitByCompany[exp.CompanyID]
		if !ok {
			continue
		}

		exp.ProfitAfter = profitAfter
		exp.UpdatedAt = time.Now()

		if profitAfter > exp.ProfitBefore {
			// Experiment succeeded — keep the new value.
			exp.Status = "completed"
			t.logger.Info("experiment succeeded, keeping new param value",
				zap.Uint("company_id", exp.CompanyID),
				zap.String("param", exp.ParamName),
				zap.Float64("old", exp.OldValue),
				zap.Float64("new", exp.NewValue),
				zap.Float64("profit_before", exp.ProfitBefore),
				zap.Float64("profit_after", profitAfter),
			)
			// Propagate winning params to other companies on the same strategy.
			t.propagateWin(exp)
		} else {
			// Experiment failed — revert.
			exp.Status = "reverted"
			t.logger.Info("experiment failed, reverting param",
				zap.Uint("company_id", exp.CompanyID),
				zap.String("param", exp.ParamName),
				zap.Float64("old", exp.OldValue),
				zap.Float64("new", exp.NewValue),
				zap.Float64("profit_before", exp.ProfitBefore),
				zap.Float64("profit_after", profitAfter),
			)
			t.setCompanyParam(exp.CompanyID, exp.ParamName, exp.OldValue)
		}

		t.gormDB.Save(&exp)
	}
}

// RunExperiment picks the worst-performing company, selects the least-recently-tuned
// parameter, and adjusts it by ±step%.
func (t *ParameterTuner) RunExperiment(metrics []companyMetrics) {
	if len(metrics) == 0 {
		return
	}

	// Don't start new experiments if one is already active.
	var activeCount int64
	t.gormDB.Model(&db.ParamExperimentLog{}).Where("status = ?", "active").Count(&activeCount)
	if activeCount > 0 {
		return
	}

	// Sort by profit ascending — experiment on worst performer.
	sorted := make([]companyMetrics, len(metrics))
	copy(sorted, metrics)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].ProfitPerHour < sorted[j].ProfitPerHour
	})

	target := sorted[0]

	// Load current params.
	var params db.CompanyParams
	if err := t.gormDB.Where("company_id = ?", target.CompanyID).First(&params).Error; err != nil {
		t.logger.Debug("no params for company, skipping experiment", zap.Uint("company_id", target.CompanyID))
		return
	}

	// Find least-recently-tuned param.
	param := t.leastRecentlyTuned(target.CompanyID)
	if param == nil {
		return
	}

	oldValue := t.getParamValue(&params, param.Name)
	step := oldValue * param.StepPct

	// Alternate direction based on the last experiment for this param.
	// This prevents always biasing upward and explores both directions.
	var lastExp db.ParamExperimentLog
	lastWentUp := true
	if err := t.gormDB.Where("company_id = ? AND param_name = ?", target.CompanyID, param.Name).
		Order("created_at DESC").First(&lastExp).Error; err == nil {
		lastWentUp = lastExp.NewValue > lastExp.OldValue
	}

	var newValue float64
	if lastWentUp {
		newValue = oldValue - step // try decreasing this time
	} else {
		newValue = oldValue + step // try increasing this time
	}

	// Clamp to bounds.
	if newValue > param.Max {
		newValue = param.Max
	}
	if newValue < param.Min {
		newValue = param.Min
	}
	if newValue == oldValue {
		t.logger.Debug("param at bounds, skipping", zap.String("param", param.Name))
		return
	}

	// Apply the new value.
	t.setCompanyParam(target.CompanyID, param.Name, newValue)

	// Record the experiment.
	exp := db.ParamExperimentLog{
		CompanyID:    target.CompanyID,
		ParamName:    param.Name,
		OldValue:     oldValue,
		NewValue:     newValue,
		ProfitBefore: target.ProfitPerHour,
		Status:       "active",
	}
	t.gormDB.Create(&exp)

	t.logger.Info("started parameter experiment",
		zap.Uint("company_id", target.CompanyID),
		zap.String("param", param.Name),
		zap.Float64("old", oldValue),
		zap.Float64("new", newValue),
		zap.Float64("profit_before", target.ProfitPerHour),
	)
}

// propagateWin applies a winning parameter value to other companies on the same strategy.
func (t *ParameterTuner) propagateWin(exp db.ParamExperimentLog) {
	// Find the strategy of the winning company.
	var record db.CompanyRecord
	if err := t.gormDB.First(&record, exp.CompanyID).Error; err != nil {
		return
	}

	// Find other companies on the same strategy.
	var peers []db.CompanyRecord
	t.gormDB.Where("strategy = ? AND id != ? AND status = ?", record.Strategy, exp.CompanyID, "running").Find(&peers)

	for _, peer := range peers {
		t.setCompanyParam(peer.ID, exp.ParamName, exp.NewValue)
		t.logger.Debug("propagated winning param to peer",
			zap.Uint("peer_id", peer.ID),
			zap.String("param", exp.ParamName),
			zap.Float64("value", exp.NewValue),
		)
	}
}

// leastRecentlyTuned returns the param definition that has been experimented
// on least recently for the given company.
func (t *ParameterTuner) leastRecentlyTuned(companyID uint) *paramDef {
	lastTuned := make(map[string]time.Time)
	for _, p := range tunableParams {
		lastTuned[p.Name] = time.Time{} // zero time = never tuned
	}

	var experiments []db.ParamExperimentLog
	t.gormDB.Where("company_id = ?", companyID).Order("created_at DESC").Find(&experiments)

	for _, exp := range experiments {
		if _, ok := lastTuned[exp.ParamName]; ok {
			if exp.CreatedAt.After(lastTuned[exp.ParamName]) {
				lastTuned[exp.ParamName] = exp.CreatedAt
			}
		}
	}

	// Return the param with the oldest (or zero) last experiment time.
	var oldest *paramDef
	oldestTime := time.Now()
	for i, p := range tunableParams {
		if lastTuned[p.Name].Before(oldestTime) {
			oldestTime = lastTuned[p.Name]
			oldest = &tunableParams[i]
		}
	}
	return oldest
}

// getParamValue reads a named parameter from a CompanyParams struct.
func (t *ParameterTuner) getParamValue(params *db.CompanyParams, name string) float64 {
	switch name {
	case "MinMarginPct":
		return params.MinMarginPct
	case "PassengerWeight":
		return params.PassengerWeight
	case "PassengerDestBonus":
		return params.PassengerDestBonus
	case "FleetEvalIntervalSec":
		return float64(params.FleetEvalIntervalSec)
	case "MarketEvalIntervalSec":
		return float64(params.MarketEvalIntervalSec)
	default:
		return 0
	}
}

// setCompanyParam updates a single named parameter in the DB and live state.
func (t *ParameterTuner) setCompanyParam(companyID uint, name string, value float64) {
	updates := map[string]any{}
	switch name {
	case "MinMarginPct":
		updates["min_margin_pct"] = value
	case "PassengerWeight":
		updates["passenger_weight"] = value
	case "PassengerDestBonus":
		updates["passenger_dest_bonus"] = value
	case "FleetEvalIntervalSec":
		updates["fleet_eval_interval_sec"] = int(value)
	case "MarketEvalIntervalSec":
		updates["market_eval_interval_sec"] = int(value)
	default:
		return
	}

	t.gormDB.Model(&db.CompanyParams{}).Where("company_id = ?", companyID).Updates(updates)

	// Update live state if runner is available.
	var record db.CompanyRecord
	if err := t.gormDB.First(&record, companyID).Error; err != nil {
		return
	}
	runner := t.manager.GetRunner(record.GameID)
	if runner == nil {
		return
	}
	state := runner.State()
	state.Lock()
	if state.Params != nil {
		switch name {
		case "MinMarginPct":
			state.Params.MinMarginPct = value
		case "PassengerWeight":
			state.Params.PassengerWeight = value
		case "PassengerDestBonus":
			state.Params.PassengerDestBonus = value
		case "FleetEvalIntervalSec":
			state.Params.FleetEvalIntervalSec = int(value)
		case "MarketEvalIntervalSec":
			state.Params.MarketEvalIntervalSec = int(value)
		}
	}
	state.Unlock()
}
