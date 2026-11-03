package store

import (
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

// remainingLoanInstallments counts scheduled dates, not owed/installment: early
// repayments and the smaller final payment make that monetary ratio unreliable.
// Guide §3.13.1 defines a ten-game-day cadence with a final payment at maturity.
func remainingLoanInstallments(loan gameapi.Loan, clock gameapi.Clock) (*int, bool, string) {
	if !loanOpenStatus(loan.Status) && loanFundedStatus(loan.Status) && loan.OwedPence == 0 {
		zero := 0
		return &zero, false, "Closed loan; no scheduled payments remain."
	}
	if loan.Status == "defaulted" {
		return nil, false, "Defaulted debt is collected outside the normal installment schedule."
	}
	if loan.Status != "active" || loan.OwedPence <= 0 {
		return nil, false, "A current outstanding loan is needed to establish its payment schedule."
	}
	if loan.ArrearsPence > 0 || loan.ArrearsDueBy != nil && !loan.ArrearsDueBy.IsZero() {
		return nil, false, "The next payment may be an arrears grace deadline, not a scheduled installment."
	}
	if clock.RealUTC.IsZero() || clock.Calendar.TimeScale <= 0 || clock.Paused {
		return nil, false, "A valid, running recorded game clock is needed to derive the schedule."
	}
	if loan.TermGameDays < 1 || loan.TermGameDays > 360 || loan.MaturesAt == nil || loan.MaturesAt.IsZero() {
		return nil, false, "The recorded loan term or maturity is missing or invalid."
	}
	maturity := *loan.MaturesAt
	if maturity.Before(clock.RealUTC) {
		return nil, false, "Maturity predates the recorded clock while debt remains; the normal schedule is uncertain."
	}
	if loan.DisbursedAt != nil && (loan.DisbursedAt.IsZero() || !loan.DisbursedAt.Before(maturity)) {
		return nil, false, "The recorded disbursement and maturity do not form a valid term."
	}
	if loan.RateBasis == "daily" || loan.RateBasis == "annual" && loan.Repayment == "balloon" {
		if loan.NextPaymentDueAt != nil && !loan.NextPaymentDueAt.Equal(maturity) {
			return nil, false, "The recorded next payment does not match balloon maturity."
		}
		one := 1
		return &one, true, "One payment at maturity under the recorded balloon or legacy daily terms."
	}
	if loan.RateBasis != "annual" || loan.Repayment != "amortised" ||
		loan.InstallmentPence == nil || *loan.InstallmentPence <= 0 ||
		loan.NextPaymentPence == nil || *loan.NextPaymentPence <= 0 ||
		loan.NextPaymentDueAt == nil || loan.NextPaymentDueAt.IsZero() {
		return nil, false, "The recorded installment terms or next scheduled payment are missing or invalid."
	}
	next := *loan.NextPaymentDueAt
	if next.After(maturity) || next.Before(clock.RealUTC) || loan.DisbursedAt != nil && !next.After(*loan.DisbursedAt) {
		return nil, false, "The next payment falls outside the remaining recorded loan term."
	}
	scale := time.Duration(clock.Calendar.TimeScale)
	remaining := maturity.Sub(next)
	term := time.Duration(loan.TermGameDays) * 24 * time.Hour
	// Bound the real duration before scaling, both to reject incoherent dates
	// and to keep multiplication safely inside time.Duration's range.
	if remaining > term/scale {
		return nil, false, "The remaining dates exceed the recorded loan term at this game speed."
	}
	gameRemaining := remaining * scale
	period := 10 * 24 * time.Hour
	count := int(gameRemaining / period)
	if gameRemaining%period != 0 {
		count++
	}
	count++ // The next due date itself is a payment, including maturity.
	if count > int((loan.TermGameDays+9)/10) {
		return nil, false, "The scheduled dates imply more installments than the recorded term permits."
	}
	return &count, true, "Derived from the next due date: every 10 game days, with the final payment at maturity."
}
