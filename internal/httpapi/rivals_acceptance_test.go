package httpapi

// Assumed API: NewHandler(*store.Store, *zap.Logger, string) http.Handler serves
// GET /api/rivals?account_id=... via Store.Rivals(ctx, id, time.Now().UTC()).
// The route uses the existing readError status mapping and snake_case RivalReport
// JSON with integer-pence amounts and RFC3339 source timestamps. No game client.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestRivalsAcceptanceValidationAndUnavailable(t *testing.T) {
	handler := NewHandler(nil, zap.NewNop(), "127.0.0.1:8585")
	for _, test := range []struct {
		name   string
		method string
		query  string
		status int
		code   string
	}{
		{"missing account", "GET", "", 400, "invalid_account"},
		{"empty account", "GET", "?account_id=", 400, "invalid_account"},
		{"blank account", "GET", "?account_id=%20%09", 400, "invalid_account"},
		{"oversized account", "GET", "?account_id=" + strings.Repeat("a", 256), 400, "invalid_account"},
		{"valid account unavailable", "GET", "?account_id=one", 503, "database_unavailable"},
		{"255 bytes allowed", "GET", "?account_id=" + strings.Repeat("a", 255), 503, "database_unavailable"},
		{"mutation denied", "POST", "?account_id=one", 405, "method_not_allowed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, "/api/rivals"+test.query, nil)
			request.Host = "127.0.0.1:8585"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("non-JSON response: %v", err)
			}
			if response.Code != test.status || body.Error != test.code {
				t.Fatalf("status=%d code=%s, want %d/%s", response.Code, body.Error, test.status, test.code)
			}
			if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("recorded rival responses permit caching or content sniffing")
			}
			if test.status == 405 && response.Header().Get("Allow") != "GET" {
				t.Fatal("read-only rivals route omitted Allow: GET")
			}
		})
	}
}

func TestRivalsAcceptanceReadErrorMapping(t *testing.T) {
	handler := &handler{log: zap.NewNop(), readLog: zap.NewNop()}
	for _, test := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unknown account", store.ErrNotFound, 404, "not_found"},
		{"invalid query", store.ErrInvalidInput, 400, "invalid_query"},
		{"bounded read", store.ErrResponseTooLarge, 503, "response_too_large"},
		{"failed database read", errors.New("read failed"), 503, "database_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.readError(context.Background(), response, "rivals", test.err)
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"error":"`+test.code+`"`) {
				t.Fatalf("read error became a misleading response: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestRivalsAcceptanceDatabaseRouteAndWireContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run isolated PostgreSQL route acceptance")
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
	schema := "tradewinds_rivals_http_" + hex.EncodeToString(random[:])
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("drop isolated route schema: %v", err)
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
	if err := database.SaveAccount(ctx, store.Account{ID: "one", Label: "One", CompanyID: "company-one"}, store.Credentials{Handle: "test-player", Passphrase: "test-only-passphrase"}); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"Our fleet"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	raw, err := json.Marshal(map[string]any{
		"season_number": 1, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour),
		"entries": []map[string]any{
			{"rank": 1, "company_name": "Rival", "net_worth_pence": 436501259},
			{"rank": 2, "company_name": "Our fleet", "net_worth_pence": 11613290},
		},
		"you": map[string]any{"rank": 2, "company_name": "Our fleet", "net_worth_pence": 11613290},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.RecordRequest(ctx, "one", "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, raw, ""); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	request := httptest.NewRequest("GET", "/api/rivals?account_id=one", nil)
	request.Host = "127.0.0.1:8585"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("saved fixture route: status=%d body=%s", response.Code, response.Body.String())
	}
	var report struct {
		AccountID   string    `json:"account_id"`
		GeneratedAt time.Time `json:"generated_at"`
		ValuationAt time.Time `json:"valuation_at"`
		Rivals      []struct {
			CompanyName string      `json:"company_name"`
			Worth       json.Number `json:"net_worth_pence"`
		} `json:"rivals"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatalf("snake_case/RFC3339 wire contract: %v", err)
	}
	if report.AccountID != "one" || report.GeneratedAt.IsZero() || !report.ValuationAt.Equal(at) || len(report.Rivals) != 1 || report.Rivals[0].CompanyName != "Rival" || report.Rivals[0].Worth.String() != "436501259" {
		t.Fatalf("lost source time, exact pence or snake_case fields: %+v", report)
	}
	for _, test := range []struct {
		name    string
		close   bool
		account string
		status  int
		code    string
	}{
		{"unknown stored account", false, "missing", 404, "not_found"},
		{"failed read is not empty success", true, "one", 503, "database_unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.close {
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			}
			request := httptest.NewRequest("GET", "/api/rivals?account_id="+test.account, nil)
			request.Host = "127.0.0.1:8585"
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || !strings.Contains(response.Body.String(), `"error":"`+test.code+`"`) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
