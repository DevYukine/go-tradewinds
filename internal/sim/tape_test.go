package sim

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestTapeUsesLastGoodBookUntilLaterPassResponseCompletes(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	makeBook := func(when time.Time, amount int64) store.ReplayDataset {
		meta, err := json.Marshal(engine.DatasetMeta{Complete: true, UpdatedAt: when, UpstreamAsOf: &when})
		if err != nil {
			t.Fatal(err)
		}
		market := engine.Market{PortID: "a", Visibility: "quay"}
		market.Books = []engine.CommodityBook{{CommodityID: "cloth"}}
		var book engine.CommodityBook
		if err := json.Unmarshal([]byte(`{"commodity_id":"cloth","depth":{"asks":[{"order_id":"npc","price_pence":90,"remaining_quantity":1,"is_npc":true}]}}`), &book); err != nil {
			t.Fatal(err)
		}
		book.Depth.Asks[0].RemainingQuantity = amount
		market.Books[0] = book
		encoded, err := json.Marshal(market)
		if err != nil {
			t.Fatal(err)
		}
		return store.ReplayDataset{Data: encoded, Meta: meta}
	}
	frames := []store.ReplayFrame{
		{ObservedAt: at, Phase: 1, Datasets: map[string]store.ReplayDataset{"books/a": makeBook(at, 10)}},
		{ObservedAt: at.Add(time.Minute), Phase: 1, Datasets: map[string]store.ReplayDataset{"books/a": makeBook(at.Add(2*time.Minute), 20)}},
	}
	reader := NewTapeReader(frames)
	view, err := reader.Advance(at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Books["a"].Market.Books[0].Depth.Asks[0].RemainingQuantity; got != 10 {
		t.Fatalf("unfinished observation hid last-good state or leaked future response: %d", got)
	}
	if next, err := reader.NextTime(); err != nil || !next.Equal(at.Add(2*time.Minute)) {
		t.Fatalf("stream cursor skipped a deferred response completion: %s, %v", next, err)
	}
	view, err = reader.Advance(at.Add(2 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := view.Books["a"].Market.Books[0].Depth.Asks[0].RemainingQuantity; got != 20 {
		t.Fatalf("completed observation never became visible: %d", got)
	}
}

func TestCoverageIncludesDarkTimeBeforeFirstGroupedBook(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	seen := at.Add(2 * time.Hour)
	meta, err := json.Marshal(engine.DatasetMeta{Complete: true, UpdatedAt: seen})
	if err != nil {
		t.Fatal(err)
	}
	books, err := json.Marshal(map[string]engine.Market{"a": {PortID: "a", Visibility: "quay", AsOf: &seen}})
	if err != nil {
		t.Fatal(err)
	}
	tape := Tape{Frames: []store.ReplayFrame{{ObservedAt: seen, Datasets: map[string]store.ReplayDataset{
		"books":   {Data: books, Meta: meta},
		"catalog": {Data: json.RawMessage(`{"ports":[{"id":"a"},{"id":"b"}]}`), Meta: meta},
	}}}}
	coverage, err := tape.Coverage(at, at.Add(10*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(coverage) != 2 || coverage[1].Port != "b" || coverage[1].DarkHours != 10 {
		t.Fatalf("grouped or never-observed catalog port missing full-window coverage: %+v", coverage)
	}
	row := coverage[0]
	if math.Abs(row.ObservedHours-1.0/30) > 1e-9 || math.Abs(row.CarriedHours-(6-1.0/30)) > 1e-9 || math.Abs(row.DarkHours-4) > 1e-9 {
		t.Fatalf("coverage omitted pre-observation dark time or book expiry: %+v", row)
	}
}

func TestTapeIntegerClockRefreshDoesNotRewindGameHours(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	makeFrame := func(offset time.Duration, hour int64, minute int) store.ReplayFrame {
		clock := engine.Clock{RealUTC: at.Add(offset)}
		clock.Calendar.TimeScale, clock.Calendar.TotalGameHours = 60, hour
		clock.Calendar.Minute = minute
		data, err := json.Marshal(clock)
		if err != nil {
			t.Fatal(err)
		}
		return store.ReplayFrame{ObservedAt: clock.RealUTC, Clock: data}
	}
	reader := NewTapeReader([]store.ReplayFrame{makeFrame(0, 100, 0), makeFrame(45*time.Second, 100, 45), makeFrame(time.Minute, 101, 0), makeFrame(2*time.Minute, 99, 0)})
	last, accumulated := 100.0, 0.0
	for _, step := range []struct {
		offset time.Duration
		hour   float64
	}{{30 * time.Second, 100.5}, {45 * time.Second, 100.75}, {time.Minute, 101}} {
		view, err := reader.Advance(at.Add(step.offset))
		if err != nil {
			t.Fatal(err)
		}
		if view.GameHour != step.hour {
			t.Fatalf("clock refresh rewound or invented game time: got %g want %g", view.GameHour, step.hour)
		}
		accumulated += max(0, view.GameHour-last)
		last = view.GameHour
	}
	if accumulated != 1 {
		t.Fatalf("integer clock refresh double-counted the same elapsed hour: %g", accumulated)
	}
	view, err := reader.Advance(at.Add(2 * time.Minute))
	if err != nil || view.GameHour < last || len(reader.ClockIssues()) != 1 || !strings.Contains(reader.ClockIssues()[0], "authoritative game clock moved backwards") {
		t.Fatalf("raw clock regression must preserve monotone movement and explicitly invalidate trust: view=%+v issues=%v err=%v", view, reader.ClockIssues(), err)
	}
}

func TestTapeHiddenPauseIsExplicitlyUntrustedInsteadOfAborting(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	first := simAcceptanceFrame(t, at, 100, nil)
	second := simAcceptanceFrame(t, at.Add(10*time.Minute), 100, nil)
	clock := simAcceptanceJSON[engine.Clock](t, string(second.Clock))
	clock.Paused = true
	data, err := json.Marshal(clock)
	if err != nil {
		t.Fatal(err)
	}
	second.Clock, second.Datasets["clock"] = data, store.ReplayDataset{Data: data}
	tape := &Tape{Frames: []store.ReplayFrame{first, second}}
	reader := tape.Reader()
	before, err := reader.Advance(at.Add(5 * time.Minute))
	if err != nil || before.GameHour != 105 || before.State.Clock.Paused {
		t.Fatalf("future pause leaked before its sample: %+v, %v", before, err)
	}
	after, err := reader.Advance(second.ObservedAt)
	if err != nil || after.GameHour < before.GameHour || !after.State.Clock.Paused || len(reader.ClockIssues()) == 0 {
		t.Fatalf("hidden pause aborted or silently invented trusted time: %+v issues=%v err=%v", after, reader.ClockIssues(), err)
	}
	report, err := Validate(tape, at, second.ObservedAt.Add(time.Nanosecond))
	if err != nil || report.Trusted || len(report.EvidenceIssues) == 0 || !strings.Contains(strings.Join(report.FailedRules, ";"), "clock interpolation") {
		t.Fatalf("clock uncertainty did not invalidate the gate explicitly: %+v, %v", report, err)
	}
}

func TestTapeNewerFleetRowWinsOverStaleLastGoodDetail(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	detail := `{"ship":{"id":"ship","ship_class":"hoy","status":"berthed","current_port_id":"a"},"cargo_used_tons":9,"cargo_manifest":{"cloth":9},"effective_hold_capacity_tons":30}`
	for _, key := range []string{"ship_details/ship", "ship_details"} {
		t.Run(key, func(t *testing.T) {
			payload := detail
			if key == "ship_details" {
				payload = `{"ship":` + detail + `}`
			}
			first := simAcceptanceFrame(t, at, 100, map[string]string{
				"ships": `[{"ship":{"id":"ship","ship_class":"hoy","status":"berthed","current_port_id":"a"}}]`,
				key:     payload,
			})
			second := simAcceptanceFrame(t, at.Add(time.Minute), 101, map[string]string{
				"ships": `[{"ship":{"id":"ship","ship_class":"hoy","status":"berthed","current_port_id":"b"},"cargo_used_tons":4,"cargo_manifest":{"cloth":4}}]`,
				key:     payload,
			})
			failed := second.Datasets[key]
			failed.Meta = json.RawMessage(`{"complete":false,"stale":true,"error":"failed detail fetch"}`)
			second.Datasets[key] = failed
			reader := NewTapeReader([]store.ReplayFrame{first, second})
			initial, err := reader.Advance(at)
			if err != nil || len(initial.State.Ships) != 1 || initial.State.Ships[0].CargoUsedTons != 9 {
				t.Fatalf("same-time detailed hold was lost: %+v, %v", initial.State.Ships, err)
			}
			view, err := reader.Advance(second.ObservedAt)
			if err != nil || len(view.State.Ships) != 1 || view.State.Ships[0].Ship.CurrentPortID != "b" || view.State.Ships[0].CargoUsedTons != 4 {
				t.Fatalf("older detail rewound newer fleet state: %+v, %v", view.State.Ships, err)
			}
		})
	}
}

func TestTapeLastGoodRetentionBudgetFailsWithoutPartialReplacement(t *testing.T) {
	reader := NewTapeReader(nil)
	value := tapeValue{data: make(json.RawMessage, maxTapeRetainedBytes/2)}
	if err := reader.remember("first", value); err != nil {
		t.Fatal(err)
	}
	err := reader.remember("second", value)
	if !errors.Is(err, store.ErrReplayEvidenceBudget) || len(reader.values) != 1 {
		t.Fatalf("last-good aggregate overflow retained partial state or hid its reason: values=%d err=%v", len(reader.values), err)
	}
}
