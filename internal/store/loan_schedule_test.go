package store

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func loanHistoryFixture(id, accountID string, at time.Time) gameapi.Loan {
	annual, installment, payment := int32(3000), int64(10_100), int64(10_100)
	disbursed, next, maturity := at, at.Add(4*time.Hour), at.Add(12*time.Hour)
	return gameapi.Loan{
		ID: id, LenderCompanyID: "lender-recorded", BorrowerCompanyID: "company-" + accountID,
		BankingRegion: "north_sea", PrincipalPence: 30_000, RateBasis: "annual", AnnualRateBPS: &annual,
		TermGameDays: 30, Repayment: "amortised", InstallmentPence: &installment,
		NextPaymentDueAt: &next, NextPaymentPence: &payment, DisbursedAt: &disbursed, MaturesAt: &maturity,
		CollateralKind: "unsecured", CollateralStatus: "none", Recourse: true,
		Status: "active", OwedPence: 20_100, OutstandingPrincipalPence: 20_000,
		PrincipalRepaidPence: 10_000, InterestPaidPence: 100, CreatedAt: at, UpdatedAt: at,
	}
}

func TestRemainingLoanInstallmentsScheduleBoundaries(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fixture := loanHistoryFixture("schedule", "one", at)
	clock := gameapi.Clock{RealUTC: at, Calendar: gameapi.Calendar{TimeScale: 60}}
	for _, test := range []struct {
		name   string
		change func(*gameapi.Loan, *gameapi.Clock)
		want   int
	}{
		{"three scheduled dates", nil, 3},
		{"early repayment does not use owed ratio", func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.OwedPence = 1 }, 3},
		{"final fractional period", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.TermGameDays = 25
			maturity := at.Add(10 * time.Hour)
			loan.MaturesAt = &maturity
		}, 3},
		{"next is maturity", func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.NextPaymentDueAt = loan.MaturesAt }, 1},
		{"payment exactly due at maturity", func(loan *gameapi.Loan, clock *gameapi.Clock) {
			loan.NextPaymentDueAt = loan.MaturesAt
			clock.RealUTC = *loan.MaturesAt
		}, 1},
		{"short term final only", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.TermGameDays = 1
			maturity := at.Add(24 * time.Minute)
			loan.MaturesAt, loan.NextPaymentDueAt = &maturity, &maturity
		}, 1},
		{"180 day mortgage", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.IsMortgage, loan.TermGameDays = true, 180
			maturity := at.Add(72 * time.Hour)
			loan.MaturesAt = &maturity
		}, 18},
		{"maximum annual term", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.TermGameDays = 360
			maturity := at.Add(144 * time.Hour)
			loan.MaturesAt = &maturity
		}, 36},
		{"different observed speed", func(loan *gameapi.Loan, clock *gameapi.Clock) {
			clock.Calendar.TimeScale = 120
			next, maturity := at.Add(2*time.Hour), at.Add(6*time.Hour)
			loan.NextPaymentDueAt, loan.MaturesAt = &next, &maturity
		}, 3},
		{"annual balloon", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.Repayment = "balloon"
			loan.NextPaymentDueAt = loan.MaturesAt
			loan.InstallmentPence = nil
		}, 1},
		{"legacy daily maturity", func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.RateBasis, loan.Repayment = "daily", "balloon"
			loan.AnnualRateBPS, loan.NextPaymentDueAt, loan.InstallmentPence = nil, nil, nil
		}, 1},
		{"closed paid without clock or terms", func(loan *gameapi.Loan, clock *gameapi.Clock) {
			loan.Status, loan.OwedPence = "repaid", 0
			loan.MaturesAt, loan.NextPaymentDueAt, loan.InstallmentPence = nil, nil, nil
			*clock = gameapi.Clock{}
		}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			loan, observedClock := fixture, clock
			if test.change != nil {
				test.change(&loan, &observedClock)
			}
			count, derived, note := remainingLoanInstallments(loan, observedClock)
			if count == nil || *count != test.want || derived != (test.want > 0) || note == "" {
				t.Fatalf("count=%v derived=%v note=%q; want %d", count, derived, note, test.want)
			}
		})
	}
}

func TestRemainingLoanInstallmentsUnknownIsNotZero(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fixture := loanHistoryFixture("unknown", "one", at)
	clock := gameapi.Clock{RealUTC: at, Calendar: gameapi.Calendar{TimeScale: 60}}
	for name, change := range map[string]func(*gameapi.Loan, *gameapi.Clock){
		"missing clock":       func(_ *gameapi.Loan, clock *gameapi.Clock) { *clock = gameapi.Clock{} },
		"zero speed":          func(_ *gameapi.Loan, clock *gameapi.Clock) { clock.Calendar.TimeScale = 0 },
		"negative speed":      func(_ *gameapi.Loan, clock *gameapi.Clock) { clock.Calendar.TimeScale = -60 },
		"paused clock":        func(_ *gameapi.Loan, clock *gameapi.Clock) { clock.Paused = true },
		"missing real clock":  func(_ *gameapi.Loan, clock *gameapi.Clock) { clock.RealUTC = time.Time{} },
		"missing term":        func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.TermGameDays = 0 },
		"oversized term":      func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.TermGameDays = 361 },
		"missing maturity":    func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.MaturesAt = nil },
		"zero maturity":       func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.MaturesAt = &time.Time{} },
		"missing next due":    func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.NextPaymentDueAt = nil },
		"zero next due":       func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.NextPaymentDueAt = &time.Time{} },
		"missing installment": func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.InstallmentPence = nil },
		"zero installment": func(loan *gameapi.Loan, _ *gameapi.Clock) {
			value := int64(0)
			loan.InstallmentPence = &value
		},
		"missing next amount": func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.NextPaymentPence = nil },
		"unknown repayment":   func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.Repayment = "bullet" },
		"unknown rate basis":  func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.RateBasis = "monthly" },
		"defaulted":           func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.Status = "defaulted" },
		"arrears grace deadline": func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.ArrearsPence = 100
			grace := at.Add(24 * time.Minute)
			loan.ArrearsDueBy, loan.NextPaymentDueAt = &grace, &grace
		},
		"grace without amount": func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.ArrearsDueBy = loan.NextPaymentDueAt },
		"next beyond maturity": func(loan *gameapi.Loan, _ *gameapi.Clock) {
			next := loan.MaturesAt.Add(time.Microsecond)
			loan.NextPaymentDueAt = &next
		},
		"next predates observation": func(loan *gameapi.Loan, clock *gameapi.Clock) {
			clock.RealUTC = loan.NextPaymentDueAt.Add(time.Microsecond)
		},
		"maturity predates observation": func(loan *gameapi.Loan, clock *gameapi.Clock) { clock.RealUTC = loan.MaturesAt.Add(time.Microsecond) },
		"reversed disbursement":         func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.DisbursedAt = loan.MaturesAt },
		"next before disbursement":      func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.DisbursedAt = loan.NextPaymentDueAt },
		"unbounded date span": func(loan *gameapi.Loan, _ *gameapi.Clock) {
			maturity := at.AddDate(500, 0, 0)
			loan.MaturesAt = &maturity
		},
		"too many scheduled payments": func(loan *gameapi.Loan, _ *gameapi.Clock) {
			loan.TermGameDays = 30
			next := at.Add(time.Hour)
			maturity := next.Add(12 * time.Hour)
			loan.NextPaymentDueAt, loan.MaturesAt = &next, &maturity
		},
		"inconsistent balloon due": func(loan *gameapi.Loan, _ *gameapi.Clock) { loan.Repayment = "balloon" },
	} {
		t.Run(name, func(t *testing.T) {
			loan, observedClock := fixture, clock
			change(&loan, &observedClock)
			count, derived, note := remainingLoanInstallments(loan, observedClock)
			if count != nil || derived || note == "" {
				t.Fatalf("uncertain schedule became count=%v derived=%v note=%q", count, derived, note)
			}
		})
	}
}
