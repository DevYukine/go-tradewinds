package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestLoansHandlerValidationAndReadOnlyBoundary(t *testing.T) {
	handler := NewHandler(nil, zap.NewNop(), "127.0.0.1:8585")
	for _, test := range []struct {
		name   string
		method string
		query  string
		status int
		code   string
	}{
		{"all accounts unavailable", "GET", "", 503, "database_unavailable"},
		{"scoped unavailable", "GET", "?account_id=one", 503, "database_unavailable"},
		{"empty scope", "GET", "?account_id=", 400, "invalid_account"},
		{"blank scope", "GET", "?account_id=%20%09", 400, "invalid_account"},
		{"oversized scope", "GET", "?account_id=" + strings.Repeat("a", 256), 400, "invalid_account"},
		{"255 byte scope", "GET", "?account_id=" + strings.Repeat("a", 255), 503, "database_unavailable"},
		{"duplicate scope", "GET", "?account_id=one&account_id=two", 400, "invalid_account"},
		{"duplicate empty scope", "GET", "?account_id=&account_id=one", 400, "invalid_account"},
		{"NUL scope", "GET", "?account_id=one%00two", 400, "invalid_account"},
		{"malformed escape", "GET", "?account_id=%zz", 400, "invalid_account"},
		{"mutation denied", "POST", "?account_id=one", 405, "method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/api/loans"+test.query, nil)
			request.Host = "127.0.0.1:8585"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status || body.Error != test.code {
				t.Fatalf("status=%d body=%s; want %d/%s", response.Code, response.Body.String(), test.status, test.code)
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("loan responses permitted caching or sniffing")
			}
			if test.status == 405 && response.Header().Get("Allow") != "GET" {
				t.Fatal("read-only loan route omitted Allow: GET")
			}
		})
	}
	request := httptest.NewRequest("GET", "/api/loans", nil)
	request.Host = "attacker.invalid"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 403 || !strings.Contains(response.Body.String(), `"error":"host_not_allowed"`) {
		t.Fatalf("loan route escaped host protection: %d %s", response.Code, response.Body.String())
	}
}

func TestLoansHandlerRecordedWireContractAndErrors(t *testing.T) {
	database, fixtureDB := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest("GET", path, nil)
		request.Host = "127.0.0.1:8585"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	empty := get("/api/loans")
	if empty.Code != 200 || !strings.Contains(empty.Body.String(), `"accounts":[]`) {
		t.Fatalf("empty configured scope was not an explicit empty list: %d %s", empty.Code, empty.Body.String())
	}
	const secret = "tw_live_loans_http_secret"
	for _, id := range []string{"one", "two"} {
		if err := database.SaveAccount(ctx, store.Account{ID: id, Label: id, CompanyID: "company-" + id},
			store.Credentials{Handle: "player-" + id, Passphrase: "private-passphrase-" + id, APIKey: secret}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	annual, payment := int32(3000), int64(10_100)
	next, maturity := at.Add(4*time.Hour), at.Add(12*time.Hour)
	active := gameapi.Loan{
		ID: "active", LenderCompanyID: "recorded-bank", BorrowerCompanyID: "company-one", BankingRegion: "north_sea",
		PrincipalPence: 9_007_199_254_740_993, RateBasis: "annual", AnnualRateBPS: &annual, TermGameDays: 30,
		Repayment: "amortised", InstallmentPence: &payment, NextPaymentDueAt: &next, NextPaymentPence: &payment,
		Status: "active", OwedPence: 20_100, PrincipalRepaidPence: 10_000, InterestPaidPence: 100,
		DisbursedAt: &at, MaturesAt: &maturity, CreatedAt: at, UpdatedAt: at,
	}
	closed := active
	closed.ID, closed.Status, closed.OwedPence = "closed", "repaid", 0
	closed.LenderCompanyID = "fallback-lender-long-id"
	closed.PrincipalRepaidPence, closed.InterestPaidPence = 30_000, 300
	closed.ClosedAt = &at
	closed.NextPaymentDueAt, closed.NextPaymentPence = nil, nil
	rows := make([]json.RawMessage, 0, 2)
	for _, loan := range []gameapi.Loan{active, closed} {
		raw, err := json.Marshal(loan)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, raw)
	}
	if err := database.SyncLoanHistory(ctx, "one", rows, at); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtureDB.ExecContext(ctx, `INSERT INTO rival_companies (season_number, company_id, company_name, observed_at)
		VALUES ($1, $2, $3, $4)`, 1, active.LenderCompanyID, "Recorded bank "+secret, at); err != nil {
		t.Fatal(err)
	}
	fresh := map[string]any{"updated_at": at, "attempted_at": at, "complete": true, "stale": false, "error": ""}
	snapshot := make(map[string]json.RawMessage)
	for name, value := range map[string]any{
		"loans": []gameapi.Loan{active},
		"clock": gameapi.Clock{RealUTC: at, Calendar: gameapi.Calendar{TimeScale: 60}},
		"_meta": map[string]any{"loans": fresh, "clock": fresh, "loan_history": fresh},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		snapshot[name] = raw
	}
	if err := database.SaveSnapshot(ctx, "one", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.SyncCompanyFlows(ctx, "one", []store.CompanyLedgerFlow{{
		LedgerID: "interest", Reason: "loan_interest_payment", AmountPence: -350, CreatedAt: at,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	response := get("/api/loans?account_id=one")
	if response.Code != 200 || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("recorded loan response failed or exposed a credential: %d %s", response.Code, response.Body.String())
	}
	var report store.LoanReport
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.UpdatedAt.IsZero() || len(report.Accounts) != 1 {
		t.Fatalf("report envelope changed: %+v", report)
	}
	account := report.Accounts[0]
	if !account.HistoryComplete || account.LoansStale || account.ObservedAt == nil || account.HistoryRefreshedAt == nil ||
		account.HistoryAttemptedAt == nil || len(account.ActiveLoans) != 1 || len(account.ClosedLoans) != 1 ||
		account.ActiveLoans[0].PrincipalPence != active.PrincipalPence || account.ActiveLoans[0].RemainingInstallments == nil ||
		*account.ActiveLoans[0].RemainingInstallments != 3 || !account.ActiveLoans[0].RemainingInstallmentsDerived ||
		account.ClosedLoans[0].RemainingInstallments == nil || *account.ClosedLoans[0].RemainingInstallments != 0 ||
		!strings.HasPrefix(account.ActiveLoans[0].LenderName, "Recorded bank ") || account.ClosedLoans[0].LenderName != "Lender nder-long-id" ||
		account.Totals.PrincipalRepaidPence != 40_000 || account.Totals.InterestPaidPence != 400 || account.Totals.RepaidCount != 1 ||
		account.Totals.LedgerPrincipalRepaidPence != 0 || account.Totals.LedgerInterestPaidPence != 350 || account.Totals.InterestDifferencePence != 50 {
		t.Fatalf("loan API lost flattened fields, schedules or independent totals: %+v", account)
	}
	var wire struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	fields := []string{"account_id", "label", "company_id", "observed_at", "history_refreshed_at", "history_attempted_at", "history_complete", "history_error", "recording_error", "loans_stale", "loans_error", "active_loans", "closed_loans", "totals"}
	if len(wire.Accounts[0]) != len(fields) {
		t.Fatalf("LoanAccount contract has unexpected fields: %s", response.Body.String())
	}
	for _, field := range fields {
		if _, exists := wire.Accounts[0][field]; !exists {
			t.Fatalf("LoanAccount omitted %s", field)
		}
	}
	all := get("/api/loans")
	if err := json.Unmarshal(all.Body.Bytes(), &report); err != nil || all.Code != 200 || len(report.Accounts) != 2 ||
		report.Accounts[1].ActiveLoans == nil || report.Accounts[1].ClosedLoans == nil || report.Accounts[1].HistoryComplete || !report.Accounts[1].LoansStale {
		t.Fatalf("all-account unavailable-data contract changed: %d %s, %v", all.Code, all.Body.String(), err)
	}
	missing := get("/api/loans?account_id=" + url.QueryEscape("missing' OR '1'='1"))
	if missing.Code != 404 || !strings.Contains(missing.Body.String(), `"error":"not_found"`) {
		t.Fatalf("unknown scope was not safely bound: %d %s", missing.Code, missing.Body.String())
	}
	if _, err := fixtureDB.ExecContext(ctx, `INSERT INTO loan_history (account_id, loan_id, data, status, observed_at)
		VALUES ($1, $2, $3::jsonb, $4, $5)`, "one", "legacy-oversized", `{"extension":1e130000}`, "repaid", at); err != nil {
		t.Fatal(err)
	}
	oversized := get("/api/loans?account_id=one")
	if oversized.Code != http.StatusServiceUnavailable || !strings.Contains(oversized.Body.String(), `"error":"response_too_large"`) ||
		!strings.Contains(oversized.Body.String(), "Loan history too large to display.") {
		t.Fatalf("oversized loan history did not give an explicit view-only error: %d %s", oversized.Code, oversized.Body.String())
	}
	if unaffected := get("/api/loans?account_id=two"); unaffected.Code != http.StatusOK {
		t.Fatalf("oversized account broke another scoped loan view: %d %s", unaffected.Code, unaffected.Body.String())
	}
	if unaffected := get("/api/dashboard"); unaffected.Code != http.StatusOK {
		t.Fatalf("oversized loan history broke the dashboard: %d %s", unaffected.Code, unaffected.Body.String())
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	failed := get("/api/loans?account_id=one")
	if failed.Code != 503 || !strings.Contains(failed.Body.String(), `"error":"database_unavailable"`) || strings.Contains(failed.Body.String(), secret) {
		t.Fatalf("failed read became empty success or leaked credentials: %d %s", failed.Code, failed.Body.String())
	}
}
