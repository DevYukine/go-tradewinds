package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func localIntentTradingShipRow(id, name string) map[string]any {
	row := sailingShipRow(id, name, recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["status"] = "berthed"
	row["ship"].(map[string]any)["cannons_count"] = 4
	row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	row["effective_hold_capacity_tons"] = 52
	row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	row["route_legs"], row["voyage"] = []any{}, nil
	return row
}

func requireLocalIntentCooldown(t *testing.T, h *recoveryHarness, owner, originalID string, runner *Runner) time.Time {
	t.Helper()
	_, data := h.mission(owner)
	if data.Action != nil || !time.Now().Before(data.CooldownUntil) {
		t.Fatalf("never-sent %s did not retain its scoped retry: %+v", owner, data)
	}
	if exists, err := h.db.HasAction(h.ctx, h.account.ID, originalID); err != nil || exists {
		t.Fatalf("never-sent %s crossed the journal boundary: exists=%t err=%v", owner, exists, err)
	}
	for _, hold := range runner.holding {
		if hold.ShipID == "" && strings.Contains(hold.Reason, "authoritative resynchronization") {
			t.Fatalf("local retirement emitted a fleet-wide barrier: %+v", hold)
		}
	}
	return data.CooldownUntil
}

func expireLocalIntentRetry(t *testing.T, h *recoveryHarness, owner string) {
	t.Helper()
	_, data := h.mission(owner)
	if !time.Now().Before(data.CooldownUntil) {
		t.Fatalf("%s did not persist a local retry deadline: %+v", owner, data)
	}
	if data.Action != nil {
		if exists, err := h.db.HasAction(h.ctx, h.account.ID, data.Action.ID); err != nil || exists {
			t.Fatalf("cannot advance a possibly sent %s action: exists=%t err=%v", owner, exists, err)
		}
	}
	h.expireCooldown(owner)
}

func TestNeverSentGrowthVetoLetsIncumbentBuyAndDepart(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("stage capacity build: %t %v", changed, err)
	}
	_, before := h.mission("__growth")
	if before.Action == nil || before.Action.Stage != "build" {
		t.Fatalf("fixture did not stage a build: %+v", before.Action)
	}
	h.api.setFleet([]map[string]any{localIntentTradingShipRow(recoveryShipBlastoise, "Blastoise")})
	// An idle incumbent can now reuse this build's lane. That first-send
	// precondition veto does not invalidate the incumbent's funded voyage.
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	until := requireLocalIntentCooldown(t, h, "__growth", before.Action.ID, runner)
	if mission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil || mission == nil {
		t.Fatalf("growth retirement consumed the incumbent's pass: mission=%+v err=%v", mission, err)
	}
	for range 16 {
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/v1/market/place-order", "/v1/fleet/depart"} {
		if h.api.mutationCount(path) == 0 {
			t.Fatalf("growth cooldown starved %s: holds=%v", path, runner.holding)
		}
	}
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	if got := requireLocalIntentCooldown(t, h, "__growth", before.Action.ID, restarted); !got.Equal(until) {
		t.Fatal("restart changed the growth retry deadline")
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("never-sent growth build was restaged during its cooldown")
	}
}

func TestNeverSentShipVetoLetsFollowingShipBuyAndDepart(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{
		localIntentTradingShipRow(recoveryShipBlastoise, "Blastoise"),
		localIntentTradingShipRow(recoveryShipCharizard, "Charizard"),
	})
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if changed, err := runner.startMaintenance(h.ctx, ship, "refit", "/v1/shipyard/modify-ship", map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": 0}); err != nil || !changed {
		t.Fatalf("stage stale refit: %t %v", changed, err)
	}
	_, before := h.mission(ship.Ship.ID)
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	until := requireLocalIntentCooldown(t, h, ship.Ship.ID, before.Action.ID, runner)
	if mission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipCharizard); err != nil || mission == nil {
		t.Fatalf("first ship veto starved the following ship's same pass: mission=%+v err=%v", mission, err)
	}
	for range 16 {
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	if h.api.mutationCount("/v1/market/place-order") == 0 || h.api.mutationCount("/v1/fleet/depart") != 1 {
		t.Fatalf("following ship did not trade independently: holds=%v", runner.holding)
	}
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	if got := requireLocalIntentCooldown(t, h, ship.Ship.ID, before.Action.ID, restarted); !got.Equal(until) {
		t.Fatal("restart reset the ship retry deadline")
	}
	if h.api.mutationCount("/v1/shipyard/modify-ship") != 0 {
		t.Fatal("locally vetoed refit reached the server")
	}
}

func TestCapacityDonorRemitUsesItsReservationAndKeepsCooldown(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityAcceptedFixture(t)
	proxy := installReconciliationProxy(t, h)
	proxy.setContractRows([]map[string]any{freightContractRowFor("freight-b", "active", false, recoveryShipCharizard)})
	h.api.mu.Lock()
	credit := stationaryShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, "berthed", 24, 3)
	credit["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	h.api.fleet = append(h.api.fleet, credit)
	h.api.mu.Unlock()
	putLocalCreditDelivery(t, h, recoveryShipCharizard)
	loanCapacityRefresh(t, h, runner)
	mission, data := h.mission("__growth")
	_, required, err := runner.capacityFinanceReady(h.ctx, &data)
	if err != nil {
		t.Fatal(err)
	}
	const shortfall = int64(20_000)
	gross, err := GrossRemittance(shortfall, 100)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", required["north_sea"]-shortfall)
	donorBalance := required["channel"] + gross
	h.api.setVaultBalance("south", donorBalance)
	h.api.setVaultBalance("baltic", data.Capacity.Financing.Decision.WorkingCapitalPence+100_000)
	data.Capacity.Financing.DonorByRegion = map[string]int64{"channel": gross}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage approved package funding: %t %v", changed, err)
	}
	funding, transfer := h.mission("__treasury")
	if transfer.Action == nil || transfer.Action.Stage != "remit" || transfer.FundingMission != mission.ShipID || transfer.FundingGeneration != mission.Generation {
		t.Fatalf("funding lost its exact beneficiary: %+v", transfer)
	}
	var request struct {
		Gross int64 `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(transfer.Action.Body, &request); err != nil || request.Gross != gross {
		t.Fatalf("unexpected donor amount: %+v %v", request, err)
	}
	if err := runner.newActionPreflight(h.ctx, funding, &transfer); err != nil {
		t.Fatalf("own donor reservation was double-counted: %v", err)
	}
	// A real donor cash change then invalidates only this exact transfer.
	input, err := runner.committedPlanningInputExcept(h.ctx, ShipState{}, "__growth")
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("south", input.ReserveByRegion["channel"]+gross-1)
	loanCapacityRefresh(t, h, runner)
	if err := runner.newActionPreflight(h.ctx, funding, &transfer); err == nil || !strings.Contains(err.Error(), "donor reserves") {
		t.Fatalf("fixture lacks a donor-cash veto: %v", err)
	}
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	until := requireLocalIntentCooldown(t, h, "__treasury", transfer.Action.ID, runner)
	// Restored funding makes the approved package eligible again. It must
	// not overwrite the cooled transfer with another generation every pass.
	h.api.setVaultBalance("south", donorBalance)
	for _, active := range []*Runner{runner, h.runner(primaryTestKey)} {
		for range 3 {
			loanCapacityRefresh(t, h, active)
			if _, err := active.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			current, currentData := h.mission("__treasury")
			if current.Generation != funding.Generation || currentData.Action != nil || !currentData.CooldownUntil.Equal(until) {
				t.Fatalf("approved funding replaced its cooled owner: %+v %+v", current, currentData)
			}
		}
	}
	if h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatal("vetoed donor transfer generated a remittance storm")
	}
	if h.api.mutationCount("/v1/contract/deliver-freight") != 1 {
		t.Fatal("cooled capacity funding prevented the other ship's credit delivery")
	}
}

func putLocalCreditDelivery(t *testing.T, h *recoveryHarness, shipID string) {
	t.Helper()
	deadline := time.Now().UTC().Add(6 * time.Hour)
	h.putMission(shipID, "local-credit-"+shipID, "arrival", missionData{
		Plan: Plan{Kind: "recovery", ShipID: shipID, Destination: recoveryFarPort},
		Freight: []freightObligation{{ContractID: "freight-b", Commodity: recoveryGrain, Quantity: 24,
			Destination: recoveryFarPort, Deadline: &deadline, RewardPence: 9_000, CollateralPence: 5_000}},
		Eligible: map[string]int64{},
	})
}

func TestNeverSentShipFenceInvalidationReplansOnNextPass(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"financial", "control", "restart"} {
		t.Run(fence, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{localIntentTradingShipRow(recoveryShipBlastoise, "Blastoise")})
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
			runner := h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			h.putMission(ship.Ship.ID, "fence-replan-"+fence, "buy", missionData{
				Plan: Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort,
					Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 10,
					BuyLimitPence: 100, SellLimitPence: 1_000, TargetStoresTons: 4},
				CostsKnown: true,
			})
			mission, data := h.mission(ship.Ship.ID)
			if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed || data.Action == nil {
				t.Fatalf("stage paid-capacity buy: changed=%t err=%v data=%+v", changed, err, data)
			}
			oldID := data.Action.ID
			switch fence {
			case "financial":
				runner.publishedFinancial.Add(1)
				runner.capturedFinancial = runner.publishedFinancial.Load()
			case "control":
				runner.publishedControl.Add(1)
				runner.capturedControl = runner.publishedControl.Load()
			case "restart":
				runner = h.runner(primaryTestKey)
				loanCapacityRefresh(t, h, runner)
				ship = harnessShip(t, runner, ship.Ship.ID)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("retire fenced buy: changed=%t err=%v", changed, err)
			}
			mission, data = h.mission(ship.Ship.ID)
			if data.Action != nil || time.Now().Before(data.CooldownUntil) || h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatalf("routine fence introduced a delay or sent stale work: %+v", data)
			}
			if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed ||
				data.Action == nil || data.Action.ID == oldID || !runner.stagedFundsValid(data.Action) || !runner.stagedControlValid(data.Action) {
				t.Fatalf("next pass did not restage immediately with fresh authority: changed=%t err=%v data=%+v", changed, err, data)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/market/place-order") != 1 || len(h.pending()) != 0 {
				t.Fatal("freshly replanned buy did not complete exactly once")
			}
			if exists, err := h.db.HasAction(h.ctx, h.account.ID, oldID); err != nil || exists {
				t.Fatalf("fenced identity reached the journal: exists=%t err=%v", exists, err)
			}
		})
	}
}
