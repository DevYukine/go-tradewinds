package engine

import (
	"encoding/json"
	"fmt"
	"time"
)

const marketEventStressBPS int64 = 2_500
const marketEventRecentWindow = 30 * time.Minute

// MarketShock identifies a temporary price regime, not a published magnitude.
// Structural news expires too, but its economic change does not end with it.
type MarketShock struct {
	ID          string    `json:"id"`
	EventType   string    `json:"event_type"`
	PortID      string    `json:"target_port_id"`
	CommodityID string    `json:"commodity_id"`
	StartsAt    time.Time `json:"starts_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	Structural  bool      `json:"structural"`
}

func decodeMarketShocks(snapshot map[string]json.RawMessage) (map[string]map[string][]MarketShock, bool) {
	var events []MarketShock
	raw, exists := snapshot["world_events"]
	if !exists || json.Unmarshal(raw, &events) != nil || events == nil {
		return nil, false
	}
	shocks := make(map[string]map[string][]MarketShock)
	for _, event := range events {
		if event.ID == "" || event.EventType == "" || event.StartsAt.IsZero() || !event.ExpiresAt.After(event.StartsAt) {
			return nil, false
		}
		if event.Structural || event.PortID == "" || event.CommodityID == "" {
			continue
		}
		if shocks[event.PortID] == nil {
			shocks[event.PortID] = make(map[string][]MarketShock)
		}
		shocks[event.PortID][event.CommodityID] = append(shocks[event.PortID][event.CommodityID], event)
	}
	return shocks, true
}

func (in PlanningInput) marketShockOverlapping(port, commodity string, from, to time.Time) (MarketShock, bool) {
	if from.IsZero() || to.Before(from) {
		return MarketShock{}, false
	}
	for _, shock := range in.MarketShocks[port][commodity] {
		if !to.Before(shock.StartsAt) && from.Before(shock.ExpiresAt) {
			return shock, true
		}
	}
	return MarketShock{}, false
}

// marketObservationBounds converts host request timestamps through the clock's
// server envelope. Upstream timestamps, when available, already use server time.
func (in PlanningInput) marketObservationBounds(dataset string) (time.Time, time.Time, bool) {
	lower, upper, ok := in.serverBounds()
	meta := in.Metadata[dataset]
	if !ok || meta.UpdatedAt.IsZero() || meta.UpdatedAt.After(in.Now) {
		return time.Time{}, time.Time{}, false
	}
	if meta.UpstreamAsOf != nil {
		return *meta.UpstreamAsOf, *meta.UpstreamAsOf, !meta.UpstreamAsOf.IsZero() && !meta.UpstreamAsOf.After(upper)
	}
	attempted := meta.AttemptedAt
	if attempted.IsZero() {
		attempted = meta.UpdatedAt
	}
	if attempted.After(meta.UpdatedAt) {
		return time.Time{}, time.Time{}, false
	}
	return lower.Add(attempted.Sub(in.Now)), upper.Add(meta.UpdatedAt.Sub(in.Now)), true
}

func (in PlanningInput) marketEventUnavailable() bool {
	_, tracked := in.Metadata["world_events"]
	return in.marketEventsInvalid || tracked && !in.fresh("world_events")
}

// marketObservationPolicy treats a zero arrival as unknown, never as permission
// to chase an expiring shock. The Boolean requires a quarter-revenue net floor.
func (in PlanningInput) marketObservationPolicy(port, commodity string, from, to, arrival time.Time) (bool, string) {
	lower, upper, clockOK := in.serverBounds()
	receiptAt := upper
	if arrival.After(receiptAt) {
		receiptAt = arrival
	}
	affected := false
	for _, shock := range in.MarketShocks[port][commodity] {
		if to.Before(shock.StartsAt) {
			// A supply boom can lower bids before delivery even when the quote
			// predates it. Only known bid-supporting shocks preserve that baseline.
			if shock.EventType != "demand_shortage" && shock.EventType != "admiralty_mobilization" &&
				!receiptAt.IsZero() && !receiptAt.Before(shock.StartsAt) {
				affected = true
			}
			continue
		}
		if !from.Before(shock.ExpiresAt) {
			continue
		}
		if arrival.IsZero() || arrival.After(shock.ExpiresAt) {
			return false, fmt.Sprintf("market event %s (%s) expires before the bounded destination arrival", shock.ID, shock.EventType)
		}
		affected = true
		// Check every overlapping shock before permitting an in-window price.
	}
	if affected {
		return true, ""
	}
	if in.marketEventUnavailable() {
		// Exclude the observation only when even its youngest plausible age
		// exceeds the window. The 30-real-minute edge is inclusive.
		if clockOK && !from.IsZero() && !to.IsZero() && !from.After(upper) && lower.Sub(to) <= marketEventRecentWindow {
			return true, ""
		}
	}
	return false, ""
}

func eventPriceStress(revenue, stress int64, required bool) int64 {
	if required {
		return max(stress, ceilBPS(revenue, marketEventStressBPS))
	}
	return stress
}
