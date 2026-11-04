package engine

import (
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func installGrowthDeferredLoan(runner *Runner, region, collateral string, mortgage bool) gameapi.Loan {
	loan := snapshotTestLoan()
	loan.ID = "growth-deferred-" + region + "-" + collateral
	loan.BankingRegion, loan.CollateralKind, loan.IsMortgage = region, collateral, mortgage
	loan.BorrowerCompanyID = runner.account.CompanyID
	loan.NextPaymentDueAt = new(runner.stateCache.Clock.RealUTC.Add(gameDuration(10, runner.stateCache.Clock.Calendar.TimeScale)))
	loan.MaturesAt = new(runner.stateCache.Clock.RealUTC.Add(gameDuration(360, runner.stateCache.Clock.Calendar.TimeScale)))
	if mortgage {
		loan.ID += "-mortgage"
	}
	if collateral == "unsecured" {
		loan.CollateralShipID, loan.CollateralValuePence, loan.CollateralStatus = nil, 0, "none"
	}
	runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	return loan
}

func TestGrowthDeferredReservesDoNotDoubleCountUnsecuredPayoff(t *testing.T) {
	t.Run("mixed", func(t *testing.T) {
		h, runner := incomeFundingRunner(t)
		secured := installGrowthDeferredLoan(runner, "north", "ship_lien", false)
		mortgage := installGrowthDeferredLoan(runner, "south", "unsecured", true)
		unsecured := installGrowthDeferredLoan(runner, "north", "unsecured", false)
		input, err := runner.committedPlanningInput(h.ctx, ShipState{})
		if err != nil || len(input.DeferredLoanReserves) != 3 {
			t.Fatalf("mixed deferred-loan fixture: input=%+v err=%v", input, err)
		}
		required := runner.growthLoanReserves(input.ReserveByRegion)
		for region, full := range map[string]int64{"north": loanReserve(secured), "south": loanReserve(mortgage)} {
			if got, want := required[region], input.ReserveByRegion[region]+full; got != want {
				t.Fatalf("growth reserve in %s = %d, want %d without duplicate unsecured installments", region, got, want)
			}
		}
		const surplus = int64(12_345)
		balances := clonePence(required)
		balances["north"] += loanFullPayment(unsecured) + surplus
		setInstallmentConsumerBalances(runner, balances)
		evidence := growthEvidence{}
		if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" || evidence.available != surplus {
			t.Fatalf("growth retained more or less than secured installments plus unsecured payoff: available=%d reason=%q err=%v", evidence.available, reason, err)
		}
		if got := runner.debtServiceHoldPence(); got != loanFullPayment(unsecured) {
			t.Fatalf("deferred unsecured payoff changed: got=%d want=%d", got, loanFullPayment(unsecured))
		}
		required["north"]++
		if got := runner.growthLoanReserves(input.ReserveByRegion)["north"]; got != required["north"]-1 {
			t.Fatal("growth reserve restoration mutated its phased input")
		}
	})
	t.Run("stale clock", func(t *testing.T) {
		h, runner := incomeFundingRunner(t)
		secured := installGrowthDeferredLoan(runner, "north", "ship_lien", false)
		mortgage := installGrowthDeferredLoan(runner, "south", "unsecured", true)
		unsecured := installGrowthDeferredLoan(runner, "north", "unsecured", false)
		input, err := runner.committedPlanningInput(h.ctx, ShipState{})
		if err != nil || len(input.DeferredLoanReserves) != 3 {
			t.Fatalf("stale deferred-loan fixture is not phase zero: input=%+v err=%v", input, err)
		}
		meta := runner.meta["clock"]
		meta.Stale = true
		runner.meta["clock"] = meta
		// Reserve input captured at phase zero must keep its deferred phase
		// from the captured clock; the stale metadata must not recompute it.
		required := runner.growthLoanReserves(input.ReserveByRegion)
		for region, full := range map[string]int64{"north": loanReserve(secured), "south": loanReserve(mortgage)} {
			if got, want := required[region], input.ReserveByRegion[region]+full; got != want {
				t.Fatalf("stale clock dropped the %s deferred installment reserve: got=%d want=%d", region, got, want)
			}
		}
		if got, want := runner.debtServiceHoldPence(), loanFullPayment(unsecured); got != want {
			t.Fatalf("stale clock kept less than the whole unsecured payoff: got=%d want=%d", got, want)
		}
	})
}

func TestGrowthDeferredInstallmentsProtectHullAdmissionAndFirstSend(t *testing.T) {
	for _, mortgage := range []bool{false, true} {
		name := "secured"
		if mortgage {
			name = "mortgage"
		}
		t.Run(name, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			port, hull, control, _, _ := loanCapacityDecision(t, h, runner)
			if !control.Accepted {
				t.Fatalf("cash-funded hull control was rejected: %+v", control)
			}
			loan := installGrowthDeferredLoan(runner, port.BankingRegion, "ship_lien", mortgage)
			spend := growthCapital(control) + control.WorkingCapitalPence
			setDebtServiceFreeCash(t, h, runner, port.BankingRegion, loanReserve(loan)+spend-1)
			_, _, short, _, _ := loanCapacityDecision(t, h, runner)
			if short.Accepted || !short.EconomicallyViable || short.CashShortfallPence != 1 {
				t.Fatalf("hull admission consumed deferred installment capital: %+v", short)
			}
			setDebtServiceFreeCash(t, h, runner, port.BankingRegion, loanReserve(loan)+spend)
			_, _, funded, _, evidence := loanCapacityDecision(t, h, runner)
			if !funded.Accepted {
				t.Fatalf("exact full-reserve-funded hull was rejected: %+v", funded)
			}
			if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID); err != nil || !changed {
				t.Fatalf("stage the funded hull: changed=%t err=%v", changed, err)
			}
			mission, data := h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("funded hull did not stage construction: %+v", data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("exact hull admission disagreed with first send: %v", err)
			}
			for index := range runner.stateCache.Vaults {
				vault := &runner.stateCache.Vaults[index]
				if vault.AccountType == "vault" && vault.BankingRegion == port.BankingRegion {
					vault.BalancePence--
				}
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
				t.Fatal("hull first send spent a penny of deferred installment capital")
			}
		})
	}
}

func TestGrowthDeferredInstallmentsProtectExpansionAndHirePreflight(t *testing.T) {
	for _, mortgage := range []bool{false, true} {
		for _, probe := range []bool{false, true} {
			name := "secured/build"
			if mortgage {
				name = "mortgage/build"
			}
			if probe {
				name = strings.TrimSuffix(name, "build") + "probe"
			}
			t.Run(name, func(t *testing.T) {
				h, runner, _ := overseasPairFixture(t)
				if probe {
					removeExpansionGossip(runner)
					runner.stateCache.Quotes = nil
				}
				control, reason, err := runner.expansionAdmission(h.ctx)
				if err != nil || reason != "" || control.probe != probe || !probe && !control.build.Accepted {
					t.Fatalf("expansion control: admission=%+v reason=%q err=%v", control, reason, err)
				}
				installGrowthDeferredLoan(runner, control.region, "ship_lien", mortgage)
				input, err := runner.committedPlanningInput(h.ctx, ShipState{})
				if err != nil {
					t.Fatalf("deferred-loan planning input: %v", err)
				}
				// setDebtServiceFreeCash pins every donor vault exactly on its
				// phase floor, so the funding region alone must carry the full
				// isolated all-vault budget: the secured/mortgage full
				// installment reserve, the protected regional floor, the pair
				// hire fees and, for a build, its hull capital.
				budget := max(runner.growthLoanReserves(input.ReserveByRegion)[control.region], runner.protectedReservePence())
				spend := int64(2 * expansionAgentHireFeePence)
				if !probe {
					spend = growthCashTotal(spend, growthCapital(control.build), control.build.WorkingCapitalPence)
				}
				setDebtServiceFreeCash(t, h, runner, control.region, growthCashTotal(budget, spend)-1-input.ReserveByRegion[control.region])
				admission, reason, err := runner.expansionAdmission(h.ctx)
				if err != nil || reason == "" {
					t.Fatalf("expansion consumed deferred installment capital: admission=%+v reason=%q err=%v", admission, reason, err)
				}
				if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || changed {
					t.Fatalf("underfunded expansion staged its hire: changed=%t err=%v", changed, err)
				}
				h.missingMission("__treasury")
				setDebtServiceFreeCash(t, h, runner, control.region, growthCashTotal(budget, spend)-input.ReserveByRegion[control.region])
				admission, reason, err = runner.expansionAdmission(h.ctx)
				if err != nil || reason != "" || admission.probe != probe || !probe && !admission.build.Accepted {
					t.Fatalf("exact full-reserve-funded expansion: admission=%+v reason=%q err=%v", admission, reason, err)
				}
				if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || !changed {
					t.Fatalf("stage the funded expansion: changed=%t err=%v", changed, err)
				}
				mission, data := h.mission("__treasury")
				if data.Action == nil || data.Action.Stage != "hire-factor" {
					t.Fatalf("funded expansion did not stage its hire: %+v", data.Action)
				}
				// The persisted pair is now an incumbent commitment. Re-derive
				// the first-send boundary from that committed state rather than
				// reusing admission's pre-pair reserve map.
				stagedInput, err := runner.committedPlanningInput(h.ctx, ShipState{})
				if err != nil {
					t.Fatal(err)
				}
				stagedRequired, err := runner.expansionRequired(h.ctx, stagedInput, control.region, 2*expansionAgentHireFeePence)
				if err != nil {
					t.Fatal(err)
				}
				stagedBudget := stagedRequired[control.region]
				if !probe {
					stagedBudget = growthCashTotal(stagedBudget, growthCapital(admission.build), admission.build.WorkingCapitalPence)
				}
				setDebtServiceFreeCash(t, h, runner, control.region, stagedBudget-stagedInput.ReserveByRegion[control.region])
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("exact expansion admission disagreed with first send: %v", err)
				}
				for index := range runner.stateCache.Vaults {
					vault := &runner.stateCache.Vaults[index]
					if vault.AccountType == "vault" && vault.BankingRegion == control.region {
						vault.BalancePence--
					}
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
					t.Fatal("hire first send spent a penny of deferred installment capital")
				}
			})
		}
	}
}

func TestGrowthDeferredInstallmentsProtectPlayerLoanEvidence(t *testing.T) {
	for _, mortgage := range []bool{false, true} {
		name := "secured"
		if mortgage {
			name = "mortgage"
		}
		t.Run(name, func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			playerLoanAcceptanceAdmit(t, h, runner)
			_, data := h.mission("__growth")
			loan := installGrowthDeferredLoan(runner, data.PlayerLoan.BankingRegion, "ship_lien", mortgage)
			input, err := runner.committedPlanningInputExcept(h.ctx, ShipState{}, "__growth")
			if err != nil || len(input.DeferredLoanReserves) != 1 {
				t.Fatalf("player-loan deferral fixture: input=%+v err=%v", input, err)
			}
			_, _, _, evidence, needErr := runner.playerCapacityNeed(h.ctx, data.PlayerLoan)
			if evidence == nil {
				t.Fatalf("player-loan need did not evaluate growth cash: %v", needErr)
			}
			region := loan.BankingRegion
			if got, want := evidence.required[region], input.ReserveByRegion[region]+loanReserve(loan); got != want {
				t.Fatalf("player-loan growth reserve=%d want=%d; need error=%v", got, want, needErr)
			}
			if got, want := evidence.available, growthFreeCash(input.Balances, fullLoanReserves(input)); got != want {
				t.Fatalf("player-loan cash counted deferred installments: got=%d want=%d", got, want)
			}
		})
	}
}

func TestGrowthDeferredInstallmentsAlignRefitStagingAndFirstSend(t *testing.T) {
	for _, capacity := range []bool{false, true} {
		for _, mortgage := range []bool{false, true} {
			name := "lane/secured"
			if capacity {
				name = "capacity/secured"
			}
			if mortgage {
				name = strings.TrimSuffix(name, "secured") + "mortgage"
			}
			t.Run(name, func(t *testing.T) {
				h, runner := incomeFundingRunner(t)
				ship := runner.stateCache.Ships[0]
				ship.Ship.CannonsCount = 3
				runner.stateCache.Ships[0] = ship
				for index := range runner.stateCache.Catalog.Ports {
					if runner.stateCache.Catalog.Ports[index].ID == ship.Ship.CurrentPortID {
						runner.stateCache.Catalog.Ports[index].HasShipyard = true
					}
				}
				port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
				installGrowthDeferredLoan(runner, port.BankingRegion, "ship_lien", mortgage)
				const generation = "growth-deferred-refit"
				data := missionData{
					Plan: Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: port.ID, Destination: recoveryFarPort,
						Commodity: recoveryGrain, Quantity: 24, BuyLimitPence: 100, TargetStoresTons: 2},
					BuiltShipID: ship.Ship.ID, BuildPort: port.ID, BuildClass: ship.Ship.ShipClass,
					TargetCannons: 4, CostsKnown: true,
				}
				if capacity {
					data.Capacity = &capacityIntent{BuildPort: port.ID, BuildClass: ship.Ship.ShipClass}
				} else {
					data.GrowthWorkingCapitalPence = 3_000
					data.GrowthLane = &growthLaneIntent{AdmissionID: generation, QuantityLimit: 24, InitialProcurementLimitPence: 3_000}
				}
				h.putMission("__growth", generation, "growth_refit", data)
				mission, data := h.mission("__growth")
				input, err := runner.committedPlanningInput(h.ctx, ship)
				if err != nil || len(input.DeferredLoanReserves) != 1 {
					t.Fatalf("refit deferral fixture: input=%+v err=%v", input, err)
				}
				balances := fullLoanReserves(input)
				if !capacity {
					quoted := input
					quoted.ReserveByRegion = clonePence(input.ReserveByRegion)
					reserveGrowthCommissioning(&quoted, &data)
					quote := quoteGrowthDelivery(quoted, data.Plan, data.GrowthLane, true, nil)
					if !quote.plan.Accepted {
						t.Fatalf("refit fixture has no commissioning cargo allocation: %+v", quote.plan)
					}
					for region, required := range quote.required {
						balances[region] = max(balances[region], required)
					}
				}
				balances[port.BankingRegion] = fullLoanReserves(input)[port.BankingRegion] + 15_000 + data.GrowthWorkingCapitalPence - 1
				setInstallmentConsumerBalances(runner, balances)
				if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || changed || data.Action != nil {
					t.Fatalf("refit staging consumed a penny of deferred installments: changed=%t action=%+v err=%v", changed, data.Action, err)
				}
				balances[port.BankingRegion]++
				setInstallmentConsumerBalances(runner, balances)
				if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "refit" {
					t.Fatalf("exact funded refit did not stage its cannons: changed=%t action=%+v err=%v", changed, data.Action, err)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("exact refit admission disagreed with first send or counted its own guns twice: %v", err)
				}
				balances[port.BankingRegion]--
				setInstallmentConsumerBalances(runner, balances)
				if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected regional reserves") {
					t.Fatalf("refit first send consumed deferred installments: %v", err)
				}
			})
		}
	}
}

func TestInstallmentFuturesKeepsSecuredReservesAndUnsecuredPayoff(t *testing.T) {
	h, runner := incomeFundingRunner(t)
	secured := installGrowthDeferredLoan(runner, "north", "ship_lien", false)
	unsecured := installGrowthDeferredLoan(runner, "north", "unsecured", false)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil || len(input.DeferredLoanReserves) != 2 {
		t.Fatalf("futures deferred-loan fixture is not phase zero: input=%+v err=%v", input, err)
	}
	const surplus = int64(12_345)
	balances := clonePence(input.ReserveByRegion)
	balances["north"] += loanReserve(secured) + loanFullPayment(unsecured) + surplus
	setInstallmentConsumerBalances(runner, balances)
	budgets, free, err := runner.futuresBookBudgets(h.ctx, nil)
	if err != nil {
		t.Fatalf("futures budget refused its fully funded installment isolation: %v", err)
	}
	if got, want := free["north"], surplus; got != want {
		t.Fatalf("futures free cash exposed the secured reserve or the unsecured payoff: got=%d want=%d", got, want)
	}
	if got, want := budgets["north"], surplus/10; got != want {
		t.Fatalf("futures booking budget exposed the secured reserve or the unsecured payoff: got=%d want=%d", got, want)
	}
}
