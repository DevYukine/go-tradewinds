package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func loanReportSnapshot(t *testing.T, at time.Time, loans []gameapi.Loan, metadata map[string]loanDatasetMeta) snapshotRow {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"loans": loans, "_meta": metadata,
		"clock": gameapi.Clock{RealUTC: at, Calendar: gameapi.Calendar{TimeScale: 60}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshotRow{AccountID: "one", Data: data, UpdatedAt: at}
}

func loanReportStored(t *testing.T, loan gameapi.Loan, observedAt time.Time) loanHistoryRow {
	t.Helper()
	return loanHistoryRow{AccountID: "one", LoanID: loan.ID, Data: loanHistoryJSON(t, loan), Status: loan.Status, ObservedAt: observedAt}
}

func TestLoanReportUsesNewerSnapshotAndKeepsFundedClosedHistory(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	active := loanHistoryFixture("active", "one", at)
	closed := loanHistoryFixture("closed", "one", at)
	closed.Status, closed.OwedPence = "repaid", 0
	closed.PrincipalRepaidPence, closed.InterestPaidPence = 30_000, 300
	defaulted := loanHistoryFixture("defaulted", "one", at)
	defaulted.Status, defaulted.OwedPence = "defaulted", 5_000
	defaulted.PrincipalRepaidPence, defaulted.InterestPaidPence = 2_000, 20
	recovered := loanHistoryFixture("recovered", "one", at)
	recovered.Status, recovered.OwedPence = "recovered", 0
	recovered.PrincipalRepaidPence, recovered.InterestPaidPence = 1_000, 10
	writtenOff := loanHistoryFixture("written-off", "one", at)
	writtenOff.Status, writtenOff.OwedPence = "written_off", 0
	writtenOff.PrincipalRepaidPence, writtenOff.InterestPaidPence = 700, 7
	offered := loanHistoryFixture("offered", "one", at)
	offered.Status = "offered"
	stored := make([]loanHistoryRow, 0)
	for _, loan := range []gameapi.Loan{active, closed, defaulted, recovered, writtenOff, offered} {
		stored = append(stored, loanReportStored(t, loan, at.Add(time.Minute)))
	}
	active.OwedPence, active.PrincipalRepaidPence, active.InterestPaidPence = 15_000, 11_000, 120
	active.UpdatedAt = at.Add(2 * time.Minute)
	fresh := loanDatasetMeta{UpdatedAt: active.UpdatedAt, AttemptedAt: active.UpdatedAt, Complete: true}
	snapshot := loanReportSnapshot(t, active.UpdatedAt, []gameapi.Loan{active, defaulted, offered}, map[string]loanDatasetMeta{
		"loans": fresh, "clock": fresh, "loan_history": fresh,
	})
	report, err := loanAccountReport(Account{ID: "one", Label: "One", CompanyID: "company-one"}, stored, snapshot,
		LoanTotals{LedgerInterestPaidPence: 500}, displayCredentials{available: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ActiveLoans) != 2 || len(report.ClosedLoans) != 3 || report.Totals.OwedPence != 20_000 ||
		report.Totals.PrincipalRepaidPence != 44_700 || report.Totals.InterestPaidPence != 457 ||
		report.Totals.RepaidCount != 1 || report.Totals.LedgerPrincipalRepaidPence != 0 ||
		report.Totals.PrincipalDifferencePence != 44_700 || report.Totals.InterestDifferencePence != -43 {
		t.Fatalf("loan lifetime totals or independent ledger cross-check changed: %+v", report)
	}
	if report.LoansStale || !report.HistoryComplete || report.ObservedAt == nil || !report.ObservedAt.Equal(active.UpdatedAt) ||
		report.ActiveLoans[0].RemainingInstallments == nil || *report.ActiveLoans[0].RemainingInstallments != 3 ||
		report.ActiveLoans[1].RemainingInstallments != nil || report.ClosedLoans[0].RemainingInstallments == nil || *report.ClosedLoans[0].RemainingInstallments != 0 {
		t.Fatalf("freshness or schedule uncertainty was hidden: %+v", report)
	}
}

func TestLoanReportFreshCurrentMembershipDoesNotKeepFormerDebtOwed(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	loan := loanHistoryFixture("formerly-active", "one", at)
	stored := []loanHistoryRow{loanReportStored(t, loan, at)}
	fresh := loanDatasetMeta{UpdatedAt: at.Add(time.Minute), Complete: true}
	for _, test := range []struct {
		name     string
		metadata loanDatasetMeta
		loans    []gameapi.Loan
		wantDebt bool
	}{
		{"fresh complete empty debt", fresh, []gameapi.Loan{}, false},
		{"stale last-good empty debt", loanDatasetMeta{UpdatedAt: fresh.UpdatedAt, Complete: true, Stale: true}, []gameapi.Loan{}, false},
		{"failed last-good empty debt", loanDatasetMeta{UpdatedAt: fresh.UpdatedAt, Error: "fetch failed"}, []gameapi.Loan{}, false},
		{"missing debt list", fresh, nil, true},
		{"observation predates new debt", loanDatasetMeta{UpdatedAt: at.Add(-time.Minute), Complete: true}, []gameapi.Loan{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := loanReportSnapshot(t, fresh.UpdatedAt, test.loans, map[string]loanDatasetMeta{
				"loans": test.metadata, "clock": fresh, "loan_history": fresh,
			})
			report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
			if err != nil {
				t.Fatal(err)
			}
			if test.wantDebt {
				if len(report.ActiveLoans) != 1 || report.Totals.OwedPence != loan.OwedPence {
					t.Fatalf("lost historical fallback without current authority: %+v", report)
				}
			} else if len(report.ActiveLoans) != 0 || report.Totals.OwedPence != 0 || report.LoansStale != !test.metadata.usable() || report.HistoryComplete || report.HistoryError == "" {
				t.Fatalf("former open history became current debt or falsely complete history: %+v", report)
			}
			if report.Totals.PrincipalRepaidPence != loan.PrincipalRepaidPence || report.Totals.InterestPaidPence != loan.InterestPaidPence {
				t.Fatalf("known repayments disappeared while closed capture was pending: %+v", report)
			}
		})
	}
}

func TestLoanReportDoesNotRegressSourceOrObservationAndScrubsFreshness(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	latest := loanHistoryFixture("same", "one", at)
	latest.UpdatedAt = at.Add(time.Hour)
	stored := []loanHistoryRow{loanReportStored(t, latest, at.Add(2*time.Hour))}
	for _, test := range []struct {
		name     string
		updated  time.Time
		observed time.Time
	}{
		{"older source, later receipt", at, at.Add(3 * time.Hour)},
		{"same source, older receipt", latest.UpdatedAt, at.Add(time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			old := latest
			old.UpdatedAt, old.OwedPence = test.updated, 999_999
			meta := loanDatasetMeta{UpdatedAt: test.observed, Complete: true}
			snapshot := loanReportSnapshot(t, test.observed, []gameapi.Loan{old}, map[string]loanDatasetMeta{"loans": meta})
			report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
			if err != nil || len(report.ActiveLoans) != 1 || report.ActiveLoans[0].OwedPence != latest.OwedPence {
				t.Fatalf("old snapshot overlaid newer durable state: %+v, %v", report, err)
			}
		})
	}
	const secret = "private-loan-display-secret"
	failed := loanDatasetMeta{UpdatedAt: at, AttemptedAt: at.Add(time.Hour), Error: "failed " + secret, Stale: true}
	snapshot := loanReportSnapshot(t, at, []gameapi.Loan{}, map[string]loanDatasetMeta{"loans": failed, "loan_history": failed})
	report, err := loanAccountReport(Account{ID: "one", Label: secret, CompanyID: "company-one"}, stored, snapshot,
		LoanTotals{}, displayCredentials{available: true, secrets: []string{secret}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) || report.HistoryComplete || !report.LoansStale || report.HistoryAttemptedAt == nil || report.HistoryError == "" {
		t.Fatalf("freshness or display secrets mishandled: %s", payload)
	}
}

func TestLoanReportKeepsHistoryFailureWhenFormerDebtDisappears(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	loan := loanHistoryFixture("formerly-active", "one", at)
	stored := []loanHistoryRow{loanReportStored(t, loan, at)}
	current := loanDatasetMeta{UpdatedAt: at.Add(time.Minute), Complete: true, Stale: true}
	history := loanDatasetMeta{UpdatedAt: at, Stale: true, Error: "Full borrower history read failed."}
	snapshot := loanReportSnapshot(t, current.UpdatedAt, []gameapi.Loan{}, map[string]loanDatasetMeta{
		"loans": current, "loan_history": history,
	})
	report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
	if err != nil || len(report.ActiveLoans) != 0 || report.Totals.OwedPence != 0 ||
		report.Totals.PrincipalRepaidPence != loan.PrincipalRepaidPence || report.HistoryComplete || report.HistoryError != history.Error {
		t.Fatalf("last-good current membership hid the history failure or revived old debt: %+v, %v", report, err)
	}
}

func TestLoanReportDebtFreshnessDoesNotAdvanceForClosedHistory(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	active := loanHistoryFixture("active", "one", at)
	closed := loanHistoryFixture("closed", "one", at.Add(time.Hour))
	closed.Status, closed.OwedPence = "repaid", 0
	for _, test := range []struct {
		name     string
		current  []gameapi.Loan
		metadata loanDatasetMeta
		active   bool
		want     *time.Time
	}{
		{"fresh current debt", []gameapi.Loan{active}, loanDatasetMeta{UpdatedAt: at, Complete: true}, true, &at},
		{"stale current debt", []gameapi.Loan{active}, loanDatasetMeta{UpdatedAt: at, Complete: true, Stale: true}, true, &at},
		{"receipt without debt list", nil, loanDatasetMeta{}, true, &at},
		{"closed history without debt list", nil, loanDatasetMeta{}, false, nil},
		{"observed empty debt", []gameapi.Loan{}, loanDatasetMeta{UpdatedAt: at, Complete: true}, false, &at},
	} {
		t.Run(test.name, func(t *testing.T) {
			stored := []loanHistoryRow{loanReportStored(t, closed, at.Add(2*time.Hour))}
			if test.active {
				stored = append(stored, loanReportStored(t, active, at))
			}
			snapshot := loanReportSnapshot(t, at.Add(2*time.Hour), test.current, map[string]loanDatasetMeta{
				"loans": test.metadata, "loan_history": {UpdatedAt: at.Add(2 * time.Hour), Complete: true},
			})
			report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
			if err != nil {
				t.Fatal(err)
			}
			if test.want == nil {
				if report.ObservedAt != nil {
					t.Fatalf("closed history fabricated current debt freshness: %+v", report)
				}
			} else if report.ObservedAt == nil || !report.ObservedAt.Equal(*test.want) {
				t.Fatalf("closed history advanced current debt freshness: got %v, want %s", report.ObservedAt, test.want)
			}
		})
	}
}

func TestLoanReportSeparatesRecordingFailureFromCompleteHistory(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	const secret = "private-recording-error"
	history := loanDatasetMeta{UpdatedAt: at, AttemptedAt: at, Complete: true}
	snapshot := loanReportSnapshot(t, at, []gameapi.Loan{}, map[string]loanDatasetMeta{
		"loans": history, "loan_history": history,
		"loan_history_recording": {Stale: true, Error: "Recording failed: " + secret},
	})
	report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, nil, snapshot, LoanTotals{},
		displayCredentials{available: true, secrets: []string{secret}})
	if err != nil || !report.HistoryComplete || report.HistoryError != "" || report.RecordingError == "" ||
		strings.Contains(report.RecordingError, secret) || report.LoansStale {
		t.Fatalf("optional recording failure changed full-history or debt completeness: %+v, %v", report, err)
	}
}

func TestLoanReportFullyRepaidCountRequiresZeroOwed(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	paid := loanHistoryFixture("paid", "one", at)
	paid.Status, paid.OwedPence = "repaid", 0
	inconsistent := paid
	inconsistent.ID, inconsistent.OwedPence = "nonzero-closed", 1
	report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"},
		[]loanHistoryRow{loanReportStored(t, paid, at), loanReportStored(t, inconsistent, at)},
		snapshotRow{}, LoanTotals{}, displayCredentials{available: true})
	if err != nil || len(report.ClosedLoans) != 2 || report.Totals.RepaidCount != 1 {
		t.Fatalf("repaid status with a nonzero balance counted as fully repaid: %+v, %v", report, err)
	}
}

func TestLoanReportUnrecognizedStatusMarksCoverageIncomplete(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	unknown := loanHistoryFixture("future-status", "one", at)
	unknown.Status = "future-status"
	stored := []loanHistoryRow{loanReportStored(t, unknown, at)}
	fresh := loanDatasetMeta{UpdatedAt: at.Add(time.Minute), AttemptedAt: at.Add(time.Minute), Complete: true}
	snapshot := loanReportSnapshot(t, fresh.UpdatedAt, []gameapi.Loan{}, map[string]loanDatasetMeta{"loans": fresh, "loan_history": fresh})
	report, err := loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
	if err != nil || report.HistoryComplete || !strings.Contains(report.HistoryError, "unrecognized statuses") ||
		len(report.ActiveLoans) != 0 || len(report.ClosedLoans) != 0 {
		t.Fatalf("unknown status was silently represented as complete history: %+v, %v", report, err)
	}
	known := unknown
	known.Status, known.UpdatedAt = "active", fresh.UpdatedAt
	snapshot = loanReportSnapshot(t, fresh.UpdatedAt, []gameapi.Loan{known}, map[string]loanDatasetMeta{"loans": fresh, "loan_history": fresh})
	report, err = loanAccountReport(Account{ID: "one", CompanyID: "company-one"}, stored, snapshot, LoanTotals{}, displayCredentials{available: true})
	if err != nil || !report.HistoryComplete || report.HistoryError != "" || len(report.ActiveLoans) != 1 {
		t.Fatalf("a superseded unknown status kept coverage incomplete: %+v, %v", report, err)
	}
}

func TestLoansRecordedLenderNamesPermanentLedgerAndAccountScope(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "one")
	saveTestAccount(t, db, "two")
	at := time.Now().UTC().Truncate(time.Microsecond)
	loan := loanHistoryFixture("one-loan", "one", at)
	if err := db.SyncLoanHistory(ctx, "one", []json.RawMessage{loanHistoryJSON(t, loan)}, at); err != nil {
		t.Fatal(err)
	}
	if err := recordRivalCompany(db.db, 1, loan.LenderCompanyID, "Recorded lender "+credentials.APIKey, at); err != nil {
		t.Fatal(err)
	}
	if err := db.SyncCompanyFlows(ctx, "one", []CompanyLedgerFlow{
		{LedgerID: "interest-one", Reason: "loan_interest_payment", AmountPence: -150, CreatedAt: at},
		{LedgerID: "lending-receipt", Reason: "loan_interest_payment", AmountPence: 999, CreatedAt: at},
		{LedgerID: "not-loan", Reason: "upkeep", AmountPence: -700, CreatedAt: at},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"ledger": json.RawMessage(`[{"id":"not-permanent","reason":"loan_principal_repayment","amount_pence":999999}]`),
	}); err != nil {
		t.Fatal(err)
	}
	report, err := db.Loans(ctx, "one")
	if err != nil || len(report.Accounts) != 1 {
		t.Fatalf("scoped report: %+v, %v", report, err)
	}
	account := report.Accounts[0]
	if len(account.ActiveLoans) != 1 || !strings.HasPrefix(account.ActiveLoans[0].LenderName, "Recorded lender ") ||
		strings.Contains(account.ActiveLoans[0].LenderName, credentials.APIKey) || account.Totals.LedgerPrincipalRepaidPence != 0 ||
		account.Totals.LedgerInterestPaidPence != 150 || account.Totals.PrincipalRepaidPence != loan.PrincipalRepaidPence || account.Totals.InterestDifferencePence != -50 {
		t.Fatalf("lender names, ledger basis or lifetime authority changed: %+v", account)
	}
	all, err := db.Loans(ctx, "")
	if err != nil || len(all.Accounts) != 2 || all.Accounts[1].AccountID != "two" || all.Accounts[1].ActiveLoans == nil || all.Accounts[1].ClosedLoans == nil {
		t.Fatalf("all-account missing-data contract: %+v, %v", all, err)
	}
	if _, err := db.Loans(ctx, "missing' OR '1'='1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account was interpolated or silently succeeded: %v", err)
	}
	if _, err := (*Store)(nil).Loans(ctx, "  "); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid account accepted: %v", err)
	}
}

func TestLoansPreflightRejectsNumericExpansionWithoutHydrating(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "numeric")
	if err := db.db.Exec(`INSERT INTO loan_history (account_id, loan_id, data, status, observed_at)
		VALUES (?, ?, ?::jsonb, ?, ?)`, "numeric", "legacy-unsafe", `{"extension":1e130000}`, "repaid", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.Loans(context.Background(), "numeric"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("unsafe stored extension reached hydration: %v", err)
	}
}

func TestLoansPreflightRejectsUnsafeSnapshotNumbersWithoutHydrating(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "numeric-snapshot")
	if err := db.db.Exec(`INSERT INTO snapshots (account_id, data, updated_at) VALUES (?, ?::jsonb, ?)`,
		"numeric-snapshot", `{"loans":[{"extension":1e130000}]}`, time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := db.Loans(context.Background(), "numeric-snapshot"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("unsafe stored snapshot reached hydration: %v", err)
	}
}

func TestLoanTotalsRejectOverflow(t *testing.T) {
	total := int64(math.MaxInt64)
	if err := addLoanPence(&total, 1); !errors.Is(err, ErrInvalidInput) || total != math.MaxInt64 {
		t.Fatalf("loan totals wrapped: %d, %v", total, err)
	}
}

func TestLoansBoundRecordedLenderNameBeforeFanOut(t *testing.T) {
	db, _, _ := integrationStore(t)
	ctx := context.Background()
	credentials := saveTestAccount(t, db, "names")
	at := time.Now().UTC().Truncate(time.Microsecond)
	loan := loanHistoryFixture("name-0", "names", at)
	rows := make([]json.RawMessage, 1000)
	for index := range rows {
		loan.ID = fmt.Sprintf("name-%d", index)
		rows[index] = loanHistoryJSON(t, loan)
	}
	if err := db.SyncLoanHistory(ctx, "names", rows, at); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("x", 4*1024*1024) + credentials.APIKey
	if err := db.db.Exec(`INSERT INTO configured_company_identities (company_id, name, observed_at) VALUES (?, ?, ?)`,
		loan.LenderCompanyID, oversized, at).Error; err != nil {
		t.Fatal(err)
	}
	report, err := db.Loans(ctx, "names")
	if err != nil || len(report.Accounts) != 1 || len(report.Accounts[0].ActiveLoans) != len(rows) {
		t.Fatalf("bounded lender report: %v", err)
	}
	for _, entry := range report.Accounts[0].ActiveLoans {
		if len(entry.LenderName) > maxLoanLenderNameBytes || !strings.HasPrefix(entry.LenderName, "Lender ") ||
			strings.Contains(entry.LenderName, credentials.APIKey) {
			t.Fatalf("oversized lender name reached a repeated display row: %q", entry.LenderName)
		}
	}
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > maxDashboardSnapshotBytes {
		t.Fatalf("lender repetition exceeded the response budget: %d, %v", len(encoded), err)
	}
}
