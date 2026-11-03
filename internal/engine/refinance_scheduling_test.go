package engine

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// In-flight refinance advances early; only never-sent requests wait for ship
// recovery and delivery before their fresh first-send preflight at the tail.
func refinanceWorkFixture(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := loanRefinanceFixture(t)
	treasury := &store.Mission{AccountID: h.account.ID, ShipID: "__treasury", Phase: "settings", Generation: uuid.NewString()}
	treasuryData := missionData{CooldownUntil: time.Now().Add(time.Hour)}
	if err := runner.saveMission(h.ctx, treasury, &treasuryData); err != nil {
		t.Fatal(err)
	}
	return h, runner
}

func refinanceWorkShip(id, portID string, atSea bool) ShipState {
	var ship ShipState
	ship.Ship.ID = id
	ship.Ship.Status = "berthed"
	ship.Ship.CurrentPortID = portID
	if atSea {
		ship.Ship.Status = "sailing"
	}
	return ship
}

func TestWorkAdvancesInFlightRefinanceAheadOfShipLoop(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinancePublishQuote(t, h, runner)
	cargo := refinanceWorkShip("refi-hull", "origin", false)
	cargo.CargoUsedTons = 10
	runner.stateCache.Ships = append(runner.stateCache.Ships, cargo)
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("in-flight refinance did not advance before the ship loop: %t %v", changed, err)
	}
	_, data, err := runner.loadMission(h.ctx, loanRefinanceMission)
	if err != nil {
		t.Fatal(err)
	}
	if data.Action == nil || data.Action.Stage != "loan-accept" {
		t.Fatalf("in-flight refinance did not stage its approval: %+v", data.Action)
	}
	cargoMission, _, err := runner.loadMission(h.ctx, cargo.Ship.ID)
	if err != nil || cargoMission != nil {
		t.Fatalf("recovery preempted the in-flight approval: %+v %v", cargoMission, err)
	}
}

func TestRefinanceRequestWaitsForRecoveryThenSendsAtTail(t *testing.T) {
	h, runner := refinanceWorkFixture(t)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("cash-short mature loan did not stage a refinance request")
	}
	_, staged, err := runner.loadMission(h.ctx, loanRefinanceMission)
	if err != nil {
		t.Fatal(err)
	}
	if staged.Action == nil || staged.Action.Stage != "loan-request" {
		t.Fatalf("expected a staged loan request: %+v", staged.Action)
	}
	// A mission-less cargo ship makes the ship loop end in recovery before
	// the pass tail: the never-sent request must not go out this pass.
	cargo := refinanceWorkShip("refi-hull", "origin", false)
	cargo.CargoUsedTons = 10
	runner.stateCache.Ships = []ShipState{cargo}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("ship loop recovery did not run ahead of the staged request: %t %v", changed, err)
	}
	if sent, err := h.db.HasAction(h.ctx, h.account.ID, staged.Action.ID); err != nil || sent {
		t.Fatalf("never-sent request was sent before the recovery turn: %t %v", sent, err)
	}
	if recovery, _, err := runner.loadMission(h.ctx, cargo.Ship.ID); err != nil || recovery == nil {
		t.Fatalf("recovery mission was not persisted: %+v %v", recovery, err)
	}

	// The deferral survives a restart: the restored mission still waits while
	// another recovery turn runs.
	restarted := h.runner(primaryTestKey)
	restarted.meta, restarted.generation, restarted.stateCache = runner.meta, runner.generation, runner.stateCache
	second := refinanceWorkShip("refi-more", "origin", false)
	second.CargoUsedTons = 8
	restarted.stateCache.Ships = []ShipState{cargo, second}
	if changed, err := restarted.work(h.ctx); err != nil || !changed {
		t.Fatalf("restarted recovery did not keep the request deferred: %t %v", changed, err)
	}
	if sent, err := h.db.HasAction(h.ctx, h.account.ID, staged.Action.ID); err != nil || sent {
		t.Fatalf("restart sent the request before the recovery turn: %t %v", sent, err)
	}

	// A restart invalidates the old financial session. Retire that proven-unsent
	// intent, then stage its replacement under the restored runner's authority.
	staleMission, staleData := h.mission(loanRefinanceMission)
	if _, err := restarted.replanUnsentAction(h.ctx, staleMission, &staleData, neverSentFinancialEvent); err != nil {
		t.Fatal(err)
	}
	if !loanRefinanceDrive(t, h, restarted) {
		t.Fatal("restored runner did not restage its never-sent request")
	}
	_, staged = h.mission(loanRefinanceMission)

	// A quiet pass reaches the tail: the fresh first-send guard passes and the
	// staged request goes out.
	quiet := refinanceWorkShip("refi-hull", "origin", true)
	quietMission := &store.Mission{AccountID: h.account.ID, ShipID: quiet.Ship.ID, Phase: "sailing", Generation: uuid.NewString()}
	quietData := missionData{Plan: Plan{Kind: "freight", ShipID: quiet.Ship.ID, Destination: "elsewhere-port"}, CostsKnown: true}
	if err := restarted.saveMission(h.ctx, quietMission, &quietData); err != nil {
		t.Fatal(err)
	}
	restarted.stateCache.Ships = []ShipState{quiet}
	if _, err := restarted.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	sent, err := h.db.HasAction(h.ctx, h.account.ID, staged.Action.ID)
	if err != nil || !sent {
		t.Fatalf("quiet pass did not first-send the staged request at the tail: %t %v", sent, err)
	}
}

func TestRefinanceApprovalSendsBeforeShipRecovery(t *testing.T) {
	h, runner := loanRefinancePosted(t)
	loanRefinancePublishQuote(t, h, runner)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("open request did not stage its approval")
	}
	_, staged := h.mission(loanRefinanceMission)
	if staged.Action == nil || staged.Action.Stage != "loan-accept" {
		t.Fatalf("expected a staged approval: %+v", staged.Action)
	}
	cargo := refinanceWorkShip("refi-hull", "origin", false)
	cargo.CargoUsedTons = 10
	runner.stateCache.Ships = []ShipState{cargo}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("staged approval did not advance before recovery: %t %v", changed, err)
	}
	if sent, err := h.db.HasAction(h.ctx, h.account.ID, staged.Action.ID); err != nil || !sent {
		t.Fatalf("recovery prevented approval first send: %t %v", sent, err)
	}
	if got := h.api.mutationCount("/v1/loan/approve"); got != 1 {
		t.Fatalf("approvals=%d, want 1 despite the pending recovery turn", got)
	}
	if recovery, _, err := runner.loadMission(h.ctx, cargo.Ship.ID); err != nil || recovery != nil {
		t.Fatalf("ship recovery ran before the staged approval: %+v %v", recovery, err)
	}
}
