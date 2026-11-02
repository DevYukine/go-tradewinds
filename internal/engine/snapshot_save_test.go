package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSnapshotSaveShedsFillsWithoutChangingMemory(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.generation = "snapshot-save"
	now := time.Now().UTC()
	upstream := now.Add(-time.Second)
	runner.meta["fills"] = DatasetMeta{
		UpdatedAt: now, AttemptedAt: now.Add(-time.Millisecond), Generation: runner.generation,
		Complete: true, UpstreamAsOf: &upstream,
	}
	runner.meta["ledger"] = runner.meta["fills"]
	runner.snapshot["fills"] = encode([]map[string]string{{
		"id": "recent-fill", "created_at": now.Format(time.RFC3339Nano),
		"padding": strings.Repeat("x", store.MaxSnapshotBytes),
	}})
	protected := map[string]json.RawMessage{
		"ships":                json.RawMessage(`[]`),
		"vaults":               json.RawMessage(`{"vaults":[]}`),
		"contracts":            json.RawMessage(`[]`),
		"clock":                json.RawMessage(`{}`),
		"catalog":              json.RawMessage(`{}`),
		"ledger":               json.RawMessage(`[]`),
		"leaderboard":          json.RawMessage(`{"entries":[]}`),
		"monopolies":           json.RawMessage(`[]`),
		"routes":               json.RawMessage(`{"lane":{"distance":10}}`),
		"_expense_allocations": json.RawMessage(`{}`),
		"_engine":              json.RawMessage(`{"generation":"snapshot-save"}`),
	}
	for key, raw := range protected {
		runner.snapshot[key] = raw
	}
	runner.snapshot["_meta"] = encode(runner.meta)
	persistedFills := runner.meta["fills"]
	beforeRawMeta := bytes.Clone(runner.snapshot["_meta"])
	newer := runner.meta["fills"]
	newer.Generation = "newer-in-memory"
	runner.meta["fills"] = newer
	beforeSnapshot, beforeMeta := encode(runner.snapshot), encode(runner.meta)
	core, logs := observer.New(zap.ErrorLevel)
	runner.log = zap.New(core)

	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("save oversized refetchable fills: %v", err)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted["fills"]; exists {
		t.Fatal("persisted snapshot retained the oversized fills")
	}
	metadata := decode[map[string]DatasetMeta](persisted, "_meta")
	want := persistedFills
	want.Stale, want.Complete, want.Error = true, false, store.SnapshotShedError
	if !reflect.DeepEqual(metadata["fills"], want) {
		t.Fatalf("shed fills metadata = %+v, want %+v", metadata["fills"], want)
	}
	if !reflect.DeepEqual(metadata["ledger"], runner.meta["ledger"]) {
		t.Fatal("unshed ledger metadata changed")
	}
	for key := range protected {
		if !reflect.DeepEqual(decode[any](persisted, key), decode[any](runner.snapshot, key)) {
			t.Fatalf("size fallback changed protected dataset %s", key)
		}
	}
	if !bytes.Equal(encode(runner.snapshot), beforeSnapshot) || !bytes.Equal(encode(runner.meta), beforeMeta) {
		t.Fatal("persisting the shed copy changed live snapshot or metadata")
	}
	if !bytes.Equal(runner.snapshot["_meta"], beforeRawMeta) {
		t.Fatal("size fallback rewrote the live snapshot's _meta bytes")
	}
	entries := logs.All()
	if len(entries) != 1 || entries[0].Level != zap.ErrorLevel || entries[0].ContextMap()["dropped_datasets"] != "fills" {
		t.Fatalf("size fallback must emit one error naming fills, got %+v", entries)
	}
	if _, exists := entries[0].ContextMap()["dropped_bytes"]; !exists {
		t.Fatal("size fallback error omitted dropped byte sizes")
	}
}

func TestSnapshotSaveUnsheddableEngineStateStillFails(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	before, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner.snapshot["_expense_allocations"] = encode(strings.Repeat("x", store.MaxSnapshotBytes))
	runner.snapshot["fills"] = json.RawMessage(`[]`)
	for key, size := range map[string]int{
		"_large_b": 60_000, "_large_c": 50_000, "_large_d": 40_000,
		"_large_e": 30_000, "_large_f": 20_000,
	} {
		runner.snapshot[key] = encode(strings.Repeat("x", size))
	}
	beforeSnapshot, beforeMeta := encode(runner.snapshot), encode(runner.meta)

	err = runner.saveSnapshot(h.ctx)
	if !errors.Is(err, store.ErrSnapshotRowTooLarge) || !errors.Is(err, store.ErrResponseTooLarge) {
		t.Fatalf("oversized engine state error = %v, want a wrapped snapshot row size error", err)
	}
	var details []string
	for _, key := range []string{"_expense_allocations", "_large_b", "_large_c", "_large_d", "_large_e"} {
		details = append(details, fmt.Sprintf("%s=%d bytes", key, len(key)+len(runner.snapshot[key])))
	}
	if !strings.Contains(err.Error(), strings.Join(details, ", ")) || strings.Contains(err.Error(), "_large_f=") {
		t.Fatalf("retry failure omitted the five largest row keys and sizes: %v", err)
	}
	after, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(before), encode(after)) {
		t.Fatal("failed size fallback replaced the last persisted snapshot")
	}
	if !bytes.Equal(encode(runner.snapshot), beforeSnapshot) || !bytes.Equal(encode(runner.meta), beforeMeta) {
		t.Fatal("failed size fallback changed live snapshot or metadata")
	}
}

func TestSnapshotSaveRestartRefetchesShedFills(t *testing.T) {
	h := newRecoveryHarness(t)
	first := h.runner(primaryTestKey)
	first.generation = "before-shed"
	now := time.Now().UTC()
	first.meta["fills"] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: first.generation, Complete: true}
	first.snapshot["fills"] = encode([]map[string]string{{
		"id": "recent-fill", "created_at": now.Format(time.RFC3339Nano),
		"padding": strings.Repeat("x", store.MaxSnapshotBytes),
	}})
	first.snapshot["_meta"] = encode(first.meta)
	if err := first.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist shed snapshot before restart: %v", err)
	}

	restarted := h.runner(primaryTestKey)
	if _, exists := restarted.snapshot["fills"]; exists || restarted.fresh("fills") {
		t.Fatal("restarted runner treated shed fills as present or fresh")
	}
	if meta := restarted.meta["fills"]; !meta.Stale || meta.Complete || meta.Error != store.SnapshotShedError {
		t.Fatalf("restart lost shed metadata: %+v", meta)
	}
	restarted.generation = "after-restart"
	if err := restarted.refresh(h.ctx); err != nil {
		t.Fatalf("refresh restarted runner: %v", err)
	}
	if len(restarted.snapshot["fills"]) == 0 || !restarted.fresh("fills") {
		t.Fatalf("refresh did not refetch shed fills before planning: %+v", restarted.meta["fills"])
	}
	if fills := decode[[]json.RawMessage](restarted.snapshot, "fills"); fills == nil || len(fills) != 0 {
		t.Fatal("refresh did not replace shed fills with the API's current empty list")
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	meta := decode[map[string]DatasetMeta](persisted, "_meta")["fills"]
	if _, exists := persisted["fills"]; !exists || meta.Stale || !meta.Complete || meta.Error != "" {
		t.Fatalf("fresh fills or cleared shed marker did not survive persistence: %+v", meta)
	}
}

func TestSnapshotSaveShedPriorityMarksChildrenAndStopsWhenSaved(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	captured := DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: "shed-priority", Complete: true}
	for _, key := range []string{"world_events", "ports", "ports/laport", "bookstore", "books", "books/laport", "books/laport/depth", "insurance", "insurance_owned", "fills", "ledger", "leaderboard", "monopolies"} {
		runner.meta[key] = captured
	}
	// The large route payload has its own budget and must not drive row shedding.
	runner.snapshot["routes"] = encode(map[string]string{"large": strings.Repeat("r", store.MaxSnapshotBytes)})
	runner.snapshot["_expense_allocations"] = encode(strings.Repeat("e", 5*store.MaxSnapshotBytes/16))
	runner.snapshot["world_events"] = encode(strings.Repeat("w", store.MaxSnapshotBytes/16))
	runner.snapshot["books"] = encode(map[string]string{"laport": strings.Repeat("b", 9*store.MaxSnapshotBytes/16)})
	runner.snapshot["insurance"] = encode(strings.Repeat("i", store.MaxSnapshotBytes/16))
	runner.snapshot["insurance_owned"] = encode(strings.Repeat("o", store.MaxSnapshotBytes/16))
	runner.snapshot["fills"] = encode(strings.Repeat("f", store.MaxSnapshotBytes/16))
	runner.snapshot["ports"] = encode(map[string]string{"laport": strings.Repeat("p", store.MaxSnapshotBytes/16)})
	runner.snapshot["ledger"] = encode(strings.Repeat("l", store.MaxSnapshotBytes/16))
	runner.snapshot["leaderboard"] = json.RawMessage(`{"entries":[]}`)
	runner.snapshot["monopolies"] = json.RawMessage(`[]`)
	runner.snapshot["_meta"] = encode(runner.meta)

	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist priority-shed copy: %v", err)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"world_events", "books"} {
		if _, exists := persisted[key]; exists {
			t.Fatalf("fixed-priority fallback retained %s", key)
		}
	}
	for _, key := range []string{"insurance", "insurance_owned", "fills", "ports", "ledger", "leaderboard", "monopolies", "routes", "_expense_allocations"} {
		if _, exists := persisted[key]; !exists {
			t.Fatalf("fallback shed %s after a successful save or crossed its allowlist", key)
		}
	}
	metadata := decode[map[string]DatasetMeta](persisted, "_meta")
	for _, key := range []string{"world_events", "books", "books/laport", "books/laport/depth"} {
		want := captured
		want.Stale, want.Complete, want.Error = true, false, store.SnapshotShedError
		if !reflect.DeepEqual(metadata[key], want) {
			t.Fatalf("shed child/group metadata %s = %+v, want %+v", key, metadata[key], want)
		}
	}
	for _, key := range []string{"bookstore", "ports", "ports/laport", "insurance", "insurance_owned", "fills", "ledger", "leaderboard", "monopolies"} {
		if !reflect.DeepEqual(metadata[key], captured) {
			t.Fatalf("fallback changed unshed metadata %s: %+v", key, metadata[key])
		}
	}
}

func TestSnapshotSaveShedsPortsLastAndKeepsAccountingAndStandings(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	captured := DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: "ports-last", Complete: true}
	for _, key := range []string{"world_events", "books", "insurance", "insurance_owned", "fills"} {
		runner.snapshot[key] = json.RawMessage(`[]`)
		runner.meta[key] = captured
	}
	runner.snapshot["ports"] = encode(map[string]string{"laport": strings.Repeat("p", store.MaxSnapshotBytes)})
	runner.meta["ports"], runner.meta["ports/laport"] = captured, captured
	for _, key := range []string{"ledger", "leaderboard", "monopolies"} {
		runner.snapshot[key] = encode(strings.Repeat("k", store.MaxSnapshotBytes/16))
		runner.meta[key] = captured
	}
	runner.snapshot["_meta"] = encode(runner.meta)
	core, logs := observer.New(zap.ErrorLevel)
	runner.log = zap.New(core)

	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist snapshot after shedding ports last: %v", err)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := decode[map[string]DatasetMeta](persisted, "_meta")
	for _, key := range []string{"world_events", "books", "insurance", "insurance_owned", "fills", "ports"} {
		if _, exists := persisted[key]; exists {
			t.Fatalf("exhausted fallback retained %s", key)
		}
	}
	for _, key := range []string{"ports", "ports/laport"} {
		meta := metadata[key]
		if !meta.Stale || meta.Complete || meta.Error != store.SnapshotShedError || !meta.UpdatedAt.Equal(now) {
			t.Fatalf("ports child/group lost shed metadata: %s = %+v", key, meta)
		}
	}
	for _, key := range []string{"ledger", "leaderboard", "monopolies"} {
		if _, exists := persisted[key]; !exists || !reflect.DeepEqual(metadata[key], captured) {
			t.Fatalf("fallback lost protected dataset %s or changed its metadata", key)
		}
	}
	entries := logs.All()
	if len(entries) != 1 || entries[0].ContextMap()["dropped_datasets"] != "world_events, books, insurance, insurance_owned, fills, ports" {
		t.Fatalf("fallback did not preserve its six-key priority: %+v", entries)
	}
}

func TestSnapshotSaveDoesNotShedForRouteErrors(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.snapshot["world_events"] = json.RawMessage(`[]`)
	runner.snapshot["routes"] = json.RawMessage(`{"lane":{"distance":1e130000}}`)
	beforeSnapshot, beforeMeta := encode(runner.snapshot), encode(runner.meta)
	core, logs := observer.New(zap.ErrorLevel)
	runner.log = zap.New(core)

	err := runner.saveSnapshot(h.ctx)
	if !errors.Is(err, store.ErrResponseTooLarge) || errors.Is(err, store.ErrSnapshotRowTooLarge) {
		t.Fatalf("route overflow must remain distinct from row overflow: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatal("route overflow triggered the row shedding fallback")
	}
	if !bytes.Equal(encode(runner.snapshot), beforeSnapshot) || !bytes.Equal(encode(runner.meta), beforeMeta) {
		t.Fatal("route overflow changed live row datasets or metadata")
	}
}

func TestSnapshotSaveDoesNotShedForOtherErrors(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.snapshot["world_events"] = json.RawMessage(`[]`)
	runner.snapshot["_engine"] = json.RawMessage(`{`)
	core, logs := observer.New(zap.ErrorLevel)
	runner.log = zap.New(core)

	if err := runner.saveSnapshot(h.ctx); err == nil || errors.Is(err, store.ErrResponseTooLarge) {
		t.Fatalf("invalid JSON error = %v, want the original non-size save failure", err)
	}
	if _, exists := runner.snapshot["world_events"]; !exists || logs.Len() != 0 {
		t.Fatal("non-size failure triggered shedding or its error log")
	}
}

func TestSnapshotSaveShedsEscapingExpandedFills(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	now := time.Now().UTC()
	runner.meta["fills"] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Complete: true}
	padding := strings.Repeat("<", store.MaxSnapshotBytes/5)
	runner.snapshot["fills"] = json.RawMessage(`[{"created_at":"` + now.Format(time.RFC3339Nano) + `","padding":"` + padding + `"}]`)
	runner.snapshot["_meta"] = encode(runner.meta)
	var rawBytes int
	for key, raw := range runner.snapshot {
		if key != "routes" {
			rawBytes += len(key) + len(raw)
		}
	}
	if rawBytes >= store.MaxSnapshotBytes {
		t.Fatalf("escaping fixture's raw bytes = %d, want below the row cap", rawBytes)
	}
	beforeFills := bytes.Clone(runner.snapshot["fills"])
	beforeMeta := bytes.Clone(runner.snapshot["_meta"])

	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("HTML escaping alone must not defeat the shed fallback: %v", err)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted["fills"]; exists {
		t.Fatal("escaping-expanded fills were not shed")
	}
	meta := decode[map[string]DatasetMeta](persisted, "_meta")["fills"]
	if !meta.Stale || meta.Complete || meta.Error != store.SnapshotShedError {
		t.Fatalf("escaping-expanded fills lost their shed marker: %+v", meta)
	}
	if !bytes.Equal(runner.snapshot["fills"], beforeFills) || !bytes.Equal(runner.snapshot["_meta"], beforeMeta) {
		t.Fatal("escaping-expanded fallback changed live snapshot bytes")
	}
}

func TestSnapshotSaveStopsAfterFirstSuccessfulRetry(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.snapshot["_expense_allocations"] = encode(strings.Repeat("e", 13*store.MaxSnapshotBytes/16))
	runner.snapshot["world_events"] = encode(strings.Repeat("w", store.MaxSnapshotBytes/4))
	runner.snapshot["books"] = encode(map[string]string{"laport": strings.Repeat("b", store.MaxSnapshotBytes/16)})
	now := time.Now().UTC()
	runner.meta["books/laport"] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Complete: true}
	runner.snapshot["_meta"] = encode(runner.meta)

	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist the first acceptable shed copy: %v", err)
	}
	persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := persisted["world_events"]; exists {
		t.Fatal("priority fallback did not shed world events first")
	}
	if _, exists := persisted["books"]; !exists {
		t.Fatal("fallback kept shedding after the row already fit its encoded cap")
	}
	meta := decode[map[string]DatasetMeta](persisted, "_meta")["books/laport"]
	if meta.Stale || !meta.Complete || meta.Error != "" {
		t.Fatalf("successful retry marked unshed books stale: %+v", meta)
	}
}

func TestSnapshotSaveReturnsPlainRouteErrorAfterShedding(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.snapshot["books"] = encode(map[string]string{"laport": strings.Repeat("b", store.MaxSnapshotBytes)})
	runner.snapshot["routes"] = json.RawMessage(`{"lane":{"distance":1e130000}}`)
	beforeSnapshot := encode(runner.snapshot)

	err := runner.saveSnapshot(h.ctx)
	if !errors.Is(err, store.ErrResponseTooLarge) || errors.Is(err, store.ErrSnapshotRowTooLarge) {
		t.Fatalf("post-shed route error lost its original classification: %v", err)
	}
	if strings.Contains(err.Error(), "largest row keys") {
		t.Fatalf("a route failure was misdiagnosed as a remaining row overflow: %v", err)
	}
	if !bytes.Equal(encode(runner.snapshot), beforeSnapshot) {
		t.Fatal("post-shed route failure changed the live snapshot")
	}
}

func TestSnapshotSaveShedLogTracksSetChangesAndFullSaveReset(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	oversized := encode(strings.Repeat("x", store.MaxSnapshotBytes))
	runner.snapshot["world_events"] = oversized
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	for range 2 {
		if err := runner.saveSnapshot(h.ctx); err != nil {
			t.Fatalf("persist repeated world-event shedding: %v", err)
		}
	}
	runner.snapshot["world_events"] = json.RawMessage(`[]`)
	runner.snapshot["books"] = oversized
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist a changed shed set: %v", err)
	}
	runner.snapshot["books"] = json.RawMessage(`{}`)
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist a full snapshot to reset shed logging: %v", err)
	}
	if logs.Len() != 3 {
		t.Fatalf("a full save emitted a shed log: %d entries", logs.Len())
	}
	runner.snapshot["books"] = oversized
	if err := runner.saveSnapshot(h.ctx); err != nil {
		t.Fatalf("persist shedding after a full-save reset: %v", err)
	}

	entries := logs.All()
	levels := []zapcore.Level{zap.ErrorLevel, zap.DebugLevel, zap.ErrorLevel, zap.ErrorLevel}
	datasets := []string{"world_events", "world_events", "world_events, books", "world_events, books"}
	if len(entries) != len(levels) {
		t.Fatalf("shed log entries = %d, want %d", len(entries), len(levels))
	}
	for i, entry := range entries {
		if entry.Level != levels[i] || entry.ContextMap()["dropped_datasets"] != datasets[i] {
			t.Fatalf("shed log %d = %+v, want %s for %s", i, entry, levels[i], datasets[i])
		}
	}
}
