package engine

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// freightStops preserves every seal while visiting distinct destinations in
// earliest-deadline order. Unknown deadlines remain first and block departure;
// they must never be guessed from an open listing's acceptance expiry.
func freightStops(obligations []freightObligation) []freightObligation {
	pending := make([]freightObligation, 0, len(obligations))
	for _, obligation := range obligations {
		if !obligation.Settled {
			pending = append(pending, obligation)
		}
	}
	sort.SliceStable(pending, func(i, j int) bool {
		left, right := pending[i], pending[j]
		if left.Deadline == nil || right.Deadline == nil {
			if left.Deadline == nil && right.Deadline != nil {
				return true
			}
			if right.Deadline == nil && left.Deadline != nil {
				return false
			}
		} else if !left.Deadline.Equal(*right.Deadline) {
			return left.Deadline.Before(*right.Deadline)
		}
		return left.Destination < right.Destination
	})
	seen := make(map[string]bool)
	stops := make([]freightObligation, 0, len(pending))
	for _, obligation := range pending {
		if !seen[obligation.Destination] {
			seen[obligation.Destination] = true
			stops = append(stops, obligation)
		}
	}
	return stops
}

func (r *Runner) validateFreightDeparture(ship ShipState, destination string, data *missionData, hull Hull) error {
	stops := freightStops(data.Freight)
	if len(stops) == 0 {
		return nil
	}
	// The existing mission's next stop is immutable during an unresolved
	// departure. Check it first, then every other outstanding seal.
	foundDestination := false
	for i := range stops {
		if stops[i].Destination == destination {
			first := stops[i]
			copy(stops[1:i+1], stops[:i])
			stops[0] = first
			foundDestination = true
			break
		}
	}
	from := ship.Ship.CurrentPortID
	days := 0.0
	if !foundDestination && from != destination {
		key := routeKey(from, destination, hull.ID)
		route := r.stateCache.Routes[key]
		days = float64(route.TotalDistanceNM)/(float64(hull.BaseSpeedCentiknots)/100*0.8)/24 + 0.5
		from = destination
	}
	for _, stop := range stops {
		if stop.Deadline == nil {
			return fmt.Errorf("sealed freight %s has no authoritative acceptance delivery_deadline", stop.ContractID)
		}
		if from != stop.Destination {
			key := routeKey(from, stop.Destination, hull.ID)
			if !r.fresh("routes/" + key) {
				return fmt.Errorf("sealed freight itinerary needs fresh route %s", key)
			}
			route := r.stateCache.Routes[key]
			if err := ValidateRoute(route, hull, r.stateCache.Catalog.SeaZones); err != nil {
				return fmt.Errorf("sealed freight itinerary: %w", err)
			}
			days += float64(route.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
		}
		seconds := math.Ceil((days + 1) * 86_400 / float64(r.stateCache.Clock.Calendar.TimeScale))
		arrival := time.Now().Add(time.Duration(seconds) * time.Second)
		if !arrival.Before(*stop.Deadline) {
			return fmt.Errorf("sealed freight %s cannot meet its actual acceptance delivery_deadline after preceding stops", stop.ContractID)
		}
		days += 0.5
		from = stop.Destination
	}
	return nil
}

func removeFreightCandidate(plan *Plan, rejectedID string) {
	remaining := plan.FreightIDs[:0]
	for _, id := range plan.FreightIDs {
		if id != rejectedID {
			remaining = append(remaining, id)
		}
	}
	plan.FreightIDs = remaining
}
