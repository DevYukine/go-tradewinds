package store

import (
	"encoding/json"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strings"
	"time"
)

const RevenueCalibrationVersion = 2

const (
	revenueCalibrationMinSamples = 10
	revenueCalibrationMaxSamples = 4096
	revenueCalibrationWindow     = 14 * 24 * time.Hour
)

// RevenueCalibrationCell contains receipt factors, not net-profit multipliers.
// AdverseBPS is the receipt factor at the 65th percentile of adverse misses.
type RevenueCalibrationCell struct {
	Samples    int    `json:"samples"`
	MedianBPS  int64  `json:"median_bps"`
	AdverseBPS int64  `json:"adverse_bps"`
	Source     string `json:"source,omitempty"`
}

type RevenueCalibrationModel struct {
	Version                int                               `json:"version"`
	Since                  time.Time                         `json:"since"`
	RetainedSince          time.Time                         `json:"retained_since,omitempty"`
	FittedAt               time.Time                         `json:"fitted_at"`
	Cells                  map[string]RevenueCalibrationCell `json:"cells"`
	AdmissionSamples       int                               `json:"admission_samples"`
	CapturedSamples        int                               `json:"captured_samples"`
	DurableFallbackSamples int                               `json:"durable_fallback_samples"`
	ExcludedSamples        int                               `json:"excluded_samples"`
}

// RevenueForecast is frozen at admission, before execution can reprice a lot.
// It lives in the encrypted mission and settlement context.
type RevenueForecast struct {
	Kind            string  `json:"kind"`
	Commodity       string  `json:"commodity"`
	Basis           string  `json:"basis"`
	QuantityTons    int64   `json:"quantity_tons"`
	RevenuePence    int64   `json:"revenue_pence"`
	HorizonGameDays float64 `json:"horizon_game_days"`
	EventStress     bool    `json:"event_stress"`
}

// RevenueHorizonBucket uses forecast game days, never wall-clock hours.
func RevenueHorizonBucket(gameDays float64) string {
	switch {
	case math.IsNaN(gameDays), math.IsInf(gameDays, 0), gameDays < 0:
		return "unknown"
	case gameDays < 1:
		return "<1"
	case gameDays < 1.5:
		return "1-1.5"
	case gameDays < 2.5:
		return "1.5-2.5"
	default:
		return "2.5+"
	}
}

// CommodityGroup pools goods with similar uses. Unseen catalog additions share
// the unknown group before falling back to global evidence.
func CommodityGroup(commodity string) string {
	switch commodity {
	case "provisions", "water_beer", "grain", "fresh_fish", "livestock", "salted_fish", "salted_meat", "wine_brandy", "olive_oil", "rice":
		return "provisions"
	case "hemp_flax", "raw_wool", "hemp_rope", "canvas_sails", "coarse_cloth", "fine_broadcloth", "indian_calico":
		return "textile"
	case "amber_furs", "silver_bullion", "cane_sugar", "molasses_rum", "tobacco", "indigo_dyes", "ivory_gold_dust", "coffee_cocoa", "pepper_spices", "chinese_tea", "silk_porcelain":
		return "luxury"
	case "rough_timber", "seasoned_oak", "iron_ore", "copper_ore", "coal", "crude_salt", "quarry_stone", "saltpeter_sulfur", "sawn_planks", "pitch_tar", "ironwork_tools", "copper_sheathing", "bricks_glass", "gunpowder", "cannons_arms", "train_oil":
		return "bulk"
	default:
		return "unknown"
	}
}

func revenueCalibrationKeys(commodity, basis, horizon string) []string {
	if basis == "contract" {
		return []string{"contract/" + horizon, "contract"}
	}
	group := CommodityGroup(commodity)
	return []string{
		"basis/" + basis + "/commodity/" + commodity + "/" + horizon,
		"basis/" + basis + "/group/" + group + "/" + horizon,
		"basis/" + basis + "/" + horizon,
		"basis/" + basis,
		"commodity/" + commodity + "/" + horizon,
		"group/" + group + "/" + horizon,
		"group/" + group,
		"global/" + horizon,
		"global",
	}
}

func (model RevenueCalibrationModel) Factor(commodity, basis string, gameDays float64) RevenueCalibrationCell {
	for _, key := range revenueCalibrationKeys(commodity, basis, RevenueHorizonBucket(gameDays)) {
		minimum := 30
		if key == "global" || key == "contract" {
			minimum = revenueCalibrationMinSamples
		}
		if cell, exists := model.Cells[key]; exists && cell.Samples >= minimum {
			cell.Source = key
			cell.MedianBPS = max(0, min(10_000, cell.MedianBPS))
			cell.AdverseBPS = max(0, min(cell.MedianBPS, cell.AdverseBPS))
			return cell
		}
	}
	if basis == "contract" {
		return RevenueCalibrationCell{MedianBPS: 10_000, AdverseBPS: 10_000, Source: "fixed contract prior"}
	}
	return RevenueCalibrationCell{MedianBPS: 10_000, AdverseBPS: 10_000, Source: "unmeasured neutral revenue prior"}
}

func calibrationAdmissionSettlement(row Settlement, record *ReplayPlan) (Settlement, bool, error) {
	var plan struct {
		Kind        string `json:"kind"`
		Commodity   string `json:"commodity"`
		EventStress bool   `json:"event_price_stress"`
		Economics   struct {
			RevenueBasis             string  `json:"revenue_basis"`
			RevenuePence             int64   `json:"revenue_pence"`
			RevenueQuantityTons      int64   `json:"revenue_quantity_tons"`
			UncalibratedRevenuePence *int64  `json:"uncalibrated_revenue_pence"`
			RevenueCalibrationBPS    int64   `json:"revenue_calibration_bps"`
			GameDays                 float64 `json:"game_days"`
			RevenueHorizonGameDays   float64 `json:"revenue_horizon_game_days"`
		} `json:"economics"`
	}
	if err := json.Unmarshal(record.Plan, &plan); err != nil {
		return Settlement{}, false, err
	}
	horizon := plan.Economics.GameDays
	if plan.Economics.RevenueHorizonGameDays > 0 {
		horizon = plan.Economics.RevenueHorizonGameDays
	}
	if plan.EventStress || plan.Kind != row.Kind || plan.Commodity != row.Commodity || plan.Economics.RevenueQuantityTons != row.QuantityTons ||
		horizon <= 0 || math.IsNaN(horizon) || math.IsInf(horizon, 0) || horizon >= float64(math.MaxInt64)/86_400 {
		return Settlement{}, false, nil
	}
	revenue := plan.Economics.RevenuePence
	if plan.Economics.UncalibratedRevenuePence != nil {
		revenue = *plan.Economics.UncalibratedRevenuePence
	} else if plan.Economics.RevenueCalibrationBPS != 0 && plan.Economics.RevenueCalibrationBPS != 10_000 {
		return Settlement{}, false, nil
	}
	row.AdmissionRevenue = &RevenueForecast{
		Kind: plan.Kind, Commodity: plan.Commodity, Basis: plan.Economics.RevenueBasis,
		QuantityTons: plan.Economics.RevenueQuantityTons, RevenuePence: revenue,
		HorizonGameDays: horizon,
	}
	return row, true, nil
}

// FitRevenueCalibrationWindow can recover legacy admission quotes from replay
// plans offline. Online fitting passes no plans and reads only settlements.
func FitRevenueCalibrationWindow(settlements []Settlement, plans []ReplayPlan, regimeSince, retainedSince, asOf time.Time) (RevenueCalibrationModel, error) {
	if regimeSince.IsZero() || asOf.IsZero() || asOf.Before(regimeSince) {
		return RevenueCalibrationModel{}, fmt.Errorf("revenue calibration window: %w", ErrInvalidInput)
	}
	from := asOf.Add(-revenueCalibrationWindow)
	if regimeSince.After(from) {
		from = regimeSince
	}
	candidates := make([]int, 0, len(settlements))
	for i := range settlements {
		row := &settlements[i]
		if row.StartedAt.Before(regimeSince) || row.CompletedAt.Before(from) || !row.CompletedAt.Before(asOf) ||
			!calibrationSettlementEligible(*row) {
			continue
		}
		candidates = append(candidates, i)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := &settlements[candidates[i]], &settlements[candidates[j]]
		if left.CompletedAt.Equal(right.CompletedAt) {
			return left.ID < right.ID
		}
		return left.CompletedAt.After(right.CompletedAt)
	})
	if len(candidates) > revenueCalibrationMaxSamples {
		candidates = candidates[:revenueCalibrationMaxSamples]
	}
	type missionKey struct {
		accountID string
		missionID string
	}
	firstPlans := make(map[missionKey]*ReplayPlan, len(plans))
	for i := range plans {
		plan := &plans[i]
		key := missionKey{accountID: plan.AccountID, missionID: plan.MissionID}
		first, exists := firstPlans[key]
		if !exists || plan.ObservedAt.Before(first.ObservedAt) ||
			plan.ObservedAt.Equal(first.ObservedAt) && plan.ID < first.ID {
			firstPlans[key] = plan
		}
	}
	cohort := make([]Settlement, 0, len(candidates))
	var admissionSamples, capturedSamples, durableFallbackSamples, excludedSamples int
	for _, index := range candidates {
		row := settlements[index]
		key := missionKey{accountID: row.AccountID, missionID: row.MissionID}
		plan, captured := firstPlans[key]
		if row.AdmissionRevenue == nil && captured {
			if plan.ObservedAt.After(row.StartedAt.Add(time.Second)) || !plan.ObservedAt.Before(row.CompletedAt) {
				excludedSamples++
				continue
			}
			var comparable bool
			var err error
			row, comparable, err = calibrationAdmissionSettlement(row, plan)
			if err != nil {
				return RevenueCalibrationModel{}, fmt.Errorf("calibration admission forecast: %w", err)
			}
			if !comparable {
				excludedSamples++
				continue
			}
		}
		forecast, comparable := calibrationForecast(row)
		if !comparable || forecast.Basis == "retained" && !retainedSince.IsZero() && row.StartedAt.Before(retainedSince) {
			excludedSamples++
			continue
		}
		switch {
		case settlements[index].AdmissionRevenue != nil:
			admissionSamples++
		case captured:
			capturedSamples++
		default:
			durableFallbackSamples++
		}
		cohort = append(cohort, row)
	}
	model := FitRevenueCalibration(cohort)
	model.Since = regimeSince
	model.RetainedSince = retainedSince
	model.FittedAt = asOf
	model.AdmissionSamples = admissionSamples
	model.CapturedSamples = capturedSamples
	model.DurableFallbackSamples = durableFallbackSamples
	model.ExcludedSamples = excludedSamples
	return model, nil
}

// FitRevenueCalibration accepts only complete full-lot commercial receipts.
// Callers own the regime and time bounds; online callers use only settled history.
func FitRevenueCalibration(settlements []Settlement) RevenueCalibrationModel {
	groups := make(map[string][]int64)
	for _, row := range settlements {
		forecast, comparable := calibrationForecast(row)
		if !calibrationSettlementEligible(row) || !comparable {
			continue
		}
		ratio := int64(20_000)
		high, low := bits.Mul64(uint64(max(0, row.ProceedsPence)), 10_000)
		divisor := uint64(forecast.RevenuePence)
		if high < divisor {
			quotient, _ := bits.Div64(high, low, divisor)
			ratio = int64(min(uint64(20_000), quotient))
		}
		for _, key := range revenueCalibrationKeys(row.Commodity, forecast.Basis, RevenueHorizonBucket(forecast.HorizonGameDays)) {
			groups[key] = append(groups[key], ratio)
		}
	}
	model := RevenueCalibrationModel{Version: RevenueCalibrationVersion, Cells: make(map[string]RevenueCalibrationCell, len(groups))}
	for key, ratios := range groups {
		sort.Slice(ratios, func(i, j int) bool { return ratios[i] < ratios[j] })
		n := len(ratios)
		median := ratios[n/2]
		if n%2 == 0 {
			median = (ratios[n/2-1] + median) / 2
		}
		// Taking p65 of nonnegative adverse misses is p35 of receipt factors.
		adverse := ratios[n-int(math.Ceil(float64(n)*0.65))]
		model.Cells[key] = RevenueCalibrationCell{Samples: n, MedianBPS: median, AdverseBPS: min(median, adverse)}
	}
	return model
}

func calibrationSettlementEligible(row Settlement) bool {
	if !row.CostsKnown || row.ProceedsUnknown || row.QuantityTons <= 0 ||
		row.CargoShortfallPence != nil && *row.CargoShortfallPence != 0 {
		return false
	}
	switch row.Kind {
	case "spot", "charter", "freight":
		_, valid := calibrationForecast(row)
		return valid
	default:
		return false
	}
}

func calibrationForecast(row Settlement) (RevenueForecast, bool) {
	forecast := row.AdmissionRevenue
	if forecast == nil {
		if row.PlannedRevenuePence == nil || row.RevenueBasis == nil {
			return RevenueForecast{}, false
		}
		forecast = &RevenueForecast{
			Kind: row.Kind, Commodity: row.Commodity, Basis: *row.RevenueBasis,
			QuantityTons: row.QuantityTons, RevenuePence: *row.PlannedRevenuePence,
			HorizonGameDays: float64(row.GameSeconds) / 86_400,
		}
	}
	valid := forecast.Kind == row.Kind && forecast.Commodity == row.Commodity && forecast.QuantityTons == row.QuantityTons &&
		forecast.RevenuePence > 0 && strings.TrimSpace(forecast.Basis) != "" && !forecast.EventStress &&
		forecast.HorizonGameDays > 0 && !math.IsNaN(forecast.HorizonGameDays) && !math.IsInf(forecast.HorizonGameDays, 0)
	return *forecast, valid
}
