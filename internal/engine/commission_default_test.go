package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCommissionDurableAcceptanceSurvivesDefaultAndBooksDepositLoss(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("admission: changed=%v err=%v", changed, err)
	}
	mission, data := h.mission(commissionMission)
	required := make(map[string]int64)
	runner.reserveMissionCash(required, mission, &data, ShipState{})
	if required["america"] != data.Commission.BuildLimitPence+data.Commission.Contract.CollateralPence {
		t.Fatalf("unaccepted commission did not protect build plus deposit: %+v", required)
	}
	action := data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	// Simulate the existing authoritative-active-row reconciliation committing,
	// then crashing before mission advancement. The listing defaults meanwhile.
	if err := h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]any{"kind": "authoritative_commission_accept", "contract_id": data.Commission.Contract.ID})); err != nil {
		t.Fatal(err)
	}
	rows, err := runner.commissionListings()
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Status, rows[0].CarrierCompanyID, rows[0].UpdatedAt = "defaulted", new(recoveryCompanyID), time.Now().UTC()
	runner.snapshot["contracts"] = encode(rows)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if !data.Commission.Accepted || data.Action != nil {
		t.Fatal("default hid already durable acceptance evidence")
	}
	required = make(map[string]int64)
	runner.reserveMissionCash(required, mission, &data, ShipState{})
	if required["america"] != data.Commission.BuildLimitPence {
		t.Fatalf("escrowed deposit was reserved a second time: %+v", required)
	}
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("default closeout: changed=%v err=%v", changed, err)
	}
	_, closed := h.mission(commissionMission)
	if closed.Settlement == nil || !closed.Settlement.CostsKnown || closed.Settlement.ExpensesPence != 2_600_000 || closed.Settlement.ProceedsPence != 0 || closed.Settlement.AcquisitionPence != 0 {
		t.Fatalf("defaulted deposit became profit or an invented hull purchase: %+v", closed.Settlement)
	}
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	history, err := h.db.GrowthHistory(h.ctx, h.account.ID, "", time.Now().UTC())
	if err != nil || history.UnknownCosts || history.NetPence != 0 || history.UnknownLossPence != 0 {
		t.Fatalf("known commission default poisoned commercial history: %+v %v", history, err)
	}
	if h.api.mutationCount("/v1/contract/accept-commission") != 0 || h.api.mutationCount("/v1/contract/deliver-commission") != 0 {
		t.Fatal("durable acceptance recovery sent a fresh commission action")
	}
}

func TestCommissionDropsUnsentIntentWhenListingVanishes(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	data.Action = nil
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["contracts"] = encode([]any{})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("vanished open order retained its unsent cash reservation: %v %v", changed, err)
	}
	if mission, _, err := runner.loadMission(h.ctx, commissionMission); err != nil || mission != nil {
		t.Fatalf("unsent commission was not retired: %+v %v", mission, err)
	}
}

func TestCommissionRefusalIsNotReadmittedAfterRestart(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	if changed, err := runner.consumeCommissionResult(h.ctx, mission, &data, 409, encode(map[string]any{"code": "admiralty_commission_limit", "detail": "linked company already has an order"})); !changed || err != nil {
		t.Fatalf("definitive accept refusal was not closed: %v %v", changed, err)
	}
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); changed || err != nil {
		t.Fatalf("same refused order was admitted again: %v %v", changed, err)
	}
}

func TestCommissionGrowthCashProtectsBuildAndIgnoresGrowthRefusal(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	runner.snapshot["_growth_build_backoffs"] = encode(map[string]time.Time{"boston|" + recoveryHull: time.Now().Add(time.Hour)})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("unrelated speculative-build refusal blocked a Navy cash delivery: %v %v", changed, err)
	}
	_, data := h.mission(commissionMission)
	baseline, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, evidence); reason != "" || err != nil {
		t.Fatalf("growth funding authority: %s %v", reason, err)
	}
	expected := baseline["america"] + data.Commission.BuildLimitPence + data.Commission.Contract.CollateralPence
	if evidence.required["america"] != expected {
		t.Fatalf("growth could spend the commission's cash: got %d want %d", evidence.required["america"], expected)
	}
}

func TestCommissionAmbiguousTerminalOrderClosesWithoutInventedProfit(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	action := data.Action
	persisted, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := runner.commissionListings()
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Status, rows[0].CarrierCompanyID, rows[0].UpdatedAt = "completed", new(recoveryCompanyID), data.StartedAt.Add(-time.Hour)
	runner.snapshot["contracts"] = encode(rows)
	persisted.CreatedAt = time.Now().UTC().Add(-store.ActionReplayWindow + 30*time.Second)
	if changed, err := runner.reconcileCommission(h.ctx, mission, &data, persisted); changed || err != nil || data.Action == nil {
		t.Fatalf("terminal listing closed before receipt replay expired: changed=%v err=%v", changed, err)
	}
	persisted.CreatedAt = time.Now().UTC().Add(-store.ActionReplayWindow)
	if changed, err := runner.reconcileCommission(h.ctx, mission, &data, persisted); !changed || err != nil {
		t.Fatalf("exact completed builder row remained fleet-blocking: %v %v", changed, err)
	}
	if mission.Phase != "commission_settle" || data.Action != nil || data.Settlement == nil || data.Settlement.CostsKnown || data.Settlement.ProceedsPence != 26_000_000 {
		t.Fatalf("terminal listing invented actual build cost or retained an unresolved action: %+v", data)
	}
	completedAt := data.Settlement.CompletedAt
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("terminal settlement restart did not insert its persisted P&L: %v %v", changed, err)
	}
	settlements, err := h.db.SettlementsByID(h.ctx, h.account.ID, []string{mission.Generation})
	if err != nil || len(settlements) != 1 || !settlements[0].CompletedAt.Truncate(time.Microsecond).Equal(completedAt.Truncate(time.Microsecond)) {
		t.Fatalf("terminal settlement changed its host economic boundary: %+v %v", settlements, err)
	}
}

func TestCommissionAcceptedOrderFundsLocalBuildWithoutHidingRemitFee(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	if _, err := runner.driveCommissions(h.ctx); err != nil {
		t.Fatal(err)
	}
	mission, data := h.mission(commissionMission)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("america", 0)
	h.api.setVaultBalance("north", 100_000_000)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 10})
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("accepted order forfeited deposit despite funded donor cash: %v %v", changed, err)
	}
	mission, data = h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "remit" || data.Commission == nil || !data.Commission.Accepted {
		t.Fatalf("commission lost its own journalled funding identity: %+v", data)
	}
	var request struct {
		Gross int64 `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(data.Action.Body, &request); err != nil {
		t.Fatal(err)
	}
	fee := request.Gross / 1_000
	if _, err := runner.consumeResult(h.ctx, mission, &data, 200, encode(map[string]int64{
		"gross_amount_pence": request.Gross, "fee_pence": fee, "net_credited_pence": request.Gross - fee,
	})); err != nil {
		t.Fatal(err)
	}
	if data.FeesPence != fee || fee <= 0 || data.Commission == nil || !data.Commission.Accepted {
		t.Fatal("commission remittance fee vanished from actual net")
	}
}
