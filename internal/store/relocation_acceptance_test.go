package store

// Permanent economic history must retain bounded-probe spending, every known
// commercial loss and unknown costs independently of the display window.
// Profitable deliveries never reset already spent exploration expense.

import (
	"context"
	"encoding/json"
	"fmt"
	"go.uber.org/zap"
	"net/http"
	"testing"
	"time"
)

func relocationGuardSeed(t *testing.T, db *Store, ctx context.Context, accountID, id, kind string, completedAt time.Time, quantity, proceeds, acquisition, expenses int64, costsKnown bool) {
	t.Helper()
	actionID := id + "-action"
	body := json.RawMessage(fmt.Sprintf(`{"ship_id":"guard-ship","dest_port_id":"portb","seed":%q}`, id))
	if _, err := db.BeginAction(ctx, &Action{
		ID:        actionID,
		AccountID: accountID,
		Method:    http.MethodPost,
		Path:      "/v1/fleet/depart",
		Body:      body,
	}); err != nil {
		t.Fatalf("seed action %s: %v", actionID, err)
	}
	if err := db.CompleteAction(ctx, actionID, http.StatusOK, json.RawMessage(`{"ship_id":"guard-ship","dest_port_id":"portb","berth_dues_pence":0,"berth_dues_debt_pence":0}`), nil); err != nil {
		t.Fatalf("complete action %s: %v", actionID, err)
	}
	settlement := &Settlement{
		ID:               id,
		AccountID:        accountID,
		ShipID:           "guard-ship",
		MissionID:        id,
		Lane:             "laport|portb|grain|charter",
		ActionID:         actionID,
		Kind:             kind,
		Origin:           "laport",
		Destination:      "portb",
		Commodity:        "grain",
		QuantityTons:     quantity,
		GameSeconds:      43_200,
		CostsKnown:       costsKnown,
		AcquisitionPence: acquisition,
		ProceedsPence:    proceeds,
		ExpensesPence:    expenses,
		StartedAt:        completedAt.Add(-time.Hour),
		CompletedAt:      completedAt,
	}
	if err := db.PutSettlement(ctx, settlement); err != nil {
		t.Fatalf("seed settlement %s: %v", id, err)
	}
}

func TestRelocationPermanentHistoryRetainsLossesAndUncertaintyAcrossRestart(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "relocation-guard"
	saveTestAccount(t, db, accountID)
	base := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Microsecond)
	history, err := db.EconomicHistory(ctx, accountID, "")
	if err != nil || history != (EconomicHistory{}) {
		t.Fatalf("empty account reported economic evidence: %+v, %v", history, err)
	}
	relocationGuardSeed(t, db, ctx, accountID, "guard-earn-0", "charter", base, 48, 30_000, 4_800, 350, true)
	relocationGuardSeed(t, db, ctx, accountID, "guard-probe-1", "relocation", base.Add(time.Hour), 0, 0, 0, 18_500, true)
	history, err = db.EconomicHistory(ctx, accountID, "")
	if err != nil || history.NetPence != 6_350 || history.ProbeExpensePence != 18_500 || history.PositiveCycles != 1 {
		t.Fatalf("probe cash loss or permanent spending disappeared: %+v, %v", history, err)
	}
	relocationGuardSeed(t, db, ctx, accountID, "guard-earn-2", "charter", base.Add(2*time.Hour), 30, 40_000, 5_000, 400, true)
	relocationGuardSeed(t, db, ctx, accountID, "guard-aborted-3", "aborted", base.Add(3*time.Hour), 0, 0, 1_500, 0, true)
	relocationGuardSeed(t, db, ctx, accountID, "guard-unknown-4", "aborted", base.Add(4*time.Hour), 0, 0, 0, 0, false)
	history, err = db.EconomicHistory(ctx, accountID, "")
	if err != nil || history.NetPence != 39_450 || history.CommercialNetPence != 57_950 ||
		history.ProbeExpensePence != 18_500 || !history.UnknownCosts || history.UnknownProbeCosts ||
		history.PositiveCycles != 2 || history.GameSeconds != 5*43_200 {
		t.Fatalf("earnings reset probes or aborted loss/uncertainty vanished: %+v, %v", history, err)
	}
	const windowAccount = "relocation-guard-window"
	saveTestAccount(t, db, windowAccount)
	relocationGuardSeed(t, db, ctx, windowAccount, "window-probe-1", "relocation", base, 0, 0, 0, 18_500, true)
	for i := range 505 {
		relocationGuardSeed(t, db, ctx, windowAccount, fmt.Sprintf("window-earn-%03d", i), "charter", base.Add(time.Duration(i+1)*time.Minute), 10, 8_000, 1_000, 100, true)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	history, err = reopened.EconomicHistory(ctx, windowAccount, "")
	if err != nil || history.NetPence != 505*(8_000-1_000-100)-18_500 ||
		history.ProbeExpensePence != 18_500 || history.PositiveCycles != 505 ||
		history.UnknownCosts || history.LastProbeCompletion == nil || !history.LastProbeCompletion.Equal(base) {
		t.Fatalf("restart/windowing lost permanent account-scoped evidence: %+v, %v", history, err)
	}
}
