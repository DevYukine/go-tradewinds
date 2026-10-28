package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func retainedBookInput(t *testing.T, age float64, bids ...RetainedBid) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	delete(in.Markets, "portsmouth")
	delete(in.Metadata, "books/portsmouth")
	in.QuayBookMemory = map[string]RetainedQuayBook{
		"portsmouth": {
			ObservedAt: in.Now.Add(-gameDuration(age, in.Clock.Calendar.TimeScale)),
			Bids:       map[string][]RetainedBid{"provisions": bids},
		},
	}
	return in
}

func TestQuayBookMemoryCapturePredicateKeepsLastGood(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*DatasetMeta, *Market)
		good   bool
	}{
		{name: "current complete quay", good: true},
		{name: "previous generation", change: func(meta *DatasetMeta, _ *Market) { meta.Generation = "old" }},
		{name: "incomplete", change: func(meta *DatasetMeta, _ *Market) { meta.Complete = false }},
		{name: "stale", change: func(meta *DatasetMeta, _ *Market) { meta.Stale = true }},
		{name: "failed", change: func(meta *DatasetMeta, _ *Market) { meta.Error = "book capture failed" }},
		{name: "gossip visibility", change: func(_ *DatasetMeta, book *Market) { book.Visibility = "gossip" }},
		{name: "same observation", change: func(meta *DatasetMeta, _ *Market) { meta.UpdatedAt = meta.UpdatedAt.Add(-time.Hour) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := retainedBookInput(t, 2.5, RetainedBid{PricePence: 1_000, RemainingQuantity: 20})
			previous := in.QuayBookMemory["portsmouth"]
			meta := sourcingFreshMeta(in.Now)
			book := sourcingFixture[Market](t, `{"port_id":"portsmouth","visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":900,"remaining_quantity":25}]}}]}`)
			if tc.change != nil {
				tc.change(&meta, &book)
			}
			runner := Runner{
				generation: in.Generation,
				meta:       map[string]DatasetMeta{"books/portsmouth": meta},
				snapshot: map[string]json.RawMessage{
					"_book_observed":    encode(map[string]time.Time{"portsmouth": previous.ObservedAt}),
					"_quay_book_memory": encode(in.QuayBookMemory),
				},
			}
			runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
			memory := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")
			observed := decode[map[string]time.Time](runner.snapshot, "_book_observed")
			drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
			if tc.good {
				if memory["portsmouth"].Bids["provisions"][0].PricePence != 900 || !observed["portsmouth"].Equal(in.Now) || !memory["portsmouth"].ObservedAt.Equal(in.Now) {
					t.Fatalf("complete quay capture was not retained: %+v; observed %+v", memory, observed)
				}
				if len(drift) != 1 || drift[0].PortID != "portsmouth" || drift[0].CommodityID != "provisions" || drift[0].AgeGameDays != 2.5 || drift[0].TopBidChangeBPS != -1_000 {
					t.Fatalf("replacement lost measured adverse drift: %+v", drift)
				}
			} else if !reflect.DeepEqual(memory["portsmouth"], previous) || !observed["portsmouth"].Equal(previous.ObservedAt) || len(drift) != 0 {
				t.Fatalf("failed capture overwrote last good book: %+v; observed %+v; drift %+v", memory, observed, drift)
			}
		})
	}
}

func TestQuayBookMemoryExcludesOwnOrdersAndKeepsBestTen(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	var levels []Level
	for price := int64(1); price <= 15; price++ {
		levels = append(levels, Level{OrderID: fmt.Sprintf("external-%d", price), PricePence: price, RemainingQuantity: 20})
	}
	levels = append(levels,
		Level{OrderID: "our-bid", PricePence: 10_000, RemainingQuantity: 50},
		Level{PricePence: 20_000, RemainingQuantity: 0},
		Level{PricePence: -1, RemainingQuantity: 20},
	)
	book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{}}]}`)
	book.Books[0].Depth.Bids = levels
	runner := Runner{
		generation: in.Generation,
		meta:       map[string]DatasetMeta{"books/portsmouth": sourcingFreshMeta(in.Now)},
		snapshot:   map[string]json.RawMessage{"orders": json.RawMessage(`[{"id":"our-bid"}]`)},
	}
	runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
	memory := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")
	bids := memory["portsmouth"].Bids["provisions"]
	if len(bids) != 10 {
		t.Fatalf("retained %d bid levels, want ten", len(bids))
	}
	for i, bid := range bids {
		if bid.PricePence != int64(15-i) || bid.RemainingQuantity != 20 {
			t.Fatalf("own order, invalid depth or worse bid survived: %+v", bids)
		}
	}
	// Losing quay presence prunes live books, not the last usable memory.
	runner.recordQuayBooks(nil, nil, in.Catalog, in.Clock, in.Now.Add(time.Minute))
	if !reflect.DeepEqual(memory, decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")) {
		t.Fatal("memory disappeared when the port was no longer requested")
	}
}

func TestQuayBookMemoryAgeBoundaryAndPruning(t *testing.T) {
	t.Parallel()
	for _, extraAge := range []time.Duration{0, time.Second / 60, time.Second} {
		t.Run(extraAge.String(), func(t *testing.T) {
			in := retainedBookInput(t, 12, RetainedBid{PricePence: 5_000, RemainingQuantity: 20})
			book := in.QuayBookMemory["portsmouth"]
			book.ObservedAt = book.ObservedAt.Add(-extraAge)
			in.QuayBookMemory["portsmouth"] = book
			_, _, _, _, reason := destinationRevenue(in, "portsmouth", "provisions", 20)
			if (reason == "") != (extraAge == 0) {
				t.Fatalf("12 game day pricing boundary changed at +%s: %s", extraAge, reason)
			}
			runner := Runner{snapshot: map[string]json.RawMessage{"_quay_book_memory": encode(in.QuayBookMemory)}}
			runner.recordQuayBooks(nil, nil, in.Catalog, in.Clock, in.Now)
			_, exists := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")["portsmouth"]
			if exists != (extraAge == 0) {
				t.Fatalf("12 game day prune boundary changed at +%s", extraAge)
			}
		})
	}
}

func TestQuayBookMemoryAgeHaircutAndStressPence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		age     float64
		haircut int64
		revenue int64
		stress  int64
		limit   int64
	}{
		{age: 0.5, haircut: 50, revenue: 995_000, stress: 35_000, limit: 960_000},
		{age: 6, haircut: 600, revenue: 940_000, stress: 90_000, limit: 850_000},
		{age: 12, haircut: 1_200, revenue: 880_000, stress: 150_000, limit: 730_000},
	} {
		t.Run(fmt.Sprintf("%.1f game days", tc.age), func(t *testing.T) {
			in := retainedBookInput(t, tc.age, RetainedBid{PricePence: 1_000_001, RemainingQuantity: 20})
			revenue, limit, stress, source, reason := destinationRevenue(in, "portsmouth", "provisions", 1)
			wantSource := fmt.Sprintf("our quay book %.1f game days old; %d bps age haircut", tc.age, tc.haircut)
			if reason != "" || revenue != tc.revenue || stress != tc.stress || limit != tc.limit || source != wantSource {
				t.Fatalf("wrong aged value: revenue %dp, limit %dp, stress %dp, source %q, reason %q", revenue, limit, stress, source, reason)
			}
		})
	}
}

func TestQuayBookMemoryReservationsConsumeBestBids(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 6,
		RetainedBid{PricePence: 4_000, RemainingQuantity: 30},
		RetainedBid{PricePence: 5_000, RemainingQuantity: 20},
	)
	before := encode(in.QuayBookMemory)
	in.ReservedRetainedBidTons = map[string]map[string]int64{"portsmouth": {"provisions": 20}}
	revenue, limit, stress, _, reason := destinationRevenue(in, "portsmouth", "provisions", 20)
	if reason != "" || revenue != 75_200 || limit != 3_400 || stress != 7_200 {
		t.Fatalf("second ship reused the first ship's best twenty tons: %dp, %dp, %dp, %q", revenue, limit, stress, reason)
	}
	if string(encode(in.QuayBookMemory)) != string(before) {
		t.Fatal("planning depleted the persisted retained depth")
	}
	in.ReservedRetainedBidTons["portsmouth"]["provisions"] = 31
	if _, _, _, _, reason := destinationRevenue(in, "portsmouth", "provisions", 20); reason == "" {
		t.Fatal("insufficient remaining depth was extrapolated")
	}
}

func TestQuayBookMemoryRevenuePrecedence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		quantity   int64
		playerTons int64
		gossipAge  time.Duration
		fresh      bool
		shortFresh bool
		noMemory   bool
		wantSource string
		wantReject bool
	}{
		{name: "fresh depth", quantity: 20, playerTons: 20, fresh: true, wantSource: "observed destination full depth"},
		{name: "short fresh depth rejects", quantity: 20, playerTons: 20, fresh: true, shortFresh: true, wantReject: true},
		{name: "player quantity before retained at six game hours", quantity: 20, playerTons: 20, gossipAge: 6 * time.Minute, wantSource: playerBidGossipSource},
		{name: "old player quantity loses to retained", quantity: 20, playerTons: 20, gossipAge: 6*time.Minute + time.Second, wantSource: retainedBookSource},
		{name: "short player quantity", quantity: 20, playerTons: 19, wantSource: retainedBookSource},
		{name: "retained before one ton probe", quantity: 1, wantSource: retainedBookSource},
		{name: "one ton probe without retained", quantity: 1, noMemory: true, wantSource: "recent observed gossip; one-ton probe; unverified bid size"},
		{name: "no extrapolation beyond probe", quantity: 2, noMemory: true, wantReject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := retainedBookInput(t, 6, RetainedBid{PricePence: 5_000, RemainingQuantity: 20})
			if tc.noMemory {
				in.QuayBookMemory = nil
			}
			if tc.fresh {
				in.Metadata["books/portsmouth"] = sourcingFreshMeta(in.Now)
				book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":7000,"remaining_quantity":20}]}}]}`)
				if tc.shortFresh {
					book.Books[0].Depth.Bids[0].RemainingQuantity = 19
				}
				in.Markets["portsmouth"] = book
			}
			npcBid, playerBid := int64(6_000), int64(5_500)
			at := in.Now.Add(-tc.gossipAge)
			home := in.Ports["le_havre"]
			home.CoffeeHouseGossip = []Gossip{{SubjectPortID: "portsmouth", SnapshotAt: at, Commodities: []GossipCommodity{{
				CommodityID: "provisions", SnapshotAt: at, BestBidPence: &npcBid,
				BestPlayerBidPence: &playerBid, BestPlayerBidQuantity: &tc.playerTons,
			}}}}
			in.Ports["le_havre"] = home
			_, _, _, source, reason := destinationRevenue(in, "portsmouth", "provisions", tc.quantity)
			if (reason != "") != tc.wantReject || !strings.HasPrefix(source, tc.wantSource) {
				t.Fatalf("pricing precedence changed: source %q, reason %q", source, reason)
			}
		})
	}
}

func TestQuayBookMemoryStressGateAtZeroAndOnePenny(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		dues     int64
		accepted bool
		net      int64
	}{
		{dues: 667, net: 180},
		{dues: 666, accepted: true, net: 181},
	} {
		t.Run(fmt.Sprintf("%dp harbour expense", tc.dues), func(t *testing.T) {
			in := retainedBookInput(t, 6, RetainedBid{PricePence: 2_000, RemainingQuantity: 20})
			in.Markets["le_havre"] = sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"asks":[{"price_pence":1000,"remaining_quantity":100}]}}]}`)
			in.Ports["le_havre"] = PortDetail{ID: "le_havre", EffectiveBerths: 4}
			in.Ports["portsmouth"] = PortDetail{ID: "portsmouth", EffectiveBerths: 4, BerthDuesPerDayPence: 4 * tc.dues}
			plan := evaluatePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], Plan{
				Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: "portsmouth", Commodity: "provisions", Quantity: 1,
			}, 0, 0)
			if plan.Accepted != tc.accepted || plan.Economics.ExpectedNetPence != tc.net || plan.Economics.PriceUncertaintyStressPence != 180 || plan.Economics.RevenueBasis != "retained" {
				t.Fatalf("retained stress floor changed: %+v", plan)
			}
			if plan.Economics.StressNetPence >= 0 || plan.Economics.BallastRepeatNetPence != 0 || plan.Economics.BallastRepeatGameDays != 0 {
				t.Fatalf("diagnostic hazard gated cash or retained demand financed a repeat: %+v", plan.Economics)
			}
			if tc.accepted {
				if revised := addFundingFees(plan, 1); revised.Accepted {
					t.Fatalf("funding fee consumed the last stressed penny without rejecting: %+v", revised)
				}
				if revised := addFundingFees(plan, 0); !revised.Accepted {
					t.Fatalf("one stressed penny was not accepted: %+v", revised)
				}
			}
		})
	}
}

func TestQuayBookMemoryMeasuredP75StartsAtThirty(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 6, RetainedBid{PricePence: 100_000, RemainingQuantity: 20})
	for range 29 {
		in.BookDrift = append(in.BookDrift, BookDriftSample{AgeGameDays: 2, TopBidChangeBPS: -800})
	}
	revenue, _, _, source, reason := destinationRevenue(in, "portsmouth", "provisions", 1)
	if reason != "" || revenue != 94_000 || !strings.HasSuffix(source, "600 bps age haircut") {
		t.Fatalf("29 samples changed the floor: %dp, %q, %q", revenue, source, reason)
	}
	in.BookDrift = append(in.BookDrift, BookDriftSample{AgeGameDays: 2, TopBidChangeBPS: -800})
	revenue, limit, stress, source, reason := destinationRevenue(in, "portsmouth", "provisions", 1)
	if reason != "" || revenue != 76_000 || limit != 49_000 || stress != 27_000 || !strings.HasSuffix(source, "2400 bps age haircut") {
		t.Fatalf("thirty samples did not use measured adverse drift per day: %dp, %dp, %dp, %q, %q", revenue, limit, stress, source, reason)
	}
	// The 75th percentile is not the maximum observed adverse drift.
	for i := range 23 {
		in.BookDrift[i].TopBidChangeBPS = 200
	}
	if rate := retainedAdverseDriftBPS(in.BookDrift); rate != 100 {
		t.Fatalf("rising bids or the worst tail raised the p75 floor: %v", rate)
	}
	for i := range 8 {
		in.BookDrift[i].TopBidChangeBPS = -800
	}
	if rate := retainedAdverseDriftBPS(in.BookDrift); rate != 400 {
		t.Fatalf("nearest-rank p75 did not select the adverse observations: %v", rate)
	}
	in.BookDrift = nil
	for range 30 {
		in.BookDrift = append(in.BookDrift, BookDriftSample{AgeGameDays: 2, TopBidChangeBPS: -2_000})
	}
	_, limit, _, _, reason = destinationRevenue(in, "portsmouth", "provisions", 1)
	if reason != "" || limit != 40_000 {
		t.Fatalf("retained price stress was not capped at 6000 bps: limit %dp, reason %q", limit, reason)
	}
}

func TestQuayBookMemoryDriftRetainsLatestTwoHundred(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 6, RetainedBid{PricePence: 1_000, RemainingQuantity: 20})
	var history []BookDriftSample
	for i := range 200 {
		history = append(history, BookDriftSample{PortID: fmt.Sprintf("old-%d", i), AgeGameDays: 1, TopBidChangeBPS: -100})
	}
	runner := Runner{
		generation: in.Generation,
		meta:       map[string]DatasetMeta{"books/portsmouth": sourcingFreshMeta(in.Now)},
		snapshot: map[string]json.RawMessage{
			"_quay_book_memory": encode(in.QuayBookMemory),
			"_book_drift":       encode(history),
		},
	}
	book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":900,"remaining_quantity":20}]}}]}`)
	markets := map[string]json.RawMessage{"portsmouth": encode(book)}
	runner.recordQuayBooks(markets, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
	drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
	if len(drift) != 200 || drift[0].PortID != "old-1" || drift[199].PortID != "portsmouth" || drift[199].AgeGameDays != 6 || drift[199].TopBidChangeBPS != -1_000 {
		t.Fatalf("drift retention or signed measured change is wrong: first %+v, last %+v, count %d", drift[0], drift[len(drift)-1], len(drift))
	}
	runner.recordQuayBooks(markets, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
	if !reflect.DeepEqual(drift, decode[[]BookDriftSample](runner.snapshot, "_book_drift")) {
		t.Fatal("the same observation was appended twice")
	}
}

func TestQuayBookMemoryEncodingFitsSnapshotBudget(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	runner := Runner{generation: in.Generation, meta: make(map[string]DatasetMeta), snapshot: make(map[string]json.RawMessage)}
	markets := make(map[string]json.RawMessage)
	var ports []string
	for port := range 30 {
		id := fmt.Sprintf("port-%02d-%050d", port, port)
		ports = append(ports, id)
		book := Market{PortID: id, Visibility: "quay"}
		for commodity := range 30 {
			entry := sourcingFixture[CommodityBook](t, `{"depth":{}}`)
			entry.CommodityID = fmt.Sprintf("commodity-%02d-%050d", commodity, commodity)
			for level := range 10 {
				entry.Depth.Bids = append(entry.Depth.Bids, Level{OrderID: fmt.Sprintf("external-order-%050d", level), PricePence: maximumPence - int64(level), RemainingQuantity: math.MaxInt64})
			}
			book.Books = append(book.Books, entry)
		}
		markets[id] = encode(book)
		runner.meta["books/"+id] = sourcingFreshMeta(in.Now)
	}
	runner.recordQuayBooks(markets, ports, in.Catalog, in.Clock, in.Now)
	if size := len(runner.snapshot["_quay_book_memory"]); size > 512<<10 {
		t.Fatalf("30x30x10 retained bids need %d bytes, exceeding 512 KiB", size)
	}
	memory := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")
	if len(memory) != 30 || len(memory[ports[0]].Bids) != 30 {
		t.Fatalf("size bound was achieved by dropping required depth: %d ports, %d commodities", len(memory), len(memory[ports[0]].Bids))
	}
}

func TestQuayBookMemoryRunnerReservesUnsettledMissionsNotOwnBids(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	runner.generation = "retained-reservation-generation"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	data := missionData{Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain, Quantity: 20}, DeliveredQuantity: 5}
	h.putMission(recoveryShipCharizard, "retained-reservation", "sailing", data)
	runner.snapshot["orders"] = json.RawMessage(`[{"side":"bid","port_id":"portb","commodity_id":"grain","quantity":100,"filled_quantity":0}]`)
	runner.snapshot["_quay_book_memory"] = encode(map[string]RetainedQuayBook{
		recoveryFarPort: {ObservedAt: time.Now().Add(-time.Hour), Bids: map[string][]RetainedBid{recoveryGrain: {{PricePence: 1_000, RemainingQuantity: 20}}}},
	})
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("first ship is missing")
	}
	input, err := runner.committedPlanningInputExcept(h.ctx, ship, "")
	if err != nil {
		t.Fatal(err)
	}
	if input.ReservedRetainedBidTons[recoveryFarPort][recoveryGrain] != 15 || len(input.QuayBookMemory) != 1 {
		t.Fatalf("unsettled remaining tons or retained runner input lost: %+v", input.ReservedRetainedBidTons)
	}
	delete(input.Metadata, "books/"+recoveryFarPort)
	if _, _, _, _, reason := destinationRevenue(input, recoveryFarPort, recoveryGrain, 6); reason == "" {
		t.Fatal("second ship reused the first ship's reserved bids")
	}
	if _, _, _, source, reason := destinationRevenue(input, recoveryFarPort, recoveryGrain, 5); reason != "" || !strings.HasPrefix(source, retainedBookSource) {
		t.Fatalf("own resting bids incorrectly consumed retained external depth: source %q, reason %q", source, reason)
	}
	data.Settlement = &store.Settlement{}
	h.putMission(recoveryShipCharizard, "retained-reservation", "settled", data)
	input, err = runner.committedPlanningInputExcept(h.ctx, ship, "")
	if err != nil || input.ReservedRetainedBidTons[recoveryFarPort][recoveryGrain] != 0 {
		t.Fatalf("settled mission retained a demand reservation: %+v, %v", input.ReservedRetainedBidTons, err)
	}
}

func TestQuayBookMemoryDriftRequiresHalfGameDay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		shortBy time.Duration
		want    int
	}{
		{name: "half day minus one nanosecond", shortBy: time.Nanosecond},
		{name: "exactly half a game day", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := retainedBookInput(t, 0.5, RetainedBid{PricePence: 1_000, RemainingQuantity: 20})
			at := in.Now.Add(-tc.shortBy)
			runner := Runner{
				generation: in.Generation,
				meta:       map[string]DatasetMeta{"books/portsmouth": sourcingFreshMeta(at)},
				snapshot:   map[string]json.RawMessage{"_quay_book_memory": encode(in.QuayBookMemory)},
			}
			book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":900,"remaining_quantity":20}]}}]}`)
			runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, at)
			drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
			if len(drift) != tc.want {
				t.Fatalf("sample interval boundary: got %+v, want %d samples", drift, tc.want)
			}
			latest := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")["portsmouth"]
			if !latest.ObservedAt.Equal(at) || latest.Bids["provisions"][0].PricePence != 900 {
				t.Fatalf("minimum drift age blocked the current book capture: %+v", latest)
			}
		})
	}
}

func TestQuayBookMemoryPassRefetchesDoNotInflateP75(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 0, RetainedBid{PricePence: 1_000, RemainingQuantity: 20})
	runner := Runner{
		generation: in.Generation,
		meta:       make(map[string]DatasetMeta),
		snapshot:   map[string]json.RawMessage{"_quay_book_memory": encode(in.QuayBookMemory)},
	}
	book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":1000,"remaining_quantity":20}]}}]}`)
	for pass := range 40 {
		at := in.Now.Add(time.Duration(pass+1) * 72 * time.Second)
		book.Books[0].Depth.Bids[0].PricePence--
		runner.meta["books/portsmouth"] = sourcingFreshMeta(at)
		runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, at)
	}
	drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
	if len(drift) != 0 || retainedAdverseDriftBPS(drift) != 100 {
		t.Fatalf("forty 0.05-game-day refetches became adverse drift evidence: %+v", drift)
	}
	// Previously persisted per-pass samples must not keep the bad estimate
	// alive after deployment or count toward the thirty-sample floor.
	for range 170 {
		drift = append(drift, BookDriftSample{AgeGameDays: 0.05, TopBidChangeBPS: -100})
	}
	for range 29 {
		drift = append(drift, BookDriftSample{AgeGameDays: 0.5, TopBidChangeBPS: -200})
	}
	if rate := retainedAdverseDriftBPS(drift); rate != 100 {
		t.Fatalf("legacy per-pass samples inflated p75 or the sample count: %v", rate)
	}
	drift = append(drift, BookDriftSample{AgeGameDays: 0.5, TopBidChangeBPS: -200})
	if rate := retainedAdverseDriftBPS(drift); rate != 400 {
		t.Fatalf("thirty independent half-day samples were lost: %v", rate)
	}
}

func TestQuayBookMemoryDriftExcludesOurBidConsumption(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		change func(*PlayerBidSellFill, time.Time, time.Time)
		want   int
	}{
		{name: "our sell inside interval"},
		{name: "our sell at start", change: func(fill *PlayerBidSellFill, start, _ time.Time) { fill.CreatedAt = start }},
		{name: "our sell at end", change: func(fill *PlayerBidSellFill, _, end time.Time) { fill.CreatedAt = end }},
		{name: "old sell already reflected", change: func(fill *PlayerBidSellFill, start, _ time.Time) { fill.CreatedAt = start.Add(-time.Nanosecond) }, want: 1},
		{name: "later sell", change: func(fill *PlayerBidSellFill, _, end time.Time) { fill.CreatedAt = end.Add(time.Nanosecond) }, want: 1},
		{name: "other port", change: func(fill *PlayerBidSellFill, _, _ time.Time) { fill.PortID = "amsterdam" }, want: 1},
		{name: "other commodity", change: func(fill *PlayerBidSellFill, _, _ time.Time) { fill.CommodityID = "coarse_cloth" }, want: 1},
		{name: "buy consumes asks not bids", change: func(fill *PlayerBidSellFill, _, _ time.Time) { fill.Side = "bid" }, want: 1},
		{name: "zero quantity", change: func(fill *PlayerBidSellFill, _, _ time.Time) { fill.Quantity = 0 }, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := retainedBookInput(t, 0.5, RetainedBid{PricePence: 1_000, RemainingQuantity: 20})
			start := in.QuayBookMemory["portsmouth"].ObservedAt
			fill := PlayerBidSellFill{
				PortID: "portsmouth", CommodityID: "provisions", Side: "ask", Quantity: 20,
				CreatedAt: start.Add(6 * time.Minute),
			}
			if tc.change != nil {
				tc.change(&fill, start, in.Now)
			}
			runner := Runner{
				generation: in.Generation,
				meta:       map[string]DatasetMeta{"books/portsmouth": sourcingFreshMeta(in.Now)},
				snapshot: map[string]json.RawMessage{
					"_quay_book_memory": encode(in.QuayBookMemory),
					"fills":             encode([]PlayerBidSellFill{fill}),
				},
			}
			book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":900,"remaining_quantity":20}]}}]}`)
			runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
			drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
			if len(drift) != tc.want {
				t.Fatalf("own bid consumption contaminated market drift, or unrelated fills suppressed it: %+v", drift)
			}
		})
	}
}

func TestQuayBookMemoryRejectsHaircutBeyondStressCap(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rate int64
		want bool
	}{
		{rate: 500, want: true},
		{rate: 501},
		{rate: 800},
	} {
		t.Run(fmt.Sprintf("%d bps per game day", tc.rate), func(t *testing.T) {
			in := retainedBookInput(t, 12, RetainedBid{PricePence: 100_000, RemainingQuantity: 20})
			for range 30 {
				in.BookDrift = append(in.BookDrift, BookDriftSample{AgeGameDays: 1, TopBidChangeBPS: -tc.rate})
			}
			revenue, limit, stress, source, reason := destinationRevenue(in, "portsmouth", "provisions", 20)
			if (reason == "") != tc.want {
				t.Fatalf("age haircut above the stress cap remained usable: revenue %dp, limit %dp, stress %dp, source %q, reason %q", revenue, limit, stress, source, reason)
			}
			if tc.want && (revenue != 800_000 || limit != 40_000 || stress != 0) {
				t.Fatalf("6000 bps boundary lost a nonnegative stress reserve: revenue %dp, limit %dp, stress %dp", revenue, limit, stress)
			}
		})
	}
}

func TestQuayBookMemoryCapsRevenueAtFresherThinBids(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		quay      bool
		age       time.Duration
		memoryAge float64
		price     int64
		wantGross int64
		reject    bool
	}{
		{name: "thin player gossip lowers retained value", age: time.Minute, memoryAge: 6, price: 4_000, wantGross: 80_000},
		{name: "gossip at six game hours", age: 6 * time.Minute, memoryAge: 6, price: 4_000, wantGross: 80_000},
		{name: "gossip beyond six game hours", age: 6*time.Minute + time.Nanosecond, memoryAge: 6, price: 4_000, wantGross: 100_000},
		{name: "older gossip cannot override newer memory", age: 3 * time.Minute, memoryAge: 0.1, price: 4_000, wantGross: 100_000},
		{name: "higher gossip cannot raise remembered bids", age: time.Minute, memoryAge: 6, price: 7_000, wantGross: 100_000},
		{name: "thin quay observation within six game hours", quay: true, age: 3 * time.Minute, memoryAge: 6, price: 4_000, wantGross: 80_000},
		{name: "quay at six game hours", quay: true, age: 6 * time.Minute, memoryAge: 6, price: 4_000, wantGross: 80_000},
		{name: "quay beyond six game hours", quay: true, age: 6*time.Minute + time.Nanosecond, memoryAge: 6, price: 4_000, wantGross: 100_000},
		{name: "current thin quay still rejects missing depth", quay: true, age: time.Minute, memoryAge: 6, price: 4_000, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := retainedBookInput(t, tc.memoryAge, RetainedBid{PricePence: 5_000, RemainingQuantity: 20})
			at := in.Now.Add(-tc.age)
			if tc.quay {
				book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":4000,"remaining_quantity":1}]}}]}`)
				book.Books[0].Depth.Bids[0].PricePence = tc.price
				in.Markets["portsmouth"] = book
				in.Metadata["books/portsmouth"] = sourcingFreshMeta(at)
			} else {
				playerBid, playerTons := tc.price-100, int64(1)
				detail := in.Ports["le_havre"]
				detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "portsmouth", SnapshotAt: at, Commodities: []GossipCommodity{{
					CommodityID: "provisions", SnapshotAt: at, BestBidPence: &tc.price,
					BestPlayerBidPence: &playerBid, BestPlayerBidQuantity: &playerTons,
				}}}}
				in.Ports["le_havre"] = detail
			}
			revenue, limit, stress, source, reason := destinationRevenue(in, "portsmouth", "provisions", 20)
			if tc.reject {
				if reason == "" {
					t.Fatalf("current full depth was extrapolated: revenue %dp, source %q", revenue, source)
				}
				return
			}
			h := int64(math.Ceil(tc.memoryAge * 100))
			s := min(int64(6_000), 2*h+300)
			wantRevenue := tc.wantGross - ceilBPS(tc.wantGross, h)
			wantStress := ceilBPS(tc.wantGross, s) - ceilBPS(tc.wantGross, h)
			wantLimit := tc.wantGross / 20 * (10_000 - s) / 10_000
			if reason != "" || revenue != wantRevenue || limit != wantLimit || stress != wantStress || !strings.HasPrefix(source, retainedBookSource) {
				t.Fatalf("fresher price was ignored, raised old value, or changed depth: revenue %dp, limit %dp, stress %dp, source %q, reason %q", revenue, limit, stress, source, reason)
			}
		})
	}
}

func TestQuayBookMemoryFresherCapPreservesLowerLevels(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 6,
		RetainedBid{PricePence: 5_000, RemainingQuantity: 10},
		RetainedBid{PricePence: 3_000, RemainingQuantity: 10},
	)
	at := in.Now.Add(-time.Minute)
	bid := int64(4_000)
	detail := in.Ports["le_havre"]
	detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "portsmouth", SnapshotAt: at, Commodities: []GossipCommodity{{
		CommodityID: "provisions", SnapshotAt: at, BestBidPence: &bid,
	}}}}
	in.Ports["le_havre"] = detail
	revenue, limit, stress, _, reason := destinationRevenue(in, "portsmouth", "provisions", 20)
	if reason != "" || revenue != 65_800 || limit != 2_550 || stress != 6_300 {
		t.Fatalf("capping the top price raised lower retained levels: revenue %dp, limit %dp, stress %dp, reason %q", revenue, limit, stress, reason)
	}
}
