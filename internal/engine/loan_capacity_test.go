package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestLoanCapacityOnlyCashRejectionCanBorrow(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*CapacityGrowthInput)
		want   bool
	}{
		{"cash only", func(in *CapacityGrowthInput) { in.SurplusPence = 0 }, true},
		{"cash already sufficient", func(in *CapacityGrowthInput) {}, false},
		{"no spare work", func(in *CapacityGrowthInput) { in.SurplusPence = 0; in.HasSpareWork = false }, false},
		{"upkeep consumes earnings", func(in *CapacityGrowthInput) { in.SurplusPence = 0; in.IncrementalDailyUpkeepPence = 100_000 }, false},
		{"payback too slow", func(in *CapacityGrowthInput) { in.SurplusPence = 0; in.MaxPaybackDays = 1 }, false},
		{"unpriced hull", func(in *CapacityGrowthInput) { in.SurplusPence = 0; in.HullPricePence = 0 }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := capacityGrowthAcceptanceInput()
			test.change(&input)
			decision := EvaluateCapacityGrowth(input)
			result := evaluateCapacityLoan(decision, 100_000, 750_000, 110_000, 5_000, 3_000)
			if result.Borrow != test.want || result.Reason == "" {
				t.Fatalf("financing repaired a non-cash rejection: decision %+v, loan %+v", decision, result)
			}
		})
	}
}

func TestLoanCapacityHalfNetCoverageBoundary(t *testing.T) {
	t.Parallel()
	decision := GrowthDecision{EconomicallyViable: true, IncrementalNetPencePerGameDay: 2_000}
	result := evaluateCapacityLoan(decision, 100_000, 750_000, 116_000, 8_000, 3_000)
	if !result.Borrow || result.Coverage == nil || *result.Coverage != 1.25 || result.DebtReservePence != 16_000 || result.NetAfterInterestPence <= 0 {
		t.Fatalf("exact 1.25x coverage was rejected or misreported: %+v", result)
	}
	decision.IncrementalNetPencePerGameDay = 1_999.99
	if result := evaluateCapacityLoan(decision, 100_000, 750_000, 116_000, 8_000, 3_000); result.Borrow {
		t.Fatalf("below-boundary half-net coverage admitted: %+v", result)
	}
	if result := evaluateCapacityLoan(decision, 100_000, 115_999, 116_000, 8_000, 3_000); result.Borrow {
		t.Fatalf("principal above available liens admitted: %+v", result)
	}
}

func TestLoanCapacityExactShortfallIncludesTwoInstallments(t *testing.T) {
	t.Parallel()
	decision := GrowthDecision{QuotedHullPence: 100_000, SlippageReservePence: 10_000, CannonsPence: 60_000, WorkingCapitalPence: 20_000}
	evidence := &growthEvidence{balances: map[string]int64{"north_sea": 100_000}, required: map[string]int64{"north_sea": 10_000}}
	principal, installment := estimatedCapacityPrincipal(100_000, 1)
	if principal != 100_000+2*installment || installment <= 0 || principal > goldsmithsMaximumPence {
		t.Fatalf("estimated principal does not cover exactly two installments: %d, %d", principal, installment)
	}
	_, protected, donors, funded := capacityLoanFunding(decision, evidence, "north_sea", principal, 2*installment, nil)
	if !funded || protected["north_sea"] != 190_000+2*installment || len(donors) != 0 ||
		evidence.balances["north_sea"] != 100_000 || evidence.required["north_sea"] != 10_000 {
		t.Fatalf("financing spent its cargo buffer, copied other obligations or mutated cash: %v, %v, funded %t", protected, donors, funded)
	}
}

func TestLoanCapacityEstimateRespectsLiensAndMinimum(t *testing.T) {
	t.Parallel()
	principal, installment := estimatedCapacityPrincipal(755_000, 2)
	if principal != 755_000+2*installment || principal <= goldsmithsMaximumPence || principal > 2*goldsmithsMaximumPence {
		t.Fatalf("two-ship estimate exceeds or underuses its exact package: %d, %d", principal, installment)
	}
	if principal, installment := estimatedCapacityPrincipal(750_000, 1); principal <= goldsmithsMaximumPence || installment != 0 {
		t.Fatalf("full nominal lien leaves no room for reserves: %d, %d", principal, installment)
	}
	principal, installment = estimatedCapacityPrincipal(1_000, 1)
	decision := GrowthDecision{EconomicallyViable: true, IncrementalNetPencePerGameDay: 10_000}
	if principal >= goldsmithsMinimumPence || evaluateCapacityLoan(decision, 1_000, 750_000, principal, installment, 3_000).Borrow {
		t.Fatal("small shortfall silently rounded up into minimum debt")
	}
}

func TestLoanCapacityFiniteCharterCannotFinancePerpetualRate(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	reward := int64(100_000)
	runner.stateCache.Contracts = []Contract{{
		ID: "finite-charter", ContractType: "charter", Status: "open", OriginPortID: recoveryFarPort,
		CommodityID: recoveryGrain, RemainingQuantity: 120, RewardPerTonPence: &reward,
	}}
	planning := runner.planningInput(growthCandidate(port, hull))
	planning.FleetDailyUpkeepPence = 0
	var rate float64
	var lot int64
	charterAccepted := false
	for _, plan := range PlanVoyages(planning) {
		if !plan.Accepted || len(plan.FreightIDs) != 0 {
			continue
		}
		lot = max(lot, growthCashTotal(plan.Economics.PurchasePence, plan.Economics.CustomsPence, plan.Economics.StoreCashPence))
		if plan.ContractID != "" {
			charterAccepted = true
		} else if plan.Economics.RevenueBasis == "depth" || plan.Economics.RevenueBasis == "gossip" {
			rate = max(rate, capacityVoyageRate(plan.Economics))
		}
	}
	if !charterAccepted || rate <= 0 || lot <= 0 {
		t.Fatalf("fixture lacks accepted charter, spot rate or cargo cash: %t, %v, %d", charterAccepted, rate, lot)
	}
	evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, evidence); reason != "" || err != nil {
		t.Fatalf("cash funding: %s, %v", reason, err)
	}
	input := capacityGrowthAcceptanceInput()
	input.LotCashPence, input.CandidateRatePencePerGameDay = 0, 0
	input.SurplusPence = evidence.available
	input.IncrementalDailyUpkeepPence = 0
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
	buffer := max(input.ReservePence, ceilBPS(*quote.TotalCostPence, 1_500)) + int64(input.FleetSize+1)*lot
	if err != nil || decision.IncrementalNetPencePerGameDay != rate || decision.WorkingCapitalPence != buffer {
		t.Fatalf("finite charter inflated perpetual earnings or lost its first-load cash: %+v, rate %v, buffer %d, %v", decision, rate, buffer, err)
	}
}

func TestLoanInstallmentReservesProtectOtherSpendingAndDonors(t *testing.T) {
	t.Parallel()
	loan := snapshotTestLoan()
	state := accountState{Loans: []gameapi.Loan{loan}}
	required := obligationReserves(state, nil, 0)
	if required["north_sea"] != 48_400 {
		t.Fatalf("two-installment reserve not protected: %v", required)
	}
	balances := map[string]int64{"north_sea": 48_400, "channel": 0}
	required["channel"] = 1
	if _, _, _, ok := bestRemittance(balances, required, []Rate{{FromRegion: "north_sea", ToRegion: "channel"}}, "channel"); ok {
		t.Fatal("another regional bill spent debt-service reserves")
	}
	balances["north_sea"]++
	if _, gross, _, ok := bestRemittance(balances, required, []Rate{{FromRegion: "north_sea", ToRegion: "channel"}}, "channel"); !ok || gross != 1 {
		t.Fatal("cash above the exact debt reserve was unnecessarily blocked")
	}
	loan.ArrearsPence = 60_000
	if reserve := loanReserve(loan); reserve != 60_000 {
		t.Fatalf("arrears not protected over the normal buffer: %d", reserve)
	}
	loan.Status = "defaulted"
	if reserve := loanReserve(loan); reserve != loan.OwedPence {
		t.Fatalf("defaulted recourse claim not protected: %d", reserve)
	}
}

func TestLoanUnavailableAuthorityPreservesDebtAndAllowsSafetyStores(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loan := snapshotTestLoan()
	runner.stateCache.Loans = []gameapi.Loan{loan}
	runner.snapshot["loans"] = encode([]gameapi.Loan{loan})
	meta := runner.meta["loans"]
	meta.Stale, meta.Complete, meta.Error = true, false, "loan authority unavailable"
	runner.meta["loans"] = meta
	input := runner.planningInput(ShipState{})
	if input.ReserveByRegion[loan.BankingRegion] < 48_400 {
		t.Fatal("unavailable loan authority discarded the last-good installment reserve")
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	decision := GrowthDecision{EconomicallyViable: true, CashShortfallPence: 100_000, IncrementalNetPencePerGameDay: 10_000}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, map[string]int64{}, &growthEvidence{}); err != nil || changed {
		t.Fatalf("unavailable debt created a borrowing intent: changed %t, %v", changed, err)
	}
	h.missingMission("__growth")
	ship := &runner.stateCache.Ships[0]
	ship.Voyage, ship.Ship.Status, ship.Ship.CurrentPortID = nil, "berthed", recoveryHomePort
	ship.CargoManifest = map[string]int64{recoveryGrain: 1}
	protected, err := runner.committedPlanningInput(h.ctx, *ship)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence = protected.ReserveByRegion[port.BankingRegion]
		}
	}
	mission := &store.Mission{ShipID: ship.Ship.ID}
	data := missionData{Action: &durableAction{Stage: "provision", Body: encode(map[string]any{
		"ship_id": ship.Ship.ID, "port_id": recoveryHomePort, "commodity_id": "provisions", "quantity": 1, "price_pence": 50, "into_stores": true,
	})}}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected regional bills reserves") {
		t.Fatalf("unavailable debt did not preserve cash at first-send spending: %v", err)
	}
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence += 100
		}
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("loan-list outage blocked safety stores above the last-good debt reserve: %v", err)
	}
	for _, stage := range []string{"sell", "dock"} {
		data.Action.Stage = stage
		if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
			t.Fatalf("unavailable debt blocked non-spending %s: %v", stage, err)
		}
	}
	data.Plan = Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort, Destination: recoveryFarPort}
	data.Action.Stage = "dock"
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unavailable debt blocked docking for an income mission: %v", err)
	}
	data.Relocation = &relocationIntent{IdlePilot: true}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unavailable debt blocked docking for a relocation pilot: %v", err)
	}
	data.Relocation = nil
	runner.stateCache.Contracts = append(runner.stateCache.Contracts, Contract{ID: "outage-charter", ContractType: "charter", Status: "open", OriginPortID: recoveryHomePort, CommodityID: recoveryGrain})
	data.Eligible = map[string]int64{recoveryGrain: 1}
	data.Action = &durableAction{Stage: "deliver-charter", Body: encode(map[string]any{"ship_id": ship.Ship.ID, "contract_id": "outage-charter", "quantity": 1})}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("unavailable debt blocked delivery of owned cargo: %v", err)
	}
}

// The shared recovery world predates the API's named region enum. Translate its
// two regions at the boundary so the same build/fleet harness exercises real
// loan validation rather than weakening authoritative loan fixtures.
func loanCapacityFixture(t *testing.T, cap int64, refuseBuild bool) (*recoveryHarness, *Runner) {
	t.Helper()
	return loanCapacityRoundingFixture(t, cap, refuseBuild, math.Ceil)
}

func loanCapacityRoundingFixture(t *testing.T, cap int64, refuseBuild bool, round func(float64) float64) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := capacityExecutionFixture(t, refuseBuild)
	base := h.api.server.Config.Handler
	quotes := make(map[string]gameapi.LoanQuote)
	loans := make([]gameapi.Loan, 0)
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/quote" {
			h.api.mu.Lock()
			h.api.record(request, nil, http.StatusOK)
			h.api.mu.Unlock()
			principal, err := strconv.ParseInt(request.URL.Query().Get("principal_pence"), 10, 64)
			if err != nil || principal < goldsmithsMinimumPence || principal > cap {
				h.api.writeProblem(writer, http.StatusBadRequest, "principal_cap", "principal outside this ship's lien cap")
				return
			}
			shipID, region := request.URL.Query().Get("ship_id"), request.URL.Query().Get("banking_region")
			installment := int64(round(float64(principal) * (0.30 / 36) / (1 - math.Pow(1+0.30/36, -36))))
			quote := gameapi.LoanQuote{
				ID: fmt.Sprintf("quote-%s-%d", shipID, principal), LenderCompanyID: "goldsmiths", BorrowerCompanyID: h.account.CompanyID,
				BankingRegion: region, PrincipalPence: principal, AnnualRateBPS: 3_000, TermGameDays: 360, Repayment: "amortised",
				TotalRepayablePence: installment * 36, InstallmentPence: &installment, CollateralShipID: &shipID,
				Status: "open", ExpiresAt: time.Now().Add(10 * time.Minute), CreatedAt: time.Now(),
			}
			quotes[quote.ID] = quote
			h.api.writeJSON(writer, http.StatusOK, gameapi.GoldsmithsQuote{Quote: quote, RebuildValuePence: 2_000_000, LoanToValueBPS: 4_000, MaxPrincipalPence: cap, Recourse: true})
			return
		}
		if request.URL.Path == "/v1/loan/list" {
			if request.URL.Query().Get("role") != "borrower" {
				t.Error("financing did not request borrower-only debt authority")
			}
			filtered := make([]gameapi.Loan, 0)
			for _, loan := range loans {
				if status := request.URL.Query().Get("status"); status == "" || loan.Status == status {
					filtered = append(filtered, loan)
				}
			}
			h.api.writeJSON(writer, http.StatusOK, filtered)
			return
		}
		if request.URL.Path == "/v1/loan/accept-quote" || request.URL.Path == "/v1/loan/repay" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			mission, err := h.db.GetMission(request.Context(), h.account.ID, "__growth")
			var data missionData
			if err != nil || mission == nil || json.Unmarshal(mission.Data, &data) != nil || data.Action == nil || !bytes.Equal(data.Action.Body, body) || data.Action.ID != request.Header.Get("Idempotency-Key") {
				t.Error("loan mutation lacked its durable immutable financing intent")
				writer.WriteHeader(http.StatusConflict)
				return
			}
			if journaled, err := h.db.HasAction(request.Context(), h.account.ID, data.Action.ID); err != nil || !journaled {
				t.Error("loan mutation reached the server before journal insertion")
				writer.WriteHeader(http.StatusConflict)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			if request.URL.Path == "/v1/loan/accept-quote" {
				var payload gameapi.QuoteActionRequest
				_ = json.Unmarshal(body, &payload)
				quote, exists := quotes[payload.QuoteID]
				if !exists {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				loan := snapshotTestLoan()
				loan.ID, loan.QuoteID, loan.BankingRegion = "capacity-loan", &quote.ID, quote.BankingRegion
				loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = quote.PrincipalPence, quote.PrincipalPence, quote.PrincipalPence
				loan.InstallmentPence, loan.NextPaymentPence, loan.TotalRepayablePence = quote.InstallmentPence, quote.InstallmentPence, &quote.TotalRepayablePence
				loan.CollateralShipID = quote.CollateralShipID
				loan.CreatedAt, loan.UpdatedAt = time.Now().UTC(), time.Now().UTC()
				loans = append(loans, loan)
				h.api.vaultBalance["north"] += loan.PrincipalPence
				h.api.record(request, body, http.StatusOK)
				h.api.writeJSON(writer, http.StatusOK, loan)
				return
			}
			var payload gameapi.RepayLoanRequest
			_ = json.Unmarshal(body, &payload)
			for i := range loans {
				if loans[i].ID == payload.LoanID && payload.AmountPence == nil {
					loan := &loans[i]
					h.api.vaultBalance["north"] -= loan.OwedPence
					loan.PrincipalRepaidPence += loan.OutstandingPrincipalPence
					loan.OwedPence, loan.OutstandingPrincipalPence = 0, 0
					loan.Status, loan.CollateralStatus = "repaid", "released"
					h.api.record(request, body, http.StatusOK)
					h.api.writeJSON(writer, http.StatusOK, loan)
					return
				}
			}
			writer.WriteHeader(http.StatusConflict)
			return
		}
		if request.Method == http.MethodGet {
			recorder := httptest.NewRecorder()
			base.ServeHTTP(recorder, request)
			for key, values := range recorder.Header() {
				writer.Header()[key] = values
			}
			writer.WriteHeader(recorder.Code)
			body := strings.ReplaceAll(recorder.Body.String(), `"north"`, `"north_sea"`)
			body = strings.ReplaceAll(body, `"south"`, `"channel"`)
			_, _ = io.WriteString(writer, body)
			return
		}
		base.ServeHTTP(writer, request)
	})
	loanCapacityRefresh(t, h, runner)
	return h, runner
}

func loanCapacityRefresh(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func loanCapacityDecision(t *testing.T, h *recoveryHarness, runner *Runner) (Port, Hull, GrowthDecision, map[string]int64, *growthEvidence) {
	t.Helper()
	evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
		t.Fatalf("capacity funding: %s, %v", reason, err)
	}
	input, reason, err := runner.capacityFleetInput(h.ctx, evidence)
	if err != nil || reason != "" {
		t.Fatalf("capacity economics: %s, %v", reason, err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
	input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	decision, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
	if err != nil {
		t.Fatal(err)
	}
	return port, hull, decision, required, evidence
}

func loanCapacityCashShortfall(t *testing.T, h *recoveryHarness, runner *Runner, shortfall int64) {
	t.Helper()
	_, _, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if !decision.Accepted {
		t.Fatalf("funded fixture lacks an economic capacity candidate: %+v", decision)
	}
	h.api.setVaultBalance("north", required["north_sea"]+decision.WorkingCapitalPence-shortfall)
	h.api.setVaultBalance("south", evidence.required["channel"])
	loanCapacityRefresh(t, h, runner)
}

func TestLoanCapacityBorrowBuildAndUnusedPrincipalRepayment(t *testing.T) {
	for _, refuseBuild := range []bool{false, true} {
		t.Run(fmt.Sprintf("yard_refuses_%t", refuseBuild), func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, refuseBuild)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			if decision.Accepted || !decision.EconomicallyViable {
				t.Fatalf("fixture must be blocked solely by cash: %+v", decision)
			}
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
				t.Fatalf("cash-only financing admission: changed %t, %v", changed, err)
			}
			mission, data := h.mission("__growth")
			if data.Capacity == nil || data.Capacity.Financing == nil || data.Action != nil {
				t.Fatalf("loan was not separated from construction: %+v", data)
			}
			quote := data.Capacity.Financing.Quotes[0]
			if quote.PrincipalPence != 100_000+2**quote.InstallmentPence || quote.PrincipalPence > 750_000 {
				t.Fatalf("principal is not the exact cash shortfall plus two installments: %+v", quote)
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("stage accepted quote: %t, %v", changed, err)
			}
			mission, data = h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "loan-accept" {
				t.Fatalf("capacity financing did not stage a separate loan journal action: %+v", data.Action)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("dispatch loan: %t, %v", changed, err)
			}
			loanCapacityRefresh(t, h, runner)
			mission, data = h.mission("__growth")
			if len(data.Capacity.Financing.Loans) != 1 || data.ProceedsPence != 0 || data.ExpensesPence != 0 {
				t.Fatalf("loan receipt missing or debt was counted as trading income: %+v", data)
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("stage financed build: %t, %v, holds %v", changed, err, runner.holding)
			}
			mission, data = h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("financed cash never reached normal construction: phase %s, action %+v", mission.Phase, data.Action)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("dispatch financed build: %t, %v", changed, err)
			}
			loanCapacityRefresh(t, h, runner)
			mission, data = h.mission("__growth")
			if !refuseBuild {
				if data.BuiltShipID != recoveryBuiltShipID || loanReserve(runner.stateCache.Loans[0]) != 2**quote.InstallmentPence {
					t.Fatalf("financed hull or installment protection missing: %+v", data)
				}
				return
			}
			if mission.Phase != "growth_finance_repay" || !time.Now().Before(data.CooldownUntil) {
				t.Fatalf("yard refusal lost its repayment phase or durable cooldown: phase %s, %+v", mission.Phase, data)
			}
			data.CooldownUntil = time.Time{}
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("stage unused-loan repayment: %t, %v", changed, err)
			}
			mission, data = h.mission("__growth")
			if data.Action == nil || data.Action.Stage != "loan-repay" {
				t.Fatalf("yard refusal stranded unused debt: %+v", data.Action)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("repay unused principal: %t, %v", changed, err)
			}
			loanCapacityRefresh(t, h, runner)
			mission, data = h.mission("__growth")
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("close canceled financed build: %t, %v", changed, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/accept-quote") != 1 || h.api.mutationCount("/v1/loan/repay") != 1 || runner.stateCache.Loans[0].OwedPence != 0 {
				t.Fatal("unused capacity debt was not repaid exactly once")
			}
		})
	}
}

func TestLoanCapacityTooSmallLienCreatesNoBorrowingIntent(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 100_000, false)
	loanCapacityCashShortfall(t, h, runner, 100_000)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
		t.Fatalf("lien cap financed only part of the debt-reserved package: %t, %v", changed, err)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/loan/accept-quote") != 0 {
		t.Fatal("insufficient full-package lien capacity borrowed anyway")
	}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
		t.Fatalf("refusal backoff created another borrowing intent: %t, %v", changed, err)
	}
	if h.api.mutationCount("/v1/loan/quote") != 1 || !time.Now().Before(decode[map[string]time.Time](runner.snapshot, "_growth_build_backoffs")[port.ID+"|"+hull.ID]) {
		t.Fatal("refused lien was repeatedly quoted without the yard/hull backoff")
	}
}

func TestLoanCapacityRejectsImpossiblePackagesBeforeQuoting(t *testing.T) {
	for _, name := range []string{"insufficient theoretical liens", "weak coverage", "gossip only", "remittance only"} {
		t.Run(name, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			switch name {
			case "insufficient theoretical liens":
				decision.CashShortfallPence = 750_000
			case "weak coverage":
				decision.DepthNetPencePerGameDay = 1
				runner.opts.CapacityPaybackDays = 1e9
			case "gossip only":
				decision.DepthNetPencePerGameDay = 0
			case "remittance only":
				decision.CashShortfallPence = 0
			}
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
				t.Fatalf("impossible %s package borrowed: %t, %v", name, changed, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/quote") != 0 {
				t.Fatalf("%s created unnecessary server quote objects", name)
			}
		})
	}
}

func TestLoanCapacityDoesNotSumSharedHouseFunds(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 450_000, false)
	ship := runner.stateCache.Ships[0]
	ship.Ship.ID = "second-collateral-ship"
	runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	decision := GrowthDecision{
		EconomicallyViable: true, CashShortfallPence: 800_000, QuotedHullPence: 2_000_000,
		IncrementalNetPencePerGameDay: 100_000, DepthNetPencePerGameDay: 100_000,
	}
	evidence := &growthEvidence{balances: map[string]int64{}, required: map[string]int64{}}
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, map[string]int64{}, evidence); err != nil || changed {
		t.Fatalf("two liens incorrectly multiplied shared house funds: %t, %v", changed, err)
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/loan/quote") != 1 || h.api.mutationCount("/v1/loan/accept-quote") != 0 {
		t.Fatal("shared-funds refusal did not stop before taking the first loan")
	}
}

func TestLoanCapacityRetainsFrozenAdmissionAcrossTemporaryChanges(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loanCapacityCashShortfall(t, h, runner, 100_000)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("admit financing: %t, %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage acceptance: %t, %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("accept financing: %t, %v", changed, err)
	}
	loanCapacityRefresh(t, h, runner)
	mission, data = h.mission("__growth")
	frozen := encode(data.Capacity.Financing.Decision)
	for _, key := range []string{"zones", "profile"} {
		meta := runner.meta[key]
		meta.Stale = true
		runner.meta[key] = meta
	}
	other := runner.stateCache.Ships[0]
	other.Ship.ID, other.Ship.Status, other.Voyage = "newly-idle-ship", "berthed", nil
	runner.stateCache.Ships = append(runner.stateCache.Ships, other)
	h.putMission("__underwriting", uuid.NewString(), "underwrite", missionData{Action: &durableAction{ID: "other-control-action"}})
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || changed || data.Capacity.Financing.Cancel {
		t.Fatalf("temporary control activity unwound admitted debt: %t, %v, %+v", changed, err, data.Capacity.Financing)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__underwriting"); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Ships = runner.stateCache.Ships[:1]
	var saved int64
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.BankingRegion == port.BankingRegion {
			saved, vault.BalancePence = vault.BalancePence, 0
		}
	}
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || changed || data.Capacity.Financing.Cancel {
		t.Fatalf("temporary regional cash shortage canceled financing: %t, %v, %+v", changed, err, data.Capacity.Financing)
	}
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence = saved
		}
	}
	market := runner.stateCache.Markets[port.ID]
	market.CustomsBPS++
	runner.stateCache.Markets[port.ID] = market
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("frozen admitted package did not proceed after a temporary hold: %t, %v, holds %v", changed, err, runner.holding)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil || data.Action.Stage != "build" || data.Capacity.Financing.Cancel || !bytes.Equal(encode(data.Capacity.Financing.Decision), frozen) ||
		h.api.mutationCount("/v1/loan/quote") != 1 || h.api.mutationCount("/v1/loan/accept-quote") != 1 || h.api.mutationCount("/v1/loan/repay") != 0 {
		t.Fatalf("temporary data, cash or other-ship changes caused churn: %+v", data)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("financed first-send reran cash-only admission: %v", err)
	}
}

func TestLoanCapacityReservationsAddOnlyGrowthAndDonorOutflows(t *testing.T) {
	t.Parallel()
	decision := GrowthDecision{QuotedHullPence: 100_000, WorkingCapitalPence: 20_000}
	evidence := &growthEvidence{
		balances: map[string]int64{"yard": 0, "donor": 150_000},
		required: map[string]int64{"yard": 10_000, "donor": 30_000},
	}
	_, protected, donors, funded := capacityLoanFunding(decision, evidence, "yard", 50_000, 10_000, []Rate{{FromRegion: "donor", ToRegion: "yard"}})
	if !funded || protected["yard"] != 110_000 || protected["donor"] != 20_000 || donors["donor"] != 70_000 {
		t.Fatalf("incremental regional financing package is wrong: protected %v, donors %v, %t", protected, donors, funded)
	}
	runner := &Runner{}
	reserved := map[string]int64{"yard": 10_000, "donor": 30_000}
	mission := &store.Mission{ShipID: "__growth", Phase: "growth_finance"}
	data := missionData{Capacity: &capacityIntent{Financing: &capacityFinancing{ProtectedByRegion: protected, DonorByRegion: donors}}}
	runner.reserveMissionCash(reserved, mission, &data, ShipState{})
	if reserved["yard"] != 120_000 || reserved["donor"] != 120_000 {
		t.Fatalf("growth double-reserved another mission or released donor outflow: %v", reserved)
	}
}

func TestLoanArrearsFundingPreemptsOrdinaryCargoAndOccupiedTreasury(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loan := snapshotTestLoan()
	loan.BankingRegion, loan.ArrearsPence = "channel", 1_000
	runner.stateCache.Loans, runner.snapshot["loans"] = []gameapi.Loan{loan}, encode([]gameapi.Loan{loan})
	hard, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		vault.BalancePence = hard[vault.BankingRegion]
		if vault.BankingRegion == "channel" {
			vault.BalancePence = hard[vault.BankingRegion] - loanReserve(loan) + loan.ArrearsPence - 1_000
		} else {
			vault.BalancePence += 10_000
		}
	}
	cooldown := time.Now().Add(time.Hour)
	h.putMission(recoveryShipBlastoise, uuid.NewString(), "buy", missionData{
		Plan:   Plan{Kind: "charter", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Quantity: 100_000, BuyLimitPence: 100},
		Action: &durableAction{ID: "ordinary-pending", Stage: "settings", Body: json.RawMessage(`{}`)}, CooldownUntil: cooldown,
	})
	h.putMission("__treasury", uuid.NewString(), "remit", missionData{
		Action: &durableAction{ID: "treasury-pending", Stage: "remit", Body: json.RawMessage(`{}`)}, CooldownUntil: cooldown,
	})
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("unresolved ordinary work blocked urgent arrears funding: %t, %v, holds %v", changed, err, runner.holding)
	}
	mission, data := h.mission("__loans")
	if data.Action == nil || data.Action.Stage != "remit" || data.LoanRepayment == nil || data.LoanRepayment.Before.ID != loan.ID {
		t.Fatalf("arrears did not get its own durable hard-obligation remittance: %+v", data)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("ordinary cargo reservations blocked arrears first-send funding: %v", err)
	}
	_, treasury := h.mission("__treasury")
	if treasury.Action == nil || treasury.Action.ID != "treasury-pending" {
		t.Fatal("priority debt servicing overwrote the occupied treasury journal")
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__loans"); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Loans[0].Status = "defaulted"
	if changed, err := runner.serviceLoans(h.ctx); err != nil || changed {
		t.Fatalf("defaulted recourse debt started a manual repayment loop: %t, %v", changed, err)
	}
	h.missingMission("__loans")
}

func loanCapacityAcceptedFixture(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loanCapacityCashShortfall(t, h, runner, 100_000)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("admit financing: %t, %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage acceptance: %t, %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("accept financing: %t, %v", changed, err)
	}
	loanCapacityRefresh(t, h, runner)
	return h, runner
}

func TestLoanArrearsCashCrunchStagesAndSendsCureInSameCycle(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loan := snapshotTestLoan()
	loan.BankingRegion, loan.ArrearsPence = "channel", 1_000
	runner.stateCache.Loans, runner.snapshot["loans"] = []gameapi.Loan{loan}, encode([]gameapi.Loan{loan})
	hard, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		vault.BalancePence = hard[vault.BankingRegion]
		if vault.BankingRegion == loan.BankingRegion {
			vault.BalancePence = max(0, hard[vault.BankingRegion]-loanReserve(loan)) + loan.ArrearsPence
			if vault.BalancePence >= hard[vault.BankingRegion] {
				t.Fatal("cash-crunch fixture can already cover two installments")
			}
		}
	}
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/repay" {
			base.ServeHTTP(writer, request)
			return
		}
		body, _ := io.ReadAll(request.Body)
		var payload gameapi.RepayLoanRequest
		mission, err := h.db.GetMission(request.Context(), h.account.ID, "__loans")
		var data missionData
		if request.Method != http.MethodPost || json.Unmarshal(body, &payload) != nil ||
			payload.LoanID != loan.ID || payload.AmountPence == nil || *payload.AmountPence != loan.ArrearsPence ||
			err != nil || mission == nil || json.Unmarshal(mission.Data, &data) != nil || data.Action == nil ||
			!bytes.Equal(data.Action.Body, body) || data.Action.ID != request.Header.Get("Idempotency-Key") {
			t.Error("arrears payment was not the exact durable named-loan request")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if journaled, err := h.db.HasAction(request.Context(), h.account.ID, data.Action.ID); err != nil || !journaled {
			t.Error("arrears payment reached the API before journal insertion")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		paid := loan
		paid.ArrearsPence = 0
		paid.PrincipalRepaidPence += loan.ArrearsPence
		paid.OutstandingPrincipalPence -= loan.ArrearsPence
		paid.OwedPence -= loan.ArrearsPence
		h.api.mu.Lock()
		h.api.record(request, body, http.StatusOK)
		h.api.mu.Unlock()
		h.api.writeJSON(writer, http.StatusOK, paid)
	})
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("cash-crunch arrears cure did not complete in one work cycle: %t, %v, holds %v", changed, err, runner.holding)
	}
	mission, data := h.mission("__loans")
	if mission.Phase != "done" || data.Action != nil || data.LoanRepayment != nil || data.LastActionID == "" ||
		h.api.mutationCount("/v1/loan/repay") != 1 || h.api.mutationCount("/v1/treasury/remit") != 0 || len(h.pending()) != 0 {
		t.Fatalf("cure waited for two installments or extra regional funding: phase %s, %+v", mission.Phase, data)
	}
}

func TestLoanArrearsFundingOnlyTargetsLoanRegion(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	loan := snapshotTestLoan()
	loan.BankingRegion = "channel"
	runner.stateCache.Rates = []Rate{{FromRegion: "north_sea", ToRegion: "baltic"}, {FromRegion: "north_sea", ToRegion: "channel"}}
	balances := map[string]int64{"north_sea": 10_000}
	required := map[string]int64{"north_sea": 1_000, "baltic": 1_000, "channel": 1_000}
	if changed, err := runner.fundTreasuryMission(h.ctx, balances, required, "__loans", &loanRepaymentIntent{Before: loan, AmountPence: 1_000}); err != nil || !changed {
		t.Fatalf("fund loan region: %t, %v", changed, err)
	}
	_, data := h.mission("__loans")
	var request struct {
		ToRegion string `json:"to_region"`
	}
	if data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.ToRegion != loan.BankingRegion {
		t.Fatalf("arrears funding serviced an unrelated short region first: %+v", data.Action)
	}
}

func TestLoanCapacityAcceptedDebtDeadlineRepaysBlockedBuild(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityAcceptedFixture(t)
	mission, data := h.mission("__growth")
	financing := data.Capacity.Financing
	gameDay := 24 * time.Hour / time.Duration(runner.stateCache.Clock.Calendar.TimeScale)
	if len(financing.Loans) != len(financing.Quotes) || financing.BuildBy.IsZero() ||
		!financing.BuildBy.Equal(financing.Loans[0].CreatedAt.Add(gameDay)) {
		t.Fatalf("first acceptance did not establish exactly one game day of idle debt: %+v", financing)
	}
	port, _ := findPort(runner.stateCache.Catalog, data.BuildPort)
	for index := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[index].BankingRegion == port.BankingRegion {
			runner.stateCache.Vaults[index].BalancePence -= 3
		}
	}
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || changed || financing.Cancel || data.Action != nil {
		t.Fatalf("temporary funding drift should hold until the deadline: %t, %v, %+v", changed, err, data)
	}
	financing.Loans[0].CreatedAt = time.Now().UTC().Add(-gameDay - time.Second)
	financing.BuildBy = financing.Loans[0].CreatedAt.Add(gameDay)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("one-game-day deadline did not stage repayment: %t, %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if !data.Capacity.Financing.Cancel || mission.Phase != "growth_finance_repay" || data.Action == nil || data.Action.Stage != "loan-repay" {
		t.Fatalf("accepted idle debt remained parked after the deadline: phase %s, %+v", mission.Phase, data)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || h.api.mutationCount("/v1/loan/repay") != 1 {
		t.Fatalf("deadline repayment could not send: %t, %v", changed, err)
	}
}

func TestLoanCapacityYardAuthorityLossImmediatelyRepays(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			h, runner := loanCapacityAcceptedFixture(t)
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/shipyard/quote" {
					h.api.writeProblem(writer, status, "no_port_presence", "yard presence was lost")
					return
				}
				base.ServeHTTP(writer, request)
			})
			mission, data := h.mission("__growth")
			if !time.Now().Before(data.Capacity.Financing.BuildBy) {
				t.Fatal("yard-loss fixture already reached the financing deadline")
			}
			if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("yard authority loss did not immediately stage repayment: %t, %v", changed, err)
			}
			mission, data = h.mission("__growth")
			if !data.Capacity.Financing.Cancel || mission.Phase != "growth_finance_repay" || data.Action == nil || data.Action.Stage != "loan-repay" {
				t.Fatalf("HTTP %d left accepted loans idle: phase %s, %+v", status, mission.Phase, data)
			}
		})
	}
}

func TestLoanCapacityWorkingBufferCanMoveAcrossRegions(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityAcceptedFixture(t)
	mission, data := h.mission("__growth")
	port, _ := findPort(runner.stateCache.Catalog, data.BuildPort)
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.BankingRegion == port.BankingRegion {
			vault.BalancePence -= 1_001
		} else if vault.BankingRegion == "channel" {
			vault.BalancePence += 1_000
		}
	}
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("fleet-wide buffer with one-penny drift was frozen to old regions: %t, %v, holds %v", changed, err, runner.holding)
	}
	mission, data = h.mission("__growth")
	if data.Action == nil || data.Action.Stage != "build" || data.Capacity.Financing.Cancel {
		t.Fatalf("buffer relocation canceled financed construction: %+v", data)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("build first-send still required frozen regional buffer shares: %v", err)
	}
}

func TestLoanCapacityServerInstallmentRoundingRequotesOnceWithoutBackoff(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityRoundingFixture(t, 750_000, false, math.Floor)
	loanCapacityCashShortfall(t, h, runner, 100_000)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	estimated, _ := estimatedCapacityPrincipal(decision.CashShortfallPence, 1)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
		t.Fatalf("server-rounded installments prevented financing: %t, %v", changed, err)
	}
	mission, data := h.mission("__growth")
	quote := data.Capacity.Financing.Quotes[0]
	needed := decision.CashShortfallPence + 2**quote.InstallmentPence
	if quote.PrincipalPence == estimated || quote.PrincipalPence < needed || quote.PrincipalPence-needed > 2 ||
		h.api.mutationCount("/v1/loan/quote") != 2 ||
		time.Now().Before(decode[map[string]time.Time](runner.snapshot, "_growth_build_backoffs")[port.ID+"|"+hull.ID]) {
		t.Fatalf("corrected principal, bounded requote or refusal backoff is wrong: quote %+v, estimated %d", quote, estimated)
	}
	if changed, err := runner.driveGrowth(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage corrected acceptance: %t, %v", changed, err)
	}
	mission, data = h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("accept corrected loan: %t, %v", changed, err)
	}
	_, data = h.mission("__growth")
	if len(data.Capacity.Financing.Loans) != 1 || data.Capacity.Financing.Cancel {
		t.Fatalf("correctly rounded server loan was not retained for construction: %+v", data.Capacity.Financing)
	}
}

func TestLoanCapacityFinancingRanksDepthInsteadOfGossip(t *testing.T) {
	t.Parallel()
	gossip := GrowthDecision{QuotedHullPence: 100_000, IncrementalNetPencePerGameDay: 1_000, DepthNetPencePerGameDay: 10}
	depth := GrowthDecision{QuotedHullPence: 100_000, IncrementalNetPencePerGameDay: 100, DepthNetPencePerGameDay: 100}
	if !betterCapacityCandidate(gossip, depth) || !betterCapacityFinancingCandidate(depth, gossip) || betterCapacityFinancingCandidate(gossip, depth) {
		t.Fatal("gossip-inclusive paper earnings still outrank debt-eligible depth")
	}
}

func TestLoanControlStatusIsNotADeletedShip(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.assessShip("__loans")
	if !runner.assessedHoldIDs["__loans"] || len(runner.assessedShipIDs) != 0 {
		t.Fatal("loan control was not assessed independently of the fleet")
	}
	if err := runner.hold(h.ctx, "__loans", "arrears funding awaits donor cash"); err != nil {
		t.Fatal(err)
	}
	runner.logBlockingStatus(h.ctx, time.Now().UTC(), true)
	if _, exists := runner.blockingStatuses["__loans"]; !exists {
		t.Fatal("fresh fleet capture deleted the loan control's blocking status")
	}
}
