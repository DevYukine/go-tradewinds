package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Drive the existing refresh seam and committed request observer. No test-only
// collector API is required: the durable budget includes every real credit GET,
// including failed observations, and survives construction of another Runner.
func TestRivalCreditMoneyDurableHourlyAndCompanyBudgets(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h := newRecoveryHarness(t)
			upstream := h.api.server.Config.Handler
			at := time.Now().UTC()
			publication := map[string]any{"season_number": 1, "ranked_players": 0,
				"refreshed_at": at, "next_refresh_at": at.Add(time.Hour), "entries": []any{}, "you": nil,
				"rising": map[string]any{"entries": []any{}, "ranked_players": 0, "window_start_at": nil, "you": nil}}
			if err := h.db.RecordRequest(h.ctx, h.account.ID, "GET", "/v1/world/leaderboard", http.StatusOK, time.Millisecond, nil, encode(publication), ""); err != nil {
				t.Fatal(err)
			}
			ids := make([]string, 25)
			names := make(map[string]string, len(ids))
			for i := range ids {
				ids[i] = fmt.Sprintf("90000000-0000-4000-8000-%012d", i+1)
				names[ids[i]] = fmt.Sprintf("Observed rival %02d", i+1)
			}
			credit := func(id string) map[string]any {
				return map[string]any{
					"company_id": id, "company_name": names[id],
					"active_loans": 0, "active_owed_pence": 0, "defaulted_open_loans": 0, "defaulted_owed_pence": 0,
					"loans_repaid": 0, "loans_defaulted": 0, "loans_written_off": 0, "loan_written_off_pence": 0,
					"loans_walked_away": 0, "loan_walked_away_pence": 0, "open_mortgages": 0,
					"futures_claims_open_pence": 0, "futures_claims_total": 0, "futures_claims_written_off": 0,
					"lent_outstanding_pence": 0, "smuggling_convictions": 0,
					"last_default_at": nil, "bankrupt_at": nil, "last_conviction_at": nil,
				}
			}
			// Nineteen observations in the current real hour leave at most one
			// lookup. A 429 is still an HTTP attempt, not a free extra allowance.
			for _, id := range ids[:19] {
				body := encode(credit(id))
				if status == http.StatusTooManyRequests {
					body = []byte(`{"error":"rate_limit_exceeded"}`)
				}
				if err := h.db.RecordRequest(h.ctx, h.account.ID, "GET", "/v1/loan/credit?company_id="+id, status, time.Millisecond, nil, body, ""); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			var lookedUp []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/world/leaderboard":
					h.api.writeJSON(w, http.StatusOK, publication)
				case "/v1/world/monopolies":
					grants := []any{}
					for i, id := range ids {
						grants = append(grants, map[string]any{"id": fmt.Sprintf("80000000-0000-4000-8000-%012d", i+1),
							"holder_company_id": id, "holder_company_name": names[id], "banking_region": "north",
							"commodity_id": "grain", "price_pence": 100, "term_game_days": 10,
							"starts_at": at.Add(-time.Hour), "expires_at": at.Add(time.Hour), "in_force": true})
					}
					h.api.writeJSON(w, http.StatusOK, grants)
				case "/v1/loan/requests", "/v1/bank/list":
					h.api.writeJSON(w, http.StatusOK, []any{})
				case "/v1/loan/credit":
					id := r.URL.Query().Get("company_id")
					mu.Lock()
					lookedUp = append(lookedUp, id)
					mu.Unlock()
					if _, ok := names[id]; !ok {
						t.Errorf("collector invented a company identity: %q", id)
					}
					h.api.writeJSON(w, http.StatusOK, credit(id))
				default:
					upstream.ServeHTTP(w, r)
				}
			}))
			t.Cleanup(server.Close)
			h.api.server = server
			for range 3 {
				// The same durable store and a fresh Runner model a process restart.
				runner := h.runner(primaryTestKey)
				if err := runner.refresh(h.ctx); err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if status == http.StatusOK && len(lookedUp) == 0 {
				t.Error("rival credit collection never queried an observed company")
			}
			if len(lookedUp) > 1 {
				t.Errorf("19 existing attempts + %d fresh attempts exceed 20 per real hour across restarts: %v", len(lookedUp), lookedUp)
			}
			seen := map[string]bool{}
			for _, id := range lookedUp {
				if seen[id] {
					t.Errorf("company %s was queried twice within six real hours", id)
				}
				seen[id] = true
				for _, recent := range ids[:19] {
					if id == recent {
						t.Errorf("company %s with a recent lookup was queried again inside six real hours", id)
					}
				}
			}
		})
	}
}
