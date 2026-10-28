package sim

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestTapeCharterPrefixCacheIsBoundedAndShared(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	calls := 0
	tape := &Tape{readCharterPrefix: func(since, before time.Time) (int64, error) {
		calls++
		if !before.Equal(at) {
			return 0, errors.New("prefix unavailable")
		}
		return int64(at.Sub(since) / time.Minute), nil
	}}
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			got, present, err := tape.charterProceedsBefore(at.Add(-time.Hour), at)
			if err != nil || !present || got != 60 {
				t.Errorf("shared prefix changed: value=%d present=%t err=%v", got, present, err)
			}
		}()
	}
	workers.Wait()
	if calls != 1 {
		t.Fatalf("identical concurrent prefix reads made %d source queries", calls)
	}
	if value, present, err := tape.charterProceedsBefore(at.Add(-30*time.Minute), at); err != nil || !present || value != 30 || calls != 2 {
		t.Fatalf("new rolling cutoff reused stale prefix: value=%d present=%t calls=%d err=%v", value, present, calls, err)
	}
	if _, present, err := tape.charterProceedsBefore(at, at.Add(time.Hour)); err == nil || !present {
		t.Fatal("missing database prefix silently became a synthetic fallback")
	}
	if _, present, err := new(Tape).charterProceedsBefore(at, at); err != nil || present {
		t.Fatal("synthetic tape acquired an unavailable database prefix")
	}
}

func TestTapeProbePrefixCacheQueriesOnlyNecessaryLanes(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	calls := 0
	tape := &Tape{readProbePrefix: func(lanes []string, before time.Time) (map[string]time.Time, error) {
		calls++
		if len(lanes) > 1024 || !before.Equal(at) {
			t.Fatalf("prefix source received an unbounded shortlist or wrong cutover: count=%d at=%s", len(lanes), before)
		}
		result := make(map[string]time.Time)
		for _, lane := range lanes {
			if lane != "missing" {
				result[lane] = at.Add(-time.Minute)
			}
		}
		return result, nil
	}}
	lanes := make([]string, 1501)
	for i := range 1500 {
		lanes[i] = fmt.Sprintf("source|destination|cloth|probe-%d", i)
	}
	lanes[1500] = "missing"
	for range 2 {
		rows, err := tape.probeCompletionsBefore(lanes, at)
		if err != nil || len(rows) != 1500 || !rows[lanes[0]].Equal(at.Add(-time.Minute)) {
			t.Fatalf("cached shortlist lost completions: count=%d err=%v", len(rows), err)
		}
	}
	if calls != 2 || len(tape.probePrefixes) != len(lanes) {
		t.Fatalf("repeated positive/negative lane lookups reloaded history: calls=%d cache=%d", calls, len(tape.probePrefixes))
	}
	tape.probePrefixBytes = maxTapeRetainedBytes - 1
	if rows, err := tape.probeCompletionsBefore([]string{"new-lane"}, at); !errors.Is(err, store.ErrReplayEvidenceBudget) || rows != nil || calls != 2 {
		t.Fatalf("over-budget prefix fetched or returned partial evidence: rows=%v calls=%d err=%v", rows, calls, err)
	}
}
