package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

// store.SaveSnapshot persists at most maxSnapshotBytes (8 MiB) of encoded
// snapshot per account and rejects anything larger with ErrResponseTooLarge.
// Retained history must shrink back under that budget instead of poisoning
// every future save, so the tests below compare against the same bound.
const storeSnapshotBudgetBytes = 8 * 1024 * 1024

func newRetentionRunner(t *testing.T, handler http.Handler) *Runner {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := gameapi.New(server.URL, "test-api-key", zap.NewNop())
	account := store.Account{ID: "acc-retention", CompanyID: "company-retention"}
	runner := New(client, nil, account, zap.NewNop(), Options{})
	runner.generation = "gen-retention"
	return runner
}

func TestFinishGroupPrunesHistoricalKeysAndMetadataOrphans(t *testing.T) {
	t.Parallel()
	runner := New(nil, nil, store.Account{ID: "acc-retention"}, zap.NewNop(), Options{})
	runner.generation = "gen-a"
	now := time.Now().UTC()
	runner.snapshot["books"] = json.RawMessage(`{"le_havre":{"visibility":"quay"},"portsmouth":{"visibility":"quay"},"london":{"visibility":"quay"}}`)
	for _, port := range []string{"le_havre", "portsmouth", "london"} {
		runner.meta["books/"+port] = DatasetMeta{
			UpdatedAt:   now,
			AttemptedAt: now,
			Generation:  "gen-a",
			Complete:    true,
		}
	}
	// A failed capture leaves per-entry metadata with no grouped value at all;
	// the orphan must not survive the fleet leaving either.
	runner.meta["books/calais"] = DatasetMeta{
		AttemptedAt: now,
		Generation:  "gen-a",
		Error:       "upstream response is not the documented non-null JSON dataset",
		Stale:       true,
	}

	values := decode[map[string]json.RawMessage](runner.snapshot, "books")
	runner.finishGroup("books", values, []string{"le_havre"})

	retained := decode[map[string]json.RawMessage](runner.snapshot, "books")
	if len(retained) != 1 || retained["le_havre"] == nil {
		t.Fatalf("persisted books after presence change = %v, want only le_havre", retained)
	}
	for _, orphan := range []string{"books/portsmouth", "books/london", "books/calais"} {
		if _, exists := runner.meta[orphan]; exists {
			t.Fatalf("inactive child metadata %s survived the generation", orphan)
		}
	}
	if !runner.fresh("books") {
		t.Fatal("a pruned group whose only current child is fresh must stay fresh")
	}
}

func TestFailedCurrentCaptureKeepsLastGoodDataAndVisibleFailure(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path != "/v1/market/book" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"unexpected_request","detail":"only the book endpoint is expected"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"code":"market_closed","detail":"le_havre market refused the request"}`)
	})
	runner := newRetentionRunner(t, handler)
	now := time.Now().UTC()
	runner.snapshot["books"] = json.RawMessage(`{"le_havre":{"visibility":"quay"}}`)
	runner.meta["books/le_havre"] = DatasetMeta{
		UpdatedAt:   now,
		AttemptedAt: now,
		Generation:  runner.generation,
		Complete:    true,
	}

	values := decode[map[string]json.RawMessage](runner.snapshot, "books")
	if err := runner.captureWave(context.Background(), []fetchTask{{
		key: "books/le_havre", path: "/v1/market/book?port_id=le_havre",
	}}, map[string]map[string]json.RawMessage{"books": values}); err != nil {
		t.Fatalf("capture failed book: %v", err)
	}
	runner.finishGroup("books", values, []string{"le_havre"})

	retained := decode[map[string]json.RawMessage](runner.snapshot, "books")
	if string(retained["le_havre"]) != `{"visibility":"quay"}` {
		t.Fatalf("failed current capture damaged last-good data: %s", retained["le_havre"])
	}
	child := runner.meta["books/le_havre"]
	if child.Error == "" || !child.Stale || child.Complete {
		t.Fatalf("failed child lost its visible failure metadata: %+v", child)
	}
	group := runner.meta["books"]
	if !group.Stale || group.Complete || group.Error == "" {
		t.Fatalf("group metadata hid the failed current child: %+v", group)
	}
	if count := requests.Load(); count != 1 {
		t.Fatalf("capture attempted %d requests, want exactly one", count)
	}
}

func TestTravelCyclesReplaceBooksInsteadOfAccumulatingHistory(t *testing.T) {
	t.Parallel()
	books := map[string]string{
		"le_havre":   `{"visibility":"quay","commodities":["tea"]}`,
		"portsmouth": `{"visibility":"quay","commodities":["rum"]}`,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, known := books[request.URL.Query().Get("port_id")]
		if !known {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"port_not_found","detail":"no book for that port"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	runner := newRetentionRunner(t, handler)

	// Cycle A: the fleet occupies le_havre and captures its book.
	values := make(map[string]json.RawMessage)
	if err := runner.captureWave(context.Background(), []fetchTask{{
		key: "books/le_havre", path: "/v1/market/book?port_id=le_havre",
	}}, map[string]map[string]json.RawMessage{"books": values}); err != nil {
		t.Fatalf("capture origin book: %v", err)
	}
	runner.finishGroup("books", values, []string{"le_havre"})

	// Cycle B: the fleet sailed on to portsmouth; le_havre must disappear
	// from both the persisted values and the per-port metadata.
	values = decode[map[string]json.RawMessage](runner.snapshot, "books")
	if err := runner.captureWave(context.Background(), []fetchTask{{
		key: "books/portsmouth", path: "/v1/market/book?port_id=portsmouth",
	}}, map[string]map[string]json.RawMessage{"books": values}); err != nil {
		t.Fatalf("capture destination book: %v", err)
	}
	runner.finishGroup("books", values, []string{"portsmouth"})

	retained := decode[map[string]json.RawMessage](runner.snapshot, "books")
	if len(retained) != 1 || retained["portsmouth"] == nil {
		t.Fatalf("repeated travel accumulated historical books: %v", retained)
	}
	if _, exists := runner.meta["books/le_havre"]; exists {
		t.Fatal("abandoned origin kept per-port metadata after the fleet left")
	}
	if !runner.fresh("books/portsmouth") {
		t.Fatal("the current port book must be fresh right after capture")
	}
	if !runner.fresh("books") {
		t.Fatal("the group must stay fresh when its only current child is fresh")
	}
}

func TestHistoricalBloatPrunesBelowStoreSnapshotBudget(t *testing.T) {
	t.Parallel()
	runner := New(nil, nil, store.Account{ID: "acc-retention"}, zap.NewNop(), Options{})
	runner.generation = "gen-a"
	// Ten abandoned origins each carrying a stale megabyte-scale book push the
	// persisted snapshot past what the store accepts for an account.
	blob := strings.Repeat("x", 1024*1024)
	historical := make(map[string]json.RawMessage)
	historical["le_havre"] = json.RawMessage(`{"visibility":"quay"}`)
	for _, origin := range []string{"london", "bristol", "dover", "hull", "york", "kent", "wales", "essex", "kolkata", "bombay"} {
		historical[origin] = json.RawMessage(`{"gossip":"` + blob + `"}`)
	}
	runner.snapshot["books"] = encode(historical)

	poisoned := len(encode(runner.snapshot))
	if poisoned <= storeSnapshotBudgetBytes {
		t.Fatalf("fixture snapshot is only %d bytes; it must exceed the %d byte store budget to prove the poisoning",
			poisoned, storeSnapshotBudgetBytes)
	}

	values := decode[map[string]json.RawMessage](runner.snapshot, "books")
	runner.finishGroup("books", values, []string{"le_havre"})

	retained := decode[map[string]json.RawMessage](runner.snapshot, "books")
	if len(retained) != 1 || retained["le_havre"] == nil {
		t.Fatalf("pruned books = %v, want only the currently requested port", retained)
	}
	kept := len(encode(runner.snapshot))
	if kept >= storeSnapshotBudgetBytes {
		t.Fatalf("pruned snapshot is %d bytes, want below the store's %d byte budget", kept, storeSnapshotBudgetBytes)
	}
}

func TestFinishGroupRequiresCompleteCurrentGenerationChildren(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		generation string
		complete   bool
		keepValue  bool
	}{
		{name: "previous generation", generation: "gen-before", complete: true, keepValue: true},
		{name: "incomplete current child", generation: "gen-current", keepValue: true},
		{name: "missing current value", generation: "gen-current", complete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := New(nil, nil, store.Account{ID: "acc-retention"}, zap.NewNop(), Options{})
			runner.generation = "gen-current"
			now := time.Now().UTC()
			child := DatasetMeta{
				UpdatedAt: now, AttemptedAt: now,
				Generation: test.generation, Complete: test.complete,
			}
			runner.meta["books/source"] = child
			values := make(map[string]json.RawMessage)
			if test.keepValue {
				values["source"] = json.RawMessage(`{"visibility":"quay"}`)
			}
			runner.finishGroup("books", values, []string{"source"})

			group := runner.meta["books"]
			if group.Complete || !group.Stale || group.Error == "" || runner.fresh("books") {
				t.Fatalf("group advertised an unusable child as complete: %+v", group)
			}
			if retained := runner.meta["books/source"]; retained != child {
				t.Fatalf("group reduction changed retained child metadata: got %+v want %+v", retained, child)
			}
			retained := decode[map[string]json.RawMessage](runner.snapshot, "books")
			if test.keepValue && string(retained["source"]) != `{"visibility":"quay"}` {
				t.Fatalf("incomplete group discarded last-good requested book: %s", runner.snapshot["books"])
			}
		})
	}
}
