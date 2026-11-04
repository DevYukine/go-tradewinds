package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func installmentConsumerLoan(t *testing.T, h *recoveryHarness, runner *Runner, region string) (PlanningInput, int64) {
	t.Helper()
	loan := snapshotTestLoan()
	loan.BankingRegion = region
	loan.BorrowerCompanyID = runner.account.CompanyID
	loan.NextPaymentDueAt = new(runner.stateCache.Clock.RealUTC.Add(gameDuration(10, runner.stateCache.Clock.Calendar.TimeScale)))
	loan.MaturesAt = new(runner.stateCache.Clock.RealUTC.Add(gameDuration(360, runner.stateCache.Clock.Calendar.TimeScale)))
	runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	full := 2 * *loan.InstallmentPence
	if len(input.DeferredLoanReserves) != 1 || input.DeferredLoanReserves[0].Region != region || input.DeferredLoanReserves[0].FullReserve != full {
		t.Fatalf("fixture did not defer its two-installment reserve: %+v", input.DeferredLoanReserves)
	}
	return input, full
}

func setInstallmentConsumerBalances(runner *Runner, balances map[string]int64) {
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.AccountType == "vault" {
			vault.BalancePence = balances[vault.BankingRegion]
		}
	}
}

func TestInstallmentReservesBlockCommissionAdmissionAndPreparation(t *testing.T) {
	for _, test := range []struct {
		name          string
		loanRegion    string
		fundingRegion string
		stage         string
	}{
		{"yard cash", "america", "america", "commission-accept"},
		{"donor cash", "north", "north", "remit"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := commissionHarness(t)
			loanCapacityRefresh(t, h, runner)
			input, full := installmentConsumerLoan(t, h, runner, test.loanRegion)
			rows, err := runner.commissionListings()
			if err != nil || len(rows) != 1 {
				t.Fatalf("commission listing: %+v, %v", rows, err)
			}
			row := rows[0]
			port, _ := findPort(runner.stateCache.Catalog, row.OriginPortID)
			quote, err := runner.commissionQuote(row)
			if err != nil {
				t.Fatal(err)
			}
			capital := *quote.TotalCostPence + row.CollateralPence
			balances := clonePence(input.ReserveByRegion)
			balances[test.fundingRegion] += full + capital - 1
			runner.stateCache.Rates = []Rate{{FromRegion: "north", ToRegion: "america", FeeBPS: 0}}
			for _, short := range []bool{true, false} {
				if !short {
					balances[test.fundingRegion]++
				}
				setInstallmentConsumerBalances(runner, balances)
				input.Balances = balances
				decision := runner.commissionAdmission(row, port, quote, input, 0)
				if decision.Accepted == short {
					t.Fatalf("commission admission at one-penny short=%v: %+v", short, decision)
				}
				changed, err := runner.driveCommissions(h.ctx)
				if err != nil || changed == short {
					t.Fatalf("commission preparation at one-penny short=%v: changed=%v err=%v", short, changed, err)
				}
				if short {
					h.missingMission(commissionMission)
					continue
				}
				_, data := h.mission(commissionMission)
				if data.Action == nil || data.Action.Stage != test.stage {
					t.Fatalf("exact full-reserve funding did not stage %s: %+v", test.stage, data.Action)
				}
			}
		})
	}
}

func TestInstallmentReservesBlockCommissionAcceptancePreflight(t *testing.T) {
	h, runner := commissionHarness(t)
	loanCapacityRefresh(t, h, runner)
	input, full := installmentConsumerLoan(t, h, runner, "america")
	rows, err := runner.commissionListings()
	if err != nil || len(rows) != 1 {
		t.Fatalf("commission listing: %+v, %v", rows, err)
	}
	quote, err := runner.commissionQuote(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	balances := clonePence(input.ReserveByRegion)
	balances["america"] += full + *quote.TotalCostPence + rows[0].CollateralPence
	// A reserve-safe donor can cover the missing penny at admission, but the
	// acceptance request must already have every penny in the yard vault.
	balances["north"] += 1_000
	runner.stateCache.Rates = []Rate{{FromRegion: "north", ToRegion: "america", FeeBPS: 0}}
	setInstallmentConsumerBalances(runner, balances)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("funded admission: changed=%v err=%v", changed, err)
	}
	_, data := h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "commission-accept" {
		t.Fatalf("missing acceptance request: %+v", data.Action)
	}
	balances["america"]--
	setInstallmentConsumerBalances(runner, balances)
	input, err = runner.committedPlanningInputExcept(h.ctx, ShipState{}, commissionMission)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := findPort(runner.stateCache.Catalog, rows[0].OriginPortID)
	if decision := runner.commissionAdmission(rows[0], port, quote, input, 0); !decision.Accepted {
		t.Fatalf("fixture did not isolate local first-send funding: %+v", decision)
	}
	if err := runner.commissionPreflight(h.ctx, &data); err == nil || !strings.Contains(err.Error(), "funding in the yard region") {
		t.Fatalf("acceptance spent a penny of deferred installment cash: %v", err)
	}
	balances["america"]++
	setInstallmentConsumerBalances(runner, balances)
	if err := runner.commissionPreflight(h.ctx, &data); err != nil {
		t.Fatalf("exact full-reserve-funded acceptance failed: %v", err)
	}
}

func TestInstallmentReservesProtectAcceptedCommissionFundingDonors(t *testing.T) {
	h, runner, _, _ := acceptedCommission(t)
	_, full := installmentConsumerLoan(t, h, runner, "north")
	input, err := runner.committedPlanningInputExcept(h.ctx, ShipState{}, commissionMission)
	if err != nil {
		t.Fatal(err)
	}
	quote := runner.stateCache.Quotes["boston|"+recoveryHull]
	balances := clonePence(input.ReserveByRegion)
	balances["america"] = 0
	balances["north"] += full + *quote.TotalCostPence - 1
	runner.stateCache.Rates = []Rate{{FromRegion: "north", ToRegion: "america", FeeBPS: 0}}
	setInstallmentConsumerBalances(runner, balances)
	if changed, err := runner.driveCommissions(h.ctx); changed || err != nil {
		t.Fatalf("accepted delivery borrowed a penny from its donor's installment reserve: changed=%v err=%v", changed, err)
	}
	_, data := h.mission(commissionMission)
	if data.Action != nil {
		t.Fatalf("reserve-unsafe delivery funding was staged: %+v", data.Action)
	}
	balances["north"]++
	setInstallmentConsumerBalances(runner, balances)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("exact full-reserve-funded recovery failed: changed=%v err=%v", changed, err)
	}
	mission, data := h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "remit" {
		t.Fatalf("funded recovery did not stage a remit: %+v", data.Action)
	}
	var body struct {
		From  string `json:"from_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(data.Action.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.From != "north" || body.Gross != *quote.TotalCostPence || balances["north"]-body.Gross != input.ReserveByRegion["north"]+full {
		t.Fatalf("recovery remit did not retain its donor's full reserve: %+v", body)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("exact donor reserve failed remit first-send preflight: %v", err)
	}
	balances["north"]--
	setInstallmentConsumerBalances(runner, balances)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected donor reserves") {
		t.Fatalf("staged commission remit spent a deteriorated donor's deferred reserve: %v", err)
	}
}

func TestInstallmentReservesPreserveAcceptedCommissionAtomicDelivery(t *testing.T) {
	h, runner, _, _ := acceptedCommission(t)
	input, _ := installmentConsumerLoan(t, h, runner, "america")
	quote := runner.stateCache.Quotes["boston|"+recoveryHull]
	balances := clonePence(input.ReserveByRegion)
	balances["america"] = *quote.TotalCostPence
	setInstallmentConsumerBalances(runner, balances)
	if changed, err := runner.driveCommissions(h.ctx); !changed || err != nil {
		t.Fatalf("deferred reserve blocked atomic accepted delivery: changed=%v err=%v", changed, err)
	}
	_, data := h.mission(commissionMission)
	if data.Action == nil || data.Action.Stage != "commission-deliver" {
		t.Fatalf("raw-cash-funded delivery did not advance: %+v", data.Action)
	}
	if err := runner.commissionPreflight(h.ctx, &data); err != nil {
		t.Fatalf("atomic delivery at the raw cash boundary failed: %v", err)
	}
	balances["america"]--
	setInstallmentConsumerBalances(runner, balances)
	if err := runner.commissionPreflight(h.ctx, &data); err == nil {
		t.Fatal("atomic delivery spent beyond its raw yard cash")
	}
}

func TestInstallmentReservesBlockMarqueAdmissionAndPreflight(t *testing.T) {
	for _, loanRegion := range []string{"north", "south"} {
		t.Run(loanRegion, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			input, full := installmentConsumerLoan(t, h, runner, loanRegion)
			balances := clonePence(input.ReserveByRegion)
			balances["north"] += full + proxy.quote.CostPence - 1
			runner.stateCache.Rates = []Rate{{FromRegion: "north", ToRegion: "south", FeeBPS: 0}}
			setInstallmentConsumerBalances(runner, balances)
			region, available, err := runner.marqueHomeFunds(h.ctx, false)
			if err != nil || region != "north" || available != proxy.quote.CostPence-1 {
				t.Fatalf("deferred loan cash entered marque funds: region=%s available=%d err=%v", region, available, err)
			}
			if changed, err := runner.considerMarque(h.ctx); changed || err != nil {
				t.Fatalf("marque spent a penny of deferred loan cash: changed=%v err=%v", changed, err)
			}
			h.missingMission("__marque")
			balances["north"]++
			setInstallmentConsumerBalances(runner, balances)
			if changed, err := runner.considerMarque(h.ctx); !changed || err != nil {
				t.Fatalf("exact full-reserve-funded marque failed: changed=%v err=%v", changed, err)
			}
			if err := runner.recordMarques(h.ctx); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission("__marque")
			if changed, err := runner.driveMarque(h.ctx, mission, &data); !changed || err != nil || data.Action == nil {
				t.Fatalf("funded marque staging: changed=%v action=%+v err=%v", changed, data.Action, err)
			}
			if err := runner.marquePreflight(h.ctx, &data, true); err != nil {
				t.Fatalf("full reserve or own pending cost was counted twice: %v", err)
			}
			balances["north"]--
			setInstallmentConsumerBalances(runner, balances)
			if err := runner.marquePreflight(h.ctx, &data, true); err == nil || !strings.Contains(err.Error(), "protected regional commitments") {
				t.Fatalf("marque first send spent deferred loan cash: %v", err)
			}
		})
	}
}

func TestInstallmentReservesBlockUnderwritingAdmissionAndPreflight(t *testing.T) {
	for _, loanRegion := range []string{"north", "south"} {
		t.Run(loanRegion, func(t *testing.T) {
			h := newRecoveryHarness(t)
			base := time.Now().UTC()
			proxy := installUWExecProxy(t, h)
			proxy.insuranceRows = uwExecBoardRows(base)
			runner := h.runner(primaryTestKey)
			loanCapacityRefresh(t, h, runner)
			input, full := installmentConsumerLoan(t, h, runner, loanRegion)
			policy := uwExecPolicy(base)
			collateral := policy.InsuredValuePence / 20
			const smallVault int64 = 10_000
			shortfall := input.ReserveByRegion["south"] - smallVault
			if shortfall <= 0 {
				t.Fatal("fixture lacks a protected south-region arrival")
			}
			balances := clonePence(input.ReserveByRegion)
			balances["south"] = smallVault
			balances["north"] += shortfall + full + collateral - 1
			runner.stateCache.Rates = []Rate{{FromRegion: "north", ToRegion: "south", FeeBPS: 0}}
			setInstallmentConsumerBalances(runner, balances)
			if _, err := h.db.EnsureUnderwritingBudget(h.ctx, h.account.ID, collateral); err != nil {
				t.Fatal(err)
			}
			budget, err := runner.underwritingBudget(h.ctx, policy, false)
			if err != nil || budget.DiscretionaryPence != collateral-1 || budget.AvailableLocalPence != collateral-1 || budget.RemainingPilotPence != collateral {
				t.Fatalf("deferred loan cash entered underwriting funds: %+v err=%v", budget, err)
			}
			if changed, err := runner.considerUnderwriting(h.ctx); changed || err != nil {
				t.Fatalf("underwriting spent a penny of deferred loan cash: changed=%v err=%v", changed, err)
			}
			h.missingMission("__underwriting")
			balances["north"]++
			setInstallmentConsumerBalances(runner, balances)
			budget, err = runner.underwritingBudget(h.ctx, policy, false)
			if err != nil || budget.DiscretionaryPence != collateral || budget.AvailableLocalPence != collateral {
				t.Fatalf("exact full-reserve-funded budget: %+v err=%v", budget, err)
			}
			if changed, err := runner.considerUnderwriting(h.ctx); !changed || err != nil {
				t.Fatalf("exact full-reserve-funded underwriting failed: changed=%v err=%v", changed, err)
			}
			if err := runner.recordUnderwriting(h.ctx); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission("__underwriting")
			if changed, err := runner.driveUnderwriting(h.ctx, mission, &data); !changed || err != nil || data.Action == nil {
				t.Fatalf("funded underwriting staging: changed=%v action=%+v err=%v", changed, data.Action, err)
			}
			if err := runner.underwritingPreflight(h.ctx, &data, true); err != nil {
				t.Fatalf("full reserve or own pending collateral was counted twice: %v", err)
			}
			balances["north"]--
			setInstallmentConsumerBalances(runner, balances)
			if err := runner.underwritingPreflight(h.ctx, &data, true); err == nil || !strings.Contains(err.Error(), "full collateral exceeds") {
				t.Fatalf("underwriting first send spent deferred loan cash: %v", err)
			}
		})
	}
}

func TestInstallmentReservesBlockAmericaFlagshipBuildAndPreflight(t *testing.T) {
	h, runner, _ := americaFlagshipFixture(t)
	loanCapacityRefresh(t, h, runner)
	_, full := installmentConsumerLoan(t, h, runner, "america")
	loan := runner.stateCache.Loans[0]
	second, foreign := loan, loan
	second.ID, foreign.ID, foreign.BankingRegion = "second-america-loan", "north-loan", "north"
	runner.stateCache.Loans = append(runner.stateCache.Loans, second, foreign)
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	input, err := runner.committedPlanningInputExcept(h.ctx, ShipState{}, americaFlagshipMission)
	if err != nil || len(input.DeferredLoanReserves) != 3 {
		t.Fatalf("multiple regional loan deferrals: %+v err=%v", input.DeferredLoanReserves, err)
	}
	quote, err := runner.americaFlagshipQuote(h.ctx, recoveryHull)
	if err != nil {
		t.Fatal(err)
	}
	balances := clonePence(input.ReserveByRegion)
	balances["america"] += 2*full + *quote.TotalCostPence - 1
	setInstallmentConsumerBalances(runner, balances)
	data := missionData{
		BuildPort: "boston", BuildClass: recoveryHull, BuildName: "Lapras",
		PlayerLoan: &playerLoanIntent{HullID: recoveryHull},
		Capacity:   &capacityIntent{Financing: &capacityFinancing{Loans: []gameapi.Loan{loan}}},
		Action: &durableAction{Stage: "build", Body: encode(map[string]any{
			"port_id": "boston", "ship_class": recoveryHull, "name": "Lapras", "build_mode": "turnkey_cash", "mortgage": false,
		})},
	}
	if _, err := runner.americaFlagshipBuildQuote(h.ctx, &data); err == nil {
		t.Fatal("flagship spent a penny of its two deferred America loan reserves")
	}
	if err := runner.americaFlagshipPreflight(h.ctx, &data); err == nil {
		t.Fatal("flagship first send spent deferred installment cash")
	}
	balances["america"]++
	setInstallmentConsumerBalances(runner, balances)
	if _, err := runner.americaFlagshipBuildQuote(h.ctx, &data); err != nil {
		t.Fatalf("exact full-reserve-funded flagship quote failed: %v", err)
	}
	if err := runner.americaFlagshipPreflight(h.ctx, &data); err != nil {
		t.Fatalf("exact America reserve funding was blocked by another region's deferral: %v", err)
	}
}
