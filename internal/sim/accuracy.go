package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type KindAccuracy struct {
	N                    int     `json:"n"`
	Covered              int     `json:"covered"`
	Known                int     `json:"known"`
	ForecastAvailable    bool    `json:"forecast_available"`
	QuantityTons         int64   `json:"quantity_tons"`
	KnownRealizedPence   int64   `json:"known_realized_pence"`
	ForecastPence        int64   `json:"forecast_pence"`
	RealizedPence        int64   `json:"realized_pence"`
	ErrorPence           int64   `json:"error_pence"`
	BiasPercent          float64 `json:"bias_percent"`
	MAE                  float64 `json:"mae_pence"`
	MedianAE             float64 `json:"median_ae_pence"`
	P90AE                float64 `json:"p90_ae_pence"`
	SignAccuracy         float64 `json:"sign_accuracy"`
	FalseLossRate        float64 `json:"false_loss_rate"`
	NonnegativeForecasts int     `json:"nonnegative_forecasts"`
	FalseLosses          int     `json:"false_losses"`
	RevenueCovered       int     `json:"revenue_covered"`
	RevenueMissPence     int64   `json:"revenue_miss_pence"`
	// CostMissPence is a cost/reprice bridge, including durable revenue repricing and haircuts.
	CostMissPence            int64 `json:"cost_miss_pence"`
	SelectedRevenueMissPence int64 `json:"selected_revenue_miss_pence"`
	SelectedCostMissPence    int64 `json:"selected_cost_miss_pence"`
	QuantityMismatch         int   `json:"quantity_mismatch"`
}

type NetCalibration struct {
	Samples   int     `json:"samples"`
	Available bool    `json:"available"`
	Median    float64 `json:"median"`
	P25       float64 `json:"p25"`
	P75       float64 `json:"p75"`
}

type ArrivalAccuracy struct {
	N               int     `json:"n"`
	BiasSeconds     float64 `json:"bias_seconds"`
	MAESeconds      float64 `json:"mae_seconds"`
	MedianAESeconds float64 `json:"median_ae_seconds"`
	P90AESeconds    float64 `json:"p90_ae_seconds"`
}

// Coverage counters overlap; they are not mutually exclusive exclusion buckets.
type AccuracyCoverage struct {
	Settlements           int            `json:"settlements"`
	ByKind                map[string]int `json:"by_kind"`
	NoPlan                int            `json:"no_plan"`
	NoAdmissionPlan       int            `json:"no_admission_plan"`
	NoLatestPlan          int            `json:"no_latest_plan"`
	AdmissionKindMismatch int            `json:"admission_kind_mismatch"`
	LatestKindMismatch    int            `json:"latest_kind_mismatch"`
	UnknownCosts          int            `json:"unknown_costs"`
	UnknownProceeds       int            `json:"unknown_proceeds"`
	UnpricedPassage       int            `json:"unpriced_passage"`
	CoverageOnly          int            `json:"coverage_only"`
}

// AdmissionObservation exposes the selected cohort for other offline scorers.
// Unknown accounting and partial lots remain explicit in Settlement.
type AdmissionObservation struct {
	Settlement store.Settlement
	Plan       engine.Plan
	ObservedAt time.Time
}

type AccuracyReport struct {
	From           time.Time                             `json:"from"`
	To             time.Time                             `json:"to"`
	FitUntil       time.Time                             `json:"fit_until"`
	ByKind         map[string]KindAccuracy               `json:"by_kind"`
	Latest         map[string]KindAccuracy               `json:"latest"`
	Calibration    map[string]NetCalibration             `json:"calibration"`
	Holdout        map[string]KindAccuracy               `json:"holdout"`
	ArrivalErrors  map[string]map[string]ArrivalAccuracy `json:"arrival_errors"`
	Coverage       AccuracyCoverage                      `json:"coverage"`
	AdmissionModel AdmissionAccuracy                     `json:"admission_model"`
	Admission      []AdmissionObservation                `json:"-"`
}

type accuracyMission struct {
	account string
	mission string
}

type accuracyPlan struct {
	id         int64
	observedAt time.Time
	plan       engine.Plan
}

type accuracyAccumulator struct {
	KindAccuracy
	absoluteErrors []float64
	correctSigns   int
}

func accuracyKind(kind string) bool {
	switch kind {
	case "spot", "charter", "freight", "spot+freight", "charter+freight", "passage":
		return true
	default:
		return false
	}
}

func newAccuracyAccumulators() map[string]*accuracyAccumulator {
	values := make(map[string]*accuracyAccumulator)
	for _, kind := range []string{"spot", "charter", "freight", "spot+freight", "passage"} {
		values[kind] = &accuracyAccumulator{}
	}
	return values
}

func accuracyAccumulatorFor(values map[string]*accuracyAccumulator, kind string) *accuracyAccumulator {
	if values[kind] == nil {
		values[kind] = &accuracyAccumulator{}
	}
	return values[kind]
}

func accuracyRealized(row store.Settlement) (int64, bool) {
	if !row.CostsKnown || row.ProceedsUnknown {
		return 0, false
	}
	return row.ProceedsPence - row.AcquisitionPence - row.FeesPence - row.ProvisionsPence - row.ExpensesPence, true
}

func (a *accuracyAccumulator) addSettlement(row store.Settlement) {
	a.N++
	a.QuantityTons += row.QuantityTons
	if realized, known := accuracyRealized(row); known {
		a.Known++
		a.KnownRealizedPence += realized
		// Passage has no priced forecast. Its realized total is still useful,
		// but it must not acquire zero-error or calibration statistics.
		if row.Kind == "passage" {
			a.RealizedPence += realized
		}
	}
}

func (a *accuracyAccumulator) addForecast(row store.Settlement, plan engine.Plan, forecast int64, decompose bool) {
	if row.Kind == "passage" {
		return
	}
	a.ForecastAvailable = true
	fullLot := row.QuantityTons == plan.Economics.RevenueQuantityTons
	if !fullLot {
		a.QuantityMismatch++
	}
	realized, known := accuracyRealized(row)
	if !known {
		return
	}
	a.Covered++
	a.ForecastPence += forecast
	a.RealizedPence += realized
	errorPence := realized - forecast
	a.ErrorPence += errorPence
	a.absoluteErrors = append(a.absoluteErrors, math.Abs(float64(errorPence)))
	if (forecast >= 0) == (realized >= 0) {
		a.correctSigns++
	}
	if forecast >= 0 {
		a.NonnegativeForecasts++
		if realized < 0 {
			a.FalseLosses++
		}
	}
	if fullLot && decompose {
		selectedRevenue := plan.Economics.RevenuePence
		forecastRevenue := selectedRevenue
		if row.PlannedRevenuePence != nil {
			forecastRevenue = *row.PlannedRevenuePence
		}
		a.RevenueCovered++
		a.RevenueMissPence += row.ProceedsPence - forecastRevenue
		// Durable receipts and selected net can come from different repricings,
		// so their bridge must not be interpreted as a pure cost miss.
		actualCosts := row.AcquisitionPence + row.FeesPence + row.ProvisionsPence + row.ExpensesPence
		a.CostMissPence += forecastRevenue - forecast - actualCosts
		a.SelectedRevenueMissPence += row.ProceedsPence - selectedRevenue
		// Positive selected cost miss means actual costs were lower than planned.
		a.SelectedCostMissPence += selectedRevenue - plan.Economics.ExpectedNetPence - actualCosts
	}
}

func finishAccuracy(values map[string]*accuracyAccumulator) map[string]KindAccuracy {
	result := make(map[string]KindAccuracy, len(values))
	for kind, value := range values {
		if value.Covered > 0 {
			for _, absoluteError := range value.absoluteErrors {
				value.MAE += absoluteError
			}
			value.MAE /= float64(value.Covered)
			value.MedianAE = percentile(value.absoluteErrors, 0.5)
			value.P90AE = percentile(value.absoluteErrors, 0.9)
			value.SignAccuracy = float64(value.correctSigns) / float64(value.Covered)
		}
		if value.ForecastPence != 0 {
			value.BiasPercent = float64(value.ErrorPence) / math.Abs(float64(value.ForecastPence)) * 100
		}
		if value.NonnegativeForecasts > 0 {
			value.FalseLossRate = float64(value.FalseLosses) / float64(value.NonnegativeForecasts)
		}
		result[kind] = value.KindAccuracy
	}
	return result
}

// Accuracy measures completion-window outcomes. Training requires completion
// strictly before fitUntil; no later reprice can replace the admission plan.
func Accuracy(settlements []store.Settlement, plans []store.ReplayPlan, from, to, fitUntil time.Time, regime store.RevenueCalibrationModel) (*AccuracyReport, error) {
	if !to.After(from) || !fitUntil.After(from) || !fitUntil.Before(to) {
		return nil, errors.New("accuracy requires a nonempty window and an interior fit boundary")
	}
	report := &AccuracyReport{
		From: from.UTC(), To: to.UTC(), FitUntil: fitUntil.UTC(),
		Calibration:   make(map[string]NetCalibration),
		ArrivalErrors: make(map[string]map[string]ArrivalAccuracy),
		Coverage:      AccuracyCoverage{ByKind: make(map[string]int)},
		Admission:     make([]AdmissionObservation, 0),
	}
	rows := make([]store.Settlement, 0, len(settlements))
	completed := make(map[accuracyMission]time.Time)
	for _, row := range settlements {
		if row.CompletedAt.Before(from) || !row.CompletedAt.Before(to) {
			continue
		}
		rows = append(rows, row)
		key := accuracyMission{row.AccountID, row.MissionID}
		if row.CompletedAt.After(completed[key]) {
			completed[key] = row.CompletedAt
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CompletedAt.Equal(rows[j].CompletedAt) {
			return rows[i].CompletedAt.Before(rows[j].CompletedAt)
		}
		if rows[i].AccountID != rows[j].AccountID {
			return rows[i].AccountID < rows[j].AccountID
		}
		return rows[i].ID < rows[j].ID
	})
	byMission := make(map[accuracyMission][]accuracyPlan)
	for _, record := range plans {
		key := accuracyMission{record.AccountID, record.MissionID}
		if record.MissionID == "" || !record.ObservedAt.Before(completed[key]) {
			continue
		}
		var plan engine.Plan
		if err := json.Unmarshal(record.Plan, &plan); err != nil {
			return nil, fmt.Errorf("replay plan %d has invalid forecast JSON", record.ID)
		}
		byMission[key] = append(byMission[key], accuracyPlan{record.ID, record.ObservedAt, plan})
	}
	for key := range byMission {
		sort.Slice(byMission[key], func(i, j int) bool {
			left, right := byMission[key][i], byMission[key][j]
			if !left.observedAt.Equal(right.observedAt) {
				return left.observedAt.Before(right.observedAt)
			}
			return left.id < right.id
		})
	}
	admission, latest, holdout := newAccuracyAccumulators(), newAccuracyAccumulators(), newAccuracyAccumulators()
	arrivals := map[string]map[string][]float64{"admission": {}, "latest": {}}
	for _, row := range rows {
		report.Coverage.Settlements++
		report.Coverage.ByKind[row.Kind]++
		if !row.CostsKnown {
			report.Coverage.UnknownCosts++
		}
		if row.ProceedsUnknown {
			report.Coverage.UnknownProceeds++
		}
		series := byMission[accuracyMission{row.AccountID, row.MissionID}]
		if len(series) == 0 {
			report.Coverage.NoPlan++
		}
		var firstPlan, lastPlan *accuracyPlan
		for i := range series {
			candidate := &series[i]
			if !candidate.observedAt.Before(row.CompletedAt) {
				break
			}
			lastPlan = candidate
			if firstPlan == nil && !row.StartedAt.IsZero() && !candidate.observedAt.After(row.StartedAt.Add(time.Second)) {
				firstPlan = candidate
			}
		}
		if firstPlan == nil {
			report.Coverage.NoAdmissionPlan++
		} else if firstPlan.plan.Kind != row.Kind {
			report.Coverage.AdmissionKindMismatch++
			firstPlan = nil
		}
		if lastPlan == nil {
			report.Coverage.NoLatestPlan++
		} else if lastPlan.plan.Kind != row.Kind {
			report.Coverage.LatestKindMismatch++
			lastPlan = nil
		}
		if !accuracyKind(row.Kind) {
			report.Coverage.CoverageOnly++
			continue
		}
		if row.Kind == "passage" {
			report.Coverage.UnpricedPassage++
		}
		accuracyAccumulatorFor(admission, row.Kind).addSettlement(row)
		accuracyAccumulatorFor(latest, row.Kind).addSettlement(row)
		if !row.CompletedAt.Before(fitUntil) {
			accuracyAccumulatorFor(holdout, row.Kind).addSettlement(row)
		}
		for i, selected := range [2]*accuracyPlan{firstPlan, lastPlan} {
			if selected == nil {
				continue
			}
			view, values := "admission", admission
			if i == 1 {
				view, values = "latest", latest
			} else {
				report.Admission = append(report.Admission, AdmissionObservation{row, selected.plan, selected.observedAt.UTC()})
			}
			if !selected.plan.ExpectedArrival.IsZero() {
				arrivals[view][row.Kind] = append(arrivals[view][row.Kind], row.CompletedAt.Sub(selected.plan.ExpectedArrival).Seconds())
			}
			accuracyAccumulatorFor(values, row.Kind).addForecast(row, selected.plan, selected.plan.Economics.ExpectedNetPence, true)
		}
	}
	ratios := make(map[string][]float64)
	for _, row := range report.Admission {
		realized, known := accuracyRealized(row.Settlement)
		forecast := row.Plan.Economics.ExpectedNetPence
		if row.Settlement.Kind != "passage" && known && forecast > 0 && row.Settlement.CompletedAt.Before(fitUntil) {
			ratios[row.Settlement.Kind] = append(ratios[row.Settlement.Kind], float64(realized)/float64(forecast))
		}
	}
	for kind := range admission {
		samples := ratios[kind]
		report.Calibration[kind] = NetCalibration{Samples: len(samples), Available: len(samples) > 0,
			Median: percentile(samples, 0.5), P25: percentile(samples, 0.25), P75: percentile(samples, 0.75)}
	}
	for _, row := range report.Admission {
		fit := report.Calibration[row.Settlement.Kind]
		if row.Settlement.CompletedAt.Before(fitUntil) || !fit.Available {
			continue
		}
		forecast := int64(math.Round(float64(row.Plan.Economics.ExpectedNetPence) * fit.Median))
		accuracyAccumulatorFor(holdout, row.Settlement.Kind).addForecast(row.Settlement, row.Plan, forecast, false)
	}
	report.ByKind, report.Latest, report.Holdout = finishAccuracy(admission), finishAccuracy(latest), finishAccuracy(holdout)
	admissionModel, err := compareAdmission(report.Admission, settlements, plans, from, fitUntil, to, regime)
	if err != nil {
		return nil, err
	}
	report.AdmissionModel = admissionModel
	for view, kinds := range arrivals {
		report.ArrivalErrors[view] = make(map[string]ArrivalAccuracy)
		for kind, signed := range kinds {
			value := ArrivalAccuracy{N: len(signed)}
			absolute := make([]float64, len(signed))
			for i, seconds := range signed {
				value.BiasSeconds += seconds
				absolute[i] = math.Abs(seconds)
				value.MAESeconds += absolute[i]
			}
			value.BiasSeconds /= float64(value.N)
			value.MAESeconds /= float64(value.N)
			value.MedianAESeconds, value.P90AESeconds = percentile(absolute, 0.5), percentile(absolute, 0.9)
			report.ArrivalErrors[view][kind] = value
		}
	}
	return report, nil
}

func writeAccuracyTable(text *strings.Builder, label string, values map[string]KindAccuracy) {
	fmt.Fprintln(text, label)
	fmt.Fprintln(text, "Kind              n Covered Tons Forecast(Cr) Realized(Cr) Error(Cr) Bias% MAE(Cr) MedAE(Cr) P90AE(Cr) Sign% False-loss% Revenue-miss(Cr) Cost/reprice-bridge(Cr) Selected-revenue-miss(Cr) Selected-cost-miss(Cr) Full-lot Mismatch")
	for _, kind := range sortedKeys(values) {
		row := values[kind]
		if !row.ForecastAvailable {
			fmt.Fprintf(text, "%-16s %4d %7d %4d unavailable %12.2f forecast_available=false; known=%d\n", kind, row.N, row.Covered, row.QuantityTons, float64(row.KnownRealizedPence)/100, row.Known)
			continue
		}
		fmt.Fprintf(text, "%-16s %4d %7d %4d %12.2f %12.2f %9.2f %6.1f %7.2f %9.2f %9.2f %5.1f %11.1f %16.2f %22.2f %25.2f %22.2f %8d %8d\n", kind,
			row.N, row.Covered, row.QuantityTons, float64(row.ForecastPence)/100, float64(row.RealizedPence)/100,
			float64(row.ErrorPence)/100, row.BiasPercent, row.MAE/100, row.MedianAE/100, row.P90AE/100,
			row.SignAccuracy*100, row.FalseLossRate*100, float64(row.RevenueMissPence)/100, float64(row.CostMissPence)/100,
			float64(row.SelectedRevenueMissPence)/100, float64(row.SelectedCostMissPence)/100, row.RevenueCovered, row.QuantityMismatch)
	}
}

func (r AccuracyReport) WriteTo(writer io.Writer) (int64, error) {
	var text strings.Builder
	fmt.Fprintln(&text, "Forecast accuracy: recorded outcomes, not a validation gate. Errors are realized minus forecast; money is Cr.")
	fmt.Fprintf(&text, "Window: fit [%s, %s), holdout [%s, %s) by completion time\n", r.From.Format(time.RFC3339Nano), r.FitUntil.Format(time.RFC3339Nano), r.FitUntil.Format(time.RFC3339Nano), r.To.Format(time.RFC3339Nano))
	writeAccuracyTable(&text, "Admission forecast (earliest capture no later than start + 1 second)", r.ByKind)
	writeAccuracyTable(&text, "Latest forecast (strictly before completion; not admission accuracy)", r.Latest)
	fmt.Fprintln(&text, "Net-ratio fit (realized net / positive admission forecast; no ratio clipping)")
	for _, kind := range sortedKeys(r.Calibration) {
		fit := r.Calibration[kind]
		if !fit.Available {
			fmt.Fprintf(&text, "%-16s n=0 unavailable\n", kind)
			continue
		}
		fmt.Fprintf(&text, "%-16s n=%d p25=%.6f median=%.6f p75=%.6f\n", kind, fit.Samples, fit.P25, fit.Median, fit.P75)
	}
	writeAccuracyTable(&text, "Calibrated admission holdout (fit median applied, rounded to pence; no fit means unavailable)", r.Holdout)
	fmt.Fprintf(&text, "Receipt-model holdout: fit_samples=%d in_flight_at_fit=%d excluded_full_lots=%d excluded_mixed_lots=%d; valid captured full-lot receipt cohort only, recorded lots and costs fixed\n",
		r.AdmissionModel.FitSamples, r.AdmissionModel.InFlightAtFit, r.AdmissionModel.ExcludedFullLots, r.AdmissionModel.ExcludedMixedLots)
	for _, kind := range sortedKeys(r.AdmissionModel.ByKind) {
		comparison := r.AdmissionModel.ByKind[kind]
		if comparison.Old.Covered == 0 {
			continue
		}
		fmt.Fprintf(&text, "%-16s n=%d old_MAE=%.2f old_stressed_MAE=%.2f median_MAE=%.2f stressed_MAE=%.2f old_error=%.2f old_stressed_error=%.2f median_error=%.2f stressed_error=%.2f admitted=%d rejected=%d rejected_losses=%d rejected_realized=%.2f Cr\n",
			kind, comparison.Old.Covered, comparison.Old.MAE/100, comparison.OldStressed.MAE/100, comparison.Calibrated.MAE/100, comparison.Stressed.MAE/100,
			float64(comparison.Old.ErrorPence)/100, float64(comparison.OldStressed.ErrorPence)/100, float64(comparison.Calibrated.ErrorPence)/100, float64(comparison.Stressed.ErrorPence)/100,
			comparison.Admitted.Covered, comparison.Rejected, comparison.RejectedLosses, float64(comparison.RejectedRealizedPence)/100)
	}
	fmt.Fprintln(&text, "Comparable totals exclude unknown accounting. Known realized totals include rows without forecasts. Full-lot splits are unavailable for calibrated net-only holdout. False loss means forecast nonnegative, realized negative.")
	fmt.Fprintln(&text, "Revenue miss uses durable planned revenue, falling back to selected plan revenue. Its cost/reprice bridge includes later repricing and haircuts. Selected-plan revenue and cost misses use only that plan; positive cost miss means lower actual costs.")
	for _, view := range sortedKeys(r.ArrivalErrors) {
		for _, kind := range sortedKeys(r.ArrivalErrors[view]) {
			row := r.ArrivalErrors[view][kind]
			fmt.Fprintf(&text, "Arrival %-9s %-16s n=%d bias=%.3fs MAE=%.3fs median=%.3fs p90=%.3fs (completion minus planned arrival; real seconds)\n", view, kind, row.N, row.BiasSeconds, row.MAESeconds, row.MedianAESeconds, row.P90AESeconds)
		}
	}
	coverage := r.Coverage
	fmt.Fprintf(&text, "Coverage: settlements=%d no_plan=%d no_admission=%d no_latest=%d admission_kind_mismatch=%d latest_kind_mismatch=%d unknown_costs=%d unknown_proceeds=%d unpriced_passage=%d coverage_only=%d\n", coverage.Settlements, coverage.NoPlan, coverage.NoAdmissionPlan, coverage.NoLatestPlan, coverage.AdmissionKindMismatch, coverage.LatestKindMismatch, coverage.UnknownCosts, coverage.UnknownProceeds, coverage.UnpricedPassage, coverage.CoverageOnly)
	fmt.Fprintln(&text, "Coverage counters overlap and must not be added; they are not mutually exclusive buckets.")
	for _, kind := range sortedKeys(coverage.ByKind) {
		fmt.Fprintf(&text, "Coverage %-16s %d\n", kind, coverage.ByKind[kind])
	}
	n, err := io.WriteString(writer, text.String())
	return int64(n), err
}
