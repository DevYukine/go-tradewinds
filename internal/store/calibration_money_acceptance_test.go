package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"
)

func moneyReadCalibration(t *testing.T, database *Store, accountID string, since, asOf time.Time) RevenueCalibrationModel {
	t.Helper()
	model, err := database.RevenueCalibration(context.Background(), accountID, since, time.Time{}, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

func moneyCalibrationSettlement(t *testing.T, database *Store, accountID, id, basis string, completed time.Time, planned, realized int64, change func(*Settlement)) Settlement {
	t.Helper()
	ctx := context.Background()
	actionID := id + "-action"
	if _, err := database.BeginAction(ctx, &Action{
		ID: actionID, AccountID: accountID, Method: http.MethodPost, Path: "/v1/market/place-order",
		Body: json.RawMessage(`{"side":"ask","commodity_id":"canvas_sails","quantity":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteAction(ctx, actionID, http.StatusOK, json.RawMessage(`{"filled_quantity":1}`), nil); err != nil {
		t.Fatal(err)
	}
	row := Settlement{
		ID: id, AccountID: accountID, ShipID: "calibration-hoy", MissionID: id + "-mission", ActionID: actionID,
		Kind: "spot", Origin: "amsterdam", Destination: "hamburg", Commodity: "canvas_sails", Lane: "amsterdam|hamburg|canvas_sails|spot",
		QuantityTons: 1, CostsKnown: true, GameSeconds: 86_400, AcquisitionPence: 100, ProceedsPence: realized,
		PlannedNetPence: new(planned - 100), PlannedRevenuePence: &planned, RevenueBasis: &basis,
		CargoShortfallPence: new(int64(0)), StartedAt: completed.Add(-time.Second), CompletedAt: completed,
	}
	if change != nil {
		change(&row)
	}
	if err := database.PutSettlement(ctx, &row); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	return row
}

func TestMoneyCalibrationCleanRegimeAndExclusiveAsOf(t *testing.T) {
	database, _, _ := integrationStore(t)
	const accountID = "calibration-clean-window"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "calibration-other")
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	asOf := since.Add(2 * time.Hour)
	for i := range 10 {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("valid-%02d", i), "depth", since.Add(time.Duration(i+1)*time.Minute), 10_000, 7600, nil)
	}
	for i, change := range []func(*Settlement){
		func(row *Settlement) { row.CostsKnown = false },
		func(row *Settlement) { row.PlannedRevenuePence = nil },
		func(row *Settlement) { row.RevenueBasis = nil },
		func(row *Settlement) { row.Kind = "aborted" },
		func(row *Settlement) { row.Kind = "spot+freight" },
		func(row *Settlement) { row.CargoShortfallPence = new(int64(1)) },
		func(row *Settlement) { row.StartedAt = since.Add(-time.Second) },
	} {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("dirty-%02d", i), "depth", since.Add(time.Hour), 10_000, 1000, change)
	}
	moneyCalibrationSettlement(t, database, accountID, "boundary", "depth", asOf, 10_000, 1000, nil)
	moneyCalibrationSettlement(t, database, "calibration-other", "other", "depth", since.Add(time.Hour), 10_000, 1000, nil)
	got := moneyReadCalibration(t, database, accountID, since, asOf).Factor("canvas_sails", "depth", 1)
	if got.MedianBPS != 7600 || got.AdverseBPS != 7600 || got.Samples != 10 {
		t.Fatalf("dirty, mixed, future or cross-regime receipts leaked into fit: %+v", got)
	}
	reset := moneyReadCalibration(t, database, accountID, since.Add(30*time.Minute), asOf).Factor("canvas_sails", "depth", 1)
	if reset.Samples != 0 || reset.MedianBPS != 10_000 {
		t.Fatalf("empty regime retained prior measured calibration: %+v", reset)
	}
}

func TestMoneyCalibrationUsesAdmissionQuoteAndForecastHorizonNotLaterReprice(t *testing.T) {
	database, _, _ := integrationStore(t)
	const accountID = "calibration-admission-quotes"
	saveTestAccount(t, database, accountID)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	var rows []Settlement
	for i := range 10 {
		id := fmt.Sprintf("admission-%02d", i)
		completed := since.Add(time.Duration(i+1) * time.Minute)
		rows = append(rows, moneyCalibrationSettlement(t, database, accountID, id, "retained", completed, 10_000, 7600, func(row *Settlement) {
			row.AdmissionRevenue = &RevenueForecast{Kind: "spot", Commodity: "canvas_sails", Basis: "depth",
				RevenuePence: 20_000, QuantityTons: 1, HorizonGameDays: 2}
		}))
		for _, capture := range []struct {
			at      time.Time
			revenue int64
		}{
			{completed.Add(-900 * time.Millisecond), 20_000},
			{completed.Add(-100 * time.Millisecond), 1_000},
			{completed.Add(time.Second), 100},
		} {
			plan := json.RawMessage(fmt.Sprintf(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":%d,"uncalibrated_revenue_pence":%d,"revenue_quantity_tons":1,"game_days":1,"revenue_horizon_game_days":2}}`, capture.revenue, capture.revenue))
			if err := database.RecordReplayPlan(context.Background(), ReplayPlan{
				AccountID: accountID, MissionID: id + "-mission", ObservedAt: capture.at, Plan: plan, Policy: json.RawMessage(`{}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	model := moneyReadCalibration(t, database, accountID, since, since.Add(time.Hour))
	got := model.Factor("canvas_sails", "depth", 2)
	if got.Samples != 10 || got.MedianBPS != 3800 || model.Cells["basis/depth/commodity/canvas_sails/1.5-2.5"].Samples != 10 ||
		model.Cells["basis/depth/commodity/canvas_sails/1-1.5"].Samples != 0 || model.AdmissionSamples != 10 {
		t.Fatalf("later receipt quote, realized duration or completed reprice contaminated the admission fit: %+v", got)
	}
	plans, err := database.ReplayPlans(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := FitRevenueCalibrationWindow(rows, plans, since, time.Time{}, since.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(model, shared) {
		t.Fatalf("online and shared admission calibration differ: online=%+v shared=%+v", model, shared)
	}
}

func TestMoneyCalibrationDoesNotReadReplayPlans(t *testing.T) {
	database, _, _ := integrationStore(t)
	const account = "calibration-without-replay"
	saveTestAccount(t, database, account)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	for i := range 10 {
		row := moneyCalibrationSettlement(t, database, account, fmt.Sprintf("no-replay-%d", i), "depth",
			since.Add(time.Duration(i+1)*time.Minute), 10_000, 7600, nil)
		if err := database.RecordReplayPlan(context.Background(), ReplayPlan{
			AccountID: account, MissionID: row.MissionID, ObservedAt: row.CompletedAt.Add(time.Second),
			Plan: json.RawMessage(`{"kind":"incompatible"}`), Policy: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	model := moneyReadCalibration(t, database, account, since, since.Add(time.Hour))
	if got := model.Factor("canvas_sails", "depth", 1); got.Samples != 10 || got.MedianBPS != 7600 ||
		model.DurableFallbackSamples != 10 || model.CapturedSamples != 0 {
		t.Fatalf("online calibration still depends on replay captures: %+v", model)
	}
}

func TestFitRevenueCalibrationWindowExcludesFirstIncompatibleCapture(t *testing.T) {
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	valid := json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":14000,"uncalibrated_revenue_pence":20000,"revenue_calibration_bps":7000,"revenue_quantity_tons":1,"game_days":2}}`)
	for _, tc := range []struct {
		name     string
		first    json.RawMessage
		sameTime bool
	}{
		{name: "kind", first: json.RawMessage(`{"kind":"charter","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "commodity", first: json.RawMessage(`{"kind":"spot","commodity":"raw_wool","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "quantity", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":2,"game_days":2}}`)},
		{name: "raw-quote-unrecoverable", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":14000,"revenue_calibration_bps":7000,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "raw-quote-zero", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":14000,"uncalibrated_revenue_pence":0,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "horizon", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":1,"game_days":0}}`)},
		{name: "basis", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_pence":20000,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "event-stress", first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","event_price_stress":true,"economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":1,"game_days":2}}`)},
		{name: "same-time-id-order", sameTime: true, first: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":2,"game_days":2}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := calibrationRows("canvas_sails", "depth", 1, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600)
			var plans []ReplayPlan
			for i := range rows {
				row := &rows[i]
				row.ID = fmt.Sprintf("receipt-%02d", i)
				row.MissionID = fmt.Sprintf("mission-%02d", i)
				row.StartedAt = since.Add(time.Duration(i+1) * time.Minute)
				row.CompletedAt = row.StartedAt.Add(time.Minute)
				first := ReplayPlan{ID: 20, MissionID: row.MissionID, ObservedAt: row.StartedAt, Plan: tc.first}
				later := ReplayPlan{ID: 10, MissionID: row.MissionID, ObservedAt: row.StartedAt.Add(500 * time.Millisecond), Plan: valid}
				if tc.sameTime {
					later.ObservedAt = first.ObservedAt
					first.ID, later.ID = 10, 20
				}
				plans = append(plans, later, first)
			}
			model, err := FitRevenueCalibrationWindow(rows, plans, since, time.Time{}, since.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if got := model.Factor("canvas_sails", "depth", 2); got.Samples != 0 || got.MedianBPS != 10_000 || got.AdverseBPS != 10_000 {
				t.Fatalf("first incompatible capture fell back to a later plan or durable quote: %+v", got)
			}
		})
	}
}

func TestFitRevenueCalibrationWindowRegimeAndRollingBoundaries(t *testing.T) {
	asOf := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{30 * 24 * time.Hour, 6 * time.Hour} {
		t.Run(age.String(), func(t *testing.T) {
			since := asOf.Add(-age)
			from := asOf.Add(-14 * 24 * time.Hour)
			if since.After(from) {
				from = since
			}
			rows := calibrationRows("canvas_sails", "depth", 1, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600)
			for i := range rows {
				rows[i].ID = fmt.Sprintf("valid-%02d", i)
				rows[i].StartedAt = since
				rows[i].CompletedAt = from
			}
			for _, change := range []func(*Settlement){
				func(row *Settlement) { row.CompletedAt = from.Add(-time.Nanosecond) },
				func(row *Settlement) { row.CompletedAt = asOf },
				func(row *Settlement) { row.CompletedAt = asOf.Add(time.Nanosecond) },
				func(row *Settlement) { row.StartedAt = since.Add(-time.Nanosecond) },
				func(row *Settlement) { row.CostsKnown = false },
				func(row *Settlement) { row.ProceedsUnknown = true },
				func(row *Settlement) { row.Kind = "spot+freight" },
				func(row *Settlement) { row.PlannedRevenuePence = nil },
				func(row *Settlement) { row.RevenueBasis = new(" ") },
				func(row *Settlement) { row.QuantityTons = 0 },
				func(row *Settlement) { row.CargoShortfallPence = new(int64(1)) },
			} {
				row := rows[0]
				row.ProceedsPence = 1000
				change(&row)
				rows = append(rows, row)
			}
			model, err := FitRevenueCalibrationWindow(rows, nil, since, time.Time{}, asOf)
			if err != nil {
				t.Fatal(err)
			}
			if model.Version != RevenueCalibrationVersion || !model.Since.Equal(since) || !model.FittedAt.Equal(asOf) {
				t.Fatalf("window metadata is not tied to the regime and evidence cutoff: %+v", model)
			}
			if got := model.Factor("canvas_sails", "depth", 1); got.Samples != 10 || got.MedianBPS != 7600 || got.AdverseBPS != 7600 {
				t.Fatalf("regime, rolling or completeness boundary leaked into fit: %+v", got)
			}
		})
	}
}

func TestFitRevenueCalibrationWindowCapsNewestEligibleRowsBeforeCaptureExclusions(t *testing.T) {
	asOf := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)
	since := asOf.Add(-30 * 24 * time.Hour)
	oldest := asOf.Add(-14 * 24 * time.Hour)
	var rows []Settlement
	for i := range 4095 {
		row := calibrationRows("canvas_sails", "depth", 1, 7600)[0]
		row.ID = fmt.Sprintf("new-%04d", i)
		row.MissionID = row.ID + "-mission"
		row.CompletedAt = oldest.Add(time.Hour + time.Duration(i)*time.Second)
		row.StartedAt = row.CompletedAt.Add(-time.Minute)
		rows = append(rows, row)
	}
	for _, commodity := range []string{"coal", "coffee_cocoa"} {
		row := rows[0]
		row.ID = "z-tied-oldest"
		if commodity == "coffee_cocoa" {
			row.ID = "a-tied-oldest"
		}
		row.MissionID = row.ID + "-mission"
		row.Commodity = commodity
		row.CompletedAt = oldest
		row.StartedAt = oldest.Add(-time.Minute)
		rows = append(rows, row)
	}
	for i, change := range []func(*Settlement){
		func(row *Settlement) { row.CostsKnown = false },
		func(row *Settlement) { row.ProceedsUnknown = true },
		func(row *Settlement) { row.Kind = "spot+freight" },
		func(row *Settlement) { row.PlannedRevenuePence = nil },
		func(row *Settlement) { row.RevenueBasis = new(" ") },
		func(row *Settlement) { row.QuantityTons = 0 },
		func(row *Settlement) { row.CargoShortfallPence = new(int64(1)) },
	} {
		row := rows[0]
		row.ID = fmt.Sprintf("dirty-%02d", i)
		row.CompletedAt = asOf.Add(-time.Minute)
		change(&row)
		rows = append(rows, row)
	}
	model, err := FitRevenueCalibrationWindow(rows, nil, since, time.Time{}, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Cells["global"].Samples; got != 4096 {
		t.Fatalf("newest eligible cohort contains %d receipts, want 4096", got)
	}
	if model.Cells["group/luxury"].Samples != 1 || model.Cells["group/bulk"].Samples != 0 {
		t.Fatalf("completion ties did not use ascending settlement ID: %+v", model.Cells)
	}
	late := ReplayPlan{MissionID: rows[0].MissionID, ObservedAt: rows[0].StartedAt.Add(2 * time.Second),
		Plan: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":10000,"revenue_quantity_tons":1,"game_days":1}}`)}
	model, err = FitRevenueCalibrationWindow(rows, []ReplayPlan{late}, since, time.Time{}, asOf)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Cells["global"].Samples; got != 4095 || model.Cells["group/bulk"].Samples != 0 {
		t.Fatalf("a late capture backfilled the bounded cohort: global=%d bulk=%+v", got, model.Cells["group/bulk"])
	}
}

func calibrationRows(commodity, basis string, days float64, ratios ...int64) []Settlement {
	rows := make([]Settlement, len(ratios))
	for i, ratio := range ratios {
		rows[i] = Settlement{Kind: "spot", Commodity: commodity, CostsKnown: true, QuantityTons: 1,
			GameSeconds: int64(days * 86_400), PlannedRevenuePence: new(int64(10_000)), ProceedsPence: ratio, RevenueBasis: new(basis)}
	}
	return rows
}

func TestRevenueCalibrationPoolsSparseCommodityAndHorizon(t *testing.T) {
	ratio := func(n int, bps int64) []int64 {
		values := make([]int64, n)
		for i := range values {
			values[i] = bps
		}
		return values
	}
	rows := calibrationRows("raw_wool", "depth", 1.2, ratio(10, 7000)...)
	rows = append(rows, calibrationRows("canvas_sails", "retained", 3, ratio(9, 9000)...)...)
	model := FitRevenueCalibration(rows)
	if got := model.Factor("canvas_sails", "depth", 1.2); got.Source != "global" || got.AdverseBPS != 7000 {
		t.Fatalf("new commodity did not pool within horizon: %+v", got)
	}
	if got := model.Factor("coarse_cloth", "depth", 3); got.Source != "global" || got.Samples != 19 {
		t.Fatalf("sparse horizon did not pool commodity group: %+v", got)
	}
	if got := model.Factor("new_commodity", "depth", 1.2); got.Source != "global" || got.MedianBPS != 7000 {
		t.Fatalf("new group did not use global evidence: %+v", got)
	}
	rows = append(rows, calibrationRows("canvas_sails", "depth", 1.2, ratio(30, 6000)...)...)
	if got := FitRevenueCalibration(rows).Factor("canvas_sails", "depth", 1.2); got.Source != "basis/depth/commodity/canvas_sails/1-1.5" || got.MedianBPS != 6000 {
		t.Fatalf("thirty same-commodity receipts did not replace pooled estimate: %+v", got)
	}
	if got := (RevenueCalibrationModel{}).Factor("coffee_cocoa", "depth", 1); got.Samples != 0 || got.AdverseBPS != 10_000 {
		t.Fatalf("empty evidence must not indefinitely block admission: %+v", got)
	}
	if got := model.Factor("canvas_sails", "contract", 1); got.MedianBPS != 10_000 {
		t.Fatalf("market misses discounted a fixed contractual receipt: %+v", got)
	}
}

func TestRevenueCalibrationMedianAndP65AreNotTenPercentClamped(t *testing.T) {
	rows := calibrationRows("coal", "depth", 1, 4000, 5000, 6000, 6500, 7000, 8000, 9000, 10_000, 11_000, 12_000)
	unknown := calibrationRows("coal", "depth", 1, 0)[0]
	unknown.ProceedsUnknown = true
	rows = append(rows, unknown)
	got := FitRevenueCalibration(rows).Factor("coal", "depth", 1)
	if got.Samples != 10 || got.MedianBPS != 7500 || got.AdverseBPS != 6500 {
		t.Fatalf("receipt quantiles were clamped or net/revenue-weighted: %+v", got)
	}
}

func TestRevenueHorizonBucketEdges(t *testing.T) {
	for _, tc := range []struct {
		days float64
		want string
	}{{0, "<1"}, {0.999999, "<1"}, {1, "1-1.5"}, {1.499999, "1-1.5"}, {1.5, "1.5-2.5"}, {2.499999, "1.5-2.5"}, {2.5, "2.5+"}, {math.NaN(), "unknown"}, {math.Inf(1), "unknown"}} {
		if got := RevenueHorizonBucket(tc.days); got != tc.want {
			t.Errorf("bucket(%v)=%s, want %s", tc.days, got, tc.want)
		}
	}
}

func TestFitRevenueCalibrationWindowKeepsCapturedForecastBucketAtEdge(t *testing.T) {
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	rows := calibrationRows("canvas_sails", "depth", 3, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600, 7600)
	var plans []ReplayPlan
	for i := range rows {
		row := &rows[i]
		row.ID = fmt.Sprintf("forecast-%02d", i)
		row.MissionID = row.ID + "-mission"
		row.StartedAt = since.Add(time.Duration(i+1) * time.Minute)
		row.CompletedAt = row.StartedAt.Add(time.Minute)
		plans = append(plans, ReplayPlan{
			MissionID: row.MissionID, ObservedAt: row.StartedAt,
			Plan: json.RawMessage(`{"kind":"spot","commodity":"canvas_sails","economics":{"revenue_basis":"depth","revenue_pence":20000,"revenue_quantity_tons":1,"game_days":3,"revenue_horizon_game_days":0.999999}}`),
		})
	}
	model, err := FitRevenueCalibrationWindow(rows, plans, since, time.Time{}, since.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Cells["group/textile/<1"]; got.Samples != 10 || got.MedianBPS != 3800 || model.Cells["group/textile/2.5+"].Samples != 0 {
		t.Fatalf("forecast horizon was replaced by realized duration or rounded into another bucket: %+v", got)
	}
}

func TestRevenueCalibrationChildrenNeedThirtyReceipts(t *testing.T) {
	key := "basis/retained/group/textile/1-1.5"
	model := RevenueCalibrationModel{Cells: map[string]RevenueCalibrationCell{
		"global": {Samples: 100, MedianBPS: 9900, AdverseBPS: 9700},
		key:      {Samples: 29, MedianBPS: 5000, AdverseBPS: 4000},
	}}
	if got := model.Factor("canvas_sails", "retained", 1); got.Source != "global" || got.MedianBPS != 9900 {
		t.Fatalf("a thin child overrode its measured parent: %+v", got)
	}
	cell := model.Cells[key]
	cell.Samples = 30
	model.Cells[key] = cell
	model.Cells["commodity/canvas_sails/1-1.5"] = RevenueCalibrationCell{Samples: 100, MedianBPS: 9800, AdverseBPS: 9600}
	if got := model.Factor("canvas_sails", "retained", 1); got.Source != key || got.MedianBPS != 5000 {
		t.Fatalf("pooled commodity evidence overrode a measured basis-specific group: %+v", got)
	}
}

func TestMoneyCalibrationRetainedCutoverKeepsOtherHistoricalReceipts(t *testing.T) {
	database, _, _ := integrationStore(t)
	const account = "calibration-retained-boundary"
	saveTestAccount(t, database, account)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	retainedSince := since.Add(time.Hour)
	for _, basis := range []string{"depth", "gossip", "contract", "retained"} {
		for i := range 10 {
			ratio := int64(7600)
			if basis == "retained" {
				ratio = 1000
			}
			moneyCalibrationSettlement(t, database, account, fmt.Sprintf("legacy-%s-%d", basis, i), basis,
				retainedSince.Add(time.Minute), 10_000, ratio, nil)
		}
	}
	// Legacy missions began before the retained-only boundary.
	retainedSince = retainedSince.Add(time.Minute)
	model, err := database.RevenueCalibration(context.Background(), account, since, retainedSince, since.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if model.Cells["global"].Samples != 20 || model.Cells["global"].MedianBPS != 7600 ||
		model.Cells["contract"].Samples != 10 || model.Cells["contract"].MedianBPS != 7600 ||
		model.Cells["basis/retained/commodity/canvas_sails/1-1.5"].Samples != 0 || model.ExcludedSamples != 10 {
		t.Fatalf("retained cutover reset or polluted unchanged receipt denominators: %+v", model)
	}
}

func TestMoneyCalibrationExcludesEventStressAdmissionReceipts(t *testing.T) {
	database, _, _ := integrationStore(t)
	const account = "calibration-event-stress"
	saveTestAccount(t, database, account)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	for i := range 30 {
		receipt := int64(7600)
		if i >= 10 {
			receipt = 1000
		}
		moneyCalibrationSettlement(t, database, account, fmt.Sprintf("shock-%d", i), "depth",
			since.Add(time.Duration(i+1)*time.Minute), 10_000, receipt, func(row *Settlement) {
				row.AdmissionRevenue = &RevenueForecast{Kind: row.Kind, Commodity: row.Commodity, Basis: "depth",
					QuantityTons: 1, RevenuePence: 10_000, HorizonGameDays: 1, EventStress: i >= 10}
			})
	}
	model := moneyReadCalibration(t, database, account, since, since.Add(time.Hour))
	if got := model.Factor("canvas_sails", "depth", 1); got.Samples != 10 || got.MedianBPS != 7600 || model.AdmissionSamples != 10 {
		t.Fatalf("temporary shock reversion polluted ordinary receipt fitting: %+v", model)
	}
}

func TestRevenueCalibrationRegimeReadsOnlyRecordedBoundaries(t *testing.T) {
	database, _, _ := integrationStore(t)
	const account = "calibration-regime-projection"
	saveTestAccount(t, database, account)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	retainedSince := since.Add(time.Hour)
	first, _ := json.Marshal(since)
	second, _ := json.Marshal(retainedSince)
	if err := database.SaveSnapshot(context.Background(), account, map[string]json.RawMessage{
		"_revenue_calibration_since": first, "_revenue_calibration_v2_since": second,
		"ledger": json.RawMessage(`[{"id":"not-calibration-evidence"}]`),
	}); err != nil {
		t.Fatal(err)
	}
	regime, err := database.RevenueCalibrationRegime(context.Background(), account)
	if err != nil || !regime.Since.Equal(since) || !regime.RetainedSince.Equal(retainedSince) || len(regime.Cells) != 0 {
		t.Fatalf("offline fit lost its recorded regime: %+v, %v", regime, err)
	}
}

func TestMoneyCalibrationRetainsUpsideBeforeClampingForecast(t *testing.T) {
	var rows []Settlement
	for i := range 10 {
		proceeds := int64(9500)
		if i >= 5 {
			proceeds = 10_500
		}
		rows = append(rows, Settlement{Kind: "spot", Commodity: "canvas_sails", QuantityTons: 1, CostsKnown: true,
			ProceedsPence: proceeds, AdmissionRevenue: &RevenueForecast{
				Kind: "spot", Commodity: "canvas_sails", QuantityTons: 1, Basis: "depth", RevenuePence: 10_000, HorizonGameDays: 1,
			}})
	}
	model := FitRevenueCalibration(rows)
	if got := model.Factor("canvas_sails", "depth", 1); got.MedianBPS != 10_000 || got.AdverseBPS != 9500 {
		t.Fatalf("balanced receipt misses biased the forecast downward: %+v", got)
	}
	for i := range rows {
		rows[i].ProceedsPence = 10_500
	}
	model = FitRevenueCalibration(rows)
	if model.Cells["global"].MedianBPS != 10_500 || model.Cells["global"].AdverseBPS != 10_500 {
		t.Fatalf("upside was capped before fitting percentiles: %+v", model.Cells["global"])
	}
	if got := model.Factor("canvas_sails", "depth", 1); got.MedianBPS != 10_000 || got.AdverseBPS != 10_000 {
		t.Fatalf("upside inflated the executable forecast: %+v", got)
	}
}

func TestMoneyCalibrationReceiptRatioBoundsWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		name     string
		forecast int64
		proceeds int64
		want     int64
	}{
		{"large equal values", math.MaxInt64, math.MaxInt64, 10_000},
		{"large above forecast", math.MaxInt64 / 2, math.MaxInt64, 20_000},
		{"division overflow saturates", 1, math.MaxInt64, 20_000},
		{"above cap", 10_000, 30_000, 20_000},
		{"below cap", 10_000, 19_999, 19_999},
		{"zero receipt", 10_000, 0, 0},
		{"negative receipt", 10_000, -1, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := Settlement{Kind: "spot", Commodity: "canvas_sails", QuantityTons: 1, CostsKnown: true, ProceedsPence: test.proceeds,
				AdmissionRevenue: &RevenueForecast{Kind: "spot", Commodity: "canvas_sails", QuantityTons: 1,
					Basis: "depth", RevenuePence: test.forecast, HorizonGameDays: 1}}
			model := FitRevenueCalibration([]Settlement{row})
			if got := model.Cells["global"]; got.Samples != 1 || got.MedianBPS != test.want || got.AdverseBPS != test.want {
				t.Fatalf("receipt ratio overflowed or was truncated: %+v, want %d", got, test.want)
			}
		})
	}
}
