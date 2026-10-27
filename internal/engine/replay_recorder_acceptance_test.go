package engine

// Assumed seam: (*Runner).flushReplay(context.Context, time.Time) (no return),
// called after work and before SaveSnapshot. Options and config.Config have
// ReplayRetentionDays int and ReplayMaxGiB float64. Store replay signatures and
// types are documented in internal/store/replay_acceptance_test.go; Meta is the
// JSON encoding of DatasetMeta, not a second metadata schema. ReplayPlan.MissionID
// is the mission's Generation; ReplayActionLink.Generation is the staging pass.
// flushReplay must finish a write within 5 seconds; mission saves/staging never
// wait on recorder storage. Tests reuse the real recovery integration harness.
// Runner.passStart time.Time marks this generation's start for failed attempts.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/config"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func replayRecorderFrames(t *testing.T, h *recoveryHarness) []store.ReplayFrame {
	t.Helper()
	var frames []store.ReplayFrame
	if err := h.db.ScanReplayFrames(h.ctx, h.account.ID, func(frame store.ReplayFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return frames
}

func replayRecorderWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("recorder did not finish bounded background work")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func replayRecorderRunner(t *testing.T, h *recoveryHarness) *Runner {
	t.Helper()
	r := h.runner(primaryTestKey)
	r.opts.ReplayRetentionDays = 90
	r.opts.ReplayMaxGiB = 20
	return r
}

func replayRecorderSeed(t *testing.T, h *recoveryHarness, r *Runner, at time.Time) {
	t.Helper()
	execute := r.opts.Execute
	r.opts.Execute = false
	if _, err := h.tick(r); err != nil {
		t.Fatal(err)
	}
	r.opts.Execute = execute
	r.passStart = at
	r.snapshot = make(map[string]json.RawMessage)
	r.meta = make(map[string]DatasetMeta)
	for _, key := range []string{
		"clock", "catalog", "zones", "profile", "vaults", "loans", "rates", "ships",
		"warehouses", "orders", "fills", "factors", "monopolies", "marques", "contracts", "expansion",
		"_ship_idle", "_marque_pending", "_underwriting_pending", "_route_phase",
		"_capacity_savings_target", "_capacity_savings_expired", "_capacity_growth_port", "_expansion_agents",
		"_sourcing_probe_history", "_growth_build_backoffs", "_capacity_financing_backoffs", "_capacity_growth_paused_until", "_ship_losses",
	} {
		r.snapshot[key] = json.RawMessage(`{"input":"` + key + `"}`)
		if !strings.HasPrefix(key, "_") {
			r.meta[key] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
		}
	}
	r.snapshot["clock"] = encode(Clock{UnlockedPhase: 1})
	for group, children := range map[string][]string{
		"ship_details": {"ship-current"}, "books": {"quay-port"},
		"ports": {"quay-port", "remote-port"}, "routes": {"quay-port|remote-port|hoy"},
		"yard_quotes": {"quay-port|hoy"},
	} {
		values := make(map[string]json.RawMessage)
		for _, child := range children {
			values[child] = json.RawMessage(`{ "id": "` + child + `" }`)
			r.meta[group+"/"+child] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
		}
		r.snapshot[group] = encode(values)
	}
	r.snapshot["ports"] = encode(map[string]json.RawMessage{
		"quay-port":   json.RawMessage(`{"id":"quay-port","visibility":"quay"}`),
		"remote-port": json.RawMessage(`{"id":"remote-port","visibility":"public"}`),
	})
	r.stateCache.Ports = map[string]PortDetail{
		"quay-port":   {ID: "quay-port", Visibility: "quay"},
		"remote-port": {ID: "remote-port", Visibility: "public"},
	}
	r.snapshot["ledger"] = json.RawMessage(`[{"must_not_record":true}]`)
	r.snapshot["insurance"] = json.RawMessage(`[{"must_not_record":true}]`)
	r.snapshot["whoami"] = json.RawMessage(`{"must_not_record":true}`)
	r.snapshot["unknown_private_dataset"] = json.RawMessage(`{"must_not_record":true}`)
	r.snapshot["zones"] = json.RawMessage(`{"stale":true}`)
	r.meta["zones"] = DatasetMeta{Generation: "previous-pass", AttemptedAt: at.Add(-time.Minute), UpdatedAt: at.Add(-time.Minute), Complete: true}
}

func replayRecorderNextGeneration(r *Runner, generation string, at time.Time) {
	previous := r.generation
	r.generation = generation
	r.passStart = at
	for key, meta := range r.meta {
		if meta.Generation == previous {
			meta.Generation = generation
			meta.AttemptedAt = at
			meta.UpdatedAt = at
			r.meta[key] = meta
		}
	}
}

func TestReplayRecorderAcceptanceGenerationAllowlistAndSweep(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	at := time.Now().UTC().Truncate(time.Microsecond)
	replayRecorderSeed(t, h, r, at)
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	startup := replayRecorderFrames(t, h)[0]
	if startup.Generation != r.generation || startup.Source != "live" {
		t.Fatalf("startup frame header: %+v", startup)
	}
	for _, key := range []string{"ledger", "insurance", "whoami", "unknown_private_dataset", "zones"} {
		if _, exists := startup.Datasets[key]; exists {
			t.Fatalf("startup imported stale or non-allowlisted %s", key)
		}
	}
	for key, data := range startup.Datasets {
		if key == "events" {
			if !json.Valid(data.Data) {
				t.Fatal("startup events must be a JSON dataset")
			}
			continue
		}
		var expected json.RawMessage
		if key == "_competition" {
			expected = encode(r.competition)
		} else if key == "_charter_proceeds" {
			expected = encode(r.charterProceedsPence)
		} else if group, child, ok := strings.Cut(key, "/"); ok {
			expected = decode[map[string]json.RawMessage](r.snapshot, group)[child]
		} else {
			expected = r.snapshot[key]
		}
		if !bytes.Equal(data.Data, expected) {
			t.Fatalf("dataset %s differs from its runner source bytes: %q != %q", key, data.Data, expected)
		}
		var meta DatasetMeta
		if err := json.Unmarshal(data.Meta, &meta); err != nil {
			t.Fatalf("dataset %s metadata is invalid: %v", key, err)
		}
		if strings.HasPrefix(key, "_") {
			if meta.Generation != r.generation || !meta.Complete || meta.AttemptedAt.IsZero() || meta.UpdatedAt.IsZero() || meta.Stale || meta.Error != "" {
				t.Fatalf("runner input %s lacks current-generation frame-only metadata: %+v", key, meta)
			}
			if _, exists := r.meta[key]; exists {
				t.Fatalf("recording runner input %s polluted persisted metadata", key)
			}
		} else if !reflect.DeepEqual(meta, r.meta[key]) {
			t.Fatalf("dataset %s metadata changed: %+v", key, meta)
		}
	}
	if _, exists := r.snapshot["_competition"]; exists {
		t.Fatal("recording copied competition into the size-critical snapshot")
	}
	for _, key := range []string{
		"clock", "catalog", "profile", "vaults", "loans", "rates", "ships", "warehouses", "orders", "fills", "factors", "monopolies", "marques", "contracts", "expansion", "events",
		"ship_details/ship-current", "books/quay-port", "ports/quay-port", "ports/remote-port", "routes/quay-port|remote-port|hoy", "yard_quotes/quay-port|hoy",
		"_ship_idle", "_marque_pending", "_underwriting_pending", "_competition", "_charter_proceeds", "_route_phase",
		"_capacity_savings_target", "_capacity_savings_expired", "_capacity_growth_port", "_expansion_agents",
		"_sourcing_probe_history", "_growth_build_backoffs", "_capacity_financing_backoffs", "_capacity_growth_paused_until", "_ship_losses",
	} {
		if _, exists := startup.Datasets[key]; !exists {
			t.Fatalf("startup omitted %s", key)
		}
	}
	replayRecorderNextGeneration(r, "interval-generation", at.Add(time.Minute))
	r.flushReplay(h.ctx, at.Add(time.Minute))
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 2 })
	interval := replayRecorderFrames(t, h)[1]
	if _, exists := interval.Datasets["ports/quay-port"]; !exists {
		t.Fatal("interval omitted quay presence port")
	}
	for _, key := range []string{"ports/remote-port", "routes/quay-port|remote-port|hoy"} {
		if _, exists := interval.Datasets[key]; exists {
			t.Fatalf("ordinary interval unexpectedly swept %s", key)
		}
	}
	replayRecorderNextGeneration(r, "sweep-generation", at.Add(5*time.Minute))
	r.flushReplay(h.ctx, at.Add(5*time.Minute))
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 3 })
	for _, key := range []string{"ports/remote-port", "routes/quay-port|remote-port|hoy"} {
		if _, exists := replayRecorderFrames(t, h)[2].Datasets[key]; !exists {
			t.Fatalf("five-minute sweep omitted %s", key)
		}
	}
}

func TestReplayRecorderAcceptanceFailedAndOversizeCapturesAreGaps(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	// applyCapture deliberately retains the last-good bytes and old Generation on failure.
	r.meta["books/quay-port"] = DatasetMeta{Generation: "previous-pass", AttemptedAt: at.Add(-time.Hour), UpdatedAt: at.Add(-time.Hour), Complete: true}
	r.applyCapture(h.ctx, "books/quay-port", fetchResult{attemptedAt: at, completedAt: at, err: errors.New("403 no_port_presence")})
	noise := make([]byte, 5<<20)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	large := encode(map[string]string{"uncompressible": base64.StdEncoding.EncodeToString(noise)})
	r.snapshot["books"] = encode(map[string]json.RawMessage{
		"quay-port": json.RawMessage(`{"last_good_but_stale":true}`), "large-port": large,
		"old-failure": json.RawMessage(`{"old_book":true}`),
	})
	r.meta["books/large-port"] = DatasetMeta{Generation: r.generation, AttemptedAt: at, UpdatedAt: at, Complete: true}
	r.meta["books/old-failure"] = DatasetMeta{Generation: "previous-pass", AttemptedAt: at.Add(-time.Hour), Error: "403 no_port_presence", Stale: true}
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	if _, exists := frame.Datasets["books/old-failure"]; exists {
		t.Fatal("a failure from an earlier pass was imported as this pass's gap")
	}
	for _, key := range []string{"books/quay-port", "books/large-port"} {
		gap, exists := frame.Datasets[key]
		var meta DatasetMeta
		if err := json.Unmarshal(gap.Meta, &meta); err != nil || !exists || len(gap.Data) != 0 || meta.Complete || meta.Error == "" {
			t.Fatalf("%s must be a reasoned meta-only gap: %+v, %v", key, gap, err)
		}
		if key == "books/quay-port" && (!meta.AttemptedAt.Equal(at) || !strings.Contains(meta.Error, "no_port_presence")) {
			t.Fatalf("failed attempt lost port/time/reason: %+v", meta)
		}
	}
}

func TestReplayRecorderAcceptanceCadenceAndRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	r.opts.Execute = false
	if _, err := h.tick(r); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	if _, err := h.tick(r); err != nil {
		t.Fatal(err)
	}
	r.flushReplay(h.ctx, at.Add(20*time.Second))
	if frames := replayRecorderFrames(t, h); len(frames) != 1 {
		t.Fatalf("two unchanged passes 20 s apart wrote %d frames", len(frames))
	}
	restarted := replayRecorderRunner(t, h)
	restarted.opts.Execute = false
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	restarted.flushReplay(h.ctx, at.Add(30*time.Second))
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 2 })
	if replayRecorderFrames(t, h)[1].Generation != restarted.generation {
		t.Fatal("restart did not record a new startup generation")
	}
}

func TestReplayRecorderAcceptanceBuyPlanLinkAndUnchangedPlan(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Phase: "buy", Generation: "mission-generation"}
	data := missionData{Sequence: 3, Plan: Plan{
		Kind: "spot", ShipID: recoveryShipLapras, Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain,
		Quantity: 7, Economics: Economics{PurchasePence: 700, ExpectedNetPence: 1234},
	}}
	if err := r.stageAction(h.ctx, mission, &data, ShipState{}, "buy", "/v1/market/place-order", map[string]any{"quantity": 7}, false); err != nil {
		t.Fatal(err)
	}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	r.flushReplay(h.ctx, at)
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		links, linkErr := h.db.ReplayActionLinks(h.ctx, h.account.ID)
		return err == nil && linkErr == nil && len(plans) == 1 && len(links) == 1 && len(replayRecorderFrames(t, h)) == 1
	})
	plans, _ := h.db.ReplayPlans(h.ctx, h.account.ID)
	links, _ := h.db.ReplayActionLinks(h.ctx, h.account.ID)
	var recorded Plan
	if err := json.Unmarshal(plans[0].Plan, &recorded); err != nil || !reflect.DeepEqual(recorded, data.Plan) {
		t.Fatalf("accepted Plan/Economics changed: %+v, %v", recorded, err)
	}
	if plans[0].ShipID != mission.ShipID || plans[0].Phase != mission.Phase || plans[0].Sequence != data.Sequence || plans[0].MissionID != mission.Generation || len(plans[0].Policy) == 0 || plans[0].Revision == "" {
		t.Fatalf("plan omitted identity, policy or VCS revision: %+v", plans[0])
	}
	link := links[0]
	if link.ActionID != actionID(h.account.ID, mission.Generation, "buy", 3) || link.MissionID != mission.Generation || link.Stage != "buy" || link.Sequence != 3 || link.Generation != r.generation {
		t.Fatalf("staged action link mismatch: %+v", link)
	}
	frame := replayRecorderFrames(t, h)[0]
	if !frame.Decision || len(frame.Datasets["ports/remote-port"].Data) == 0 || len(frame.Datasets["routes/quay-port|remote-port|hoy"].Data) == 0 {
		t.Fatalf("buy did not force a full decision frame: %+v", frame)
	}
	// The first post-decision pass must preserve the immediate post-fill state,
	// even though neither the 60 s timer nor another decision is due.
	replayRecorderNextGeneration(r, "post-buy-generation", at.Add(100*time.Millisecond))
	postBook := json.RawMessage(`{"after_buy":true}`)
	r.snapshot["books"] = encode(map[string]json.RawMessage{"quay-port": postBook})
	r.flushReplay(h.ctx, at.Add(100*time.Millisecond))
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 2 })
	post := replayRecorderFrames(t, h)[1]
	if post.Decision || !bytes.Equal(post.Datasets["books/quay-port"].Data, postBook) {
		t.Fatalf("post-decision pass did not record post-fill state: %+v", post)
	}
	replayRecorderNextGeneration(r, "changed-plan-generation", at.Add(20*time.Second))
	data.Plan.Economics.ExpectedNetPence++
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	r.flushReplay(h.ctx, at.Add(20*time.Second))
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		return err == nil && len(plans) == 2 && len(replayRecorderFrames(t, h)) == 3
	})
}

func replayRecorderSQL(t *testing.T, h *recoveryHarness) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestReplayRecorderAcceptanceStoreFailureWarnsOnceAndDoesNotFailRun(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	r.opts.Execute = false
	core, logs := observer.New(zap.WarnLevel)
	r.log = zap.New(core)
	admin := replayRecorderSQL(t, h)
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE replay_frames RENAME TO replay_frames_unavailable"); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := h.tick(r); err != nil {
			t.Fatalf("recorder failure escaped refresh/work: %v", err)
		}
		r.flushReplay(h.ctx, time.Now().Add(time.Duration(i)*time.Minute))
	}
	replayRecorderWait(t, func() bool {
		stats, err := h.db.ServiceStats(h.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, counter := range stats.Counters {
			if counter.Name == "replay_record_errors_total" && counter.Value >= 2 {
				return true
			}
		}
		return false
	})
	if count := logs.FilterMessage("replay record failed").Len(); count != 1 {
		t.Fatalf("same store cause emitted %d warnings, want 1", count)
	}
	// Run must keep refreshing even while recording fails, until cancellation.
	ctx, cancel := context.WithTimeout(h.ctx, 350*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recorder failure changed Run termination: %v", err)
	}
}

func TestReplayRecorderAcceptanceTimeoutDoesNotBlockMissionMutation(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	replayRecorderSeed(t, h, r, time.Now().UTC())
	core, logs := observer.New(zap.WarnLevel)
	r.log = zap.New(core)
	admin := replayRecorderSQL(t, h)
	for _, statement := range []string{
		"CREATE FUNCTION delay_replay_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(10); RETURN NEW; END $$",
		"CREATE TRIGGER delay_replay_acceptance BEFORE INSERT ON replay_plans FOR EACH ROW EXECUTE FUNCTION delay_replay_acceptance()",
	} {
		if _, err := admin.ExecContext(h.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Phase: "buy", Generation: "timeout-mission"}
	data := missionData{Plan: Plan{Kind: "spot", ShipID: recoveryShipLapras}}
	started := time.Now()
	if err := r.stageAction(h.ctx, mission, &data, ShipState{}, "buy", "/v1/market/place-order", map[string]int{"quantity": 1}, false); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("replay store blocked mutation staging for %v", elapsed)
	}
	persisted, persistedData := h.mission(recoveryShipLapras)
	if persisted.Generation != mission.Generation || persistedData.Action == nil || persistedData.Action.ID != data.Action.ID {
		t.Fatal("replay timeout altered durable mission/action state")
	}
	r.flushReplay(h.ctx, time.Now().UTC())
	replayRecorderWait(t, func() bool { return logs.FilterMessage("replay record failed").Len() != 0 })
	if elapsed := time.Since(started); elapsed > 7*time.Second {
		t.Fatalf("recorder exceeded its 5 s write timeout: %v", elapsed)
	}
	stats, err := h.db.ServiceStats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, counter := range stats.Counters {
		if counter.Name == "replay_record_errors_total" && counter.Value >= 1 {
			return
		}
	}
	t.Fatal("write timeout did not increment replay_record_errors_total")
}

func TestReplayRecorderAcceptanceDisabledWritesAndPrunesNothing(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	r.opts.ReplayRetentionDays = 0
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Phase: "buy", Generation: "disabled"}
	data := missionData{Plan: Plan{Kind: "spot", ShipID: recoveryShipLapras}}
	if err := r.stageAction(h.ctx, mission, &data, ShipState{}, "buy", "/v1/market/place-order", map[string]int{"quantity": 1}, false); err != nil {
		t.Fatal(err)
	}
	r.flushReplay(h.ctx, at)
	plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	links, err := h.db.ReplayActionLinks(h.ctx, h.account.ID)
	if err != nil || len(plans) != 0 || len(links) != 0 || len(replayRecorderFrames(t, h)) != 0 {
		t.Fatalf("disabled recorder wrote rows: plans=%v links=%v err=%v", plans, links, err)
	}
	if _, err := h.db.RecordReplayFrame(h.ctx, store.ReplayFrame{AccountID: h.account.ID, Generation: "old", Source: "live", ObservedAt: at.Add(-100 * 24 * time.Hour), Datasets: map[string]store.ReplayDataset{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.PruneReplay(h.ctx, 0, 1); err != nil || len(replayRecorderFrames(t, h)) != 1 {
		t.Fatalf("disabled retention pruned existing tape: %v", err)
	}
}

func TestReplayRecorderAcceptanceConfigurationDefaultsAndDisable(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/synthetic")
	t.Setenv("CREDENTIAL_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32)))
	t.Setenv("REPLAY_RETENTION_DAYS", "")
	t.Setenv("REPLAY_MAX_GIB", "")
	cfg, err := config.Load()
	if err != nil || cfg.ReplayRetentionDays != 90 || cfg.ReplayMaxGiB != 20 {
		t.Fatalf("replay defaults: %+v, %v", cfg, err)
	}
	t.Setenv("REPLAY_RETENTION_DAYS", "0")
	cfg, err = config.Load()
	if err != nil || cfg.ReplayRetentionDays != 0 {
		t.Fatalf("zero must disable replay: %+v, %v", cfg, err)
	}
}

func TestReplayRecorderAcceptanceTickProducesRunnerInputs(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	r.opts.Execute = false
	if _, err := h.tick(r); err != nil {
		t.Fatal(err)
	}
	expected := make(map[string]json.RawMessage)
	for _, key := range []string{"_ship_idle", "_marque_pending", "_underwriting_pending", "_competition", "_charter_proceeds", "_route_phase"} {
		if _, exists := r.meta[key]; exists {
			t.Fatalf("real tick polluted persisted metadata with runner input %s", key)
		}
		switch key {
		case "_competition":
			expected[key] = encode(r.competition)
		case "_charter_proceeds":
			expected[key] = encode(r.charterProceedsPence)
		case "_marque_pending":
			expected[key] = encode(decode[marquePending](r.snapshot, key))
		case "_underwriting_pending":
			expected[key] = encode(decode[underwritingPending](r.snapshot, key))
		default:
			expected[key] = bytes.Clone(r.snapshot[key])
			if len(expected[key]) == 0 {
				t.Fatalf("real tick did not publish runner source %s", key)
			}
		}
	}
	r.flushReplay(h.ctx, time.Now().UTC())
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) == 1 })
	frame := replayRecorderFrames(t, h)[0]
	for key, want := range expected {
		var meta DatasetMeta
		dataset := frame.Datasets[key]
		if err := json.Unmarshal(dataset.Meta, &meta); err != nil || meta.Generation != r.generation || !meta.Complete || meta.AttemptedAt.IsZero() || meta.UpdatedAt.IsZero() || meta.Stale || meta.Error != "" || !bytes.Equal(dataset.Data, want) {
			t.Fatalf("real runner input %s absent or changed in frame: %+v, %v", key, dataset, err)
		}
		if _, exists := r.meta[key]; exists {
			t.Fatalf("recording runner input %s polluted persisted metadata", key)
		}
	}
	if _, exists := r.snapshot["_competition"]; exists {
		t.Fatal("competition belongs in the replay frame, not the persisted snapshot")
	}
}

func TestReplayRecorderAcceptanceRunCollectsSSEEventsAcrossDecimatedPasses(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	h.api.setAutoRemit(false, 0)
	stream := interceptEventStream(t, h.api, nil, 2)
	stream.frameFor = func(int32) progressFrame { return progressFrame{} }
	running := startRunner(t, r, h.ctx)
	stream.waitReady(t, 2)
	replayRecorderWait(t, func() bool { return len(replayRecorderFrames(t, h)) >= 1 })
	for _, event := range []progressFrame{
		{id: "replay-sse-one", eventType: "market_tick", data: json.RawMessage(`{"port_id":"quay-port","tick":1}`)},
		{id: "replay-sse-two", eventType: "ship_sailed", data: json.RawMessage(`{"ship_id":"foreign-ship","tick":2}`)},
	} {
		if !stream.emit(h.ctx, event) {
			t.Fatal("could not deliver replay SSE event")
		}
	}
	stream.releaseGates()
	waitEventCursor(t, h.db, h.ctx, "replay-sse-two")
	stream.waitCapture(t, 3)
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	seen := make(map[string]int)
	for _, frame := range replayRecorderFrames(t, h) {
		var events []struct {
			EventID   string          `json:"event_id"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			CreatedAt time.Time       `json:"created_at"`
		}
		data := frame.Datasets["events"].Data
		if len(data) == 0 {
			continue
		}
		if err := json.Unmarshal(data, &events); err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			seen[event.EventID]++
			if event.Type == "" || event.CreatedAt.IsZero() || !json.Valid(event.Payload) {
				t.Fatalf("event envelope lost type, payload or receive time: %+v", event)
			}
		}
	}
	if seen["replay-sse-one"] != 1 || seen["replay-sse-two"] != 1 {
		t.Fatalf("SSE event collection lost or duplicated decimated events: %v", seen)
	}
}

func TestReplayRecorderAcceptanceBoundedSingleWriterCountsDrops(t *testing.T) {
	h := newRecoveryHarness(t)
	r := replayRecorderRunner(t, h)
	at := time.Now().UTC()
	replayRecorderSeed(t, h, r, at)
	admin := replayRecorderSQL(t, h)
	lock, err := admin.Conn(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = lock.ExecContext(context.Background(), "SELECT pg_advisory_unlock_all()")
		_ = lock.Close()
	}()
	if _, err := lock.ExecContext(h.ctx, "SELECT pg_advisory_lock(hashtextextended(current_schema() || ':replay-queue', 0))"); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE FUNCTION block_replay_queue_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(hashtextextended(current_schema() || ':replay-queue', 0)); RETURN NEW; END $$",
		"CREATE TRIGGER block_replay_queue_acceptance BEFORE INSERT ON replay_frames FOR EACH ROW EXECUTE FUNCTION block_replay_queue_acceptance()",
	} {
		if _, err := admin.ExecContext(h.ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	r.flushReplay(h.ctx, at)
	waitingWriters := func() int {
		var count int
		if err := admin.QueryRowContext(h.ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event = 'advisory' AND query LIKE '%replay_frames%'").Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	replayRecorderWait(t, func() bool { return waitingWriters() >= 1 })
	started := time.Now()
	for i := range 128 {
		observedAt := at.Add(time.Duration(i+1) * time.Minute)
		replayRecorderNextGeneration(r, observedAt.Format(time.RFC3339Nano), observedAt)
		r.flushReplay(h.ctx, observedAt)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("full replay queue blocked the pass for %v", elapsed)
	}
	if count := waitingWriters(); count != 1 {
		t.Fatalf("recorder has %d writes in flight, want one", count)
	}
	stats, err := h.db.ServiceStats(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	drops := float64(0)
	for _, counter := range stats.Counters {
		if counter.Name == "replay_record_errors_total" {
			drops = counter.Value
		}
	}
	if drops == 0 {
		t.Fatal("saturated bounded queue did not count dropped interval frames")
	}
	if _, err := lock.ExecContext(h.ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRecorderAcceptanceStartupImportsPersistedMissionAndAction(t *testing.T) {
	h := newRecoveryHarness(t)
	old := h.runner(primaryTestKey)
	old.opts.ReplayRetentionDays = 0
	old.generation = "pre-deploy-pass"
	mission := &store.Mission{AccountID: h.account.ID, ShipID: recoveryShipLapras, Phase: "buy", Generation: "pre-deploy-mission"}
	data := missionData{Sequence: 4, Plan: Plan{Kind: "spot", ShipID: recoveryShipLapras, Economics: Economics{ExpectedNetPence: 1234}}}
	if err := old.stageAction(h.ctx, mission, &data, ShipState{}, "buy", "/v1/market/place-order", map[string]int{"quantity": 1}, false); err != nil {
		t.Fatal(err)
	}
	restarted := replayRecorderRunner(t, h)
	restarted.opts.Execute = false
	stream := interceptEventStream(t, h.api, nil, 2)
	stream.frameFor = func(int32) progressFrame { return progressFrame{} }
	running := startRunner(t, restarted, h.ctx)
	stream.waitReady(t, 2)
	replayRecorderWait(t, func() bool {
		plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
		links, linkErr := h.db.ReplayActionLinks(h.ctx, h.account.ID)
		return err == nil && linkErr == nil && len(plans) == 1 && len(links) == 1
	})
	running.cancel()
	stream.releaseGates()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	plans, err := h.db.ReplayPlans(h.ctx, h.account.ID)
	if err != nil || plans[0].MissionID != mission.Generation || plans[0].ShipID != mission.ShipID {
		t.Fatalf("startup did not import loaded mission plan: %+v, %v", plans, err)
	}
	links, err := h.db.ReplayActionLinks(h.ctx, h.account.ID)
	if err != nil || links[0].ActionID != data.Action.ID || links[0].MissionID != mission.Generation {
		t.Fatalf("startup did not import persisted action linkage: %+v, %v", links, err)
	}
}
