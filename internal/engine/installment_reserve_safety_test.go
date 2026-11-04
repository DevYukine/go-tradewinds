package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestInstallmentDeferredCashDoesNotFundGunRefits(t *testing.T) {
	for _, delta := range []int64{-1, 0} {
		t.Run(map[int64]string{-1: "one penny short", 0: "exact full reserve"}[delta], func(t *testing.T) {
			h, runner := incomeFundingRunner(t)
			ship := runner.stateCache.Ships[0]
			ship.Ship.CannonsCount = 3
			runner.stateCache.Ships[0] = ship
			for index := range runner.stateCache.Catalog.Ports {
				if runner.stateCache.Catalog.Ports[index].ID == ship.Ship.CurrentPortID {
					runner.stateCache.Catalog.Ports[index].HasShipyard = true
				}
			}
			loan := snapshotTestLoan()
			port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
			loan.BankingRegion = port.BankingRegion
			due := runner.stateCache.Clock.RealUTC.Add(gameDuration(3, runner.stateCache.Clock.Calendar.TimeScale) + time.Hour)
			loan.NextPaymentDueAt = &due
			runner.stateCache.Loans = []gameapi.Loan{loan}
			runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			input, err := runner.committedPlanningInput(h.ctx, ship)
			if err != nil {
				t.Fatal(err)
			}
			if len(input.DeferredLoanReserves) != 1 {
				t.Fatalf("refit fixture did not defer its installment: %+v", input.DeferredLoanReserves)
			}
			for index := range runner.stateCache.Vaults {
				vault := &runner.stateCache.Vaults[index]
				if vault.AccountType == "vault" {
					vault.BalancePence = input.ReserveByRegion[vault.BankingRegion]
					if vault.BankingRegion == port.BankingRegion {
						vault.BalancePence += loanReserve(loan) + 15_000 + delta
					}
				}
			}
			changed, err := runner.maintain(h.ctx, ship, nil, nil)
			if err != nil || changed != (delta == 0) {
				t.Fatalf("refit admission delta=%d changed=%t err=%v", delta, changed, err)
			}
			if delta < 0 {
				h.missingMission(ship.Ship.ID)
				return
			}
			mission, data := h.mission(ship.Ship.ID)
			if data.Action == nil || data.Action.Stage != "refit" {
				t.Fatalf("full-reserve control did not stage its gun refit: %+v", data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("exact full-reserve gun refit failed first-send: %v", err)
			}
			for index := range runner.stateCache.Vaults {
				if runner.stateCache.Vaults[index].AccountType == "vault" && runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
					runner.stateCache.Vaults[index].BalancePence--
				}
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected regional reserves") {
				t.Fatalf("refit first-send consumed deferred installment cash: %v", err)
			}
		})
	}
}

func TestSourcingAllowanceKeepsFullDeferredInstallmentReserve(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	loan := snapshotTestLoan()
	loan.BankingRegion = "channel"
	in.DeferredLoanReserves = []loanReserveDeferral{{Region: "channel", FullReserve: loanReserve(loan), ReserveFrom: in.Now.Add(time.Hour)}}
	source, _ := findPort(in.Catalog, "amsterdam")
	for _, extra := range []int64{-1, 0, 49, 50} {
		in.Balances["channel"] = in.ReserveByRegion["channel"] + loanReserve(loan) + extra
		bounded := in
		bounded.ReserveByRegion = sourcingReserves(in, source)
		if got, want := sourcingAllowance(bounded), max(int64(0), extra)/50; got != want {
			t.Fatalf("sourcing used deferred loan capital: extra=%d allowance=%d want=%d", extra, got, want)
		}
		if bounded.ReserveByRegion["channel"] != in.ReserveByRegion["channel"]+loanReserve(loan) {
			t.Fatalf("sourcing failed to restore the whole loan buffer: %v", bounded.ReserveByRegion)
		}
	}
}

func TestIncomeFundingProtectsEachDeferredLoanWindow(t *testing.T) {
	t.Parallel()
	in := incomeFundingInput(t, "charter")
	plan := incomeFundingBest(t, in)
	early := in.Now.Add(2 * time.Hour)
	later := early.Add(time.Hour)
	in.DeferredLoanReserves = []loanReserveDeferral{
		{Region: "donor", FullReserve: 48_402, ReserveFrom: early},
		{Region: "donor", FullReserve: 48_402, ReserveFrom: later},
	}
	for _, test := range []struct {
		arrival time.Time
		extra   int64
	}{
		{early.Add(-time.Minute), 0},
		{early, 48_402},
		{later, 96_804},
	} {
		plan.ExpectedArrival = test.arrival
		required := cargoFundingRequired(in, plan)
		if got, want := required["donor"], in.ReserveByRegion["donor"]+test.extra; got != want {
			t.Fatalf("arrival %s did not restore each due-date reserve independently: got=%d want=%d", test.arrival, got, want)
		}
	}
}

func TestInstallmentGrowthTreasuryPreflightKeepsDeferredReserves(t *testing.T) {
	h, runner := incomeFundingRunner(t)
	loan := installGrowthDeferredLoan(runner, "south", "ship_lien", false)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil || len(input.DeferredLoanReserves) != 1 {
		t.Fatalf("growth funding fixture: input=%+v err=%v", input, err)
	}
	required := runner.growthLoanReserves(input.ReserveByRegion)
	balances := clonePence(required)
	balances["north"] -= 1_000
	balances["south"] += 1_000
	setInstallmentConsumerBalances(runner, balances)
	runner.stateCache.Rates = []Rate{{FromRegion: "south", ToRegion: "north"}}
	if changed, err := runner.fundTreasury(h.ctx, balances, required); err != nil || !changed {
		t.Fatalf("exact growth remittance was not staged: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if !data.GrowthFunding || data.Action == nil || data.Action.Stage != "remit" {
		t.Fatalf("growth donor protection was not persisted: %+v", data)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("exact full-reserve donor failed first send: %v", err)
	}
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == loan.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence--
		}
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected donor reserves") {
		t.Fatalf("growth first send spent a deferred secured installment: %v", err)
	}
}
