package engine

import (
	"context"
	"math"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// Debt uses realized settlement cash, never an uncapped depth forecast. A class
// with no settlement history can use the fleet cohort; stale class history
// cannot borrow a fresher fleet rate.
func (r *Runner) capacityDebtMeasuredRate(ctx context.Context, hullID string, asOf time.Time) (float64, string, bool, error) {
	scale := float64(r.stateCache.Clock.Calendar.TimeScale)
	if !r.fresh("ships") || !r.fresh("clock") || scale <= 0 || r.stateCache.Clock.Paused || asOf.IsZero() {
		return 0, "", false, nil
	}
	window := time.Duration(float64(capacityMeasuredGameDays) * float64(24*time.Hour) / scale)
	windowStart := asOf.Add(-window)
	var classIDs, fleetIDs []string
	var classDays, fleetDays float64
	classAges, fleetAges := true, true
	oldestClass := asOf
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		fleetIDs = append(fleetIDs, ship.Ship.ID)
		class := ship.Ship.ShipClass == hullID
		if class {
			classIDs = append(classIDs, ship.Ship.ID)
		}
		created := ship.Ship.CreatedAt
		if created == nil || created.IsZero() || created.After(asOf) {
			fleetAges = false
			if class {
				classAges = false
			}
			continue
		}
		days := asOf.Sub(maxTime(windowStart, *created)).Seconds() * scale / 86_400
		fleetDays += days
		if class {
			classDays += days
			if created.Before(oldestClass) {
				oldestClass = *created
			}
		}
	}
	valid := func(history store.GrowthHistory, days float64, ages bool) bool {
		return ages && days >= capacityMinimumHullGameDays && history.LastCompletion != nil &&
			!history.LastCompletion.Before(windowStart) && !history.LastCompletion.After(asOf) &&
			!history.UnknownCosts && !history.FutureCompletion
	}
	if len(classIDs) > 0 {
		history, err := r.db.EconomicHistoryWindowForShips(ctx, r.account.ID, classIDs, asOf, window)
		if err != nil {
			return 0, "", false, err
		}
		if history.LastCompletion != nil || history.UnknownCosts || history.FutureCompletion {
			if !valid(history, classDays, classAges) {
				return 0, "", false, nil
			}
			rate := float64(history.NetPence) / classDays
			return rate, "min(depth-plan rate, measured class rate)", !math.IsNaN(rate) && !math.IsInf(rate, 0), nil
		}
		if !classAges {
			return 0, "", false, nil
		}
		if oldestClass.Before(windowStart) {
			lifetime, err := r.db.EconomicHistoryWindowForShips(ctx, r.account.ID, classIDs, asOf, asOf.Sub(oldestClass)+time.Nanosecond)
			if err != nil {
				return 0, "", false, err
			}
			if lifetime.LastCompletion != nil || lifetime.UnknownCosts || lifetime.FutureCompletion {
				return 0, "", false, nil
			}
		}
	}
	if len(fleetIDs) == 0 {
		return 0, "", false, nil
	}
	history, err := r.db.EconomicHistoryWindowForShips(ctx, r.account.ID, fleetIDs, asOf, window)
	if err != nil {
		return 0, "", false, err
	}
	if !valid(history, fleetDays, fleetAges) {
		return 0, "", false, nil
	}
	rate := float64(history.NetPence) / fleetDays
	return rate, "unmeasured class: min(depth-plan rate, measured fleet rate)", !math.IsNaN(rate) && !math.IsInf(rate, 0), nil
}

func (r *Runner) capacityDebtDecision(ctx context.Context, hull Hull, decision GrowthDecision, asOf time.Time, excludedQuoteIDs map[string]bool) (GrowthDecision, string, error) {
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	rate, basis, measured, err := r.capacityDebtMeasuredRate(ctx, hull.ID, asOf)
	if err != nil {
		return decision, "", err
	}
	if !measured {
		return decision, "measured_net", nil
	}
	forecast := decision.DepthForecastRatePencePerGameDay
	if forecast <= 0 {
		forecast = decision.DepthNetPencePerGameDay + float64(decision.IncrementalUpkeepPence) + mortgageInterestPerGameDay(decision.Mortgage)
	}
	if forecast <= 0 || math.IsNaN(forecast) || math.IsInf(forecast, 0) {
		return decision, "no_depth", nil
	}
	var existingInterest float64
	for _, loan := range r.stateCache.Loans {
		if loan.Status != "active" && loan.Status != "defaulted" {
			continue
		}
		if loan.QuoteID != nil && excludedQuoteIDs[*loan.QuoteID] {
			continue
		}
		existingInterest += loanDailyInterest(loan)
	}
	net := min(forecast, rate) - float64(decision.IncrementalUpkeepPence) - existingInterest - mortgageInterestPerGameDay(decision.Mortgage)
	decision.EvidenceBasis = basis + "; less incremental upkeep and existing interest"
	decision.IncrementalNetPencePerGameDay = net
	decision.DepthNetPencePerGameDay = net
	if net <= 0 || math.IsNaN(net) || math.IsInf(net, 0) {
		return decision, "dscr", nil
	}
	decision.PaybackGameDays = float64(growthCapital(decision)) / net
	return decision, "", nil
}
