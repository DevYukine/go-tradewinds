package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func loanJournalCapture(t *testing.T, runner *Runner, loans []gameapi.Loan) {
	t.Helper()
	raw := encode(loans)
	if err := validateBorrowerLoans(raw, runner.account.CompanyID); err != nil {
		t.Fatalf("invalid test borrower authority: %v", err)
	}
	now := time.Now().UTC()
	runner.generation = uuid.NewString()
	runner.snapshot["loans"] = raw
	runner.stateCache.Loans = loans
	runner.meta["loans"] = DatasetMeta{
		AttemptedAt: now, UpdatedAt: now, Generation: runner.generation, Complete: true,
	}
	if !runner.loanAuthority() {
		t.Fatal("complete current raw borrower list did not establish loan authority")
	}
}

// Resume an already-journaled request and lose its outcome to an in-flight
// response. The mission and immutable journal survive independently of Runner.
func loanJournalAmbiguousAction(t *testing.T, stage string) (*recoveryHarness, *Runner, gameapi.Loan, *atomic.Int64) {
	t.Helper()
	h := newRecoveryHarness(t)
	loan := snapshotTestLoan()
	quoteID := "capacity-quote-one"
	loan.QuoteID = &quoteID
	quote := gameapi.LoanQuote{
		ID: quoteID, LenderCompanyID: loan.LenderCompanyID, BorrowerCompanyID: loan.BorrowerCompanyID,
		BankingRegion: loan.BankingRegion, PrincipalPence: loan.PrincipalPence,
		AnnualRateBPS: *loan.AnnualRateBPS, TermGameDays: loan.TermGameDays, Repayment: loan.Repayment,
		TotalRepayablePence: *loan.TotalRepayablePence, InstallmentPence: loan.InstallmentPence,
		CollateralShipID: loan.CollateralShipID, Status: "open", ExpiresAt: time.Now().Add(time.Hour),
	}
	var sends atomic.Int64
	var actionID string
	actionLogIntercept(t, h, func(writer http.ResponseWriter, request *http.Request) {
		sends.Add(1)
		if request.Method != http.MethodPost || request.Header.Get("Idempotency-Key") != actionID {
			t.Errorf("loan retry lost its immutable method or action key: %s %s", request.Method, request.Header.Get("Idempotency-Key"))
		}
		if stage == "loan-accept" {
			var body gameapi.QuoteActionRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || request.URL.Path != "/v1/loan/accept-quote" || body.QuoteID != quoteID {
				t.Errorf("loan acceptance sent a different quote: path %s body %+v error %v", request.URL.Path, body, err)
			}
		} else {
			var body gameapi.RepayLoanRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || request.URL.Path != "/v1/loan/repay" || body.LoanID != loan.ID || body.AmountPence == nil || *body.AmountPence != loan.OwedPence {
				t.Errorf("loan repayment sent a different debt or amount: path %s body %+v error %v", request.URL.Path, body, err)
			}
		}
		h.api.writeProblem(writer, http.StatusConflict, "idempotency_request_in_flight", "loan outcome is not yet known")
	})
	runner := h.runner(primaryTestKey)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__growth", Phase: "growth_finance", Generation: uuid.NewString()}
	data := missionData{
		Plan: Plan{Kind: "growth"}, CostsKnown: true,
		AcquisitionPence: 1100, ProceedsPence: 1700, FeesPence: 20, ProvisionsPence: 30, ExpensesPence: 40,
		Capacity: &capacityIntent{Financing: &capacityFinancing{
			Quotes: []gameapi.LoanQuote{quote}, ProtectedByRegion: map[string]int64{"north_sea": 90_000},
		}},
	}
	if stage == "loan-accept" {
		loanJournalCapture(t, runner, []gameapi.Loan{})
		if err := runner.stage(h.ctx, mission, &data, ShipState{}, stage, "/v1/loan/accept-quote", gameapi.QuoteActionRequest{QuoteID: quote.ID}); err != nil {
			t.Fatal(err)
		}
	} else {
		loanJournalCapture(t, runner, []gameapi.Loan{loan})
		mission.Phase = "growth_finance_repay"
		data.Capacity.Financing.Loans = []gameapi.Loan{loan}
		if changed, err := runner.stageLoanRepayment(h.ctx, mission, &data, loan, loan.OwedPence); err != nil || !changed {
			t.Fatalf("stage repayment: changed %t error %v", changed, err)
		}
	}
	actionID = data.Action.ID
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: actionID, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body,
	}); err != nil {
		t.Fatal(err)
	}
	mission, data = h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("ambiguous loan outcome advanced: changed %t error %v", changed, err)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != actionID || pending[0].State != store.ActionPending || sends.Load() != 1 {
		t.Fatalf("ambiguous action lost its durable request: pending %+v sends %d", pending, sends.Load())
	}
	mission, data = h.mission("__growth")
	data.CooldownUntil = time.Now().UTC().Add(time.Hour)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	return h, runner, loan, &sends
}

func loanJournalRequireAccounting(t *testing.T, h *recoveryHarness, data missionData) {
	t.Helper()
	if data.AcquisitionPence != 1100 || data.ProceedsPence != 1700 || data.FeesPence != 20 || data.ProvisionsPence != 30 || data.ExpensesPence != 40 || data.Settlement != nil {
		t.Fatalf("loan principal or debt service changed trading accounting: %+v", data)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("financing invented trading settlements: %+v", rows)
	}
}

func TestLoanAcceptAmbiguousOutcomeReconcilesExactQuoteOnceOnReplay(t *testing.T) {
	h, _, loan, sends := loanJournalAmbiguousAction(t, "loan-accept")
	runner := h.runner(primaryTestKey)
	unrelated := loan
	unrelated.ID = "unrelated-loan"
	otherQuoteID := "other-quote"
	unrelated.QuoteID = &otherQuoteID
	loanJournalCapture(t, runner, []gameapi.Loan{unrelated, loan})
	mission, data := h.mission("__growth")
	replayMission, replayData := h.mission("__growth")
	action := *data.Action
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("unique authoritative quote did not reconcile: changed %t error %v", changed, err)
	}
	persisted, current := h.mission("__growth")
	if persisted.Phase != "growth_finance" || current.Action != nil || current.LastActionID != action.ID || current.Sequence != 1 {
		t.Fatalf("acceptance did not durably consume exactly one action: phase %s data %+v", persisted.Phase, current)
	}
	if receipts := current.Capacity.Financing.Loans; len(receipts) != 1 || receipts[0].ID != loan.ID || receipts[0].QuoteID == nil || *receipts[0].QuoteID != *loan.QuoteID {
		t.Fatalf("acceptance selected a loan by terms rather than exact quote identity: %+v", receipts)
	}
	if current.Capacity.Financing.ProtectedByRegion["north_sea"] != 90_000 {
		t.Fatal("reconciliation lost durable protected regional cash")
	}
	journal, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Kind string       `json:"kind"`
		Loan gameapi.Loan `json:"loan"`
	}
	if err := json.Unmarshal(journal.Response, &evidence); err != nil || journal.State != store.ActionReconciled || evidence.Kind != "authoritative_loan" || evidence.Loan.ID != loan.ID {
		t.Fatalf("journal lacks exact authoritative loan evidence: journal %+v evidence %+v error %v", journal, evidence, err)
	}

	// Crash after journal reconciliation but before mission consumption: replay
	// the pre-transition mission, then replay the already-consumed mission too.
	restarted := h.runner(primaryTestKey)
	if changed, err := restarted.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
		t.Fatalf("reconciled journal replay failed: changed %t error %v", changed, err)
	}
	persisted, current = h.mission("__growth")
	if current.Action != nil || current.LastActionID != action.ID || current.Sequence != 1 || len(current.Capacity.Financing.Loans) != 1 || current.Capacity.Financing.Loans[0].ID != loan.ID {
		t.Fatalf("replay duplicated the accepted receipt or sequence: %+v", current)
	}
	if changed, err := restarted.dispatch(h.ctx, persisted, &current); err != nil || changed {
		t.Fatalf("consumed acceptance replay advanced again: changed %t error %v", changed, err)
	}
	if sends.Load() != 1 || len(h.pending()) != 0 {
		t.Fatalf("reconciliation resent or retained accepted borrowing: sends %d pending %+v", sends.Load(), h.pending())
	}
	loanJournalRequireAccounting(t, h, current)
}

func TestLoanAcceptMissingOrDuplicateQuoteMatchesRemainUnresolved(t *testing.T) {
	for _, name := range []string{"absent", "missing quote ID", "different quote ID", "duplicate quote matches"} {
		t.Run(name, func(t *testing.T) {
			h, runner, loan, sends := loanJournalAmbiguousAction(t, "loan-accept")
			loans := []gameapi.Loan{loan}
			switch name {
			case "absent":
				loans = []gameapi.Loan{}
			case "missing quote ID":
				loans[0].QuoteID = nil
			case "different quote ID":
				other := "different-quote"
				loans[0].QuoteID = &other
			case "duplicate quote matches":
				duplicate := loan
				duplicate.ID = "second-loan-for-same-quote"
				loans = append(loans, duplicate)
			}
			loanJournalCapture(t, runner, loans)
			// Cash growth alone is deliberately not request-specific evidence.
			runner.stateCache.Vaults = []Vault{{
				AccountType: "vault", BankingRegion: loan.BankingRegion, BalancePence: loan.PrincipalPence,
			}}
			runner.snapshot["vaults"] = encode(map[string]any{"vaults": runner.stateCache.Vaults})
			runner.meta["vaults"] = runner.meta["loans"]
			mission, data := h.mission("__growth")
			action := *data.Action
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
				t.Fatalf("non-unique acceptance invented success: changed %t error %v", changed, err)
			}
			persisted, current := h.mission("__growth")
			pending := h.pending()
			if persisted.Phase != "growth_finance" || current.Action == nil || current.Action.ID != action.ID || current.LastActionID != "" || current.Sequence != 0 || len(current.Capacity.Financing.Loans) != 0 {
				t.Fatalf("non-unique acceptance consumed its intent: phase %s data %+v", persisted.Phase, current)
			}
			if len(pending) != 1 || pending[0].ID != action.ID || !bytes.Equal(pending[0].Body, action.Body) || sends.Load() != 1 {
				t.Fatalf("unresolved acceptance lost its immutable journal or resent during cooldown: %+v sends %d", pending, sends.Load())
			}
			loanJournalRequireAccounting(t, h, current)
		})
	}
}

func TestLoanMissingOrFailedAuthorityCannotReconcileSuccess(t *testing.T) {
	for _, stage := range []string{"loan-accept", "loan-repay"} {
		for _, name := range []string{"missing metadata", "incomplete", "stale", "failed", "prior generation"} {
			t.Run(stage+"/"+name, func(t *testing.T) {
				h, runner, loan, sends := loanJournalAmbiguousAction(t, stage)
				if stage == "loan-repay" {
					loan.Status = "repaid"
					loan.OwedPence = 0
					loan.OutstandingPrincipalPence = 0
					loan.PrincipalRepaidPence = loan.PrincipalPence
					loan.CollateralStatus = "released"
				}
				loanJournalCapture(t, runner, []gameapi.Loan{loan})
				meta := runner.meta["loans"]
				switch name {
				case "missing metadata":
					delete(runner.meta, "loans")
				case "incomplete":
					meta.Complete = false
				case "stale":
					meta.Stale = true
				case "failed":
					meta.Error = "borrower list refresh failed"
				case "prior generation":
					meta.Generation = uuid.NewString()
				}
				if name != "missing metadata" {
					runner.meta["loans"] = meta
				}
				mission, data := h.mission("__growth")
				action := *data.Action
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
					t.Fatalf("unknown loan authority reconciled success: changed %t error %v", changed, err)
				}
				_, current := h.mission("__growth")
				pending := h.pending()
				if current.Action == nil || current.Action.ID != action.ID || current.LastActionID != "" || current.Sequence != 0 || len(pending) != 1 || pending[0].ID != action.ID || sends.Load() != 1 {
					t.Fatalf("unknown authority released or resent the pending action: data %+v pending %+v sends %d", current, pending, sends.Load())
				}
				if stage == "loan-accept" && len(current.Capacity.Financing.Loans) != 0 || stage == "loan-repay" && current.LoanRepayment == nil {
					t.Fatalf("unknown authority altered its debt receipts or repayment intent: %+v", current)
				}
				loanJournalRequireAccounting(t, h, current)
			})
		}
	}
}

func TestLoanRepaymentInsufficientCountersDoNotProvePayment(t *testing.T) {
	for _, name := range []string{"partial principal", "interest only", "wrong named closed loan"} {
		t.Run(name, func(t *testing.T) {
			h, runner, loan, sends := loanJournalAmbiguousAction(t, "loan-repay")
			switch name {
			case "partial principal":
				loan.PrincipalRepaidPence = *loan.InstallmentPence
				loan.OutstandingPrincipalPence -= *loan.InstallmentPence
				loan.OwedPence -= *loan.InstallmentPence
			case "interest only":
				loan.InterestPaidPence = 2000
			case "wrong named closed loan":
				loan.ID = "different-closed-loan"
				loan.Status = "repaid"
				loan.PrincipalRepaidPence = loan.PrincipalPence
				loan.OutstandingPrincipalPence = 0
				loan.OwedPence = 0
			}
			loanJournalCapture(t, runner, []gameapi.Loan{loan})
			mission, data := h.mission("__growth")
			action := *data.Action
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
				t.Fatalf("automatic-payment-compatible counters reconciled manual repayment: changed %t error %v", changed, err)
			}
			persisted, current := h.mission("__growth")
			pending := h.pending()
			if persisted.Phase != "growth_finance_repay" || current.Action == nil || current.Action.ID != action.ID || current.LoanRepayment == nil || current.LoanRepayment.Before.ID != "loan-one" || current.LoanRepayment.AmountPence != 750_000 || current.Sequence != 0 || current.LastActionID != "" {
				t.Fatalf("ambiguous repayment lost its named original intent: phase %s data %+v", persisted.Phase, current)
			}
			if len(pending) != 1 || pending[0].ID != action.ID || !bytes.Equal(pending[0].Body, action.Body) || sends.Load() != 1 {
				t.Fatalf("ambiguous repayment changed its journal or resent during cooldown: %+v sends %d", pending, sends.Load())
			}
			loanJournalRequireAccounting(t, h, current)
		})
	}
}

func TestLoanRepaymentNamedRepaidOrRecoveredDebtReconcilesWithoutResend(t *testing.T) {
	for _, status := range []string{"repaid", "recovered"} {
		t.Run(status, func(t *testing.T) {
			h, _, loan, sends := loanJournalAmbiguousAction(t, "loan-repay")
			runner := h.runner(primaryTestKey)
			loan.Status = status
			loan.OwedPence = 0
			loan.OutstandingPrincipalPence = 0
			loan.PrincipalRepaidPence = loan.PrincipalPence
			loan.CollateralStatus = "released"
			loan.NextPaymentDueAt = nil
			loan.NextPaymentPence = nil
			loanJournalCapture(t, runner, []gameapi.Loan{loan})
			mission, data := h.mission("__growth")
			replayMission, replayData := h.mission("__growth")
			actionID := data.Action.ID
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("named %s debt did not reconcile: changed %t error %v", status, changed, err)
			}
			restarted := h.runner(primaryTestKey)
			if changed, err := restarted.dispatch(h.ctx, replayMission, &replayData); err != nil || !changed {
				t.Fatalf("named repayment journal replay failed: changed %t error %v", changed, err)
			}
			persisted, current := h.mission("__growth")
			if persisted.Phase != "growth_finance_repay" || current.Action != nil || current.LoanRepayment != nil || current.LastActionID != actionID || current.Sequence != 1 || len(current.Capacity.Financing.Loans) != 1 {
				t.Fatalf("closed named debt did not durably consume exactly one repayment: phase %s data %+v", persisted.Phase, current)
			}
			if sends.Load() != 1 || len(h.pending()) != 0 {
				t.Fatalf("closed loan was repaid again or left pending: sends %d pending %+v", sends.Load(), h.pending())
			}
			loanJournalRequireAccounting(t, h, current)
		})
	}
}

func TestLoanRepaymentCounterDeltaAtLeastAmountReconciles(t *testing.T) {
	h, runner, loan, sends := loanJournalAmbiguousAction(t, "loan-repay")
	loan.PrincipalRepaidPence += loan.OwedPence
	loan.InterestPaidPence += 10
	loan.OutstandingPrincipalPence, loan.OwedPence = 0, 0
	loanJournalCapture(t, runner, []gameapi.Loan{loan})
	mission, data := h.mission("__growth")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("sufficient named repayment counters did not reconcile: %t, %v", changed, err)
	}
	_, data = h.mission("__growth")
	if data.Action != nil || data.LoanRepayment != nil || data.Sequence != 1 || sends.Load() != 1 || len(h.pending()) != 0 {
		t.Fatalf("repayment delta was resent or left unresolved: %+v", data)
	}
	loanJournalRequireAccounting(t, h, data)
}

func TestLoanCommittedReceiptsConsumeMatchingIdentityDespiteChangedTerms(t *testing.T) {
	for _, stage := range []string{"loan-accept", "loan-repay"} {
		t.Run(stage, func(t *testing.T) {
			h, runner, loan, sends := loanJournalAmbiguousAction(t, stage)
			mission, data := h.mission("__growth")
			actionID := data.Action.ID
			if stage == "loan-accept" {
				rate := int32(3_500)
				loan.AnnualRateBPS = &rate
				loan.TotalRepayablePence = nil
			} else {
				loan.PrincipalRepaidPence += data.LoanRepayment.AmountPence + 100
				loan.InterestPaidPence += 50
			}
			if err := h.db.CompleteAction(h.ctx, actionID, http.StatusOK, encode(loan), nil); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("committed matching-ID receipt wedged: %t, %v", changed, err)
			}
			mission, data = h.mission("__growth")
			if data.Action != nil || data.LastActionID != actionID || data.Sequence != 1 || sends.Load() != 1 || len(h.pending()) != 0 {
				t.Fatalf("committed loan outcome was not consumed exactly once: %+v", data)
			}
			if stage == "loan-accept" && (!data.Capacity.Financing.Cancel || mission.Phase != "growth_finance_repay" || len(data.Capacity.Financing.Loans) != 1) {
				t.Fatalf("unexpected accepted terms did not preserve debt for repayment: phase %s, %+v", mission.Phase, data)
			}
			loanJournalRequireAccounting(t, h, data)
		})
	}
}

func TestLoanFullRepaymentOmitsAmountAndRequiresClosedReconciliation(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	loan := snapshotTestLoan()
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__growth", Phase: "growth_finance_repay", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "growth"}}
	if changed, err := runner.stageLoanRepayment(h.ctx, mission, &data, loan, 0); err != nil || !changed {
		t.Fatalf("stage full repayment: %t, %v", changed, err)
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(data.Action.Body, &request); err != nil || request["amount_pence"] != nil {
		t.Fatalf("full repayment froze a stale amount: %s, %v", data.Action.Body, err)
	}
	if payment := loanFullPayment(loan); payment != 750_625 {
		t.Fatalf("full repayment lacks one game-day's actual annual-rate interest: %d", payment)
	}
	loan.PrincipalRepaidPence = loan.PrincipalPence
	loan.OutstandingPrincipalPence, loan.OwedPence = 0, 0
	if loanRepaymentObserved(loan, data.LoanRepayment) {
		t.Fatal("full repayment reconciled without a named terminal closed loan")
	}
	loan.Status = "repaid"
	if !loanRepaymentObserved(loan, data.LoanRepayment) {
		t.Fatal("full repayment did not reconcile its named terminal closed loan")
	}
}

func TestLoanTemporaryRefusalKeepsFinancingPhase(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		for _, stage := range []string{"loan-accept", "loan-repay", "build"} {
			phase := "growth_finance"
			if stage == "loan-repay" {
				phase = "growth_finance_repay"
			} else if stage == "build" {
				phase = "growth_build"
			}
			mission := &store.Mission{ShipID: "__growth", Phase: phase}
			data := missionData{Action: &durableAction{ID: "temporary-refusal", Stage: stage}, Capacity: &capacityIntent{Financing: &capacityFinancing{}}, CooldownUntil: time.Now().Add(time.Hour)}
			applyRejectedResult(mission, &data, status, "temporary loan refusal")
			if mission.Phase != phase || data.Capacity.Financing.Cancel || data.Action != nil || data.Sequence != 1 || !time.Now().Before(data.CooldownUntil) {
				t.Fatalf("temporary %d %s unwound financing or lost cooldown: phase %s, %+v", status, stage, mission.Phase, data)
			}
		}
	}
}
