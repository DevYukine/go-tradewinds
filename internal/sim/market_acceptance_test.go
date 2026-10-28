package sim

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func simAcceptanceJSON[T any](t *testing.T, value string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(value), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func simAcceptanceBook(t *testing.T, firstID string, firstQuantity int64) engine.Market {
	t.Helper()
	return simAcceptanceJSON[engine.Market](t, fmt.Sprintf(`{"port_id":"a","visibility":"quay","customs_bps":250,"books":[{"commodity_id":"cloth","depth":{"asks":[{"order_id":%q,"price_pence":90,"remaining_quantity":%d,"is_npc":true},{"order_id":"npc-expensive","price_pence":100,"remaining_quantity":101,"is_npc":true}],"bids":[{"order_id":"npc-bid","price_pence":80,"remaining_quantity":100,"is_npc":true}]}}]}`, firstID, firstQuantity))
}

func simAcceptanceView(t *testing.T, at time.Time, hour float64, id string) FrameView {
	t.Helper()
	return FrameView{At: at, GameHour: hour, Phase: 1, Snapshot: map[string]json.RawMessage{}, Books: map[string]BookObservation{"a": {Market: simAcceptanceBook(t, id, 66), Status: Observed, ObservedAt: at, AsOf: at}}}
}

func simAcceptanceAsk(t *testing.T, model *MarketModel, want int64) {
	t.Helper()
	book, coverage := model.Book("a")
	if coverage == Dark || len(book.Books) != 1 || book.Books[0].Depth == nil || len(book.Books[0].Depth.Asks) == 0 || book.Books[0].Depth.Asks[0].RemainingQuantity != want {
		t.Fatalf("shared residual book first rung, want %dt: book=%+v coverage=%s", want, book, coverage)
	}
}

func TestMarketAcceptanceIOCSharedDepthAndVariantIsolation(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	view := simAcceptanceView(t, at, 100, "npc-cheap")
	one, other := NewMarketModel(0.5), NewMarketModel(0.5)
	one.Observe(view)
	other.Observe(view)
	fill, err := one.Execute("a", "cloth", "bid", 30, 90)
	if err != nil || fill.Quantity != 30 || fill.GrossPence != 2_700 || fill.CustomsPence != 68 {
		t.Fatalf("IOC 30t@90 execution: %+v, %v", fill, err)
	}
	simAcceptanceAsk(t, one, 36)
	simAcceptanceAsk(t, other, 66)
	view.At = at.Add(time.Minute)
	view.GameHour = 101
	one.Observe(view)
	simAcceptanceAsk(t, one, 36)
	second, err := one.Execute("a", "cloth", "bid", 50, 90)
	if err != nil || second.Quantity != 36 || second.GrossPence != 3_240 {
		t.Fatalf("second ship double-spent first ship's depth or crossed its limit: %+v, %v", second, err)
	}
	simAcceptanceAsk(t, other, 66)
}

func TestMarketAcceptanceRequoteRegenerationRetainsStockDeviation(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		regen float64
		want  int64
	}{{0, 36}, {0.5, 51}, {1, 66}} {
		t.Run(fmt.Sprintf("regen_%g", tc.regen), func(t *testing.T) {
			model := NewMarketModel(tc.regen)
			model.Observe(simAcceptanceView(t, at, 100, "npc-old"))
			if _, err := model.Execute("a", "cloth", "bid", 30, 90); err != nil {
				t.Fatal(err)
			}
			model.Observe(simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-new"))
			simAcceptanceAsk(t, model, tc.want)
		})
	}
}

func TestMarketAcceptanceCustomsRoundPerExecutedLevel(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	view := simAcceptanceView(t, at, 100, "npc-cheap")
	obs := view.Books["a"]
	obs.Market.CustomsBPS = 1
	obs.Market.Books[0].Depth.Asks = []engine.Level{{OrderID: "tiny-1", IsNPC: true, PricePence: 1, RemainingQuantity: 1}, {OrderID: "tiny-2", IsNPC: true, PricePence: 2, RemainingQuantity: 1}}
	view.Books["a"] = obs
	model := NewMarketModel(0)
	model.Observe(view)
	fill, err := model.Execute("a", "cloth", "bid", 2, 2)
	if err != nil || fill.Quantity != 2 || fill.GrossPence != 3 || fill.CustomsPence != 2 {
		t.Fatalf("buyer customs must ceil each executed rung, not aggregate gross: %+v, %v", fill, err)
	}
	fill, err = model.Execute("a", "cloth", "ask", 1, 80)
	if err != nil || fill.CustomsPence != 0 {
		t.Fatalf("seller was charged buyer customs: %+v, %v", fill, err)
	}
}

func simAcceptanceFrame(t *testing.T, at time.Time, hour int, datasets map[string]string) store.ReplayFrame {
	t.Helper()
	generation := fmt.Sprintf("at-%d", at.UnixNano())
	frame := store.ReplayFrame{ObservedAt: at, Generation: generation, Phase: 1, Datasets: map[string]store.ReplayDataset{}}
	clock := fmt.Sprintf(`{"real_utc":%q,"unlocked_phase":1,"calendar":{"month":3,"day":5,"time_scale":60,"total_game_hours":%d}}`, at.Format(time.RFC3339Nano), hour)
	frame.Clock = json.RawMessage(clock)
	frame.Datasets["clock"] = store.ReplayDataset{Data: []byte(clock)}
	for key, data := range datasets {
		meta, err := json.Marshal(engine.DatasetMeta{Complete: true, Generation: generation, UpdatedAt: at, AttemptedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		frame.Datasets[key] = store.ReplayDataset{Data: []byte(data), Meta: meta}
	}
	return frame
}

func TestMarketAcceptanceTapeNeverReadsFutureAndExpiresBooks(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	first := simAcceptanceFrame(t, at, 100, map[string]string{"books/a": `{"port_id":"a","visibility":"quay","books":[]}`})
	future := simAcceptanceFrame(t, at.Add(7*time.Hour), 520, map[string]string{"books/future": `{"port_id":"future","visibility":"quay","books":[]}`})
	reader := NewTapeReader([]store.ReplayFrame{first, future})
	view, err := reader.Advance(at)
	if err != nil || view.Books["a"].Status != Observed {
		t.Fatalf("first observation: %+v, %v", view.Books, err)
	}
	view, err = reader.Advance(at.Add(3 * time.Minute))
	if err != nil || view.Books["a"].Status != Carried {
		t.Fatalf("carry forward: %+v, %v", view.Books, err)
	}
	view, err = reader.Advance(at.Add(6*time.Hour + time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if book, ok := view.Books["a"]; ok && book.Status != Dark {
		t.Fatalf("book carried past six real hours: %+v", book)
	}
	if book, ok := view.Books["future"]; ok && book.Status != Dark {
		t.Fatalf("future book leaked backward: %+v", book)
	}
}

func TestMarketAcceptanceRecordedFillsAddBackOnlyAfterAsOfAndBeforeRequote(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fillAt := at.Add(time.Minute)
	fill := fmt.Sprintf(`[{"id":"our-fill","order_id":"npc-cheap","port_id":"a","commodity_id":"cloth","side":"buy","quantity":20,"price_pence":90,"created_at":%q}]`, fillAt.Format(time.RFC3339Nano))
	model := NewMarketModel(0)
	view := simAcceptanceView(t, at, 100, "npc-cheap")
	view.Snapshot["fills"] = json.RawMessage(fill)
	model.Observe(view)
	simAcceptanceAsk(t, model, 66)
	view = simAcceptanceView(t, at.Add(2*time.Minute), 102, "npc-cheap")
	observed := view.Books["a"]
	observed.Market.Books[0].Depth.Asks[0].RemainingQuantity = 46
	view.Books["a"] = observed
	view.Snapshot["fills"] = json.RawMessage(fill)
	model.Observe(view)
	simAcceptanceAsk(t, model, 66)
	model.Observe(view)
	simAcceptanceAsk(t, model, 66)
	view = simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-requoted")
	view.Snapshot["fills"] = json.RawMessage(fill)
	model.Observe(view)
	// Skipping the recorded 20t trade leaves a 20t stock surplus at regen 0.
	simAcceptanceAsk(t, model, 86)
}

func TestMarketAcceptancePlayerDepletionDoesNotRegenerate(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	view := simAcceptanceView(t, at, 100, "player-order")
	obs := view.Books["a"]
	obs.Market.Books[0].Depth.Asks[0].IsNPC = false
	view.Books["a"] = obs
	model := NewMarketModel(1)
	model.Observe(view)
	if _, err := model.Execute("a", "cloth", "bid", 30, 90); err != nil {
		t.Fatal(err)
	}
	view.At, view.GameHour = at.Add(6*time.Minute), 106
	obs.Market.Books[0].Depth.Asks[1].OrderID = "new-npc-expensive"
	view.Books["a"] = obs
	model.Observe(view)
	simAcceptanceAsk(t, model, 36)
	view.At, view.GameHour = at.Add(12*time.Minute), 112
	obs.Market.Books[0].Depth.Asks[0].OrderID = "replacement-player"
	view.Books["a"] = obs
	model.Observe(view)
	simAcceptanceAsk(t, model, 66)
}

func TestMarketAcceptancePresenceMasksPrivatePortIntelligence(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	rawPort := `{"id":"a","effective_berths":4,"coffee_house_gossip":[{"subject_port_id":"b","commodities":[]}],"recent_sailings":[{"ship_id":"private-route"}]}`
	view := FrameView{At: at, State: engine.AccountState{
		Ports:   map[string]engine.PortDetail{"a": simAcceptanceJSON[engine.PortDetail](t, rawPort)},
		Markets: map[string]engine.Market{"a": simAcceptanceBook(t, "npc", 66)},
	}, Snapshot: map[string]json.RawMessage{"ports/a": json.RawMessage(rawPort)}}
	masked := view.VisibleTo(nil)
	if len(masked.State.Ports["a"].CoffeeHouseGossip) != 0 || masked.State.Markets["a"].Visibility == "quay" {
		t.Fatalf("absent ship/factor retained private intelligence: %+v", masked.State)
	}
	var hidden map[string]json.RawMessage
	if err := json.Unmarshal(masked.Snapshot["ports/a"], &hidden); err != nil {
		t.Fatal(err)
	}
	if len(hidden["coffee_house_gossip"]) != 0 || len(hidden["recent_sailings"]) != 0 {
		t.Fatalf("raw public projection leaked intelligence: %s", masked.Snapshot["ports/a"])
	}
	ship := simAcceptanceJSON[engine.ShipState](t, `{"ship":{"id":"own","current_port_id":"a","status":"berthed"}}`)
	if seen := view.VisibleTo([]engine.ShipState{ship}); len(seen.State.Ports["a"].CoffeeHouseGossip) != 1 || seen.State.Markets["a"].Visibility != "quay" {
		t.Fatal("own quay presence lost recorded intelligence")
	}
	view.State.Factors = []string{"a"}
	if seen := view.VisibleTo(nil); len(seen.State.Ports["a"].CoffeeHouseGossip) != 1 || seen.State.Markets["a"].Visibility != "quay" {
		t.Fatal("recorded factor lost historical intelligence")
	}
	if len(view.State.Ports["a"].CoffeeHouseGossip) != 1 {
		t.Fatal("visibility projection modified the shared tape view")
	}
}

func TestMarketAcceptancePublicViewsExpireAndRoutesStayInPhase(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	first := simAcceptanceFrame(t, at, 100, map[string]string{
		"ports/a":        `{"id":"a","effective_berths":4}`,
		"routes/a|b|hoy": `{"from_port":"a","to_port":"b","ship_class":"hoy","total_distance_nm":30}`,
	})
	nextPhase := simAcceptanceFrame(t, at.Add(6*time.Minute), 106, nil)
	nextPhase.Phase = 2
	reader := NewTapeReader([]store.ReplayFrame{first, nextPhase})
	view, err := reader.Advance(at.Add(5 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if view.Meta["ports/a"].Stale || !view.Meta["ports/a"].Complete {
		t.Fatal("five-minute public port view was discarded early")
	}
	view, err = reader.Advance(at.Add(5*time.Minute + time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if !view.Meta["ports/a"].Stale || view.Meta["ports/a"].Complete {
		t.Fatal("public port view remained current beyond five minutes")
	}
	view, err = reader.Advance(at.Add(6 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := view.State.Routes["a|b|hoy"]; exists {
		t.Fatal("route geometry from a different phase leaked into planning")
	}
}

func TestMarketAcceptanceRequoteFoldsRecordedAddbacksIntoSignedDeviation(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fill, err := json.Marshal([]RecordedFill{{ID: "recorded", OrderID: "npc-old", PortID: "a", CommodityID: "cloth", Side: "buy", Quantity: 20, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		regen float64
		skip  int64
	}{{0, 86}, {0.5, 76}, {1, 66}} {
		for _, replicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("regen_%g_replicate_%t", tc.regen, replicate), func(t *testing.T) {
				model := NewMarketModel(tc.regen)
				model.Observe(simAcceptanceView(t, at, 100, "npc-old"))
				if replicate {
					if trade, err := model.Execute("a", "cloth", "buy", 20, 90); err != nil || trade.Quantity != 20 {
						t.Fatalf("replicating trade: %+v, %v", trade, err)
					}
				}
				view := simAcceptanceView(t, at.Add(2*time.Minute), 102, "npc-old")
				book := view.Books["a"]
				book.Market.Books[0].Depth.Asks[0].RemainingQuantity = 46
				view.Books["a"], view.Snapshot["fills"] = book, fill
				model.Observe(view)
				want := int64(66)
				if replicate {
					want = 46
				}
				simAcceptanceAsk(t, model, want)
				view = simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-new")
				view.Snapshot["fills"] = fill
				model.Observe(view)
				want = tc.skip
				if replicate {
					want = 66
				}
				simAcceptanceAsk(t, model, want)
			})
		}
	}
}

func TestMarketAcceptanceFillFirstSeenWithRequoteFoldsIntoDeviation(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fill, err := json.Marshal([]RecordedFill{{ID: "late", OrderID: "npc-old", PortID: "a", CommodityID: "cloth", Side: "buy", Quantity: 20, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		regen float64
		skip  int64
	}{{0, 86}, {0.5, 76}, {1, 66}} {
		for _, replicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("regen_%g_replicate_%t", tc.regen, replicate), func(t *testing.T) {
				model := NewMarketModel(tc.regen)
				model.Observe(simAcceptanceView(t, at, 100, "npc-old"))
				if replicate {
					if trade, err := model.Execute("a", "cloth", "buy", 20, 90); err != nil || trade.Quantity != 20 {
						t.Fatalf("replicating trade: %+v, %v", trade, err)
					}
				}
				// The recorded fill only becomes visible together with the
				// re-quote that retires the order it was filled against, so
				// the fold must refresh the outgoing quote's add-backs first
				// instead of netting only the previous observation's.
				view := simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-new")
				view.Snapshot["fills"] = fill
				model.Observe(view)
				want := tc.skip
				if replicate {
					want = 66
				}
				simAcceptanceAsk(t, model, want)
			})
		}
	}
}

func TestMarketAcceptanceNewOrderFillPredatingEpochRestoresOnce(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	fill, err := json.Marshal([]RecordedFill{{ID: "early", OrderID: "npc-new", PortID: "a", CommodityID: "cloth", Side: "buy", Quantity: 20, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		regen float64
		skip  int64
	}{{0, 86}, {0.5, 71}, {1, 66}} {
		for _, replicate := range []bool{false, true} {
			t.Run(fmt.Sprintf("regen_%g_replicate_%t", tc.regen, replicate), func(t *testing.T) {
				model := NewMarketModel(tc.regen)
				view := simAcceptanceView(t, at, 100, "npc-outgoing")
				book := view.Books["a"]
				book.Market.Books[0].Depth.Asks = book.Market.Books[0].Depth.Asks[1:]
				view.Books["a"] = book
				model.Observe(view)
				// npc-new was never observed before this re-quote, yet the
				// recorded fill against its stable OrderID predates the new
				// AsOf, so the exact match must restore it despite the epoch.
				// The observed 46 already reflects the live account's 20t.
				view = simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-new")
				book = view.Books["a"]
				book.Market.Books[0].Depth.Asks[0].RemainingQuantity = 46
				view.Books["a"], view.Snapshot["fills"] = book, fill
				model.Observe(view)
				simAcceptanceAsk(t, model, 66)
				if replicate {
					if trade, err := model.Execute("a", "cloth", "buy", 20, 90); err != nil || trade.Quantity != 20 {
						t.Fatalf("replicating trade: %+v, %v", trade, err)
					}
					simAcceptanceAsk(t, model, 46)
				}
				view = simAcceptanceView(t, at.Add(12*time.Minute), 112, "npc-new")
				book = view.Books["a"]
				book.Market.Books[0].Depth.Asks[0].RemainingQuantity = 46
				view.Books["a"], view.Snapshot["fills"] = book, fill
				model.Observe(view)
				if replicate {
					simAcceptanceAsk(t, model, 46)
				} else {
					simAcceptanceAsk(t, model, 66)
				}
				// The next re-quote folds the restoration into the signed
				// stock deviation exactly once; crediting the folded fill
				// again onto npc-fresh would double-restore the same 20t.
				view = simAcceptanceView(t, at.Add(18*time.Minute), 118, "npc-fresh")
				view.Snapshot["fills"] = fill
				model.Observe(view)
				want := tc.skip
				if replicate {
					want = 66
				}
				simAcceptanceAsk(t, model, want)
			})
		}
	}
}

func TestMarketAcceptanceExhaustedNPCLevelDoesNotResetEpoch(t *testing.T) {
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	model := NewMarketModel(1)
	model.Observe(simAcceptanceView(t, at, 100, "npc-cheap"))
	if _, err := model.Execute("a", "cloth", "buy", 30, 90); err != nil {
		t.Fatal(err)
	}
	view := simAcceptanceView(t, at.Add(2*time.Minute), 102, "npc-cheap")
	book := view.Books["a"]
	book.Market.Books[0].Depth.Asks = book.Market.Books[0].Depth.Asks[1:]
	view.Books["a"] = book
	var err error
	view.Snapshot["fills"], err = json.Marshal([]RecordedFill{{ID: "recorded-exhaustion", OrderID: "npc-cheap", PortID: "a", CommodityID: "cloth", Side: "buy", Quantity: 66, PricePence: 90, CreatedAt: at.Add(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	model.Observe(view)
	// Same quote: 0 observed +66 recorded addback -30 variant tons =36.
	simAcceptanceAsk(t, model, 36)
	view = simAcceptanceView(t, at.Add(6*time.Minute), 106, "npc-replacement")
	model.Observe(view)
	simAcceptanceAsk(t, model, 66)
}
