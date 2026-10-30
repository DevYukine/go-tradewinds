package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

// The ledger is newest first. Paging must stop at the first page that reaches
// the floor instead of reading the company's whole history.
func TestReadPagesStopsAtLedgerFloor(t *testing.T) {
	now := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	entry := func(index int) map[string]any {
		return map[string]any{
			"id":         fmt.Sprintf("00000000-0000-7000-8000-%012d", index),
			"created_at": now.Add(-time.Duration(index) * time.Hour),
		}
	}
	var pages atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if r.URL.Query().Get("cursor") != "" {
			start = 200
		}
		pages.Add(1)
		rows := make([]map[string]any, 0, 200)
		for index := start; index < start+200; index++ {
			rows = append(rows, entry(index))
		}
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	api := gameapi.New(server.URL, "synthetic-key", nil)

	raw, err := readPages(context.Background(), api, "/v1/treasury/ledger", now.Add(-150*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 151 {
		t.Fatalf("kept %d rows, want the 151 entries at or after the floor", len(rows))
	}
	if got := pages.Load(); got != 1 {
		t.Fatalf("requested %d pages, want paging to stop at the floor", got)
	}

	raw, err = readPages(context.Background(), api, "/v1/treasury/ledger", now.Add(-250*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 251 || pages.Load() != 3 {
		t.Fatalf("kept %d rows over %d pages, want 251 rows from a second page", len(rows), pages.Load())
	}
}

// Only missions that claim ledger expenses may hold older entries, and none
// may hold the whole history.
func TestLedgerFloorFollowsExpenseMissionsWithinCap(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC().Truncate(time.Second)
	put := func(shipID string, startedAt time.Time, baseline map[string]bool) {
		data := map[string]any{"started_at": startedAt, "initial_ledger_ids": baseline}
		if err := h.db.PutMission(h.ctx, &store.Mission{
			AccountID: h.account.ID,
			ShipID:    shipID,
			Phase:     "sailing",
			Data:      encode(data),
		}); err != nil {
			t.Fatalf("persist mission %s: %v", shipID, err)
		}
	}
	floor := func() time.Time {
		t.Helper()
		value, err := runner.ledgerFloor(h.ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	put("00000000-0000-7000-8000-00000000c0de", now.Add(-160*time.Hour), nil)
	if got, want := floor(), now.Add(-ledgerRetention); !got.Equal(want) {
		t.Fatalf("control mission moved the floor to %s, want %s", got, want)
	}
	// A voyage that started while the retained ledger was empty still claims
	// every later expense.
	put("00000000-0000-7000-8000-00000000a001", now.Add(-80*time.Hour), map[string]bool{})
	if got, want := floor(), now.Add(-81*time.Hour); !got.Equal(want) {
		t.Fatalf("floor %s, want one hour before the expense mission at %s", got, want)
	}
	// A defaulted intent is terminal, but its cash flows are not recorded yet.
	futuresCreditDefault(t, h, now.Add(-90*time.Hour))
	intents, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(intents) != 1 || !store.FuturesTerminal(intents[0].Status) || futuresAccounted(intents[0]) {
		t.Fatalf("fixture is not a terminal intent with unfinished accounting: %+v %v", intents, err)
	}
	if got, want := floor(), now.Add(-91*time.Hour); !got.Equal(want) {
		t.Fatalf("floor %s, want one hour before the unaccounted futures intent at %s", got, want)
	}
	put("00000000-0000-7000-8000-00000000a002", now.Add(-300*time.Hour), map[string]bool{"00000000-0000-7000-8000-000000000001": true})
	if got, want := floor(), now.Add(-ledgerMaxRetention); !got.Equal(want) {
		t.Fatalf("floor %s, want the retention cap %s", got, want)
	}
}
