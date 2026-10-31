package engine

import (
	"github.com/DevYukine/go-tradewinds/internal/store"
	"strconv"
	"testing"
	"time"
)

// Live prices change a rejected plan's figures every cycle. That used to write
// about 60 decision rows per second; only a changed reason class or the
// refresh interval may record again.
func TestDecisionPriceChurnRecordsOncePerClass(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	record := func(reason string, price int64) {
		t.Helper()
		if err := runner.decision(h.ctx, recoveryShipBlastoise, "rejected:spot:portb:grain:", reason, map[string]int64{"price_pence": price}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		t.Helper()
		history, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
		if err != nil {
			t.Fatal(err)
		}
		decisions, ok := history.([]store.Decision)
		if !ok {
			t.Fatalf("unexpected decision history %T", history)
		}
		return len(decisions)
	}
	before := count()
	for price := int64(100); price < 110; price++ {
		record("non-positive net: expected cash net -"+strconv.FormatInt(price*13, 10)+"p", price)
	}
	if got := count() - before; got != 1 {
		t.Fatalf("price churn wrote %d rows, want 1", got)
	}
	record("destination has neither executable full depth nor a fresh book", 100)
	if got := count() - before; got != 2 {
		t.Fatalf("a new reason class wrote %d rows in total, want 2", got)
	}
	cached := runner.decisionHashes[recoveryShipBlastoise+"|rejected:spot:portb:grain:"]
	cached.writtenAt = cached.writtenAt.Add(-time.Hour)
	runner.decisionHashes[recoveryShipBlastoise+"|rejected:spot:portb:grain:"] = cached
	record("destination has neither executable full depth nor a fresh book", 101)
	if got := count() - before; got != 3 {
		t.Fatalf("changed figures after the refresh interval wrote %d rows in total, want 3", got)
	}
	// Event kinds keep a constant reason and carry identity in details; every
	// distinct event must still be recorded.
	for _, actionID := range []string{"action-a", "action-b"} {
		if err := runner.decision(h.ctx, recoveryShipBlastoise, "action_rejected", "sell rejected with HTTP 409", map[string]string{"action_id": actionID}); err != nil {
			t.Fatal(err)
		}
	}
	if got := count() - before; got != 5 {
		t.Fatalf("two distinct rejected actions wrote %d rows in total, want 5", got)
	}
}
