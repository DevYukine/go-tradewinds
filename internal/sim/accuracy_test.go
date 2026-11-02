package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func accuracyFixture(t *testing.T, id int64, kind string, completed time.Time, forecast, realized int64) (store.Settlement, store.ReplayPlan) {
	t.Helper()
	row := store.Settlement{ID: fmt.Sprintf("settlement-%03d", id), AccountID: "accuracy", MissionID: fmt.Sprintf("mission-%03d", id),
		Kind: kind, Commodity: "coffee_cocoa", QuantityTons: 10, CostsKnown: true,
		AcquisitionPence: 1000, FeesPence: 20, ProvisionsPence: 30, ExpensesPence: 50, ProceedsPence: 1100 + realized,
		StartedAt: completed.Add(-10 * time.Minute), CompletedAt: completed}
	plan := engine.Plan{Kind: kind, Commodity: row.Commodity, Quantity: 10, ExpectedArrival: completed.Add(-30 * time.Second),
		Economics: engine.Economics{ExpectedNetPence: forecast, RevenuePence: 1100 + forecast, RevenueQuantityTons: 10, GameDays: 2}}
	return row, accuracyRecord(t, id, row, plan, row.StartedAt)
}

func accuracyRecord(t *testing.T, id int64, row store.Settlement, plan engine.Plan, observed time.Time) store.ReplayPlan {
	t.Helper()
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return store.ReplayPlan{ID: id, AccountID: row.AccountID, MissionID: row.MissionID, ObservedAt: observed, Plan: encoded}
}

func TestAccuracyKeepsEarliestAdmissionAndLatestSeparate(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	to, fit := from.Add(4*time.Hour), from.Add(2*time.Hour)
	row, record := accuracyFixture(t, 1, "spot", from.Add(time.Hour), 100, -50)
	var plan engine.Plan
	if err := json.Unmarshal(record.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	record.ObservedAt = row.StartedAt.Add(time.Second)
	plan.Economics.ExpectedNetPence = 200
	tie := accuracyRecord(t, 2, row, plan, record.ObservedAt)
	plan.Economics.ExpectedNetPence = 300
	reprice := accuracyRecord(t, 3, row, plan, row.StartedAt.Add(2*time.Second))
	plan.Economics.ExpectedNetPence = 999
	atCompletion := accuracyRecord(t, 4, row, plan, row.CompletedAt)
	future := accuracyRecord(t, 5, row, plan, row.CompletedAt.Add(time.Second))
	future.Plan = []byte("invalid future JSON")
	late, latePlan := accuracyFixture(t, 6, "spot", from.Add(90*time.Minute), 400, 10)
	latePlan.ObservedAt = late.StartedAt.Add(time.Second + time.Nanosecond)
	plans := []store.ReplayPlan{future, reprice, latePlan, tie, atCompletion, record}
	report, err := Accuracy([]store.Settlement{late, row}, plans, from, to, fit, store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	admission, latest := report.ByKind["spot"], report.Latest["spot"]
	if admission.N != 2 || admission.Covered != 1 || admission.ForecastPence != 100 || admission.RealizedPence != -50 || admission.ErrorPence != -150 {
		t.Fatalf("later capture replaced admission or already-sailing capture was admitted: %+v", admission)
	}
	if latest.Covered != 2 || latest.ForecastPence != 700 || latest.RealizedPence != -40 {
		t.Fatalf("latest forecast crossed completion or was mixed into admission: %+v", latest)
	}
	if report.Coverage.NoAdmissionPlan != 1 || report.Coverage.NoLatestPlan != 0 || len(report.Admission) != 1 ||
		report.Admission[0].Plan.Economics.ExpectedNetPence != 100 || !report.Admission[0].ObservedAt.Equal(record.ObservedAt) {
		t.Fatalf("admission cohort was not exported with its exact boundary: %+v", report.Coverage)
	}
	arrival := report.ArrivalErrors["admission"]["spot"]
	if arrival.N != 1 || arrival.BiasSeconds != 30 || arrival.MAESeconds != 30 {
		t.Fatalf("planning arrival diagnostic used a departure ETA or future reprice: %+v", arrival)
	}
	plan.Economics.ExpectedNetPence = -900
	plans[1] = accuracyRecord(t, 3, row, plan, reprice.ObservedAt)
	again, err := Accuracy([]store.Settlement{row, late}, plans, from, to, fit, store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.ByKind, again.ByKind) || !reflect.DeepEqual(report.Calibration, again.Calibration) {
		t.Fatal("a later reprice changed the admission forecast or fit")
	}
}

func TestAccuracyFitsNetRatiosOnlyBeforeBoundaryAndScoresHoldout(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	fit, to := from.Add(4*time.Hour), from.Add(8*time.Hour)
	var rows []store.Settlement
	var plans []store.ReplayPlan
	for i, terms := range []struct {
		completed time.Time
		forecast  int64
		realized  int64
	}{
		{from.Add(time.Hour), 100, 20},
		{from.Add(2 * time.Hour), 100, 60},
		{from.Add(3 * time.Hour), 100, 100},
		{fit, 100, 0},
		{fit.Add(time.Hour), 100, -20},
		{fit.Add(2 * time.Hour), -100, -50},
	} {
		row, plan := accuracyFixture(t, int64(i+1), "spot", terms.completed, terms.forecast, terms.realized)
		if i == 0 {
			row.QuantityTons--
		}
		rows, plans = append(rows, row), append(plans, plan)
	}
	report, err := Accuracy(rows, plans, from, to, fit, store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	calibration := report.Calibration["spot"]
	if !calibration.Available || calibration.Samples != 3 || math.Abs(calibration.Median-0.6) > 1e-12 ||
		math.Abs(calibration.P25-0.4) > 1e-12 || math.Abs(calibration.P75-0.8) > 1e-12 {
		t.Fatalf("completion boundary leaked holdout into ratio fit or excluded net evidence: %+v", calibration)
	}
	holdout := report.Holdout["spot"]
	if holdout.N != 3 || holdout.Covered != 3 || holdout.ForecastPence != 60 || holdout.RealizedPence != -70 || holdout.ErrorPence != -130 ||
		holdout.MAE != 50 || holdout.MedianAE != 60 || math.Abs(holdout.P90AE-76) > 1e-12 || math.Abs(holdout.SignAccuracy-2.0/3) > 1e-12 ||
		holdout.FalseLossRate != 0.5 || holdout.RevenueCovered != 0 {
		t.Fatalf("holdout did not apply the training median to the admission forecast: %+v", holdout)
	}
	if report.ByKind["spot"].ForecastPence != 400 || report.ByKind["spot"].RealizedPence != 110 || report.ByKind["spot"].QuantityMismatch != 1 {
		t.Fatalf("net totals silently excluded a partial lot or negative forecast: %+v", report.ByKind["spot"])
	}
	rows[3].ProceedsPence += 100000
	again, err := Accuracy(rows, plans, from, to, fit, store.RevenueCalibrationModel{})
	if err != nil || !reflect.DeepEqual(report.Calibration, again.Calibration) {
		t.Fatalf("future outcomes changed the fitted ratios: %+v, %v", again, err)
	}
}

func TestAccuracyFullLotsDecomposeRevenueAndCostMisses(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	var rows []store.Settlement
	var plans []store.ReplayPlan
	for i, realized := range []int64{50, -50, 20} {
		row, plan := accuracyFixture(t, int64(i+1), "spot", from.Add(time.Duration(i+1)*time.Hour), 100, realized)
		if i == 0 {
			revenue := int64(1300)
			row.PlannedRevenuePence = &revenue
		}
		if i == 1 {
			row.QuantityTons = 9
		}
		rows, plans = append(rows, row), append(plans, plan)
	}
	report, err := Accuracy(rows, plans, from, from.Add(6*time.Hour), from.Add(4*time.Hour), store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	actual := report.ByKind["spot"]
	if actual.Covered != 3 || actual.RevenueCovered != 2 || actual.QuantityMismatch != 1 || actual.ErrorPence != -280 ||
		actual.RevenueMissPence != -230 || actual.CostMissPence != 100 ||
		actual.SelectedRevenueMissPence != -130 || actual.SelectedCostMissPence != 0 {
		t.Fatalf("partial-lot revenue entered a full-lot split or exact 1100-pence costs became a selected cost miss: %+v", actual)
	}
}

func TestAccuracySeparatesSelectedPlanCostsFromDurableRevenueRepricing(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	row, first := accuracyFixture(t, 1, "spot", from.Add(time.Hour), 100, 50)
	durableRevenue := int64(1300)
	row.PlannedRevenuePence = &durableRevenue
	var plan engine.Plan
	if err := json.Unmarshal(first.Plan, &plan); err != nil {
		t.Fatal(err)
	}
	plan.Economics.ExpectedNetPence = 300
	plan.Economics.RevenuePence = 1400
	latest := accuracyRecord(t, 2, row, plan, row.StartedAt.Add(2*time.Second))
	report, err := Accuracy([]store.Settlement{row}, []store.ReplayPlan{latest, first}, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name            string
		value           KindAccuracy
		bridge          int64
		selectedRevenue int64
	}{
		{"admission", report.ByKind["spot"], 100, -50},
		{"latest", report.Latest["spot"], -100, -250},
	} {
		if test.value.RevenueCovered != 1 || test.value.RevenueMissPence != -150 ||
			test.value.CostMissPence != test.bridge || test.value.SelectedRevenueMissPence != test.selectedRevenue ||
			test.value.SelectedCostMissPence != 0 {
			t.Fatalf("%s mixed durable repricing into the selected plan's exact 1100-pence costs: %+v", test.name, test.value)
		}
		if test.value.SelectedRevenueMissPence+test.value.SelectedCostMissPence != test.value.ErrorPence ||
			test.value.RevenueMissPence+test.value.CostMissPence != test.value.ErrorPence {
			t.Fatalf("%s decomposition did not bridge to net error: %+v", test.name, test.value)
		}
	}
	for _, costChange := range []int64{-25, 25} {
		changed := row
		changed.ExpensesPence += costChange
		again, err := Accuracy([]store.Settlement{changed}, []store.ReplayPlan{latest, first}, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
		if err != nil {
			t.Fatal(err)
		}
		if again.ByKind["spot"].SelectedCostMissPence != -costChange || again.Latest["spot"].SelectedCostMissPence != -costChange {
			t.Fatalf("actual cost change %d did not retain its savings sign in both selected plans: admission=%+v latest=%+v",
				costChange, again.ByKind["spot"], again.Latest["spot"])
		}
	}
	var text bytes.Buffer
	if _, err := report.WriteTo(&text); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"cost/reprice bridge", "Selected-revenue-miss(Cr)", "Selected-cost-miss(Cr)"} {
		if !strings.Contains(text.String(), label) {
			t.Fatalf("report omitted %q: %s", label, text.String())
		}
	}
}

func TestAccuracyBiasUsesMagnitudeOfNegativeForecastTotals(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		realized [2]int64
		bias     float64
	}{
		{"better than forecast", [2]int64{-50, -250}, 25},
		{"worse than forecast", [2]int64{-150, -350}, -25},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rows []store.Settlement
			var plans []store.ReplayPlan
			for i, forecast := range []int64{-100, -300} {
				row, plan := accuracyFixture(t, int64(i+1), "spot", from.Add(time.Duration(1+2*i)*time.Hour), forecast, test.realized[i])
				rows, plans = append(rows, row), append(plans, plan)
			}
			report, err := Accuracy(rows, plans, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
			if err != nil {
				t.Fatal(err)
			}
			if report.ByKind["spot"].ForecastPence != -400 || report.ByKind["spot"].BiasPercent != test.bias ||
				report.Latest["spot"].BiasPercent != test.bias {
				t.Fatalf("negative forecast total reversed the bias direction: admission=%+v latest=%+v", report.ByKind["spot"], report.Latest["spot"])
			}
		})
	}
}

func TestAccuracyKindCoverageAndUnpricedPassage(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	var rows []store.Settlement
	var plans []store.ReplayPlan
	for i, kind := range []string{"spot", "charter", "freight", "spot+freight", "charter+freight", "passage", "relocation", "aborted", "probe", "recovery"} {
		row, plan := accuracyFixture(t, int64(i+1), kind, from.Add(time.Hour), 100, 50)
		if kind == "passage" {
			plan = accuracyRecord(t, plan.ID, row, engine.Plan{Kind: kind}, row.StartedAt)
			row.QuantityTons = 12
		}
		rows, plans = append(rows, row), append(plans, plan)
	}
	unknownCosts, costPlan := accuracyFixture(t, 11, "spot", from.Add(time.Hour), 100000, 50000)
	unknownCosts.CostsKnown = false
	unknownProceeds, proceedsPlan := accuracyFixture(t, 12, "spot", from.Add(time.Hour), 100000, 50000)
	unknownProceeds.ProceedsUnknown = true
	missing, _ := accuracyFixture(t, 13, "spot", from.Add(time.Hour), 100000, 25)
	mismatch, mismatchPlan := accuracyFixture(t, 14, "spot", from.Add(time.Hour), 100000, 25)
	mismatchPlan = accuracyRecord(t, mismatchPlan.ID, mismatch, engine.Plan{Kind: "charter"}, mismatch.StartedAt)
	rows = append(rows, unknownCosts, unknownProceeds, missing, mismatch)
	plans = append(plans, costPlan, proceedsPlan, mismatchPlan)
	outside, outsidePlan := accuracyFixture(t, 15, "spot", from.Add(4*time.Hour), 100000, 50000)
	rows, plans = append(rows, outside), append(plans, outsidePlan)
	report, err := Accuracy(rows, plans, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"spot", "charter", "freight", "spot+freight", "charter+freight"} {
		if report.ByKind[kind].Covered != 1 || report.ByKind[kind].ForecastPence != 100 || report.ByKind[kind].RealizedPence != 50 {
			t.Fatalf("kind %s was merged or unknown accounting imputed: %+v", kind, report.ByKind[kind])
		}
	}
	passage := report.ByKind["passage"]
	if passage.N != 1 || passage.Known != 1 || passage.Covered != 0 || passage.ForecastAvailable || passage.ForecastPence != 0 || passage.RealizedPence != 50 || passage.QuantityTons != 12 || report.Calibration["passage"].Available {
		t.Fatalf("unpriced passage was dressed up as a zero forecast: %+v", passage)
	}
	coverage := report.Coverage
	if coverage.Settlements != 14 || coverage.CoverageOnly != 4 || coverage.NoPlan != 1 || coverage.NoAdmissionPlan != 1 ||
		coverage.AdmissionKindMismatch != 1 || coverage.LatestKindMismatch != 1 || coverage.UnknownCosts != 1 || coverage.UnknownProceeds != 1 || coverage.UnpricedPassage != 1 {
		t.Fatalf("coverage gaps were hidden: %+v", coverage)
	}
	if spot := report.ByKind["spot"]; spot.N != 5 || spot.Known != 3 || spot.KnownRealizedPence != 100 {
		t.Fatalf("all-known and comparable realized cohorts were confused: %+v", spot)
	}
	if len(report.Admission) != 8 {
		t.Fatalf("exported cohort lost unknown economics or included coverage-only/mismatched rows: %d", len(report.Admission))
	}
	var text bytes.Buffer
	if _, err := report.WriteTo(&text); err != nil || !strings.Contains(text.String(), "forecast_available=false") || !strings.Contains(text.String(), "coverage_only=4") {
		t.Fatalf("report hid unpriced passage or missing coverage: %s, %v", text.String(), err)
	}
	for _, note := range []string{"Coverage counters overlap", "valid captured full-lot receipt cohort only"} {
		if !strings.Contains(text.String(), note) {
			t.Fatalf("report omitted cohort limitation %q: %s", note, text.String())
		}
	}
}

func TestAccuracyJSONIsDeterministicAndAccountScoped(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	first, firstPlan := accuracyFixture(t, 1, "spot", from.Add(time.Hour), 100, 20)
	second, secondPlan := accuracyFixture(t, 2, "charter", from.Add(time.Hour), 200, 60)
	foreign := firstPlan
	foreign.ID, foreign.AccountID, foreign.Plan = 99, "other-account", []byte("invalid foreign JSON")
	left, err := Accuracy([]store.Settlement{second, first}, []store.ReplayPlan{foreign, secondPlan, firstPlan}, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	right, err := Accuracy([]store.Settlement{first, second}, []store.ReplayPlan{firstPlan, secondPlan, foreign}, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{})
	if err != nil {
		t.Fatal(err)
	}
	leftJSON, err := json.MarshalIndent(left, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.MarshalIndent(right, "", "  ")
	if err != nil || !bytes.Equal(leftJSON, rightJSON) {
		t.Fatalf("input order changed deterministic accuracy JSON: %v", err)
	}
	if bytes.Contains(leftJSON, []byte(`"Admission"`)) || bytes.Contains(leftJSON, []byte(`"Plan"`)) {
		t.Fatal("internal reusable cohort bloated the report JSON")
	}
	for _, field := range []string{`"selected_revenue_miss_pence"`, `"selected_cost_miss_pence"`} {
		if !bytes.Contains(leftJSON, []byte(field)) {
			t.Fatalf("accuracy JSON omitted %s", field)
		}
	}
	firstPlan.Plan = []byte("invalid relevant JSON")
	if report, err := Accuracy([]store.Settlement{first}, []store.ReplayPlan{firstPlan}, from, from.Add(4*time.Hour), from.Add(2*time.Hour), store.RevenueCalibrationModel{}); err == nil || report != nil {
		t.Fatalf("invalid relevant forecast became a partial report: %+v, %v", report, err)
	}
}

func TestAccuracyRejectsInvalidWindowAndFitBoundary(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, bounds := range [][3]time.Time{{from, from, from}, {to, from, from}, {from, to, from}, {from, to, to}} {
		if report, err := Accuracy(nil, nil, bounds[0], bounds[1], bounds[2], store.RevenueCalibrationModel{}); err == nil || report != nil {
			t.Fatalf("invalid accuracy window returned a report: %+v, %v", bounds, err)
		}
	}
}

func TestTapeLoadersWithoutDatabaseLeaveSyntheticEvidenceEmpty(t *testing.T) {
	from := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	for _, loader := range []struct {
		name string
		load func(context.Context, *store.Store, string, time.Time, time.Time) (*Tape, error)
	}{
		{"replay", LoadTape},
		{"accuracy", LoadAccuracyTape},
	} {
		t.Run(loader.name, func(t *testing.T) {
			tape, err := loader.load(context.Background(), nil, "synthetic", from, from.Add(time.Hour))
			if err != nil || tape == nil || len(tape.Plans) != 0 || len(tape.Settlements) != 0 {
				t.Fatalf("synthetic tape tried to load database evidence: %+v, %v", tape, err)
			}
		})
	}
}
