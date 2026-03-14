package optimizer

import (
	"math"
	"testing"
	"time"
)

func TestAggregateByStrategy_MultipleStrategies(t *testing.T) {
	metrics := []companyMetrics{
		{CompanyID: 1, Strategy: "arbitrage", ProfitPerHour: 100, WinRate: 0.8, TradesExecuted: 10, TradesPerHour: 5, CapacityUtil: 0.6},
		{CompanyID: 2, Strategy: "arbitrage", ProfitPerHour: 200, WinRate: 0.9, TradesExecuted: 15, TradesPerHour: 7, CapacityUtil: 0.7},
		{CompanyID: 3, Strategy: "bulk_hauler", ProfitPerHour: 50, WinRate: 0.6, TradesExecuted: 5, TradesPerHour: 2, CapacityUtil: 0.9},
	}
	stats := aggregateByStrategy(metrics)

	if len(stats) != 2 {
		t.Fatalf("expected 2 strategies, got %d", len(stats))
	}

	// Find each strategy.
	var arb, bulk *strategyStats
	for i := range stats {
		switch stats[i].StrategyName {
		case "arbitrage":
			arb = &stats[i]
		case "bulk_hauler":
			bulk = &stats[i]
		}
	}

	if arb == nil || bulk == nil {
		t.Fatal("missing strategy in results")
	}

	if arb.CompanyCount != 2 {
		t.Errorf("arbitrage company count: got %d, want 2", arb.CompanyCount)
	}
	if bulk.CompanyCount != 1 {
		t.Errorf("bulk_hauler company count: got %d, want 1", bulk.CompanyCount)
	}
	if arb.TotalTrades != 25 {
		t.Errorf("arbitrage total trades: got %d, want 25", arb.TotalTrades)
	}
}

func TestAggregateByStrategy_SingleCompany(t *testing.T) {
	metrics := []companyMetrics{
		{CompanyID: 1, Strategy: "solo", ProfitPerHour: 100, WinRate: 0.75, TradesExecuted: 10},
	}
	stats := aggregateByStrategy(metrics)

	if len(stats) != 1 {
		t.Fatalf("expected 1 strategy, got %d", len(stats))
	}
	s := stats[0]

	// Single company: CI should be point estimate (low == high == mean).
	if s.ConfidenceLow != s.MeanProfit || s.ConfidenceHigh != s.MeanProfit {
		t.Errorf("single company CI should be point estimate: low=%f, high=%f, mean=%f",
			s.ConfidenceLow, s.ConfidenceHigh, s.MeanProfit)
	}
	if s.StdDevProfit != 0 {
		t.Errorf("single company std dev should be 0, got %f", s.StdDevProfit)
	}
}

func TestAggregateByStrategy_ZeroTrades(t *testing.T) {
	metrics := []companyMetrics{
		{CompanyID: 1, Strategy: "idle", ProfitPerHour: 0, WinRate: 0, TradesExecuted: 0},
	}
	stats := aggregateByStrategy(metrics)

	if len(stats) != 1 {
		t.Fatalf("expected 1 strategy, got %d", len(stats))
	}
	s := stats[0]

	if s.MeanProfit != 0 {
		t.Errorf("zero trades should have 0 mean profit, got %f", s.MeanProfit)
	}
	if s.MeanWinRate != 0 {
		t.Errorf("zero trades should have 0 win rate, got %f", s.MeanWinRate)
	}
}

func TestAggregateByStrategy_WinRate(t *testing.T) {
	metrics := []companyMetrics{
		{CompanyID: 1, Strategy: "test", WinRate: 0.8, TradesExecuted: 10},
		{CompanyID: 2, Strategy: "test", WinRate: 0.6, TradesExecuted: 10},
	}
	stats := aggregateByStrategy(metrics)
	s := stats[0]

	expectedWinRate := 0.7
	if math.Abs(s.MeanWinRate-expectedWinRate) > 0.001 {
		t.Errorf("mean win rate: got %f, want %f", s.MeanWinRate, expectedWinRate)
	}
}

func TestDecayWeight(t *testing.T) {
	now := time.Now()

	// Recent trade should have weight close to 1.0.
	recent := decayWeight(now.Add(-1*time.Minute), now)
	if recent < 0.9 {
		t.Errorf("1-min-old trade weight should be > 0.9, got %f", recent)
	}

	// 14-minute-old trade should have weight close to 0.5.
	halfLife := decayWeight(now.Add(-14*time.Minute), now)
	if halfLife < 0.4 || halfLife > 0.6 {
		t.Errorf("14-min-old trade weight should be ~0.5, got %f", halfLife)
	}

	// Old trade should have lower weight than recent.
	old := decayWeight(now.Add(-30*time.Minute), now)
	if old >= recent {
		t.Error("old trade should have lower weight than recent")
	}
}

func TestToAgentMetrics(t *testing.T) {
	stats := []strategyStats{
		{
			StrategyName: "arbitrage",
			CompanyCount: 2,
			TotalTrades:  10,
			MeanWinRate:  0.8,
			MeanProfit:   100,
			Companies: []companyMetrics{
				{TotalProfit: 500, TotalLoss: 200},
				{TotalProfit: 300, TotalLoss: 100},
			},
		},
	}
	result := toAgentMetrics(stats)
	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if result[0].TotalProfit != 800 {
		t.Errorf("total profit: got %d, want 800", result[0].TotalProfit)
	}
	if result[0].TotalLoss != 300 {
		t.Errorf("total loss: got %d, want 300", result[0].TotalLoss)
	}
}
