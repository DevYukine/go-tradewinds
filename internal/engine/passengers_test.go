package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func passengerInputFixture() PlanningInput {
	in := hazardInputFixture()
	in.Catalog.ShipClasses[0].PassengersPerTon = 1
	in.Contracts = nil
	in.Ship.Ship.Status = "anchored_roadstead"
	in.Metadata["marques"] = in.Metadata["zones"]
	for i := range in.Zones {
		in.Zones[i].MarquePiracyBPS = new(int64(0))
		in.Zones[i].ActiveMarques = new(int64(0))
	}
	return in
}

func TestPassengerFloorBoundaries(t *testing.T) {
	for _, test := range []struct{ cost, heads, bps, want int64 }{{1000, 10, 0, 100}, {999, 10, 100, 101}, {1, 37, 351, 1}, {1000, 1, 9999, 10_000_000}} {
		floor, err := passengerFloor(test.cost, test.heads, test.bps)
		if err != nil || floor != test.want || !passengerFareAllowed(floor, floor) || passengerFareAllowed(floor-1, floor) {
			t.Fatalf("floor %v = %d, %v", test, floor, err)
		}
		charge, ok := hazardCharge(floor*test.heads, test.bps)
		if !ok || floor*test.heads-charge < test.cost {
			t.Fatalf("fare floor fails to cover bound: %v floor %d charge %d", test, floor, charge)
		}
	}
	if _, err := passengerFloor(100, 1, 10_000); err == nil {
		t.Fatal("100% loss was priced as affordable")
	}
}

func TestPassengerLadderDeadlineAndRatio(t *testing.T) {
	for _, start := range []int64{118, 200, 1000, 10_000} {
		fare := start
		for remaining := 8.0; remaining >= 2; remaining-- {
			next := passengerNextFare(fare, 100, remaining)
			if next < 100 || next > fare || fare > 100 && next*100 > fare*85 {
				t.Fatalf("illegal ladder step %d -> %d with %.0f hours left", fare, next, remaining)
			}
			fare = next
		}
		if fare != 100 {
			t.Fatalf("ladder did not reach floor >=1gh before boarding deadline: %d", fare)
		}
	}
	if got := passengerNextFare(1000, 100, 2.75); got != 100 {
		t.Fatalf("fractional deadline did not reserve final floor hour: %d", got)
	}
}

func TestPassengerWholeGroupsAndHullDensity(t *testing.T) {
	in := passengerInputFixture()
	in.Ship.EffectiveHoldCapacityTons = 6
	in.Catalog.ShipClasses[0].PassengersPerTon = 2
	groups := []gameapi.PassengerWaitingGroup{{ID: "fitting", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}, {ID: "oversize", Kind: "emigrants", Size: 11, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}}
	small, large, _ := passengerCapacity(in.Ship, in.Catalog.ShipClasses[0], 1, groups, "destination", in.Now)
	if small != 10 || large != 10 {
		t.Fatalf("whole group capacity = %d/%d; split group or forgot stores/density", small, large)
	}
	groups = groups[1:]
	small, large, _ = passengerCapacity(in.Ship, in.Catalog.ShipClasses[0], 1, groups, "destination", in.Now)
	if small != 0 || large != 0 {
		t.Fatalf("11-head group split to fill 10 berths: %d/%d", small, large)
	}
}

func TestPassengerRouteSafetyAndProtectedCash(t *testing.T) {
	in := passengerInputFixture()
	groups := []gameapi.PassengerWaitingGroup{{ID: "whole", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}}
	quote, err := quotePassage(in, "destination", groups, nil, nil)
	if err != nil || quote.Floor <= 0 || quote.Berths != 10 {
		t.Fatalf("safe passage not quoted: %+v %v", quote, err)
	}
	in.Zones[0].NetPiracyBPS = 1000
	if _, err := quotePassage(in, "destination", groups, nil, nil); err == nil {
		t.Fatal("1000bps route accepted")
	}
	in.Zones[0].NetPiracyBPS = 999
	in.Zones[0].MarquePiracyBPS, in.Zones[0].ActiveMarques = new(int64(500)), new(int64(1))
	if _, err := quotePassage(in, "destination", groups, nil, nil); err == nil {
		t.Fatal("public foreign marque aggregate accepted despite private own-only list being empty")
	}
	own := letterOfMarque{SeaZoneID: "channel", SponsorCompanyID: in.CompanyID, AddedPiracyBPS: 500, ExpiresAt: in.Now.Add(time.Hour)}
	if err := passengerRouteSafe(in, in.Routes[routeKey("origin", "destination", in.Ship.Ship.ShipClass)], []letterOfMarque{own}); err != nil {
		t.Fatalf("own marque alone was misclassified foreign: %v", err)
	}
	in.Zones[0].MarquePiracyBPS, in.Zones[0].ActiveMarques = new(int64(0)), new(int64(0))
	in.Zones[0].NetPiracyBPS = 200
	in.Ship.Stores.ProvisionsTons = 0
	in.Balances["channel"] = 0
	if _, err := quotePassage(in, "destination", groups, nil, nil); err == nil {
		t.Fatal("unaffordable safe stores accepted")
	}
}

func passengerRunnerFixture(t *testing.T) (*recoveryHarness, *Runner, PlanningInput) {
	t.Helper()
	h := newRecoveryHarness(t)
	r := h.runner(primaryTestKey)
	defaultHandler := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/passengers/offers" {
			_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{})
			return
		}
		defaultHandler.ServeHTTP(w, req)
	})
	in := passengerInputFixture()
	in.CompanyID = h.account.CompanyID
	r.generation = in.Generation
	r.meta = in.Metadata
	for _, key := range []string{"marques", "passenger_offers", "passenger_waiting/origin", "passenger_aboard/ship-a", "insurance", "loans", "settings", "ledger", "rates", "factors", "profile", "orders", "fills", "warehouses"} {
		r.meta[key] = r.meta["zones"]
	}
	r.stateCache = accountState{Catalog: in.Catalog, Clock: in.Clock, Ships: []ShipState{in.Ship}, Markets: in.Markets, Ports: in.Ports, Routes: in.Routes, HazardQuotes: in.HazardQuotes, HomePortID: "origin", Vaults: []Vault{{AccountID: "vault", AccountType: "vault", BankingRegion: "channel", BalancePence: 10_000_000}}}
	r.snapshot["zones"] = encode(in.Zones)
	r.snapshot["marques"] = encode([]letterOfMarque{})
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{})
	r.snapshot["passenger_waiting"] = encode(map[string]any{"origin": []gameapi.PassengerWaitingGroup{{ID: "whole", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)}}})
	r.snapshot["passenger_aboard"] = encode(map[string]any{in.Ship.Ship.ID: []gameapi.PassengerAboardGroup{}})
	r.snapshot["passenger_fares"] = encode(map[string]any{"origin|destination": []gameapi.PassengerFare{}})
	return h, r, in
}

func passengerTick(t *testing.T, h *recoveryHarness, r *Runner) {
	t.Helper()
	mission, data := h.mission("ship-a")
	if changed, err := r.driveMission(h.ctx, r.stateCache.Ships[0], mission, &data); err != nil || !changed {
		t.Fatalf("passage tick phase %s action %+v: changed %t error %v", mission.Phase, data.Action, changed, err)
	}
}

func TestPassengerFleetCapAndCargoPriority(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	other := in.Ship
	other.Ship.ID = "ship-b"
	r.stateCache.Ships = append(r.stateCache.Ships, other)
	committed := &store.Mission{Data: encode(missionData{Passage: &passageIntent{}})}
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, map[string]*store.Mission{"ship-b": committed}); err != nil || changed {
		t.Fatalf("two-ship fleet exceeded cap1: %t %v", changed, err)
	}
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("idle ship did not admit passage: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	// An executable delivery charter appears after the fare mission exists.
	reward := int64(10_000)
	r.stateCache.Contracts = []Contract{{ID: "cargo", ContractType: "charter", OriginPortID: "destination", CommodityID: "provisions", RemainingQuantity: 20, RewardPerTonPence: &reward, Status: "open"}}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("cargo did not displace unboarded offer: %t %v", changed, err)
	}
	h.missingMission(in.Ship.Ship.ID)
}

func TestPassengerLadderJournalRestartExactlyTwoWrites(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	offers := make([]gameapi.PassengerOffer, 0)
	writes := []string{}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/passengers/offers" {
			page := make([]gameapi.PassengerOffer, 0, len(offers))
			for i := len(offers) - 1; i >= 0; i-- {
				page = append(page, offers[i])
				if req.URL.Query().Get("limit") == "1" {
					break
				}
			}
			_ = json.NewEncoder(w).Encode(page)
			return
		}
		if req.Header.Get("Idempotency-Key") == "" {
			t.Error("passenger mutation missing durable key")
		}
		writes = append(writes, req.URL.Path)
		switch req.URL.Path {
		case "/v1/passengers/offer":
			var body passengerOfferRequest
			_ = json.NewDecoder(req.Body).Decode(&body)
			offer := gameapi.PassengerOffer{ID: fmt.Sprintf("offer-%d", len(offers)), ShipID: body.ShipID, PortID: "origin", DestPortID: body.Destination, FarePerHeadPence: body.Fare, MaxBerths: body.MaxBerths, Status: "open", CreatedAt: time.Now().UTC()}
			offers = append(offers, offer)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(offer)
		case "/v1/passengers/cancel-offer":
			var body struct {
				OfferID string `json:"offer_id"`
			}
			_ = json.NewDecoder(req.Body).Decode(&body)
			for i := range offers {
				if offers[i].ID == body.OfferID {
					offers[i].Status = "closed"
					_ = json.NewEncoder(w).Encode(offers[i])
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected passenger write %s", req.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission("ship-a")
	data.Passage.FarePence = data.Passage.FloorPence * 10
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	passengerTick(t, h, r)
	passengerTick(t, h, r)
	r.snapshot["passenger_offers"] = encode(offers)
	_, data = h.mission("ship-a")
	oldFare := data.Passage.FarePence
	// Persist and reconstruct the runner while the first offer is open.
	replacement := h.runner(primaryTestKey)
	replacement.stateCache = r.stateCache
	replacement.snapshot, replacement.meta, replacement.generation = r.snapshot, r.meta, r.generation
	r = replacement
	mission, data = h.mission("ship-a")
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed {
		t.Fatalf("restart reposted/changed open ladder: %t %v", changed, err)
	}
	if len(writes) != 1 {
		t.Fatalf("restart created second offer: %v", writes)
	}
	// Advance a persisted full hour without sleeps or moving the game server.
	data.Passage.StepHour--
	data.Passage.StepOpenedAt = data.Passage.StepOpenedAt.Add(-gameDuration(1.1/24, in.Clock.Calendar.TimeScale))
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	passengerTick(t, h, r)
	passengerTick(t, h, r)
	r.snapshot["passenger_offers"] = encode(offers)
	passengerTick(t, h, r)
	passengerTick(t, h, r)
	if len(writes) != 3 || writes[1] != "/v1/passengers/cancel-offer" || writes[2] != "/v1/passengers/offer" {
		t.Fatalf("step is not exactly cancel/reoffer: %v", writes)
	}
	if len(offers) != 2 || offers[0].Status == "open" || offers[1].FarePerHeadPence*100 > oldFare*85 {
		t.Fatalf("invalid offer ladder: %+v", offers)
	}
	history, err := h.db.History(h.ctx, h.account.ID, "journal", 10)
	if err != nil {
		t.Fatal(err)
	}
	rows := history.([]store.JournalRow)
	if len(rows) != 3 {
		t.Fatalf("passenger writes not all journaled: %+v", rows)
	}
}

func TestPassengerDuplicateAndAmbiguousOfferNeverReposts(t *testing.T) {
	for _, ambiguous := range []bool{false, true} {
		t.Run(fmt.Sprint(ambiguous), func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			passengerTick(t, h, r)
			mission, data := h.mission("ship-a")
			var request passengerOfferRequest
			_ = json.Unmarshal(data.Action.Body, &request)
			offer := gameapi.PassengerOffer{ID: "existing-offer", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: request.Fare, MaxBerths: request.MaxBerths, Status: "open", CreatedAt: time.Now().UTC()}
			posts, gets := 0, 0
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == http.MethodGet {
					gets++
					_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{offer})
					return
				}
				posts++
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"offer_exists"}`))
			})
			if ambiguous {
				if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: data.Action.ID, AccountID: h.account.ID, Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
					t.Fatal(err)
				}
			}
			if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("reconcile: %t %v", changed, err)
			}
			if posts != map[bool]int{false: 1, true: 0}[ambiguous] || gets != 1 || data.Action != nil || data.Passage.OfferID != offer.ID {
				t.Fatalf("POST replayed instead of GET reconcile: posts%d gets%d %+v", posts, gets, data.Passage)
			}
		})
	}
}

func TestPassengerPromiseDepartureGuards(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	boarded := in.Now.Add(-time.Minute)
	promised := in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "aboard", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	data := missionData{Plan: Plan{Kind: "passage", Origin: "origin", Destination: "destination"}, Passage: &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}}}
	if err := r.passageDepartureReady(ship, "destination", &data); err != nil {
		t.Fatalf("safe promise refused: %v", err)
	}
	if err := r.passageDepartureReady(ship, "origin", &data); err == nil {
		t.Fatal("wrong destination accepted")
	}
	promised = in.Now.Add(-time.Second)
	data.Passage.Groups[0].PromisedArrivalAt = &promised
	group.PromisedArrivalAt = &promised
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	if err := r.passageDepartureReady(ship, "destination", &data); err != nil {
		t.Fatalf("already boarded passengers were stranded after their promise: %v", err)
	}
	_ = h
}

func TestPassengerPaidRestartIdempotentAndEconomicHistory(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded := in.Now.Add(-time.Minute)
	promised := in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "paid-group", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 200, FullFarePence: 2000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.StartedAt = boarded
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
	data.ProvisionsPence = 100
	data.ExpensesPence = 50
	mission.Phase = "sailing"
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	eventID := uuid.NewString()
	if err := h.db.RecordEvent(h.ctx, h.account.ID, eventID, "passengers_paid", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "group_id": group.ID, "dest_port_id": "destination", "heads": 10, "fare_paid_pence": 1800, "game_days_late": 1.0}})); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("persist measured leeway: %t %v", changed, err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed || !data.Passage.ReceiptsPrepared {
		t.Fatalf("prepare terminal receipt: %t %v %+v", changed, err, data.Passage)
	}
	// Simulate crash after the database receipt insert but before mission cleanup.
	if err := h.db.PutSettlement(h.ctx, data.Passage.Receipts[0]); err != nil {
		t.Fatal(err)
	}
	replacement := h.runner(primaryTestKey)
	replacement.stateCache = r.stateCache
	replacement.snapshot, replacement.meta, replacement.generation = r.snapshot, r.meta, r.generation
	mission, data = h.mission(in.Ship.Ship.ID)
	if changed, err := replacement.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("restart settlement: %t %v", changed, err)
	}
	h.missingMission(in.Ship.Ship.ID)
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].Kind != "passage" || rows[0].ProceedsPence != 1800 || rows[0].ProvisionsPence != 100 || rows[0].ExpensesPence != 50 || rows[0].GameSeconds != 3600 {
		t.Fatalf("passage receipt: %+v %v", rows, err)
	}
	history, err := h.db.EconomicHistoryWindow(h.ctx, h.account.ID, "origin|destination", in.Now.Add(time.Second), time.Hour)
	if err != nil || history.NetPence != 1650 || history.CommercialNetPence != 1650 || history.PositiveCycles != 1 {
		t.Fatalf("passage absent from economics: %+v %v", history, err)
	}
	evidence, err := h.db.PassengerEvidenceHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(evidence) < 2 {
		t.Fatalf("measured leeway/late cut not retained: %+v %v", evidence, err)
	}
}

func TestPassengerFloorEnforcedByOfferPreflight(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	var request passengerOfferRequest
	if err := json.Unmarshal(data.Action.Body, &request); err != nil {
		t.Fatal(err)
	}
	request.Fare = data.Passage.FloorPence
	data.Passage.FarePence = request.Fare
	data.Action.Body = encode(request)
	if err := r.passengerActionReady(h.ctx, mission, &data); err != nil {
		t.Fatalf("exact floor refused: %v", err)
	}
	request.Fare--
	data.Passage.FarePence = request.Fare
	data.Action.Body = encode(request)
	if err := r.passengerActionReady(h.ctx, mission, &data); err == nil {
		t.Fatal("floor minus one reached transport preflight")
	}
}

func TestPassengerLostAfterShipLeavesFleetKeepsKnownCosts(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded := in.Now.Add(-time.Minute)
	promised := in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "lost-group", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 200, FullFarePence: 2000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
	data.ProvisionsPence = 100
	mission.Phase = "sailing"
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, "lost-event", "passengers_lost", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "group_id": group.ID, "heads": 10, "cause": "captured"}})); err != nil {
		t.Fatal(err)
	}
	r.stateCache.Ships = nil
	for range 2 {
		if changed, err := r.reconcileMissingPassages(h.ctx); err != nil || !changed {
			t.Fatalf("lost fleet reconciliation: %t %v", changed, err)
		}
	}
	h.missingMission(in.Ship.Ship.ID)
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ProceedsPence != 0 || !rows[0].CostsKnown || rows[0].ProvisionsPence != 100 {
		t.Fatalf("captured passenger costs vanished: %+v %v", rows, err)
	}
}

func TestPassengerBoardingAndPaymentBetweenCaptures(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	boarded := time.Now().UTC()
	promised := boarded.Add(time.Hour)
	for _, event := range []struct {
		id, kind string
		payload  any
	}{
		{"fast-board", "passengers_boarded", map[string]any{"ship_id": in.Ship.Ship.ID, "group_id": "fast-group", "kind": "travellers", "origin_port_id": "origin", "dest_port_id": "destination", "heads": 10, "fare_per_head_pence": 100, "full_fare_pence": 1000, "boarded_at": boarded, "promised_arrival_at": promised}},
		{"fast-paid", "passengers_paid", map[string]any{"ship_id": in.Ship.Ship.ID, "group_id": "fast-group", "heads": 10, "fare_paid_pence": 1000}},
	} {
		if err := h.db.RecordEvent(h.ctx, h.account.ID, event.id, event.kind, encode(map[string]any{"timestamp": boarded.Add(time.Minute), "payload": event.payload})); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
			t.Fatalf("between-capture passenger recovery: %t %v", changed, err)
		}
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ProceedsPence != 1000 || rows[0].QuantityTons != 10 {
		t.Fatalf("fast boarding/payment was omitted: %+v %v", rows, err)
	}
}

func TestPassengerAmbiguousAbsentOfferReplaysSameImmutableAction(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	gets, posts := 0, 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			gets++
			_, _ = w.Write([]byte("[]"))
		} else {
			posts++
			if request.Header.Get("Idempotency-Key") != action.ID {
				t.Errorf("retry changed immutable key: %q", request.Header.Get("Idempotency-Key"))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	for range 2 {
		data.CooldownUntil = time.Time{}
		if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || changed {
			t.Fatalf("absent offer was treated as a resolved timeout: %t %v", changed, err)
		}
	}
	if gets != 2 || posts < 2 || data.Action == nil || data.Action.ID != action.ID || len(h.pending()) != 1 {
		t.Fatalf("uncertain offer did not retain and retry its original action: gets %d posts %d action %+v", gets, posts, data.Action)
	}
}

func TestPassengerAlreadyClosedCancelReconcilesDone(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	offer := gameapi.PassengerOffer{ID: "cancelled-offer", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: data.Passage.FarePence, MaxBerths: data.Passage.MaxBerths, Status: "closed", CreatedAt: in.Now}
	data.Passage.OfferID = offer.ID
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{offer})
	if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID}); err != nil {
		t.Fatal(err)
	}
	gets, posts := 0, 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet {
			gets++
			_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{offer})
		} else {
			posts++
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"already_closed"}`))
		}
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || data.Passage.OfferID != "" {
		t.Fatalf("closed cancellation stranded: %t %v %+v", changed, err, data)
	}
	if gets != 1 || posts != 1 {
		t.Fatalf("closed cancel transport repeated: gets %d posts %d", gets, posts)
	}
}

func TestPassengerConflictDifferentFareAdoptsWithoutRepost(t *testing.T) {
	for _, belowFloor := range []bool{false, true} {
		t.Run(fmt.Sprint(belowFloor), func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			passengerTick(t, h, r)
			mission, data := h.mission(in.Ship.Ship.ID)
			fare := data.Passage.FloorPence + 1
			if belowFloor {
				fare = data.Passage.FloorPence - 1
			}
			offer := gameapi.PassengerOffer{ID: "different-price", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: fare, MaxBerths: data.Passage.MaxBerths, Status: "open", CreatedAt: in.Now.Add(-time.Hour)}
			posts := 0
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{offer})
					return
				}
				posts++
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"offer_exists"}`))
			})
			if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("conflict reconciliation: %t %v", changed, err)
			}
			if posts != 1 || data.Passage.OfferID != offer.ID || data.Passage.Abandon != belowFloor {
				t.Fatalf("different fare caused duplicate or unsafe adoption: posts %d %+v", posts, data.Passage)
			}
		})
	}
}

func TestPassengerOldClosedOfferCannotResolveNewAmbiguousPost(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	old := gameapi.PassengerOffer{ID: "old-closed-offer", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: data.Passage.FarePence, MaxBerths: data.Passage.MaxBerths, Status: "closed", CreatedAt: in.Now.Add(-time.Hour)}
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{old})
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{old})
	})
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil {
		t.Fatalf("stage new offer with historical baseline: %t %v", changed, err)
	}
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action == nil {
		t.Fatalf("old closed offer falsely resolved a new POST: %t %v %+v", changed, err, data)
	}
}

func TestPassengerRepriceRetryRetainsFareAndCooldown(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	mission, data := h.mission(in.Ship.Ship.ID)
	p := data.Passage
	p.FarePence, p.Reoffer, p.OfferID = p.FloorPence*8, true, "retry-offer"
	p.StepOpenedAt, p.StepHour = in.Now.Add(-time.Minute), in.Clock.Calendar.TotalGameHours-1
	open := gameapi.PassengerOffer{ID: p.OfferID, ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: p.FarePence * 100 / 85, MaxBerths: p.MaxBerths, Status: "open"}
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{open})
	fare := p.FarePence
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "passenger-cancel" {
		t.Fatalf("retry did not stage the original cancellation: %t %v %+v", changed, err, data)
	}
	if data.Passage.FarePence != fare {
		t.Fatalf("replan applied a second ladder drop: %d -> %d", fare, data.Passage.FarePence)
	}
	data.CooldownUntil = time.Now().Add(time.Minute)
	if changed, err := r.consumePassengerResult(h.ctx, mission, &data, http.StatusTooManyRequests, json.RawMessage(`{}`)); err != nil || !changed {
		t.Fatalf("429 handling: %t %v", changed, err)
	}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed || data.Action != nil || data.Passage.FarePence != fare {
		t.Fatalf("cooldown staged or repriced an action: %t %v %+v", changed, err, data)
	}
	data.CooldownUntil = time.Time{}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Passage.FarePence != fare || data.Action.Stage != "passenger-cancel" {
		t.Fatalf("retry after cooldown changed the planned fare: %t %v %+v", changed, err, data)
	}
}

func TestPassengerConflictWithoutExistingOfferReleasesRejectedIntent(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	passengerTick(t, h, r)
	mission, data := h.mission(in.Ship.Ship.ID)
	posts, gets := 0, 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			gets++
			_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{})
			return
		}
		posts++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"offer_exists"}`))
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || !data.Passage.Abandon {
		t.Fatalf("definitively rejected offer remained quarantined: %t %v %+v", changed, err, data)
	}
	if posts != 1 || gets != 1 {
		t.Fatalf("409 replayed: POST %d GET %d", posts, gets)
	}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || changed || data.CooldownUntil.IsZero() {
		t.Fatalf("definitive refusal ignored shared cooldown: %t %v", changed, err)
	}
	data.CooldownUntil = time.Time{}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("rejected unboarded intent not released: %t %v", changed, err)
	}
	h.missingMission(in.Ship.Ship.ID)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || changed {
		t.Fatalf("unchanged refused opportunity was re-posted: %t %v", changed, err)
	}
}

func TestPassengerMeasuredUnsafePromiseBlocksFutureLane(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	evidence := store.PassengerEvidence{ID: uuid.NewString(), AccountID: h.account.ID, Lane: "origin|destination", Kind: "travellers", FarePerHeadPence: 100, Outcome: "boarded", Heads: 10, GameHour: 1, LeewayGameHours: new(-0.5)}
	if err := h.db.PutPassengerEvidence(h.ctx, &evidence); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || changed {
		t.Fatalf("lane with measured negative promise leeway readmitted: %t %v", changed, err)
	}
}

func TestPassengerMinimalMultiGroupPaidReceiptsAccountBatchOnce(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded, secondBoarded, promised := in.Now.Add(-2*time.Minute), in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	groups := []gameapi.PassengerAboardGroup{
		{ID: "batch-a", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 200, FullFarePence: 2000, BoardedAt: &boarded, PromisedArrivalAt: &promised},
		{ID: "batch-b", Kind: "emigrants", Size: 20, HoldTons: 20, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 150, FullFarePence: 3000, BoardedAt: &secondBoarded, PromisedArrivalAt: &promised},
	}
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.StartedAt, data.ProvisionsPence, data.FeesPence = boarded, 100, 50
	data.Passage = &passageIntent{Groups: groups}
	for _, fare := range []int64{1800, 2700} {
		if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_paid", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "fare_paid_pence": fare, "game_days_late": 0}})); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("persist batch measurement: %t %v", changed, err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed || len(data.Passage.Receipts) != 2 {
		t.Fatalf("unidentified paid events did not reconcile: %t %v %+v", changed, err, data.Passage)
	}
	for _, receipt := range data.Passage.Receipts {
		if err := h.db.PutSettlement(h.ctx, receipt); err != nil {
			t.Fatal(err)
		}
	}
	// A restart after insertion must preserve the batch's immutable attribution.
	mission, data = h.mission(in.Ship.Ship.ID)
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("batch restart: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	var proceeds, cost, seconds, hold int64
	for _, receipt := range rows {
		if receipt.GameSeconds != 7200 || receipt.QuantityTons != 15 || receipt.RevenueBasis == nil || *receipt.RevenueBasis != "passenger_batch_allocation" {
			t.Fatalf("receipt has unmeasured or falsely attributed batch economics: %+v", receipt)
		}
		proceeds += receipt.ProceedsPence
		cost += receipt.FeesPence + receipt.ProvisionsPence + receipt.ExpensesPence
		seconds += receipt.GameSeconds
		hold += receipt.QuantityTons
	}
	if err != nil || len(rows) != 2 || proceeds != 4500 || cost != 150 || seconds != 14400 || hold != 30 {
		t.Fatalf("batch attributed guessed or repeated economics: rows %+v totals %d/%d/%d/%d error %v", rows, proceeds, cost, seconds, hold, err)
	}
	growth, err := h.db.GrowthHistory(h.ctx, h.account.ID, "origin|destination", in.Now.Add(time.Second))
	if err != nil || growth.UnmeasuredDuration || growth.PositiveCycles != 2 || growth.CommercialNetPence != 4350 {
		t.Fatalf("measured batch poisoned profitable growth evidence: %+v %v", growth, err)
	}
}

func TestPassengerQuoteIgnoresGroupLeavingBeforeFullHour(t *testing.T) {
	in := passengerInputFixture()
	groups := []gameapi.PassengerWaitingGroup{
		{ID: "too-late", Kind: "travellers", Size: 1, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(gameDuration(0.5/24, in.Clock.Calendar.TimeScale))},
		{ID: "viable", Kind: "travellers", Size: 10, OriginPortID: "origin", DestPortID: "destination", BoardBy: in.Now.Add(time.Hour)},
	}
	quote, err := quotePassage(in, "destination", groups, nil, nil)
	if err != nil || quote.Berths != 10 || !quote.BoardBy.Equal(groups[1].BoardBy) {
		t.Fatalf("expiring group poisoned viable whole-group offer: %+v %v", quote, err)
	}
}

func TestPassengerClosureWaitsForBoardedPromiseCapture(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.Passage = &passageIntent{Abandon: true}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	if changed, err := r.driveMission(h.ctx, ship, mission, &data); err != nil || changed {
		t.Fatalf("unobserved boarded promises were discarded: %t %v", changed, err)
	}
	if saved, _, err := r.loadMission(h.ctx, ship.Ship.ID); err != nil || saved == nil {
		t.Fatalf("occupied passenger mission deleted: %v", err)
	}
}

func TestPassengerFleetCapCountsAliveAndAvoidsOwnLaneCompetition(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	other := in.Ship
	other.Ship.ID = "other"
	r.stateCache.Ships = append(r.stateCache.Ships, other)
	for i := range 2 {
		sunk := in.Ship
		sunk.Ship.ID, sunk.Ship.Status = fmt.Sprintf("sunk-%d", i), "sunk"
		r.stateCache.Ships = append(r.stateCache.Ships, sunk)
	}
	existing := &store.Mission{Data: encode(missionData{Plan: Plan{Origin: "origin", Destination: "destination"}, Passage: &passageIntent{}})}
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, map[string]*store.Mission{"other": existing}); err != nil || changed {
		t.Fatalf("sunk hulls enlarged the passenger cap: %t %v", changed, err)
	}
	// Four live hulls permit two commitments, but not competing fare ladders
	// from our own fleet on the same origin/destination.
	for i := 2; i < 4; i++ {
		r.stateCache.Ships[i].Ship.Status = "berthed"
	}
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, map[string]*store.Mission{"other": existing}); err != nil || changed {
		t.Fatalf("own fare ladder competed for the same waiting groups: %t %v", changed, err)
	}
}

func TestPassengerRecoveryRetainsFreightDepartureGuard(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.PassengerHoldTons = 10
	r.stateCache.Ships[0] = ship
	boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	group := gameapi.PassengerAboardGroup{ID: "recover-freight", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised}
	r.snapshot["passenger_aboard"] = encode(map[string]any{ship.Ship.ID: []gameapi.PassengerAboardGroup{group}})
	r.stateCache.Contracts = []Contract{{ID: "sealed", ContractType: "freight", Status: "active", CarrierCompanyID: h.account.CompanyID, CarrierShipID: ship.Ship.ID, DestPortID: "destination", QuantityRequired: 1}}
	if changed, err := r.recoverPassage(h.ctx, ship); err != nil || !changed {
		t.Fatalf("recover mixed obligations: %t %v", changed, err)
	}
	_, data := h.mission(ship.Ship.ID)
	if len(data.Freight) != 1 || data.Freight[0].ContractID != "sealed" {
		t.Fatalf("passenger recovery dropped freight seal: %+v", data.Freight)
	}
	if err := r.passageDepartureReady(ship, "destination", &data); err == nil {
		t.Fatal("safe passenger promise bypassed unknown sealed-freight deadline")
	}
}

func TestPassengerAmbiguousOfferQuarantinesOnlyItsShip(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
		t.Fatalf("admit: %t %v", changed, err)
	}
	passengerTick(t, h, r)
	_, data := h.mission(in.Ship.Ship.ID)
	action := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	other := in.Ship
	other.Ship.ID = "other-free-ship"
	r.stateCache.Ships = append(r.stateCache.Ships, other)
	otherMission, otherData := r.newVoyage(other, Plan{Kind: "passage", ShipID: other.Ship.ID, Origin: "origin", Destination: "destination"})
	otherData.Passage = &passageIntent{Abandon: true}
	if err := r.saveMission(h.ctx, otherMission, &otherData); err != nil {
		t.Fatal(err)
	}
	// Keep unrelated company controllers idle; the observable work is the
	// other ship releasing a rejected, unboarded passage.
	treasury := &store.Mission{AccountID: h.account.ID, ShipID: "__treasury", Phase: "settings", Generation: uuid.NewString()}
	treasuryData := missionData{CooldownUntil: time.Now().Add(time.Minute)}
	if err := r.saveMission(h.ctx, treasury, &treasuryData); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ships", "contracts", "vaults", "clock", "catalog", "factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
		r.meta[key] = r.meta["zones"]
	}
	gets, posts := 0, 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			posts++
			if req.Header.Get("Idempotency-Key") != action.ID {
				t.Errorf("ambiguous offer changed key: %s", req.Header.Get("Idempotency-Key"))
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		} else {
			gets++
		}
		_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{})
	})
	if changed, err := r.work(h.ctx); err != nil || !changed {
		t.Fatalf("pending fare request froze unrelated ship: %t %v", changed, err)
	}
	h.missingMission(other.Ship.ID)
	if pendingMission, pendingData, err := r.loadMission(h.ctx, in.Ship.Ship.ID); err != nil || pendingMission == nil || pendingData.Action == nil || pendingData.Action.ID != action.ID {
		t.Fatalf("ambiguous ship was not kept quarantined: %v %+v", err, pendingData)
	}
	if gets != 1 || posts < 1 {
		t.Fatalf("ambiguous request was not reconciled then retried: GET %d POST %d", gets, posts)
	}
}

func TestPassengerPendingCancelGETOpenPermitsSafeNewCancel(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: "origin", Destination: "destination"})
	offer := gameapi.PassengerOffer{ID: "open-cancel-target", ShipID: in.Ship.Ship.ID, PortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, MaxBerths: 10, Status: "open"}
	data.Passage = &passageIntent{OfferID: offer.ID, Abandon: true}
	r.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{offer})
	if err := r.stage(h.ctx, mission, &data, in.Ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID}); err != nil {
		t.Fatal(err)
	}
	old := data.Action
	if _, err := r.db.BeginAction(h.ctx, &store.Action{ID: old.ID, AccountID: h.account.ID, Method: old.Method, Path: old.Path, Body: old.Body}); err != nil {
		t.Fatal(err)
	}
	posts := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]gameapi.PassengerOffer{offer})
			return
		}
		posts++
		if req.URL.Path != "/v1/passengers/cancel-offer" || req.Header.Get("Idempotency-Key") == old.ID {
			t.Errorf("GET-open reconciliation replayed an unsafe request: %s", req.URL.Path)
		}
		offer.Status = "closed"
		_ = json.NewEncoder(w).Encode(offer)
	})
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || posts != 0 {
		t.Fatalf("GET-open did not reconcile original cancellation: %t %v %+v", changed, err, data)
	}
	if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed || data.Action == nil || data.Action.ID == old.ID {
		t.Fatalf("safe new cancellation not staged: %t %v %+v", changed, err, data)
	}
	if changed, err := r.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || posts != 1 {
		t.Fatalf("safe cancellation failed: %t %v posts%d", changed, err, posts)
	}
}

func TestPassengerOfferHistoryStopsAtPreSendFence(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	requests := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if req.Method != http.MethodGet || req.URL.Query().Get("limit") != "200" {
			t.Errorf("unexpected history read %s %s", req.Method, req.URL)
		}
		page := make([]gameapi.PassengerOffer, 200)
		for i := range page {
			page[i] = gameapi.PassengerOffer{ID: fmt.Sprintf("history-%d", i), ShipID: in.Ship.Ship.ID}
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	offers, err := r.passengerOfferHistory(h.ctx, in.Ship.Ship.ID, "history-1", false, false)
	if err != nil || requests != 1 || len(offers) != 1 || offers[0].ID != "history-0" {
		t.Fatalf("history replayed the hull's older ladder: requests%d offers%+v err%v", requests, offers, err)
	}
}

func TestPassengerAuthorizationRejectionUsesSharedStopAndDurableLatch(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, r, in := passengerRunnerFixture(t)
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || !changed {
				t.Fatalf("admit: %t %v", changed, err)
			}
			passengerTick(t, h, r)
			mission, data := h.mission(in.Ship.Ship.ID)
			if _, err := r.consumePassengerResult(h.ctx, mission, &data, status, json.RawMessage(`{"code":"missing_scope"}`)); err == nil || data.Action != nil || data.CooldownUntil.IsZero() || !data.Passage.Abandon {
				t.Fatalf("authorization refusal did not stop through shared handling: %v %+v", err, data)
			}
			// The immutable refusal survives releasing the no-longer-useful intent.
			data.CooldownUntil = time.Time{}
			if changed, err := r.driveMission(h.ctx, in.Ship, mission, &data); err != nil || !changed {
				t.Fatalf("refused intent release: %t %v", changed, err)
			}
			if changed, err := r.considerPassage(h.ctx, in.Ship, in, nil); err != nil || changed {
				t.Fatalf("same forbidden opportunity was re-admitted: %t %v", changed, err)
			}
		})
	}
}

func TestPassengerOverdueFirstCannotHideLaterPaidBatchReceipt(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	groups := []gameapi.PassengerAboardGroup{
		{ID: "overdue-a", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised},
		{ID: "paid-b", Kind: "travellers", Size: 10, HoldTons: 10, OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 100, FullFarePence: 1000, BoardedAt: &boarded, PromisedArrivalAt: &promised},
	}
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.StartedAt, data.ProvisionsPence, data.Passage = boarded, 100, &passageIntent{Groups: groups}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_lost", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "cause": "overdue"}})); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed {
		t.Fatalf("measurement: %t %v", changed, err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || changed || data.Passage.ReceiptsPrepared {
		t.Fatalf("single overdue event prematurely completed paid batch: %t %v %+v", changed, err, data.Passage)
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_paid", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "fare_paid_pence": 1000}})); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed || len(data.Passage.Receipts) != 2 {
		t.Fatalf("late paid event not retained: %t %v %+v", changed, err, data.Passage)
	}
	var proceeds, cost int64
	for _, receipt := range data.Passage.Receipts {
		proceeds += receipt.ProceedsPence
		cost += receipt.ProvisionsPence
	}
	if proceeds != 1000 || cost != 100 {
		t.Fatalf("mixed batch lost money attribution: proceeds%d costs%d", proceeds, cost)
	}
}

func TestPassengerMinimalBoardingEventUsesUniqueObservedWholeGroup(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	started, boarded, promised := in.Now.Add(-2*time.Minute), in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.StartedAt = started
	data.Passage = &passageIntent{StepGroups: r.passengerGroups("origin")}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_boarded", encode(map[string]any{"timestamp": boarded, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "heads": 10, "fare_per_head_pence": 100, "promised_arrival_at": promised}})); err != nil {
		t.Fatal(err)
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_paid", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "fare_paid_pence": 1000}})); err != nil {
		t.Fatal(err)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed || len(data.Passage.Groups) != 1 || data.Passage.Groups[0].ID != "whole" {
		t.Fatalf("minimal boarding did not bind the unique observed group: %t %v %+v", changed, err, data.Passage)
	}
	if changed, err := r.syncPassage(h.ctx, in.Ship, mission, &data); err != nil || !changed || len(data.Passage.Receipts) != 1 || data.Passage.Receipts[0].ProceedsPence != 1000 {
		t.Fatalf("between-capture minimal events lost actual fare: %t %v %+v", changed, err, data.Passage)
	}
}

func TestPassengerWholeShipLossMaySettleOneUnidentifiedBatch(t *testing.T) {
	h, r, in := passengerRunnerFixture(t)
	boarded, promised := in.Now.Add(-time.Minute), in.Now.Add(time.Hour)
	mission, data := r.newVoyage(in.Ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: in.Ship.Ship.ID})
	data.StartedAt, data.ProvisionsPence = boarded, 100
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{
		{ID: "lost-batch-a", Kind: "travellers", Size: 10, HoldTons: 10, BoardedAt: &boarded, PromisedArrivalAt: &promised},
		{ID: "lost-batch-b", Kind: "travellers", Size: 10, HoldTons: 10, BoardedAt: &boarded, PromisedArrivalAt: &promised},
	}}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, uuid.NewString(), "passengers_lost", encode(map[string]any{"timestamp": in.Now, "payload": map[string]any{"ship_id": in.Ship.Ship.ID, "cause": "captured"}})); err != nil {
		t.Fatal(err)
	}
	absent := ShipState{}
	absent.Ship.ID, absent.Ship.Status = in.Ship.Ship.ID, "sunk"
	if changed, err := r.syncPassage(h.ctx, absent, mission, &data); err != nil || !changed || len(data.Passage.Receipts) != 1 {
		t.Fatalf("whole-ship loss did not settle retained batch: %t %v %+v", changed, err, data.Passage)
	}
	receipt := data.Passage.Receipts[0]
	if receipt.ProceedsPence != 0 || receipt.ProvisionsPence != 100 || !receipt.CostsKnown || receipt.GameSeconds != 3600 || receipt.QuantityTons != 20 {
		t.Fatalf("whole-ship loss discarded known economics: %+v", receipt)
	}
}
