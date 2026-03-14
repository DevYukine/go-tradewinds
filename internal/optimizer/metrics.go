package optimizer

import (
	"math"
	"time"

	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/agent"
	"github.com/DevYukine/go-tradewinds/internal/db"
)

// companyMetrics holds computed performance metrics for a single company.
type companyMetrics struct {
	CompanyID      uint
	Strategy       string
	TradesExecuted int
	TotalProfit    int64
	TotalLoss      int64
	WinRate        float64
	ProfitPerHour  float64
}

// collectCompanyMetrics computes performance metrics for a company over the given period.
func collectCompanyMetrics(gormDB *gorm.DB, companyID uint, strategy string, since time.Time) companyMetrics {
	m := companyMetrics{
		CompanyID: companyID,
		Strategy:  strategy,
	}

	var trades []db.TradeLog
	gormDB.Where("company_id = ? AND created_at >= ?", companyID, since).Find(&trades)

	m.TradesExecuted = len(trades)
	if m.TradesExecuted == 0 {
		return m
	}

	wins := 0
	for _, t := range trades {
		if t.Action == "sell" {
			m.TotalProfit += int64(t.TotalPrice)
			wins++
		} else {
			m.TotalLoss += int64(t.TotalPrice)
		}
	}

	m.WinRate = float64(wins) / float64(m.TradesExecuted)

	// Calculate profit per hour based on period duration.
	hours := time.Since(since).Hours()
	if hours > 0 {
		netProfit := m.TotalProfit - m.TotalLoss
		m.ProfitPerHour = float64(netProfit) / hours
	}

	return m
}

// strategyStats holds aggregated statistics for a strategy across multiple companies.
type strategyStats struct {
	StrategyName   string
	CompanyCount   int
	Companies      []companyMetrics
	MeanProfit     float64
	StdDevProfit   float64
	ConfidenceLow  float64
	ConfidenceHigh float64
	TotalTrades    int
	MeanWinRate    float64
	Score          float64
}

// aggregateByStrategy groups company metrics by strategy and computes
// statistical measures (mean, std dev, 95% confidence intervals).
func aggregateByStrategy(metrics []companyMetrics) []strategyStats {
	grouped := make(map[string][]companyMetrics)
	for _, m := range metrics {
		grouped[m.Strategy] = append(grouped[m.Strategy], m)
	}

	var stats []strategyStats
	for name, companies := range grouped {
		s := strategyStats{
			StrategyName: name,
			CompanyCount: len(companies),
			Companies:    companies,
		}

		// Collect profit-per-hour values for statistical analysis.
		profits := make([]float64, len(companies))
		var sumProfit, sumWinRate float64

		for i, c := range companies {
			profits[i] = c.ProfitPerHour
			sumProfit += c.ProfitPerHour
			sumWinRate += c.WinRate
			s.TotalTrades += c.TradesExecuted
		}

		n := float64(len(companies))
		s.MeanProfit = sumProfit / n
		s.MeanWinRate = sumWinRate / n

		// Standard deviation.
		if len(companies) > 1 {
			var sumSqDiff float64
			for _, p := range profits {
				diff := p - s.MeanProfit
				sumSqDiff += diff * diff
			}
			s.StdDevProfit = math.Sqrt(sumSqDiff / (n - 1))

			// 95% confidence interval using t-distribution approximation.
			// For small samples, t ≈ 2.0 is reasonable.
			tValue := 2.0
			standardError := s.StdDevProfit / math.Sqrt(n)
			s.ConfidenceLow = s.MeanProfit - tValue*standardError
			s.ConfidenceHigh = s.MeanProfit + tValue*standardError
		} else {
			// Single company: no CI, just point estimate.
			s.ConfidenceLow = s.MeanProfit
			s.ConfidenceHigh = s.MeanProfit
		}

		// Composite score: weights consistency (CI lower bound) heavily.
		s.Score = 0.5*s.ConfidenceLow + 0.3*s.MeanProfit + 0.2*s.MeanWinRate

		stats = append(stats, s)
	}

	return stats
}

// toAgentMetrics converts internal stats to agent-compatible metrics.
func toAgentMetrics(stats []strategyStats) []agent.StrategyMetrics {
	result := make([]agent.StrategyMetrics, len(stats))
	for i, s := range stats {
		var totalProfit, totalLoss int64
		for _, c := range s.Companies {
			totalProfit += c.TotalProfit
			totalLoss += c.TotalLoss
		}
		result[i] = agent.StrategyMetrics{
			StrategyName:   s.StrategyName,
			CompanyCount:   s.CompanyCount,
			TradesExecuted: s.TotalTrades,
			TotalProfit:    totalProfit,
			TotalLoss:      totalLoss,
			WinRate:        s.MeanWinRate,
			ProfitPerHour:  s.MeanProfit,
		}
	}
	return result
}
