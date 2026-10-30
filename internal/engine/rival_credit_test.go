package engine

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestRivalCreditIDsUsePublishedPartiesOnly(t *testing.T) {
	ids := make(map[string]bool)
	addRivalCreditIDs(ids, []byte(`[{"holder_company_id":"monopoly"},{"company_id":"bank"},{"borrower_company_id":"borrower"},{"creator_company_id":"creator","seller_company_id":"seller","buyer_company_id":null},{"company_name":"No published ID","rank":1}]`))
	// An event delivery envelope is not a directory or an acting company.
	addRivalCreditIDs(ids, []byte(`{"company_id":"event-recipient","payload":{"ship_name":"Unattributable"}}`))
	addRivalCreditIDs(ids, encode([]any{map[string]string{"company_id": "   "}, map[string]string{"company_id": strings.Repeat("x", 256)}}))
	want := map[string]bool{"monopoly": true, "bank": true, "borrower": true, "creator": true, "seller": true}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("published IDs=%v, want %v", ids, want)
	}
}

func TestRivalCreditCollectorNeverRetriesBudgetedServerFailure(t *testing.T) {
	h := newRecoveryHarness(t)
	at := time.Now().UTC().Add(-time.Minute)
	if err := h.db.RecordRequest(h.ctx, h.account.ID, "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil,
		encode(map[string]any{"season_number": 1, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour), "entries": []any{}, "rising": map[string]any{"entries": []any{}, "ranked_players": 0}}), ""); err != nil {
		t.Fatal(err)
	}
	var lookups atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/loan/credit" {
			lookups.Add(1)
			h.api.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
			return
		}
		h.api.writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(server.Close)
	h.api.server = server
	for range 2 {
		runner := h.runner(primaryTestKey)
		runner.snapshot["monopolies"] = encode([]any{map[string]string{"holder_company_id": "90000000-0000-4000-8000-000000000001"}})
		if err := runner.refreshRivalCredit(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls := lookups.Load(); calls != 1 {
		t.Fatalf("a reserved failed lookup was retried or forgotten across runners: %d attempts", calls)
	}
}

func TestRivalCreditCollectorFailureIsOptional(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	runner.account.ID = " "
	if err := runner.refreshRivalCredit(h.ctx); err != nil {
		t.Fatalf("optional candidate failure halted the runner: %v", err)
	}
	if _, exists := runner.meta["rival_credit"]; exists {
		t.Fatal("collector throttle appeared as dataset health")
	}
	if logs.FilterMessage("rival credit observation failed").Len() != 1 {
		t.Fatal("optional collector failure did not produce a warning")
	}
}

func TestRivalCreditCollectorCachesDiscoveryHourly(t *testing.T) {
	h := newRecoveryHarness(t)
	at := time.Now().UTC().Add(-time.Minute)
	if err := h.db.RecordRequest(h.ctx, h.account.ID, "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil,
		encode(map[string]any{"season_number": 1, "refreshed_at": at, "next_refresh_at": at.Add(time.Hour), "entries": []any{}, "rising": map[string]any{"entries": []any{}, "ranked_players": 0}}), ""); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h.api.writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(server.Close)
	h.api.server = server
	runner := h.runner(primaryTestKey)
	for range 2 {
		if err := runner.refreshRivalCredit(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("directories were not cached for an hour: %d requests", calls.Load())
	}
	if _, exists := runner.meta["rival_credit"]; exists {
		t.Fatal("collector throttle appeared as dataset health")
	}
}

func TestRivalCreditCollectorWaitsForPublicationSeason(t *testing.T) {
	h := newRecoveryHarness(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		h.api.writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(server.Close)
	h.api.server = server
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	runner.snapshot["monopolies"] = encode([]any{map[string]string{"holder_company_id": "unseasoned-rival"}})
	if err := runner.refreshRivalCredit(h.ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("unretainable intelligence consumed upstream requests: %d", calls.Load())
	}
	if logs.Len() != 0 {
		t.Fatalf("expected publication wait emitted warnings: %+v", logs.All())
	}
}

func TestRivalCreditCollectorReservationFailureIsOptional(t *testing.T) {
	h := newRecoveryHarness(t)
	at := time.Now().UTC().Add(-time.Minute)
	if err := h.db.RecordRequest(h.ctx, h.account.ID, "GET", "/v1/world/leaderboard", 200, time.Millisecond, nil,
		encode(map[string]any{"season_number": 1, "refreshed_at": at, "rising": map[string]any{"entries": []any{}, "ranked_players": 0}}), ""); err != nil {
		t.Fatal(err)
	}
	var creditCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/loan/credit" {
			creditCalls.Add(1)
		}
		h.api.writeJSON(w, http.StatusOK, []any{})
	}))
	t.Cleanup(server.Close)
	h.api.server = server
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.WarnLevel)
	runner.log = zap.New(core)
	// Discovery is read-only; reservation must fail its account foreign key.
	runner.account.ID = "missing-account"
	runner.snapshot["monopolies"] = encode([]any{map[string]string{"holder_company_id": "observed-rival"}})
	if err := runner.refreshRivalCredit(h.ctx); err != nil {
		t.Fatalf("optional reservation error stopped execution: %v", err)
	}
	if creditCalls.Load() != 0 {
		t.Fatal("collector sent an unreserved credit lookup")
	}
	if logs.FilterMessage("rival credit observation failed").Len() != 1 {
		t.Fatal("reservation failure did not produce a warning")
	}
}
