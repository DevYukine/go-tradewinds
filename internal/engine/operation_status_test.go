package engine

// Failure-first acceptance contract for L1: the runner publishes exactly one
// startup fleet snapshot per process, later successful refresh observations of
// an unchanged fleet stay completely quiet, and only fresh authoritative
// transitions between observations produce one labelled entry each — ordinary
// sailing-to-port arrivals at INFO, permanent unexpected hull loss at WARN as
// an actionable asset-loss event. Stale or missing observations and startup
// history never invent arrivals or losses, an incoherent first observation
// never consumes the startup snapshot, and a passed ETA on a still-sailing
// ship is never an arrival.

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// operationShip builds one fleet observation row with the identity, status and
// authoritative port the transition tests pivot on.
func operationShip(id, name, status, port string) ShipState {
	state := ShipState{}
	state.Ship.ID = id
	state.Ship.Name = name
	state.Ship.Status = status
	state.Ship.CurrentPortID = port
	return state
}

// operationObservedAt publishes one fresh complete ships observation so the
// next logStatus call sees a coherent authoritative fleet.
func operationObservedAt(runner *Runner, at time.Time) {
	runner.meta["ships"] = DatasetMeta{
		Generation:  runner.generation,
		AttemptedAt: at,
		UpdatedAt:   at,
		Complete:    true,
	}
}

func acceptanceFleetStatus(t *testing.T, logs *observer.ObservedLogs) []observer.LoggedEntry {
	t.Helper()
	entries := logs.FilterMessage("fleet status").All()
	for _, entry := range entries {
		if entry.Level != zap.InfoLevel {
			t.Fatalf("fleet status must stay at INFO: %+v", entry)
		}
	}
	return entries
}

// acceptanceEvents reads one stable transition announcement and enforces its
// level; the message strings are the pinned stable event names.
func acceptanceEvents(t *testing.T, logs *observer.ObservedLogs, message string) []observer.LoggedEntry {
	t.Helper()
	entries := logs.FilterMessage(message).All()
	for _, entry := range entries {
		if entry.Level != zap.InfoLevel {
			t.Fatalf("%s must stay at INFO: %+v", message, entry)
		}
	}
	return entries
}

// acceptanceLossEvents reads the pinned permanent-loss announcement and
// enforces its actionable WARN severity: an unexpected hull loss demands
// operator attention, unlike an ordinary arrival.
func acceptanceLossEvents(t *testing.T, logs *observer.ObservedLogs) []observer.LoggedEntry {
	t.Helper()
	entries := logs.FilterMessage("ship sunk").All()
	for _, entry := range entries {
		if entry.Level != zap.WarnLevel {
			t.Fatalf("ship sunk must stay at WARN: %+v", entry)
		}
	}
	return entries
}

func acceptanceFields(t *testing.T, entry observer.LoggedEntry, keys ...string) map[string]any {
	t.Helper()
	fields := entry.ContextMap()
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Fatalf("%s entry missing %s: %v", entry.Message, key, fields)
		}
	}
	return fields
}

func TestOperationStatusStartupSnapshotIsSingleAcrossRefreshObservations(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "operation-status-generation"
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
		operationShip(recoveryShipCharizard, "Charizard", "sailing", recoveryFarPort),
	}
	start := time.Now().UTC().Truncate(time.Second)
	operationObservedAt(runner, start)
	runner.logStatus(h.ctx, start)

	statuses := acceptanceFleetStatus(t, logs)
	if len(statuses) != 1 {
		t.Fatalf("the first coherent observation must emit exactly one fleet snapshot: %+v", statuses)
	}
	fields := acceptanceFields(t, statuses[0], "mode", "reconciled_at", "fleet_fresh", "fleet_observed_at")
	if fields["mode"] != "execute" {
		t.Fatalf("status mode = %v, want the configured execution mode", fields["mode"])
	}
	if fields["fleet_fresh"] != true {
		t.Fatalf("a coherent startup observation must be marked fresh: %v", fields["fleet_fresh"])
	}
	if reconciled, ok := fields["reconciled_at"].(time.Time); !ok || !reconciled.Equal(start) {
		t.Fatalf("reconciled_at = %v, want the emitting observation %v", fields["reconciled_at"], start)
	}

	// Two further successful minute-separated refresh observations of the
	// unchanged fleet must stay completely quiet: the startup snapshot is the
	// one fleet announcement per runner start, not a periodic heartbeat.
	for _, offset := range []time.Duration{70 * time.Second, 140 * time.Second} {
		observed := start.Add(offset)
		operationObservedAt(runner, observed)
		runner.logStatus(h.ctx, observed)
	}
	if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 1 {
		t.Fatalf("repeated successful refresh observations must stay silent after the startup snapshot: %+v", statuses)
	}
	if infos := logs.FilterLevelExact(zap.InfoLevel); infos.Len() != 1 {
		t.Fatalf("quiet refreshes must not log anything at INFO: %+v", infos.All())
	}
}

func TestOperationStatusIncoherentFirstFleetDoesNotConsumeStartupSnapshot(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "operation-coherence-generation"
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}

	// A stale first observation is not a coherent persisted fleet: it must
	// not publish, and must not spend the single startup announcement.
	staleAt := time.Now().UTC().Truncate(time.Second)
	operationObservedAt(runner, staleAt)
	meta := runner.meta["ships"]
	meta.Stale = true
	runner.meta["ships"] = meta
	runner.logStatus(h.ctx, staleAt)
	if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 0 {
		t.Fatalf("a stale first observation must not publish the startup snapshot: %+v", statuses)
	}

	// A missing ships dataset likewise consumes nothing.
	runner.stateCache.Ships = nil
	delete(runner.meta, "ships")
	runner.logStatus(h.ctx, staleAt.Add(70*time.Second))
	if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 0 {
		t.Fatalf("a missing first fleet must not publish the startup snapshot: %+v", statuses)
	}

	// The first coherent observation still owns the one startup snapshot.
	coherent := staleAt.Add(140 * time.Second)
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	operationObservedAt(runner, coherent)
	runner.logStatus(h.ctx, coherent)
	statuses := acceptanceFleetStatus(t, logs)
	if len(statuses) != 1 {
		t.Fatalf("the first coherent observation must still publish exactly one startup snapshot: %+v", statuses)
	}
	fields := acceptanceFields(t, statuses[0], "reconciled_at", "fleet_fresh")
	if fields["fleet_fresh"] != true {
		t.Fatalf("the coherent snapshot must be marked fresh: %v", fields["fleet_fresh"])
	}
	if reconciled, ok := fields["reconciled_at"].(time.Time); !ok || !reconciled.Equal(coherent) {
		t.Fatalf("reconciled_at = %v, want the coherent observation %v", fields["reconciled_at"], coherent)
	}

	// Later refreshes of the unchanged fleet stay silent as always.
	later := coherent.Add(70 * time.Second)
	operationObservedAt(runner, later)
	runner.logStatus(h.ctx, later)
	if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 1 {
		t.Fatalf("refreshes after the coherent snapshot must stay silent: %+v", statuses)
	}
}

func TestOperationStatusSailingToPortArrivalLogsOnceWithContext(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "operation-arrival-generation"
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipCharizard, "Charizard", "sailing", recoveryFarPort),
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	start := time.Now().UTC().Truncate(time.Second)
	operationObservedAt(runner, start)
	runner.logStatus(h.ctx, start)
	if arrivals := acceptanceEvents(t, logs, "ship arrived"); len(arrivals) != 0 {
		t.Fatalf("startup history must not announce arrivals for under-way or already-berthed ships: %+v", arrivals)
	}

	// The next fresh authoritative observation shows Charizard completed its
	// voyage into the home port: exactly one arrival entry with ship and port
	// context, and no revival of the fleet heartbeat.
	arrival := start.Add(70 * time.Second)
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipCharizard, "Charizard", "berthed", recoveryHomePort),
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	operationObservedAt(runner, arrival)
	runner.logStatus(h.ctx, arrival)

	arrivals := acceptanceEvents(t, logs, "ship arrived")
	if len(arrivals) != 1 || arrivals[0].Level != zap.InfoLevel {
		t.Fatalf("a fresh sailing-to-port transition must announce exactly once at INFO: %+v", arrivals)
	}
	fields := acceptanceFields(t, arrivals[0], "ship_id", "port_id")
	if fields["ship_id"] != recoveryShipCharizard || fields["port_id"] != recoveryHomePort {
		t.Fatalf("arrival context = %v, want ship %s at port %s", fields, recoveryShipCharizard, recoveryHomePort)
	}
	if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 1 {
		t.Fatalf("an arrival transition must not revive the periodic fleet status: %+v", statuses)
	}

	// Later fresh observations of the same arrival state stay silent.
	settled := arrival.Add(70 * time.Second)
	operationObservedAt(runner, settled)
	runner.logStatus(h.ctx, settled)
	if arrivals := acceptanceEvents(t, logs, "ship arrived"); len(arrivals) != 1 {
		t.Fatalf("the same arrival must never be announced twice: %+v", arrivals)
	}
}

func TestOperationStatusNeverInventsArrivalsFromStaleMissingOrStartupStates(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "operation-invention-generation"

	t.Run("startup terminal and berthed history claims nothing", func(t *testing.T) {
		runner.stateCache.Ships = []ShipState{
			operationShip(recoveryShipCharizard, "Charizard", "sailing", recoveryFarPort),
			operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
			operationShip(recoveryShipBlastoise, "Blastoise", "sunk", ""),
		}
		start := time.Now().UTC().Truncate(time.Second)
		operationObservedAt(runner, start)
		runner.logStatus(h.ctx, start)
		if statuses := acceptanceFleetStatus(t, logs); len(statuses) != 1 {
			t.Fatalf("startup must emit only the one fleet snapshot: %+v", statuses)
		}
		for _, check := range []struct {
			name   string
			events []observer.LoggedEntry
		}{
			{"arrivals", acceptanceEvents(t, logs, "ship arrived")},
			{"losses", acceptanceLossEvents(t, logs)},
		} {
			if len(check.events) != 0 {
				t.Fatalf("startup history invented %s events: %+v", check.name, check.events)
			}
		}
	})

	t.Run("stale observation of changed statuses claims nothing", func(t *testing.T) {
		stale := time.Now().UTC().Truncate(time.Second)
		runner.stateCache.Ships = []ShipState{
			operationShip(recoveryShipCharizard, "Charizard", "berthed", recoveryHomePort),
			operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
			operationShip(recoveryShipBlastoise, "Blastoise", "sunk", ""),
		}
		operationObservedAt(runner, stale)
		meta := runner.meta["ships"]
		meta.Stale = true
		runner.meta["ships"] = meta
		runner.logStatus(h.ctx, stale.Add(70*time.Second))
		for _, check := range []struct {
			name   string
			events []observer.LoggedEntry
		}{
			{"arrivals", acceptanceEvents(t, logs, "ship arrived")},
			{"losses", acceptanceLossEvents(t, logs)},
		} {
			if len(check.events) != 0 {
				t.Fatalf("a stale observation invented %s events: %+v", check.name, check.events)
			}
		}
	})

	t.Run("missing ships dataset claims nothing", func(t *testing.T) {
		missing := time.Now().UTC().Truncate(time.Second)
		runner.stateCache.Ships = nil
		delete(runner.meta, "ships")
		runner.logStatus(h.ctx, missing.Add(140*time.Second))
		for _, check := range []struct {
			name   string
			events []observer.LoggedEntry
		}{
			{"arrivals", acceptanceEvents(t, logs, "ship arrived")},
			{"losses", acceptanceLossEvents(t, logs)},
		} {
			if len(check.events) != 0 {
				t.Fatalf("a missing dataset invented %s events: %+v", check.name, check.events)
			}
		}
	})

	t.Run("a passed ETA on an unchanged sailing ship is not an arrival", func(t *testing.T) {
		quiet := newRecoveryHarness(t)
		idle := quiet.runner(primaryTestKey)
		quietCore, quietLogs := observer.New(zap.DebugLevel)
		idle.log = zap.New(quietCore)
		idle.generation = "operation-eta-generation"
		underWay := operationShip(recoveryShipCharizard, "Charizard", "sailing", recoveryFarPort)
		eta := struct {
			EstimatedArrivalAt time.Time `json:"estimated_arrival_at"`
		}{EstimatedArrivalAt: time.Now().UTC().Add(-time.Minute)}
		underWay.Voyage = &eta
		idle.stateCache.Ships = []ShipState{underWay}
		start := time.Now().UTC().Truncate(time.Second)
		operationObservedAt(idle, start)
		idle.logStatus(quiet.ctx, start)
		later := start.Add(70 * time.Second)
		operationObservedAt(idle, later)
		idle.logStatus(quiet.ctx, later)
		if arrivals := acceptanceEvents(t, quietLogs, "ship arrived"); len(arrivals) != 0 {
			t.Fatalf("a passed ETA must never announce an arrival while the ship still sails: %+v", arrivals)
		}
	})
}

func TestOperationStatusSunkTransitionLogsOnceWithContext(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = "operation-loss-generation"
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipBlastoise, "Blastoise", "sailing", recoveryFarPort),
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	start := time.Now().UTC().Truncate(time.Second)
	operationObservedAt(runner, start)
	runner.logStatus(h.ctx, start)
	if losses := acceptanceLossEvents(t, logs); len(losses) != 0 {
		t.Fatalf("a sailing hull at startup must not be announced lost: %+v", losses)
	}

	// The next fresh authoritative observation shows Blastoise was lost.
	loss := start.Add(70 * time.Second)
	runner.stateCache.Ships = []ShipState{
		operationShip(recoveryShipBlastoise, "Blastoise", "sunk", ""),
		operationShip(recoveryShipLapras, "Lapras", "berthed", recoveryHomePort),
	}
	operationObservedAt(runner, loss)
	runner.logStatus(h.ctx, loss)

	losses := acceptanceLossEvents(t, logs)
	if len(losses) != 1 {
		t.Fatalf("a fresh sunk transition must announce exactly once at WARN: %+v", losses)
	}
	fields := acceptanceFields(t, losses[0], "ship_id")
	if fields["ship_id"] != recoveryShipBlastoise {
		t.Fatalf("sunk context = %v, want the lost hull %s", fields, recoveryShipBlastoise)
	}

	after := loss.Add(70 * time.Second)
	operationObservedAt(runner, after)
	runner.logStatus(h.ctx, after)
	if losses := acceptanceLossEvents(t, logs); len(losses) != 1 {
		t.Fatalf("the same loss must never be announced twice: %+v", losses)
	}
}
