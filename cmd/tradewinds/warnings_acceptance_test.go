package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type warningsAcceptanceStep struct {
	at       time.Time
	accounts []store.AccountWarnings
	err      error
}

// A final injected cancellation is the barrier: every preceding evaluation has
// returned before the worker asks the source again. No sleeps or wall-clock
// ticker intervals participate in transition assertions.
func warningsAcceptanceRun(t *testing.T, steps []warningsAcceptanceStep) []observer.LoggedEntry {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	core, observed := observer.New(zap.DebugLevel)
	ticks := make(chan time.Time, len(steps))
	for _, step := range steps[1:] {
		ticks <- step.at
	}
	ticks <- steps[len(steps)-1].at.Add(time.Minute)
	index := 0
	now := func() time.Time {
		if index >= len(steps) {
			return steps[len(steps)-1].at.Add(time.Minute)
		}
		return steps[index].at
	}
	source := func(_ context.Context, at time.Time) ([]store.AccountWarnings, error) {
		if index == len(steps) {
			cancel()
			return nil, context.Canceled
		}
		step := steps[index]
		index++
		if !at.Equal(step.at) {
			t.Errorf("source time=%s, want injected time=%s", at, step.at)
		}
		return step.accounts, step.err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchWarnings(ctx, zap.New(core), source, now, ticks)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("warning worker did not exit after injected cancellation")
	}
	if index != len(steps) {
		t.Fatalf("evaluated %d steps, want %d", index, len(steps))
	}
	return observed.All()
}

func warningsAcceptanceActive(at time.Time, minutes int64) []store.AccountWarnings {
	return []store.AccountWarnings{{AccountID: "one", Label: "Yukine", Available: true, Warnings: []store.Warning{{
		Code: "controller_stalled", Severity: "critical", Since: at.Add(-14 * time.Minute),
		Facts: map[string]int64{"minutes": minutes}, Names: map[string]string{"ship": "Lapras"},
	}}}}
}

func TestWatchWarningsAcceptanceRaisePersistChangedFactsClear(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	active := warningsAcceptanceActive(at, 14)
	changed := warningsAcceptanceActive(at, 43)
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{
		{at: at, accounts: active},
		{at: at.Add(29*time.Minute + 59*time.Second), accounts: changed},
		{at: at.Add(30 * time.Minute), accounts: changed},
		{at: at.Add(59*time.Minute + 59*time.Second), accounts: changed},
		{at: at.Add(60 * time.Minute), accounts: changed},
		{at: at.Add(61 * time.Minute), accounts: warningsAcceptanceClear()},
		{at: at.Add(62 * time.Minute), accounts: warningsAcceptanceClear()},
	})
	want := []string{"engine warning", "engine warning persists", "engine warning persists", "engine warning cleared"}
	if len(entries) != len(want) {
		t.Fatalf("duplicate, lost or cancellation logs: %+v", entries)
	}
	for i, entry := range entries {
		if entry.Message != want[i] {
			t.Fatalf("entry %d=%q, want %q", i, entry.Message, want[i])
		}
		fields := entry.ContextMap()
		if fields["account"] != "Yukine" || fields["account_id"] != "one" || fields["code"] != "controller_stalled" || fields["severity"] != "critical" {
			t.Fatalf("missing account/code/severity fields: %+v", fields)
		}
		if strings.Contains(entry.Message, "Cr") {
			t.Fatalf("formatted money embedded in message: %q", entry.Message)
		}
		if i < 3 {
			if entry.Level != zapcore.WarnLevel || fields["since"] == nil || fields["ship"] != "Lapras" {
				t.Fatalf("raise/persistence fields: %+v", entry)
			}
			wantMinutes := int64(43)
			if i == 0 {
				wantMinutes = 14
			}
			if fields["minutes"] != wantMinutes {
				t.Fatalf("integer minutes lost: %+v", fields)
			}
		} else {
			if entry.Level != zapcore.InfoLevel || fields["active_for"] == nil {
				t.Fatalf("clear must carry active_for at INFO: %+v", entry)
			}
			value, ok := fields["active_for"].(time.Duration)
			if !ok || value != 61*time.Minute {
				t.Fatalf("active_for=%v (%T), want 61m since raise", fields["active_for"], fields["active_for"])
			}
		}
	}
}

func TestWatchWarningsAcceptanceUnavailableOncePerStreak(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	active := warningsAcceptanceActive(at, 14)
	failed := errors.New("private database details must not reach logs")
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{
		{at: at, accounts: active},
		{at: at.Add(time.Minute), err: failed},
		{at: at.Add(2 * time.Minute), err: failed},
		{at: at.Add(3 * time.Minute), accounts: active},
		{at: at.Add(4 * time.Minute), err: failed},
		{at: at.Add(5 * time.Minute), err: failed},
		{at: at.Add(6 * time.Minute), accounts: warningsAcceptanceClear()},
	})
	want := []string{"engine warning", "engine warnings unavailable", "engine warnings unavailable", "engine warning cleared"}
	if len(entries) != len(want) {
		t.Fatalf("failure streak re-raised, cleared or duplicated logs: %+v", entries)
	}
	for i, entry := range entries {
		if entry.Message != want[i] {
			t.Fatalf("entry %d=%q, want %q", i, entry.Message, want[i])
		}
		if strings.Contains(entry.Message, "unavailable") && (entry.Level != zapcore.WarnLevel || entry.ContextMap()["error"] != "database_unavailable") {
			t.Fatalf("unavailable must be WARN with safe constant error code: %+v", entry)
		}
	}
}

func TestWatchWarningsAcceptanceAccountIsolationAndRestart(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	accounts := warningsAcceptanceActive(at, 14)
	second := accounts[0]
	second.AccountID, second.Label = "two", "Second"
	accounts = append(accounts, second)
	for restart := range 2 {
		cleared := append([]store.AccountWarnings{accounts[0]}, store.AccountWarnings{AccountID: "two", Label: "Second", Available: true, Warnings: []store.Warning{}})
		entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{{at: at, accounts: accounts}, {at: at.Add(time.Minute), accounts: cleared}})
		if len(entries) != 3 || entries[0].Message != "engine warning" || entries[1].Message != "engine warning" || entries[2].Message != "engine warning cleared" || entries[2].ContextMap()["account_id"] != "two" {
			t.Fatalf("restart %d lost initial raises or mixed accounts: %+v", restart, entries)
		}
	}
}

func TestWatchWarningsAcceptanceCancellationIsSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	core, observed := observer.New(zap.DebugLevel)
	started := make(chan struct{})
	source := func(ctx context.Context, _ time.Time) ([]store.AccountWarnings, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchWarnings(ctx, zap.New(core), source, time.Now, make(chan time.Time))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("worker did not evaluate immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker ignored context cancellation")
	}
	if entries := observed.All(); len(entries) != 0 {
		t.Fatalf("ordinary cancellation logged an error: %+v", entries)
	}
}

func TestWatchWarningsAcceptanceStructuredPenceFields(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	accounts := []store.AccountWarnings{{AccountID: "one", Label: "Yukine", Available: true, Warnings: []store.Warning{{
		Code: "loss_streak", Severity: "warning", Since: at.Add(-time.Hour),
		Facts: map[string]int64{"count": 3, "net_pence": -421055},
	}}}}
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{{at: at, accounts: accounts}})
	if len(entries) != 1 || entries[0].Message != "engine warning" {
		t.Fatalf("initial active loss warning: %+v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["net_pence"] != int64(-421055) || fields["count"] != int64(3) {
		t.Fatalf("loss log must carry flat integer pence and count, not formatted money: %+v", fields)
	}
}

func warningsAcceptanceClear() []store.AccountWarnings {
	return []store.AccountWarnings{{AccountID: "one", Label: "Yukine", Available: true, Warnings: []store.Warning{}}}
}

func TestWatchWarningsAcceptanceSuppressionPreservesUntilEvaluatedClear(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	active := warningsAcceptanceActive(at, 14)
	active[0].Warnings[0].Code = "fleet_idle"
	active[0].Warnings[0].Severity = "warning"
	active[0].Warnings[0].Facts = map[string]int64{"idle": 1, "total": 1, "unknown": 0, "minutes": 30}
	suppressed := warningsAcceptanceClear()
	suppressed[0].Suppressed = []string{"fleet_idle", "hold_stuck"}
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{
		{at: at, accounts: active},
		{at: at.Add(time.Minute), accounts: suppressed},
		{at: at.Add(40 * time.Minute), accounts: suppressed},
		{at: at.Add(41 * time.Minute), accounts: active},
		{at: at.Add(42 * time.Minute), accounts: warningsAcceptanceClear()},
	})
	want := []string{"engine warning", "engine warning persists", "engine warning cleared"}
	if len(entries) != len(want) {
		t.Fatalf("suppression must not clear, re-raise or claim persistence from unevaluated data: %+v", entries)
	}
	for i, entry := range entries {
		if entry.Message != want[i] {
			t.Fatalf("entry %d=%q, want %q", i, entry.Message, want[i])
		}
	}
	if entries[2].ContextMap()["active_for"] != 42*time.Minute {
		t.Fatalf("suppression reset active duration: %+v", entries[2].ContextMap())
	}
}

func TestWatchWarningsAcceptanceUnavailableAccountPreservesAndIsolates(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	active := warningsAcceptanceActive(at, 14)
	second := active[0]
	second.AccountID, second.Label = "two", "Second"
	both := append([]store.AccountWarnings{active[0]}, second)
	clearSecond := store.AccountWarnings{AccountID: "two", Label: "Second", Available: true, Warnings: []store.Warning{}}
	unavailable := store.AccountWarnings{AccountID: "one", Label: "Yukine", Available: false, ErrorCode: "snapshot_invalid", Warnings: []store.Warning{}, Suppressed: []string{}}
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{
		{at: at, accounts: both},
		{at: at.Add(time.Minute), accounts: []store.AccountWarnings{unavailable, clearSecond}},
		{at: at.Add(2 * time.Minute), accounts: []store.AccountWarnings{unavailable, clearSecond}},
		{at: at.Add(3 * time.Minute), accounts: []store.AccountWarnings{active[0], clearSecond}},
		{at: at.Add(4 * time.Minute), accounts: []store.AccountWarnings{unavailable, clearSecond}},
		{at: at.Add(5 * time.Minute), accounts: append(warningsAcceptanceClear(), clearSecond)},
	})
	want := []string{"engine warning", "engine warning", "engine warnings unavailable", "engine warning cleared", "engine warnings unavailable", "engine warning cleared"}
	if len(entries) != len(want) {
		t.Fatalf("account failure duplicated warnings, cleared unavailable state, or hid another account's clear: %+v", entries)
	}
	for i, entry := range entries {
		if entry.Message != want[i] {
			t.Fatalf("entry %d=%q, want %q", i, entry.Message, want[i])
		}
		if entry.Message == "engine warnings unavailable" {
			fields := entry.ContextMap()
			if fields["account_id"] != "one" || fields["account"] != "Yukine" || fields["error"] != "snapshot_invalid" || entry.Level != zapcore.WarnLevel {
				t.Fatalf("per-account unavailable log must carry only safe structured status: %+v", entry)
			}
		}
	}
	if entries[3].ContextMap()["account_id"] != "two" || entries[5].ContextMap()["account_id"] != "one" || entries[5].ContextMap()["active_for"] != 5*time.Minute {
		t.Fatalf("account availability streak mixed identities or reset active duration: %+v", entries)
	}
}

func TestWatchWarningsAcceptanceMissingAccountDoesNotClear(t *testing.T) {
	at := time.Date(2026, 10, 27, 2, 14, 0, 0, time.UTC)
	active := warningsAcceptanceActive(at, 14)
	entries := warningsAcceptanceRun(t, []warningsAcceptanceStep{
		{at: at, accounts: active},
		{at: at.Add(time.Minute)},
		{at: at.Add(2 * time.Minute), accounts: active},
		{at: at.Add(3 * time.Minute), accounts: warningsAcceptanceClear()},
	})
	if len(entries) != 2 || entries[0].Message != "engine warning" || entries[1].Message != "engine warning cleared" || entries[1].ContextMap()["active_for"] != 3*time.Minute {
		t.Fatalf("omitted account falsely cleared or re-raised an unassessed warning: %+v", entries)
	}
}
