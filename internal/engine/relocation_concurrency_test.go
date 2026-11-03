package engine

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestRealizedRelocationConcurrentTargetsAndShipOwnership(t *testing.T) {
	h, runner := realizedRelocationRunner(t)
	putProbeHistorySettlement(t, h, store.Settlement{ID: "second-measured-origin", Kind: "spot", ShipID: recoveryShipLapras,
		Origin: "flushing", Destination: "hamburg", Commodity: "copper_sheathing", QuantityTons: 30, GameSeconds: 21_600,
		ProceedsPence: 60_000, AcquisitionPence: 10_000, CompletedAt: time.Now().UTC().Add(-time.Hour)})
	runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
	runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
	first, firstData := scoutAcceptanceAdmit(t, h, runner)
	firstData.CooldownUntil = time.Now().UTC().Add(time.Hour)
	if err := runner.saveMission(h.ctx, first, &firstData); err != nil {
		t.Fatal(err)
	}
	// Exercise the scheduler, not just candidate generation: a scoped retry on
	// one relocation must leave another free hull eligible for another target.
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("active relocation blocked another idle hull: changed=%t error=%v holds=%+v", changed, err, runner.holding)
	}
	second, secondData := h.mission(recoveryShipLapras)
	if secondData.Relocation == nil || !isRealizedRelocation(secondData.Plan) || first.Destination == second.Destination {
		t.Fatalf("concurrent hulls did not retain distinct measured targets: first=%+v second=%+v", first, second)
	}
	retained, _ := h.mission(recoveryShipBlastoise)
	if retained.Generation != first.Generation {
		t.Fatal("second admission replaced the first hull's durable mission")
	}
	if changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise)); err != nil || changed || !strings.Contains(reason, "ship already has a durable mission") {
		t.Fatalf("one hull acquired a second relocation: changed=%t reason=%q error=%v", changed, reason, err)
	}
	if changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipCharizard)); err != nil || changed {
		t.Fatalf("a third hull reused an already-reserved source: changed=%t reason=%q error=%v", changed, reason, err)
	}
	if count := scoutAcceptanceMissionCount(t, h, runner); count != 2 {
		t.Fatalf("distinct measured origins created %d missions, want two", count)
	}
	modeled := secondData.Relocation.ModeledExpensePence
	secondData.Relocation.ModeledExpensePence = firstData.Relocation.ExpenseCapPence
	if err := runner.saveMission(h.ctx, second, &secondData); err != nil {
		t.Fatal(err)
	}
	if err := runner.relocationSpendingReady(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), &firstData, 0); err == nil || !strings.Contains(err.Error(), "rolling exploration budget") {
		t.Fatalf("first send reused risk already committed to another target: %v", err)
	}
	secondData.Relocation.ModeledExpensePence = modeled
	if err := runner.saveMission(h.ctx, second, &secondData); err != nil {
		t.Fatal(err)
	}
	scoutAcceptanceServeDeparture(t, h, runner)
	for index, id := range []string{recoveryShipBlastoise, recoveryShipLapras} {
		for range 10 {
			mission, data := h.mission(id)
			data.CooldownUntil = time.Time{}
			if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, id), mission, &data); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/fleet/depart") > index {
				break
			}
		}
	}
	calls := h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != 2 || calls[0].IdempotencyKey == "" || calls[1].IdempotencyKey == "" || calls[0].IdempotencyKey == calls[1].IdempotencyKey {
		t.Fatalf("independent relocation journals did not each depart exactly once: calls=%+v holds=%+v", calls, runner.holding)
	}
	targets := make(map[string]bool)
	for _, call := range calls {
		var request struct {
			Destination string `json:"dest_port_id"`
		}
		if err := json.Unmarshal(call.Body, &request); err != nil || request.Destination == "" || targets[request.Destination] {
			t.Fatalf("concurrent departure reused a target: body=%s error=%v", call.Body, err)
		}
		targets[request.Destination] = true
	}
}

func TestRelocationConcurrentCommitmentsProtectCashAndRisk(t *testing.T) {
	for _, kind := range []string{"realized", "scout"} {
		t.Run(kind, func(t *testing.T) {
			var h *recoveryHarness
			var runner *Runner
			if kind == "realized" {
				h, runner = realizedRelocationRunner(t)
			} else {
				h, runner = scoutAcceptanceRunner(t)
			}
			other := harnessShip(t, runner, recoveryShipCharizard)
			before, err := runner.sourcingInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			input, err := runner.sourcingInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			cash := growthWorkingCapital(data.Plan)
			if input.ReserveByRegion["north"] < before.ReserveByRegion["north"]+cash {
				t.Fatalf("other relocation's unpaid cash became free: cash=%d before=%+v after=%+v", cash, before.ReserveByRegion, input.ReserveByRegion)
			}
			if input.RelocationTargets[data.Plan.Destination] == "" {
				t.Fatal("other relocation's target was not reserved")
			}
			if kind == "realized" {
				if input.SourcingCommittedPence != data.Relocation.ModeledExpensePence || input.ScoutCommittedPence != 0 || sourcingAllowance(input) != min(sourcingCashCap(input), *input.SourcingAllowancePence-input.SourcingCommittedPence) {
					t.Fatalf("measured relocation did not retain rolling risk capital: %+v", input)
				}
			} else if input.ScoutCommittedPence != data.Relocation.ModeledExpensePence || input.SourcingCommittedPence != 0 || scoutAllowance(input) != min(sourcingFreeCash(input)/50, scoutDailySeedPence-input.ScoutCommittedPence) {
				t.Fatalf("scout did not retain shared daily risk capital: %+v", input)
			}
			priorLedger := runner.snapshot["ledger"]
			ownBill := data.Relocation.ModeledExpensePence + 100
			runner.snapshot["ledger"] = encode([]map[string]any{
				{"id": "fleet-upkeep", "debit_account_id": runner.stateCache.Vaults[0].AccountID, "credit_account_id": "crown",
					"amount_pence": 1_000_000, "reason": "daily_fleet_and_factor_sprawl_upkeep"},
				{"id": "another-ship-berth", "debit_account_id": runner.stateCache.Vaults[0].AccountID, "credit_account_id": "harbour",
					"amount_pence": 500_000, "reason": "berth_dues", "metadata": map[string]any{"ship_id": other.Ship.ID}},
				{"id": "own-berth", "debit_account_id": runner.stateCache.Vaults[0].AccountID, "credit_account_id": "harbour",
					"amount_pence": ownBill, "reason": "berth_dues", "metadata": map[string]any{"ship_id": data.Plan.ShipID}},
			})
			input, err = runner.sourcingInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if input.SourcingCommittedPence+input.ScoutCommittedPence != ownBill {
				t.Fatalf("in-flight probe multiplied company bills or another ship's dues: sourcing=%d scout=%d own=%d",
					input.SourcingCommittedPence, input.ScoutCommittedPence, ownBill)
			}
			runner.snapshot["ledger"] = priorLedger
			own, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil || own.SourcingCommittedPence != 0 || own.ScoutCommittedPence != 0 || own.RelocationTargets[data.Plan.Destination] != "" {
				t.Fatalf("mission blocked itself instead of only its peers: input=%+v error=%v", own, err)
			}
			input.Balances["north"] = before.ReserveByRegion["north"] + cash - 1
			var plans []Plan
			if kind == "realized" {
				plans = realizedRelocationPlans(input, nil)
			} else {
				plans = scoutCandidates(input, nil)
			}
			if len(acceptedRelocations(plans)) != 0 {
				t.Fatal("a second relocation spent the first relocation's protected cash")
			}
			// Real incurred expense cannot disappear behind an older, smaller
			// modeled quote when another hull asks for risk capital.
			data.FeesPence = data.Relocation.ModeledExpensePence + 100
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			input, err = runner.sourcingInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if input.SourcingCommittedPence+input.ScoutCommittedPence < data.FeesPence {
				t.Fatal("active incurred expense was not protected")
			}
			data.CostsKnown = false
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			input, err = runner.sourcingInput(h.ctx, other)
			if err != nil {
				t.Fatal(err)
			}
			if sourcingAllowance(input) != 0 || scoutAllowance(input) != 0 {
				t.Fatal("unknown active expense did not close both exploration budgets")
			}
		})
	}
	input := PlanningInput{Balances: map[string]int64{"north": 100_000_000}, ScoutCommittedPence: math.MaxInt64,
		SourcingProbeHistory: sourcingProbeHistory{ScoutExpensePence: scoutDailyBudgetPence + 1}}
	if scoutAllowance(input) != 0 {
		t.Fatal("exhausted daily budget plus unknown active risk overflowed into new capital")
	}
}

func TestRelocationConcurrentTargetCollisionStopsFirstSend(t *testing.T) {
	h, runner := realizedRelocationRunner(t)
	_, data := scoutAcceptanceAdmit(t, h, runner)
	other := harnessShip(t, runner, recoveryShipCharizard)
	plan := data.Plan
	plan.ShipID = other.Ship.ID
	mission, duplicate := runner.newVoyage(other, plan)
	mission.Phase = "sourcing_fund"
	intent := *data.Relocation
	duplicate.Relocation = &intent
	if err := runner.saveMission(h.ctx, mission, &duplicate); err != nil {
		t.Fatal(err)
	}
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if err := runner.relocationSpendingReady(h.ctx, ship, &data, 0); err == nil || !strings.Contains(err.Error(), "another relocation already targets") {
		t.Fatalf("same-target commitment did not veto new spending: %v", err)
	}
	if err := runner.relocationDepartureReady(h.ctx, ship, &data); err == nil || !strings.Contains(err.Error(), "another relocation already targets") {
		t.Fatalf("same-target commitment did not veto departure: %v", err)
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("target collision sent a departure")
	}
}

func TestSourcingConcurrentTargetExclusionPrecedesShortlist(t *testing.T) {
	in := sourcingInputFixture(t)
	for _, source := range []string{"b-source", "c-source", "d-available"} {
		addSourcingSource(t, &in, source, 33, 900)
	}
	in.RelocationTargets = map[string]string{
		"amsterdam": "another relocation already targets amsterdam",
		"b-source":  "another relocation already targets b-source",
		"c-source":  "another relocation already targets c-source",
	}
	accepted := acceptedRelocations(planSourcingRelocation(in))
	if len(accepted) != 1 || accepted[0].Destination != "d-available" {
		t.Fatalf("reserved targets consumed the charter-source shortlist: %+v", accepted)
	}
}
