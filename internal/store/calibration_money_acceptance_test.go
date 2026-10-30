package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// This runtime interface keeps the tests compilable before the new store query
// exists. since is the event-pricing deployment boundary; asOf is the decision's clock.
type moneyCalibrationStore interface {
	RevenueCalibration(context.Context, string, time.Time, time.Time) (map[string]int64, error)
}

func moneyReadCalibration(t *testing.T, database *Store, accountID string, since, asOf time.Time) map[string]int64 {
	t.Helper()
	reader, ok := any(database).(moneyCalibrationStore)
	if !ok {
		t.Fatal("Store.RevenueCalibration(ctx, accountID, since, asOf) must expose the clean, bounded per-basis revenue window")
	}
	got, err := reader.RevenueCalibration(context.Background(), accountID, since, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func moneyCalibrationSettlement(t *testing.T, database *Store, accountID, id, basis string, completed time.Time, planned, realized int64, change func(*Settlement)) {
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
}

func TestMoneyCalibrationMedianClampAndMinimumSample(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ratios []int64
		want   int64
	}{
		{"nine is unmeasured", []int64{5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000, 5000}, 10000},
		{"ten is measured", []int64{9500, 9500, 9500, 9500, 9500, 9500, 9500, 9500, 9500, 9500}, 9500},
		{"median not average", []int64{2000, 2000, 2000, 2000, 2000, 2000, 2000, 2000, 2000, 9400, 9600, 20000, 20000, 20000, 20000, 20000, 20000, 20000, 20000, 20000}, 9500},
		{"lower clamp after median", []int64{5000, 5000, 5000, 5000, 5000, 12000, 12000, 12000, 12000, 12000}, 9000},
		{"upper clamp", []int64{10100, 10100, 10100, 10100, 10100, 10100, 10100, 10100, 10100, 10100}, 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, _, _ := integrationStore(t)
			const accountID = "calibration-median"
			saveTestAccount(t, database, accountID)
			since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
			for i, ratio := range tc.ratios {
				moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("median-%02d", i), "depth", since.Add(time.Duration(i+1)*time.Minute), 10_000, ratio, nil)
			}
			if got := moneyReadCalibration(t, database, accountID, since, since.Add(time.Hour))["depth"]; got != tc.want {
				t.Fatalf("revenue median calibration = %d bps, want %d for %v", got, tc.want, tc.ratios)
			}
		})
	}
}

func TestMoneyCalibrationLatestTwentyCleanSettlementsSinceDeploy(t *testing.T) {
	database, _, _ := integrationStore(t)
	const accountID = "calibration-clean-window"
	saveTestAccount(t, database, accountID)
	saveTestAccount(t, database, "calibration-other")
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	asOf := since.Add(2 * time.Hour)
	// Earlier valid observations must not dilute the most recent twenty.
	for i := range 25 {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("old-valid-%02d", i), "depth", since.Add(time.Duration(i+1)*time.Second), 10_000, 1000, nil)
	}
	for i := range 20 {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("latest-valid-%02d", i), "depth", since.Add(time.Duration(i+1)*time.Minute), 10_000, 9600, nil)
	}
	// More than twenty recent dirty rows prove filtering precedes LIMIT 20.
	for i := range 30 {
		change := []func(*Settlement){
			func(row *Settlement) { row.CostsKnown = false },
			func(row *Settlement) { row.PlannedRevenuePence = nil },
			func(row *Settlement) { row.PlannedRevenuePence = new(int64(0)) },
			func(row *Settlement) { row.RevenueBasis = nil },
			func(row *Settlement) { row.Kind = "aborted" },
			func(row *Settlement) { row.CargoShortfallPence = new(int64(1)) },
		}[i%6]
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("dirty-%02d", i), "depth", since.Add(time.Hour+time.Duration(i)*time.Second), 10_000, 1000, change)
	}
	for i := range 20 {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("pre-deploy-%02d", i), "depth", since.Add(-time.Duration(i+1)*time.Second), 10_000, 1000, nil)
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("future-%02d", i), "depth", asOf.Add(time.Duration(i+1)*time.Second), 10_000, 1000, nil)
		moneyCalibrationSettlement(t, database, "calibration-other", fmt.Sprintf("other-account-%02d", i), "depth", since.Add(time.Hour+time.Duration(i)*time.Second), 10_000, 1000, nil)
	}
	if got := moneyReadCalibration(t, database, accountID, since, asOf)["depth"]; got != 9600 {
		t.Fatalf("calibration included stale regime, dirty, future, other-account or older-than-20 evidence: got %d bps, want 9600", got)
	}
	// Moving the cutover beyond all clean observations must reset calibration,
	// not keep a previously cached low factor from before deployment.
	reset := moneyReadCalibration(t, database, accountID, since.Add(30*time.Minute), asOf)
	if value, present := reset["depth"]; present && value != 10000 {
		t.Fatalf("post-deploy window with no clean settlements retained old calibration: %+v", reset)
	}
}

func TestMoneyCalibrationUsesPerBasisRatiosNotPooledNetOrWeightedRevenue(t *testing.T) {
	database, _, _ := integrationStore(t)
	const accountID = "calibration-per-basis"
	saveTestAccount(t, database, accountID)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	for i := range 10 {
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("depth-ratio-%02d", i), "depth", since.Add(time.Duration(i+1)*time.Minute), 10_000, 9700, func(row *Settlement) {
			row.AcquisitionPence = 9600
			row.PlannedNetPence = new(int64(400))
		})
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("retained-ratio-%02d", i), "retained", since.Add(time.Duration(i+20)*time.Minute), 1_000_000, 920_000, nil)
		moneyCalibrationSettlement(t, database, accountID, fmt.Sprintf("contract-ratio-%02d", i), "contract", since.Add(time.Duration(i+40)*time.Minute), 100_000, 101_000, nil)
	}
	got := moneyReadCalibration(t, database, accountID, since, since.Add(2*time.Hour))
	for basis, want := range map[string]int64{"depth": 9700, "retained": 9200, "contract": 10000} {
		if got[basis] != want {
			t.Errorf("%s factor = %d, want %d; each basis needs its own last-20 median realized/planned revenue, not pooled receipts or realized/planned net", basis, got[basis], want)
		}
	}
}
