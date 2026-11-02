package engine

import (
	"testing"
	"time"
)

func TestFuturesDatasetUsesHourlyBackstopAcrossCaptures(t *testing.T) {
	now := time.Now()
	runner := &Runner{generation: "next-capture", meta: map[string]DatasetMeta{
		"futures": {Generation: "previous-capture", Complete: true, UpdatedAt: now.Add(-30 * time.Minute)},
	}}
	if !runner.fresh("futures") {
		t.Fatal("unchanged futures were re-polled merely because another capture began")
	}
	meta := runner.meta["futures"]
	meta.UpdatedAt = now.Add(-time.Hour)
	runner.meta["futures"] = meta
	if runner.fresh("futures") {
		t.Fatal("hourly gap backstop retained expired list authority")
	}
	meta.UpdatedAt = now
	runner.meta["futures"] = meta
	runner.invalidateFutures()
	if runner.fresh("futures") {
		t.Fatal("an exchange event failed to force authoritative resynchronization")
	}
}

func TestFuturesEventsInvalidateEveryListingAndObligationChange(t *testing.T) {
	for _, event := range []string{"futures_listed", "futures_accepted", "futures_settled", "futures_defaulted", "futures_expired", "futures_claim_paid", "exchange_posted", "exchange_withdrawn"} {
		if !futuresDatasetEvent(event) {
			t.Errorf("%s did not wake futures collection", event)
		}
	}
	if futuresDatasetEvent("ship_arrived") || futuresDatasetEvent("marque_prize_paid") {
		t.Fatal("unrelated traffic restarted derivatives polling")
	}
}
