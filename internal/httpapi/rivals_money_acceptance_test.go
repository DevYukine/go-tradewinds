package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func rivalsMoneyHTTPJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRivalsMoneyHTTPOverviewKeepsUnknownCreditNullAndPublishedUnits(t *testing.T) {
	database, _ := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	for _, id := range []string{"one", "secondary"} {
		if err := database.SaveAccount(ctx, store.Account{ID: id, Label: id, CompanyID: "company-" + id, Enabled: true},
			store.Credentials{Handle: "player-" + id, Passphrase: "synthetic-passphrase", APIKey: "tw_test_" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.SaveSnapshot(ctx, "one", map[string]json.RawMessage{
		"profile": json.RawMessage(`{"company":{"id":"company-one","name":"Ours"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	start := at.Add(-7 * 24 * time.Hour)
	entry := map[string]any{"rank": 1, "company_name": "Rival", "home_port_id": "london", "net_worth_pence": 436501259, "titles": []string{}}
	board := map[string]any{
		"entries": []any{map[string]any{"rank": 1, "company_name": "Rival", "home_port_id": "london", "growth_bps": 1250,
			"gained_pence": 9876543, "window_start_at": start}},
		"ranked_players": 1, "window_start_at": start, "you": nil,
	}
	raw := rivalsMoneyHTTPJSON(t, map[string]any{
		"season_number": 1, "ranked_players": 3, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour),
		"entries": []any{entry,
			map[string]any{"rank": 2, "company_name": "Own secondary", "home_port_id": "london", "net_worth_pence": 2000000, "titles": []string{}},
			map[string]any{"rank": 3, "company_name": "Ours", "home_port_id": "london", "net_worth_pence": 1000000, "titles": []string{}}},
		"you": map[string]any{"rank": 3, "company_name": "Ours", "net_worth_pence": 1000000, "is_calling_company": true}, "rising": board,
	})
	if err := database.RecordRequest(ctx, "one", "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil, raw, ""); err != nil {
		t.Fatal(err)
	}
	credit := json.RawMessage(`{"company_id":"company-secondary","company_name":"Own secondary","active_loans":0,"active_owed_pence":0,"defaulted_open_loans":0,"defaulted_owed_pence":0,"loans_repaid":0,"loans_defaulted":0,"loans_written_off":0,"loan_written_off_pence":0,"loans_walked_away":0,"loan_walked_away_pence":0,"open_mortgages":0,"futures_claims_open_pence":0,"futures_claims_total":0,"futures_claims_written_off":0,"lent_outstanding_pence":0,"smuggling_convictions":0,"last_default_at":null,"bankrupt_at":null,"last_conviction_at":null}`)
	if err := database.RecordRequest(ctx, "one", "GET", "/v1/loan/credit?company_id=company-secondary", 200, time.Millisecond, nil, credit, ""); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	request := httptest.NewRequest("GET", "/api/rivals?account_id=one", nil)
	request.Host = "127.0.0.1:8585"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("rivals overview failed: status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Error("financial observations must not be cached")
	}
	var report struct {
		Rivals []map[string]json.RawMessage `json:"rivals"`
		Rising json.RawMessage              `json:"rising"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Rivals) != 1 {
		t.Errorf("all registered companies must be excluded even without their own profile snapshot: %s", response.Body.String())
	}
	var rival map[string]json.RawMessage
	for _, row := range report.Rivals {
		if string(row["company_name"]) == `"Rival"` {
			rival = row
		}
	}
	if rival == nil {
		t.Fatalf("Rival missing from overview: %s", response.Body.String())
	}
	for field, want := range map[string]string{
		"net_worth_pence": "436501259", "rising_rank": "1", "growth_bps": "1250", "gained_pence": "9876543",
		"company_id": "null", "credit": "null", "credit_observed_at": "null",
	} {
		if got := string(rival[field]); got != want {
			t.Errorf("/api/rivals %s=%q, want %q (missing observations are explicit null)", field, got, want)
		}
	}
	for _, field := range []string{"observed_ships_24h", "observed_ships_7d", "expansion_contributions"} {
		if len(rival[field]) == 0 {
			t.Errorf("overview omitted %s", field)
		}
	}
	var published struct {
		Ranked  int `json:"ranked_players"`
		Entries []struct {
			Growth int64 `json:"growth_bps"`
			Gained int64 `json:"gained_pence"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(report.Rising, &published); err != nil {
		t.Errorf("rising board missing or malformed: %s (%v)", report.Rising, err)
	} else if published.Ranked != 1 || len(published.Entries) != 1 || published.Entries[0].Growth != 1250 || published.Entries[0].Gained != 9876543 {
		t.Errorf("public rising values were rescaled/recomputed: %+v", published)
	}
}

func TestRivalsMoneyHTTPDiagnosticsPublishStableDatabaseIDs(t *testing.T) {
	database, _ := warningsAcceptanceHTTPStore(t)
	ctx := context.Background()
	if err := database.SaveAccount(ctx, store.Account{ID: "one", Label: "One", CompanyID: "company-one", Enabled: true},
		store.Credentials{Handle: "player-one", Passphrase: "synthetic-passphrase", APIKey: "tw_test_one"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := database.RecordDecision(ctx, "one", "ship", "rejected", "No destination bids", map[string]any{"accepted": false}); err != nil {
			t.Fatal(err)
		}
		if err := database.RecordRequest(ctx, "one", "GET", "/v1/world/clock", 200, time.Millisecond, nil, []byte(`{}`), ""); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewHandler(database, zap.NewNop(), "127.0.0.1:8585")
	get := func(path string, target any) {
		t.Helper()
		request := httptest.NewRequest("GET", path, nil)
		request.Host = "127.0.0.1:8585"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatalf("%s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"decisions", "requests"} {
		t.Run(kind, func(t *testing.T) {
			var history []struct {
				ID int64 `json:"id"`
			}
			get("/api/history?account_id=one&kind="+kind, &history)
			if len(history) != 2 || history[0].ID <= 0 || history[1].ID <= 0 || history[0].ID == history[1].ID {
				t.Fatalf("%s lack distinct permanent ids: %+v", kind, history)
			}
			var dashboard map[string]json.RawMessage
			get("/api/dashboard", &dashboard)
			var initial []struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(dashboard[kind], &initial); err != nil {
				t.Fatal(err)
			}
			if len(initial) != 2 || initial[0].ID != history[0].ID || initial[1].ID != history[1].ID {
				t.Errorf("%s ids changed between dashboard and history: dashboard=%+v history=%+v", kind, initial, history)
			}
		})
	}
}
