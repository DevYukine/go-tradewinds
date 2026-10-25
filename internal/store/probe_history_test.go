package store

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestRelocationProbeCompletionsKeepLatestVerifiedScope(t *testing.T) {
	db, dsn, key := integrationStore(t)
	ctx := context.Background()
	const accountID = "probe-completion-one"
	const otherID = "probe-completion-two"
	const lane = "probe-v2|ship-a|origin|destination|grain|evidence"
	const legacy = "origin|destination|grain|old-market"
	const future = "probe-v2|ship-a|origin|destination|grain|future"
	saveTestAccount(t, db, accountID)
	saveTestAccount(t, db, otherID)
	asOf := time.Date(2026, 10, 24, 12, 0, 0, 123456000, time.UTC)
	for _, row := range []Settlement{
		{ID: "probe-old", AccountID: accountID, Lane: lane, Kind: "relocation", CompletedAt: asOf.Add(-48 * time.Hour)},
		{ID: "probe-latest", AccountID: accountID, Lane: lane, Kind: "relocation", CompletedAt: asOf.Add(-23 * time.Minute)},
		{ID: "probe-legacy", AccountID: accountID, Lane: legacy, Kind: "relocation", CompletedAt: asOf.Add(-24 * time.Minute)},
		{ID: "probe-future", AccountID: accountID, Lane: future, Kind: "relocation", CompletedAt: asOf.Add(time.Microsecond)},
		{ID: "probe-other-ship", AccountID: accountID, Lane: "probe-v2|ship-b|origin|destination|grain|evidence", Kind: "relocation", CompletedAt: asOf},
		{ID: "probe-other-account", AccountID: otherID, Lane: lane, Kind: "relocation", CompletedAt: asOf.Add(time.Hour)},
		{ID: "probe-not-relocation", AccountID: accountID, Lane: lane, Kind: "spot", CompletedAt: asOf.Add(2 * time.Hour)},
	} {
		// Costs and duration are intentionally unknown: completed evidence is
		// still a permanent probe attempt and cannot receive a free retry.
		row.CompletedAt = row.CompletedAt.In(time.FixedZone("host", 9*60*60))
		putGrowthHistoryEvidence(t, db, row)
	}
	lanes := []string{lane, lane, legacy, future, "missing' OR true --"}
	want := map[string]time.Time{
		lane: asOf.Add(-23 * time.Minute), legacy: asOf.Add(-24 * time.Minute), future: asOf.Add(time.Microsecond),
	}
	check := func(db *Store) {
		t.Helper()
		got, err := db.RelocationProbeCompletions(ctx, accountID, lanes)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("completion lookup lost latest/legacy/account evidence: %+v, %v", got, err)
		}
		for _, completed := range got {
			if completed.Location() != time.UTC {
				t.Fatalf("completion escaped the recorded UTC clock: %v", completed)
			}
		}
		permanent, err := db.RelocationProbes(ctx, accountID, lanes)
		wantPermanent := []string{lane, legacy, future}
		sort.Strings(wantPermanent)
		if err != nil || !reflect.DeepEqual(permanent, wantPermanent) {
			t.Fatalf("completion API changed permanent sorted membership: %+v, %v", permanent, err)
		}
		other, err := db.RelocationProbeCompletions(ctx, otherID, []string{lane, legacy})
		if err != nil || len(other) != 1 || !other[lane].Equal(asOf.Add(time.Hour)) {
			t.Fatalf("account-specific completion leaked: %+v, %v", other, err)
		}
	}
	check(db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dsn, key, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	check(reopened)
	if err := reopened.db.Model(&settlementRow{}).Where("id = ?", "probe-latest").
		Update("context_cipher", []byte("invalid authenticated context")).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.RelocationProbeCompletions(ctx, accountID, []string{lane}); err == nil {
		t.Fatal("completion query trusted an undecryptable lane identity")
	}
	if _, err := reopened.RelocationProbes(ctx, accountID, []string{lane}); err == nil {
		t.Fatal("permanent membership bypassed shared encrypted-lane verification")
	}
}

func TestRelocationProbeQueriesShareBoundedValidation(t *testing.T) {
	db := &Store{}
	ctx := context.Background()
	for _, test := range []struct {
		account string
		lanes   []string
	}{
		{account: ""},
		{account: strings.Repeat("a", 256)},
		{account: "probe-valid", lanes: make([]string, 1025)},
		{account: "probe-valid", lanes: []string{""}},
		{account: "probe-valid", lanes: []string{strings.Repeat("a", 1025)}},
	} {
		if _, err := db.RelocationProbeCompletions(ctx, test.account, test.lanes); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid completion query reached storage: %v", err)
		}
		if _, err := db.RelocationProbes(ctx, test.account, test.lanes); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid permanent query reached storage: %v", err)
		}
	}
	completions, err := db.RelocationProbeCompletions(ctx, "probe-valid", nil)
	if err != nil || completions == nil || len(completions) != 0 {
		t.Fatalf("empty completion shortlist queried storage: %+v, %v", completions, err)
	}
	permanent, err := db.RelocationProbes(ctx, "probe-valid", nil)
	if err != nil || permanent == nil || len(permanent) != 0 {
		t.Fatalf("empty permanent shortlist changed semantics: %+v, %v", permanent, err)
	}
}
