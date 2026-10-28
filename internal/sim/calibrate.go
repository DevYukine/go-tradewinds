package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type Calibration struct {
	Samples      int                    `json:"samples"`
	Conservative float64                `json:"conservative"`
	Base         float64                `json:"base"`
	Optimistic   float64                `json:"optimistic"`
	Uncalibrated bool                   `json:"uncalibrated"`
	Speeds       map[string]SpeedFactor `json:"speeds"`
}

func percentile(samples []float64, fraction float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]float64(nil), samples...)
	sort.Float64s(ordered)
	position := math.Max(0, math.Min(1, fraction)) * float64(len(ordered)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	return ordered[lower] + (ordered[upper]-ordered[lower])*(position-float64(lower))
}

func RegenerationQuantiles(samples []float64) Calibration {
	calibration := Calibration{Samples: len(samples), Speeds: make(map[string]SpeedFactor)}
	if len(samples) < 5 {
		calibration.Uncalibrated = true
		calibration.Conservative, calibration.Base, calibration.Optimistic = 0, 0.5, 1
		return calibration
	}
	clipped := make([]float64, len(samples))
	for i, sample := range samples {
		clipped[i] = math.Max(0, math.Min(1, sample))
	}
	calibration.Conservative = percentile(clipped, 0.25)
	calibration.Base = percentile(clipped, 0.5)
	calibration.Optimistic = percentile(clipped, 0.75)
	return calibration
}

type sideObservation struct {
	at     time.Time
	hour   float64
	total  int64
	levels []engine.Level
}

func fillIdentity(fill RecordedFill) string {
	if fill.ID != "" {
		return fill.ID
	}
	encoded, _ := json.Marshal(fill)
	return string(encoded)
}

// recordedFills streams the tape once for the deduplicated fills inside the
// fit window. A fill can become visible in the snapshots long after its
// CreatedAt, so the fill set is only complete once this pass has ended.
func recordedFills(tape *Tape, from, until time.Time) (map[string]RecordedFill, int, error) {
	reader := tape.Reader()
	fills := make(map[string]RecordedFill)
	retained := 0
	retain := func(change int) error {
		if change > maxTapeRetainedBytes-retained {
			return fmt.Errorf("regeneration evidence exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
		}
		retained += change
		return nil
	}
	fillBytes := func(id string, fill RecordedFill) int {
		return len(id) + len(fill.OrderID) + len(fill.PortID) + len(fill.CommodityID) + len(fill.Side) + 128
	}
	for {
		at, err := reader.NextTime()
		if errors.Is(err, io.EOF) || err == nil && !at.Before(until) {
			break
		}
		if err != nil {
			return nil, 0, err
		}
		view, err := reader.Advance(at)
		if err != nil {
			return nil, 0, err
		}
		var recorded []RecordedFill
		_ = json.Unmarshal(view.Snapshot["fills"], &recorded)
		for _, fill := range recorded {
			fill.Side = tradeSide(fill.Side)
			if !fill.CreatedAt.Before(from) && fill.CreatedAt.Before(until) {
				id := fillIdentity(fill)
				change := fillBytes(id, fill)
				if old, exists := fills[id]; exists {
					change -= fillBytes(id, old)
				}
				if err := retain(change); err != nil {
					return nil, 0, err
				}
				fills[id] = fill
			}
		}
	}
	return fills, retained, nil
}

// Two streaming passes bound regeneration evidence to the state actually
// needed around recorded fills: the first collects the fills, the second
// keeps only each fill's pre observation and the open scan to its first
// actual re-quote. Repeated or unrelated book observations in a long fit
// window are dropped as they stream instead of being retained for the whole
// interval.
func regenerationSamples(tape *Tape, from, until time.Time) ([]float64, error) {
	fills, retained, err := recordedFills(tape, from, until)
	if err != nil {
		return nil, err
	}
	retain := func(change int) error {
		if change > maxTapeRetainedBytes-retained {
			return fmt.Errorf("regeneration evidence exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
		}
		retained += change
		return nil
	}
	bySide := make(map[string][]RecordedFill)
	for _, fill := range fills {
		key := sideKey(fill.PortID, fill.CommodityID, fill.Side)
		bySide[key] = append(bySide[key], fill)
	}
	scans := make(map[string]*sideScan, len(bySide))
	for key, recorded := range bySide {
		sort.Slice(recorded, func(i, j int) bool {
			if recorded[i].CreatedAt.Equal(recorded[j].CreatedAt) {
				return fillIdentity(recorded[i]) < fillIdentity(recorded[j])
			}
			return recorded[i].CreatedAt.Before(recorded[j].CreatedAt)
		})
		// The sorted fill index is derived state, so it is budgeted lightly
		// per entry on top of the fill records themselves.
		if err := retain(24 * len(recorded)); err != nil {
			return nil, err
		}
		scans[key] = &sideScan{key: key, fillsByTime: recorded}
	}
	if len(scans) == 0 {
		return nil, nil
	}
	reader := tape.Reader()
	var samples []float64
	for {
		at, err := reader.NextTime()
		if errors.Is(err, io.EOF) || err == nil && !at.Before(until) {
			break
		}
		if err != nil {
			return nil, err
		}
		view, err := reader.Advance(at)
		if err != nil {
			return nil, err
		}
		for _, port := range sortedKeys(view.Books) {
			book := view.Books[port]
			if book.Status == Dark || book.Status == "" || book.AsOf.Before(from) {
				continue
			}
			for _, commodity := range book.Market.Books {
				if commodity.Depth == nil {
					continue
				}
				for _, side := range []string{"buy", "sell"} {
					scan := scans[sideKey(port, commodity.CommodityID, side)]
					if scan == nil {
						continue
					}
					levels := commodity.Depth.Asks
					if side == "sell" {
						levels = commodity.Depth.Bids
					}
					observation := sideObservation{at: book.AsOf, hour: view.GameHour, levels: levels}
					for _, level := range levels {
						if level.IsNPC {
							observation.total += level.RemainingQuantity
						}
					}
					produced, err := scan.observe(observation, retain)
					if err != nil {
						return nil, err
					}
					samples = append(samples, produced...)
				}
			}
		}
	}
	return samples, nil
}

func observationSize(key string, observation sideObservation) int {
	size := len(key) + 128
	for _, level := range observation.levels {
		size += len(level.OrderID) + 64
	}
	return size
}

// fillMatchesRecordedLevel reports whether the fill hit one of the observed
// NPC levels, by order id when present and by price otherwise.
func fillMatchesRecordedLevel(fill RecordedFill, levels []engine.Level) bool {
	for _, level := range levels {
		if level.IsNPC && (fill.OrderID != "" && fill.OrderID == level.OrderID || fill.OrderID == "" && fill.PricePence == level.PricePence) {
			return true
		}
	}
	return false
}

// sideScan holds the bounded state one commodity side needs while streaming:
// its recorded fills in CreatedAt order, the pending pre observation for
// fills not yet crossed, and one open scan per distinct pre observation.
type sideScan struct {
	key          string
	fillsByTime  []RecordedFill
	next         int
	pending      sideObservation
	pendingBytes int
	hasPending   bool
	batches      []fillBatch
}

// fillBatch follows the observations after one shared pre observation until
// the first actual re-quote resolves every fill waiting on it.
type fillBatch struct {
	pre      sideObservation
	preBytes int
	eligible []RecordedFill
}

// observe advances the scan with one book observation. Only strictly newer
// observations join the recorded series, as before: an observation first
// becomes the pre observation of every fill whose CreatedAt it follows, then
// the first re-quote candidate of every open scan. Nothing else about the
// observation is retained, so unrelated book traffic costs nothing.
func (s *sideScan) observe(observation sideObservation, retain func(int) error) ([]float64, error) {
	if s.hasPending && !observation.at.After(s.pending.at) {
		return nil, nil
	}
	for s.next < len(s.fillsByTime) && s.fillsByTime[s.next].CreatedAt.Before(observation.at) {
		fill := s.fillsByTime[s.next]
		s.next++
		if !s.hasPending || fill.Quantity <= 0 || !fillMatchesRecordedLevel(fill, s.pending.levels) {
			continue
		}
		merged := false
		for i := range s.batches {
			if s.batches[i].pre.at.Equal(s.pending.at) {
				s.batches[i].eligible = append(s.batches[i].eligible, fill)
				merged = true
				break
			}
		}
		if merged {
			continue
		}
		preBytes := observationSize(s.key, s.pending)
		if err := retain(preBytes); err != nil {
			return nil, err
		}
		s.batches = append(s.batches, fillBatch{pre: s.pending, preBytes: preBytes, eligible: []RecordedFill{fill}})
	}
	var samples []float64
	remaining := s.batches[:0]
	for _, batch := range s.batches {
		if !npcRequoted(batch.pre.levels, observation.levels) {
			remaining = append(remaining, batch)
			continue
		}
		if err := retain(-batch.preBytes); err != nil {
			return nil, err
		}
		// Exhausted-order removals never end a scan; only a newly observed
		// NPC order does, and a first re-quote more than twelve game hours
		// after the pre observation is too stale to measure regeneration.
		if observation.hour-batch.pre.hour > 12 {
			continue
		}
		// Every fill lies inside its own intervening window because a pre
		// observation never follows the fill it precedes, so exactly one
		// match in [pre.at, post.at] is the fill itself.
		lo := sort.Search(len(s.fillsByTime), func(i int) bool { return !s.fillsByTime[i].CreatedAt.Before(batch.pre.at) })
		hi := sort.Search(len(s.fillsByTime), func(i int) bool { return s.fillsByTime[i].CreatedAt.After(observation.at) })
		for _, fill := range batch.eligible {
			if observation.at.Before(fill.CreatedAt) || hi-lo-1 > 0 {
				continue
			}
			sample := float64(observation.total-(batch.pre.total-fill.Quantity)) / float64(fill.Quantity)
			samples = append(samples, math.Max(0, math.Min(1, sample)))
			if err := retain(16); err != nil {
				return nil, err
			}
		}
	}
	s.batches = remaining
	size := observationSize(s.key, observation)
	if err := retain(size - s.pendingBytes); err != nil {
		return nil, err
	}
	s.pending, s.pendingBytes, s.hasPending = observation, size, true
	return samples, nil
}

type voyageObservation struct {
	actionID  string
	hull      string
	at        time.Time
	hours     float64
	baseHours float64
	cargo     int64
	capacity  int64
}

func voyageObservations(tape *Tape, from, to time.Time) ([]voyageObservation, error) {
	actions := append([]store.Action(nil), tape.Actions...)
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].CreatedAt.Equal(actions[j].CreatedAt) {
			return actions[i].ID < actions[j].ID
		}
		return actions[i].CreatedAt.Before(actions[j].CreatedAt)
	})
	reader := tape.Reader()
	nextDeparture := make(map[string]time.Time)
	type departureBoundary struct {
		at    time.Time
		after time.Time
	}
	byShip := make(map[string]departureBoundary)
	for i := len(actions) - 1; i >= 0; i-- {
		action := actions[i]
		if !action.CreatedAt.Before(to) || action.Status < 200 || action.Status >= 300 || !strings.HasSuffix(action.Path, "/depart") {
			continue
		}
		var request struct {
			ShipID string `json:"ship_id"`
		}
		if json.Unmarshal(action.Body, &request) != nil || request.ShipID == "" {
			continue
		}
		boundary, exists := byShip[request.ShipID]
		if !exists {
			boundary = departureBoundary{at: to, after: to}
		}
		if action.CreatedAt.Before(boundary.at) {
			boundary = departureBoundary{at: action.CreatedAt, after: boundary.at}
		}
		nextDeparture[action.ID] = boundary.after
		byShip[request.ShipID] = boundary
	}
	var arrivalIndex map[string][]recordedArrival
	observations := make([]voyageObservation, 0)
	for _, action := range actions {
		if action.CreatedAt.Before(from) || !action.CreatedAt.Before(to) || action.Status < 200 || action.Status >= 300 || !strings.HasSuffix(action.Path, "/depart") {
			continue
		}
		var request struct {
			ShipID      string `json:"ship_id"`
			Destination string `json:"dest_port_id"`
		}
		var response struct {
			ETA    time.Time `json:"estimated_arrival_at"`
			Voyage *struct {
				ETA time.Time `json:"estimated_arrival_at"`
			} `json:"voyage"`
		}
		if json.Unmarshal(action.Body, &request) != nil || json.Unmarshal(action.Response, &response) != nil {
			continue
		}
		view, err := reader.Advance(action.CreatedAt)
		if err != nil {
			return nil, err
		}
		var ship engine.ShipState
		for _, candidate := range view.State.Ships {
			if candidate.Ship.ID == request.ShipID {
				ship = candidate
				break
			}
		}
		if ship.Ship.ID == "" || ship.Ship.CurrentPortID == "" {
			continue
		}
		route, ok := view.State.Routes[ship.Ship.CurrentPortID+"|"+request.Destination+"|"+ship.Ship.ShipClass]
		if !ok {
			route, ok = view.State.Routes[ship.Ship.CurrentPortID+"|"+request.Destination+"|"]
		}
		if !ok {
			continue
		}
		var hull engine.Hull
		for _, candidate := range view.State.Catalog.ShipClasses {
			if candidate.ID == ship.Ship.ShipClass {
				hull = candidate
				break
			}
		}
		baseHours, err := VoyageGameHours(route, hull.BaseSpeedCentiknots, 1)
		if err != nil {
			continue
		}
		eta := response.ETA
		if eta.IsZero() && response.Voyage != nil {
			eta = response.Voyage.ETA
		}
		if eta.IsZero() {
			if arrivalIndex == nil {
				arrivalIndex, err = recordedETAs(tape, to)
				if err != nil {
					return nil, err
				}
			}
			eta = recordedETA(arrivalIndex[request.ShipID], action.CreatedAt, nextDeparture[action.ID])
		}
		hours := eta.Sub(action.CreatedAt).Hours() * float64(max(1, view.State.Clock.Calendar.TimeScale))
		if eta.IsZero() || hours <= 0 {
			continue
		}
		capacity := ship.EffectiveHoldCapacityTons
		if capacity <= 0 {
			capacity = hull.HoldCapacityTons
		}
		observations = append(observations, voyageObservation{actionID: action.ID, hull: hull.ID, at: action.CreatedAt, hours: hours, baseHours: baseHours, cargo: ship.CargoUsedTons, capacity: capacity})
	}
	return observations, nil
}

type recordedArrival struct {
	at  time.Time
	eta time.Time
}

// One monotone pass serves all departures, including overlapping voyages of
// different ships. Only changed ETA observations, not full frames, are kept.
func recordedETAs(tape *Tape, before time.Time) (map[string][]recordedArrival, error) {
	reader := tape.Reader()
	index := make(map[string][]recordedArrival)
	retained := 0
	for {
		at, err := reader.NextTime()
		if errors.Is(err, io.EOF) || err == nil && !at.Before(before) {
			break
		}
		if err != nil {
			return nil, err
		}
		view, err := reader.Advance(at)
		if err != nil {
			return nil, err
		}
		for _, ship := range view.State.Ships {
			if ship.Voyage == nil || ship.Voyage.EstimatedArrivalAt.IsZero() {
				continue
			}
			series := index[ship.Ship.ID]
			eta := ship.Voyage.EstimatedArrivalAt
			if len(series) != 0 && series[len(series)-1].eta.Equal(eta) {
				continue
			}
			retained += len(ship.Ship.ID) + 64
			if retained > maxTapeRetainedBytes {
				return nil, fmt.Errorf("recorded ETA index exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
			}
			index[ship.Ship.ID] = append(series, recordedArrival{at: at, eta: eta})
		}
	}
	return index, nil
}

func recordedETA(series []recordedArrival, after, before time.Time) time.Time {
	start := sort.Search(len(series), func(i int) bool { return !series[i].at.Before(after) })
	for _, observation := range series[start:] {
		if !observation.at.Before(before) {
			break
		}
		if observation.eta.After(after) {
			return observation.eta
		}
	}
	return time.Time{}
}

func fitSpeedFactors(observations []voyageObservation, until time.Time) map[string]SpeedFactor {
	byHull := make(map[string][]voyageObservation)
	for _, observation := range observations {
		if observation.at.Before(until) {
			byHull[observation.hull] = append(byHull[observation.hull], observation)
		}
	}
	factors := make(map[string]SpeedFactor)
	for _, hull := range sortedKeys(byHull) {
		rows := byHull[hull]
		var sumX, sumY, sumXX, sumXY float64
		for _, row := range rows {
			x := 0.0
			if row.capacity > 0 {
				x = math.Max(0, math.Min(1, float64(row.cargo)/float64(row.capacity)))
			}
			y := row.baseHours / row.hours
			sumX += x
			sumY += y
			sumXX += x * x
			sumXY += x * y
		}
		n := float64(len(rows))
		slope := 0.0
		if denominator := n*sumXX - sumX*sumX; math.Abs(denominator) > 1e-9 {
			slope = (n*sumXY - sumX*sumY) / denominator
		}
		empty := (sumY - slope*sumX) / n
		laden := empty + slope
		if empty <= 0 || laden <= 0 {
			empty, laden = 1, 0.8
		}
		factors[hull] = SpeedFactor{Empty: empty, Laden: laden, N: len(rows)}
	}
	return factors
}

func Calibrate(tape *Tape, from, fitUntil time.Time) (Calibration, error) {
	samples, err := regenerationSamples(tape, from, fitUntil)
	if err != nil {
		return Calibration{}, err
	}
	calibration := RegenerationQuantiles(samples)
	observations, err := voyageObservations(tape, from, fitUntil)
	if err != nil {
		return Calibration{}, err
	}
	calibration.Speeds = fitSpeedFactors(observations, fitUntil)
	reader := tape.Reader()
	view, err := reader.Advance(fitUntil.Add(-time.Nanosecond))
	if err != nil {
		return Calibration{}, err
	}
	for _, hull := range view.State.Catalog.ShipClasses {
		factor, exists := calibration.Speeds[hull.ID]
		if calibration.Uncalibrated || !exists {
			calibration.Speeds[hull.ID] = SpeedFactor{Empty: 1, Laden: 0.8, N: factor.N}
		}
	}
	return calibration, nil
}
