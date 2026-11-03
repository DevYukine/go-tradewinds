package engine

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func installDebtServiceHoldLoan(runner *Runner, region string) gameapi.Loan {
	loan := snapshotTestLoan()
	loan.BankingRegion = region
	loan.CollateralKind, loan.CollateralShipID = "unsecured", nil
	loan.CollateralValuePence, loan.CollateralStatus = 0, "none"
	runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	return loan
}

func serveDebtServiceLoanPayments(h *recoveryHarness, runner *Runner) {
	h.api.mu.Lock()
	h.api.playerLoans = append([]gameapi.Loan(nil), runner.stateCache.Loans...)
	h.api.mu.Unlock()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" || request.URL.Path == "/v1/loan/repay" {
			h.api.handle(writer, request)
			return
		}
		downstream.ServeHTTP(writer, request)
	})
}

func setDebtServiceFreeCash(t *testing.T, h *recoveryHarness, runner *Runner, region string, free int64) {
	t.Helper()
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.AccountType != "vault" {
			continue
		}
		vault.BalancePence = input.ReserveByRegion[vault.BankingRegion]
		if vault.BankingRegion == region {
			vault.BalancePence += free
			found = true
		}
	}
	if !found {
		t.Fatalf("no vault in debt-service fixture region %s", region)
	}
}

func requireDebtServiceHoldReason(t *testing.T, reason string) {
	t.Helper()
	if !strings.HasPrefix(reason, debtServiceHoldPrefix) {
		t.Fatalf("missing named debt-service hold: %q", reason)
	}
}

func requireDebtServiceHoldDecision(t *testing.T, h *recoveryHarness, shipID, kind string) store.Decision {
	t.Helper()
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range history.([]store.Decision) {
		if row.ShipID == shipID && row.Kind == kind {
			requireDebtServiceHoldReason(t, row.Reason)
			return row
		}
	}
	t.Fatalf("no newest debt-service decision for %q %q", shipID, kind)
	return store.Decision{}
}

func TestDebtServiceHoldCountsOnlyOwnActiveUnsecuredPayoffs(t *testing.T) {
	t.Parallel()
	loan := snapshotTestLoan()
	loan.CollateralKind, loan.CollateralShipID = "unsecured", nil
	runner := &Runner{account: store.Account{CompanyID: recoveryCompanyID}}
	runner.stateCache.Loans = []gameapi.Loan{loan}
	if got := runner.debtServiceHoldPence(); got != 702_225 {
		t.Fatalf("hold = %d, want 750000 owed + 625 interest - 48400 reserved", got)
	}
	for _, test := range []struct {
		name   string
		change func(*gameapi.Loan)
	}{
		{"mortgage", func(loan *gameapi.Loan) { loan.IsMortgage = true }},
		{"ship lien", func(loan *gameapi.Loan) { loan.CollateralKind = "ship_lien" }},
		{"other company", func(loan *gameapi.Loan) { loan.BorrowerCompanyID = "other" }},
		{"defaulted", func(loan *gameapi.Loan) { loan.Status = "defaulted" }},
		{"repaid", func(loan *gameapi.Loan) { loan.Status = "repaid" }},
		{"no balance", func(loan *gameapi.Loan) { loan.OwedPence = 0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			ignored := loan
			test.change(&ignored)
			runner.stateCache.Loans = []gameapi.Loan{ignored}
			if got := runner.debtServiceHoldPence(); got != 0 {
				t.Fatalf("excluded loan created a hold of %d: %+v", got, ignored)
			}
		})
	}
	second := loan
	second.ID = "second-unsecured"
	runner.stateCache.Loans = []gameapi.Loan{loan, second}
	if got := runner.debtServiceHoldPence(); got != 1_404_450 {
		t.Fatalf("two qualifying loans did not each retain their remaining payoff: %d", got)
	}
	installment := loan.OwedPence
	loan.InstallmentPence = &installment
	runner.stateCache.Loans = []gameapi.Loan{loan}
	if got := runner.debtServiceHoldPence(); got != 0 {
		t.Fatalf("a fully reserved payoff created a negative or duplicate hold: %d", got)
	}
}

func TestDebtServiceHoldGrowthHullCashBoundaries(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"below hold", "one penny short of build", "exact build surplus", "above build surplus"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			installDebtServiceHoldLoan(runner, "north_sea")
			serveDebtServiceLoanPayments(h, runner)
			hold := runner.debtServiceHoldPence()
			setDebtServiceFreeCash(t, h, runner, "north_sea", hold+1_000_000)
			_, _, control, _, _ := loanCapacityDecision(t, h, runner)
			if !control.Accepted {
				t.Fatalf("well-funded control rejected its growth hull: %+v", control)
			}
			free := hold + growthCapital(control) + control.WorkingCapitalPence
			if scenario == "below hold" {
				free = hold - 1
			} else if scenario == "one penny short of build" {
				free--
			} else if scenario == "above build surplus" {
				free++
			}
			setDebtServiceFreeCash(t, h, runner, "north_sea", free)
			runner.snapshot["_capacity_financing_backoffs"] = encode(map[string]time.Time{
				recoveryHomePort + "|" + recoveryHull: time.Now().UTC().Add(time.Hour),
			})
			changed, err := runner.considerGrowth(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "below hold" || scenario == "one penny short of build" {
				h.missingMission("__growth")
				if scenario == "below hold" {
					requireDebtServiceHoldDecision(t, h, "", "capacity_growth")
				} else {
					requireDebtServiceHoldDecision(t, h, "", "capacity_candidate:"+recoveryHomePort+"|"+recoveryHull)
				}
				if h.api.mutationCount("/v1/loan/quote") != 0 || h.api.mutationCount("/v1/loan/request") != 0 {
					t.Fatal("cash withheld for repayment was replaced with new growth debt")
				}
				wantRepayments := 0
				if scenario == "one penny short of build" {
					wantRepayments = 1
				}
				if got := h.api.mutationCount("/v1/loan/repay"); got != wantRepayments {
					t.Fatalf("blocked growth did not safely use its actual repayment surplus: got %d repayments, want %d", got, wantRepayments)
				}
				return
			}
			mission, data := h.mission("__growth")
			if !changed || data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("cash above payoff plus full build did not admit growth: %+v", data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("exactly funded growth disagreed with its first-send gate: %v", err)
			}
		})
	}
	if !expectedGrowthAdmission(debtServiceHoldPrefix) {
		t.Fatal("ordinary debt-service waiting was reported as a controller stall")
	}
}

func TestDebtServiceHoldExpansionProbeAndFirstHire(t *testing.T) {
	t.Parallel()
	h, runner, _ := overseasPairFixture(t)
	removeExpansionGossip(runner)
	runner.stateCache.Quotes = nil
	control, reason, err := runner.expansionAdmission(h.ctx)
	if err != nil || reason != "" || !control.probe {
		t.Fatalf("evidence-only probe control: %+v %q %v", control, reason, err)
	}
	installDebtServiceHoldLoan(runner, "north")
	hold := runner.debtServiceHoldPence()
	setDebtServiceFreeCash(t, h, runner, "north", hold-1)
	if _, reason, err := runner.expansionAdmission(h.ctx); err != nil {
		t.Fatal(err)
	} else {
		requireDebtServiceHoldReason(t, reason)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("probe spent its loan payoff: %t %v", changed, err)
	}
	h.missingMission("__treasury")
	now := time.Now().UTC()
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: now, Probe: true},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: now, Probe: true},
	})
	if err := runner.expansionHireReady(h.ctx, encode(map[string]string{"port_id": "boston"})); err == nil {
		t.Fatal("first hire bypassed the probe's loan hold")
	} else {
		requireDebtServiceHoldReason(t, err.Error())
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("first hire selection disagreed with its hold preflight: %t %v", changed, err)
	}
	h.missingMission("__treasury")
	agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
	agents[0].HiredAt = now
	runner.snapshot["_expansion_agents"] = encode(agents)
	runner.stateCache.Factors = append(runner.stateCache.Factors, "boston")
	setDebtServiceFreeCash(t, h, runner, "america", 0)
	if err := runner.expansionHireReady(h.ctx, encode(map[string]string{"port_id": "new_york"})); err != nil {
		t.Fatalf("already-paid pair could not finish its reserved partner hire: %v", err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("already-paid pair's partner was blocked at selection: %t %v", changed, err)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "new_york") {
		t.Fatalf("pair did not finish its admitted partner: %+v", data.Action)
	}
}

func TestDebtServiceHoldGrowthBorrowingCashBoundaries(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{"capacity", "savings"} {
		for _, boundary := range []string{"no loan control", "below hold", "one penny short of buffer", "exact buffer", "above buffer"} {
			t.Run(entry+"/"+boundary, func(t *testing.T) {
				h, runner := playerLoanAcceptanceFixture(t)
				pledge := runner.stateCache.Catalog.ShipClasses[0]
				pledge.ID = "brigantine"
				runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, pledge)
				pledgedShip := runner.stateCache.Ships[0]
				pledgedShip.Ship.ID, pledgedShip.Ship.ShipClass = recoveryShipCharizard, pledge.ID
				runner.stateCache.Ships = append(runner.stateCache.Ships, pledgedShip)
				quote := runner.stateCache.Quotes[recoveryHomePort+"|"+recoveryHull]
				quote.ShipClass, quote.TotalCostPence = pledge.ID, new(int64(500_000))
				pledgeKey := recoveryHomePort + "|" + pledge.ID
				runner.stateCache.Quotes[pledgeKey] = quote
				runner.meta["yard_quotes/"+pledgeKey] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+recoveryHull]
				setDebtServiceFreeCash(t, h, runner, "north_sea", 1_000_000)
				_, _, funded, _, _ := loanCapacityDecision(t, h, runner)
				if !funded.Accepted {
					t.Fatalf("borrowing control is not viable when cash-funded: %+v", funded)
				}
				setDebtServiceFreeCash(t, h, runner, "north_sea", growthCapital(funded)+funded.WorkingCapitalPence-25_000)
				port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
				if boundary != "no loan control" {
					installDebtServiceHoldLoan(runner, port.BankingRegion)
					hold := runner.debtServiceHoldPence()
					buffer := max(0, decision.WorkingCapitalPence-decision.CandidateLotCashPence)
					if buffer <= 0 {
						t.Fatal("borrowing fixture has no incumbent cash spend to protect")
					}
					setDebtServiceFreeCash(t, h, runner, port.BankingRegion, hold+buffer)
					port, hull, decision, required, evidence = loanCapacityDecision(t, h, runner)
					free := hold + buffer
					if boundary == "below hold" {
						free = hold - 1
					} else if boundary == "one penny short of buffer" {
						free--
					} else if boundary == "above buffer" {
						free++
					}
					setDebtServiceFreeCash(t, h, runner, port.BankingRegion, free)
					evidence.balances = runner.planningInput(ShipState{}).Balances
				}
				if decision.Accepted || !decision.EconomicallyViable || decision.CashShortfallPence <= 0 {
					t.Fatalf("borrowing fixture is not cash-only short: %+v", decision)
				}
				var changed bool
				var err error
				if entry == "capacity" {
					changed, err = runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence)
				} else {
					if !capacitySavingsFinancingWorthwhile(GrowthDecision{}, decision, 1, 1) {
						t.Fatal("savings fixture would not borrow without its hold")
					}
					changed, err = runner.considerCapacitySavingsFinancing(h.ctx, GrowthDecision{}, &capacityCandidate{
						port: port, hull: hull, decision: decision, required: required,
					}, 1, 1, evidence)
				}
				if err != nil {
					t.Fatal(err)
				}
				blocked := boundary == "below hold" || boundary == "one penny short of buffer"
				if blocked {
					if changed {
						t.Fatal("growth borrowing spent the payoff hold")
					}
					requireDebtServiceHoldDecision(t, h, "", "capacity_financing:"+port.ID+"|"+hull.ID)
					h.missingMission("__growth")
					return
				}
				if !changed {
					history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
					if err != nil {
						t.Fatal(err)
					}
					for _, row := range history.([]store.Decision) {
						if row.Kind == "capacity_financing:"+port.ID+"|"+hull.ID {
							t.Fatalf("operating-safe borrowing was refused: %s %s", row.Reason, row.Details)
						}
					}
					t.Fatal("operating-safe borrowing did not stage or record a refusal")
				}
				mission, data := h.mission("__growth")
				if !changed || data.PlayerLoan == nil || data.Action == nil || data.Action.Stage != "loan-request" {
					t.Fatalf("operating-safe surplus did not stage real borrowing: %+v", data)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("admitted borrowing disagreed with first send: %v", err)
				}
			})
		}
	}
}

func TestDebtServiceHoldExpansionCashBoundaries(t *testing.T) {
	t.Parallel()
	for _, probe := range []bool{true, false} {
		name := "build"
		if probe {
			name = "probe"
		}
		for _, delta := range []int64{-1, 0, 1} {
			t.Run(name+"/"+map[int64]string{-1: "one penny short", 0: "exact", 1: "above"}[delta], func(t *testing.T) {
				h, runner, _ := overseasPairFixture(t)
				if probe {
					removeExpansionGossip(runner)
					runner.stateCache.Quotes = nil
				}
				control, reason, err := runner.expansionAdmission(h.ctx)
				if err != nil || reason != "" || control.probe != probe {
					t.Fatalf("expansion control failed: %+v %q %v", control, reason, err)
				}
				spend := control.required[control.region] - control.input.ReserveByRegion[control.region]
				if !probe {
					if !control.build.Accepted {
						t.Fatalf("build control was not economically admitted: %+v", control.build)
					}
					spend = growthCashTotal(spend, growthCapital(control.build), control.build.WorkingCapitalPence)
				}
				installDebtServiceHoldLoan(runner, "north")
				setDebtServiceFreeCash(t, h, runner, "america", runner.debtServiceHoldPence()+spend+delta)
				admission, reason, err := runner.expansionAdmission(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				if delta < 0 {
					requireDebtServiceHoldReason(t, reason)
					if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || changed {
						t.Fatalf("cash-short expansion spent its payoff hold: %t %v", changed, err)
					}
					h.missingMission("__treasury")
					return
				}
				if reason != "" || admission.probe != probe || !probe && !admission.build.Accepted {
					t.Fatalf("exactly funded expansion was rejected: %q %+v", reason, admission.build)
				}
				if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || !changed {
					t.Fatalf("funded expansion did not stage its first hire: %t %v", changed, err)
				}
				mission, data := h.mission("__treasury")
				if data.Action == nil || data.Action.Stage != "hire-factor" {
					t.Fatalf("funded expansion did not stage its actual first fee: %+v", data.Action)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("funded expansion disagreed with first-send cash: %v", err)
				}
			})
		}
	}
}

func TestDebtServiceHoldGrowthRequestAndApprovalFirstSend(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"request", "approval"} {
		t.Run(stage, func(t *testing.T) {
			var h *recoveryHarness
			var runner *Runner
			if stage == "request" {
				h, runner = playerLoanAcceptanceFixture(t)
				playerLoanAcceptanceAdmit(t, h, runner)
				playerLoanAcceptanceDrive(t, h, runner)
			} else {
				h, runner = playerLoanAcceptancePosted(t)
				quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
				playerLoanAcceptancePublishQuote(t, h, runner, quote)
				playerLoanAcceptanceStageApproval(t, h, runner)
			}
			mission, data := h.mission("__growth")
			installDebtServiceHoldLoan(runner, "north_sea")
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
				t.Fatalf("growth %s first-send missed a new unsecured liability", stage)
			} else {
				requireDebtServiceHoldReason(t, err.Error())
			}
		})
	}
}

func TestDebtServiceHoldFuturesAdmissionAndRegionalCap(t *testing.T) {
	t.Parallel()
	h, _, runner, mission, data := futuresStagedAccept(t)
	installDebtServiceHoldLoan(runner, "north_sea")
	hold := runner.debtServiceHoldPence()
	setDebtServiceFreeCash(t, h, runner, "north_sea", hold-1)
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.futuresBookBudgets(h.ctx, rows); err == nil || !errors.Is(err, errFuturesPrecondition) {
		t.Fatalf("futures budget admitted cash below the fleet-wide payoff: %v", err)
	} else {
		requireDebtServiceHoldReason(t, err.Error())
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("staged futures admission bypassed the debt hold")
	} else {
		requireDebtServiceHoldReason(t, err.Error())
	}
	setDebtServiceFreeCash(t, h, runner, "north_sea", hold+10_000)
	_, free, err := runner.futuresBookBudgets(h.ctx, rows)
	if err != nil || free["north_sea"] != 10_000 {
		t.Fatalf("regional futures free cash did not cap at fleet free minus hold: %v %v", free, err)
	}
}

func TestDebtServiceHoldDoesNotBlockCargoBuy(t *testing.T) {
	t.Parallel()
	h, runner, ship, plan := incomeReviewFixture(t, "charter", true, 4)
	mission, data := incomeReviewStage(t, h, runner, ship, plan, "buy")
	port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
	installDebtServiceHoldLoan(runner, port.BankingRegion)
	setDebtServiceFreeCash(t, h, runner, port.BankingRegion, 100_000)
	if runner.debtServiceHoldPence() <= 100_000 {
		t.Fatal("cargo regression did not exceed its free cash with debt")
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("loan payoff hold leaked into ordinary cargo buying: %v", err)
	}
}

func TestDebtServiceHoldDoesNotBlockCommissioningRefit(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("admit commissioning control: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("build commissioning control: %t %v", changed, err)
	}
	actionLogRefresh(t, h, runner)
	installDebtServiceHoldLoan(runner, "north")
	setDebtServiceFreeCash(t, h, runner, "north", 100_000)
	mission, data = h.mission("__growth")
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("loan hold stopped commissioning cannon staging: %t %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil || data.Action.Stage != "refit" {
		t.Fatalf("commissioning did not stage its cannons: %+v", data.Action)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("loan hold leaked into commissioning cannon first-send: %v", err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || h.api.mutationCount("/v1/shipyard/modify-ship") != 1 {
		t.Fatalf("commissioning cannons did not go ahead below the hold: %t %v", changed, err)
	}
}

func TestDebtServiceHoldDoesNotBlockArrearsCure(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loan := installDebtServiceHoldLoan(runner, "north_sea")
	loan.ArrearsPence = 1_000
	runner.stateCache.Loans[0] = loan
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	serveDebtServiceLoanPayments(h, runner)
	setDebtServiceFreeCash(t, h, runner, loan.BankingRegion, 0)
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.AccountType == "vault" && vault.BankingRegion == loan.BankingRegion {
			vault.BalancePence += loan.ArrearsPence - loanReserve(loan)
		}
	}
	if changed, err := runner.serviceLoans(h.ctx); err != nil || !changed {
		t.Fatalf("loan payoff hold stopped arrears cure: %t %v", changed, err)
	}
	mission, data := h.mission("__loans")
	calls := h.api.mutationsFor("/v1/loan/repay")
	var repayment gameapi.RepayLoanRequest
	if len(calls) != 1 || json.Unmarshal(calls[0].Body, &repayment) != nil || repayment.LoanID != loan.ID ||
		repayment.AmountPence == nil || *repayment.AmountPence != loan.ArrearsPence {
		t.Fatalf("arrears cure did not send its exact partial payment: %+v", calls)
	}
	if mission.Phase != "done" || data.Action != nil || len(h.pending()) != 0 {
		t.Fatalf("loan hold left an unresolved arrears cure: phase %s, %+v", mission.Phase, data)
	}
}

func TestDebtServiceHoldClearsCapacitySavingsBeforePrepayment(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	capacitySavingsSpendMarker(runner)
	loan := installDebtServiceHoldLoan(runner, "north_sea")
	serveDebtServiceLoanPayments(h, runner)
	setDebtServiceFreeCash(t, h, runner, "north_sea", 2_000_000)
	input, ready, err := runner.loanPrepaymentInput(h.ctx, loan)
	if err != nil || !ready {
		t.Fatalf("a savings target still prevented safe loan prepayment: %t %v", ready, err)
	}
	if _, exists := runner.snapshot["_capacity_savings_target"]; exists {
		t.Fatal("unsecured payoff left its capacity savings target live")
	}
	if input.Balances[loan.BankingRegion]-input.ReserveByRegion[loan.BankingRegion] < loanFullPayment(loan) {
		t.Fatal("prepayment control did not retain actual operating-safe surplus")
	}
	if changed, err := runner.prepayLoans(h.ctx); err != nil || !changed {
		t.Fatalf("cleared savings target did not permit repayment: %t %v", changed, err)
	}
}

func TestDebtServiceHoldFuturesActualAdmissionCashBoundaries(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"below hold", "one penny short", "exact", "above"} {
		t.Run(boundary, func(t *testing.T) {
			h, _ := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h)
			refreshMarqueExecution(t, h, runner)
			futuresCounterpartyFixture(runner, "buyer")
			listing := futuresListingFixture(runner, "debt-service-listing", "buyer", "bid", 1700, 1)
			runner.snapshot["futures"] = encode([]gameapi.FuturesContract{listing})
			control, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, 1)
			if err != nil || control == nil {
				t.Fatalf("actual futures admission control failed: %+v %v", control, err)
			}
			installDebtServiceHoldLoan(runner, "north_sea")
			hold := runner.debtServiceHoldPence()
			// A single counterparty may use one quarter of the ten-percent book.
			free := hold + 40*control.ExposurePence
			if boundary == "below hold" {
				free = hold - 1
			} else if boundary == "one penny short" {
				free--
			} else if boundary == "above" {
				free++
			}
			setDebtServiceFreeCash(t, h, runner, "north_sea", free)
			mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "debt-service", Phase: "evaluating"}
			data := missionData{Futures: &futuresState{}}
			changed, err := runner.driveFuturesController(h.ctx, mission, &data, nil)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "below hold" || boundary == "one penny short" {
				if changed || data.Action != nil {
					t.Fatalf("cash-short futures admission spent its payoff hold: %t %+v", changed, data.Action)
				}
				if boundary == "below hold" {
					requireDebtServiceHoldDecision(t, h, "__futures", "hold")
				}
				return
			}
			if !changed || data.Action == nil || data.Action.Stage != "futures-accept" {
				t.Fatalf("funded futures admission did not stage a real acceptance: %t %+v", changed, data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("funded futures admission disagreed with first send: %v", err)
			}
		})
	}
}

func TestDebtServiceHoldFuturesIntentDoesNotRestageWhileCashShort(t *testing.T) {
	t.Parallel()
	h, _, runner, mission, data := futuresStagedAccept(t)
	installDebtServiceHoldLoan(runner, "north_sea")
	setDebtServiceFreeCash(t, h, runner, "north_sea", runner.debtServiceHoldPence()-1)
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	data.Action = nil
	for range 2 {
		if changed, err := runner.driveFuturesController(h.ctx, mission, &data, rows); err != nil || changed || data.Action != nil {
			t.Fatalf("cash-short admitted futures intent re-entered its stage/veto loop: %t %v %+v", changed, err, data.Action)
		}
	}
	requireDebtServiceHoldDecision(t, h, "__futures", "hold")
}

func TestDebtServiceHoldFuturesOversizedRegionalObligations(t *testing.T) {
	t.Parallel()
	h, _, runner, mission, data := futuresStagedAccept(t)
	installDebtServiceHoldLoan(runner, "north_sea")
	setDebtServiceFreeCash(t, h, runner, "north_sea", runner.debtServiceHoldPence()-1)
	for _, region := range []string{"channel", "america"} {
		loan := snapshotTestLoan()
		loan.ID, loan.BankingRegion = "oversized-"+region, region
		loan.NextPaymentPence = new(int64(math.MaxInt64))
		runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
		runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{
			AccountType: "vault", BankingRegion: region, BalancePence: 1_000_000,
		})
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runner.futuresBookBudgets(h.ctx, rows); err == nil || !errors.Is(err, errFuturesPrecondition) {
		t.Fatalf("oversized regional deficits overflowed into a spendable budget: %v", err)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.HasPrefix(err.Error(), debtServiceHoldPrefix) {
		t.Fatalf("oversized regional deficits bypassed first-send payoff protection: %v", err)
	}
}

func TestDebtServiceHoldPhaseTwoPrepaymentReachable(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	installDebtServiceHoldLoan(runner, "north_sea")
	serveDebtServiceLoanPayments(h, runner)
	runner.stateCache.Clock.UnlockedPhase = 2
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, Port{
		ID: "unprobeable-overseas-yard", Phase: 2, HasShipyard: true, BankingRegion: "america",
	})
	setDebtServiceFreeCash(t, h, runner, "north_sea", 2_000_000)
	if !runner.overseasYardUnlocked() {
		t.Fatal("repayment regression did not enter the Phase-2 overseas hold")
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("Phase-2 overseas hold prevented safe voluntary payoff: %t %v", changed, err)
	}
	if got := h.api.mutationCount("/v1/loan/repay"); got != 1 {
		t.Fatalf("Phase-2 surplus did not repay the existing loan exactly once: %d", got)
	}
}

func TestDebtServiceHoldDoesNotVetoAdmittedMortgageBuild(t *testing.T) {
	t.Parallel()
	h, runner, _ := newMortgageFixture(t)
	port, hull, decision, _, evidence := loanCapacityDecision(t, h, runner)
	if !decision.Accepted || decision.Mortgage == nil {
		t.Fatalf("mortgage control did not admit its financed hull: %+v", decision)
	}
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID); err != nil || !changed {
		t.Fatalf("mortgage control did not stage its admitted build: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	loan := installDebtServiceHoldLoan(runner, port.BankingRegion)
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.AccountType == "vault" && vault.BankingRegion == port.BankingRegion {
			vault.BalancePence += loanReserve(loan)
		}
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unsecured payoff hold vetoed an already-admitted mortgage build: %v", err)
	}
}

func TestDebtServiceHoldMortgageAdmissionCashBoundaries(t *testing.T) {
	t.Parallel()
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(map[int64]string{-1: "one penny short", 0: "exact", 1: "above"}[delta], func(t *testing.T) {
			h, runner, _ := newMortgageFixture(t)
			port, hull, control, _, _ := loanCapacityDecision(t, h, runner)
			if !control.Accepted || control.Mortgage == nil {
				t.Fatalf("mortgage control was not admitted: %+v", control)
			}
			installDebtServiceHoldLoan(runner, port.BankingRegion)
			spend := growthCapital(control) + control.WorkingCapitalPence
			setDebtServiceFreeCash(t, h, runner, port.BankingRegion, runner.debtServiceHoldPence()+spend+delta)
			_, _, decision, _, evidence := loanCapacityDecision(t, h, runner)
			if delta < 0 {
				if decision.Accepted {
					t.Fatal("new mortgage spent the remaining unsecured payoff")
				}
				requireDebtServiceHoldReason(t, decision.Reason)
				return
			}
			if !decision.Accepted || decision.Mortgage == nil {
				t.Fatalf("cash above hold plus the financed spend did not admit a mortgage: %+v", decision)
			}
			if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID); err != nil || !changed {
				t.Fatalf("funded mortgage admission did not stage its build: %t %v", changed, err)
			}
			mission, data := h.mission("__growth")
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("funded mortgage admission disagreed with first send: %v", err)
			}
		})
	}
}

func TestDebtServiceHoldStableReasonKeepsCrownDiagnostics(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	installDebtServiceHoldLoan(runner, "north_sea")
	for _, free := range []int64{702_224, 702_223} {
		setDebtServiceFreeCash(t, h, runner, "north_sea", free)
		if reason, err := runner.growthCashFunding(h.ctx, &growthEvidence{}); err != nil || reason != debtServiceHoldPrefix {
			t.Fatalf("debt hold reason changed with cash: %q %v", reason, err)
		}
		if _, err := runner.holdCapacityGrowth(h.ctx, debtServiceHoldPrefix); err != nil {
			t.Fatal(err)
		}
	}
	row := requireDebtServiceHoldDecision(t, h, "", "capacity_financing:debt_service_hold")
	var details map[string]any
	if err := json.Unmarshal(row.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details["hold_crowns"] != 7022.25 || details["free_cash_crowns"] != 7022.24 {
		t.Fatalf("hold diagnostics lost their Crown amounts: %s", row.Details)
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, row := range history.([]store.Decision) {
		if strings.HasPrefix(row.Reason, debtServiceHoldPrefix) {
			counts[row.Kind]++
		}
	}
	for _, kind := range []string{"capacity_financing:debt_service_hold", "capacity_growth", "hold"} {
		if counts[kind] != 1 {
			t.Fatalf("cash drift churned %s decisions: %v", kind, counts)
		}
	}
}
