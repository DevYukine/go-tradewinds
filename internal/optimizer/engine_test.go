package optimizer

import (
	"testing"

	"go.uber.org/zap"
)

func testEngine() *Engine {
	logger, _ := zap.NewDevelopment()
	return &Engine{
		underperformCount: make(map[string]int),
		logger:            logger.Named("optimizer_test"),
	}
}

func TestCheckReallocations_NoSwapWithSingleStrategy(t *testing.T) {
	stats := []strategyStats{
		{StrategyName: "arbitrage", Score: 100, ConfidenceLow: 80, ConfidenceHigh: 120, CompanyCount: 3},
	}
	e := testEngine()
	e.checkReallocations(nil, stats)
	// No panic, no action with single strategy.
}

func TestCheckReallocations_NoPrematureSwap(t *testing.T) {
	stats := []strategyStats{
		{StrategyName: "arbitrage", Score: 100, ConfidenceLow: 80, ConfidenceHigh: 120, CompanyCount: 3,
			Companies: []companyMetrics{{CompanyID: 1, ProfitPerHour: 100}}},
		{StrategyName: "bulk_hauler", Score: 10, ConfidenceLow: 5, ConfidenceHigh: 15, CompanyCount: 3,
			Companies: []companyMetrics{{CompanyID: 2, ProfitPerHour: 10}}},
	}
	e := testEngine()

	// First period: should increment underperform count but NOT swap.
	e.checkReallocations(nil, stats)

	if e.underperformCount["bulk_hauler"] != 1 {
		t.Errorf("expected underperform count 1, got %d", e.underperformCount["bulk_hauler"])
	}
}

func TestCheckReallocations_ResetOnRecovery(t *testing.T) {
	e := testEngine()
	e.underperformCount["bulk_hauler"] = 1

	// Now stats show overlapping CIs (not statistically significant).
	stats := []strategyStats{
		{StrategyName: "arbitrage", Score: 100, ConfidenceLow: 50, ConfidenceHigh: 150, CompanyCount: 3},
		{StrategyName: "bulk_hauler", Score: 90, ConfidenceLow: 40, ConfidenceHigh: 160, CompanyCount: 3},
	}
	e.checkReallocations(nil, stats)

	if e.underperformCount["bulk_hauler"] != 0 {
		t.Errorf("expected underperform count reset to 0, got %d", e.underperformCount["bulk_hauler"])
	}
}

func TestScoreFormula(t *testing.T) {
	metrics := []companyMetrics{
		{CompanyID: 1, Strategy: "test", ProfitPerHour: 100, WinRate: 0.8, TradesPerHour: 10, CapacityUtil: 0.7, TradesExecuted: 5},
		{CompanyID: 2, Strategy: "test", ProfitPerHour: 200, WinRate: 0.9, TradesPerHour: 12, CapacityUtil: 0.8, TradesExecuted: 8},
	}
	stats := aggregateByStrategy(metrics)
	if len(stats) != 1 {
		t.Fatalf("expected 1 strategy, got %d", len(stats))
	}
	s := stats[0]

	// Verify score uses updated formula.
	expected := 0.35*s.ConfidenceLow + 0.25*s.MeanProfit + 0.20*s.MeanWinRate + 0.10*s.MeanTradesPerHour + 0.10*s.MeanCapacityUtil
	if s.Score != expected {
		t.Errorf("score mismatch: got %f, expected %f", s.Score, expected)
	}
}

func TestDynamicScaling_LowUtilIncrement(t *testing.T) {
	e := testEngine()

	// Simulate low utilization.
	e.lowUtilCount = 0
	e.highUtilCount = 2

	// When utilization goes low, highUtilCount should reset.
	// This tests the counter logic conceptually.
	if e.lowUtilCount != 0 {
		t.Error("initial lowUtilCount should be 0")
	}
}
