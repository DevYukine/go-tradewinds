package strategy

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/DevYukine/go-tradewinds/internal/bot"
)

// schemeState holds shared coordination state for the feeder/harvester bailout
// exploitation scheme. All strategies share one process, so package-level state
// with atomic operations is sufficient — no DB needed.
type schemeState struct {
	mu          sync.RWMutex
	portOrder   []uuid.UUID
	initialized bool

	currentIndex atomic.Int32
	stocked      atomic.Bool
}

var scheme schemeState

// initScheme builds the sorted port rotation from the world cache. Idempotent.
func initScheme(world *bot.WorldCache) {
	scheme.mu.Lock()
	defer scheme.mu.Unlock()

	if scheme.initialized {
		return
	}

	ids := world.PortIDs()
	// Deterministic ordering so all strategies agree on rotation.
	sort.Slice(ids, func(i, j int) bool {
		return ids[i].String() < ids[j].String()
	})

	scheme.portOrder = ids
	scheme.initialized = true
}

// schemeTargetPort returns the current target port for the scheme.
func schemeTargetPort() uuid.UUID {
	scheme.mu.RLock()
	defer scheme.mu.RUnlock()

	if len(scheme.portOrder) == 0 {
		return uuid.Nil
	}
	idx := int(scheme.currentIndex.Load()) % len(scheme.portOrder)
	return scheme.portOrder[idx]
}

// schemeNextPort returns the next target port (for harvester pre-positioning).
func schemeNextPort() uuid.UUID {
	scheme.mu.RLock()
	defer scheme.mu.RUnlock()

	if len(scheme.portOrder) == 0 {
		return uuid.Nil
	}
	idx := (int(scheme.currentIndex.Load()) + 1) % len(scheme.portOrder)
	return scheme.portOrder[idx]
}

// schemeAdvancePort atomically advances to the next port in the rotation.
// Resets stocked flag. Returns the new target port.
func schemeAdvancePort() uuid.UUID {
	scheme.mu.RLock()
	n := len(scheme.portOrder)
	scheme.mu.RUnlock()

	if n == 0 {
		return uuid.Nil
	}

	for {
		old := scheme.currentIndex.Load()
		next := (old + 1) % int32(n)
		if scheme.currentIndex.CompareAndSwap(old, next) {
			scheme.stocked.Store(false)
			scheme.mu.RLock()
			port := scheme.portOrder[next]
			scheme.mu.RUnlock()
			return port
		}
	}
}

// schemeSetStocked sets whether the harvester has enough goods stocked at the
// target port.
func schemeSetStocked(stocked bool) {
	scheme.stocked.Store(stocked)
}

// schemeIsStocked returns true when the harvester has signaled that it has
// enough goods at the target port for feeders to start posting P2P orders.
func schemeIsStocked() bool {
	return scheme.stocked.Load()
}
