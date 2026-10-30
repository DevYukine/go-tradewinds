package engine

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMarketEventsDecodeHistoricalAndStructural(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	start := in.Now.Add(-time.Hour)
	expiry := in.Now.Add(-30 * time.Minute)
	events := []MarketShock{
		{ID: "historical", EventType: "demand_shortage", PortID: "portsmouth", CommodityID: "provisions", StartsAt: start, ExpiresAt: expiry},
		{ID: "lasting", EventType: "production_collapse", PortID: "portsmouth", CommodityID: "provisions", StartsAt: start, ExpiresAt: expiry, Structural: true},
		{ID: "piracy", EventType: "pirate_outbreak", StartsAt: start, ExpiresAt: expiry},
	}
	shocks, valid := decodeMarketShocks(map[string]json.RawMessage{"world_events": encode(events)})
	if !valid || len(shocks["portsmouth"]["provisions"]) != 1 || shocks["portsmouth"]["provisions"][0].ID != "historical" {
		t.Fatalf("historical price shocks or structural-news exclusion changed: %+v, valid %t", shocks, valid)
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`{"events":[]}`), json.RawMessage(`[{}]`)} {
		snapshot := map[string]json.RawMessage{"world_events": raw}
		input := PlanningInputFor(in.Now, in.Ship, AccountState{Clock: in.Clock}, snapshot, PlanningSources{Metadata: in.Metadata, Generation: in.Generation})
		if !input.marketEventUnavailable() {
			t.Fatalf("malformed event feed silently became healthy: %s", raw)
		}
	}
}

func TestMarketEventsPreTransitionAdverseShockStressesRetainedPlan(t *testing.T) {
	t.Parallel()
	serverNow := moneyTime("2026-10-29T02:11:14Z")
	observedAt := moneyTime("2026-10-29T02:08:06.010336902Z")
	for _, tc := range []struct {
		name       string
		eventType  string
		expired    bool
		wantStress bool
	}{
		{name: "active supply boom", eventType: "supply_boom", wantStress: true},
		{name: "expired supply boom still crossed the quote", eventType: "supply_boom", expired: true, wantStress: true},
		{name: "unknown receipt shock is conservative", eventType: "receipt_slump", wantStress: true},
		{name: "active positive demand shock", eventType: "demand_shortage"},
		{name: "expired positive demand keeps pre-event fallback", eventType: "demand_shortage", expired: true},
		{name: "positive Admiralty demand", eventType: "admiralty_mobilization"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := moneyInput(t, serverNow, "water_beer", 1_850, 2_011, 30, 80)
			// Match the live clock offset without comparing host timestamps to
			// upstream event or commodity-observation timestamps.
			in.Now = serverNow.Add(65 * time.Second)
			for key, meta := range in.Metadata {
				meta.AttemptedAt, meta.UpdatedAt = in.Now, in.Now
				in.Metadata[key] = meta
			}
			delete(in.Markets, "hamburg")
			delete(in.Metadata, "books/hamburg")
			shock := MarketShock{
				ID: "pre-transition", EventType: tc.eventType, PortID: "hamburg", CommodityID: "water_beer",
				StartsAt: moneyTime("2026-10-29T02:08:23.025374Z"), ExpiresAt: moneyTime("2026-10-29T02:38:23.025374Z"),
			}
			if tc.expired {
				shock.ExpiresAt = serverNow.Add(-time.Minute)
			}
			in = moneySnapshotInput(in, map[string]json.RawMessage{
				"world_events": encode([]MarketShock{shock}),
				"_quay_book_memory": encode(map[string]RetainedQuayBook{"hamburg": {
					ObservedAt: observedAt,
					Bids:       map[string][]RetainedBid{"water_beer": {{PricePence: 2_011, RemainingQuantity: 885}}},
				}}),
			})
			// Isolate event policy from the independent calibration regime.
			in.RevenueCalibration = map[string]int64{"retained": 10_000}
			plan := moneySpot(t, in, "water_beer")
			if !plan.ExpectedArrival.After(shock.StartsAt) || plan.ExpectedArrival.After(shock.ExpiresAt) != tc.expired {
				t.Fatalf("fixture arrival does not cross the intended transition: arrival %s, event %+v", plan.ExpectedArrival, shock)
			}
			if !strings.HasPrefix(plan.PriceSource, retainedBookSource) || plan.EventPriceStress != tc.wantStress {
				t.Fatalf("pre-transition retained quote lost event policy: %+v", plan)
			}
			if tc.wantStress {
				if plan.Accepted || plan.Economics.PriceUncertaintyStressPence < ceilBPS(plan.Economics.RevenuePence, marketEventStressBPS) {
					t.Fatalf("thin pre-adverse-event lot was admitted without the quarter-revenue floor: %+v", plan)
				}
			} else if !plan.Accepted {
				t.Fatalf("known positive shock changed the safe pre-event fallback: %+v", plan)
			}
		})
	}
}

func TestMarketEventsAdverseTransitionUsesServerHorizon(t *testing.T) {
	t.Parallel()
	base := sourcingInputFixture(t).Now
	for _, tc := range []struct {
		name       string
		now        time.Duration
		arrival    time.Duration
		wantStress bool
	}{
		{name: "server and arrival before transition", arrival: 10 * time.Second},
		{name: "arrival exactly at transition", arrival: 20 * time.Second, wantStress: true},
		{name: "arrival after transition", arrival: 30 * time.Second, wantStress: true},
		{name: "server already crossed transition", now: 30 * time.Second, arrival: 10 * time.Second, wantStress: true},
		{name: "server crossed an expired adverse shock", now: time.Minute, arrival: time.Minute, wantStress: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sourcingInputFixture(t)
			in.Clock.RealUTC = base.Add(tc.now)
			in.Now = in.Clock.RealUTC.Add(65 * time.Second)
			in.Metadata["clock"] = DatasetMeta{
				AttemptedAt: in.Now, UpdatedAt: in.Now, Generation: in.Generation, Complete: true,
			}
			shock := MarketShock{
				ID: "server-transition", EventType: "supply_boom", PortID: "portsmouth", CommodityID: "provisions",
				StartsAt: base.Add(20 * time.Second), ExpiresAt: base.Add(40 * time.Second),
			}
			in.MarketShocks = map[string]map[string][]MarketShock{"portsmouth": {"provisions": {shock}}}
			observedAt := base.Add(-time.Second)
			required, reason := in.marketObservationPolicy("portsmouth", "provisions", observedAt, observedAt, base.Add(tc.arrival))
			if reason != "" || required != tc.wantStress {
				t.Fatalf("adverse transition used host time or ignored arrival/server progress: stress %t, reason %q; want %t", required, reason, tc.wantStress)
			}
		})
	}
}

func TestMarketEventsServerEnvelopeAndExpiryBoundary(t *testing.T) {
	t.Parallel()
	in := sourcingInputFixture(t)
	serverNow := in.Now
	in.Now = serverNow.Add(62 * time.Second)
	in.Metadata["clock"] = DatasetMeta{
		AttemptedAt: in.Now.Add(-2 * time.Second), UpdatedAt: in.Now,
		Generation: in.Generation, Complete: true,
	}
	in.Metadata["books/portsmouth"] = DatasetMeta{
		AttemptedAt: in.Now.Add(-time.Second), UpdatedAt: in.Now,
		Generation: in.Generation, Complete: true,
	}
	shock := MarketShock{ID: "offset-shock", EventType: "demand_shortage", PortID: "portsmouth", CommodityID: "provisions", StartsAt: serverNow, ExpiresAt: serverNow.Add(30 * time.Minute)}
	in.MarketShocks = map[string]map[string][]MarketShock{"portsmouth": {"provisions": {shock}}}
	from, to, valid := in.marketObservationBounds("books/portsmouth")
	if !valid || !from.Equal(serverNow.Add(-time.Second)) || !to.Equal(serverNow.Add(2*time.Second)) {
		t.Fatalf("book observation used host time instead of server envelope: %s to %s, valid %t", from, to, valid)
	}
	for _, tc := range []struct {
		name    string
		arrival time.Time
		allowed bool
	}{
		{name: "exact expiry", arrival: shock.ExpiresAt, allowed: true},
		{name: "one nanosecond late", arrival: shock.ExpiresAt.Add(time.Nanosecond)},
		{name: "unknown arrival"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			required, reason := in.marketObservationPolicy("portsmouth", "provisions", from, to, tc.arrival)
			if tc.allowed && (!required || reason != "") || !tc.allowed && !strings.Contains(reason, shock.ID) {
				t.Fatalf("wrong bounded expiry policy: stress %t, reason %q", required, reason)
			}
		})
	}
}

func TestMarketEventsPreShockHaircutIncludesRemainingVoyage(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 1, RetainedBid{PricePence: 10_000, RemainingQuantity: 20})
	shock := MarketShock{ID: "expiry", EventType: "demand_shortage", PortID: "portsmouth", CommodityID: "provisions", StartsAt: in.Now.Add(-10 * time.Minute), ExpiresAt: in.Now.Add(20 * time.Minute)}
	in.MarketShocks = map[string]map[string][]MarketShock{"portsmouth": {"provisions": {shock}}}
	in.Metadata["books/portsmouth"] = sourcingFreshMeta(in.Now)
	in.Markets["portsmouth"] = sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":20000,"remaining_quantity":20}]}}]}`)
	arrival := in.Now.Add(24 * time.Minute)
	revenue, _, _, source, reason, required := destinationRevenue(in, "portsmouth", "provisions", 1, arrival)
	// 24 real minutes at 60x is one additional game day of retained-book age.
	if reason != "" || required || revenue != 9_800 || !strings.Contains(source, "2.0 game days old; 200 bps") {
		t.Fatalf("pre-shock replacement did not age through arrival: revenue %d, source %q, reason %q, stress %t", revenue, source, reason, required)
	}
}

func TestMarketEventsBookDriftDropsShockDecayIntervals(t *testing.T) {
	t.Parallel()
	in := retainedBookInput(t, 2, RetainedBid{PricePence: 10_000, RemainingQuantity: 20})
	oldAt := in.QuayBookMemory["portsmouth"].ObservedAt
	shock := MarketShock{ID: "decayed", EventType: "demand_shortage", PortID: "portsmouth", CommodityID: "provisions", StartsAt: in.Now.Add(-30 * time.Minute), ExpiresAt: in.Now.Add(-time.Minute)}
	runner := Runner{
		generation: in.Generation,
		meta: map[string]DatasetMeta{
			"clock": sourcingFreshMeta(in.Now), "world_events": sourcingFreshMeta(in.Now), "books/portsmouth": sourcingFreshMeta(in.Now),
		},
		snapshot: map[string]json.RawMessage{
			"world_events":      encode([]MarketShock{shock}),
			"_quay_book_memory": encode(in.QuayBookMemory),
			"_book_drift": encode([]BookDriftSample{
				{PortID: "portsmouth", CommodityID: "provisions", AgeGameDays: 2, TopBidChangeBPS: -5_000, FromObservedAt: oldAt, ToObservedAt: in.Now},
				{PortID: "portsmouth", CommodityID: "canvas_sails", AgeGameDays: 2, TopBidChangeBPS: -100, FromObservedAt: oldAt, ToObservedAt: in.Now},
			}),
		},
	}
	book := sourcingFixture[Market](t, `{"visibility":"quay","books":[{"commodity_id":"provisions","depth":{"bids":[{"price_pence":9000,"remaining_quantity":20}]}}]}`)
	runner.recordQuayBooks(map[string]json.RawMessage{"portsmouth": encode(book)}, []string{"portsmouth"}, in.Catalog, in.Clock, in.Now)
	drift := decode[[]BookDriftSample](runner.snapshot, "_book_drift")
	if len(drift) != 1 || drift[0].CommodityID != "canvas_sails" {
		t.Fatalf("shock jump/decay survived normal-drift filtering: %+v", drift)
	}
	remembered := decode[map[string]RetainedQuayBook](runner.snapshot, "_quay_book_memory")["portsmouth"]
	if remembered.Bids["provisions"][0].PricePence != 9_000 || !remembered.observedAt("provisions").Equal(in.Now) {
		t.Fatalf("post-event baseline was not captured: %+v", remembered)
	}
}

func TestMarketEventsFundingStressSurvivesSerialization(t *testing.T) {
	t.Parallel()
	plan := Plan{Kind: "spot", Accepted: true, EventPriceStress: true, Economics: Economics{
		RevenuePence: 1_000, PurchasePence: 748, CustomsPence: 1, ExpectedNetPence: 251,
		PriceUncertaintyStressPence: 250, StressNetPence: 1, GameDays: 1,
	}}
	var decoded Plan
	if err := json.Unmarshal(encode(plan), &decoded); err != nil {
		t.Fatal(err)
	}
	if !addFundingFees(decoded, 1).Accepted || addFundingFees(decoded, 2).Accepted {
		t.Fatalf("funding lost the inclusive event stress boundary after restart: %+v", decoded)
	}
}

func TestMarketEventsProcurementCannotReuseAdmittedShockFloor(t *testing.T) {
	t.Parallel()
	in := moneyInput(t, moneyTime("2026-10-28T11:10:00Z"), "canvas_sails", 749, 1_000, 1, 30)
	in = moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(moneyCanvasShock)})
	plan := moneySpot(t, in, "canvas_sails")
	if !plan.Accepted || !plan.EventPriceStress {
		t.Fatalf("fixture needs an admitted in-window shock: %+v", plan)
	}
	in.Now = moneyTime("2026-10-28T11:34:00Z")
	in.Clock.RealUTC = in.Now
	for key := range in.Metadata {
		in.Metadata[key] = sourcingFreshMeta(in.Now)
	}
	if revenue, reason := acquiredRevenue(in, plan, 1, nil); revenue != 0 || !strings.Contains(reason, "market event") {
		t.Fatalf("procurement reused an obsolete admitted ETA: %d, %q", revenue, reason)
	}
	if revenue, valid := paidSpotRevenue(in, plan, 1); valid {
		t.Fatalf("fresh paid-cargo recovery reused an expiring shock: %d", revenue)
	}
	delete(in.Metadata, "books/hamburg")
	delete(in.Markets, "hamburg")
	if revenue, valid := paidSpotRevenue(in, plan, 1); valid {
		t.Fatalf("lost destination presence made the admitted shock a permanent floor: %d", revenue)
	}
}

func TestMarketEventsMixedShipmentPricesOnceAtLadenArrival(t *testing.T) {
	t.Parallel()
	in := moneyInput(t, moneyTime("2026-10-28T11:10:00Z"), "canvas_sails", 749, 1_000, 2, 30)
	in.Contracts = []Contract{{
		ID: "event-freight", ContractType: "freight", Status: "open", IssuerCompanyID: "other",
		OriginPortID: "amsterdam", DestPortID: "hamburg", CommodityID: "provisions",
		QuantityRequired: 1, RewardPence: 1_000,
	}}
	in = moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(moneyCanvasShock)})
	for _, plan := range PlanVoyages(in) {
		if plan.Kind != "spot+freight" || plan.Commodity != "canvas_sails" {
			continue
		}
		if !plan.Accepted || plan.Economics.RevenuePence != 2_000 || plan.Economics.PriceUncertaintyStressPence != 250 {
			t.Fatalf("mixed shipment lost its arrival-aware quarter-cargo-revenue stress: %+v", plan)
		}
		requoted := priceIncomePlan(in, plan, 0)
		if !requoted.Accepted || requoted.Economics.RevenuePence != plan.Economics.RevenuePence ||
			requoted.Economics.PriceUncertaintyStressPence != plan.Economics.PriceUncertaintyStressPence ||
			!requoted.ExpectedArrival.Equal(plan.ExpectedArrival) {
			t.Fatalf("income repricing double-counted cargo or omitted the sealed lot's arrival: before %+v, after %+v", plan, requoted)
		}
		return
	}
	t.Fatal("missing mixed event shipment")
}

func TestMarketEventsCommittedFreshBidsReserveOutstandingAndNextPlanOnce(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	row := destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, map[string]int64{recoveryGrain: 6})
	row["ship"].(map[string]any)["berthed_at"] = time.Now().UTC().Add(-time.Minute)
	h.api.setFleet([]map[string]any{
		row,
		sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
	})
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort)
	runner := h.runner(primaryTestKey)
	runner.generation = "ordinary-event-reservations"
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	data := missionData{
		Plan: Plan{
			Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort,
			Commodity: recoveryGrain, Quantity: 20,
		},
		DeliveredQuantity: 5,
		GrowthLane: &growthLaneIntent{NextPlan: &Plan{
			Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort,
			Commodity: recoveryGrain, Quantity: 3,
		}},
	}
	h.putMission(recoveryShipCharizard, "ordinary-event-reservations", "sailing", data)
	runner.stateCache.Markets[recoveryFarPort] = procurementReviewBook(recoveryFarPort, []Level{
		{OrderID: "best-external-bids", PricePence: 1_000, RemainingQuantity: 15},
		{OrderID: "second-external-bids", PricePence: 900, RemainingQuantity: 8},
	})
	runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{{
		OrderID: "local-exit", PricePence: 800, RemainingQuantity: 6,
	}})
	runner.meta["books/"+recoveryFarPort] = runner.meta["clock"]
	before := encode(runner.stateCache)
	paid := missionData{Plan: Plan{
		Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort,
		Commodity: recoveryGrain, Quantity: 6, Accepted: true, SellLimitPence: 1_000,
	}, BoughtQuantity: 6, BuyDone: true}
	h.putMission(recoveryShipBlastoise, "ordinary-event-reservations", "depart", paid)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	if reserved := input.ReservedRetainedBidTons[recoveryFarPort][recoveryGrain]; reserved != 18 {
		t.Fatalf("ordinary outstanding or growth next-plan reservation was lost, or the ship reserved its own paid lot: %d", reserved)
	}
	revenue, _, _, _, reason, _ := destinationRevenue(input, recoveryFarPort, recoveryGrain, 5, time.Time{})
	if reason != "" || revenue != 4_500 {
		t.Fatalf("fresh bids were reserved twice instead of leaving five tons: %d, %q", revenue, reason)
	}
	if _, _, _, _, reason, _ := destinationRevenue(input, recoveryFarPort, recoveryGrain, 6, time.Time{}); reason == "" {
		t.Fatal("fresh pricing omitted an ordinary outstanding mission's reservation")
	}
	if receipts, full, err := procurementBidRevenue(input, recoveryFarPort, recoveryGrain, 6); err != nil || full || receipts != 4_500 {
		t.Fatalf("procurement reused reserved fresh bids instead of five remaining tons: %d, full %t, %v", receipts, full, err)
	}
	if receipts, valid := paidSpotRevenue(input, paid.Plan, 6); !valid || receipts != 4_500 {
		t.Fatalf("paid-cargo recovery reused reserved fresh bids: %d, valid %t", receipts, valid)
	}
	if proceed, err := runner.committedProcurementDepartureDecision(h.ctx, ship, recoveryFarPort, &paid); err != nil || proceed {
		t.Fatalf("committed recovery must prefer the 4800p local exit over 4500p destination receipts: proceed %t, %v", proceed, err)
	}
	input.ReservedRetainedBidTons[recoveryFarPort][recoveryGrain] = 23
	if receipts, full, err := procurementBidRevenue(input, recoveryFarPort, recoveryGrain, 1); err != nil || full || receipts != 0 {
		t.Fatalf("fully reserved fresh depth is known empty liquidity, not reusable or missing evidence: %d, full %t, %v", receipts, full, err)
	}
	if string(before) != string(encode(runner.stateCache)) {
		t.Fatal("private bid reservations changed the authoritative market cache")
	}
}

func TestMarketEventsPaidCargoWithoutSafeDestinationQuoteSelectsExit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		localTons   int64
		localPrice  int64
		originDues  int64
		unknownCost bool
		badRoute    bool
		staleClock  bool
		wantProceed bool
		wantError   bool
	}{
		{name: "thin post-shock bid below paid cost", localTons: 20, localPrice: 300, wantProceed: true},
		{name: "one penny per ton below paid cost", localTons: 20, localPrice: 499, wantProceed: true},
		{name: "exact paid acquisition floor", localTons: 20, localPrice: 500},
		{name: "above paid acquisition floor", localTons: 20, localPrice: 501},
		{name: "local handling would cross below paid cost", localTons: 20, localPrice: 550, originDues: 40_000, wantProceed: true},
		{name: "unknown paid cost cannot authorize liquidation", localTons: 20, localPrice: 1_000, unknownCost: true, wantProceed: true},
		{name: "partial liquidity cannot empty hold despite high receipts", localTons: 5, localPrice: 3_000, wantProceed: true},
		{name: "no local liquidity", wantProceed: true},
		{name: "missing route still holds", localTons: 20, localPrice: 600, badRoute: true, wantError: true},
		{name: "stale clock still holds", localTons: 20, localPrice: 600, staleClock: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(20)
			data.Plan.EventPriceStress = true
			data.Plan.Quantity = 40
			data.Plan.BuyLimitPence = 900
			data.Plan.SellLimitPence = 1_000
			data.AcquisitionPence = 0
			// The actual partial lot cost 10000p over two fills, independent
			// of the originally quoted forty tons and its limit prices.
			for _, fill := range []struct {
				quantity int64
				gross    int64
			}{{quantity: 7, gross: 3_300}, {quantity: 13, gross: 6_700}} {
				quantity, gross, customs := fill.quantity, fill.gross, int64(0)
				recordTradeCosts(&data, "buy", tradeResult{
					FilledQuantity: &quantity, TotalGrossPence: &gross, CustomsDutyPence: &customs,
				})
			}
			data.CostsKnown = !tc.unknownCost
			var bids []Level
			if tc.localTons > 0 {
				bids = []Level{{PricePence: tc.localPrice, RemainingQuantity: tc.localTons}}
			}
			runner.stateCache.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, bids)
			origin := runner.stateCache.Ports[recoveryHomePort]
			origin.BerthDuesPerDayPence = tc.originDues
			runner.stateCache.Ports[recoveryHomePort] = origin
			delete(runner.stateCache.Markets, recoveryFarPort)
			delete(runner.meta, "books/"+recoveryFarPort)
			if tc.badRoute {
				delete(runner.stateCache.Routes, routeKey(recoveryHomePort, recoveryFarPort, recoveryHull))
			}
			if tc.staleClock {
				meta := runner.meta["clock"]
				meta.Stale = true
				runner.meta["clock"] = meta
			}
			input := runner.planningInput(ship)
			if receipts, valid := paidSpotRevenue(input, data.Plan, 20); valid {
				t.Fatalf("fixture retained the admitted shock as a permanent receipt floor: %d", receipts)
			}
			proceed, err := runner.procurementDepartureDecision(input, ship, recoveryFarPort, &data)
			if proceed != tc.wantProceed || (err != nil) != tc.wantError {
				t.Fatalf("paid event cargo exit = %t, %v; want departure %t, error %t", proceed, err, tc.wantProceed, tc.wantError)
			}
			if data.AcquisitionPence != 10_000 || data.BoughtQuantity != 20 {
				t.Fatalf("recovery rewrote the actual paid lot: %+v", data)
			}
		})
	}
}

func TestMarketEventsPaidCargoFloorUsesNetPreEventEstimate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		preBid        int64
		delta         int64
		unknownCost   bool
		belowPaidCost bool
	}{
		{name: "below higher estimated recovery", preBid: 1_000, delta: -1},
		{name: "exact higher estimated recovery", preBid: 1_000},
		{name: "above higher estimated recovery", preBid: 1_000, delta: 1},
		{name: "paid cost exceeds estimated recovery", preBid: 500, delta: 1, belowPaidCost: true},
		{name: "exact estimated recovery below paid cost", preBid: 500, belowPaidCost: true},
		{name: "below estimated recovery below paid cost", preBid: 500, delta: -1, belowPaidCost: true},
		{name: "unknown paid cost with usable estimate", preBid: 500, unknownCost: true, belowPaidCost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, ship, data := procurementReviewRunner(20)
			data.Plan.EventPriceStress = true
			data.CostsKnown = !tc.unknownCost
			delete(runner.stateCache.Markets, recoveryFarPort)
			delete(runner.meta, "books/"+recoveryFarPort)
			input := runner.planningInput(ship)
			input.marketEventsInvalid = false
			input.Metadata["world_events"] = input.Metadata["clock"]
			now := input.Clock.RealUTC
			input.MarketShocks = map[string]map[string][]MarketShock{recoveryFarPort: {recoveryGrain: {{
				ID: "paid-pre-event", EventType: "demand_shortage", PortID: recoveryFarPort, CommodityID: recoveryGrain,
				StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Minute),
			}}}}
			input.QuayBookMemory = map[string]RetainedQuayBook{recoveryFarPort: {
				ObservedAt: now.Add(-10 * time.Minute),
				Bids:       map[string][]RetainedBid{recoveryGrain: {{PricePence: tc.preBid, RemainingQuantity: 20}}},
			}}
			arrival := procurementArrival(input, data.Plan, 20)
			revenue, _, _, source, reason, _ := destinationRevenue(input, recoveryFarPort, recoveryGrain, 20, arrival)
			if reason != "" || !strings.HasPrefix(source, retainedBookSource) {
				t.Fatalf("fixture needs a labelled, arrival-aged pre-event estimate: %d, %q, %q", revenue, source, reason)
			}
			// The new stay is 1000p. At the fixture's laden speed and crew,
			// sailing plus port consumption costs another 2p of replacement stores.
			estimatedRecovery := revenue - 1_000 - 2
			floor := estimatedRecovery
			if tc.belowPaidCost && (floor >= data.AcquisitionPence || floor+tc.delta >= data.AcquisitionPence) {
				t.Fatalf("fixture needs local and estimated recovery below the sunk acquisition: acquisition %d, estimate %d, local %d", data.AcquisitionPence, estimatedRecovery, floor+tc.delta)
			}
			unit := floor / 20
			input.Markets[recoveryHomePort] = procurementReviewBook(recoveryHomePort, []Level{
				{PricePence: unit, RemainingQuantity: 19},
				{PricePence: floor - 19*unit + tc.delta, RemainingQuantity: 1},
			})
			proceed, err := runner.procurementDepartureDecision(input, ship, recoveryFarPort, &data)
			if err != nil || proceed != (tc.delta < 0) {
				t.Fatalf("local recovery %d against floor %d (%s): departure %t, %v", floor+tc.delta, floor, source, proceed, err)
			}
		})
	}
}

func TestMarketEventsGossipCannotOutliveShock(t *testing.T) {
	t.Parallel()
	now := moneyTime("2026-10-28T11:10:00Z")
	expiry := moneyTime("2026-10-28T11:38:22Z")
	for _, player := range []bool{false, true} {
		name := "one-ton probe"
		if player {
			name = "whole player lot"
		}
		t.Run(name, func(t *testing.T) {
			in := moneyInput(t, now, "canvas_sails", 1, 1_000, 2, 30)
			delete(in.Markets, "hamburg")
			delete(in.Metadata, "books/hamburg")
			bid, playerBid, tons := int64(1_000), int64(900), int64(2)
			price := GossipCommodity{CommodityID: "canvas_sails", SnapshotAt: now, BestBidPence: &bid}
			quantity, wantRevenue, wantStress := int64(1), int64(1_000), int64(250)
			wantSource := "recent observed gossip; one-ton probe; unverified bid size"
			if player {
				price.BestPlayerBidPence, price.BestPlayerBidQuantity = &playerBid, &tons
				quantity, wantRevenue, wantStress = 2, 1_800, 450
				wantSource = playerBidGossipSource
			}
			detail := in.Ports["amsterdam"]
			detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "hamburg", SnapshotAt: now, Commodities: []GossipCommodity{price}}}
			in.Ports["amsterdam"] = detail
			in = moneySnapshotInput(in, map[string]json.RawMessage{"world_events": json.RawMessage(moneyCanvasShock)})
			revenue, _, stress, source, reason, required := destinationRevenue(in, "hamburg", "canvas_sails", quantity, expiry)
			if reason != "" || !required || revenue != wantRevenue || stress != wantStress || source != wantSource {
				t.Fatalf("in-window gossip quote lost event policy: %d, stress %d, source %q, reason %q, required %t", revenue, stress, source, reason, required)
			}
			_, _, _, _, reason, _ = destinationRevenue(in, "hamburg", "canvas_sails", quantity, expiry.Add(time.Nanosecond))
			if !strings.Contains(reason, "01a18232-6491-76c9-860d-ebea77ef8c93") {
				t.Fatalf("after-expiry gossip did not reject with its event identity: %q", reason)
			}
		})
	}
}

func TestMarketEventsUnavailableFeedStressesSelectedGossip(t *testing.T) {
	t.Parallel()
	for _, player := range []bool{false, true} {
		name := "one-ton probe"
		if player {
			name = "whole player lot"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				age      time.Duration
				stale    bool
				failed   bool
				required bool
			}{
				{name: "healthy", age: 30 * time.Minute},
				{name: "stale inclusive edge", age: 30 * time.Minute, stale: true, required: true},
				{name: "failed inclusive edge", age: 30 * time.Minute, failed: true, required: true},
				{name: "stale older than edge", age: 30*time.Minute + time.Nanosecond, stale: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					in := moneyInput(t, moneyTime("2026-10-28T11:10:00Z"), "canvas_sails", 1, 1_000, 2, 30)
					// A one-times clock keeps these 30-real-minute reports inside
					// the separate six-game-hour gossip freshness window.
					in.Clock.Calendar.TimeScale = 1
					delete(in.Markets, "hamburg")
					delete(in.Metadata, "books/hamburg")
					at := in.Clock.RealUTC.Add(-tc.age)
					bid, playerBid, tons := int64(1_000), int64(900), int64(2)
					price := GossipCommodity{CommodityID: "canvas_sails", SnapshotAt: at, BestBidPence: &bid}
					quantity, wantRevenue, wantStress := int64(1), int64(1_000), int64(250)
					if player {
						price.BestPlayerBidPence, price.BestPlayerBidQuantity = &playerBid, &tons
						quantity, wantRevenue, wantStress = 2, 1_800, 300
						if tc.required {
							wantStress = 450
						}
					}
					detail := in.Ports["amsterdam"]
					detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "hamburg", SnapshotAt: at, Commodities: []GossipCommodity{price}}}
					in.Ports["amsterdam"] = detail
					meta := in.Metadata["world_events"]
					meta.Stale = tc.stale
					if tc.failed {
						meta.Error = "event request failed"
					}
					in.Metadata["world_events"] = meta
					revenue, _, stress, _, reason, required := destinationRevenue(in, "hamburg", "canvas_sails", quantity, in.Clock.RealUTC.Add(time.Hour))
					if reason != "" || revenue != wantRevenue || stress != wantStress || required != tc.required {
						t.Fatalf("selected gossip feed stress = %d, stress %d, reason %q, required %t; want %d, %d, %t", revenue, stress, reason, required, wantRevenue, wantStress, tc.required)
					}
				})
			}
		})
	}
}

func TestMarketEventsRetainedStressKeepsStrictBoundary(t *testing.T) {
	t.Parallel()
	in := moneyInput(t, moneyTime("2026-10-28T11:10:00Z"), "canvas_sails", 749, 1_000, 1, 30)
	delete(in.Markets, "hamburg")
	delete(in.Metadata, "books/hamburg")
	in.QuayBookMemory = map[string]RetainedQuayBook{"hamburg": {
		ObservedAt: in.Clock.RealUTC,
		Bids:       map[string][]RetainedBid{"canvas_sails": {{PricePence: 1_000, RemainingQuantity: 10}}},
	}}
	meta := in.Metadata["world_events"]
	meta.Stale = true
	in.Metadata["world_events"] = meta
	equal := moneySpot(t, in, "canvas_sails")
	if equal.Accepted || !equal.EventPriceStress || equal.Economics.ExpectedNetPence != equal.Economics.PriceUncertaintyStressPence ||
		!strings.Contains(equal.Reason, "retained quay book") {
		t.Fatalf("event flag admitted retained pricing at zero stressed net: %+v", equal)
	}
	in.Markets["amsterdam"].Books[0].Depth.Asks[0].PricePence = 748
	positive := moneySpot(t, in, "canvas_sails")
	if !positive.Accepted || !positive.EventPriceStress || positive.Economics.ExpectedNetPence-positive.Economics.PriceUncertaintyStressPence != 1 {
		t.Fatalf("one-penny positive retained stressed net should remain admissible: %+v", positive)
	}
	if funded := addFundingFees(positive, 1); funded.Accepted || !strings.Contains(funded.Reason, "retained quay book") {
		t.Fatalf("funding admitted a retained lot exactly at its stress boundary: %+v", funded)
	}
}

func TestMarketEventsPaidCargoLosesGossipAndStillDeparts(t *testing.T) {
	t.Parallel()
	h, runner, proxy, ship, _ := procurementReviewGossipHarness(t)
	now := runner.stateCache.Clock.RealUTC
	shock := MarketShock{
		ID: "paid-gossip-shock", EventType: "demand_shortage",
		PortID: recoveryFarPort, CommodityID: recoveryGrain,
		StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(30 * time.Minute),
	}
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/world/events" {
			h.api.writeJSON(writer, http.StatusOK, []MarketShock{shock})
			return
		}
		if request.Method == http.MethodGet && request.URL.Path == "/v1/world/clock" {
			clock := h.api.clockDocument()
			clock["calendar"].(map[string]any)["time_scale"] = 60
			h.api.writeJSON(writer, http.StatusOK, clock)
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	ship = harnessShip(t, runner, ship.Ship.ID)
	var plan Plan
	for _, candidate := range PlanVoyages(runner.planningInput(ship)) {
		if candidate.Accepted && candidate.Kind == "spot" && candidate.EventPriceStress {
			plan = candidate
			break
		}
	}
	if plan.Quantity != 1 {
		t.Fatalf("fixture must admit an in-window event gossip probe: %+v", plan)
	}
	mission, data := runner.newVoyage(ship, plan)
	mission.Phase = "buy"
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		mission, data = h.mission(ship.Ship.ID)
		if data.BuyDone {
			break
		}
	}
	if !data.BuyDone || data.BoughtQuantity != 1 || !data.Plan.EventPriceStress {
		t.Fatalf("fixture did not buy and persist the admitted event lot: %+v", data)
	}
	procurementReviewExpireGossip(t, runner, proxy)
	actionLogRefresh(t, h, runner)
	ship = harnessShip(t, runner, ship.Ship.ID)
	if receipts, valid := paidSpotRevenue(runner.planningInput(ship), data.Plan, 1); valid {
		t.Fatalf("expired gossip unexpectedly supplied a permanent event floor: %d", receipts)
	}
	mission.Phase = "depart"
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	mission, data = h.mission(ship.Ship.ID)
	departures := h.api.mutationsFor("/v1/fleet/depart")
	if h.api.mutationCount("/v1/market/place-order") != 1 || len(departures) != 1 ||
		departures[0].Status != http.StatusOK || mission.Phase != "sailing" || data.DepartedCargo[recoveryGrain] != 1 {
		t.Fatalf("lost event quote stranded or repurchased paid cargo: departures %+v, phase %s, data %+v", departures, mission.Phase, data)
	}
}
