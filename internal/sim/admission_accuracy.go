package sim

import (
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type AdmissionComparison struct {
	Old                   KindAccuracy `json:"old"`
	OldStressed           KindAccuracy `json:"old_stressed"`
	Calibrated            KindAccuracy `json:"calibrated"`
	Stressed              KindAccuracy `json:"stressed"`
	Admitted              KindAccuracy `json:"admitted"`
	Rejected              int          `json:"rejected"`
	RejectedLosses        int          `json:"rejected_losses"`
	RejectedRealizedPence int64        `json:"rejected_realized_pence"`
}

type AdmissionAccuracy struct {
	Calibration       store.RevenueCalibrationModel  `json:"calibration"`
	ByKind            map[string]AdmissionComparison `json:"by_kind"`
	FitSamples        int                            `json:"fit_samples"`
	InFlightAtFit     int                            `json:"in_flight_at_fit"`
	ExcludedFullLots  int                            `json:"excluded_full_lots"`
	ExcludedMixedLots int                            `json:"excluded_mixed_lots"`
}

// compareAdmission holds the recorded lot and costs fixed. It measures receipt
// calibration and admission selection, not the profits of hypothetical voyages.
func compareAdmission(observations []AdmissionObservation, settlements []store.Settlement, plans []store.ReplayPlan, from, fitUntil, to time.Time, regime store.RevenueCalibrationModel) (AdmissionAccuracy, error) {
	report := AdmissionAccuracy{ByKind: make(map[string]AdmissionComparison)}
	since := from
	if regime.Since.After(since) {
		since = regime.Since
	}
	report.Calibration = store.RevenueCalibrationModel{Version: store.RevenueCalibrationVersion, Since: since, RetainedSince: regime.RetainedSince}
	if !since.After(fitUntil) {
		var err error
		report.Calibration, err = store.FitRevenueCalibrationWindow(settlements, plans, since, regime.RetainedSince, fitUntil)
		if err != nil {
			return report, err
		}
	}
	if global, exists := report.Calibration.Cells["global"]; exists {
		report.FitSamples += global.Samples
	}
	if contracts, exists := report.Calibration.Cells["contract"]; exists {
		report.FitSamples += contracts.Samples
	}
	old, calibrated, admitted := newAccuracyAccumulators(), newAccuracyAccumulators(), newAccuracyAccumulators()
	stressed := newAccuracyAccumulators()
	oldStressed := newAccuracyAccumulators()
	for _, observation := range observations {
		row, plan := observation.Settlement, observation.Plan
		if row.CompletedAt.Before(fitUntil) || !row.CompletedAt.Before(to) {
			continue
		}
		if row.StartedAt.Before(fitUntil) {
			report.InFlightAtFit++
			continue
		}
		if plan.Kind != "spot" && plan.Kind != "charter" && plan.Kind != "freight" {
			if plan.Kind == "spot+freight" || plan.Kind == "charter+freight" {
				report.ExcludedMixedLots++
			}
			continue
		}
		if !admissionFullLot(row, plan) {
			report.ExcludedFullLots++
			continue
		}
		realized, known := accuracyRealized(row)
		if !known {
			continue
		}
		kind := row.Kind
		oldAccumulator := accuracyAccumulatorFor(old, kind)
		oldAccumulator.addSettlement(row)
		oldAccumulator.addForecast(row, plan, plan.Economics.ExpectedNetPence, false)
		oldStressedAccumulator := accuracyAccumulatorFor(oldStressed, kind)
		oldStressedAccumulator.addSettlement(row)
		oldStressedAccumulator.addForecast(row, plan, plan.Economics.ExpectedNetPence-plan.Economics.PriceUncertaintyStressPence, false)
		revenue := plan.Economics.RevenuePence
		if plan.Economics.UncalibratedRevenuePence != nil {
			revenue = *plan.Economics.UncalibratedRevenuePence
		}
		horizon := plan.Economics.RevenueHorizonGameDays
		if horizon == 0 {
			horizon = plan.Economics.GameDays
		}
		factor := report.Calibration.Factor(plan.Commodity, plan.Economics.RevenueBasis, horizon)
		// New captures carry the exact age add-on used online. Legacy raw
		// quotes were already aged, so absence means no additional aging.
		if !strings.HasPrefix(factor.Source, "basis/retained/") {
			factor.MedianBPS = max(0, factor.MedianBPS-plan.Economics.RevenueAgeAddonBPS)
			factor.AdverseBPS = max(0, factor.AdverseBPS-plan.Economics.RevenueAgeAddonBPS)
		}
		median, adverse := revenue, revenue
		if plan.Kind != "freight" {
			median = revenue - ceilBPS(revenue, 10_000-factor.MedianBPS)
			adverse = revenue - ceilBPS(revenue, 10_000-factor.AdverseBPS)
		}
		forecast := plan.Economics.ExpectedNetPence + median - plan.Economics.RevenuePence
		stress := median - adverse
		if plan.Economics.RevenueBasis == "gossip" && plan.Quantity > 0 && plan.SellLimitPence > 0 &&
			plan.SellLimitPence <= revenue/plan.Quantity {
			quoteStress := revenue - plan.SellLimitPence*plan.Quantity
			stress = max(stress, ceilBPS(quoteStress, factor.MedianBPS))
		}
		if plan.EventPriceStress {
			stress = max(stress, ceilBPS(median, engine.MarketEventStressBPS))
		}
		calibratedPlan := plan
		calibratedPlan.Economics.RevenuePence = median
		calibratedPlan.Economics.ExpectedNetPence = forecast
		calibratedPlan.Economics.PriceUncertaintyStressPence = stress
		calibratedAccumulator := accuracyAccumulatorFor(calibrated, kind)
		calibratedAccumulator.addSettlement(row)
		calibratedAccumulator.addForecast(row, calibratedPlan, forecast, false)
		stressedAccumulator := accuracyAccumulatorFor(stressed, kind)
		stressedAccumulator.addSettlement(row)
		stressedAccumulator.addForecast(row, calibratedPlan, forecast-stress, false)
		minimum := ceilBPS(plan.Economics.PurchasePence+plan.Economics.CustomsPence, 200)
		if forecast-stress > 0 && forecast >= minimum {
			accumulator := accuracyAccumulatorFor(admitted, kind)
			accumulator.addSettlement(row)
			accumulator.addForecast(row, calibratedPlan, forecast, false)
		} else {
			comparison := report.ByKind[kind]
			comparison.Rejected++
			comparison.RejectedRealizedPence += realized
			if realized < 0 {
				comparison.RejectedLosses++
			}
			report.ByKind[kind] = comparison
		}
	}
	oldStats, calibratedStats, admittedStats := finishAccuracy(old), finishAccuracy(calibrated), finishAccuracy(admitted)
	stressedStats := finishAccuracy(stressed)
	oldStressedStats := finishAccuracy(oldStressed)
	for kind, stats := range oldStats {
		comparison := report.ByKind[kind]
		comparison.Old, comparison.Calibrated, comparison.Admitted = stats, calibratedStats[kind], admittedStats[kind]
		comparison.Stressed = stressedStats[kind]
		comparison.OldStressed = oldStressedStats[kind]
		report.ByKind[kind] = comparison
	}
	return report, nil
}

func admissionFullLot(row store.Settlement, plan engine.Plan) bool {
	return row.CostsKnown && !row.ProceedsUnknown && row.QuantityTons > 0 && row.QuantityTons == plan.Economics.RevenueQuantityTons &&
		row.PlannedRevenuePence != nil && *row.PlannedRevenuePence > 0 && plan.Economics.GameDays > 0 &&
		(row.CargoShortfallPence == nil || *row.CargoShortfallPence == 0)
}
