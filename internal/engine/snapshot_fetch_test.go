package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func snapshotContractPage(status string) []Contract {
	rows := make([]Contract, 200)
	for index := range rows {
		rows[index] = Contract{ID: fmt.Sprintf("contract-%03d", index), Status: status}
	}
	return rows
}

func TestContractCaptureKeepsOrderedPagesStatusesAndNewestDuplicate(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if request.URL.Path != "/v1/contract/list" || query.Get("limit") != "200" {
			t.Errorf("unexpected contract request: %s", request.URL)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		status, cursor := query.Get("status"), query.Get("cursor")
		mu.Lock()
		queries = append(queries, status+":"+cursor)
		mu.Unlock()
		rows := []Contract{}
		switch {
		case status == "open" && cursor == "":
			rows = snapshotContractPage("open")
		case status == "open" && cursor == "contract-199":
			rows = []Contract{{ID: "open-tail", Status: "open"}}
		case status == "active" && cursor == "":
			rows = []Contract{{ID: "contract-000", Status: "active"}, {ID: "active-only", Status: "active"}}
		}
		_ = json.NewEncoder(writer).Encode(rows)
	}))
	if err := runner.captureWave(context.Background(), []fetchTask{{key: "contracts", kind: fetchContracts}}, nil); err != nil {
		t.Fatalf("capture contracts: %v", err)
	}
	mu.Lock()
	gotQueries := append([]string(nil), queries...)
	mu.Unlock()
	wantQueries := []string{"open:", "open:contract-199", "active:", "completed:", "cancelled:", "defaulted:"}
	if !reflect.DeepEqual(gotQueries, wantQueries) {
		t.Fatalf("contract status/page order = %v, want %v", gotQueries, wantQueries)
	}
	contracts := decode[[]Contract](runner.snapshot, "contracts")
	if len(contracts) != 202 || contracts[0].ID != "contract-000" || contracts[0].Status != "active" ||
		contracts[200].ID != "open-tail" || contracts[201].ID != "active-only" {
		t.Fatalf("contract movement changed aggregate order or duplicated sealed cargo: %+v", contracts)
	}
	if !runner.fresh("contracts") {
		t.Fatalf("complete contract task is not fresh: %+v", runner.meta["contracts"])
	}
}

func TestFailedLaterContractPageRetainsWholeLastGoodAggregate(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/zones" {
			_, _ = writer.Write([]byte(`[{"id":"new-zone"}]`))
			return
		}
		status, cursor := request.URL.Query().Get("status"), request.URL.Query().Get("cursor")
		mu.Lock()
		queries = append(queries, status+":"+cursor)
		mu.Unlock()
		switch {
		case status == "open" && cursor == "":
			_ = json.NewEncoder(writer).Encode([]Contract{{ID: "new-open", Status: "open"}})
		case status == "active" && cursor == "":
			_ = json.NewEncoder(writer).Encode(snapshotContractPage("active"))
		default:
			writer.WriteHeader(http.StatusNotFound)
			_, _ = writer.Write([]byte(`{"code":"page_unavailable"}`))
		}
	}))
	lastGood := json.RawMessage(`[{"id":"last-good","status":"active"}]`)
	observed := time.Now().Add(-time.Minute)
	prior := DatasetMeta{UpdatedAt: observed, AttemptedAt: observed, Generation: "gen-before", Complete: true}
	runner.snapshot["contracts"] = lastGood
	runner.meta["contracts"] = prior
	if err := runner.captureWave(context.Background(), []fetchTask{
		{key: "contracts", kind: fetchContracts},
		{key: "zones", path: "/v1/world/zones"},
	}, nil); err != nil {
		t.Fatalf("isolated contract page failure aborted successful siblings: %v", err)
	}
	mu.Lock()
	gotQueries := append([]string(nil), queries...)
	mu.Unlock()
	if want := []string{"open:", "active:", "active:contract-199"}; !reflect.DeepEqual(gotQueries, want) {
		t.Fatalf("failed contract task retried or continued partial status reads: got %v want %v", gotQueries, want)
	}
	failed := runner.meta["contracts"]
	if string(runner.snapshot["contracts"]) != string(lastGood) || !failed.UpdatedAt.Equal(prior.UpdatedAt) || failed.Generation != prior.Generation ||
		!failed.AttemptedAt.After(prior.UpdatedAt) || failed.Complete || !failed.Stale || failed.Error == "" {
		t.Fatalf("failed later page published partial contracts or erased last-good authority: data %s meta %+v", runner.snapshot["contracts"], failed)
	}
	if !runner.fresh("zones") || string(runner.snapshot["zones"]) != `[{"id":"new-zone"}]` {
		t.Fatal("contract page failure hid the independent successful dataset")
	}
}

func TestListCaptureRejectsNullAndRepeatedIDsWithoutPublishingPartialRows(t *testing.T) {
	for _, scenario := range []string{"null", "repeated id"} {
		t.Run(scenario, func(t *testing.T) {
			runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if scenario == "null" {
					_, _ = writer.Write([]byte("null"))
					return
				}
				if request.URL.Query().Get("cursor") == "" {
					_ = json.NewEncoder(writer).Encode(snapshotContractPage("open"))
					return
				}
				_ = json.NewEncoder(writer).Encode([]Contract{{ID: "contract-000", Status: "open"}})
			}))
			runner.snapshot["orders"] = json.RawMessage(`[{"id":"last-good"}]`)
			if err := runner.captureWave(context.Background(), []fetchTask{{key: "orders", path: "/v1/market/orders", kind: fetchPages}}, nil); err != nil {
				t.Fatalf("isolated invalid page aborted refresh: %v", err)
			}
			if string(runner.snapshot["orders"]) != `[{"id":"last-good"}]` || runner.meta["orders"].Error == "" || runner.meta["orders"].Complete {
				t.Fatalf("invalid list replaced complete last-good rows: data %s meta %+v", runner.snapshot["orders"], runner.meta["orders"])
			}
		})
	}
}

func TestCaptureWaveReducesOnlyAfterJoinInDescriptorOrder(t *testing.T) {
	entered := make(chan string, 3)
	completed := make(chan string, 3)
	gates := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		port := request.URL.Query().Get("port_id")
		entered <- port
		select {
		case <-gates[port]:
			_ = json.NewEncoder(writer).Encode(map[string]string{"port_id": port})
		case <-request.Context().Done():
		}
	}))
	runner.api.SetObserver(func(_, path string, _ int, _ time.Duration, _, _ []byte, _ string) {
		completed <- path
	})
	values := map[string]json.RawMessage{"old": json.RawMessage(`{"port_id":"old"}`)}
	tasks := []fetchTask{
		{key: "books/a", path: "/v1/market/book?port_id=a"},
		{key: "books/b", path: "/v1/market/book?port_id=b"},
		{key: "books/c", path: "/v1/market/book?port_id=c"},
	}
	done := make(chan error, 1)
	go func() { done <- runner.captureWave(ctx, tasks, map[string]map[string]json.RawMessage{"books": values}) }()
	for range tasks {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("independent child fetches did not overlap")
		}
	}
	for _, port := range []string{"c", "b"} {
		close(gates[port])
		select {
		case path := <-completed:
			if !strings.HasSuffix(path, "port_id="+port) {
				t.Fatalf("unexpected completion order: %s before %s", path, port)
			}
		case <-ctx.Done():
			t.Fatal("released child did not finish")
		}
		if len(values) != 1 || values["old"] == nil || len(runner.meta) != 0 {
			t.Fatal("workers published child state before all siblings joined")
		}
	}
	close(gates["a"])
	if err := <-done; err != nil {
		t.Fatalf("join reversed child completions: %v", err)
	}
	runner.finishGroup("books", values, []string{"a", "b", "c"})
	if len(values) != 3 || !runner.fresh("books") {
		t.Fatalf("owner did not reduce complete child group: values %v meta %+v", values, runner.meta["books"])
	}
	for _, port := range []string{"a", "b", "c"} {
		if string(values[port]) != `{"port_id":"`+port+`"}` || runner.snapshot["books/"+port] != nil || !runner.fresh("books/"+port) {
			t.Fatalf("completion order misattributed %s: data %s meta %+v", port, values[port], runner.meta["books/"+port])
		}
	}
}

func TestDatasetFailureLogsSafeBoundedCauseAndSingleRecovery(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	cause := errors.New(strings.Repeat("界", 500) + primaryTestKey + " tw_live_dataset_echo " + strings.Repeat("x", 100))
	now := time.Now()
	failed := fetchResult{err: cause, attemptedAt: now, completedAt: now}
	for range 2 {
		if runner.applyCapture(h.ctx, "zones", failed) {
			t.Fatal("failed result was accepted")
		}
	}
	failures := logs.FilterMessage("dataset refresh failed").All()
	if len(failures) != 2 || failures[0].Level != zap.WarnLevel || failures[1].Level != zap.DebugLevel {
		t.Fatalf("unchanged failure repeated operational warnings: %+v", failures)
	}
	text, _ := failures[0].ContextMap()["cause"].(string)
	if !utf8.ValidString(text) || !strings.Contains(text, "[REDACTED]") || !strings.HasSuffix(text, "...") || strings.Contains(text, primaryTestKey) || strings.Contains(text, "tw_live_dataset_echo") {
		t.Fatalf("failure cause was not scrubbed before Unicode-safe bounding: %q", text)
	}
	good := fetchResult{data: json.RawMessage(`[]`), attemptedAt: now, completedAt: now}
	for range 2 {
		if !runner.applyCapture(h.ctx, "zones", good) {
			t.Fatal("recovered result was rejected")
		}
	}
	recoveries := logs.FilterMessage("dataset refresh recovered").All()
	if len(recoveries) != 1 || recoveries[0].Level != zap.InfoLevel || recoveries[0].ContextMap()["dataset"] != "zones" {
		t.Fatalf("dataset recovery was missing or repeated: %+v", recoveries)
	}
}

type datasetCredentialBarrierCore struct {
	zapcore.Core
	beforeFailure func()
}

func (core datasetCredentialBarrierCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if entry.Message == "dataset refresh failed" {
		core.beforeFailure()
	}
	return core.Core.Check(entry, checked)
}

func TestDatasetFailureScrubsAfterEnabledLogCheckAndFailsClosed(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.WarnLevel)
	checked := false
	runner.log = zap.New(datasetCredentialBarrierCore{Core: core, beforeFailure: func() {
		checked = true
		if err := h.db.Close(); err != nil {
			t.Fatal(err)
		}
	}})
	now := time.Now()
	runner.applyCapture(h.ctx, "books/"+primaryTestKey, fetchResult{
		err: errors.New("upstream echoed " + primaryTestKey), attemptedAt: now, completedAt: now,
	})
	failures := logs.FilterMessage("dataset refresh failed").All()
	if !checked || len(failures) != 1 || failures[0].ContextMap()["cause"] != "[REDACTED]" || failures[0].ContextMap()["dataset"] != "[REDACTED]" {
		t.Fatalf("credential lookup before Check or fail-open dataset fields: checked %t logs %+v", checked, failures)
	}
}
