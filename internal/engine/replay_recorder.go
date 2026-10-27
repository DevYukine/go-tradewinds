package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const (
	replayEventLimit    = store.MaxReplayEventCount
	replayEventBytes    = store.MaxReplayEventBytes
	replayEvidenceLimit = 256
)

var replayPassRunnerKeys = []string{
	"_ship_idle", "_book_observed", "_marque_pending", "_underwriting_pending", "_competition", "_charter_proceeds", "_route_phase",
}

var replaySweepRunnerKeys = []string{
	"_capacity_savings_target", "_capacity_savings_expired", "_capacity_growth_port", "_expansion_agents",
	"_sourcing_probe_history", "_growth_build_backoffs", "_capacity_financing_backoffs", "_capacity_growth_paused_until", "_ship_losses",
}

var (
	errReplayQueueFull     = errors.New("replay frame queue full")
	errReplayEvidenceFull  = errors.New("replay evidence buffer full")
	errReplayWriterPanic   = errors.New("replay writer panicked")
	errReplayWriterStopped = errors.New("replay writer is unavailable")
	errReplayShutdownDrop  = errors.New("replay shutdown dropped queued evidence")
	replayRevision         = sync.OnceValue(func() string {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					return setting.Value
				}
			}
		}
		return "unavailable"
	})
)

type replayCapture struct {
	frame         store.ReplayFrame
	events        []store.ReplayEvent
	droppedEvents int64
	droppedBytes  int64
	recordedAt    time.Time
	competition   *store.CompetitionSummary
}

type replayRecorder struct {
	mu            sync.Mutex
	frames        chan replayCapture
	wake          chan struct{}
	stop          chan struct{}
	done          chan struct{}
	plans         []store.ReplayPlan
	links         []store.ReplayActionLink
	causes        map[string]bool
	planDigests   map[string][sha256.Size]byte
	linked        map[string]bool
	lastAt        time.Time
	lastSweep     time.Time
	decision      bool
	cancel        context.CancelFunc
	postDecision  bool
	events        []store.ReplayEvent
	eventBytes    int
	droppedEvents int64
	droppedBytes  int64
	stopped       bool
	importing     bool
}

func (r *Runner) startReplay(ctx context.Context) *replayRecorder {
	if r.opts.ReplayRetentionDays == 0 || r.db == nil {
		return nil
	}
	if r.replay != nil {
		return r.replay
	}
	recorder := &replayRecorder{
		frames: make(chan replayCapture, 4), wake: make(chan struct{}, 1),
		stop: make(chan struct{}), done: make(chan struct{}), causes: make(map[string]bool),
		planDigests: make(map[string][sha256.Size]byte), linked: make(map[string]bool),
	}
	r.replay = recorder
	writerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	recorder.cancel = cancel
	go r.writeReplay(writerCtx, recorder)
	return recorder
}
func (r *Runner) replayError(recorder *replayRecorder, err error) {
	r.db.CountReplayRecordError(r.account.ID)
	cause := err.Error()
	recorder.mu.Lock()
	first := !recorder.causes[cause]
	recorder.causes[cause] = true
	recorder.mu.Unlock()
	if first {
		// Store errors expose safe classifications, not driver SQL or payloads.
		r.log.Warn("replay record failed", zap.String("cause", cause))
	}
}

func (r *Runner) writeReplay(writerCtx context.Context, recorder *replayRecorder) {
	defer close(recorder.done)
	defer recorder.cancel()
	defer func() {
		if recover() != nil {
			r.replayError(recorder, errReplayWriterPanic)
			r.discardReplay(recorder)
		}
	}()
	write := func(operation func(context.Context) error) (err error) {
		writeCtx, cancel := context.WithTimeout(writerCtx, 5*time.Second)
		defer cancel()
		defer func() {
			if recover() != nil {
				err = errReplayWriterPanic
			}
			if err != nil {
				r.replayError(recorder, err)
			}
		}()
		if writerCtx.Err() != nil {
			return errReplayShutdownDrop
		}
		return operation(writeCtx)
	}
	flushPlans := func() {
		recorder.mu.Lock()
		plans, links := recorder.plans, recorder.links
		recorder.plans, recorder.links = nil, nil
		recorder.mu.Unlock()
		for _, plan := range plans {
			err := write(func(ctx context.Context) error { return r.db.RecordReplayPlan(ctx, plan) })
			if err != nil {
				recorder.mu.Lock()
				if recorder.planDigests[plan.MissionID] == store.ReplayPlanFingerprint(plan) {
					delete(recorder.planDigests, plan.MissionID)
				}
				recorder.mu.Unlock()
			}
		}
		for _, link := range links {
			if err := write(func(ctx context.Context) error { return r.db.LinkReplayAction(ctx, link) }); err != nil {
				recorder.mu.Lock()
				delete(recorder.linked, link.ActionID)
				recorder.mu.Unlock()
			}
		}
	}
	writeFrame := func(capture replayCapture) {
		_ = write(func(ctx context.Context) error {
			if capture.competition != nil {
				dataset := capture.frame.Datasets["_competition"]
				var err error
				dataset.Data, err = json.Marshal(capture.competition)
				if err != nil {
					return err
				}
				capture.frame.Datasets["_competition"] = dataset
			}
			events := capture.events
			if events == nil {
				events = make([]store.ReplayEvent, 0)
			}
			payload, err := json.Marshal(events)
			if err != nil {
				return err
			}
			meta := DatasetMeta{Generation: capture.frame.Generation, AttemptedAt: capture.recordedAt,
				UpdatedAt: capture.recordedAt, Complete: capture.droppedEvents == 0}
			if capture.droppedEvents != 0 {
				meta.Stale = true
				meta.Error = "replay event buffer overflow"
			}
			capture.frame.Datasets["events"] = store.ReplayDataset{Data: payload, Meta: encode(struct {
				DatasetMeta
				DroppedEvents int64 `json:"dropped_events,omitempty"`
				DroppedBytes  int64 `json:"dropped_bytes,omitempty"`
			}{meta, capture.droppedEvents, capture.droppedBytes})}
			_, err = r.db.RecordReplayFrame(ctx, capture.frame)
			return err
		})
	}
	drain := func() {
		if writerCtx.Err() != nil {
			r.discardReplay(recorder)
			return
		}
		flushPlans()
		for {
			if writerCtx.Err() != nil {
				r.discardReplay(recorder)
				return
			}
			select {
			case capture := <-recorder.frames:
				writeFrame(capture)
			default:
				return
			}
		}
	}
	for {
		select {
		case <-recorder.stop:
			drain()
			return
		case <-recorder.wake:
			flushPlans()
		case capture := <-recorder.frames:
			flushPlans()
			writeFrame(capture)
		}
	}
}

func (r *Runner) stopReplay(ctx context.Context) {
	recorder := r.replay
	if recorder == nil || recorder.stopped {
		return
	}
	if len(recorder.events) != 0 || recorder.droppedEvents != 0 {
		recorder.lastAt = time.Time{}
		r.flushReplay(context.WithoutCancel(ctx), time.Now().UTC())
		if len(recorder.events) != 0 || recorder.droppedEvents != 0 {
			r.replayError(recorder, errReplayShutdownDrop)
			recorder.events = nil
			recorder.eventBytes, recorder.droppedEvents, recorder.droppedBytes = 0, 0, 0
		}
	}
	recorder.stopped = true
	close(recorder.stop)
	select {
	case <-recorder.done:
	case <-time.After(5 * time.Second):
		recorder.cancel()
		r.replayError(recorder, errors.New("replay shutdown drain timed out"))
		r.discardReplay(recorder)
	}
}

func (r *Runner) discardReplay(recorder *replayRecorder) {
	recorder.mu.Lock()
	dropped := len(recorder.plans) + len(recorder.links)
	for _, plan := range recorder.plans {
		if recorder.planDigests[plan.MissionID] == store.ReplayPlanFingerprint(plan) {
			delete(recorder.planDigests, plan.MissionID)
		}
	}
	for _, link := range recorder.links {
		delete(recorder.linked, link.ActionID)
	}
	recorder.plans, recorder.links = nil, nil
	recorder.mu.Unlock()
	for {
		select {
		case <-recorder.frames:
			dropped++
		default:
			for range dropped {
				r.replayError(recorder, errReplayShutdownDrop)
			}
			return
		}
	}
}

func (r *Runner) replayPlan(ctx context.Context, mission *store.Mission, data *missionData) {
	recorder := r.startReplay(ctx)
	if recorder == nil || recorder.stopped {
		return
	}
	select {
	case <-recorder.done:
		r.replayError(recorder, errReplayWriterStopped)
		return
	default:
	}
	generation, at := r.generation, time.Now().UTC()
	if recorder.importing {
		generation, at = mission.Generation, mission.UpdatedAt
	}
	entry := store.ReplayPlan{AccountID: r.account.ID, MissionID: mission.Generation, ShipID: mission.ShipID,
		Phase: mission.Phase, Sequence: data.Sequence, Generation: generation, ObservedAt: at,
		Plan: encode(data.Plan), Policy: encode(r.opts), Revision: replayRevision()}
	digest := store.ReplayPlanFingerprint(entry)
	recorder.mu.Lock()
	previous, exists := recorder.planDigests[mission.Generation]
	if exists && previous == digest {
		recorder.mu.Unlock()
		return
	}
	if !recorder.importing {
		recorder.decision = true
	}
	if len(recorder.plans) >= replayEvidenceLimit {
		recorder.mu.Unlock()
		r.replayError(recorder, errReplayEvidenceFull)
		return
	}
	recorder.planDigests[mission.Generation] = digest
	recorder.plans = append(recorder.plans, entry)
	recorder.mu.Unlock()
	select {
	case recorder.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) replayLink(ctx context.Context, mission *store.Mission, data *missionData) {
	recorder := r.startReplay(ctx)
	if recorder == nil || recorder.stopped || data.Action == nil {
		return
	}
	select {
	case <-recorder.done:
		r.replayError(recorder, errReplayWriterStopped)
		return
	default:
	}
	generation := r.generation
	if recorder.importing {
		generation = mission.Generation
	}
	entry := store.ReplayActionLink{AccountID: r.account.ID, ActionID: data.Action.ID,
		MissionID: mission.Generation, Stage: data.Action.Stage, Sequence: data.Sequence, Generation: generation}
	recorder.mu.Lock()
	if recorder.linked[data.Action.ID] {
		recorder.mu.Unlock()
		return
	}
	if !recorder.importing {
		recorder.decision = true
	}
	if len(recorder.links) >= replayEvidenceLimit {
		recorder.mu.Unlock()
		r.replayError(recorder, errReplayEvidenceFull)
		return
	}
	recorder.linked[data.Action.ID] = true
	recorder.links = append(recorder.links, entry)
	recorder.mu.Unlock()
	select {
	case recorder.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) replayEvent(ctx context.Context, eventID, kind string, payload json.RawMessage) {
	if recorder := r.startReplay(ctx); recorder != nil && !recorder.stopped {
		payload = store.BoundEventPayload(payload)
		size := len(payload) + len(eventID) + len(kind) + 64
		if len(recorder.events) >= replayEventLimit || size > replayEventBytes-recorder.eventBytes {
			recorder.droppedEvents++
			recorder.droppedBytes += int64(size)
			return
		}
		recorder.events = append(recorder.events, store.ReplayEvent{
			EventID: eventID, Type: kind, Payload: payload, CreatedAt: time.Now().UTC(),
		})
		recorder.eventBytes += size
	}
}

func (r *Runner) flushReplay(ctx context.Context, at time.Time) {
	recorder := r.startReplay(ctx)
	if recorder == nil || recorder.stopped {
		return
	}
	decision := recorder.decision
	postDecision := recorder.postDecision
	if !recorder.lastAt.IsZero() && !decision && !postDecision && at.Sub(recorder.lastAt) < time.Minute && !(ctx.Err() != nil && len(recorder.events) != 0) {
		return
	}
	select {
	case <-recorder.done:
		r.replayError(recorder, errReplayWriterStopped)
		return
	default:
	}
	if len(recorder.frames) == cap(recorder.frames) {
		r.replayError(recorder, errReplayQueueFull)
		return
	}
	sweep := recorder.lastSweep.IsZero() || decision || at.Sub(recorder.lastSweep) >= 5*time.Minute
	observedAt := r.passStart
	if observedAt.IsZero() {
		observedAt = at
	}
	frame := store.ReplayFrame{AccountID: r.account.ID, Generation: r.generation, Source: "live", ObservedAt: observedAt,
		Decision: decision, Clock: r.snapshot["clock"], Phase: decode[Clock](r.snapshot, "clock").UnlockedPhase,
		Datasets: make(map[string]store.ReplayDataset)}
	add := func(key string, data json.RawMessage, cached bool) {
		meta, exists := r.meta[key]
		if !exists {
			return
		}
		failedThisPass := meta.Error != "" && !meta.AttemptedAt.Before(r.passStart)
		if !cached && meta.Generation != r.generation && !failedThisPass {
			return
		}
		if meta.Error != "" || !meta.Complete || meta.Stale {
			if !failedThisPass {
				return
			}
			data = nil
		}
		frame.Datasets[key] = store.ReplayDataset{Data: data, Meta: encode(meta)}
	}
	for _, key := range []string{"clock", "catalog", "zones", "profile", "settings", "vaults", "loans", "rates", "ships", "warehouses", "orders", "fills", "factors", "monopolies", "marques", "contracts", "expansion"} {
		add(key, r.snapshot[key], false)
	}
	for _, group := range []string{"ship_details", "books", "ports", "routes", "yard_quotes", "inventory"} {
		if group == "routes" && !sweep {
			continue
		}
		var values map[string]json.RawMessage
		if group == "ports" && !sweep {
			values = r.replayPorts
		} else {
			values = decode[map[string]json.RawMessage](r.snapshot, group)
			if group == "ports" {
				r.replayPorts = values
			}
		}
		for key, meta := range r.meta {
			child, matches := strings.CutPrefix(key, group+"/")
			if !matches {
				continue
			}
			if group == "ports" && !sweep && r.stateCache.Ports[child].Visibility != "quay" && meta.Error == "" {
				continue
			}
			if group == "yard_quotes" {
				port, _, _ := strings.Cut(child, "|")
				if r.stateCache.Ports[port].Visibility != "quay" {
					continue
				}
			}
			add(key, values[child], sweep && (group == "ports" || group == "routes"))
		}
	}
	addRunnerKeys := func(keys []string) {
		for _, key := range keys {
			meta := DatasetMeta{Generation: r.generation, AttemptedAt: observedAt, UpdatedAt: at, Complete: true}
			data := r.snapshot[key]
			switch key {
			case "_competition":
				data = nil
				if r.competition == nil {
					meta.Complete, meta.Stale, meta.Error = false, true, "competition evidence unavailable"
				}
			case "_charter_proceeds":
				data = encode(r.charterProceedsPence)
			case "_marque_pending":
				if len(data) == 0 {
					data = encode(marquePending{})
				}
			case "_underwriting_pending":
				if len(data) == 0 {
					data = encode(underwritingPending{})
				}
			}
			if len(data) != 0 || key == "_competition" {
				frame.Datasets[key] = store.ReplayDataset{Data: data, Meta: encode(meta)}
			}
		}
	}
	addRunnerKeys(replayPassRunnerKeys)
	if sweep {
		addRunnerKeys(replaySweepRunnerKeys)
	}
	capture := replayCapture{frame: frame, events: recorder.events, droppedEvents: recorder.droppedEvents,
		droppedBytes: recorder.droppedBytes, recordedAt: at, competition: r.competition}
	select {
	case recorder.frames <- capture:
		recorder.lastAt = at
		if sweep {
			recorder.lastSweep = at
		}
		recorder.decision = false
		recorder.postDecision = decision
		recorder.events = nil
		recorder.eventBytes, recorder.droppedEvents, recorder.droppedBytes = 0, 0, 0
	default:
		// Retain decisions, links, plans and events in memory for the next
		// opportunity; an interval never waits for a database connection.
		r.replayError(recorder, errReplayQueueFull)
	}
}
