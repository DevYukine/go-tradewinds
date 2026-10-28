package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type Coverage string

const (
	Observed        Coverage = "observed"
	Carried         Coverage = "carried"
	Dark            Coverage = "dark"
	BookCarryLimit           = 6 * time.Hour
	PublicViewLimit          = 5 * time.Minute
)

// AccountingSnapshot is used only by receipt validation, never by a simulated
// policy: the current ledger must not leak future information into a run.
type Tape struct {
	Frames             []store.ReplayFrame
	Actions            []store.Action
	Settlements        []store.Settlement
	Links              []store.ReplayActionLink
	AccountingSnapshot map[string]json.RawMessage
	openFrames         func() *store.ReplayFrameReader
	first              time.Time
	last               time.Time
	readCharterPrefix  func(time.Time, time.Time) (int64, error)
	readProbePrefix    func([]string, time.Time) (map[string]time.Time, error)
	seedMu             sync.Mutex
	charterSince       time.Time
	charterBefore      time.Time
	charterProceeds    int64
	charterCached      bool
	probePrefixes      map[probePrefixKey]time.Time
	probePrefixBytes   int
	readSourcingPrefix func(time.Time, time.Time) ([]store.Settlement, error)
	sourcingSince      time.Time
	sourcingAsOf       time.Time
	sourcingPrefix     []store.Settlement
}

type probePrefixKey struct {
	lane   string
	before time.Time
}

func LoadTape(ctx context.Context, db *store.Store, account string, from, to time.Time) (*Tape, error) {
	if account == "" || !to.After(from) {
		return nil, errors.New("invalid replay window")
	}
	first, last, err := db.ReplayFrameBounds(ctx, account, to)
	if err != nil {
		return nil, err
	}
	tape := &Tape{first: first, last: last,
		openFrames: func() *store.ReplayFrameReader { return db.NewReplayFrameReader(ctx, account, to) },
		readCharterPrefix: func(since, before time.Time) (int64, error) {
			return db.ReplayCharterProceedsBefore(ctx, account, since, before)
		},
		readProbePrefix: func(lanes []string, before time.Time) (map[string]time.Time, error) {
			return db.ReplayProbeCompletionsBefore(ctx, account, lanes, before)
		},
		readSourcingPrefix: func(since, asOf time.Time) ([]store.Settlement, error) {
			return db.ReplaySourcingBudgetBefore(ctx, account, since, from, asOf)
		},
	}
	tape.Settlements, err = db.SettlementsBetween(ctx, account, from, to)
	if err != nil {
		return nil, err
	}
	missions := make([]string, 0, len(tape.Settlements))
	seenMissions := make(map[string]bool)
	for _, settlement := range tape.Settlements {
		if settlement.MissionID != "" && !seenMissions[settlement.MissionID] {
			seenMissions[settlement.MissionID] = true
			missions = append(missions, settlement.MissionID)
		}
	}
	tape.Links, err = db.ReplayActionLinks(ctx, account, from, to, missions)
	if err != nil {
		return nil, err
	}
	if err := tape.loadWindowActions(ctx, db, account, from, to); err != nil {
		return nil, err
	}
	tape.AccountingSnapshot, err = db.ReplayAccountingSnapshot(ctx, account)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return tape, nil
}

// FrameBounds is metadata-only for a database tape; loading does not retain
// every decrypted frame. Synthetic tapes may supply their frames in memory.
func (t *Tape) FrameBounds() (first, last time.Time) {
	if t.openFrames != nil {
		return t.first, t.last
	}
	for _, frame := range t.Frames {
		if first.IsZero() || frame.ObservedAt.Before(first) {
			first = frame.ObservedAt
		}
		if last.IsZero() || frame.ObservedAt.After(last) {
			last = frame.ObservedAt
		}
	}
	return first, last
}

func (t *Tape) Reader() *TapeReader {
	if t.openFrames != nil {
		return &TapeReader{source: t.openFrames(), values: make(map[string]tapeValue)}
	}
	return NewTapeReader(t.Frames)
}

// Retain the rolling accounting prefix, not a scalar lifetime allowance. Older
// probes remain necessary to attribute a later delivery to its source inspection.
func (t *Tape) sourcingSettlementsBefore(at time.Time, scale int64) ([]store.Settlement, error) {
	if t.readSourcingPrefix == nil {
		return nil, nil
	}
	since := at.Add(-max(engine.SourcingBudgetWindow(scale), 7*24*time.Hour))
	t.seedMu.Lock()
	defer t.seedMu.Unlock()
	if !at.Equal(t.sourcingAsOf) || t.sourcingSince.IsZero() || since.Before(t.sourcingSince) {
		rows, err := t.readSourcingPrefix(since, at)
		if err != nil {
			return nil, err
		}
		t.sourcingSince, t.sourcingAsOf, t.sourcingPrefix = since, at, rows
	}
	return t.sourcingPrefix, nil
}

func (t *Tape) eachFrame(before time.Time, yield func(store.ReplayFrame) error) error {
	reader := t.Reader()
	for {
		at, err := reader.nextFrameTime()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !before.IsZero() && !at.Before(before) {
			return nil
		}
		frame, err := reader.takeFrame()
		if err != nil {
			return err
		}
		if err := yield(frame); err != nil {
			return err
		}
	}
}

type BookObservation struct {
	Market     engine.Market
	Status     Coverage
	ObservedAt time.Time
	AsOf       time.Time
}

type FrameView struct {
	At       time.Time
	GameHour float64
	Phase    int
	State    engine.AccountState
	Snapshot map[string]json.RawMessage
	Meta     map[string]engine.DatasetMeta
	Books    map[string]BookObservation
}

type tapeValue struct {
	data  json.RawMessage
	meta  engine.DatasetMeta
	at    time.Time
	phase int
}

type pendingTapeValue struct {
	key   string
	value tapeValue
}

// TapeReader is a monotone cursor. It retains last-good values, not future
// frames; all freshness is measured against upstream observation time.
type TapeReader struct {
	frames        []store.ReplayFrame
	next          int
	at            time.Time
	latest        time.Time
	phase         int
	values        map[string]tapeValue
	pending       []pendingTapeValue
	source        *store.ReplayFrameReader
	retained      int
	clock         engine.Clock
	gameHour      float64
	lastClockAt   time.Time
	lastClockHour float64
	clockIssues   []string
}

func NewTapeReader(frames []store.ReplayFrame) *TapeReader {
	ordered := append([]store.ReplayFrame(nil), frames...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
	})
	return &TapeReader{frames: ordered, values: make(map[string]tapeValue)}
}

func (r *TapeReader) nextFrameTime() (time.Time, error) {
	if r.source != nil {
		return r.source.PeekTime()
	}
	if r.next == len(r.frames) {
		return time.Time{}, io.EOF
	}
	return r.frames[r.next].ObservedAt, nil
}

func (r *TapeReader) takeFrame() (store.ReplayFrame, error) {
	if r.source != nil {
		return r.source.Next()
	}
	if r.next == len(r.frames) {
		return store.ReplayFrame{}, io.EOF
	}
	frame := r.frames[r.next]
	r.next++
	return frame, nil
}

// NextTime reads only frame metadata and pending completion timestamps.
func (r *TapeReader) NextTime() (time.Time, error) {
	next, err := r.nextFrameTime()
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, err
	}
	for _, pending := range r.pending {
		if next.IsZero() || pending.value.at.Before(next) {
			next = pending.value.at
		}
	}
	if next.IsZero() {
		return next, io.EOF
	}
	return next, nil
}

const maxTapeRetainedBytes = 64 << 20

func tapeValueBytes(key string, value tapeValue) int {
	return len(key) + len(value.data) + len(value.meta.Generation) + len(value.meta.Error) + 256
}

func (r *TapeReader) remember(key string, value tapeValue) error {
	change := tapeValueBytes(key, value)
	if old, exists := r.values[key]; exists {
		change -= tapeValueBytes(key, old)
	}
	if change > maxTapeRetainedBytes-r.retained {
		return fmt.Errorf("last-good replay state exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
	}
	r.retained += change
	r.values[key] = value
	return nil
}

func (r *TapeReader) Advance(at time.Time) (FrameView, error) {
	if !r.at.IsZero() && at.Before(r.at) {
		return FrameView{}, errors.New("replay time cannot move backwards")
	}
	for {
		next, err := r.nextFrameTime()
		if errors.Is(err, io.EOF) || err == nil && next.After(at) {
			break
		}
		if err != nil {
			return FrameView{}, err
		}
		frame, err := r.takeFrame()
		if err != nil {
			return FrameView{}, err
		}
		r.latest, r.phase = frame.ObservedAt, frame.Phase
		r.applyRouteInventory(frame)
		for _, key := range sortedKeys(frame.Datasets) {
			dataset := frame.Datasets[key]
			var meta engine.DatasetMeta
			if len(dataset.Meta) != 0 && json.Unmarshal(dataset.Meta, &meta) != nil {
				return FrameView{}, fmt.Errorf("invalid metadata for dataset %s", key)
			}
			if len(dataset.Data) == 0 || !json.Valid(dataset.Data) || !meta.Complete || meta.Stale || meta.Error != "" {
				continue
			}
			observedAt := meta.UpdatedAt
			if observedAt.IsZero() {
				observedAt = frame.ObservedAt
			}
			value := tapeValue{dataset.Data, meta, observedAt, frame.Phase}
			if observedAt.After(at) {
				size := tapeValueBytes(key, value)
				if size > maxTapeRetainedBytes-r.retained {
					return FrameView{}, fmt.Errorf("pending replay state exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
				}
				r.retained += size
				r.pending = append(r.pending, pendingTapeValue{key, value})
			} else if previous, exists := r.values[key]; !exists || !previous.at.After(observedAt) {
				if err := r.remember(key, value); err != nil {
					return FrameView{}, err
				}
			}
		}
		if len(frame.Clock) != 0 {
			var clock engine.Clock
			if json.Unmarshal(frame.Clock, &clock) == nil && !clock.RealUTC.After(at) {
				r.clock = clock
			}
		}
	}
	remaining := r.pending[:0]
	for _, pending := range r.pending {
		if pending.value.at.After(at) {
			remaining = append(remaining, pending)
			continue
		}
		r.retained -= tapeValueBytes(pending.key, pending.value)
		if previous, exists := r.values[pending.key]; !exists || !previous.at.After(pending.value.at) {
			if err := r.remember(pending.key, pending.value); err != nil {
				return FrameView{}, err
			}
		}
	}
	clear(r.pending[len(remaining):])
	r.pending = remaining
	if value, exists := r.values["clock"]; exists {
		_ = json.Unmarshal(value.data, &r.clock)
	}
	r.at = at
	view := FrameView{At: at, Phase: r.phase, Snapshot: make(map[string]json.RawMessage), Meta: make(map[string]engine.DatasetMeta), Books: make(map[string]BookObservation)}
	if r.latest.IsZero() {
		return view, nil
	}
	for _, key := range sortedKeys(r.values) {
		value := r.values[key]
		if value.at.After(at) {
			continue
		}
		if (key == "routes" || strings.HasPrefix(key, "routes/")) && value.phase != r.phase {
			continue
		}
		view.Snapshot[key] = value.data
		view.Meta[key] = value.meta
	}
	// The engine snapshot format groups child datasets; tape frames flatten
	// them so one stale child cannot falsely invalidate its siblings.
	for _, group := range []string{"books", "ports", "routes", "ship_details", "yard_quotes", "inventory"} {
		children := make(map[string]json.RawMessage)
		_ = json.Unmarshal(view.Snapshot[group], &children)
		for key, data := range view.Snapshot {
			if strings.HasPrefix(key, group+"/") {
				children[strings.TrimPrefix(key, group+"/")] = data
			}
		}
		if len(children) != 0 {
			view.Snapshot[group], _ = json.Marshal(children)
		}
	}
	clock := r.clock
	if clock.Calendar.TimeScale <= 0 {
		clock.Calendar.TimeScale = 60
	}
	view.GameHour = float64(clock.Calendar.TotalGameHours) + float64(clock.Calendar.Minute)/60
	clockAt := clock.RealUTC
	if clockAt.IsZero() {
		clockAt = r.latest
	}
	if !clock.Paused && at.After(clockAt) {
		view.GameHour += at.Sub(clockAt).Hours() * float64(clock.Calendar.TimeScale)
	}
	if !clock.RealUTC.IsZero() {
		sampleHour := float64(clock.Calendar.TotalGameHours) + float64(clock.Calendar.Minute)/60
		if clock.RealUTC.After(r.lastClockAt) {
			if !r.lastClockAt.IsZero() && sampleHour+1.0/60+1e-9 < r.lastClockHour {
				r.noteClockIssue(clock.RealUTC, "authoritative game clock moved backwards")
			}
			r.lastClockAt, r.lastClockHour = clock.RealUTC, sampleHour
		}
		if view.GameHour+1.0/60+1e-9 < r.gameHour {
			r.noteClockIssue(clock.RealUTC, "clock interpolation exceeded a later sample; an unobserved pause or clock gap makes elapsed simulation time uncertain")
		}
		// Do not rewind ships or reverse bills already emitted on an estimated
		// interval. The explicit evidence issue prevents this run being trusted.
		view.GameHour = max(view.GameHour, r.gameHour)
		r.gameHour = view.GameHour
	}
	clock.RealUTC = at
	clock.Calendar.TotalGameHours = int64(view.GameHour)
	clock.Calendar.Minute = min(59, int((view.GameHour-float64(clock.Calendar.TotalGameHours))*60+1e-9))
	view.Snapshot["clock"], _ = json.Marshal(clock)
	view.State = engine.StateFromSnapshot(at, view.Snapshot, engine.PlanningSources{Metadata: view.Meta})
	// A last-good detail may have survived a failed fetch; it cannot replace
	// a newer fleet row with an older location, cargo, or voyage.
	fleetAt := r.values["ships"].at
	var groupedDetails map[string]json.RawMessage
	for i, ship := range view.State.Ships {
		key := "ship_details/" + ship.Ship.ID
		value, exists := r.values[key]
		if !exists {
			value, exists = r.values["ship_details"]
			if exists {
				if groupedDetails == nil {
					_ = json.Unmarshal(value.data, &groupedDetails)
				}
				value.data = groupedDetails[ship.Ship.ID]
			}
		}
		if !exists || value.at.Before(fleetAt) {
			continue
		}
		var detail engine.ShipState
		if json.Unmarshal(value.data, &detail) == nil && detail.Ship.ID == ship.Ship.ID {
			view.State.Ships[i] = detail
		}
	}
	for _, port := range view.State.Catalog.Ports {
		view.Books[port.ID] = BookObservation{Status: Dark}
	}
	for _, port := range sortedKeys(view.State.Markets) {
		market := view.State.Markets[port]
		value, exists := r.values["books/"+port]
		if !exists {
			value, exists = r.values["books"]
		}
		if !exists {
			view.Books[port] = BookObservation{Status: Dark}
			delete(view.State.Markets, port)
			continue
		}
		asOf := value.at
		if value.meta.UpstreamAsOf != nil {
			asOf = *value.meta.UpstreamAsOf
		} else if market.AsOf != nil {
			asOf = *market.AsOf
		}
		age := at.Sub(asOf)
		status := Dark
		if age >= 0 && age <= BookCarryLimit {
			status = Carried
			if age <= 2*time.Minute {
				status = Observed
			}
		}
		view.Books[port] = BookObservation{Market: market, Status: status, ObservedAt: value.at, AsOf: asOf}
		if status == Dark {
			delete(view.State.Markets, port)
		}
	}
	for _, port := range sortedKeys(view.State.Ports) {
		value, exists := r.values["ports/"+port]
		if !exists {
			value, exists = r.values["ports"]
		}
		if !exists || at.Sub(value.at) > PublicViewLimit {
			meta := view.Meta["ports/"+port]
			meta.Stale, meta.Complete = true, false
			view.Meta["ports/"+port] = meta
		}
	}
	return view, nil
}

func (r *TapeReader) noteClockIssue(at time.Time, reason string) {
	if len(r.clockIssues) == 0 {
		r.clockIssues = append(r.clockIssues, fmt.Sprintf("%s at %s", reason, at.UTC().Format(time.RFC3339Nano)))
	}
}

func (r *TapeReader) ClockIssues() []string {
	return append([]string(nil), r.clockIssues...)
}

func (r *TapeReader) applyRouteInventory(frame store.ReplayFrame) {
	listed := make(map[string]bool)
	full := frame.Decision || frame.Source != "diagnostic_backfill"
	for key, dataset := range frame.Datasets {
		if key == "routes" {
			var routes map[string]json.RawMessage
			if json.Unmarshal(dataset.Data, &routes) == nil {
				for route := range routes {
					listed["routes/"+route] = true
				}
				full = true
			}
		}
		if !strings.HasPrefix(key, "routes/") {
			continue
		}
		listed[key] = true
		var meta engine.DatasetMeta
		if json.Unmarshal(dataset.Meta, &meta) == nil && !meta.UpdatedAt.IsZero() && meta.UpdatedAt.Before(frame.ObservedAt) {
			full = true
		}
	}
	// Live routes are emitted only by full sweep/decision frames. Backfill
	// also emits individual fresh route responses; those are not inventories.
	if !full || len(listed) == 0 {
		return
	}
	for key := range r.values {
		if (key == "routes" || strings.HasPrefix(key, "routes/")) && !listed[key] {
			r.retained -= tapeValueBytes(key, r.values[key])
			delete(r.values, key)
		}
	}
	remaining := r.pending[:0]
	for _, pending := range r.pending {
		if !strings.HasPrefix(pending.key, "routes/") || listed[pending.key] {
			remaining = append(remaining, pending)
		} else {
			r.retained -= tapeValueBytes(pending.key, pending.value)
		}
	}
	clear(r.pending[len(remaining):])
	r.pending = remaining
}

// VisibleTo strips presence-gated intelligence without manufacturing books for
// an unseen port. Recorded factors are fixed historical visibility, not hires.
func (v FrameView) VisibleTo(ships []engine.ShipState) FrameView {
	visible := make(map[string]bool)
	for _, ship := range ships {
		if ship.Voyage == nil && ship.Ship.CurrentPortID != "" {
			visible[ship.Ship.CurrentPortID] = true
		}
	}
	for _, port := range v.State.Factors {
		visible[port] = true
	}
	v.State.Ports = cloneMap(v.State.Ports)
	v.State.Markets = cloneMap(v.State.Markets)
	v.Snapshot = cloneMap(v.Snapshot)
	for _, port := range sortedKeys(v.State.Ports) {
		detail := v.State.Ports[port]
		if visible[port] {
			continue
		}
		detail.CoffeeHouseGossip = nil
		v.State.Ports[port] = detail
		key := "ports/" + port
		var public map[string]json.RawMessage
		if json.Unmarshal(v.Snapshot[key], &public) == nil && public != nil {
			delete(public, "coffee_house_gossip")
			delete(public, "recent_sailings")
			v.Snapshot[key], _ = json.Marshal(public)
		}
	}
	for _, port := range sortedKeys(v.State.Markets) {
		market := v.State.Markets[port]
		if !visible[port] {
			market.Visibility = "public"
			v.State.Markets[port] = market
		}
	}
	v.Snapshot["ports"], _ = json.Marshal(v.State.Ports)
	return v
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	result := make(map[K]V, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

type PortCoverage struct {
	Port          string  `json:"port"`
	ObservedHours float64 `json:"observed_hours"`
	CarriedHours  float64 `json:"carried_hours"`
	DarkHours     float64 `json:"dark_hours"`
}

func (t *Tape) Coverage(from, to time.Time) ([]PortCoverage, error) {
	rows, _, err := t.coverage(from, to)
	return rows, err
}

func (t *Tape) coverage(from, to time.Time) ([]PortCoverage, []string, error) {
	ports := make(map[string]bool)
	err := t.eachFrame(to, func(frame store.ReplayFrame) error {
		for key, dataset := range frame.Datasets {
			switch {
			case key == "catalog":
				var catalog engine.Catalog
				_ = json.Unmarshal(dataset.Data, &catalog)
				for _, port := range catalog.Ports {
					ports[port.ID] = true
				}
			case key == "books":
				var books map[string]json.RawMessage
				_ = json.Unmarshal(dataset.Data, &books)
				for port := range books {
					ports[port] = true
				}
			case strings.HasPrefix(key, "books/"):
				ports[strings.TrimPrefix(key, "books/")] = true
			}
		}
		if len(ports) > 65_536 {
			return fmt.Errorf("coverage port count exceeds 65536: %w", store.ErrReplayEvidenceBudget)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	reader := t.Reader()
	missingHazard := false
	totals := make(map[string]PortCoverage, len(ports))
	for at := from; at.Before(to); {
		view, err := reader.Advance(at.Add(time.Nanosecond))
		if err != nil {
			return nil, nil, err
		}
		missingHazard = missingHazard || !recordedHazardAvailable(view)
		next := to
		frameAt, err := reader.NextTime()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, nil, err
		}
		if err == nil && frameAt.Before(next) {
			next = frameAt
		}
		for port, book := range view.Books {
			ports[port] = true
			for _, expiry := range []time.Time{book.AsOf.Add(2 * time.Minute), book.AsOf.Add(BookCarryLimit)} {
				if expiry.After(at) && expiry.Before(next) {
					next = expiry
				}
			}
		}
		hours := next.Sub(at).Hours()
		for port := range ports {
			row := totals[port]
			row.Port = port
			switch view.Books[port].Status {
			case Observed:
				row.ObservedHours += hours
			case Carried:
				row.CarriedHours += hours
			default:
				row.DarkHours += hours
			}
			totals[port] = row
		}
		// Check the estimate immediately before the next available sample,
		// without exposing that sample early. Hidden pauses invalidate trust
		// even when no trade happened during the gap.
		if next.After(reader.at.Add(time.Nanosecond)) {
			if _, err := reader.Advance(next.Add(-time.Nanosecond)); err != nil {
				return nil, nil, err
			}
		}
		at = next
	}
	result := make([]PortCoverage, 0, len(totals))
	for _, row := range totals {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Port < result[j].Port })
	issues := reader.ClockIssues()
	if missingHazard {
		issues = append(issues, "no recorded Lloyd's quotes")
	}
	return result, issues, nil
}

func recordedHazardAvailable(view FrameView) bool {
	for _, quote := range view.State.HazardQuotes {
		if !quote.ObservedAt.IsZero() && !quote.ObservedAt.After(view.At) &&
			quote.ExpectedLossBPS >= 0 && !quote.Illegal {
			return true
		}
	}
	return false
}
