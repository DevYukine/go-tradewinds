package engine

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacityDebtOwnCashAndCoveragePennyBoundaries(t *testing.T) {
	decision := GrowthDecision{WorkingCapitalPence: 60_000, CandidateLotCashPence: 10_000}
	for _, delta := range []int64{-1, 0, 1} {
		if got := capacityDebtCashReady(decision, 50_000+delta); got != (delta >= 0) {
			t.Fatalf("own-cash threshold %+dp: got %t", delta, got)
		}
		if got := capacityHalfNetCoverage(1_000, 4_000+delta); got != (delta <= 0) {
			t.Fatalf("half-net ten-day installment threshold %+dp: got %t", delta, got)
		}
	}
	principal, installment := estimatedPlayerPrincipal(100_000)
	if principal != 106_900 || installment != 3_450 {
		t.Fatalf("rounded funded principal = %dp, installment %dp", principal, installment)
	}
	for _, delta := range []int64{-1, 0, 1} {
		p := principal + delta
		i := int64(math.Ceil(float64(p) * capacityAnnuityFactor(3_000)))
		funded := p >= 100_000+2*i
		if funded != (delta >= 0) {
			t.Fatalf("smallest funded principal %+dp: principal %d installment %d", delta, p, i)
		}
	}
	decision.IncrementalNetPencePerGameDay = 1_000
	decision.Mortgage = &gameapi.MortgageTerms{
		PrincipalPence: 100_000, TotalRepayablePence: 100_000, TermGameDays: 180,
		InstallmentPence: new(int64(4_000)),
	}
	for _, delta := range []int64{-1, 0, 1} {
		*decision.Mortgage.InstallmentPence = 4_000 + delta
		if got := capacityPackageCoverage(decision, 4_000); got != (delta <= 0) {
			t.Fatalf("combined mortgage-plus-loan DSCR %+dp: got %t", delta, got)
		}
	}
	for _, net := range []float64{math.NaN(), math.Inf(1), 0, -1} {
		if capacityHalfNetCoverage(net, 1) {
			t.Fatalf("nonpositive or nonfinite net admitted: %v", net)
		}
	}
}

func TestCapacityDebtExposurePennyBoundaryIncludesMortgage(t *testing.T) {
	decision := GrowthDecision{Mortgage: &gameapi.MortgageTerms{PrincipalPence: 100_000}}
	for _, delta := range []int64{-1, 0, 1} {
		if got := capacityDebtExposure(decision, 250_000+delta, 1_000_000, nil, nil); got != (delta <= 0) {
			t.Fatalf("gross debt cap %+dp: got %t", delta, got)
		}
	}
	if capacityDebtExposure(decision, 1, 0, nil, nil) {
		t.Fatal("unknown net worth admitted debt")
	}
}

func TestCapacityDeployedLotsRemoveOnlyAlreadyFundedLots(t *testing.T) {
	input := capacityGrowthAcceptanceInput()
	input.FleetSize, input.LotCashPence, input.CandidateLotCashPence = 3, 10_000, 12_000
	input.Mortgage = nil
	allCash := EvaluateCapacityGrowth(input)
	input.DeployedLots = 3
	loaded := EvaluateCapacityGrowth(input)
	if allCash.WorkingCapitalPence-loaded.WorkingCapitalPence != 30_000 || loaded.CandidateLotCashPence != 12_000 {
		t.Fatalf("deployed incumbents lost the candidate lot or retained a second lot: empty %+v loaded %+v", allCash, loaded)
	}
	input.DeployedLots = 4
	if got := EvaluateCapacityGrowth(input); got.Accepted || got.EconomicallyViable {
		t.Fatalf("impossible deployed-lot count admitted: %+v", got)
	}
}

func TestCapacityFleetCountsCargoAndReservedMissionLotsOnce(t *testing.T) {
	h, runner := loanCapacityFixture(t, 750_000, false)
	first := runner.stateCache.Ships[0]
	first.CargoUsedTons = 1
	second := first
	second.Ship.ID, second.CargoUsedTons = recoveryShipCharizard, 0
	third := first
	third.Ship.ID, third.CargoUsedTons = "00000000-0000-7000-8000-000000000003", 0
	runner.stateCache.Ships = []ShipState{first, second, third}
	h.putMission(second.Ship.ID, "reserved-lot", "provision", missionData{
		Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Quantity: 1, BuyLimitPence: 100},
	})
	input, reason, err := runner.capacityFleetInput(h.ctx, &growthEvidence{asOf: time.Now().UTC()})
	if err != nil || reason != "" || input.FleetSize != 3 || input.DeployedLots != 2 {
		t.Fatalf("cargo/reserved/free lot classification: %+v, %q, %v", input, reason, err)
	}
}

func TestPlayerLoanQuoteIdentityUsesRequestAndPinnedCollateral(t *testing.T) {
	requestID, quoteID, shipID := "request-1", "quote-1", "ship-1"
	quote := gameapi.LoanQuote{ID: quoteID, RequestID: &requestID}
	loan := gameapi.Loan{ID: "loan-1", QuoteID: &quoteID, RequestID: &requestID}
	if !loanQuoteIdentity(loan, quote) {
		t.Fatal("unsecured player request did not match its exact quote and request")
	}
	// The wire quote has null collateral. Approval pins the request's ship in
	// the durable quote before any send, so restart can check the actual lien.
	wire := quote
	quote.CollateralShipID, loan.CollateralShipID = &shipID, &shipID
	if wire.CollateralShipID != nil || !loanQuoteIdentity(loan, quote) {
		t.Fatal("player quote with a null wire lien lost its request-specific pledge")
	}
	for _, field := range []string{"request", "quote", "ship"} {
		bad := loan
		wrong := "wrong"
		switch field {
		case "request":
			bad.RequestID = &wrong
		case "quote":
			bad.QuoteID = &wrong
		case "ship":
			bad.CollateralShipID = &wrong
		}
		if loanQuoteIdentity(bad, quote) {
			t.Fatalf("different %s was matched to player debt", field)
		}
	}
	quote.RequestID, loan.RequestID = nil, nil
	if !loanQuoteIdentity(loan, quote) {
		t.Fatal("Goldsmiths quote-specific lien rule changed")
	}
}

func TestOceanicCapacityHamburgWorkedCapitalAndOwnCashGate(t *testing.T) {
	// Captured Hamburg barque terms from KrakenLoanResearch: unavailable while
	// Charmander's mortgage is open. Availability is flipped only to isolate
	// the hypothetical own-cash gate; this is not a live admission claim.
	mortgage := &gameapi.MortgageTerms{
		Available: false, LenderCompanyID: "01a170be-0b61-707a-8b45-363ab5002952",
		PrincipalPence: 17_195_116, AnnualRateBPS: 3_819,
		TermGameDays: 180, Repayment: "amortised", Installments: 18,
		InstallmentPence: new(int64(1_054_435)), TotalRepayablePence: 18_979_812,
	}
	input := CapacityGrowthInput{
		Hull:           Hull{ID: "barque", HullCategory: "oceanic", HoldCapacityTons: 240, MaxCannons: 14},
		HullPricePence: 21_493_896, SurplusPence: 3_600_000,
		FleetSize: 5, DeployedLots: 5, LotCashPence: 1_000_000,
		CandidateRatePencePerGameDay: 200_000, MeasuredRatePencePerGameDay: 200_000,
		MeasuredNetAvailable: true, MaxPaybackDays: 420, HasSpareWork: true, Mortgage: mortgage,
	}
	if got := EvaluateCapacityGrowth(input); got.Verdict != "mortgage_unavailable" {
		t.Fatalf("occupied mortgage slot was not explicit: %+v", got)
	}
	mortgage.Available = true
	decision := EvaluateCapacityGrowth(input)
	if growthCapital(decision) != 6_508_170 || decision.WorkingCapitalPence-decision.CandidateLotCashPence != 5_332_955 {
		t.Fatalf("Hamburg price, 80%% mortgage, 10%% slippage, guns or 53,329.55Cr own buffer changed: %+v", decision)
	}
	if !decision.EconomicallyViable || decision.Accepted || capacityDebtCashReady(decision, input.SurplusPence) {
		t.Fatalf("36,000Cr own cash funded the researched barque buffer: %+v", decision)
	}
	if capped := capacityEstimatedExposure(decision, 23_862_876); capped.EconomicallyViable || capped.Verdict != "zone_exposure" {
		t.Fatalf("237,032.86Cr gross hull exposure escaped the 83,520.06Cr cap: %+v", capped)
	}
}

func TestPlayerLoanLedgerPrincipalAndRepaymentDoNotCreateMeasuredIncome(t *testing.T) {
	h, runner := loanCapacityFixture(t, 750_000, false)
	asOf := time.Now().UTC()
	beforeSavings, err := runner.capacitySavingsRate(h.ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	beforeFleet, reason, err := runner.capacityFleetInput(h.ctx, &growthEvidence{asOf: asOf})
	if err != nil || reason != "" {
		t.Fatalf("measured fleet fixture: %s, %v", reason, err)
	}
	beforeHistory, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/treasury/ledger" {
			h.api.writeJSON(w, http.StatusOK, []map[string]any{
				{"id": "loan-disbursement", "reason": "loan_disbursed", "amount_pence": 10_000_000, "banking_region": "north_sea", "created_at": asOf.Add(-time.Minute)},
				{"id": "loan-payment", "reason": "loan_repayment", "amount_pence": -2_000_000, "banking_region": "north_sea", "created_at": asOf.Add(-time.Second)},
			})
			return
		}
		base.ServeHTTP(w, req)
	})
	loanCapacityRefresh(t, h, runner)
	afterSavings, err := runner.capacitySavingsRate(h.ctx, asOf)
	if err != nil {
		t.Fatal(err)
	}
	afterFleet, reason, err := runner.capacityFleetInput(h.ctx, &growthEvidence{asOf: asOf})
	if err != nil || reason != "" {
		t.Fatalf("measured fleet after financing ledger: %s, %v", reason, err)
	}
	afterHistory, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if beforeSavings != afterSavings || beforeFleet.MeasuredRatePencePerGameDay != afterFleet.MeasuredRatePencePerGameDay || string(encode(beforeHistory)) != string(encode(afterHistory)) {
		t.Fatalf("financing ledger became savings, measured earnings or settlement profit: savings %f/%f, fleet %f/%f", beforeSavings, afterSavings, beforeFleet.MeasuredRatePencePerGameDay, afterFleet.MeasuredRatePencePerGameDay)
	}
	var ledger []json.RawMessage
	if json.Unmarshal(runner.snapshot["ledger"], &ledger) != nil || len(ledger) != 2 {
		t.Fatal("financing ledger fixture was not captured")
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", store.MaxHistoryLimit)
	if err != nil || len(rows) != 2 {
		t.Fatalf("financing ledger invented trading settlements: %+v, %v", rows, err)
	}
}

func TestPlayerLoanRequestAuthorityRejectsIncompleteOrForeignRows(t *testing.T) {
	shipID := recoveryShipBlastoise
	now := time.Now().UTC()
	valid := gameapi.LoanRequestView{
		ID: "request-one", BorrowerCompanyID: recoveryCompanyID, BorrowerName: "Borrower",
		BorrowerRecord: json.RawMessage(`{}`), BankingRegion: "north_sea", PrincipalPence: 106_900,
		TermGameDays: 360, Repayment: "amortised", Recourse: true, AutoQuote: true,
		CollateralKind: "ship_lien", CollateralShipID: &shipID, GoldsmithsRateBPS: 3_000,
		RateFloorBPS: 300, RateCeilingBPS: 9_000, Status: "open",
		CreatedAt: now, ExpiresAt: now.Add(24 * time.Minute), Quotes: []gameapi.LoanQuote{},
	}
	if err := validateLoanRequests(encode([]gameapi.LoanRequestView{valid}), recoveryCompanyID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"null", "foreign borrower", "unknown status", "null quotes", "missing rate", "null row", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			var fields map[string]any
			if err := json.Unmarshal(encode(valid), &fields); err != nil {
				t.Fatal(err)
			}
			raw := encode([]any{fields})
			switch name {
			case "null":
				raw = json.RawMessage(`null`)
			case "foreign borrower":
				fields["borrower_company_id"] = "another-company"
			case "unknown status":
				fields["status"] = "unrecognized"
			case "null quotes":
				fields["quotes"] = nil
			case "missing rate":
				delete(fields, "goldsmiths_rate_bps")
			case "null row":
				raw = json.RawMessage(`[null]`)
			case "duplicate":
				raw = encode([]any{fields, fields})
			}
			if name == "foreign borrower" || name == "unknown status" || name == "null quotes" || name == "missing rate" {
				raw = encode([]any{fields})
			}
			if err := validateLoanRequests(raw, recoveryCompanyID); err == nil {
				t.Fatalf("incomplete own request authority accepted: %s", raw)
			}
		})
	}
}

func TestPlayerLoanForecastCoverageCannotOverrideRealizedCash(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	port, hull, forecast, required, evidence := loanCapacityDecision(t, h, runner)
	principal, installment := estimatedPlayerPrincipal(growthCashTotal(forecast.CashShortfallPence, runner.capacityLoanHeadroom(forecast)))
	if !evaluateCapacityLoan(forecast, forecast.CashShortfallPence, math.MaxInt64, principal, installment, 3_000).Borrow {
		t.Fatal("fixture's executable depth forecast does not pass coverage")
	}
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "realized-spot-loss", Kind: "aborted", CostsKnown: true,
		ExpensesPence: 1_980_200, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-time.Minute),
	})
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, forecast, required, evidence); err != nil || changed {
		t.Fatalf("forecast overrode realized debt coverage: changed %t, error %v", changed, err)
	}
	h.missingMission("__growth")
	if len(h.pending()) != 0 || h.api.mutationCount("/v1/loan/request") != 0 || h.api.mutationCount("/v1/loan/quote") != 0 {
		t.Fatal("failed realized coverage still created a debt request or quote")
	}
	detail := playerLoanAcceptanceDecision(t, h, "dscr")
	if detail["verdict"] != "dscr" {
		t.Fatalf("realized coverage refusal lost its gate: %+v", detail)
	}
}

func TestCapacityDebtMeasurementRejectsStaleClassAndUsesFreshFleetOnlyWithoutHistory(t *testing.T) {
	h, runner := playerLoanAcceptanceFixture(t)
	now := time.Now().UTC()
	if rate, _, available, err := runner.capacityDebtMeasuredRate(h.ctx, recoveryHull, now); err != nil || !available || rate <= 0 {
		t.Fatalf("fresh known class settlement not measured: %f, %t, %v", rate, available, err)
	}
	window := time.Duration(float64(capacityMeasuredGameDays) * float64(24*time.Hour) / float64(runner.stateCache.Clock.Calendar.TimeScale))
	if _, _, available, err := runner.capacityDebtMeasuredRate(h.ctx, recoveryHull, now.Add(window+time.Hour)); err != nil || available {
		t.Fatalf("stale class settlement used for recourse debt: %t, %v", available, err)
	}
	if rate, basis, available, err := runner.capacityDebtMeasuredRate(h.ctx, "barque", now); err != nil || !available || rate <= 0 || basis != "unmeasured class: min(depth-plan rate, measured fleet rate)" {
		t.Fatalf("history-free class did not use fresh fleet cohort: %f, %q, %t, %v", rate, basis, available, err)
	}
	meta := runner.meta["ships"]
	meta.Stale = true
	runner.meta["ships"] = meta
	if _, _, available, err := runner.capacityDebtMeasuredRate(h.ctx, recoveryHull, now); err != nil || available {
		t.Fatalf("stale hull cohort authorized measured debt: %t, %v", available, err)
	}
}
