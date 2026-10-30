package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestReplayQueuedFrameOwnsSnapshotMaps(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	admin := replayRecorderSQL(t, h)
	lock, err := admin.Conn(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(h.ctx, "BEGIN; LOCK TABLE replay_frames IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	defer lock.ExecContext(context.Background(), "ROLLBACK")
	before := bytes.Clone(r.snapshot["profile"])
	r.flushReplay(h.ctx, at)
	r.snapshot["profile"] = json.RawMessage(`{"next_pass":true}`)
	r.meta["profile"] = DatasetMeta{Generation: "next-pass", Complete: true}
	r.snapshot["books"] = json.RawMessage(`{}`)
	if _, err := lock.ExecContext(h.ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	if !bytes.Equal(frame.Datasets["profile"].Data, before) || len(frame.Datasets["books/quay-port"].Data) == 0 {
		t.Fatal("background writer read the next pass's mutable snapshot maps")
	}
}

func TestReplayEventsAreBoundedAndDiscloseDrops(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	at := time.Now().UTC().Truncate(time.Microsecond)
	r.generation, r.passStart = "bounded-events", at
	r.snapshot["clock"] = encode(Clock{UnlockedPhase: 1})
	r.meta["clock"] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
	for i := range replayEventLimit + 5 {
		r.replayEvent(h.ctx, strconv.Itoa(i), "synthetic", json.RawMessage(`{"value":1}`))
	}
	if len(r.replay.events) != replayEventLimit || r.replay.eventBytes > replayEventBytes || r.replay.droppedEvents != 5 {
		t.Fatalf("event count bound was not enforced: %d events, %d bytes, %d dropped",
			len(r.replay.events), r.replay.eventBytes, r.replay.droppedEvents)
	}
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	if !frame.ObservedAt.Equal(at) {
		t.Fatalf("live observation did not use pass start: %s", frame.ObservedAt)
	}
	var events []store.ReplayEvent
	if err := json.Unmarshal(frame.Datasets["events"].Data, &events); err != nil || len(events) != replayEventLimit {
		t.Fatalf("retained events changed: %d, %v", len(events), err)
	}
	var metadata struct {
		Complete      bool  `json:"complete"`
		DroppedEvents int64 `json:"dropped_events"`
		DroppedBytes  int64 `json:"dropped_bytes"`
	}
	if err := json.Unmarshal(frame.Datasets["events"].Meta, &metadata); err != nil || metadata.Complete || metadata.DroppedEvents != 5 || metadata.DroppedBytes == 0 {
		t.Fatalf("event gaps were not disclosed: %+v, %v", metadata, err)
	}
	payload := encode(map[string]string{"value": strings.Repeat("x", replayEventBytes)})
	r.replayEvent(h.ctx, "oversized", "synthetic", payload)
	if len(r.replay.events) != 0 || r.replay.droppedEvents != 1 || r.replay.droppedBytes < int64(len(payload)) {
		t.Fatal("an oversized individual event bypassed the byte bound")
	}
}

func TestReplayEventCaptureBoundsNumericTokensAndNesting(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	at := time.Now().UTC().Truncate(time.Microsecond)
	r.generation, r.passStart = "event-ingest-gaps", at
	r.snapshot["clock"] = encode(Clock{UnlockedPhase: 1})
	r.meta["clock"] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
	tests := []struct {
		name    string
		payload string
		reason  string
	}{
		{name: "zero-exponent", payload: `{"nested":[0e-16000]}`, reason: "numeric token outside storage bounds"},
		{name: "large-exponent", payload: `{"value":1e130000}`, reason: "numeric token outside storage bounds"},
		{name: "long-number", payload: `{"value":` + strings.Repeat("9", 33) + `}`, reason: "numeric token outside storage bounds"},
		{name: "deep", payload: strings.Repeat("[", 10000) + "0" + strings.Repeat("]", 10000), reason: "JSON nesting outside storage bounds"},
		{name: "healthy", payload: `{"value":1}`},
	}
	for _, test := range tests {
		r.replayEvent(h.ctx, test.name, "synthetic", json.RawMessage(test.payload))
	}
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	var events []store.ReplayEvent
	if err := json.Unmarshal(frame.Datasets["events"].Data, &events); err != nil || len(events) != len(tests) {
		t.Fatalf("event gaps lost their envelopes: rows=%d err=%v", len(events), err)
	}
	for i, event := range events {
		test := tests[i]
		if event.EventID != test.name || event.Type != "synthetic" || event.CreatedAt.Before(at) || event.CreatedAt.After(time.Now().UTC()) {
			t.Fatalf("event gap lost identity/type/receipt time: %+v", event)
		}
		if test.reason != "" {
			var gap struct {
				Gap    bool   `json:"gap"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(event.Payload, &gap); err != nil || !gap.Gap || gap.Reason != test.reason {
				t.Fatalf("unsafe replay event did not become a reasoned gap: %s, %v", event.Payload, err)
			}
		} else if !bytes.Equal(event.Payload, []byte(test.payload)) {
			t.Fatalf("healthy replay event changed: %s", event.Payload)
		}
	}
}

func TestReplayWriterWaitsForExplicitStopAfterOwnerCancellation(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	ctx, cancel := context.WithCancel(h.ctx)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	at := time.Now().UTC()
	r.generation, r.passStart = "cancelled-owner", at
	r.snapshot["clock"] = encode(Clock{UnlockedPhase: 1})
	r.meta["clock"] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
	recorder := r.startReplay(ctx)
	cancel()
	select {
	case <-recorder.done:
		t.Fatal("writer drained before Run's explicit stop handoff")
	case <-time.After(100 * time.Millisecond):
	}
	r.replayEvent(ctx, "final-event", "synthetic", json.RawMessage(`{"value":1}`))
	r.stopReplay(ctx)
	select {
	case <-recorder.done:
	default:
		t.Fatal("explicit stop did not drain the writer")
	}
	frames := replayRecorderFrames(t, h)
	if len(frames) != 1 || !bytes.Contains(frames[0].Datasets["events"].Data, []byte("final-event")) {
		t.Fatal("owner cancellation lost the final event handoff")
	}
}

func TestReplayFailedEvidenceCanRetryAndPhaseChangesAreRecorded(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	r.generation = "retry-evidence"
	admin := replayRecorderSQL(t, h)
	_, err := admin.ExecContext(h.ctx, `CREATE FUNCTION reject_replay_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected replay failure'; END; $$;
		CREATE TRIGGER reject_replay_plan BEFORE INSERT ON replay_plans FOR EACH ROW EXECUTE FUNCTION reject_replay_evidence();
		CREATE TRIGGER reject_replay_link BEFORE INSERT ON replay_action_links FOR EACH ROW EXECUTE FUNCTION reject_replay_evidence();`)
	if err != nil {
		t.Fatal(err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Generation: "retry-mission", Phase: "buy"}
	data := &missionData{Plan: Plan{Kind: "spot", ShipID: recoveryShipLapras}, Sequence: 2,
		Action: &durableAction{ID: "retry-action", Stage: "buy", CreatedAt: time.Now().UTC()}}
	r.replayPlan(h.ctx, mission, data)
	r.replayLink(h.ctx, mission, data)
	replayRecorderWait(t, func() bool {
		r.replay.mu.Lock()
		defer r.replay.mu.Unlock()
		_, planned := r.replay.planDigests[mission.Generation]
		return !planned && !r.replay.linked[data.Action.ID]
	})
	if _, err := admin.ExecContext(h.ctx, "DROP TRIGGER reject_replay_plan ON replay_plans; DROP TRIGGER reject_replay_link ON replay_action_links"); err != nil {
		t.Fatal(err)
	}
	r.replayPlan(h.ctx, mission, data)
	r.replayLink(h.ctx, mission, data)
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		links, linkErr := h.db.ReplayActionLinks(h.ctx, h.account.ID, time.Unix(0, 0).UTC(), time.Now().Add(time.Hour), []string{mission.Generation})
		return err == nil && linkErr == nil && len(plans) == 1 && len(links) == 1
	})
	mission.Phase = "depart"
	r.replayPlan(h.ctx, mission, data)
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		return err == nil && len(plans) == 2 && plans[1].Phase == "depart"
	})
}

func TestReplayStartupImportsDoNotClaimNewDecisions(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	recorder := r.startReplay(h.ctx)
	recorder.importing = true
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Generation: "original-mission",
		Phase: "buy", UpdatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	data := &missionData{Plan: Plan{Kind: "spot"}, Action: &durableAction{ID: "original-action", Stage: "buy"}}
	r.replayPlan(h.ctx, mission, data)
	r.replayLink(h.ctx, mission, data)
	recorder.importing = false
	if recorder.decision || recorder.postDecision {
		t.Fatal("startup evidence was labelled as a newly accepted decision")
	}
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		links, linkErr := h.db.ReplayActionLinks(h.ctx, h.account.ID, time.Unix(0, 0).UTC(), time.Now().Add(time.Hour), []string{mission.Generation})
		return err == nil && linkErr == nil && len(plans) == 1 && len(links) == 1 &&
			plans[0].Generation == mission.Generation && links[0].Generation == mission.Generation &&
			plans[0].ObservedAt.Equal(mission.UpdatedAt)
	})
}

func TestReplayCompetitionDoesNotConsumeSnapshotBudget(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	r.opts.Execute = false
	r.competition = &store.CompetitionSummary{IdentityBasis: strings.Repeat("x", store.MaxSnapshotBytes+1)}
	r.competitionNextLoad = time.Now().Add(time.Hour)
	if _, err := h.tick(r); err != nil {
		t.Fatalf("replay-only competition exhausted the operational snapshot budget: %v", err)
	}
	if _, exists := r.snapshot["_competition"]; exists {
		t.Fatal("large competition evidence entered the operational snapshot")
	}
	for _, keys := range [][]string{replayPassRunnerKeys, replaySweepRunnerKeys} {
		for _, key := range keys {
			if _, exists := r.meta[key]; exists {
				t.Fatalf("replay-only metadata entered operational health: %s", key)
			}
		}
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil || len(encode(snapshot)) > store.MaxSnapshotBytes {
		t.Fatalf("operational snapshot exceeds its cap: %v", err)
	}
}

func TestReplayLiveWriterPersistsRepeatedAggregateOverflowAsGaps(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	at := time.Now().UTC().Truncate(time.Microsecond)
	r.generation, r.passStart = "aggregate-live-0", at
	r.snapshot["clock"] = encode(Clock{UnlockedPhase: 1})
	payload := json.RawMessage(`"` + strings.Repeat("x", 6<<20) + `"`)
	for _, key := range []string{"clock", "profile", "settings", "zones"} {
		if key != "clock" {
			r.snapshot[key] = payload
		}
		r.meta[key] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
	}
	for opportunity := range 2 {
		observedAt := at.Add(time.Duration(opportunity) * time.Minute)
		replayRecorderNextGeneration(r, "aggregate-live-"+strconv.Itoa(opportunity), observedAt)
		r.flushReplay(h.ctx, observedAt)
		replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == opportunity+1 })
	}
	frames := replayRecorderFrames(t, h)
	for _, frame := range frames {
		var gap struct {
			Complete      bool   `json:"complete"`
			Stale         bool   `json:"stale"`
			Error         string `json:"error"`
			OriginalBytes int    `json:"original_bytes"`
		}
		if err := json.Unmarshal(frame.Datasets["profile"].Meta, &gap); err != nil || gap.Complete || !gap.Stale ||
			gap.Error == "" || gap.OriginalBytes != len(payload) || len(frame.Datasets["profile"].Data) != 0 {
			t.Fatalf("live aggregate did not disclose deterministic largest gap: %+v, %v", gap, err)
		}
		if !bytes.Equal(frame.Datasets["settings"].Data, payload) || !bytes.Equal(frame.Datasets["zones"].Data, payload) {
			t.Fatal("live aggregate normalization lost retained evidence")
		}
	}
	if !bytes.Equal(r.snapshot["profile"], payload) || !r.meta["profile"].Complete || r.meta["profile"].Stale {
		t.Fatal("live writer normalization mutated runner evidence")
	}
	stats, err := h.db.ServiceStats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var demotions float64
	for _, counter := range stats.Counters {
		if counter.AccountID == h.account.ID && counter.Name == "replay_record_errors_total" {
			demotions += counter.Value
		}
	}
	if demotions != 2 {
		t.Fatalf("successful live gap frames did not count each dataset loss: %g", demotions)
	}
}

func TestReplayPassRetainsCalibrationAndActiveMarquePlanningInputs(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	t.Cleanup(func() { r.stopReplay(context.Background()) })
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	r.snapshot["_revenue_calibration"] = encode(map[string]int64{"depth": 9500})
	r.snapshot["_marque_active"] = encode(map[string]letterOfMarque{"observed-marque": {
		ID: "55555555-5555-4555-8555-000000000001", SeaZoneID: "retained-danger",
		SponsorCompanyID: h.account.CompanyID, AddedPiracyBPS: 500, CostPence: 1000,
		ExpiresAt: at.Add(time.Hour), CreatedAt: at.Add(-time.Minute), UpdatedAt: at,
	}})
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	snapshot := make(map[string]json.RawMessage)
	for key, dataset := range frame.Datasets {
		snapshot[key] = dataset.Data
	}
	for _, key := range []string{"_revenue_calibration", "_marque_active"} {
		if !bytes.Equal(snapshot[key], r.snapshot[key]) {
			t.Fatalf("recorded %s differs from live input: %s, want %s", key, snapshot[key], r.snapshot[key])
		}
	}
	replayed := PlanningInputFor(at, ShipState{}, r.stateCache, snapshot, PlanningSources{
		Metadata: r.meta, Generation: r.generation,
	})
	if replayed.RevenueCalibration["depth"] != 9500 || !replayed.MarqueZones["retained-danger"] {
		t.Fatalf("recorded pass changed live calibration or marque route exclusions: %+v, %+v",
			replayed.RevenueCalibration, replayed.MarqueZones)
	}
}
