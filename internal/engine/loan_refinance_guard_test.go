package engine

import (
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestLoanRefinanceAllVaultCashAndCompetingPayments(t *testing.T) {
	for _, test := range []struct {
		name  string
		north int64
		south int64
		other bool
		want  bool
	}{
		{name: "another region covers", north: 0, south: 100_000},
		{name: "negative vault subtracts", north: -1, south: 100_000, want: true},
		{name: "simultaneous mortgage consumes cash", north: 100_000, other: true, want: true},
		{name: "two payments exactly covered", north: 100_000, south: 100_000, other: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			h.api.setVaultBalance("north", test.north)
			h.api.setVaultBalance("south", test.south)
			if test.other {
				h.api.mu.Lock()
				other := h.api.playerLoans[0]
				other.ID, other.CollateralKind, other.IsMortgage = "other-mortgage", "ship_lien", true
				h.api.playerLoans = append(h.api.playerLoans, other)
				h.api.mu.Unlock()
			}
			loanCapacityRefresh(t, h, runner)
			if needed, reason := runner.loanRefinanceNeeded(runner.stateCache.Loans[0]); needed != test.want {
				t.Fatalf("needed=%v, want %v, reason=%s", needed, test.want, reason)
			}
		})
	}
}

func TestLoanRefinanceOneRequestPerDueDateAcrossRestart(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "posted", true: "refused"}[refused], func(t *testing.T) {
			h, runner := loanRefinanceFixture(t)
			if !loanRefinanceDrive(t, h, runner) {
				t.Fatal("first installment emergency did not stage")
			}
			if refused {
				h.api.loanRequestMode = "conflict"
			}
			loanRefinanceDispatch(t, h, runner)
			loanCapacityRefresh(t, h, runner)
			if !refused {
				h.api.mu.Lock()
				h.api.loanRequests[0].Status = "lapsed"
				h.api.mu.Unlock()
				loanCapacityRefresh(t, h, runner)
				loanRefinanceDrive(t, h, runner)
			}
			for range 3 {
				runner = h.runner(primaryTestKey)
				loanCapacityRefresh(t, h, runner)
				runner.snapshot["_loan_refinance_retry_at"] = encode(time.Now().UTC().Add(-time.Second))
				if err := runner.saveSnapshot(h.ctx); err != nil {
					t.Fatal(err)
				}
				if loanRefinanceDrive(t, h, runner) {
					t.Fatal("request was resent for the same installment after cooldown expired")
				}
			}
			if h.api.mutationCount("/v1/loan/request") != 1 {
				t.Fatal("same due date caused repeated public request attempts")
			}
		})
	}
}

func TestLoanRefinanceUnsentStopDoesNotConsumeDueDateOrCooldown(t *testing.T) {
	h, runner := loanRefinanceFixture(t)
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("emergency did not stage")
	}
	h.api.setVaultBalance("north", 100_000)
	loanCapacityRefresh(t, h, runner)
	mission, data := h.mission(loanRefinanceMission)
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil {
		t.Fatal("recovered cash did not veto the unsent request")
	}
	if _, err := runner.replanUnsentAction(h.ctx, mission, &data, neverSentFinancialEvent); err != nil {
		t.Fatal(err)
	}
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("recovered cash did not stop the unsent request")
	}
	h.api.setVaultBalance("north", 99_999)
	runner = h.runner(primaryTestKey)
	loanCapacityRefresh(t, h, runner)
	if until := decode[time.Time](runner.snapshot, "_loan_refinance_retry_at"); !until.IsZero() {
		t.Fatalf("unsent stop armed cooldown: %v", until)
	}
	if !loanRefinanceDrive(t, h, runner) {
		t.Fatal("unsent stop consumed the due date without posting")
	}
	loanRefinanceDispatch(t, h, runner)
	if h.api.mutationCount("/v1/loan/request") != 1 {
		t.Fatal("unsent stop did not permit the first actual request")
	}
}

func TestLoanRefinanceOtherPaymentUsesMaxArrearsAndNext(t *testing.T) {
	h, runner := loanRefinanceFixture(t)
	h.api.setVaultBalance("north", 120_000)
	loanCapacityRefresh(t, h, runner)
	loan := runner.stateCache.Loans[0]
	other := gameapi.Loan{ID: "other", BorrowerCompanyID: h.account.CompanyID, Status: "active", ArrearsPence: 20_000, NextPaymentPence: new(int64(20_000))}
	runner.stateCache.Loans = append(runner.stateCache.Loans, other)
	if cash := runner.loanRefinanceCash(loan); cash != 100_000 {
		t.Fatalf("arrears and next payment were double-counted: %d", cash)
	}
}
