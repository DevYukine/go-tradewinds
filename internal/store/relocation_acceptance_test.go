package store

// Acceptance contract for the durable relocation restart guard (plan E13):
// the first bounded sourcing probe is permitted, but after a settled
// relocation the engine must see, account-scoped and without any latest-500
// history window, the latest probe, the loss-inclusive net of every settlement
// completed afterwards, whether any later settlement has unknown costs, and
// whether a later known-cost positive delivery re-earned the allowance. The
// query below fails to compile until the implementation lands.

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestRelocationRestartGuardTracksLatestProbeAndLaterEarnings(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	const accountID = "relocation-guard"
	saveTestAccount(t, db, accountID)
	base := time.Now().UTC().Add(-6 * time.Hour)

	// No relocation has ever settled: the first bounded probe is permitted.
	guard, err := db.LatestRelocationRestart(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Latest != nil || guard.NetSincePence != 0 || guard.UnknownCostSince || guard.PositiveDeliverySince {
		t.Fatalf("empty account reported a relocation history: %+v", guard)
	}

	// A profitable delivered shipment precedes the probe: it must never
	// unlock a restart by itself.
	relocationGuardSeed(t, db, ctx, accountID, "guard-earn-0", "charter", base, 48, 30_000, 4_800, 350, true)
	relocationGuardSeed(t, db, ctx, accountID, "guard-probe-1", "relocation", base.Add(time.Hour), 0, 0, 0, 18_500, true)
	guard, err = db.LatestRelocationRestart(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Latest == nil || guard.Latest.Kind != "relocation" || guard.Latest.ID != "guard-probe-1" {
		t.Fatalf("latest relocation probe not identified: %+v", guard.Latest)
	}
	if guard.NetSincePence != 0 || guard.UnknownCostSince || guard.PositiveDeliverySince {
		t.Fatalf("a failed probe with no later settlement looked restartable: %+v", guard)
	}

	// A later known-cost positive delivery re-earns the exploration
	// allowance, including the probe loss in the interval net.
	relocationGuardSeed(t, db, ctx, accountID, "guard-earn-2", "charter", base.Add(2*time.Hour), 30, 40_000, 5_000, 400, true)
	guard, err = db.LatestRelocationRestart(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if guard.NetSincePence != 34_600 || !guard.PositiveDeliverySince || guard.UnknownCostSince {
		t.Fatalf("later earnings did not restore the allowance: %+v", guard)
	}

	// An unknown-cost record inside the funding interval blocks again.
	relocationGuardSeed(t, db, ctx, accountID, "guard-unknown-3", "spot", base.Add(3*time.Hour), 0, 0, 0, 0, false)
	guard, err = db.LatestRelocationRestart(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if !guard.UnknownCostSince {
		t.Fatalf("unknown-cost settlement did not block the restart: %+v", guard)
	}
	if guard.NetSincePence != 34_600 {
		t.Fatalf("unknown-cost settlement was valued instead of flagged: %+v", guard)
	}

	// The query is account-scoped and never windowed: a fresh account with
	// more than 500 later settlements still resolves its own probe.
	const windowAccount = "relocation-guard-window"
	saveTestAccount(t, db, windowAccount)
	relocationGuardSeed(t, db, ctx, windowAccount, "window-probe-1", "relocation", base, 0, 0, 0, 18_500, true)
	for i := range 505 {
		relocationGuardSeed(t, db, ctx, windowAccount, fmt.Sprintf("window-earn-%03d", i), "charter", base.Add(time.Duration(i+1)*time.Minute), 10, 8_000, 1_000, 100, true)
	}
	guard, err = db.LatestRelocationRestart(ctx, windowAccount)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Latest == nil || guard.Latest.ID != "window-probe-1" {
		t.Fatalf("windowed history lost the latest relocation: %+v", guard.Latest)
	}
	if guard.NetSincePence != 505*(8_000-1_000-100) || !guard.PositiveDeliverySince || guard.UnknownCostSince {
		t.Fatalf("post-probe accounting did not cover the full interval beyond the history window: %+v", guard)
	}
}
