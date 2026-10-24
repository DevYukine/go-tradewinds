package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

func putEconomicEvidence(t *testing.T, db *Store, accountID, id, kind, lane string, quantity, proceeds, acquisition int64, known bool, expense *int64) Settlement {
	t.Helper()
	ctx := context.Background()
	if _, err := db.BeginAction(ctx, &Action{
		ID: id + "-action", AccountID: accountID, Method: http.MethodPost,
		Path: "/v1/fleet/depart", Body: json.RawMessage(`{"ship_id":"economic-ship","dest_port_id":"portb"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteAction(ctx, id+"-action", http.StatusOK, json.RawMessage(`{"ship_id":"economic-ship"}`), nil); err != nil {
		t.Fatal(err)
	}
	row := Settlement{
		ID: id, AccountID: accountID, ShipID: "economic-ship", MissionID: id,
		ActionID: id + "-action", Kind: kind, Lane: lane, QuantityTons: quantity,
		GameSeconds: 86_400, CostsKnown: known, ProceedsPence: proceeds,
		AcquisitionPence: acquisition, ExplorationExpensePence: expense,
		CompletedAt: time.Now().UTC(),
	}
	if err := db.PutSettlement(ctx, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestEconomicHistoryKeepsLossesAndSeparateProbeExpense(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "economic-history"
	saveTestAccount(t, db, accountID)
	const lane = "origin|destination|grain|spot"
	putEconomicEvidence(t, db, accountID, "economic-winner-1", "spot", lane, 20, 1_100, 100, true, nil)
	putEconomicEvidence(t, db, accountID, "economic-winner-2", "spot", lane, 20, 1_100, 100, true, nil)
	putEconomicEvidence(t, db, accountID, "economic-failed-buy", "spot", lane, 0, 0, 500, true, nil)
	measuredExpense := int64(25)
	row := putEconomicEvidence(t, db, accountID, "economic-probe", "relocation", "fingerprint", 0, 0, 1_000, true, &measuredExpense)
	putEconomicEvidence(t, db, accountID, "economic-legacy-probe", "relocation", "legacy-fingerprint", 0, 0, 50, true, nil)
	putEconomicEvidence(t, db, accountID, "economic-maintenance", "maintenance", "", 0, 0, 100, false, nil)

	if err := db.PutSettlement(ctx, &row); err != nil {
		t.Fatalf("same optional expense was not idempotent: %v", err)
	}
	changed := row
	zero := int64(0)
	changed.ExplorationExpensePence = &zero
	if err := db.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("changed optional expense rewrote settlement: %v", err)
	}
	changed.ExplorationExpensePence = nil
	if err := db.PutSettlement(ctx, &changed); !errors.Is(err, ErrActionConflict) {
		t.Fatalf("nil optional expense erased measured expense: %v", err)
	}
	negative := int64(-1)
	changed.ExplorationExpensePence = &negative
	if err := db.PutSettlement(ctx, &changed); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("negative exploration expense passed validation: %v", err)
	}

	history, err := db.EconomicHistory(ctx, accountID, "")
	if err != nil {
		t.Fatal(err)
	}
	if history.CommercialNetPence != 1_500 || history.NetPence != 450 || history.ProbeExpensePence != 75 ||
		history.PositiveCycles != 2 || history.PositiveQuantityTons != 40 || history.PositiveNetPence != 2_000 ||
		history.GameSeconds != 5*86_400 || history.PositiveGameSeconds != 2*86_400 || history.UnknownCosts || history.UnknownProbeCosts ||
		history.LastCompletion == nil || history.LastProbeCompletion == nil {
		t.Fatalf("loss-inclusive cash and consumed probe expense were conflated: %+v", history)
	}
	laneHistory, err := db.EconomicHistory(ctx, accountID, lane)
	if err != nil || laneHistory.NetPence != 1_500 || laneHistory.ProbeExpensePence != 0 {
		t.Fatalf("exact lane aggregate leaked other evidence: %+v, %v", laneHistory, err)
	}
	probes, err := db.RelocationProbes(ctx, accountID, []string{"fingerprint", "fingerprint", "missing", "legacy-fingerprint"})
	if err != nil || len(probes) != 2 || probes[0] != "fingerprint" || probes[1] != "legacy-fingerprint" {
		t.Fatalf("bounded permanent fingerprint lookup failed: %v, %v", probes, err)
	}
	putEconomicEvidence(t, db, accountID, "economic-unknown-probe", "relocation", "unknown-fingerprint", 0, 0, 0, false, nil)
	history, err = db.EconomicHistory(ctx, accountID, "")
	if err != nil || !history.UnknownCosts || !history.UnknownProbeCosts || history.ProbeExpensePence != 75 {
		t.Fatalf("unknown probe costs became a free retry: %+v, %v", history, err)
	}
}

func TestEconomicHistoryUsesExactOverflowCheckedPence(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "economic-overflow"
	saveTestAccount(t, db, accountID)
	const exact = int64(9_007_199_254_740_993)
	putEconomicEvidence(t, db, accountID, "economic-exact", "spot", "exact-lane", 1, exact, 0, true, nil)
	history, err := db.EconomicHistory(ctx, accountID, "exact-lane")
	if err != nil || history.NetPence != exact || history.CommercialNetPence != exact {
		t.Fatalf("permanent aggregate rounded integer pence: %+v, %v", history, err)
	}
	putEconomicEvidence(t, db, accountID, "economic-max-1", "spot", "overflow-lane", 1, math.MaxInt64, 0, true, nil)
	putEconomicEvidence(t, db, accountID, "economic-max-2", "spot", "overflow-lane", 1, math.MaxInt64, 0, true, nil)
	if _, err := db.EconomicHistory(ctx, accountID, "overflow-lane"); err == nil {
		t.Fatal("overflowing permanent profits became valid growth evidence")
	}
	maxExpense := int64(math.MaxInt64)
	putEconomicEvidence(t, db, accountID, "economic-probe-max-1", "relocation", "probe-overflow", 0, 0, 0, true, &maxExpense)
	putEconomicEvidence(t, db, accountID, "economic-probe-max-2", "relocation", "probe-overflow", 0, 0, 0, true, &maxExpense)
	if _, err := db.EconomicHistory(ctx, accountID, "probe-overflow"); err == nil {
		t.Fatal("overflowing cumulative probe expense wrapped into an available budget")
	}
}
