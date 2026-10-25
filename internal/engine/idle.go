package engine

import (
	"math"
	"time"
)

const sourcingIdleGameHours int64 = 6
const sourcingReinspectionCooldown = 24 * time.Minute

// Only successive coherent REST observations earn game hours. A gap rebases the
// next observation without erasing hours already earned at the same port.
type shipIdleState struct {
	LastValidatedGameHour int64     `json:"last_validated_game_hour"`
	PortID                string    `json:"port_id"`
	EarnedGameHours       int64     `json:"earned_game_hours"`
	LastClockUTC          time.Time `json:"last_clock_utc"`
	Validated             bool      `json:"validated"`
	ClockRegressed        bool      `json:"clock_regressed,omitempty"`
}

func idleClockReady(in PlanningInput) bool {
	return in.fresh("ships") && in.fresh("contracts") && in.fresh("clock") &&
		!in.Clock.Paused && in.Clock.Calendar.TimeScale > 0 && in.Clock.Calendar.TotalGameHours >= 0
}

func idleFreight(shipID string, contracts []Contract) bool {
	for _, contract := range contracts {
		if contract.ContractType == "freight" && contract.Status == "active" && contract.CarrierShipID == shipID {
			return true
		}
	}
	return false
}

func (r *Runner) recordShipIdle(state accountState, previous PlanningInput) {
	idle := decode[map[string]shipIdleState](r.snapshot, "_ship_idle")
	if idle == nil {
		idle = make(map[string]shipIdleState)
	}
	in := r.planningInputFrom(ShipState{}, state)
	coherent := idleClockReady(in)
	previous.Now = in.Now
	previousFresh := idleClockReady(previous)
	shipsFresh := in.fresh("ships")
	contractsFresh := in.fresh("contracts")
	if !coherent {
		for id, observed := range idle {
			observed.Validated = false
			observed.ClockRegressed = false
			idle[id] = observed
		}
	}
	seen := make(map[string]bool, len(state.Ships))
	for _, ship := range state.Ships {
		id := ship.Ship.ID
		seen[id] = true
		if !shipsFresh || id == "" {
			continue
		}
		if ship.Ship.Status == "sunk" {
			delete(idle, id)
			continue
		}
		observed := idle[id]
		// The prior durable frame must still satisfy the same freshness rule
		// as the current capture. A fresh response after a long restart gap
		// rebases the clock; it cannot turn unobserved downtime into work.
		if !previousFresh {
			observed.Validated = false
		}
		busy := ship.Voyage != nil || ship.Ship.Status == "sailing" || !emptyCargo(ship) ||
			contractsFresh && idleFreight(id, state.Contracts)
		if busy || observed.PortID != ship.Ship.CurrentPortID {
			observed.EarnedGameHours = 0
			observed.Validated = false
			observed.ClockRegressed = false
			observed.PortID = ship.Ship.CurrentPortID
		}
		if busy || !coherent || ship.Ship.CurrentPortID == "" {
			observed.Validated = false
			idle[id] = observed
			continue
		}
		hour := in.Clock.Calendar.TotalGameHours
		// A replay or backward UTC reading is only a rebaseline candidate.
		// A subsequent advancing frame must establish continuity without
		// credit. Retaining the candidate also avoids an old UTC high-water
		// mark blocking every otherwise coherent rollback-era observation.
		if !observed.LastClockUTC.IsZero() && !in.Clock.RealUTC.After(observed.LastClockUTC) {
			observed.Validated = false
			observed.ClockRegressed = false
			observed.LastClockUTC = in.Clock.RealUTC
			idle[id] = observed
			continue
		}
		regressed := hour < observed.LastValidatedGameHour
		if observed.Validated && !regressed {
			delta := hour - observed.LastValidatedGameHour
			observed.EarnedGameHours += min(delta, math.MaxInt64-observed.EarnedGameHours)
		}
		observed.LastValidatedGameHour = hour
		observed.LastClockUTC = in.Clock.RealUTC
		observed.Validated = true
		observed.ClockRegressed = regressed
		idle[id] = observed
	}
	if shipsFresh {
		for id := range idle {
			if !seen[id] {
				delete(idle, id)
			}
		}
	}
	r.snapshot["_ship_idle"] = encode(idle)
}

func (r *Runner) shipIdleEligible(in PlanningInput) bool {
	if !idleClockReady(in) || in.Ship.Ship.ID == "" || in.Ship.Ship.Status == "sunk" ||
		in.Ship.Voyage != nil || in.Ship.Ship.Status == "sailing" || !emptyCargo(in.Ship) ||
		idleFreight(in.Ship.Ship.ID, in.Contracts) {
		return false
	}
	observed := decode[map[string]shipIdleState](r.snapshot, "_ship_idle")[in.Ship.Ship.ID]
	return observed.Validated && !observed.ClockRegressed && observed.EarnedGameHours >= sourcingIdleGameHours &&
		observed.PortID != "" && observed.PortID == in.Ship.Ship.CurrentPortID &&
		observed.LastValidatedGameHour == in.Clock.Calendar.TotalGameHours && observed.LastClockUTC.Equal(in.Clock.RealUTC)
}
