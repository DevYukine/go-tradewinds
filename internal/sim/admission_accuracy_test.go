package sim

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestAdmissionAccuracyFitsBeforeBoundaryAndMeasuresSameHoldout(t *testing.T) {
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	fitUntil, to := from.Add(time.Hour), from.Add(2*time.Hour)
	observation := func(start, complete time.Time, cost int64) AdmissionObservation {
		return AdmissionObservation{
			Settlement: store.Settlement{Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 86400, AcquisitionPence: cost, ProceedsPence: 7000, PlannedRevenuePence: new(int64(10000)),
				RevenueBasis: new("depth"), StartedAt: start, CompletedAt: complete},
			Plan: engine.Plan{Kind: "spot", Commodity: "coal", Quantity: 1, Economics: engine.Economics{
				RevenuePence: 10000, RevenueQuantityTons: 1, UncalibratedRevenuePence: new(int64(10000)),
				RevenueBasis: "depth", PurchasePence: cost, ExpectedNetPence: 10000 - cost, GameDays: 1}},
		}
	}
	var rows []AdmissionObservation
	for i := range 10 {
		rows = append(rows, observation(from.Add(time.Minute), from.Add(time.Duration(i+2)*time.Minute), 6000))
	}
	rows = append(rows, observation(fitUntil.Add(time.Minute), fitUntil.Add(2*time.Minute), 6000))
	rows = append(rows, observation(fitUntil.Add(time.Minute), fitUntil.Add(3*time.Minute), 9000))
	// This outcome is held out by completion, but calibration would have been
	// unavailable when it was admitted. It must not receive a hindsight reprice.
	rows = append(rows, observation(fitUntil.Add(-time.Minute), fitUntil.Add(time.Minute), 6000))
	first := compareAdmissionObservations(t, rows, from, fitUntil, to)
	spot := first.ByKind["spot"]
	if first.FitSamples != 10 || first.InFlightAtFit != 1 || spot.Old.Covered != 2 ||
		spot.Old.MAE != 3000 || spot.Calibrated.MAE != 0 || spot.Rejected != 1 || spot.RejectedLosses != 1 ||
		spot.RejectedRealizedPence != -2000 || spot.Admitted.Covered != 1 {
		t.Fatalf("holdout was mixed, peeked, or scored on different lots: %+v", first)
	}
	rows[10].Settlement.ProceedsPence = 1000
	second := compareAdmissionObservations(t, rows, from, fitUntil, to)
	if second.Calibration.Factor("coal", "depth", 1) != first.Calibration.Factor("coal", "depth", 1) {
		t.Fatal("a holdout outcome changed its fitted haircut")
	}
}

func TestAdmissionAccuracySeparatesOldPointAndStressedScores(t *testing.T) {
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	fit, to := from.Add(time.Hour), from.Add(2*time.Hour)
	makeObservation := func(start, complete time.Time, proceeds int64) AdmissionObservation {
		return AdmissionObservation{
			Settlement: store.Settlement{Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 86400, AcquisitionPence: 6000, ProceedsPence: proceeds, PlannedRevenuePence: new(int64(10000)),
				RevenueBasis: new("depth"), StartedAt: start, CompletedAt: complete},
			Plan: engine.Plan{Kind: "spot", Commodity: "coal", Quantity: 1, Economics: engine.Economics{
				RevenuePence: 10000, RevenueQuantityTons: 1, UncalibratedRevenuePence: new(int64(10000)),
				RevenueBasis: "depth", PurchasePence: 6000, ExpectedNetPence: 4000, PriceUncertaintyStressPence: 500, GameDays: 1}},
		}
	}
	var observations []AdmissionObservation
	for i, proceeds := range []int64{6500, 7000, 7000, 7000, 8000, 8000, 8000, 9000, 10000, 10000} {
		observations = append(observations, makeObservation(from, from.Add(time.Duration(i+1)*time.Minute), proceeds))
	}
	observations = append(observations, makeObservation(fit, fit.Add(time.Minute), 7000))
	spot := compareAdmissionObservations(t, observations, from, fit, to).ByKind["spot"]
	if spot.Old.MAE != 3000 || spot.OldStressed.MAE != 2500 || spot.Calibrated.MAE != 1000 || spot.Stressed.MAE != 0 {
		t.Fatalf("different forecast bases were silently mixed: %+v", spot)
	}
}

func compareAdmissionObservations(t *testing.T, observations []AdmissionObservation, from, fit, to time.Time) AdmissionAccuracy {
	t.Helper()
	settlements := make([]store.Settlement, len(observations))
	plans := make([]store.ReplayPlan, len(observations))
	for i, observation := range observations {
		row := observation.Settlement
		row.ID = fmt.Sprintf("settlement-%d", i)
		row.MissionID = fmt.Sprintf("mission-%d", i)
		settlements[i] = row
		raw, err := json.Marshal(observation.Plan)
		if err != nil {
			t.Fatal(err)
		}
		plans[i] = store.ReplayPlan{ID: int64(i + 1), MissionID: row.MissionID,
			ObservedAt: row.StartedAt, Plan: raw}
	}
	report, err := compareAdmission(observations, settlements, plans, from, fit, to, store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestAdmissionAccuracyUsesCapturedRetainedAddonAndFittedGossipStress(t *testing.T) {
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	fit, to := from.Add(time.Hour), from.Add(2*time.Hour)
	var observations []AdmissionObservation
	for i, proceeds := range []int64{6500, 7000, 7000, 7000, 8000, 8000, 8000, 9000, 10000, 10000, 7000, 7000} {
		start := from
		basis := "depth"
		addon := int64(0)
		if i >= 10 {
			start = fit
			basis = "gossip"
			if i == 11 {
				basis, addon = "retained", 200
			}
		}
		observations = append(observations, AdmissionObservation{
			Settlement: store.Settlement{Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 86400, AcquisitionPence: 6000, ProceedsPence: proceeds, PlannedRevenuePence: new(int64(10000)),
				RevenueBasis: new(basis), StartedAt: start, CompletedAt: start.Add(time.Duration(i+1) * time.Minute)},
			Plan: engine.Plan{Kind: "spot", Commodity: "coal", Quantity: 1, Economics: engine.Economics{
				RevenuePence: 10000, RevenueQuantityTons: 1, UncalibratedRevenuePence: new(int64(10000)),
				RevenueBasis: basis, RevenueAgeAddonBPS: addon, RevenueHorizonGameDays: 1, PurchasePence: 6000,
				ExpectedNetPence: 4000, PriceUncertaintyStressPence: 5000, GameDays: 1}},
		})
	}
	report := compareAdmissionObservations(t, observations, from, fit, to)
	spot := report.ByKind["spot"]
	if spot.Calibrated.ForecastPence != 3800 || spot.Stressed.ForecastPence != 1800 || spot.Rejected != 0 {
		t.Fatalf("holdout reused obsolete gossip stress or omitted the captured retained age add-on: %+v", spot)
	}
}

func TestAdmissionAccuracyKeepsTheRecordedCalibrationRegime(t *testing.T) {
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	since, boundary, fit := from.Add(time.Minute), from.Add(20*time.Minute), from.Add(time.Hour)
	var settlements []store.Settlement
	for i := range 40 {
		row := store.Settlement{
			ID: fmt.Sprintf("regime-%d", i), MissionID: fmt.Sprintf("regime-%d", i),
			Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
			StartedAt: since, CompletedAt: boundary.Add(time.Minute),
			AdmissionRevenue: &store.RevenueForecast{Kind: "spot", Commodity: "coal", Basis: "depth",
				QuantityTons: 1, RevenuePence: 10_000, HorizonGameDays: 1},
			ProceedsPence: 1000,
		}
		switch {
		case i < 10:
			row.StartedAt = from
		case i < 20:
			row.AdmissionRevenue.Basis = "retained"
		case i < 30:
			row.AdmissionRevenue.EventStress = true
		default:
			row.ProceedsPence = 9500
		}
		settlements = append(settlements, row)
	}
	report, err := compareAdmission(nil, settlements, nil, from, fit, fit.Add(time.Hour),
		store.RevenueCalibrationModel{Since: since, RetainedSince: boundary})
	if err != nil || report.FitSamples != 10 || !report.Calibration.Since.Equal(since) ||
		report.Calibration.Cells["global"].MedianBPS != 9500 {
		t.Fatalf("offline fit mixed pre-regime, legacy retained or temporary event quotes: %+v, %v", report, err)
	}
}

func TestAdmissionAccuracyKeepsTheGossipLiquidationFloor(t *testing.T) {
	from := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	fit, to := from.Add(time.Hour), from.Add(2*time.Hour)
	var observations []AdmissionObservation
	for i := range 10 {
		observations = append(observations, AdmissionObservation{
			Settlement: store.Settlement{Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 86400, AcquisitionPence: 6000, ProceedsPence: 10000, PlannedRevenuePence: new(int64(10000)),
				RevenueBasis: new("depth"), StartedAt: from, CompletedAt: from.Add(time.Duration(i+1) * time.Minute)},
			Plan: engine.Plan{Kind: "spot", Commodity: "coal", Quantity: 1, Economics: engine.Economics{
				RevenuePence: 10000, RevenueQuantityTons: 1, RevenueBasis: "depth", GameDays: 1}},
		})
	}
	for _, revenue := range []int64{50000, 60000} {
		observations = append(observations, AdmissionObservation{
			Settlement: store.Settlement{Kind: "spot", Commodity: "coal", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 86400, AcquisitionPence: 46000, ProceedsPence: 45000, PlannedRevenuePence: new(revenue),
				RevenueBasis: new("gossip"), StartedAt: fit, CompletedAt: fit.Add(time.Minute)},
			Plan: engine.Plan{Kind: "spot", Commodity: "coal", Quantity: 1, SellLimitPence: 45000, Economics: engine.Economics{
				RevenuePence: revenue, RevenueQuantityTons: 1, RevenueBasis: "gossip", GameDays: 1,
				ExpectedNetPence: revenue - 46000, PurchasePence: 46000}},
		})
	}
	report := compareAdmissionObservations(t, observations, from, fit, to)
	spot := report.ByKind["spot"]
	if spot.Rejected != 2 || spot.RejectedLosses != 2 || spot.Stressed.ForecastPence != -2000 {
		t.Fatalf("counterfactual gossip lots bypassed the production NPC liquidation floor: %+v", spot)
	}
}
