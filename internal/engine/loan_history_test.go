package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestLoanHistoryRefreshGate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		meta DatasetMeta
		due  bool
	}{
		{name: "first run", due: true},
		{name: "before six hours", meta: DatasetMeta{AttemptedAt: now.Add(-6*time.Hour + time.Nanosecond)}},
		{name: "exactly six hours", meta: DatasetMeta{AttemptedAt: now.Add(-6 * time.Hour)}, due: true},
		{name: "failed attempt still gates", meta: DatasetMeta{AttemptedAt: now.Add(-time.Hour), Stale: true, Error: "unavailable"}},
		{name: "newer reconciliation gates", meta: DatasetMeta{AttemptedAt: now.Add(-7 * time.Hour), UpdatedAt: now.Add(-time.Minute)}},
		{name: "future attempt", meta: DatasetMeta{AttemptedAt: now.Add(time.Hour)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := loanHistoryRefreshDue(test.meta, now); got != test.due {
				t.Fatalf("refresh due = %t, want %t", got, test.due)
			}
		})
	}
}

func TestLoanHistoryRefreshBackfillsTenClosedLoansAndSurvivesRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	active := snapshotTestLoan()
	active.UpdatedAt = now.Add(-time.Minute)
	history := []gameapi.Loan{active}
	for index := range 10 {
		closed := active
		closed.ID = fmt.Sprintf("repaid-history-%02d", index)
		closed.Status, closed.CollateralStatus = "repaid", "released"
		closed.OwedPence, closed.OutstandingPrincipalPence = 0, 0
		closed.PrincipalRepaidPence, closed.InterestPaidPence = closed.PrincipalPence, 100
		closed.ClosedAt = &now
		closed.NextPaymentDueAt, closed.NextPaymentPence = nil, nil
		history = append(history, closed)
	}
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
			base.ServeHTTP(writer, request)
			return
		}
		calls.Add(1)
		if request.URL.Query().Get("role") != "borrower" || request.URL.Query().Get("limit") != "200" {
			t.Errorf("wrong borrower backfill query: %s", request.URL)
		}
		_ = json.NewEncoder(writer).Encode(history)
	})
	debt := encode([]gameapi.Loan{active})
	runner.snapshot["loans"] = debt
	runner.refreshLoanHistory(h.ctx, now)
	if calls.Load() != 1 || !runner.meta["loan_history"].Complete || runner.meta["loan_history"].Stale {
		t.Fatalf("first borrower backfill failed: calls %d meta %+v", calls.Load(), runner.meta["loan_history"])
	}
	if !bytes.Equal(runner.snapshot["loans"], debt) {
		t.Fatal("reporting history changed operational loan authority")
	}
	restarted := h.runner(primaryTestKey)
	restarted.refreshLoanHistory(h.ctx, now.Add(6*time.Hour-time.Second))
	if calls.Load() != 1 {
		t.Fatal("restart ignored the durable six-hour refresh gate")
	}
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 10 || len(report.Accounts[0].ActiveLoans) != 1 || report.Accounts[0].Totals.RepaidCount != 10 {
		t.Fatalf("full history did not retain all ten repaid rows: %+v", report)
	}
	// A routine active-only upsert and its saved snapshot keep closed history.
	if !restarted.applyCapture(h.ctx, "loans", fetchResult{data: debt, attemptedAt: now, completedAt: now}) {
		t.Fatal("ordinary debt capture failed")
	}
	restarted.snapshot["_meta"] = encode(restarted.meta)
	if err := restarted.saveSnapshot(h.ctx); err != nil {
		t.Fatal(err)
	}
	report, err = h.db.Loans(h.ctx, h.account.ID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 10 {
		t.Fatalf("ordinary capture discarded closed history: %+v %v", report, err)
	}
}

func TestLoanHistoryFailedRefreshGatesAfterRestartAndDoesNotBlockDebtCapture(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" && request.URL.Query().Get("status") == "" {
			calls.Add(1)
			http.Error(writer, "history unavailable", http.StatusNotFound)
			return
		}
		base.ServeHTTP(writer, request)
	})
	now := time.Now().UTC()
	runner.refreshLoanHistory(h.ctx, now)
	meta := runner.meta["loan_history"]
	if calls.Load() != 1 || !meta.Stale || meta.Complete || meta.Error == "" || !meta.UpdatedAt.IsZero() {
		t.Fatalf("failed refresh claimed complete history: calls %d meta %+v", calls.Load(), meta)
	}
	restarted := h.runner(primaryTestKey)
	restarted.refreshLoanHistory(h.ctx, now.Add(time.Hour))
	if calls.Load() != 1 {
		t.Fatal("failed full-history read retried after restart before six real hours")
	}
	restarted.refreshLoanHistory(h.ctx, now.Add(6*time.Hour))
	if calls.Load() != 2 {
		t.Fatal("failed history read never retried at the six-hour boundary")
	}
	loan := snapshotTestLoan()
	if !restarted.applyCapture(h.ctx, "loans", fetchResult{data: encode([]gameapi.Loan{loan}), attemptedAt: now, completedAt: now}) || !restarted.fresh("loans") {
		t.Fatal("optional history failure blocked current debt authority")
	}
	// Recording failure is also optional; it cannot turn a valid debt book stale.
	restarted.account.ID = " "
	if !restarted.applyCapture(h.ctx, "loans", fetchResult{data: encode([]gameapi.Loan{loan}), attemptedAt: now, completedAt: now}) || !restarted.fresh("loans") {
		t.Fatal("history write failure blocked a valid operational debt capture")
	}
}

func TestLoanHistoryRefinanceRecordsClosedRowsBeforeDebtFiltering(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	closed := snapshotTestLoan()
	closed.Status, closed.CollateralStatus = "repaid", "released"
	closed.OwedPence, closed.OutstandingPrincipalPence = 0, 0
	closed.PrincipalRepaidPence = closed.PrincipalPence
	closed.NextPaymentDueAt, closed.NextPaymentPence = nil, nil
	closedAt := time.Now().UTC()
	closed.UpdatedAt, closed.ClosedAt = closedAt, &closedAt
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" {
			_ = json.NewEncoder(writer).Encode([]gameapi.Loan{closed})
			return
		}
		base.ServeHTTP(writer, request)
	})
	observed, exists, err := runner.refreshLoanRefinanceLoan(h.ctx, closed.ID)
	if err != nil || !exists || observed.Status != "repaid" || len(runner.stateCache.Loans) != 0 {
		t.Fatalf("refinance did not retain its existing closed-debt semantics: %+v %t %v", observed, exists, err)
	}
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 1 || !runner.meta["loan_history"].Complete {
		t.Fatalf("refinance filtered a closed loan before recording: %+v %v", report, err)
	}
}

func TestLoanHistoryCaptureWaveDistinguishesCurrentDebtFromFullHistory(t *testing.T) {
	for _, test := range []struct {
		name string
		task fetchTask
		full bool
	}{
		{"routine", fetchTask{key: "loans", kind: fetchBorrowerLoans}, false},
		{"full borrower history", fetchTask{key: "loans", path: "/v1/loan/list?role=borrower", kind: fetchPages}, true},
		{"filtered ordinary pages", fetchTask{key: "loans", path: "/v1/loan/list?role=borrower&status=active", kind: fetchPages}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			loan := snapshotTestLoan()
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/loan/list" {
					base.ServeHTTP(writer, request)
					return
				}
				if request.URL.Query().Get("status") == "defaulted" {
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{})
				} else {
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{loan})
				}
			})
			if err := runner.captureWave(h.ctx, []fetchTask{test.task}, nil); err != nil {
				t.Fatal(err)
			}
			meta := runner.meta["loan_history"]
			if meta.Complete != test.full || loanHistoryRefreshDue(meta, time.Now().UTC()) == test.full {
				t.Fatalf("wrong full-history authority or gate: %+v", meta)
			}
		})
	}
}

func TestLoanHistoryPassBackfillRunsOnceAfterWork(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	closed := snapshotTestLoan()
	closed.Status, closed.OwedPence, closed.OutstandingPrincipalPence = "repaid", 0, 0
	closed.PrincipalRepaidPence = closed.PrincipalPence
	closed.ClosedAt = new(time.Now().UTC())
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" && request.URL.Query().Get("status") == "" {
			calls.Add(1)
			_ = json.NewEncoder(writer).Encode([]gameapi.Loan{closed})
			return
		}
		base.ServeHTTP(writer, request)
	})
	for range 2 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := decode[map[string]DatasetMeta](persisted, "_meta")
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || calls.Load() != 1 || metadata["loan_history"].AttemptedAt.IsZero() ||
		len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 1 {
		t.Fatalf("pass-level history backfill or gate failed: calls %d meta %+v error %v", calls.Load(), metadata["loan_history"], err)
	}
}

func TestLoanHistoryReservationPrecedesReadAndCanceledRefreshStaysGated(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	started := make(chan struct{})
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
			base.ServeHTTP(writer, request)
			return
		}
		calls.Add(1)
		persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
		if err != nil {
			t.Error(err)
		}
		metadata := decode[map[string]DatasetMeta](persisted, "_meta")
		if !metadata["loan_history"].AttemptedAt.Truncate(time.Microsecond).Equal(now.Truncate(time.Microsecond)) {
			t.Error("history read began before its durable attempt reservation")
		}
		close(started)
		<-request.Context().Done()
	})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		runner.refreshLoanHistory(ctx, now)
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("borrower history read did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled history read blocked the runner")
	}
	if !runner.meta["loan_history"].Stale {
		t.Fatal("canceled capture claimed complete history")
	}
	restarted := h.runner(primaryTestKey)
	restarted.refreshLoanHistory(h.ctx, now.Add(time.Hour))
	if calls.Load() != 1 {
		t.Fatal("restart retried a canceled history read inside six hours")
	}
}

func TestLoanHistoryReceiptsKeepClosureMortgageAndReceiptObservation(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	closed := snapshotTestLoan()
	closed.ID, closed.Status, closed.OwedPence, closed.OutstandingPrincipalPence = "receipt-repaid", "repaid", 0, 0
	closed.PrincipalRepaidPence, closed.ClosedAt = closed.PrincipalPence, &at
	mortgage := snapshotTestLoan()
	mortgage.ID, mortgage.IsMortgage = "receipt-mortgage", true
	mortgage.CollateralKind, mortgage.CollateralStatus, mortgage.CollateralShipID = "ship_lien", "pledged", new(recoveryShipBlastoise)
	for _, loan := range []gameapi.Loan{closed, mortgage} {
		runner.recordLoanReceipt(h.ctx, encode(loan), at)
	}
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 1 ||
		len(report.Accounts[0].ActiveLoans) != 1 || !report.Accounts[0].ActiveLoans[0].IsMortgage ||
		!report.Accounts[0].ClosedLoans[0].ObservedAt.Equal(at) || !report.Accounts[0].ActiveLoans[0].ObservedAt.Equal(at) {
		t.Fatalf("receipt history or observation timestamp was lost: %+v, %v", report, err)
	}
	current := encode([]gameapi.Loan{mortgage})
	runner.snapshot["loans"] = current
	runner.meta["loans"] = DatasetMeta{Complete: true, UpdatedAt: time.Now().UTC(), Generation: runner.generation}
	runner.meta["loan_history"] = DatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true}
	foreign := mortgage
	foreign.ID, foreign.BorrowerCompanyID = "foreign-receipt", "another-borrower"
	runner.recordLoanReceipt(h.ctx, encode(foreign), at)
	runner.recordLoanReceipt(h.ctx, json.RawMessage(`{"id":"incomplete-receipt"}`), at)
	if !bytes.Equal(runner.snapshot["loans"], current) || !runner.fresh("loans") ||
		!runner.meta["loan_history_recording"].Stale || !runner.meta["loan_history"].Complete || runner.meta["loan_history"].Stale {
		t.Fatal("invalid optional receipt changed operational debt or full-history authority")
	}
}
