package engine

import (
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func loanRefinanceDueKey(loan gameapi.Loan) string {
	if loan.NextPaymentDueAt == nil {
		return loan.ID
	}
	return loan.ID + "|" + loan.NextPaymentDueAt.UTC().Format(time.RFC3339Nano)
}

func loanRefinancePayment(loan gameapi.Loan) int64 {
	var next int64
	if loan.NextPaymentPence != nil {
		next = *loan.NextPaymentPence
	}
	return max(next, loan.ArrearsPence)
}

func (r *Runner) loanRefinanceCash(loan gameapi.Loan) int64 {
	var available int64
	// Collections draw from every vault. An overdrawn vault subtracts cash.
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" {
			available += vault.BalancePence
		}
	}
	for _, other := range r.stateCache.Loans {
		if other.ID == loan.ID || other.BorrowerCompanyID != r.account.CompanyID ||
			other.Status != "active" && other.Status != "defaulted" {
			continue
		}
		if other.ArrearsPence > 0 || other.NextPaymentDueAt != nil && loan.NextPaymentDueAt != nil && !other.NextPaymentDueAt.After(*loan.NextPaymentDueAt) {
			available -= loanRefinancePayment(other)
		}
	}
	return available
}

func (r *Runner) recordLoanRefinanceDue(loan gameapi.Loan, key string) {
	dates := decode[map[string]string](r.snapshot, "_loan_refinance_due_dates")
	if dates == nil {
		dates = make(map[string]string)
	}
	for id := range dates {
		if debt, exists := r.loanByID(id); !exists || debt.Status != "active" && debt.Status != "defaulted" {
			delete(dates, id)
		}
	}
	if key == "" {
		key = loanRefinanceDueKey(loan)
	}
	dates[loan.ID] = key
	r.snapshot["_loan_refinance_due_dates"] = encode(dates)
}

func (r *Runner) loanRefinanceMayRequest(loan gameapi.Loan) (bool, string) {
	if decode[map[string]string](r.snapshot, "_loan_refinance_due_dates")[loan.ID] == loanRefinanceDueKey(loan) {
		return false, "refinance already requested for this installment due date"
	}
	if until := decode[time.Time](r.snapshot, "_loan_refinance_retry_at"); time.Now().Before(until) {
		r.wakeBy(until)
		return false, "refinance is cooling down for 24 real hours after its previous request ended"
	}
	return true, ""
}
