package engine

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func reviewRepaidLoan(at time.Time) gameapi.Loan {
	loan := snapshotTestLoan()
	loan.Status, loan.CollateralStatus = "repaid", "released"
	loan.OwedPence, loan.OutstandingPrincipalPence = 0, 0
	loan.PrincipalRepaidPence, loan.InterestPaidPence = loan.PrincipalPence, 100
	loan.UpdatedAt, loan.ClosedAt = at, &at
	loan.NextPaymentDueAt, loan.NextPaymentPence = nil, nil
	return loan
}

func TestLoanHistoryReportingRetainsLegacyRowsAndCountsMalformedRows(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	closed := reviewRepaidLoan(now)
	closed.BankingRegion, closed.CollateralKind, closed.CollateralStatus = "future-region", "future-collateral", "future-release"
	rows := []json.RawMessage{
		encode(closed),
		json.RawMessage(`{"id":"legacy-closed","status":"repaid","principal_repaid_pence":2100}`),
		json.RawMessage(`null`),
		json.RawMessage(`{"id":"missing-status"}`),
		json.RawMessage(`{"id":"bad-counter","status":"repaid","interest_paid_pence":"not-a-number"}`),
		json.RawMessage(`{"id":"foreign","status":"repaid","borrower_company_id":"another-company"}`),
	}
	if err := validateBorrowerLoans(encode(rows), h.account.CompanyID); err == nil {
		t.Fatal("reporting fixture unexpectedly satisfies operational debt authority")
	}
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/list" && request.URL.Query().Get("status") == "" {
			_ = json.NewEncoder(writer).Encode(rows)
			return
		}
		base.ServeHTTP(writer, request)
	})
	debt := encode([]gameapi.Loan{snapshotTestLoan()})
	runner.snapshot["loans"] = debt
	runner.refreshLoanHistory(h.ctx, now)
	meta := runner.meta["loan_history"]
	if meta.Complete || meta.Stale || meta.Error != "Skipped 4 malformed borrower history rows; valid rows were retained." || meta.UpdatedAt.IsZero() {
		t.Fatalf("partial reporting history has incorrect coverage: %+v", meta)
	}
	if !bytes.Equal(runner.snapshot["loans"], debt) {
		t.Fatal("lenient reporting rows changed strict operational debt")
	}
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 2 || report.Accounts[0].HistoryComplete {
		t.Fatalf("valid legacy history was lost with malformed neighbors: %+v, %v", report, err)
	}
	if report.Accounts[0].ClosedLoans[1].BankingRegion != "future-region" || report.Accounts[0].ClosedLoans[1].CollateralKind != "future-collateral" {
		t.Fatalf("unknown reporting vocabulary was discarded: %+v", report.Accounts[0].ClosedLoans)
	}
}

func TestLoanHistoryDebtDisappearanceRefreshesBeforeSixHoursAndGatesFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("refresh fails %t", fail), func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			now := time.Now().UTC()
			active := snapshotTestLoan()
			closed := reviewRepaidLoan(now.Add(time.Minute))
			var calls atomic.Int32
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
					base.ServeHTTP(writer, request)
					return
				}
				call := calls.Add(1)
				if call == 1 {
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{active})
				} else if fail {
					http.Error(writer, "history unavailable", http.StatusNotFound)
				} else {
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{closed})
				}
			})
			if !runner.applyCapture(h.ctx, "loans", fetchResult{data: encode([]gameapi.Loan{active}), attemptedAt: now, completedAt: now}) {
				t.Fatal("initial operational debt capture failed")
			}
			runner.refreshLoanHistory(h.ctx, now)
			if !runner.applyCapture(h.ctx, "loans", fetchResult{data: json.RawMessage(`[]`), attemptedAt: now.Add(time.Minute), completedAt: now.Add(time.Minute)}) {
				t.Fatal("empty current debt capture failed")
			}
			runner.refreshLoanHistory(h.ctx, now.Add(2*time.Minute))
			if calls.Load() != 2 {
				t.Fatal("disappeared debt waited for the six-hour periodic scan")
			}
			restarted := h.runner(primaryTestKey)
			restarted.refreshLoanHistory(h.ctx, now.Add(3*time.Minute))
			if calls.Load() != 2 {
				t.Fatal("consumed closure invalidation caused a per-pass history retry after restart")
			}
			report, err := h.db.Loans(h.ctx, h.account.ID)
			if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ActiveLoans) != 0 || report.Accounts[0].Totals.OwedPence != 0 {
				t.Fatalf("closed history changed current debt membership: %+v, %v", report, err)
			}
			account := report.Accounts[0]
			if fail {
				if account.HistoryComplete || account.HistoryError == "" || account.Totals.PrincipalRepaidPence != active.PrincipalRepaidPence {
					t.Fatalf("failed closure refresh erased known repayments or claimed complete coverage: %+v", account)
				}
			} else if !account.HistoryComplete || len(account.ClosedLoans) != 1 || account.Totals.PrincipalRepaidPence != closed.PrincipalPence {
				t.Fatalf("early refresh did not record the final repayment: %+v", account)
			}
		})
	}
}

func TestLoanHistoryOversizedPageRetainsLastGoodRowsAndDurableFailure(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	closed := reviewRepaidLoan(now.Add(-time.Hour))
	if err := h.db.SyncLoanHistory(h.ctx, h.account.ID, []json.RawMessage{encode(closed)}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	runner.meta["loan_history"] = DatasetMeta{UpdatedAt: now.Add(-7 * time.Hour), AttemptedAt: now.Add(-7 * time.Hour), Complete: true}
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
			base.ServeHTTP(writer, request)
			return
		}
		calls.Add(1)
		rows := make([]gameapi.Loan, 201)
		for index := range rows {
			rows[index] = closed
			rows[index].ID = fmt.Sprintf("overflow-%03d", index)
		}
		_ = json.NewEncoder(writer).Encode(rows)
	})
	runner.refreshLoanHistory(h.ctx, now)
	restarted := h.runner(primaryTestKey)
	restarted.refreshLoanHistory(h.ctx, now.Add(time.Minute))
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || calls.Load() != 1 || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 1 ||
		report.Accounts[0].ClosedLoans[0].ID != closed.ID || report.Accounts[0].HistoryComplete || report.Accounts[0].HistoryError == "" {
		t.Fatalf("oversized history response replaced last-good rows or lost its gate: calls %d, %+v, %v", calls.Load(), report, err)
	}
}

func TestLoanHistorySlowReadStopsAtDeadlineAndPersistsFailure(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
			base.ServeHTTP(writer, request)
			return
		}
		calls.Add(1)
		<-request.Context().Done()
	})
	started := time.Now()
	runner.refreshLoanHistory(h.ctx, now)
	if elapsed := time.Since(started); elapsed < loanHistoryRefreshTimeout-time.Second || elapsed > loanHistoryRefreshTimeout+5*time.Second {
		t.Fatalf("optional history read did not honor its deadline: %s", elapsed)
	}
	restarted := h.runner(primaryTestKey)
	meta := restarted.meta["loan_history"]
	if !meta.Stale || meta.Complete || meta.Error == "" || !meta.AttemptedAt.Equal(now) {
		t.Fatalf("read deadline did not persist incomplete history: %+v", meta)
	}
	restarted.refreshLoanHistory(h.ctx, now.Add(time.Minute))
	if calls.Load() != 1 {
		t.Fatal("timed-out history read retried before the durable gate")
	}
}

func TestLoanHistorySlowSuccessfulBackfillPersistsCompletionWithFreshSaveDeadline(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	lock, err := admin.BeginTx(h.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	closed := reviewRepaidLoan(time.Now().UTC())
	released := make(chan error, 1)
	var calls atomic.Int32
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/loan/list" || request.URL.Query().Get("status") != "" {
			base.ServeHTTP(writer, request)
			return
		}
		calls.Add(1)
		timer := time.NewTimer(loanHistoryRefreshTimeout - 2*time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-request.Context().Done():
			released <- request.Context().Err()
			return
		}
		if _, err := lock.ExecContext(h.ctx, "SELECT account_id FROM snapshots WHERE account_id = $1 FOR UPDATE", h.account.ID); err != nil {
			released <- err
			http.Error(writer, "test snapshot lock failed", http.StatusInternalServerError)
			return
		}
		// The row is readable during the loan-history transaction, but saving
		// completion must wait beyond the original read deadline.
		go func() {
			timer := time.NewTimer(2500 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-h.ctx.Done():
			}
			released <- lock.Rollback()
		}()
		_ = json.NewEncoder(writer).Encode([]gameapi.Loan{closed})
	})
	started := time.Now()
	now := started.UTC()
	runner.refreshLoanHistory(h.ctx, now)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < loanHistoryRefreshTimeout || elapsed > loanHistoryRefreshTimeout+loanHistoryMetadataSaveTimeout+3*time.Second {
		t.Fatalf("fixture did not exercise completion after the read deadline: %s", elapsed)
	}
	restarted := h.runner(primaryTestKey)
	meta := restarted.meta["loan_history"]
	if !meta.Complete || meta.Stale || meta.Error != "" || meta.UpdatedAt.IsZero() || restarted.meta["loan_history_recording"].Stale {
		t.Fatalf("successful slow backfill was persisted as incomplete: history %+v, recording %+v", meta, restarted.meta["loan_history_recording"])
	}
	restarted.refreshLoanHistory(h.ctx, now.Add(time.Hour))
	report, err := h.db.Loans(h.ctx, h.account.ID)
	if err != nil || calls.Load() != 1 || len(report.Accounts) != 1 || len(report.Accounts[0].ClosedLoans) != 1 || !report.Accounts[0].HistoryComplete {
		t.Fatalf("successful backfill completion or restart gate was lost: calls %d, %+v, %v", calls.Load(), report, err)
	}
}

func TestLoanHistoryUnreservedFailureDoesNotConsumeRefresh(t *testing.T) {
	for _, closure := range []bool{false, true} {
		t.Run(fmt.Sprintf("closure invalidation %t", closure), func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			now := time.Now().UTC()
			previous := DatasetMeta{UpdatedAt: now.Add(-7 * time.Hour), AttemptedAt: now.Add(-7 * time.Hour), Complete: true}
			if closure {
				previous.UpdatedAt, previous.AttemptedAt = now.Add(-time.Hour), now.Add(-time.Hour)
				runner.meta["loan_history_due"] = DatasetMeta{UpdatedAt: now}
			}
			runner.meta["loan_history"] = previous
			var calls atomic.Int32
			base := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/loan/list" && request.URL.Query().Get("status") == "" {
					calls.Add(1)
					_ = json.NewEncoder(writer).Encode([]gameapi.Loan{})
					return
				}
				base.ServeHTTP(writer, request)
			})
			runner.account.ID = " "
			runner.refreshLoanHistory(h.ctx, now)
			meta := runner.meta["loan_history"]
			if calls.Load() != 0 || !meta.AttemptedAt.Equal(previous.AttemptedAt) || !meta.UpdatedAt.Equal(previous.UpdatedAt) ||
				!meta.Complete || meta.Stale || !runner.meta["loan_history_recording"].Stale {
				t.Fatalf("failed reservation consumed an unread refresh or erased coverage: calls %d, history %+v", calls.Load(), meta)
			}
			runner.account = h.account
			if err := runner.saveSnapshot(h.ctx); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			restarted.refreshLoanHistory(h.ctx, now.Add(time.Minute))
			if calls.Load() != 1 || !restarted.meta["loan_history"].Complete || restarted.meta["loan_history_recording"].Stale {
				t.Fatalf("recovered reservation did not retry the unread refresh: calls %d, metadata %+v", calls.Load(), restarted.meta)
			}
		})
	}
}

func TestLoanHistoryReportingRetainsUnknownStatus(t *testing.T) {
	raw := json.RawMessage(`[{"id":"future-loan","status":"future-status","principal_repaid_pence":123}]`)
	reported, skipped, err := reportedLoanHistory(raw, recoveryCompanyID)
	if err != nil || skipped != 0 || !bytes.Equal(reported, raw) {
		t.Fatalf("unfamiliar reporting status was treated as malformed: %s, skipped %d, %v", reported, skipped, err)
	}
}
