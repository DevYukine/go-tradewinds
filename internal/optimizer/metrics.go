package optimizer

import (
	"math"
	"time"

	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/db"
)

// decayWeight returns an exponential decay weight based on trade age.
// Recent trades (last 5 min) get weight ~1.0, trades 14+ min old get ~0.5.
func decayWeight(tradeTime time.Time, now time.Time) float64 {
	age := now.Sub(tradeTime).Minutes()
	return math.Exp(-0.05 * age)
}

// companyMetrics holds computed performance metrics for a single company.
type companyMetrics struct {
	CompanyID        uint
	Strategy         string
	TradesExecuted   int
	TotalProfit      int64
	TotalLoss        int64
	PassengerRevenue int64
	WinRate          float64
	ProfitPerHour    float64
	AvgTradeProfit   int64
	TradesPerHour    float64
	CapacityUtil     float64
}

// collectCompanyMetrics computes performance metrics for a company over the given period.
func collectCompanyMetrics(gormDB *gorm.DB, companyID uint, strategy string, since time.Time) companyMetrics {
	m := companyMetrics{
		CompanyID: companyID,
		Strategy:  strategy,
	}

	var trades []db.TradeLog
	gormDB.Where("company_id = ? AND created_at >= ?", companyID, since).Find(&trades)

	// Query passenger revenue for this period.
	var passengers []db.PassengerLog
	gormDB.Where("company_id = ? AND created_at >= ?", companyID, since).Find(&passengers)

	now := time.Now()
	for _, p := range passengers {
		m.PassengerRevenue += int64(p.Bid)
	}

	m.TradesExecuted = len(trades)
	if m.TradesExecuted == 0 && m.PassengerRevenue == 0 {
		return m
	}

	wins := 0
	var weightedProfit, weightedLoss float64
	var totalWeight float64

	for _, t := range trades {
		w := decayWeight(t.CreatedAt, now)
		totalWeight += w
		if t.Action == "sell" {
			m.TotalProfit += int64(t.TotalPrice)
			weightedProfit += float64(t.TotalPrice) * w
			wins++
		} else {
			m.TotalLoss += int64(t.TotalPrice)
			weightedLoss += float64(t.TotalPrice) * w
		}
	}

	// Add decay-weighted passenger revenue.
	var weightedPassengerRev float64
	for _, p := range passengers {
		w := decayWeight(p.CreatedAt, now)
		weightedPassengerRev += float64(p.Bid) * w
		totalWeight += w
	}
	m.TotalProfit += m.PassengerRevenue

	if m.TradesExecuted > 0 {
		m.WinRate = float64(wins) / float64(m.TradesExecuted)
	}

	// Use decay-weighted profit (including passengers) for per-hour calculation.
	hours := time.Since(since).Hours()
	if hours > 0 && totalWeight > 0 {
		weightedNet := weightedProfit - weightedLoss + weightedPassengerRev
		m.ProfitPerHour = weightedNet / hours
	}

	// Average trade profit.
	if m.TradesExecuted > 0 {
		m.AvgTradeProfit = (m.TotalProfit - m.TotalLoss) / int64(m.TradesExecuted)
	}

	// Trades per hour.
	if hours > 0 {
		m.TradesPerHour = float64(m.TradesExecuted) / hours
	}

	// Capacity utilization from latest PnL snapshot.
	var latestPnL db.PnLSnapshot
	if err := gormDB.Where("company_id = ?", companyID).Order("created_at DESC").First(&latestPnL).Error; err == nil {
		m.CapacityUtil = latestPnL.AvgCapacityUtil
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
	MeanWinRate       float64
	MeanTradesPerHour float64
	MeanCapacityUtil  float64
	Score             float64
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
		var sumProfit, sumWinRate, sumTradesPerHour, sumCapacityUtil float64

		for i, c := range companies {
			profits[i] = c.ProfitPerHour
			sumProfit += c.ProfitPerHour
			sumWinRate += c.WinRate
			sumTradesPerHour += c.TradesPerHour
			sumCapacityUtil += c.CapacityUtil
			s.TotalTrades += c.TradesExecuted
		}

		n := float64(len(companies))
		s.MeanProfit = sumProfit / n
		s.MeanWinRate = sumWinRate / n
		s.MeanTradesPerHour = sumTradesPerHour / n
		s.MeanCapacityUtil = sumCapacityUtil / n

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

		// Score is net profit per hour. The tuner just needs a relative
		// comparison between strategies — no need for a weighted composite.
		s.Score = s.MeanProfit

		stats = append(stats, s)
	}

	return stats
}

