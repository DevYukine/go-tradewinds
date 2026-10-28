package engine

import (
	"bytes"
	"testing"
	"time"
)

func TestMortgageCashIntentCannotSwitchToDebtAtPreflight(t *testing.T) {
	t.Parallel()
	h, runner, _ := newMortgageFixture(t)
	h.api.setVaultBalance("north", 1_000_000_000)
	loanCapacityRefresh(t, h, runner)
	port, hull, cash, _, evidence := loanCapacityDecision(t, h, runner)
	if !cash.Accepted || cash.Mortgage != nil {
		t.Fatalf("fixture lacks a fully funded cash admission: %+v", cash)
	}
	if changed, err := runner.startCapacityGrowth(h.ctx, port, hull, cash, nil); err != nil || !changed {
		t.Fatalf("stage cash build: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if data.Capacity.Mortgage != nil || !bytes.Contains(data.Action.Body, []byte(`"mortgage":false`)) {
		t.Fatalf("cash intent was not immutable: %+v", data)
	}
	h.api.setVaultBalance("north", evidence.required[port.BankingRegion]+growthCapital(cash)+cash.WorkingCapitalPence-20_000)
	h.api.setVaultBalance("south", evidence.required["channel"])
	loanCapacityRefresh(t, h, runner)
	_, _, newlyFinanced, _, _ := loanCapacityDecision(t, h, runner)
	if !newlyFinanced.Accepted || newlyFinanced.Mortgage == nil {
		t.Fatalf("fixture does not distinguish fresh debt admission from stored cash intent: %+v", newlyFinanced)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("cash first-send used mortgage down-payment economics without a mortgage transport flag")
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("cash shortfall did not safely replan the never-sent build: %t %v", changed, err)
	}
	_, data = h.mission("__growth")
	if data.Capacity.Mortgage != nil || data.Action != nil || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatalf("cash preflight silently borrowed or spent protected working cash: %+v", data)
	}
}

func TestMortgagePrefersCheaperRemittanceOfExistingCash(t *testing.T) {
	t.Parallel()
	h, runner, _ := newMortgageFixture(t)
	_, _, initial, _, baseline := loanCapacityDecision(t, h, runner)
	h.api.setVaultBalance("north", baseline.required["north_sea"]+growthCapital(initial)-20_000)
	h.api.setVaultBalance("south", baseline.required["channel"]+1_000_000)
	loanCapacityRefresh(t, h, runner)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if !decision.Accepted || decision.Mortgage != nil || decision.FundingFeesPence <= 0 || decision.FundingFeesPence >= mortgageTestTerms().TotalRepayablePence-mortgageTestTerms().PrincipalPence {
		t.Fatalf("remote surplus with cheaper fees was replaced by term mortgage interest: %+v", decision)
	}
	if evidence.balances[port.BankingRegion] >= required[port.BankingRegion] {
		t.Fatal("fixture does not require a remittance to the yard")
	}
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID); err != nil || !changed {
		t.Fatalf("stage cheaper cash remittance: %t %v", changed, err)
	}
	h.missingMission("__growth")
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" {
		t.Fatalf("capacity did not remit its already-owned remote cash before considering debt: %+v", data)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("reserve-safe cash remittance could not be sent: %t %v", changed, err)
	}
	if h.api.mutationCount("/v1/treasury/remit") != 1 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("remote cash admission skipped remittance or built using debt")
	}
}

func TestMortgageAvailableMeasuredNetCapsCheapDebtButMissingNetDoesNotWait(t *testing.T) {
	t.Parallel()
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmeasured reinvestment", true: "measured zero cannot service debt"}[available], func(t *testing.T) {
			h, runner, _ := newMortgageFixture(t)
			_, _, _, _, evidence := loanCapacityDecision(t, h, runner)
			input, reason, err := runner.capacityFleetInput(h.ctx, evidence)
			if err != nil || reason != "" {
				t.Fatalf("fleet input: %s %v", reason, err)
			}
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
			grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
			input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
			input.MeasuredNetAvailable, input.MeasuredRatePencePerGameDay = available, 0
			quote, err := runner.growthQuote(port, hull)
			if err != nil {
				t.Fatal(err)
			}
			decision, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if available {
				if decision.Accepted || decision.Mortgage != nil {
					t.Fatalf("known zero net used predicted earnings to introduce mortgage debt: %+v", decision)
				}
			} else if !decision.Accepted || decision.Mortgage == nil || mortgageReserve(decision.Mortgage) != 4_600 || required[port.BankingRegion] != evidence.required[port.BankingRegion]+growthCapital(decision)+4_600 {
				t.Fatalf("missing measured net hoarded cash or lost two installments: %+v %v", decision, required)
			}
		})
	}
}

func TestMortgageConflictBackoffSurvivesEmptyLoanRefreshAndRestart(t *testing.T) {
	t.Parallel()
	h, runner, fixture := newMortgageFixture(t)
	fixture.refuse = true
	_, _, _, _, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("stage mortgage: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume mortgage conflict: %t %v", changed, err)
	}
	fixture.loans = nil
	for range 2 {
		runner = h.runner(primaryTestKey)
		loanCapacityRefresh(t, h, runner)
		if runner.mortgageAvailable() {
			t.Fatal("fresh empty loan list immediately cleared the persisted mortgage conflict backoff")
		}
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("mortgage conflict retried during its cooldown")
	}
	runner.snapshot["_mortgage_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	if !runner.mortgageAvailable() {
		t.Fatal("fresh empty loan authority did not reopen mortgage admission after backoff expired")
	}
}
