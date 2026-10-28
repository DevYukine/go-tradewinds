package sim

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestRecordedETADoesNotBorrowAnotherVoyageOrFutureResponse(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	eta := at.Add(3 * time.Hour)
	completion := at.Add(2 * time.Hour)
	ship := json.RawMessage(`{"ship":{"id":"ship","ship_class":"hoy","status":"sailing"},"voyage":{"estimated_arrival_at":"` + eta.Format(time.RFC3339) + `"}}`)
	meta, err := json.Marshal(engine.DatasetMeta{Complete: true, UpdatedAt: completion})
	if err != nil {
		t.Fatal(err)
	}
	tape := &Tape{Frames: []store.ReplayFrame{
		{ObservedAt: at, Datasets: map[string]store.ReplayDataset{
			"ships":          {Data: json.RawMessage(`[{"ship":{"id":"ship","ship_class":"hoy","status":"berthed","current_port_id":"a"},"effective_hold_capacity_tons":40}]`), Meta: json.RawMessage(`{"complete":true}`)},
			"catalog":        {Data: json.RawMessage(`{"ship_classes":[{"id":"hoy","base_speed_centiknots":600,"hold_capacity_tons":40}]}`), Meta: json.RawMessage(`{"complete":true}`)},
			"routes/a|b|hoy": {Data: json.RawMessage(`{"from_port":"a","to_port":"b","ship_class":"hoy","total_distance_nm":240}`), Meta: json.RawMessage(`{"complete":true}`)},
		}},
		{ObservedAt: at.Add(time.Hour + time.Minute)},
		{ObservedAt: at.Add(90 * time.Minute), Datasets: map[string]store.ReplayDataset{"ship_details/ship": {Data: ship, Meta: meta}}},
	}}
	index, err := recordedETAs(tape, completion)
	if err != nil {
		t.Fatal(err)
	}
	if got := recordedETA(index["ship"], at, completion); !got.IsZero() {
		t.Fatalf("ETA response at the exclusive training boundary leaked: %s", got)
	}
	index, err = recordedETAs(tape, completion.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got := recordedETA(index["ship"], at, completion.Add(time.Minute)); !got.Equal(eta) {
		t.Fatalf("long voyage observation was discarded: got %s want %s", got, eta)
	}
	departure := store.Action{ID: "first-departure", Path: "/v1/fleet/depart", Status: 200, CreatedAt: at, Body: json.RawMessage(`{"ship_id":"ship","dest_port_id":"b"}`), Response: json.RawMessage(`{}`)}
	tape.Actions = []store.Action{departure}
	observations, err := voyageObservations(tape, at, completion.Add(time.Minute))
	if err != nil || len(observations) != 1 || observations[0].actionID != departure.ID {
		t.Fatalf("fallback did not associate first voyage: %+v, %v", observations, err)
	}
	next := departure
	next.ID, next.CreatedAt = "next-departure", at.Add(time.Hour)
	tape.Actions = append(tape.Actions, next)
	observations, err = voyageObservations(tape, at, completion.Add(time.Minute))
	if err != nil || len(observations) != 1 || observations[0].actionID != next.ID {
		t.Fatalf("borrowed later voyage ETA for first departure: %+v, %v", observations, err)
	}
}

func TestRegenerationIgnoresExhaustedOrderRemovalUntilNewNPCQuote(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fills, err := json.Marshal([]RecordedFill{{ID: "exhaust", OrderID: "npc-cheap", PortID: "a", CommodityID: "cloth", Side: "buy", Quantity: 66, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	frame := func(offset time.Duration, book engine.Market, includeFill bool) store.ReplayFrame {
		data, err := json.Marshal(book)
		if err != nil {
			t.Fatal(err)
		}
		datasets := map[string]string{"books/a": string(data)}
		if includeFill {
			datasets["fills"] = string(fills)
		}
		return simAcceptanceFrame(t, at.Add(offset), 100+int(offset/time.Minute), datasets)
	}
	before := simAcceptanceBook(t, "npc-cheap", 66)
	exhausted := simAcceptanceBook(t, "npc-cheap", 66)
	exhausted.Books[0].Depth.Asks = exhausted.Books[0].Depth.Asks[1:]
	tape := &Tape{Frames: []store.ReplayFrame{
		frame(0, before, false),
		frame(2*time.Minute, exhausted, true),
		frame(6*time.Minute, simAcceptanceBook(t, "npc-new", 66), true),
	}}
	samples, err := regenerationSamples(tape, at, at.Add(7*time.Minute))
	if err != nil || len(samples) != 1 || samples[0] != 1 {
		t.Fatalf("exhaustion was mistaken for a zero-regeneration re-quote: %v, %v", samples, err)
	}
}

func TestRegenerationSamplesStreamLongWindowsWithoutRetainingEveryObservation(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	quietBook := func(firstID string, firstQuantity int64) string {
		return fmt.Sprintf(`{"port_id":"quiet","visibility":"quay","customs_bps":250,"books":[{"commodity_id":"cloth","depth":{"asks":[{"order_id":%q,"price_pence":90,"remaining_quantity":%d,"is_npc":true},{"order_id":"npc-expensive","price_pence":100,"remaining_quantity":101,"is_npc":true}],"bids":[{"order_id":"npc-bid","price_pence":80,"remaining_quantity":100,"is_npc":true}]}}]}`, firstID, firstQuantity)
	}
	// Each unrelated observation charges its full level sizes against the
	// retention budget, so sixty-four of them exceed the 64 MiB cap if the
	// scan keeps every side observation instead of streaming past them.
	noise := strings.Repeat("n", 560_000)
	loudBook := fmt.Sprintf(`{"port_id":"loud","visibility":"quay","customs_bps":0,"books":[{"commodity_id":"grain","depth":{"asks":[{"order_id":%q,"price_pence":10,"remaining_quantity":7,"is_npc":true}],"bids":[{"order_id":%q,"price_pence":9,"remaining_quantity":7,"is_npc":true}]}}]}`, noise, noise)
	fills, err := json.Marshal([]RecordedFill{{ID: "quiet-fill", OrderID: "npc-before", PortID: "quiet", CommodityID: "cloth", Side: "buy", Quantity: 30, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	frames := make([]store.ReplayFrame, 0, 64)
	for i := range 64 {
		datasets := map[string]string{"books/loud": loudBook}
		switch i {
		case 0:
			datasets["books/quiet"] = quietBook("npc-before", 66)
		case 2:
			datasets["fills"] = string(fills)
		case 6:
			datasets["books/quiet"] = quietBook("npc-after", 51)
		}
		frames = append(frames, simAcceptanceFrame(t, at.Add(time.Duration(i)*time.Minute), 100+i, datasets))
	}
	samples, err := regenerationSamples(&Tape{Frames: frames}, at, at.Add(64*time.Minute))
	if err != nil {
		t.Fatalf("a long fit window of unrelated book observations exceeded the retention budget: %v", err)
	}
	if len(samples) != 1 || samples[0] != 0.5 {
		t.Fatalf("streaming scan lost the single eligible re-quote pair: %v, %v", samples, err)
	}
}
