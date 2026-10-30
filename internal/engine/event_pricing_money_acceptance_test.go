package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Event rows use the live /world/events shape, including canvas_sails rather
// than the display name. Prices are integer pence: 1,177 Cr/t = 117,700p/t.
const moneyCanvasShock = `[{"id":"01a18232-6491-76c9-860d-ebea77ef8c93","event_type":"demand_shortage","commodity_id":"canvas_sails","target_port_id":"hamburg","starts_at":"2026-10-28T11:08:22Z","expires_at":"2026-10-28T11:38:22Z","is_active":true,"structural":false,"title":"Acute Shortage at Hamburg: Canvas Sailcloth Scarcity"}]`
const moneyAdmiraltyShock = `[{"id":"01a182e9-8714-74e1-b14f-21fdd4bcfe5d","event_type":"admiralty_mobilization","commodity_id":"provisions","target_port_id":"amsterdam","starts_at":"2026-10-28T14:28:22Z","expires_at":"2026-10-28T14:58:22Z","is_active":true,"structural":false,"title":"Admiralty Mobilization at Amsterdam: Naval Provisions Requisition"}]`

func moneyTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return at
}

func moneyMarket(t *testing.T, port, commodity string, ask, bid, tons int64) Market {
	t.Helper()
	books := fmt.Sprintf(`{"commodity_id":%q,"depth":{"asks":[{"price_pence":%d,"remaining_quantity":%d}],"bids":[{"price_pence":%d,"remaining_quantity":%d}]}}`, commodity, ask, tons, bid, tons)
	if commodity != "provisions" {
		books += `,{"commodity_id":"provisions","depth":{"asks":[{"price_pence":100,"remaining_quantity":1000}],"bids":[]}}`
	}
	return sourcingFixture[Market](t, fmt.Sprintf(`{"port_id":%q,"visibility":"quay","customs_bps":0,"books":[%s]}`, port, books))
}

func moneyInput(t *testing.T, now time.Time, commodity string, ask, bid, quantity, distance int64) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Now, in.Clock.RealUTC = now, now
	in.Catalog.Ports = []Port{
		{ID: "amsterdam", BankingRegion: "channel", Phase: 1, Manufactures: []string{commodity}},
		{ID: "hamburg", BankingRegion: "channel", Phase: 1, Imports: []string{commodity}},
	}
	in.Catalog.ShipClasses[0].CrewRequired = 0
	in.Ship.Ship.CurrentPortID, in.Ship.EffectiveHoldCapacityTons = "amsterdam", quantity
	in.Contracts, in.Fleet = nil, []ShipState{in.Ship}
	in.Markets = map[string]Market{
		"amsterdam": moneyMarket(t, "amsterdam", commodity, ask, 0, 1000),
		"hamburg":   moneyMarket(t, "hamburg", commodity, 100_000_000, bid, 1000),
	}
	in.Ports = map[string]PortDetail{
		"amsterdam": {ID: "amsterdam", EffectiveBerths: 4, Visibility: "quay"},
		"hamburg":   {ID: "hamburg", EffectiveBerths: 4, Visibility: "quay"},
	}
	in.Routes = map[string]Route{}
	in.Metadata = map[string]DatasetMeta{}
	for _, key := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "world_events", "books/amsterdam", "books/hamburg", "ports/amsterdam", "ports/hamburg"} {
		in.Metadata[key] = sourcingFreshMeta(now)
	}
	for _, pair := range [][2]string{{"amsterdam", "hamburg"}, {"hamburg", "amsterdam"}} {
		key := routeKey(pair[0], pair[1], "hoy")
		in.Routes[key] = sourcingRoute(pair[0], pair[1], distance)
		in.Metadata["routes/"+key] = sourcingFreshMeta(now)
	}
	in.Balances["channel"] = 100_000_000
	in.HazardQuotes = nil
	seedZeroHazardQuotes(&in)
	return in
}

func moneySnapshotInput(in PlanningInput, snapshot map[string]json.RawMessage) PlanningInput {
	state := AccountState{
		Catalog: in.Catalog, Clock: in.Clock, Ships: in.Fleet, Markets: in.Markets,
		Ports: in.Ports, Routes: in.Routes, Contracts: in.Contracts, HazardQuotes: in.HazardQuotes,
	}
	for region, balance := range in.Balances {
		state.Vaults = append(state.Vaults, Vault{BankingRegion: region, AccountType: "vault", BalancePence: balance})
	}
	if snapshot == nil {
		snapshot = map[string]json.RawMessage{}
	}
	snapshot["zones"] = encode(in.Zones)
	return PlanningInputFor(in.Now, in.Ship, state, snapshot, PlanningSources{
		CompanyID: in.CompanyID, ReservePence: in.ReservePence, Metadata: in.Metadata, Generation: in.Generation,
	})
}

func moneySpot(t *testing.T, in PlanningInput, commodity string) Plan {
	t.Helper()
	plans := PlanVoyages(in)
	for _, plan := range plans {
		if plan.Kind == "spot" && plan.Commodity == commodity && plan.Destination == "hamburg" {
			return plan
		}
	}
	t.Fatalf("missing spot alternative for %s: %+v", commodity, plans)
	return Plan{}
}

func TestMoneyEventCanvasArrivalAfterExpiryNeverUsesShockBid(t *testing.T) {
	for _, remembered := range []bool{false, true} {
		t.Run(fmt.Sprintf("pre_event_book_%t", remembered), func(t *testing.T) {
			in := moneyInput(t, moneyTime("2026-10-28T11:32:24Z"), "canvas_sails", 50_000, 117_700, 30, 130)
			snapshot := map[string]json.RawMessage{"world_events": json.RawMessage(moneyCanvasShock)}
			if remembered {
				snapshot["_quay_book_memory"] = encode(map[string]RetainedQuayBook{"hamburg": {
					ObservedAt: moneyTime("2026-10-28T11:00:00Z"),
					Bids:       map[string][]RetainedBid{"canvas_sails": {{PricePence: 57_700, RemainingQuantity: 1000}}},
				}})
			}
			plan := moneySpot(t, moneySnapshotInput(in, snapshot), "canvas_sails")
			if !plan.ExpectedArrival.After(moneyTime("2026-10-28T11:38:22Z")) || plan.ExpectedArrival.Hour() != 12 || plan.ExpectedArrival.Minute() >= 10 {
				t.Fatalf("canvas fixture must arrive at 12:0x, not within the event: %+v", plan)
			}
			if plan.Accepted {
				if !remembered || plan.Economics.RevenuePence >= plan.Quantity*57_700 || plan.SellLimitPence >= 57_700 {
					t.Fatalf("after-expiry canvas was priced on the 1,177 Cr/t shock instead of aged pre-event 577 Cr/t: %+v", plan)
				}
			} else if !strings.Contains(plan.Reason, "01a18232-6491-76c9-860d-ebea77ef8c93") &&
				!strings.Contains(strings.ToLower(plan.Reason), "shortage") && !strings.Contains(strings.ToLower(plan.Reason), "event") {
				t.Fatalf("after-expiry rejection must name its event, not hide a missing fixture dependency: %+v", plan)
			}
		})
	}
}

func TestMoneyEventInsideWindowRequiresQuarterRevenueStressAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		ask      int64
		accepted bool
	}{{749, true}, {750, false}} {
		t.Run(fmt.Sprint(tc.ask), func(t *testing.T) {
			in := moneyInput(t, moneyTime("2026-10-28T11:10:00Z"), "canvas_sails", tc.ask, 1000, 1, 30)
			plan := moneySpot(t, moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(moneyCanvasShock)}), "canvas_sails")
			if !plan.ExpectedArrival.Before(moneyTime("2026-10-28T11:38:22Z")) || plan.Economics.ExpectedNetPence != 999-tc.ask {
				t.Fatalf("in-window fixture lost exact integer-pence economics: %+v", plan)
			}
			if plan.Accepted != tc.accepted || plan.Economics.PriceUncertaintyStressPence != 250 {
				t.Fatalf("in-event admission requires net - 25%% revenue >= 0, including the zero boundary: %+v", plan)
			}
		})
	}
}

func TestMoneyEventRecordingKeepsCommodityPreShockObservation(t *testing.T) {
	serverNow := moneyTime("2026-10-28T14:43:00Z")
	in := moneyInput(t, serverNow, "provisions", 1000, 0, 1, 3200)
	in.Clock.RealUTC = serverNow
	// Deliberately model the measured host/server offset. Book capture must use
	// server time from clock, not relabel the old bids with this host timestamp.
	hostNow := serverNow.Add(62 * time.Second)
	meta := sourcingFreshMeta(hostNow)
	oldAt := moneyTime("2026-10-28T14:20:00Z")
	runner := Runner{generation: in.Generation, meta: map[string]DatasetMeta{
		"books/amsterdam": meta, "world_events": meta, "clock": meta,
	}, snapshot: map[string]json.RawMessage{
		"world_events":   json.RawMessage(moneyAdmiraltyShock),
		"_book_observed": encode(map[string]time.Time{"amsterdam": oldAt}),
		"_quay_book_memory": encode(map[string]RetainedQuayBook{"amsterdam": {
			ObservedAt: oldAt, Bids: map[string][]RetainedBid{
				"provisions":   {{PricePence: 4000, RemainingQuantity: 100}},
				"canvas_sails": {{PricePence: 2000, RemainingQuantity: 100}},
			},
		}}),
	}}
	book := moneyMarket(t, "amsterdam", "provisions", 1000, 10_000, 1000)
	book.Books = append(book.Books, moneyMarket(t, "amsterdam", "canvas_sails", 1000, 1500, 1000).Books[0])
	runner.recordQuayBooks(map[string]json.RawMessage{"amsterdam": encode(book)}, []string{"amsterdam"}, in.Catalog, in.Clock, hostNow)
	// Consume the persisted memory through the production decoder after restart.
	in.Ship.Ship.CurrentPortID = "hamburg"
	in.Fleet = []ShipState{in.Ship}
	in.Markets["hamburg"] = moneyMarket(t, "hamburg", "provisions", 1000, 0, 1000)
	in.Markets["amsterdam"] = book
	in.HazardQuotes = nil
	seedZeroHazardQuotes(&in)
	planned := moneySnapshotInput(in, runner.snapshot)
	hull, _ := findHull(in.Catalog, "hoy")
	origin, _ := findPort(in.Catalog, "hamburg")
	plan := evaluatePlan(planned, hull, origin, Plan{Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "hamburg", Destination: "amsterdam", Commodity: "provisions", Quantity: 1}, 0, 0)
	if !plan.Accepted || plan.Economics.RevenuePence >= 4000 || plan.Economics.RevenueAgeGameHours == nil || *plan.Economics.RevenueAgeGameHours < 23 {
		t.Fatalf("14:43 admiralty book erased or re-timestamped pre-14:28 provisions bids: %+v; persisted=%s", plan, runner.snapshot["_quay_book_memory"])
	}
	for _, sample := range decode[[]BookDriftSample](runner.snapshot, "_book_drift") {
		if sample.CommodityID == "provisions" {
			t.Fatalf("admiralty price jump contaminated normal adverse drift: %+v", sample)
		}
	}
}

func TestMoneyEventFreshDepthStartsAfterRetainedReservation(t *testing.T) {
	in := moneyInput(t, moneyTime("2026-10-28T11:00:00Z"), "canvas_sails", 1000, 5000, 1, 30)
	bookCommodity(in.Markets["hamburg"], "canvas_sails").Depth.Bids = []Level{
		{PricePence: 5000, RemainingQuantity: 20}, {PricePence: 4000, RemainingQuantity: 30},
	}
	in = moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(`[]`)})
	in.ReservedRetainedBidTons = map[string]map[string]int64{"hamburg": {"canvas_sails": 20}}
	before := encode(in.Markets)
	plan := moneySpot(t, in, "canvas_sails")
	if !plan.Accepted || plan.Economics.RevenuePence != 4000 || plan.SellLimitPence != 4000 {
		t.Fatalf("fresh walk reused the first ship's first twenty retained bid tons: %+v", plan)
	}
	if !reflect.DeepEqual(before, encode(in.Markets)) {
		t.Fatal("planning mutated the shared fresh book")
	}
	in.ReservedRetainedBidTons["hamburg"]["canvas_sails"] = 50
	if exhausted := moneySpot(t, in, "canvas_sails"); exhausted.Accepted {
		t.Fatalf("fresh depth extrapolated a 51st ton beyond reservations: %+v", exhausted)
	}
}

func TestMoneyEventStaleOrFailedFeedStressesRecentObservations(t *testing.T) {
	for _, failure := range []string{"stale", "failed"} {
		for _, age := range []time.Duration{0, 30 * time.Minute, 30*time.Minute + time.Second} {
			t.Run(failure+"/"+age.String(), func(t *testing.T) {
				in := moneyInput(t, moneyTime("2026-10-28T11:00:00Z"), "canvas_sails", 100, 4000, 1, 30)
				meta := in.Metadata["world_events"]
				meta.Stale = failure == "stale"
				if failure == "failed" {
					meta.Error = "world events unavailable"
				}
				in.Metadata["world_events"] = meta
				snapshot := map[string]json.RawMessage{"world_events": json.RawMessage(`[]`)}
				if age > 0 {
					delete(in.Metadata, "books/hamburg")
					delete(in.Markets, "hamburg")
					snapshot["_quay_book_memory"] = encode(map[string]RetainedQuayBook{"hamburg": {
						ObservedAt: in.Now.Add(-age), Bids: map[string][]RetainedBid{"canvas_sails": {{PricePence: 4000, RemainingQuantity: 100}}},
					}})
				}
				plan := moneySpot(t, moneySnapshotInput(in, snapshot), "canvas_sails")
				want := ceilBPS(plan.Economics.RevenuePence, 2500)
				if !plan.Accepted || plan.Economics.RevenuePence <= 0 ||
					age <= 30*time.Minute && plan.Economics.PriceUncertaintyStressPence < want ||
					age > 30*time.Minute && plan.Economics.PriceUncertaintyStressPence >= want {
					t.Fatalf("failed event feed did not apply the inclusive 30-real-minute stress boundary: age=%s %+v", age, plan)
				}
			})
		}
	}
}

func TestMoneyEventNoShockNoReservationPreservesLegacyEconomics(t *testing.T) {
	in := moneyInput(t, moneyTime("2026-10-28T11:00:00Z"), "canvas_sails", 749, 1000, 1, 30)
	plan := moneySpot(t, moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(`[]`)}), "canvas_sails")
	got := plan.Economics
	gameDays := float64(30)/4.8/24 + 1.0/24 + 4.0/1440 + 1.0/16
	repeatDays := gameDays + float64(30)/4.8/24 + 30.0/1440 + 1.0/16
	if !plan.Accepted || got.Basis != "destination" || got.RevenueBasis != "depth" || got.RevenuePence != 1000 || got.PurchasePence != 749 || got.CustomsPence != 1 ||
		got.ProvisionsPence != 0 || got.StoreCashPence != 0 || got.BerthDuesPence != 0 || got.UpkeepPence != 0 || got.TollPence != 0 || got.FundingFeesPence != 0 ||
		got.CollateralPence != 0 || got.HazardBPS != 0 || got.HazardPence != 0 || got.HazardSource != "Lloyd's expected-claim upper bound, hull-only, 0.00 gh old" ||
		got.ExpectedNetPence != 250 || got.StressHaircutBPS != 1000 || got.StressHaircutPence != 100 || got.PriceUncertaintyStressPence != 0 || got.StressNetPence != 150 ||
		got.RevenueAgeGameHours == nil || *got.RevenueAgeGameHours != 0 || got.ProbabilityEstimate != nil || got.BallastRepeatNetPence != 250 ||
		math.Abs(got.GameDays-gameDays) > 1e-12 || math.Abs(got.NetPencePerGameDay-250/gameDays) > 1e-9 || math.Abs(got.BallastRepeatGameDays-repeatDays) > 1e-12 {
		t.Fatalf("ordinary full-depth economics changed without a shock or reservation: %+v", plan)
	}
}
