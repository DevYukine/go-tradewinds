package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

func TestMarqueExposureProtectsFleetPlansAndUnderwriting(t *testing.T) {
	t.Parallel()
	state := AccountState{
		HomePortID: "home",
		Catalog: Catalog{
			Ports: []Port{
				{ID: "home", SeaZoneID: "home_zone"},
				{ID: "quay", SeaZoneID: "quay_zone"},
				{ID: "destination", SeaZoneID: "destination_zone"},
				{ID: "insured_origin", SeaZoneID: "insured_origin_zone"},
				{ID: "insured_destination", SeaZoneID: "insured_destination_zone"},
				{ID: "unused", SeaZoneID: "unused_zone"},
			},
			SeaZones: []Zone{
				{ID: "voyage_zone"}, {ID: "planned_zone"}, {ID: "return_zone"},
				{ID: "insured_zone"}, {ID: "closed_policy_zone"}, {ID: "sunk_zone"},
			},
		},
		Routes: map[string]Route{
			routeKey("quay", "destination", "hoy"): {
				FromPort: "quay", ToPort: "destination",
				Legs: []RouteLeg{{FromNode: "quay", ToNode: "planned_zone"}, {FromNode: "planned_zone", ToNode: "destination"}},
			},
			routeKey("destination", "home", "hoy"): {
				FromPort: "destination", ToPort: "home",
				Legs: []RouteLeg{{FromNode: "destination", ToNode: "return_zone"}, {FromNode: "return_zone", ToNode: "home"}},
			},
			routeKey("insured_origin", "insured_destination", ""): {
				FromPort: "insured_origin", ToPort: "insured_destination",
				Legs: []RouteLeg{{FromNode: "insured_origin", ToNode: "insured_zone"}, {FromNode: "insured_zone", ToNode: "insured_destination"}},
			},
		},
	}
	idle := ShipState{}
	idle.Ship.ID, idle.Ship.Status, idle.Ship.ShipClass, idle.Ship.CurrentPortID = "idle", "berthed", "hoy", "quay"
	sailing := ShipState{}
	sailing.Ship.ID, sailing.Ship.Status = "sailing", "sailing"
	sailing.RouteLegs = []RouteLeg{{FromNode: "voyage_zone", ToNode: "destination"}}
	sunk := ShipState{}
	sunk.Ship.ID, sunk.Ship.Status = "lost", "sunk"
	sunk.RouteLegs = []RouteLeg{{FromNode: "sunk_zone", ToNode: "unused"}}
	state.Ships = []ShipState{idle, sailing, sunk}
	plans := map[string][]Plan{"idle": {
		{Destination: "destination"},
		{Origin: "destination", Destination: "home"},
	}}
	closedAt := time.Now()
	policy := uwExecPolicy(closedAt)
	policy.OriginPortID, policy.DestPortID = "insured_origin", "insured_destination"
	positions := []store.UnderwritingPosition{
		{FrozenPolicy: encode(policy)},
		{ClosedAt: &closedAt, FrozenPolicy: encode(InsurancePolicy{OriginPortID: "unused", DestPortID: "home"})},
	}
	exposed, err := marqueExposure(state, plans, positions)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"home_zone": true, "quay_zone": true, "destination_zone": true, "voyage_zone": true,
		"planned_zone": true, "return_zone": true, "insured_origin_zone": true,
		"insured_destination_zone": true, "insured_zone": true,
	}
	if !reflect.DeepEqual(exposed, want) {
		t.Fatalf("exposed zones = %v, want %v", exposed, want)
	}
	delete(state.Routes, routeKey("quay", "destination", "hoy"))
	if _, err := marqueExposure(state, plans, positions); err == nil {
		t.Fatal("missing persisted ship route was treated as no exposure")
	}
	if _, err := marqueExposure(state, nil, []store.UnderwritingPosition{{FrozenPolicy: positions[0].FrozenPolicy}}); err != nil {
		t.Fatalf("known permanent underwriting route rejected: %v", err)
	}
	delete(state.Routes, routeKey("insured_origin", "insured_destination", ""))
	if _, err := marqueExposure(state, nil, positions); err == nil {
		t.Fatal("missing underwriting route was treated as no exposure")
	}
}

func TestMarqueExposureAllowsSailingMissionWithoutSyntheticOrigin(t *testing.T) {
	t.Parallel()
	ship := ShipState{}
	ship.Ship.ID, ship.Ship.ShipClass, ship.Ship.Status = "sailing", "hoy", "sailing"
	ship.RouteLegs = []RouteLeg{{FromNode: "voyage", ToNode: "arrival"}}
	state := AccountState{
		HomePortID: "home", Ships: []ShipState{ship},
		Catalog: Catalog{Ports: []Port{{ID: "home", SeaZoneID: "home-zone"}, {ID: "arrival", SeaZoneID: "arrival-zone"}},
			SeaZones: []Zone{{ID: "voyage"}, {ID: "future"}}},
		Routes: make(map[string]Route),
	}
	plans := map[string][]Plan{"sailing": {{Destination: "arrival"}}}
	exposed, err := marqueExposure(state, plans, nil)
	if err != nil || !exposed["voyage"] || !exposed["arrival-zone"] {
		t.Fatalf("sailing mission required an empty-origin route instead of its current legs: %v %v", exposed, err)
	}
	plans["sailing"] = append(plans["sailing"], Plan{Destination: "home"})
	if _, err := marqueExposure(state, plans, nil); err == nil || !strings.Contains(err.Error(), "committed route") {
		t.Fatalf("unknown sailing continuation geometry was treated as no exposure: %v", err)
	}
	state.Routes[routeKey("arrival", "home", "hoy")] = Route{
		FromPort: "arrival", ToPort: "home", Legs: []RouteLeg{{FromNode: "arrival", ToNode: "future"}, {FromNode: "future", ToNode: "home"}},
	}
	exposed, err = marqueExposure(state, plans, nil)
	if err != nil || !exposed["future"] {
		t.Fatalf("known future route from the observed sailing arrival was ignored: %v %v", exposed, err)
	}
	state.Ships[0].Ship.CurrentPortID = "home"
	exposed, err = marqueExposure(state, plans, nil)
	if err != nil || !exposed["future"] {
		t.Fatalf("retained departed port replaced the sailing arrival's known future route: %v %v", exposed, err)
	}
}

func TestMarqueMissionPlansRequireEverySailingContinuation(t *testing.T) {
	t.Parallel()
	firstDeadline, secondDeadline := time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)
	base := Plan{Kind: "spot", Origin: "home", Destination: "arrival"}
	cases := []struct {
		name string
		data missionData
		want map[[2]string]bool
	}{
		{name: "return", data: missionData{Plan: base}, want: map[[2]string]bool{{"arrival", "home"}: true}},
		{name: "sealed recovery", data: missionData{
			Plan:    Plan{Kind: "recovery", Destination: "arrival"},
			Freight: []freightObligation{{Destination: "second", Deadline: &firstDeadline}, {Destination: "third", Deadline: &secondDeadline}},
		}, want: map[[2]string]bool{{"arrival", "second"}: true, {"second", "third"}: true}},
		{name: "growth next lot", data: missionData{
			Plan: base, GrowthLane: &growthLaneIntent{NextPlan: &Plan{Origin: "home", Destination: "second"}},
		}, want: map[[2]string]bool{{"arrival", "home"}: true, {"home", "second"}: true}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ship := ShipState{}
			ship.Ship.ID, ship.Ship.ShipClass, ship.Ship.Status = "sailing", "hoy", "sailing"
			ship.RouteLegs = []RouteLeg{{FromNode: "voyage", ToNode: "arrival"}}
			state := AccountState{
				HomePortID: "home", Ships: []ShipState{ship}, Routes: make(map[string]Route),
				Catalog: Catalog{Ports: []Port{{ID: "home"}, {ID: "arrival"}, {ID: "second"}, {ID: "third"}},
					SeaZones: []Zone{{ID: "voyage"}, {ID: "future"}}},
			}
			plans := marqueMissionPlans(state, ship, &store.Mission{Destination: "arrival"}, &test.data)
			pairs := make(map[[2]string]bool)
			for _, plan := range plans {
				if plan.Origin != plan.Destination {
					pairs[[2]string{plan.Origin, plan.Destination}] = true
				}
			}
			if !reflect.DeepEqual(pairs, test.want) {
				t.Fatalf("persisted continuations = %v, want %v", pairs, test.want)
			}
			itinerary := map[string][]Plan{"sailing": plans}
			if _, err := marqueExposure(state, itinerary, nil); err == nil || !strings.Contains(err.Error(), "committed route") {
				t.Fatalf("unknown continuation must hold marque admission: %v", err)
			}
			for pair := range test.want {
				state.Routes[routeKey(pair[0], pair[1], "hoy")] = Route{FromPort: pair[0], ToPort: pair[1],
					Legs: []RouteLeg{{FromNode: pair[0], ToNode: "future"}, {FromNode: "future", ToNode: pair[1]}}}
			}
			exposed, err := marqueExposure(state, itinerary, nil)
			if err != nil || !exposed["future"] {
				t.Fatalf("known committed continuation was not protected: %v %v", exposed, err)
			}
		})
	}
}

func TestMarqueSourcingRouteRejectsOwnActiveOrPendingZone(t *testing.T) {
	t.Parallel()
	input := sourcingInputFixture(t)
	hull, _ := findHull(input.Catalog, "hoy")
	if _, err := sourcingRouteReady(input, "le_havre", "amsterdam", hull); err != nil {
		t.Fatalf("control route should be legal before our marque: %v", err)
	}
	input.MarqueZones = map[string]bool{"english_channel": true}
	if _, err := sourcingRouteReady(input, "le_havre", "amsterdam", hull); err == nil || !strings.Contains(err.Error(), "letter of marque") {
		t.Fatalf("sourcing failed to reject our active/pending marque in plain language: %v", err)
	}
}

func TestMarqueQuoteRefreshRetainsLastGoodAndUsesRealTimeTTL(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	var fail atomic.Bool
	runner := newRetentionRunner(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		query := request.URL.Query()
		if request.URL.Path != "/v1/fleet/marque-quote" || query.Get("added_piracy_bps") != "500" || query.Get("duration_game_hours") != "60" {
			t.Errorf("unexpected quote request: %s", request.URL)
		}
		if fail.Load() {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"code":"quote_unavailable"}`))
			return
		}
		_ = json.NewEncoder(writer).Encode(MarqueQuote{
			SeaZoneID: query.Get("sea_zone_id"), AddedPiracyBPS: 500, DurationGameHours: 60,
			CostPence: 10_000, ExpectedNetPrizePence: 17_000, LadenPassagesPerDay: 2,
		})
	}))
	catalog := Catalog{SeaZones: []Zone{{ID: "zone_a"}, {ID: "zone_b"}}}
	if err := runner.refreshMarqueQuotes(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatalf("initial quote requests = %d, want one per zone", requests.Load())
	}
	lastGood := string(runner.snapshot["marque_quotes"])
	runner.generation = "next-capture"
	if err := runner.refreshMarqueQuotes(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || !marqueQuoteFresh(runner.meta["marque_quotes/zone_a"], time.Now()) {
		t.Fatal("a new capture generation invalidated the thirty-real-minute cache")
	}
	for _, zone := range catalog.SeaZones {
		meta := runner.meta["marque_quotes/"+zone.ID]
		meta.AttemptedAt = time.Now().Add(-31 * time.Minute)
		meta.UpdatedAt = meta.AttemptedAt
		runner.meta["marque_quotes/"+zone.ID] = meta
	}
	fail.Store(true)
	if err := runner.refreshMarqueQuotes(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 || string(runner.snapshot["marque_quotes"]) != lastGood {
		t.Fatal("a failed refresh discarded the last good zone quote")
	}
	if runner.meta["marque_quotes"].Complete || marqueQuoteFresh(runner.meta["marque_quotes/zone_a"], time.Now()) {
		t.Fatal("a failed quote was admitted as current economic evidence")
	}
	if err := runner.refreshMarqueQuotes(context.Background(), catalog); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 4 {
		t.Fatal("failed quote retried before thirty real minutes elapsed")
	}
}

func TestMarqueOutcomesMergeSurvivesOmittedHistory(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	outcomes := map[string]marqueOutcome{"old": {ID: "old", CostPence: 10_000, PrizePence: 7_000}}
	marques := []letterOfMarque{
		{ID: "new", CostPence: 20_000, PrizeEarnedPence: 18_000, ExpiresAt: now},
		{ID: "active", CostPence: 30_000, PrizeEarnedPence: 5_000, ExpiresAt: now.Add(time.Hour)},
	}
	mergeMarqueOutcomes(outcomes, marques, now)
	var restored map[string]marqueOutcome
	if err := json.Unmarshal(encode(outcomes), &restored); err != nil {
		t.Fatal(err)
	}
	marques[0].PrizeEarnedPence = 19_000
	mergeMarqueOutcomes(restored, marques[:1], now.Add(8*24*time.Hour))
	if len(restored) != 2 || restored["old"].PrizePence != 7_000 || restored["new"].PrizePence != 19_000 {
		t.Fatalf("expired outcomes were duplicated, lost or not updated: %+v", restored)
	}
}

func TestMarquePendingProtectsOnlyItsHomeFundsAndZone(t *testing.T) {
	t.Parallel()
	now := time.Now()
	state := AccountState{
		HomePortID: "home", Catalog: Catalog{Ports: []Port{{ID: "home", BankingRegion: "north"}}},
	}
	state.Clock.RealUTC = now
	runner := New(nil, nil, store.Account{}, zap.NewNop(), Options{ReservePence: 75_000})
	runner.generation = "current"
	runner.meta["clock"] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: runner.generation}
	runner.snapshot["marques"] = encode([]letterOfMarque{
		{ID: "00000000-0000-4000-8000-000000000001", SeaZoneID: "active", AddedPiracyBPS: 500, CostPence: 10_000,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "00000000-0000-4000-8000-000000000002", SeaZoneID: "expired", AddedPiracyBPS: 500, CostPence: 10_000,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(-time.Hour)},
	})
	runner.snapshot["_marque_pending"] = encode(marquePending{SeaZoneID: "pending", BankingRegion: "north", CostPence: 10_000})
	input := runner.planningInputFrom(ShipState{}, state)
	if input.ReserveByRegion["north"] != 85_000 || input.ReserveByRegion["south"] != 0 {
		t.Fatalf("pending marque reserved the wrong funds: %v", input.ReserveByRegion)
	}
	if !reflect.DeepEqual(input.MarqueZones, map[string]bool{"active": true, "pending": true}) {
		t.Fatalf("active and pending marque zones = %v", input.MarqueZones)
	}
	runner.snapshot["_marque_pending"] = encode(marquePending{SeaZoneID: "unlisted-issued", IssuedID: "issued", ExpiresAt: now.Add(time.Hour)})
	input = runner.planningInputFrom(ShipState{}, state)
	if !input.MarqueZones["unlisted-issued"] || input.ReserveByRegion["north"] != 75_000 {
		t.Fatal("an issued but unlisted marque lost its zone guard or reserved its paid cost twice")
	}
	runner.snapshot["_marque_pending"] = encode(marquePending{SeaZoneID: "unlisted-issued", IssuedID: "issued", ExpiresAt: now.Add(-time.Hour)})
	if runner.planningInputFrom(ShipState{}, state).MarqueZones["unlisted-issued"] {
		t.Fatal("an authoritatively expired issued marque retained its pending zone guard")
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		mission := &store.Mission{ShipID: "__marque", Phase: "marque"}
		data := missionData{Action: &durableAction{ID: "issue", Stage: "marque"}}
		applyRejectedResult(mission, &data, status, "refused")
		if mission.Phase != "done" || data.Action != nil || data.LastActionID != "issue" {
			t.Fatalf("HTTP %d left the marque control intent active: %+v %+v", status, mission, data)
		}
	}
}

func TestMarqueDecodeRejectsMissingPricingAndPrizeEvidence(t *testing.T) {
	t.Parallel()
	quote := MarqueQuote{
		SeaZoneID: "zone", AddedPiracyBPS: 500, DurationGameHours: 60,
		CostPence: 10_000, ExpectedNetPrizePence: 17_000, LadenPassagesPerDay: 2,
	}
	for _, field := range []string{"floor_applied", "marque_bps_in_force", "laden_passages_per_day", "expected_net_prize_pence"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encode(quote), &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, field)
		var decoded MarqueQuote
		if err := json.Unmarshal(encode(fields), &decoded); err == nil {
			t.Fatalf("missing %s was treated as observed zero or false pricing evidence", field)
		}
	}
	now := time.Now().UTC()
	marque := letterOfMarque{
		ID: "00000000-0000-4000-8000-000000000003", SeaZoneID: "zone", AddedPiracyBPS: 500,
		CostPence: 10_000, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	for _, field := range []string{"cost_pence", "prize_earned_pence"} {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encode(marque), &fields); err != nil {
			t.Fatal(err)
		}
		delete(fields, field)
		var decoded letterOfMarque
		if err := json.Unmarshal(encode(fields), &decoded); err == nil {
			t.Fatalf("missing %s was treated as observed zero money", field)
		}
	}
}
