package engine

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestLoanPrepayRanksDailyAndAnnualRates(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 123_456, true)
	for i := range runner.stateCache.Loans {
		loan := &runner.stateCache.Loans[i]
		if loan.ID == "partial-goldsmiths-cheap" {
			loan.RateBasis, loan.AnnualRateBPS, loan.DailyRateBPS = "daily", nil, 20
			api.loans[i] = *loan
		}
	}
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	if changed, err := runner.prepayLoans(h.ctx); err != nil || !changed {
		t.Fatalf("mixed-rate prepayment: changed=%t err=%v", changed, err)
	}
	if len(api.repays) != 1 || api.repays[0].LoanID != "partial-goldsmiths-cheap" || api.repays[0].AmountPence == nil || *api.repays[0].AmountPence != 123_400 {
		t.Fatalf("20 daily bps (7200 annual bps) must rank ahead of 3000 annual bps: %+v", api.repays)
	}
}

func TestLoanPrepaySkipsUnfundedHigherRateRegion(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 123_456, true)
	for i := range runner.stateCache.Loans {
		loan := &runner.stateCache.Loans[i]
		if loan.ID == "partial-goldsmiths-dear" {
			loan.BankingRegion = "channel"
			api.loans[i] = *loan
		}
	}
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	cheap, _ := runner.loanByID("partial-goldsmiths-cheap")
	input, _, err := runner.loanPrepaymentInput(h.ctx, cheap)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.BankingRegion == "channel" {
			vault.BalancePence = input.ReserveByRegion["channel"]
			h.api.setVaultBalance("channel", vault.BalancePence)
		}
	}
	dear, exists := runner.loanByID("partial-goldsmiths-dear")
	if !exists {
		t.Fatal("fixture lost its higher-rate loan")
	}
	dearInput, ready, err := runner.loanPrepaymentInput(h.ctx, dear)
	if err != nil || !ready {
		t.Fatalf("higher-rate region input: ready=%t err=%v", ready, err)
	}
	h.api.mu.Lock()
	channelCash := h.api.vaultBalance["channel"]
	h.api.mu.Unlock()
	if channelCash != dearInput.Balances["channel"] ||
		channelCash-dearInput.ReserveByRegion["channel"] >= loanFullPayment(dear) || loanPrepaymentSurplus(dearInput, dear) >= 100_000 {
		t.Fatalf("higher-rate loan's region must have neither full-payoff funds nor partial surplus: cash=%d input=%+v", channelCash, dearInput)
	}
	cheapInput, ready, err := runner.loanPrepaymentInput(h.ctx, cheap)
	if err != nil || !ready {
		t.Fatalf("lower-rate region input: ready=%t err=%v", ready, err)
	}
	want := loanPrepaymentSurplus(cheapInput, cheap) / 100 * 100
	if want != 171_600 {
		t.Fatalf("moving the expensive loan must preserve its full buffer in the newly funded region while releasing the cheap loan's own buffer for payoff: %d", want)
	}
	if changed, err := runner.prepayLoans(h.ctx); err != nil || !changed {
		t.Fatalf("funded lower-rate loan was starved: changed=%t err=%v", changed, err)
	}
	if len(api.repays) != 1 || api.repays[0].LoanID != cheap.ID || api.repays[0].AmountPence == nil || *api.repays[0].AmountPence != want {
		t.Fatalf("unfunded higher-rate debt blocked the other region's reserve-safe surplus: %+v", api.repays)
	}
}

func TestLoanPrepayRejectsInvalidRatesAndShrunkenDebt(t *testing.T) {
	for _, rate := range []int32{-1, 1_000_001, math.MaxInt32} {
		t.Run(string(encode(rate)), func(t *testing.T) {
			h, runner, api := partialPrepayFixture(t, 700_000, false)
			loan := &runner.stateCache.Loans[0]
			loan.AnnualRateBPS = &rate
			runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			if payment := loanFullPayment(*loan); payment != math.MaxInt64 {
				t.Fatalf("invalid rate produced a usable payoff buffer: %d", payment)
			}
			if changed, err := runner.prepayLoans(h.ctx); err != nil || changed || len(api.repays) != 0 {
				t.Fatalf("invalid rate admitted prepayment: changed=%t err=%v requests=%+v", changed, err, api.repays)
			}
		})
	}
	h, runner, _ := partialPrepayFixture(t, 123_400, false)
	loan := runner.stateCache.Loans[0]
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__loans", Phase: "loan_prepay", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
	if _, err := runner.stageLoanRepayment(h.ctx, mission, &data, loan, 123_400); err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Loans[0].OwedPence = 123_399
	if err := runner.loanActionReady(h.ctx, mission, &data); err == nil {
		t.Fatal("never-sent frozen partial payment exceeded current owed debt")
	}
}

func TestLoanPrepayPartialAmbiguousInstallmentCannotReconcile(t *testing.T) {
	h, runner, _ := partialPrepayFixture(t, 123_400, false)
	loan := runner.stateCache.Loans[0]
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__loans", Phase: "loan_prepay", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
	if _, err := runner.stageLoanRepayment(h.ctx, mission, &data, loan, 123_400); err != nil {
		t.Fatal(err)
	}
	original := *data.Action
	// Model a crash after sending the immutable request but before storing its receipt.
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body}); err != nil {
		t.Fatal(err)
	}
	loan.PrincipalRepaidPence += 150_000
	loan.OutstandingPrincipalPence -= 150_000
	loan.OwedPence -= 150_000
	runner.stateCache.Loans[0] = loan
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	data.CooldownUntil = time.Now().Add(time.Hour)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("unrelated installment reconciled ambiguous partial payment: changed=%t err=%v", changed, err)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != original.ID || !bytes.Equal(pending[0].Body, original.Body) || data.Action == nil || data.LoanRepayment == nil {
		t.Fatalf("ambiguous partial payment lost its identity or cash exposure: pending=%+v data=%+v", pending, data)
	}
	calls := 0
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			base.ServeHTTP(writer, request)
			return
		}
		calls++
		var body gameapi.RepayLoanRequest
		if request.Header.Get("Idempotency-Key") != original.ID || json.NewDecoder(request.Body).Decode(&body) != nil || body.AmountPence == nil || *body.AmountPence != 123_400 || body.LoanID != loan.ID {
			t.Error("partial replay lost its original idempotency key or payload")
		}
		h.api.writeJSON(writer, http.StatusOK, loan)
	})
	data.CooldownUntil = time.Time{}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || calls != 1 || len(h.pending()) != 0 {
		t.Fatalf("request-specific replay receipt did not resolve partial payment: changed=%t err=%v calls=%d pending=%v", changed, err, calls, h.pending())
	}
}

func TestLoanPrepayPartialDoneWaitsForFreshCapture(t *testing.T) {
	h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 123_456, false)
	loan := runner.stateCache.Loans[0]
	// Optional partial prepayment still keeps the full two-installment buffer,
	// even when scheduled service is deferred.
	buffer := loanReserve(loan) - runner.stateCache.loanServiceReserve(loan, runner.stateCache.Clock.RealUTC.Add(serverClockStaleness))
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.BankingRegion == loan.BankingRegion {
			vault.BalancePence += buffer
			h.api.setVaultBalance("north", vault.BalancePence)
		}
	}
	oldLoans := append([]gameapi.Loan(nil), runner.stateCache.Loans...)
	oldVaults := append([]Vault(nil), runner.stateCache.Vaults...)
	if changed, err := runner.prepayLoans(h.ctx); err != nil || !changed {
		t.Fatalf("partial prepayment: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__loans")
	if mission.Phase != "loan_prepay_done" || data.LoanRepayment == nil {
		t.Fatalf("partial receipt lost its fresh-capture fence: mission=%+v data=%+v", mission, data)
	}
	runner.stateCache.Loans, runner.stateCache.Vaults = oldLoans, oldVaults
	if changed, err := runner.prepayLoans(h.ctx); err != nil || changed || h.api.mutationCount("/v1/loan/repay") != 1 {
		t.Fatalf("pre-capture snapshot restaged partial payment: changed=%t err=%v", changed, err)
	}
	base := h.api.server.Config.Handler
	var debtCaptured, cashCaptured atomic.Bool
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" {
			debtCaptured.Store(true)
		}
		if request.URL.Path == "/v1/treasury/vaults" {
			cashCaptured.Store(true)
		}
		base.ServeHTTP(writer, request)
	})
	if changed, err := runner.serviceLoans(h.ctx); err != nil || !changed {
		t.Fatalf("partial done did not reconcile after fresh capture: changed=%t err=%v", changed, err)
	}
	if !debtCaptured.Load() || !cashCaptured.Load() {
		t.Fatal("partial done released its fence without capturing both debt and cash")
	}
	h.missingMission("__loans")
	if runner.stateCache.Loans[0].OwedPence != oldLoans[0].OwedPence-123_400 {
		t.Fatal("service released the fence without publishing fresh debt")
	}
	if changed, err := runner.prepayLoans(h.ctx); err != nil || changed || h.api.mutationCount("/v1/loan/repay") != 1 {
		t.Fatalf("fresh post-payment surplus restaged a payment: changed=%t err=%v", changed, err)
	}
}

func TestLoanPrepayPartialAmountCannotExceedObservedDebt(t *testing.T) {
	h, runner, api := partialPrepayFixture(t, 600_000, false)
	rate := int32(1_000_000)
	runner.stateCache.Loans[0].AnnualRateBPS = &rate
	api.loans[0] = runner.stateCache.Loans[0]
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	if changed, err := runner.prepayLoans(h.ctx); err != nil || !changed {
		t.Fatalf("bounded partial prepayment: changed=%t err=%v", changed, err)
	}
	if len(api.repays) != 1 || api.repays[0].AmountPence == nil || *api.repays[0].AmountPence != 563_700 {
		t.Fatalf("large interest buffer allowed partial amount above observed owed debt: %+v", api.repays)
	}
}

func TestLoanAnnualRateBounds(t *testing.T) {
	loan := snapshotTestLoan()
	for _, test := range []struct {
		basis string
		rate  int32
		valid bool
	}{
		{basis: "annual", rate: 0, valid: true},
		{basis: "annual", rate: 1_000_000, valid: true},
		{basis: "annual", rate: 1_000_001},
		{basis: "annual", rate: -1},
		{basis: "daily", rate: 2_777, valid: true},
		{basis: "daily", rate: 2_778},
		{basis: "daily", rate: math.MaxInt32},
		{basis: "daily", rate: -1},
	} {
		loan.RateBasis = test.basis
		loan.AnnualRateBPS, loan.DailyRateBPS = &test.rate, test.rate
		rate, valid := loanAnnualRate(loan)
		if valid != test.valid {
			t.Fatalf("%s rate %d normalized to %d with validity %t, want %t", test.basis, test.rate, rate, valid, test.valid)
		}
	}
}

func TestLoanFullPaymentZeroDebtNeedsNoRate(t *testing.T) {
	for _, basis := range []string{"", "annual", "daily", "unknown"} {
		loan := gameapi.Loan{RateBasis: basis, DailyRateBPS: -1}
		if payment := loanFullPayment(loan); payment != 0 {
			t.Fatalf("zero debt with rate basis %q produced payoff %d", basis, payment)
		}
	}
}

func TestLoanAmbiguousArrearsCureAndClosedPartialReconcileWithoutReplay(t *testing.T) {
	for _, phase := range []string{"loan_repay", "loan_prepay"} {
		t.Run(phase, func(t *testing.T) {
			h, runner, _ := partialPrepayFixture(t, 123_400, false)
			loan := runner.stateCache.Loans[0]
			if phase == "loan_repay" {
				loan.ArrearsPence = 123_400
			}
			mission := &store.Mission{AccountID: h.account.ID, ShipID: "__loans", Phase: phase, Generation: uuid.NewString()}
			data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
			if _, err := runner.stageLoanRepayment(h.ctx, mission, &data, loan, 123_400); err != nil {
				t.Fatal(err)
			}
			original := *data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body}); err != nil {
				t.Fatal(err)
			}
			loan.PrincipalRepaidPence += 123_400
			loan.OutstandingPrincipalPence -= 123_400
			loan.OwedPence -= 123_400
			if phase == "loan_prepay" {
				loan.Status, loan.OwedPence, loan.OutstandingPrincipalPence = "repaid", 0, 0
			}
			runner.stateCache.Loans[0] = loan
			runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			data.CooldownUntil = time.Now().Add(time.Hour)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if phase == "loan_repay" {
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || len(h.pending()) != 1 {
					t.Fatalf("counters reconciled an uncleared arrears balance: changed=%t err=%v", changed, err)
				}
				loan.ArrearsPence = 0
				runner.stateCache.Loans[0] = loan
				runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			}
			replayMission, replayData := h.mission("__loans")
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("authoritative debt evidence did not settle repayment: changed=%t err=%v", changed, err)
			}
			if changed, err := runner.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
				t.Fatalf("persisted reconciliation evidence failed its journal guard: changed=%t err=%v", changed, err)
			}
			currentMission, current := h.mission("__loans")
			wantPhase := "done"
			if phase == "loan_prepay" {
				wantPhase = "loan_prepay_done"
			}
			if currentMission.Phase != wantPhase || current.Action != nil || current.LastActionID != original.ID ||
				current.Sequence != 1 || len(h.pending()) != 0 || h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatalf("observed repayment replayed or lost its terminal journal identity: mission=%+v data=%+v pending=%v", currentMission, current, h.pending())
			}
		})
	}
}
