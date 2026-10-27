package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestWarningsAcceptanceUnavailableIsNotAllClear(t *testing.T) {
	handler := NewHandler(nil, zap.NewNop(), "127.0.0.1:8585")
	request := httptest.NewRequest("GET", "/api/warnings", nil)
	request.Host = "127.0.0.1:8585"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != 503 || string(body["error"]) != `"database_unavailable"` || body["accounts"] != nil {
		t.Fatalf("nil database became all-clear: %d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unavailable warning state can be cached")
	}
}

func warningsAcceptanceHTTPStore(t *testing.T) (*store.Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated PostgreSQL warnings route acceptance")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	schema := "tradewinds_warnings_http_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated warnings schema: %v", err)
		}
		_ = admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") {
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " search_path=" + schema
	}
	database, err := store.Open(ctx, dsn, bytes.Repeat([]byte{0x42}, 32), zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	fixtureDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixtureDB.Close() })
	return database, fixtureDB
}

func TestWarningsAcceptanceDatabaseWireContractAndFailedRead(t *testing.T) {
	database, fixtureDB := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	if err := database.SaveAccount(ctx, store.Account{ID: "one", Label: "One", CompanyID: "company-one", Enabled: true}, store.Credentials{Handle: "test-player", Passphrase: "test-only-passphrase", APIKey: "tw_live_test_only"}); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC().Add(-14 * time.Minute).Truncate(time.Second)
	raw, err := json.Marshal(map[string]any{"observed_at": since, "mode": "execute"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{"_engine": raw}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	read := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest("GET", "/api/warnings", nil)
		request.Host = "127.0.0.1:8585"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	before := time.Now().UTC()
	response := read()
	after := time.Now().UTC()
	if response.Code != 200 {
		t.Fatalf("recorded warnings: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		GeneratedAt time.Time `json:"generated_at"`
		Build       struct {
			Revision  *string   `json:"revision"`
			Modified  bool      `json:"modified"`
			StartedAt time.Time `json:"started_at"`
		} `json:"build"`
		Accounts    []store.AccountWarnings `json:"accounts"`
		Unmonitored []struct {
			AccountID string `json:"account_id"`
			Reason    string `json:"reason"`
		} `json:"unmonitored"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.GeneratedAt.Before(before) || body.GeneratedAt.After(after) || body.Build.StartedAt.IsZero() || body.Build.StartedAt.After(before) {
		t.Fatalf("source/build clocks missing or invented: %+v", body)
	}
	if body.Unmonitored == nil || len(body.Unmonitored) != 0 {
		t.Fatalf("unmonitored must be an explicit empty array when all accounts are monitored: %s", response.Body.String())
	}
	var revision *string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && setting.Value != "" {
				value := setting.Value
				revision = &value
			}
		}
	}
	if revision == nil {
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		var build map[string]json.RawMessage
		if err := json.Unmarshal(wire["build"], &build); err != nil {
			t.Fatal(err)
		}
		if string(build["revision"]) != "null" {
			t.Fatalf("absent VCS revision must be explicit null, not an invented identifier: %s", wire["build"])
		}
	} else if body.Build.Revision == nil || *body.Build.Revision != *revision {
		t.Fatalf("build revision does not match binary VCS metadata: %+v", body.Build)
	}
	if len(body.Accounts) != 1 || body.Accounts[0].AccountID != "one" || body.Accounts[0].Label != "One" || !body.Accounts[0].Available || body.Accounts[0].ErrorCode != "" || len(body.Accounts[0].Warnings) != 1 {
		t.Fatalf("account/warnings wire shape: %+v", body.Accounts)
	}
	warning := body.Accounts[0].Warnings[0]
	if warning.Code != "controller_stalled" || warning.Severity != "critical" || !warning.Since.Equal(since) {
		t.Fatalf("recorded stale controller missing: %+v", warning)
	}
	firstStart := body.Build.StartedAt
	response = read()
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Build.StartedAt.Equal(firstStart) {
		t.Fatal("started_at changes per request rather than describing process start")
	}
	if err := database.SaveAccount(ctx, store.Account{ID: "bad", Label: "Bad snapshot", CompanyID: "company-bad", Enabled: true}, store.Credentials{Handle: "bad-player", Passphrase: "test-only-passphrase", APIKey: "tw_live_bad_test_only"}); err != nil {
		t.Fatal(err)
	}
	privatePayload := "private-snapshot-value-must-not-leak"
	if err := database.SaveSnapshot(ctx, "bad", map[string]json.RawMessage{"ships": json.RawMessage(`"` + privatePayload + `"`)}); err != nil {
		t.Fatal(err)
	}
	response = read()
	if response.Code != 200 {
		t.Fatalf("one malformed account erased healthy account warnings: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Accounts) != 2 {
		t.Fatalf("unavailable account disappeared from response scope: %+v", body.Accounts)
	}
	for _, account := range body.Accounts {
		if account.AccountID == "bad" {
			if account.Available || account.ErrorCode != "snapshot_invalid" || account.Warnings == nil || len(account.Warnings) != 0 || account.Suppressed == nil || len(account.Suppressed) != 0 {
				t.Fatalf("unavailable account must have explicit safe status, not an all-clear: %+v", account)
			}
		} else if account.AccountID != "one" || !account.Available || len(account.Warnings) != 1 {
			t.Fatalf("healthy warning lost while another account was unavailable: %+v", account)
		}
	}
	if strings.Contains(response.Body.String(), privatePayload) {
		t.Fatal("account decode failure leaked snapshot content")
	}
	for _, fixture := range []struct {
		id        string
		enabled   bool
		companyID string
		apiKey    string
	}{
		{"disabled", false, "company-disabled", "tw_live_disabled_test"},
		{"unregistered", true, "", "tw_live_unregistered_test"},
		{"no-api-key", true, "company-no-api-key", ""},
		{"corrupt", true, "company-corrupt", "tw_live_corrupt_test"},
	} {
		if err := database.SaveAccount(ctx, store.Account{ID: fixture.id, Label: fixture.id, CompanyID: fixture.companyID, Enabled: fixture.enabled}, store.Credentials{Handle: fixture.id, Passphrase: "test-only-passphrase", APIKey: fixture.apiKey}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixtureDB.ExecContext(ctx, "UPDATE account_secrets SET ciphertext = $1 WHERE account_id = $2", []byte("corrupt"), "corrupt"); err != nil {
		t.Fatal(err)
	}
	response = read()
	if response.Code != 200 {
		t.Fatalf("explicit monitoring exclusions prevented warning read: %d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantReasons := map[string]string{"disabled": "disabled", "unregistered": "unregistered", "no-api-key": "no_api_key"}
	if body.Unmonitored == nil || len(body.Unmonitored) != len(wantReasons) {
		t.Fatalf("monitoring exclusions missing or unexpectedly expanded: %+v", body.Unmonitored)
	}
	seen := make(map[string]bool)
	for _, account := range body.Unmonitored {
		wantReason, known := wantReasons[account.AccountID]
		if !known || wantReason != account.Reason || seen[account.AccountID] {
			t.Fatalf("wrong or duplicate monitoring exclusion: %+v", account)
		}
		seen[account.AccountID] = true
	}
	if len(body.Accounts) != 3 {
		t.Fatalf("excluded accounts leaked into monitored scope or unavailable accounts disappeared: %+v", body.Accounts)
	}
	seen = make(map[string]bool)
	for _, account := range body.Accounts {
		if _, excluded := wantReasons[account.AccountID]; excluded || seen[account.AccountID] {
			t.Fatalf("account is monitored twice or also excluded: %+v", account)
		}
		seen[account.AccountID] = true
		switch account.AccountID {
		case "one":
			if !account.Available || len(account.Warnings) != 1 {
				t.Fatalf("healthy warning lost after scope exclusions: %+v", account)
			}
		case "bad", "corrupt":
			wantCode := "snapshot_invalid"
			if account.AccountID == "corrupt" {
				wantCode = "credentials_unavailable"
			}
			if account.Available || account.ErrorCode != wantCode || account.Warnings == nil || len(account.Warnings) != 0 {
				t.Fatalf("corrupt data must remain monitored and unavailable, not excluded: %+v", account)
			}
		default:
			t.Fatalf("unexpected monitored account: %+v", account)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	response = read()
	if response.Code != 503 || !strings.Contains(response.Body.String(), `"error":"database_unavailable"`) {
		t.Fatalf("failed read became a successful all-clear: %d %s", response.Code, response.Body.String())
	}
}
