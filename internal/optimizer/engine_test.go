package optimizer

import (
	"testing"
)

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

	// Score should be MeanProfit (simplified formula).
	if s.Score != s.MeanProfit {
		t.Errorf("score should equal MeanProfit: got %f, expected %f", s.Score, s.MeanProfit)
	}
}
