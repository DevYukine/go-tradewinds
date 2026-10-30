package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func mortgageTestTerms() *gameapi.MortgageTerms {
	installment := int64(2_300)
	return &gameapi.MortgageTerms{LenderCompanyID: "goldsmiths", LenderName: "Goldsmiths",
		PrincipalPence: 36_000, AnnualRateBPS: 3_819, TermGameDays: 180, Repayment: "amortised",
		Installments: 18, InstallmentPence: &installment, TotalRepayablePence: 41_400, Available: true}
}

func TestMortgageCapacityFinancedNetBoundaryAndCapital(t *testing.T) {
	t.Parallel()
	input := capacityGrowthAcceptanceInput()
	input.HullPricePence, input.InitialStoresPence = 60_000, 70_000
	input.Mortgage = mortgageTestTerms()
	input.MaxPaybackDays = 1_000_000 // Isolate the positive-net boundary from the separate payback gate.
	for _, net := range []float64{0, 1} {
		input.CandidateRatePencePerGameDay = float64(input.IncrementalDailyUpkeepPence) + 30 + net
		decision := EvaluateCapacityGrowth(input)
		if decision.Accepted != (net == 1) {
			t.Fatalf("financed net %.0fp/gd: %+v", net, decision)
		}
		if growthCapital(decision) != 160_000 || decision.SlippageReservePence != 6_000 || mortgageReserve(decision.Mortgage) != 4_600 {
			t.Fatalf("down payment, full-price slippage, guns, stores or debt reserve lost: %+v", decision)
		}
	}
	input.MaxPaybackDays = 420
	input.CandidateRatePencePerGameDay = float64(input.IncrementalDailyUpkeepPence) + 30 + 160_000.0/420
	if decision := EvaluateCapacityGrowth(input); !decision.Accepted {
		t.Fatalf("420-game-day financed payback rejected: %+v", decision)
	}
	input.CandidateRatePencePerGameDay = float64(input.IncrementalDailyUpkeepPence) + 30 + 160_000.0/420.01
	if decision := EvaluateCapacityGrowth(input); decision.Accepted {
		t.Fatalf("420.01-game-day financed payback admitted: %+v", decision)
	}
}

type mortgageFixture struct {
	h             *recoveryHarness
	base          http.Handler
	mu            sync.Mutex
	loans         []gameapi.Loan
	uninterpreted bool
	refuse        bool
	posted        bool
}

func newMortgageFixture(t *testing.T) (*recoveryHarness, *Runner, *mortgageFixture) {
	t.Helper()
	h, runner := loanCapacityFixture(t, 750_000, false)
	fixture := &mortgageFixture{h: h, base: h.api.server.Config.Handler}
	h.api.server.Config.Handler = fixture
	loanCapacityCashShortfall(t, h, runner, 20_000)
	return h, runner, fixture
}

func (f *mortgageFixture) mortgage(shipID string) gameapi.Loan {
	terms := mortgageTestTerms()
	loan := snapshotTestLoan()
	loan.ID, loan.IsMortgage, loan.CollateralShipID = "mortgage-built-hull", true, &shipID
	loan.LenderCompanyID, loan.PrincipalPence, loan.OutstandingPrincipalPence = terms.LenderCompanyID, terms.PrincipalPence, terms.PrincipalPence
	loan.TotalRepayablePence, loan.OwedPence, loan.InstallmentPence = &terms.TotalRepayablePence, terms.TotalRepayablePence, terms.InstallmentPence
	loan.TermGameDays, loan.AnnualRateBPS = terms.TermGameDays, &terms.AnnualRateBPS
	loan.NextPaymentPence = terms.InstallmentPence
	return loan
}

func (f *mortgageFixture) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/list" {
		f.mu.Lock()
		loans := append([]gameapi.Loan{}, f.loans...)
		f.mu.Unlock()
		f.h.api.writeJSON(writer, http.StatusOK, loans)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v1/shipyard/quote" {
		recorder := httptest.NewRecorder()
		f.base.ServeHTTP(recorder, request)
		var quote YardQuote
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &quote) != nil {
			writer.WriteHeader(recorder.Code)
			_, _ = writer.Write(recorder.Body.Bytes())
			return
		}
		quote.Mortgage = mortgageTestTerms()
		f.h.api.writeJSON(writer, http.StatusOK, quote)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/shipyard/build-ship" {
		f.base.ServeHTTP(writer, request)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		f.h.api.t.Error(err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var payload struct {
		PortID   string `json:"port_id"`
		Name     string `json:"name"`
		Class    string `json:"ship_class"`
		Mortgage bool   `json:"mortgage"`
	}
	if json.Unmarshal(body, &payload) != nil || !payload.Mortgage {
		f.base.ServeHTTP(writer, request)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.h.api.mu.Lock()
	defer f.h.api.mu.Unlock()
	if f.refuse {
		f.loans = []gameapi.Loan{f.mortgage(recoveryShipBlastoise)}
		f.h.api.record(request, body, http.StatusConflict)
		f.h.api.writeProblem(writer, http.StatusConflict, "mortgage_still_owed", "mortgage still owed")
		return
	}
	if !f.posted {
		row := stationaryShipRow(recoveryBuiltShipID, payload.Name, payload.PortID, "berthed", 0, 0)
		row["ship"].(map[string]any)["ship_class"] = payload.Class
		row["ship"].(map[string]any)["created_at"] = time.Now().UTC()
		f.h.api.fleet = append(f.h.api.fleet, row)
		f.h.api.vaultBalance["north"] -= 24_000
		f.loans = []gameapi.Loan{f.mortgage(recoveryBuiltShipID)}
		f.posted = true
	}
	f.h.api.record(request, body, http.StatusOK)
	if f.uninterpreted {
		f.h.api.writeJSON(writer, http.StatusOK, map[string]any{"committed_uninterpreted": true})
		return
	}
	f.h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": recoveryBuiltShipID, "name": payload.Name,
		"ship_class": payload.Class, "total_cost_pence": 60_000, "mortgage": f.loans[0]})
}

func TestMortgageCapacityBuildUsesImmutableFlagWithMeasuredClassNet(t *testing.T) {
	t.Parallel()
	h, runner, _ := newMortgageFixture(t)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if !decision.Accepted || decision.Mortgage == nil || required[port.BankingRegion] != evidence.required[port.BankingRegion]+growthCapital(decision)+4_600 {
		t.Fatalf("cash-blocked cheap hull did not use down payment and regional debt reserve: %+v %v", decision, required)
	}
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID); err != nil || !changed {
		t.Fatalf("stage cheap mortgage with measured class earnings: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if data.Capacity.Mortgage == nil || !bytes.Contains(data.Action.Body, []byte(`"mortgage":true`)) {
		t.Fatalf("mortgage absent from durable admission/body: %+v", data)
	}
	stagedBody, stagedKey := append([]byte(nil), data.Action.Body...), data.Action.ID
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("dispatch mortgage build: %t %v", changed, err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || !bytes.Equal(calls[0].Body, stagedBody) || calls[0].IdempotencyKey != stagedKey {
		t.Fatalf("mortgage transport lost its immutable journal body/key: %+v", calls)
	}
}

func TestMortgageCapacitySecondOpenMortgageRefused(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"active", "defaulted"} {
		t.Run(status, func(t *testing.T) {
			h, runner, fixture := newMortgageFixture(t)
			loan := fixture.mortgage(recoveryShipBlastoise)
			loan.Status, loan.CollateralStatus = status, "released"
			fixture.loans = []gameapi.Loan{loan}
			loanCapacityRefresh(t, h, runner)
			_, _, decision, _, _ := loanCapacityDecision(t, h, runner)
			if decision.Accepted || decision.Mortgage != nil || runner.mortgageAvailable() || !runner.mortgagedShip(recoveryShipBlastoise) {
				t.Fatalf("second %s mortgage admitted or released for another pledge: %+v", status, decision)
			}
		})
	}
}

func TestMortgageInstallmentIsRemittedBeforeDue(t *testing.T) {
	t.Parallel()
	h, runner, fixture := newMortgageFixture(t)
	loan := fixture.mortgage(recoveryShipBlastoise)
	future := time.Now().UTC().Add(time.Hour)
	loan.NextPaymentDueAt, loan.ArrearsPence = &future, 0
	fixture.loans = []gameapi.Loan{loan}
	h.api.setVaultBalance("north", 0)
	h.api.setVaultBalance("south", 50_000_000)
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.serviceLoans(h.ctx); err != nil || !changed {
		t.Fatalf("upcoming mortgage installment was not funded before arrears: %t %v", changed, err)
	}
	mission, data := h.mission("__loans")
	if data.Action == nil || data.Action.Stage != "remit" || !bytes.Contains(data.Action.Body, []byte(`"to_region":"north_sea"`)) {
		t.Fatalf("upcoming installment did not draw remote free cash: %+v", data.Action)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
		t.Fatalf("upcoming installment remittance could not be sent: %v", err)
	}
}

func TestMortgageAmbiguousBuildReconcilesFleetAndLoanAfterRestart(t *testing.T) {
	t.Parallel()
	h, runner, fixture := newMortgageFixture(t)
	fixture.uninterpreted = true
	_, _, _, _, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("stage mortgage: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	// The actual build may consume changed live mortgage terms. Its collateral
	// identity, not the earlier quote's principal, proves this committed build.
	fixture.mu.Lock()
	fixture.loans[0].PrincipalPence += 1_000
	fixture.loans[0].OutstandingPrincipalPence += 1_000
	fixture.mu.Unlock()
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	mission, data = h.mission("__growth")
	loans := runner.stateCache.Loans
	fixture.loans = []gameapi.Loan{}
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.reconcileCommitted(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("fleet alone reconciled mortgage: %t %v", changed, err)
	}
	fixture.loans = loans
	loanCapacityRefresh(t, h, runner)
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("restart reconcile: %t %v", changed, err)
	}
	_, data = h.mission("__growth")
	if data.BuiltShipID != recoveryBuiltShipID || data.Action != nil || data.CostsKnown || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatalf("mortgage reconciliation replayed a build or lost its hull: %+v", data)
	}
}

func TestMortgageStillOwedConflictWaitsForAuthoritativeLoans(t *testing.T) {
	t.Parallel()
	h, runner, fixture := newMortgageFixture(t)
	fixture.refuse = true
	_, _, _, _, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("stage: %t %v", changed, err)
	}
	mission, data := h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume 409: %t %v", changed, err)
	}
	if runner.mortgageAvailable() || decode[time.Time](runner.snapshot, "_mortgage_refused_at").IsZero() {
		t.Fatal("409 did not latch authoritative loan refresh")
	}
	loanCapacityRefresh(t, h, runner)
	for range 3 {
		if runner.mortgageAvailable() {
			t.Fatal("open mortgage permitted a retry")
		}
	}
	if h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatal("mortgage 409 caused a retry storm")
	}
}

func TestMortgageDoesNotReplaceAffordableCashForPaybackAlone(t *testing.T) {
	t.Parallel()
	h, runner, _ := newMortgageFixture(t)
	h.api.setVaultBalance("north", 1_000_000_000)
	loanCapacityRefresh(t, h, runner)
	_, _, cash, _, _ := loanCapacityDecision(t, h, runner)
	if !cash.Accepted || cash.Mortgage != nil {
		t.Fatalf("fully funded build did not prefer cash: %+v", cash)
	}
	financedPayback := float64(growthCapital(cash)-mortgageTestTerms().PrincipalPence) /
		(cash.IncrementalNetPencePerGameDay - mortgageInterestPerGameDay(mortgageTestTerms()))
	if financedPayback >= cash.PaybackGameDays {
		t.Fatal("fixture does not isolate improved financed payback")
	}
	runner.opts.CapacityPaybackDays = (cash.PaybackGameDays + financedPayback) / 2
	_, _, held, _, _ := loanCapacityDecision(t, h, runner)
	if held.Accepted || held.Mortgage != nil {
		t.Fatalf("mortgage introduced leverage without a binding or delaying cash constraint: %+v", held)
	}
}

func TestMortgageCollateralCannotBePledgedOrListed(t *testing.T) {
	t.Parallel()
	h, runner, fixture := newMortgageFixture(t)
	loan := fixture.mortgage(recoveryShipBlastoise)
	loan.CollateralStatus = "released"
	fixture.loans = []gameapi.Loan{loan}
	loanCapacityRefresh(t, h, runner)
	port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
	if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || changed {
		t.Fatalf("mortgaged collateral offered to a second lender: %t %v", changed, err)
	}
	ship, _ := runner.ship(recoveryShipBlastoise)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: "mortgage-list", Phase: "listing"}
	data := missionData{}
	if err := runner.stage(h.ctx, mission, &data, ship, "list-ship", "/v1/fleet/list-ship", map[string]any{"ship_id": ship.Ship.ID, "price_pence": 100_000}); err != nil {
		t.Fatal(err)
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "mortgaged ship cannot be listed or sold") {
		t.Fatalf("mortgaged hull could be listed: %v", err)
	}
	if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/fleet/list-ship") != 0 {
		t.Fatal("mortgaged collateral reached a transfer endpoint")
	}
}
