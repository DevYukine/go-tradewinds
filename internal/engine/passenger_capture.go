package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

const passengerFareIntervalGameHours = 10.0

// Retained fare evidence stays well inside the account snapshot budget: at
// most the documented 200-row page per lane and an eighth of
// store.MaxSnapshotBytes of raw rows across every retained lane.
const (
	passengerFareLaneMaxRows = 200
	passengerFareBudgetBytes = store.MaxSnapshotBytes / 8
)

func validatePassengerCapture(key string, raw json.RawMessage) error {
	switch {
	case key == "passenger_offers":
		var offers []gameapi.PassengerOffer
		return json.Unmarshal(raw, &offers)
	case strings.HasPrefix(key, "passenger_waiting/"):
		var groups []gameapi.PassengerWaitingGroup
		return json.Unmarshal(raw, &groups)
	case strings.HasPrefix(key, "passenger_aboard/"):
		var groups []gameapi.PassengerAboardGroup
		return json.Unmarshal(raw, &groups)
	case strings.HasPrefix(key, "passenger_fares/"):
		var fares []gameapi.PassengerFare
		return json.Unmarshal(raw, &fares)
	default:
		return nil
	}
}

// The clock request envelope bounds elapsed game time without relying on the
// workstation clock matching the server. Attempts retain the upper bound;
// admission uses the lower bound so a slow capture cannot shorten the interval.
func passengerCaptureGameHours(now time.Time, clock Clock, rawClock gameapi.Clock, meta DatasetMeta, generation string) (float64, float64, bool) {
	lower, upper, valid := serverBounds(now, clock, meta, generation)
	if !valid || clock.Calendar.TimeScale <= 0 || rawClock.Calendar.TotalGameHours < 0 {
		return 0, 0, false
	}
	hour := float64(rawClock.Calendar.TotalGameHours) + float64(rawClock.Calendar.Minute)/60
	if clock.Paused {
		return hour, hour, true
	}
	scale := float64(clock.Calendar.TimeScale)
	return hour + lower.Sub(clock.RealUTC).Hours()*scale, hour + upper.Sub(clock.RealUTC).Hours()*scale, true
}

// Passenger endpoints are optional observations: each failed capture retains
// its last-good payload and stale metadata, without failing the account refresh.
// The caller persists these snapshots together with the rest of this generation.
func (r *Runner) refreshPassengers(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := r.state()
	current := PlanningInput{Now: time.Now(), Clock: state.Clock, Metadata: r.meta, Generation: r.generation}
	waiting := decode[map[string]json.RawMessage](r.snapshot, "passenger_waiting")
	if waiting == nil {
		waiting = make(map[string]json.RawMessage)
	}
	ports := make(map[string]bool)
	fleetFresh := current.fresh("ships")
	boardingShips := decode[map[string]bool](r.snapshot, "_passenger_boarding_ships")
	retainedOffers := decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers")
	offeredShips := make(map[string]bool)
	for _, offer := range retainedOffers {
		if offer.Status == "open" {
			offeredShips[offer.ShipID] = true
		}
	}
	captureOffers := false
	if fleetFresh {
		for _, ship := range state.Ships {
			if ship.Ship.ID != "" && ship.Ship.Status != "sunk" &&
				(ship.PassengerHoldTons > 0 || boardingShips[ship.Ship.ID] || offeredShips[ship.Ship.ID]) {
				captureOffers = true
			}
			if ship.Ship.CurrentPortID != "" && ship.Voyage == nil &&
				(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") {
				ports[ship.Ship.CurrentPortID] = true
			}
		}
	}
	waitingKeys := make([]string, 0, len(ports))
	for port := range ports {
		waitingKeys = append(waitingKeys, port)
	}
	sort.Strings(waitingKeys)
	tasks := make([]fetchTask, 0, len(waitingKeys)+1)
	if captureOffers || len(waitingKeys) > 0 {
		tasks = append(tasks, fetchTask{key: "passenger_offers", path: "/v1/passengers/offers?open_only=true", kind: fetchPages})
	} else {
		meta := r.meta["passenger_offers"]
		meta.Complete, meta.Stale = false, true
		meta.Error = "no current stationary or passenger-bound owned ship"
		r.meta["passenger_offers"] = meta
	}
	for _, port := range waitingKeys {
		tasks = append(tasks, fetchTask{key: "passenger_waiting/" + port, path: "/v1/passengers/waiting?port_id=" + url.QueryEscape(port), kind: fetchJSON})
	}
	if err := r.captureWave(ctx, tasks, map[string]map[string]json.RawMessage{"passenger_waiting": waiting}); err != nil {
		return err
	}
	r.finishGroup("passenger_waiting", waiting, waitingKeys)
	if !fleetFresh {
		meta := r.meta["passenger_waiting"]
		meta.Complete = false
		meta.Stale = true
		meta.Error = "current fleet capture unavailable"
		r.meta["passenger_waiting"] = meta
	}

	// A lane stays relevant only with fresh occupied-port waiting evidence, an
	// open offer of ours, or passengers aboard. Irrelevance alone prunes
	// nothing: gates and payloads below survive failures and short absences
	// until ten authoritative game hours of irrelevance expire them.
	relevant := make(map[string]bool)
	openOffers := make(map[string]bool)
	for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
		if offer.Status == "open" {
			openOffers[offer.ShipID] = true
			if offer.PortID != "" && offer.DestPortID != "" && offer.PortID != offer.DestPortID {
				relevant[offer.PortID+"|"+offer.DestPortID] = true
			}
		}
	}
	aboard := decode[map[string]json.RawMessage](r.snapshot, "passenger_aboard")
	if aboard == nil {
		aboard = make(map[string]json.RawMessage)
	}
	aboardKeys := make([]string, 0)
	for _, ship := range state.Ships {
		if fleetFresh && ship.Ship.ID != "" && ship.Ship.Status != "sunk" &&
			(openOffers[ship.Ship.ID] || ship.PassengerHoldTons > 0 || boardingShips[ship.Ship.ID]) {
			aboardKeys = append(aboardKeys, ship.Ship.ID)
		}
	}
	sort.Strings(aboardKeys)
	tasks = tasks[:0]
	for _, ship := range aboardKeys {
		tasks = append(tasks, fetchTask{key: "passenger_aboard/" + ship, path: "/v1/passengers/aboard?ship_id=" + url.QueryEscape(ship), kind: fetchJSON})
	}

	fares := decode[map[string]json.RawMessage](r.snapshot, "passenger_fares")
	if fares == nil {
		fares = make(map[string]json.RawMessage)
	}
	fareHours := decode[map[string]float64](r.snapshot, "_passenger_fare_hours")
	if fareHours == nil {
		fareHours = make(map[string]float64)
	}
	rawClock := decode[gameapi.Clock](r.snapshot, "clock")
	current.Now = time.Now()
	lowerHour, upperHour, clockValid := passengerCaptureGameHours(current.Now, state.Clock, rawClock, r.meta["clock"], r.generation)
	lanes := make(map[string][2]string)
	observedWaiting := make(map[string]bool)
	for _, port := range waitingKeys {
		if !current.fresh("passenger_waiting/" + port) {
			continue
		}
		var groups []gameapi.PassengerWaitingGroup
		if err := json.Unmarshal(waiting[port], &groups); err != nil {
			continue
		}
		observedWaiting[port] = true
		for _, group := range groups {
			if group.OriginPortID != port || group.DestPortID == "" || group.DestPortID == port {
				continue
			}
			lane := port + "|" + group.DestPortID
			relevant[lane] = true
			if !clockValid {
				continue
			}
			if attempted, exists := fareHours[lane]; exists && lowerHour-attempted < passengerFareIntervalGameHours {
				continue
			}
			lanes[lane] = [2]string{port, group.DestPortID}
		}
	}
	requestedFares := make([]string, 0, len(lanes))
	for lane := range lanes {
		requestedFares = append(requestedFares, lane)
	}
	sort.Strings(requestedFares)
	for _, lane := range requestedFares {
		pair := lanes[lane]
		query := url.Values{"from": {pair[0]}, "to": {pair[1]}, "limit": {"200"}}
		tasks = append(tasks, fetchTask{key: "passenger_fares/" + lane, path: "/v1/passengers/fares?" + query.Encode(), kind: fetchJSON})
		fareHours[lane] = upperHour
	}
	r.snapshot["_passenger_fare_hours"] = encode(fareHours)
	if err := r.captureWave(ctx, tasks, map[string]map[string]json.RawMessage{"passenger_aboard": aboard, "passenger_fares": fares}); err != nil {
		return err
	}
	if _, completedHour, valid := passengerCaptureGameHours(time.Now(), state.Clock, rawClock, r.meta["clock"], r.generation); valid {
		for _, lane := range requestedFares {
			fareHours[lane] = completedHour
		}
	}
	// Passengers still ours keep their mission lane's evidence current even
	// when the origin port's waiting capture has moved on.
	for _, ship := range aboardKeys {
		var groups []gameapi.PassengerAboardGroup
		if err := json.Unmarshal(aboard[ship], &groups); err != nil {
			continue
		}
		// REST-observed boarding warrants arrival reconciliation just like a
		// retained boarding SSE event. Keep the hint until receipt settlement:
		// fleet hold can become zero before the next aboard capture.
		if len(groups) > 0 && r.fresh("passenger_aboard/"+ship) {
			if boardingShips == nil {
				boardingShips = make(map[string]bool)
			}
			boardingShips[ship] = true
		}
		for _, group := range groups {
			if group.OriginPortID != "" && group.DestPortID != "" && group.OriginPortID != group.DestPortID {
				relevant[group.OriginPortID+"|"+group.DestPortID] = true
			}
		}
	}
	r.snapshot["_passenger_boarding_ships"] = encode(boardingShips)
	// Fare gates and payloads survive captures that prove nothing: a failed
	// waiting read, a stale fleet or a short departure never resets the
	// ten-game-hour interval, and returning inside it grants no re-read. A
	// lane expires only after ten authoritative game hours of irrelevance,
	// counted from the conservative upper bound of the first pass that could
	// prove the lane empty (a fresh waiting capture at its origin without it,
	// or a fresh fleet with no stationary ship there and no open offer or
	// aboard evidence) and checked against the lower bound, so expiry happens
	// late, never early, and keeps working across restarts.
	irrelevantSince := decode[map[string]float64](r.snapshot, "_passenger_fare_irrelevant")
	if irrelevantSince == nil {
		irrelevantSince = make(map[string]float64)
	}
	retainedLanes := make([]string, 0, len(fareHours)+len(fares))
	for lane := range fareHours {
		retainedLanes = append(retainedLanes, lane)
	}
	for lane := range fares {
		if _, gated := fareHours[lane]; !gated {
			retainedLanes = append(retainedLanes, lane)
		}
	}
	for _, lane := range retainedLanes {
		if relevant[lane] {
			delete(irrelevantSince, lane)
			continue
		}
		origin, _, _ := strings.Cut(lane, "|")
		if clockValid && fleetFresh && (observedWaiting[origin] || !ports[origin]) {
			if _, started := irrelevantSince[lane]; !started {
				irrelevantSince[lane] = upperHour
			}
		}
	}
	for lane, since := range irrelevantSince {
		if _, gated := fareHours[lane]; !gated && len(fares[lane]) == 0 {
			delete(irrelevantSince, lane)
			continue
		}
		if clockValid && lowerHour-since >= passengerFareIntervalGameHours {
			delete(irrelevantSince, lane)
			delete(fareHours, lane)
			delete(fares, lane)
		}
	}
	for lane := range fares {
		fares[lane] = capPassengerFareRows(fares[lane])
	}
	r.boundPassengerFareBytes(fares)
	r.snapshot["_passenger_fare_hours"] = encode(fareHours)
	r.snapshot["_passenger_fare_irrelevant"] = encode(irrelevantSince)
	r.finishGroup("passenger_aboard", aboard, aboardKeys)
	fareKeys := make([]string, 0, len(fareHours)+len(fares))
	for lane := range fareHours {
		fareKeys = append(fareKeys, lane)
	}
	for lane := range fares {
		if _, exists := fareHours[lane]; !exists {
			fareKeys = append(fareKeys, lane)
		}
	}
	sort.Strings(fareKeys)
	r.finishGroup("passenger_fares", fares, fareKeys)
	// finishGroup counts every retained lane from an earlier generation as a
	// failure, but a lane the gate deliberately did not re-read keeps its
	// last-good value as valid evidence; only lanes whose latest attempt
	// failed leave the group stale.
	attempted := make(map[string]bool, len(requestedFares))
	for _, lane := range requestedFares {
		attempted[lane] = true
	}
	meta := DatasetMeta{UpdatedAt: time.Now(), AttemptedAt: time.Now(), Generation: r.generation, Complete: true}
	for _, lane := range fareKeys {
		child := r.meta["passenger_fares/"+lane]
		if !attempted[lane] && len(fares[lane]) > 0 &&
			!child.UpdatedAt.IsZero() && child.Complete && !child.Stale && child.Error == "" {
			continue
		}
		if child.Error != "" || child.Stale || !child.Complete || child.UpdatedAt.IsZero() || len(fares[lane]) == 0 {
			meta.Stale = true
			meta.Complete = false
			meta.Error = "one or more entries failed; see per-entry metadata"
		}
	}
	r.meta["passenger_fares"] = meta
	return nil
}

// capPassengerFareRows bounds one lane to the documented 200-row fare page so
// an oversized response cannot inflate the stored payload.
func capPassengerFareRows(raw json.RawMessage) json.RawMessage {
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) <= passengerFareLaneMaxRows {
		return raw
	}
	capped, err := json.Marshal(rows[:passengerFareLaneMaxRows])
	if err != nil {
		return raw
	}
	return capped
}

// boundPassengerFareBytes evicts the oldest lane payloads until the group's
// raw rows fit the budget. Evicted lanes keep their gate hour, so eviction
// never buys an extra read before the ten-game-hour interval expires.
func (r *Runner) boundPassengerFareBytes(fares map[string]json.RawMessage) {
	total := 0
	lanes := make([]string, 0, len(fares))
	for lane, raw := range fares {
		total += len(raw)
		lanes = append(lanes, lane)
	}
	if total <= passengerFareBudgetBytes {
		return
	}
	sort.Slice(lanes, func(i, j int) bool {
		older, newer := r.meta["passenger_fares/"+lanes[i]].UpdatedAt, r.meta["passenger_fares/"+lanes[j]].UpdatedAt
		if !older.Equal(newer) {
			return older.Before(newer)
		}
		return lanes[i] < lanes[j]
	})
	for _, lane := range lanes {
		if total <= passengerFareBudgetBytes {
			return
		}
		total -= len(fares[lane])
		delete(fares, lane)
	}
}
