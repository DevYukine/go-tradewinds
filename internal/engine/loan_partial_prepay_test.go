package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// partialPrepayAPI accepts partial principal repayments and records every
// repayment request, unlike moneyLoanAPI which only accepts full payoffs.
type partialPrepayAPI struct {
	mu     sync.Mutex
	h      *recoveryHarness
	base   http.Handler
	loans  []gameapi.Loan
	repays []gameapi.RepayLoanRequest
}

func (f *partialPrepayAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/v1/loan/list" {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.h.api.writeJSON(writer, http.StatusOK, f.loans)
		return
	}
	if request.Method == http.MethodPost && request.URL.Path == "/v1/loan/repay" {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			f.h.t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload gameapi.RepayLoanRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			f.h.t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		pending, err := f.h.db.PendingActions(request.Context(), f.h.account.ID)
		journaled := false
		if err == nil {
			for _, action := range pending {
				journaled = journaled || action.ID == request.Header.Get("Idempotency-Key") &&
					action.Method == request.Method && action.Path == request.URL.Path && bytes.Equal(action.Body, body)
			}
		}
		if !journaled {
			f.h.t.Error("prepayment reached the API without its immutable pending journal request")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		for i := range f.loans {
			loan := &f.loans[i]
			if loan.ID != payload.LoanID {
				continue
			}
			amount := loan.OwedPence
			if payload.AmountPence != nil {
				amount = *payload.AmountPence
			}
			if loan.IsMortgage || loan.Status != "active" || amount <= 0 || amount > loan.OwedPence {
				f.h.t.Errorf("repayment exceeded the observed outstanding balance: request=%s loan=%+v", body, loan)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			f.repays = append(f.repays, payload)
			interest := min(amount, max(int64(0), loan.AccruedInterestPence))
			loan.AccruedInterestPence -= interest
			principal := amount - interest
			loan.OutstandingPrincipalPence -= principal
			loan.PrincipalRepaidPence += principal
			loan.OwedPence -= amount
			if loan.OwedPence == 0 {
				loan.Status, loan.CollateralStatus = "repaid", "released"
				zero := int64(0)
				loan.NextPaymentPence = &zero
			}
			loan.UpdatedAt = time.Now().UTC()
			f.h.api.mu.Lock()
			f.h.api.vaultBalance["north"] -= amount
			f.h.api.record(request, body, http.StatusOK)
			f.h.api.mu.Unlock()
			f.h.api.writeJSON(writer, http.StatusOK, loan)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	// Reuse the recovery server unchanged, with the canonical banking regions
	// required by authoritative loan validation (as loanCapacityFixture does).
	if request.Method == http.MethodGet {
		recorder := httptest.NewRecorder()
		f.base.ServeHTTP(recorder, request)
		for key, values := range recorder.Header() {
			writer.Header()[key] = values
		}
		writer.WriteHeader(recorder.Code)
		body := strings.ReplaceAll(recorder.Body.String(), `"north"`, `"north_sea"`)
		body = strings.ReplaceAll(body, `"south"`, `"channel"`)
		_, _ = io.WriteString(writer, body)
		return
	}
	f.base.ServeHTTP(writer, request)
}

// partialPrepayFixture mirrors moneyPrepayFixture with a partial-tolerant loan
// API, then calibrates the loan region's balance so the reserve-safe surplus —
// balance minus every protected reserve including this loan's own
// two-installment reserve — equals targetSurplus exactly.
func partialPrepayFixture(t *testing.T, targetSurplus int64, withCheap bool) (*recoveryHarness, *Runner, *partialPrepayAPI) {
	t.Helper()
	h := newRecoveryHarness(t)
	fleet := make([]map[string]any, 0, 2)
	ships := []string{recoveryShipBlastoise, recoveryShipCharizard}
	count := 1
	if withCheap {
		count = 2
	}
	for i := range count {
		row := sailingShipRow(ships[i], fmt.Sprintf("partial-hoy-%d", i), recoveryHomePort, recoveryFarPort)
		row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
		fleet = append(fleet, row)
	}
	h.api.setFleet(fleet)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	h.api.mu.Lock()
	h.api.autoRemit, h.api.minReserve = true, 10_000_000
	h.api.mu.Unlock()
	api := &partialPrepayAPI{h: h, base: h.api.server.Config.Handler, loans: make([]gameapi.Loan, 0, 2)}
	specs := []struct {
		id           string
		rate         int32
		owed         int64
		outstanding  int64
		createdAt    time.Time
		collateralID string
	}{
		{id: "partial-goldsmiths-cheap", rate: 2000, owed: 300_000, outstanding: 298_000,
			createdAt: time.Now().UTC().Add(-72 * time.Hour), collateralID: recoveryShipBlastoise},
		{id: "partial-goldsmiths-dear", rate: 3000, owed: 563_785, outstanding: 561_367,
			createdAt: time.Now().UTC().Add(-48 * time.Hour), collateralID: recoveryShipCharizard},
	}
	if !withCheap {
		specs = specs[1:]
	}
	for _, spec := range specs {
		loan := snapshotTestLoan()
		loan.ID = spec.id
		loan.IsMortgage = false
		loan.AnnualRateBPS = &spec.rate
		loan.PrincipalPence = 746_952
		loan.OwedPence = spec.owed
		loan.OutstandingPrincipalPence = spec.outstanding
		loan.AccruedInterestPence = spec.owed - spec.outstanding
		loan.PrincipalRepaidPence = loan.PrincipalPence - spec.outstanding
		loan.InstallmentPence, loan.NextPaymentPence = new(int64(24_103)), new(int64(24_103))
		loan.NextPaymentDueAt = new(time.Now().UTC().Add(24 * time.Hour))
		loan.MaturesAt = new(time.Now().UTC().Add(72 * time.Hour))
		loan.CollateralShipID = new(spec.collateralID)
		loan.CreatedAt, loan.UpdatedAt = spec.createdAt, time.Now().UTC()
		api.loans = append(api.loans, loan)
	}
	h.api.server.Config.Handler = api
	for i := range 10 {
		persistGrowthWindowSettlement(t, h, store.Settlement{
			ID: fmt.Sprintf("partial-lot-%02d", i), Kind: "spot", CostsKnown: true,
			QuantityTons: 10, AcquisitionPence: 10_000, ProceedsPence: 0, GameSeconds: 86_400,
			CompletedAt: time.Now().UTC().Add(-time.Duration(20-i) * time.Minute),
		})
	}
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "partial-measured-closeout", Kind: "aborted", CostsKnown: true,
		ProceedsPence: 10_000_000, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-time.Hour),
	})
	runner := h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.snapshot["_capacity_growth_paused_until"] = encode(time.Now().Add(time.Hour))
	dear, exists := runner.loanByID("partial-goldsmiths-dear")
	if !exists || dear.Status != "active" {
		t.Fatalf("fixture lost its highest-rate loan: %+v", dear)
	}
	protected, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Optional repayment cannot use another loan's deferred operating capital.
	for _, loan := range runner.stateCache.Loans {
		if runner.loanServiceReserve(loan) == 0 {
			protected[loan.BankingRegion] += loanReserve(loan)
		}
	}
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		vault.BalancePence = protected[vault.BankingRegion]
		region := "south"
		if vault.BankingRegion == "north_sea" {
			region = "north"
		}
		h.api.setVaultBalance(region, vault.BalancePence)
	}
	surplusAt := func() int64 {
		input, ready, err := runner.loanPrepaymentInput(h.ctx, dear)
		if err != nil || !ready {
			t.Fatalf("prepayment input readiness: ready=%t err=%v", ready, err)
		}
		return input.Balances[dear.BankingRegion] - input.ReserveByRegion[dear.BankingRegion] - loanReserve(dear)
	}
	if delta := targetSurplus - surplusAt(); delta != 0 {
		for i := range runner.stateCache.Vaults {
			vault := &runner.stateCache.Vaults[i]
			if vault.BankingRegion == dear.BankingRegion {
				vault.BalancePence += delta
				h.api.setVaultBalance("north", vault.BalancePence)
			}
		}
	}
	if got := surplusAt(); got != targetSurplus {
		t.Fatalf("reserve-safe surplus calibration: got %d want %d", got, targetSurplus)
	}
	return h, runner, api
}

// planner.md D11: cash above every protected reserve in the loan region —
// including the loan's own two-installment reserve — prepays the highest-rate
// active non-mortgage loan by exactly that surplus in whole Crowns.
func TestMoneyLoanSurplusAboveReservesPrepaysWholeCrownSurplus(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 123_456, true)
	dear, exists := runner.loanByID("partial-goldsmiths-dear")
	if !exists {
		t.Fatal("fixture lost its highest-rate loan")
	}
	if loanReserve(dear)+123_456 >= loanFullPayment(dear) {
		t.Fatalf("fixture surplus must sit below the full payoff: reserve=%d full=%d", loanReserve(dear), loanFullPayment(dear))
	}
	h.api.mu.Lock()
	before := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	changed, err := runner.prepayLoans(h.ctx)
	if err != nil || !changed {
		t.Fatalf("reserve-safe surplus must stage a partial prepayment of the highest-rate loan: changed=%t err=%v holds=%v",
			changed, err, runner.holding)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.repays) != 1 || api.repays[0].LoanID != dear.ID {
		t.Fatalf("expected exactly one partial repayment of the highest-rate loan %s: %+v", dear.ID, api.repays)
	}
	if api.repays[0].AmountPence == nil || *api.repays[0].AmountPence != 123_400 {
		t.Fatalf("partial prepayment must send exactly the whole-crown surplus 123,400p (1,234 Cr): %+v", api.repays[0].AmountPence)
	}
	mission, data := h.mission("__loans")
	if mission == nil || data.LoanRepayment == nil || data.LoanRepayment.AmountPence != 123_400 || data.LoanRepayment.Before.ID != dear.ID {
		t.Fatalf("staged intent lost the frozen surplus amount: mission=%+v intent=%+v", mission, data.LoanRepayment)
	}
	h.api.mu.Lock()
	after := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	if after != before-123_400 {
		t.Fatalf("repayment spent more than the staged surplus: before=%d after=%d", before, after)
	}
	// The loan's own two-installment reserve (2x24,103p) and both working lots
	// (2x10,000p) must survive the partial repayment untouched.
	if after < 2*24_103+2*10_000 {
		t.Fatalf("partial repayment consumed the two-installment reserve or the working lots: %d", after)
	}
	if len(h.pending()) != 0 {
		t.Fatalf("partial repayment left unresolved journal rows: %v", h.pending())
	}
}

func TestMoneyLoanWithoutSurplusSendsNothing(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 0, false)
	changed, err := runner.prepayLoans(h.ctx)
	if err != nil || changed {
		t.Fatalf("free cash at the protected-reserve line must not prepay: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.repays) != 0 || h.api.mutationCount("/v1/loan/repay") != 0 || len(h.pending()) != 0 {
		t.Fatal("a fixture without reserve-safe surplus sent a repayment")
	}
	if mission, err := h.db.GetMission(h.ctx, h.account.ID, "__loans"); err != nil || mission != nil {
		t.Fatalf("a fixture without reserve-safe surplus staged a loan mission: mission=%+v err=%v", mission, err)
	}
}

func TestMoneyLoanBelowOneThousandCrownsSendsNothing(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 99_999, false)
	changed, err := runner.prepayLoans(h.ctx)
	if err != nil || changed {
		t.Fatalf("surplus under 1,000 Cr must not prepay: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.repays) != 0 || h.api.mutationCount("/v1/loan/repay") != 0 {
		t.Fatal("a sub-minimum surplus reached the repayment endpoint")
	}
}

func TestMoneyLoanFullPayoffStillRepaysWholeBalance(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 700_000, false)
	dear, exists := runner.loanByID("partial-goldsmiths-dear")
	if !exists {
		t.Fatal("fixture lost its loan")
	}
	h.api.mu.Lock()
	before := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	changed, err := runner.prepayLoans(h.ctx)
	if err != nil || !changed {
		t.Fatalf("surplus above the full payoff must still repay the whole balance: changed=%t err=%v holds=%v",
			changed, err, runner.holding)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.repays) != 1 || api.repays[0].LoanID != dear.ID {
		t.Fatalf("expected exactly one full repayment of %s: %+v", dear.ID, api.repays)
	}
	amount := dear.OwedPence
	if api.repays[0].AmountPence != nil {
		amount = *api.repays[0].AmountPence
	}
	if amount != 563_785 {
		t.Fatalf("full payoff spent %d, want the exact observed owed balance 563,785p", amount)
	}
	closed := api.loans[0]
	if closed.OwedPence != 0 || closed.Status != "repaid" {
		t.Fatalf("full payoff did not close the loan: %+v", closed)
	}
	h.api.mu.Lock()
	after := h.api.vaultBalance["north"]
	h.api.mu.Unlock()
	if after != before-amount {
		t.Fatalf("full payoff spent %d, vault moved %d -> %d", amount, before, after)
	}
	if len(h.pending()) != 0 {
		t.Fatalf("full payoff left unresolved journal rows: %v", h.pending())
	}
}
