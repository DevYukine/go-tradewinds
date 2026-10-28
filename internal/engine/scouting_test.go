package engine

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestBookObservedKeepsLastGoodAcrossFailureAndDepartureAndPrunes(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	var failBook atomic.Bool
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/world/monopolies" {
			h.api.writeJSON(writer, http.StatusOK, []any{})
			return
		}
		if failBook.Load() && request.URL.Path == "/v1/market/book" {
			h.api.writeProblem(writer, http.StatusNotFound, "book_unavailable", "synthetic book failure")
			return
		}
		base.ServeHTTP(writer, request)
	})
	runner := refreshedRunner(t, h)
	observed := decode[map[string]time.Time](runner.snapshot, "_book_observed")
	first := observed[recoveryHomePort]
	if first.IsZero() || !first.Equal(runner.meta["books/"+recoveryHomePort].UpdatedAt) {
		t.Fatalf("last complete book timestamp disagrees with capture: observed=%v meta=%+v", first, runner.meta["books/"+recoveryHomePort])
	}
	failBook.Store(true)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	observed = decode[map[string]time.Time](runner.snapshot, "_book_observed")
	if runner.meta["books/"+recoveryHomePort].Error == "" || !observed[recoveryHomePort].Equal(first) {
		t.Fatal("failed capture changed the last-good book observation")
	}
	observed[recoveryFarPort] = time.Now().UTC().Add(-25 * time.Hour)
	observed["not-in-catalog"] = time.Now().UTC()
	runner.snapshot["_book_observed"] = encode(observed)
	h.api.setFleet([]map[string]any{sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)})
	failBook.Store(false)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	observed = decode[map[string]time.Time](runner.snapshot, "_book_observed")
	if runner.stateCache.Clock.Calendar.TimeScale != 1 || observed[recoveryFarPort].IsZero() {
		t.Fatalf("a slow game clock must retain observations past 24 real hours: %+v", observed)
	}
	observed[recoveryFarPort] = time.Now().UTC().Add(-168*time.Hour - time.Second)
	runner.snapshot["_book_observed"] = encode(observed)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	observed = decode[map[string]time.Time](runner.snapshot, "_book_observed")
	if !observed[recoveryHomePort].Equal(first) || len(observed) != 1 {
		t.Fatalf("departure lost the retained observation or failed to prune: %+v", observed)
	}
	if _, exists := runner.meta["books/"+recoveryHomePort]; exists {
		t.Fatal("book capture metadata should be gone after departure")
	}
	if logs.Len() != 0 {
		t.Fatalf("book observation maintenance emitted logs: %+v", logs.All())
	}
}

func TestScoutHistoryLoadsCompletionLanesForStationaryOrigins(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	in := scoutAcceptanceInput(t)
	runner.stateCache.Ships = []ShipState{in.Ship}
	completed := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	for _, hint := range []struct{ target, commodity string }{
		{"amsterdam", "copper_sheathing"},
		{"flushing", "salted_meat"},
	} {
		expense := int64(0)
		lane := scoutLaneKey(hint.target, "hamburg", hint.commodity)
		putProbeHistorySettlement(t, h, store.Settlement{
			ID: "scout-lane-" + hint.target, Kind: "relocation", ShipID: in.Ship.Ship.ID,
			Origin: "hamburg", Destination: hint.target, Commodity: hint.commodity,
			Lane: lane, CompletedAt: completed, ExplorationExpensePence: &expense,
		})
	}
	if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"amsterdam|hamburg|copper_sheathing|scout", "flushing|hamburg|salted_meat|scout"} {
		if !in.SourcingProbeLanes[lane] || !in.SourcingProbeCompletions[lane].Equal(completed) {
			t.Fatalf("stationary origin omitted scout completion lane %q: %+v", lane, in.SourcingProbeCompletions)
		}
	}
}

func TestScoutHistoryLoadsWithZeroSourcingAllowanceButExcludesCurrentOrigin(t *testing.T) {
	for _, zeroAllowance := range []bool{false, true} {
		t.Run(map[bool]string{false: "origin excluded", true: "zero allowance"}[zeroAllowance], func(t *testing.T) {
			h := newRecoveryHarness(t)
			in := scoutAcceptanceInput(t)
			if zeroAllowance {
				expense := int64(20_000)
				idleFallbackSettlement(t, h, store.Settlement{
					ID: "scout-current-budget-exhausted", Kind: "relocation", CostsKnown: true,
					CompletedAt: in.Now.Add(-time.Hour), ExplorationExpensePence: &expense,
				})
			}
			in.SourcingIdleEligible = false
			home := in.Ports["hamburg"]
			ownHint := home.CoffeeHouseGossip[0]
			ownHint.SubjectPortID = "hamburg"
			home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, ownHint)
			in.Ports["hamburg"] = home
			runner := h.runner(primaryTestKey)
			runner.stateCache.Ships = []ShipState{in.Ship}
			for _, target := range []string{"amsterdam", "hamburg"} {
				expense := int64(0)
				putProbeHistorySettlement(t, h, store.Settlement{
					ID: "scout-history-" + target, Kind: "relocation", ShipID: in.Ship.Ship.ID,
					Origin: "hamburg", Destination: target, Commodity: "copper_sheathing",
					Lane: scoutLaneKey(target, "hamburg", "copper_sheathing"), CompletedAt: time.Now().UTC(),
					ExplorationExpensePence: &expense,
				})
			}
			if err := runner.loadSourcingHistory(h.ctx, &in); err != nil {
				t.Fatal(err)
			}
			if in.SourcingProbeLanes[scoutLaneKey("hamburg", "hamburg", "copper_sheathing")] {
				t.Fatal("current-origin hint entered the lane query set")
			}
			if zeroAllowance && (sourcingAllowance(in) != 0 || scoutAllowance(in) == 0) {
				t.Fatalf("fixture did not isolate positive scout allowance from exhausted sourcing: sourcing=%dp scout=%dp", sourcingAllowance(in), scoutAllowance(in))
			}
			if in.SourcingProbeCompletions[scoutLaneKey("amsterdam", "hamburg", "copper_sheathing")].IsZero() {
				t.Fatal("positive scout allowance omitted the eligible scout lane")
			}
		})
	}
}

func TestBookObservedDoesNotRecordNonQuayCapture(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/market/book" {
			h.api.writeJSON(writer, http.StatusOK, map[string]any{
				"port_id": recoveryHomePort, "visibility": "gossip", "books": []any{},
			})
			return
		}
		base.ServeHTTP(writer, request)
	})
	runner := refreshedRunner(t, h)
	meta := runner.meta["books/"+recoveryHomePort]
	if !meta.Complete || meta.Error != "" {
		t.Fatalf("fixture did not capture a successful nonquay book: %+v", meta)
	}
	if observed := decode[map[string]time.Time](runner.snapshot, "_book_observed"); len(observed) != 0 {
		t.Fatalf("nonquay capture fabricated a complete quay observation: %+v", observed)
	}
}

func TestScoutFleetNonconversionCapSurvivesIdleAndRestartUntilLadenDeparture(t *testing.T) {
	in := scoutAcceptanceInput(t)
	history := probeHistoryFixture()
	for i, target := range []string{"amsterdam", "flushing"} {
		at := in.Now.Add(time.Duration(i-2) * time.Hour)
		history.settlement(store.Settlement{
			Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "hamburg", Destination: target,
			Commodity: "copper_sheathing", Lane: scoutLaneKey(target, "hamburg", "copper_sheathing"), CompletedAt: at,
		}, nil)
		history.departure(in.Ship.Ship.ID, target, at.Add(time.Minute), false, shipIdleState{})
	}
	history.releaseIdleStreak(in.Ship.Ship.ID, shipIdleState{PortID: "flushing", EarnedGameHours: sourcingIdleGameHours})
	if history.Ships[in.Ship.Ship.ID].Failures != 0 || history.ScoutFailures != 2 {
		t.Fatalf("idle must release only the existing ship streak, not fleet nonconversions: %+v", history)
	}
	var restarted sourcingProbeHistory
	if err := json.Unmarshal(encode(history), &restarted); err != nil {
		t.Fatal(err)
	}
	in.SourcingProbeHistory = restarted
	if got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam"); got.Accepted || got.Reason != failedFleetScoutReason {
		t.Fatalf("restart replenished the fleet scout allowance: %+v", got)
	}
	restarted.departure("another-ship", "hamburg", in.Now, true, shipIdleState{})
	if restarted.ScoutFailures != 2 {
		t.Fatal("a laden departure from an unscouted port released the fleet guard")
	}
	restarted.departure("another-ship", "amsterdam", in.Now.Add(time.Second), true, shipIdleState{})
	if restarted.ScoutFailures != 0 || len(restarted.Sources) != 0 {
		t.Fatalf("laden departure must release fleet guard without inventing charter source evidence: %+v", restarted)
	}
	in.SourcingProbeHistory = restarted
	if got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "portsmouth"); !got.Accepted {
		t.Fatalf("laden departure did not restore bounded scouting: %+v", got)
	}
}

func TestScoutFleetCapCannotBeRearmedByOldScoutedPorts(t *testing.T) {
	in := scoutAcceptanceInput(t)
	history := probeHistoryFixture()
	complete := func(target string, at time.Time) {
		history.settlement(store.Settlement{
			Kind: "relocation", ShipID: "scout-" + target, Origin: "hamburg", Destination: target,
			Commodity: "copper_sheathing", Lane: scoutLaneKey(target, "hamburg", "copper_sheathing"), CompletedAt: at,
		}, nil)
	}
	complete("amsterdam", in.Now.Add(-3*time.Hour))
	history.departure("trader", "amsterdam", in.Now.Add(-2*time.Hour), true, shipIdleState{})
	complete("flushing", in.Now.Add(-time.Hour))
	complete("portsmouth", in.Now.Add(-30*time.Minute))
	var restarted sourcingProbeHistory
	if err := json.Unmarshal(encode(history), &restarted); err != nil {
		t.Fatal(err)
	}
	history = restarted
	history.departure("trader", "amsterdam", in.Now.Add(-time.Minute), true, shipIdleState{})
	if history.ScoutFailures != 2 || !history.ScoutResetAt.Equal(in.Now.Add(-2*time.Hour)) {
		t.Fatalf("routine trade from an old scout port rearmed the fleet cap: %+v", history)
	}
	history.departure("trader", "flushing", in.Now, true, shipIdleState{})
	if history.ScoutFailures != 0 || !history.ScoutResetAt.Equal(in.Now) {
		t.Fatalf("a still-counted scout did not release the fleet cap: %+v", history)
	}
	complete("calais", in.Now.Add(time.Minute))
	complete("rotterdam", in.Now.Add(2*time.Minute))
	history.departure("trader", "portsmouth", in.Now.Add(3*time.Minute), true, shipIdleState{})
	if history.ScoutFailures != 2 || !history.ScoutResetAt.Equal(in.Now) {
		t.Fatalf("another pre-reset completion released newly incurred scout failures: %+v", history)
	}
	history.departure("trader", "calais", in.Now.Add(4*time.Minute), true, shipIdleState{})
	if history.ScoutFailures != 0 || len(history.Sources) != 0 {
		t.Fatalf("counted scout conversion did not release only scout controls: %+v", history)
	}
}

func TestScoutHintsDiscardSupersededAndUnknownCatalogEvidence(t *testing.T) {
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(offset.String(), func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			at := in.Ports["hamburg"].CoffeeHouseGossip[0].SnapshotAt
			in.BookObservedAt = map[string]time.Time{"amsterdam": at.Add(offset)}
			_, exists := scoutBidHints(in)["amsterdam"]["copper_sheathing"]
			if exists != (offset < 0) {
				t.Fatalf("own book must supersede equally old or older hints; offset=%v hints=%+v", offset, scoutBidHints(in))
			}
		})
	}
	in := scoutAcceptanceInput(t)
	home := in.Ports["hamburg"]
	unknown := home.CoffeeHouseGossip[0]
	unknown.SubjectPortID = "unknown-port"
	home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, unknown)
	unknown = home.CoffeeHouseGossip[0]
	unknown.Commodities = append([]GossipCommodity(nil), unknown.Commodities...)
	unknown.Commodities[0].CommodityID = "unknown-commodity"
	home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, unknown)
	in.Ports["hamburg"] = home
	in.Ports["unknown-reporter"] = PortDetail{CoffeeHouseGossip: []Gossip{home.CoffeeHouseGossip[0]}}
	in.Metadata["ports/unknown-reporter"] = sourcingFreshMeta(in.Now)
	hints := scoutBidHints(in)
	if len(hints) != 4 || len(hints["amsterdam"]) != 1 || hints["unknown-port"] != nil {
		t.Fatalf("noncatalog gossip polluted scout candidates or history lanes: %+v", hints)
	}
}

func TestScoutTargetCooldownSpansOriginsAndRecentDepartures(t *testing.T) {
	for _, source := range []string{"another scout origin", "own departure"} {
		for _, age := range []time.Duration{24*time.Minute - time.Second, 24 * time.Minute} {
			t.Run(source+"/"+age.String(), func(t *testing.T) {
				in := scoutAcceptanceInput(t)
				at := in.Now.Add(-age)
				if source == "another scout origin" {
					in.SourcingProbeCompletions[scoutLaneKey("amsterdam", "portsmouth", "salted_meat")] = at
				} else {
					in.SourcingProbeHistory.ScoutPorts = map[string]scoutPortHistory{"amsterdam": {LastDepartureAt: at}}
				}
				got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam")
				if got.Accepted != (age == 24*time.Minute) || !got.Accepted && !strings.Contains(got.Reason, "24 real minutes") {
					t.Fatalf("target cooldown evaded or exceeded its real-time boundary: %+v", got)
				}
			})
		}
	}
	in := scoutAcceptanceInput(t)
	self := in.Ship
	self.Ship.Status = "sailing"
	self.RouteLegs = []RouteLeg{{ToNode: "amsterdam"}}
	runner := &Runner{stateCache: AccountState{Ships: []ShipState{self}}}
	if got := runner.scoutBlockedPorts(in, nil)["amsterdam"]; got != "" {
		t.Fatalf("a ship's own ambiguous route must not reserve its prospective target: %q", got)
	}
}

func TestScoutExposureOmitsCharterInspectionAndCatalogValidationStaysFirst(t *testing.T) {
	in := scoutAcceptanceInput(t)
	base := Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "hamburg", Destination: "amsterdam"}
	origin, _ := findPort(in.Catalog, base.Origin)
	scout, inspection := scoutExposure(in, base), relocationCost(in, base, origin, 6)
	if !scout.Accepted || !inspection.Accepted || math.Abs(inspection.Economics.GameDays-scout.Economics.GameDays-0.25) > 1e-9 ||
		scout.Economics.ExpectedNetPence <= inspection.Economics.ExpectedNetPence {
		t.Fatalf("scouting retained the six-hour charter inspection: scout=%+v inspection=%+v", scout, inspection)
	}
	in.Catalog.ShipClasses = nil
	if got := relocationExposure(in, base); got.Reason != "sourcing hull or ports are missing from catalog" {
		t.Fatalf("charter failure obscured the earlier catalog failure: %+v", got)
	}
}

func TestScoutDesignationSkipsIndividuallyInadmissibleSmallerShips(t *testing.T) {
	for _, scenario := range []string{"convoy", "berth state", "stores fit", "ship streak"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			for i := range runner.stateCache.Ships {
				ship := &runner.stateCache.Ships[i]
				if ship.Ship.ID != recoveryShipBlastoise {
					continue
				}
				switch scenario {
				case "convoy":
					ship.Ship.ConvoyID = "bound-convoy"
				case "berth state":
					ship.Ship.Status = "repairing"
				case "stores fit":
					ship.EffectiveHoldCapacityTons, ship.Stores.ProvisionsTons = 1, 0
				case "ship streak":
					runner.sourcingHistory.Outcomes.Ships[ship.Ship.ID] = probeShipHistory{Failures: 2}
				}
			}
			changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
			if err != nil || !changed {
				t.Fatalf("ineligible smaller hull blocked the next ready scout: changed=%v reason=%q err=%v", changed, reason, err)
			}
			mission, data := h.mission(recoveryShipCharizard)
			if mission.Phase != "sourcing_fund" || data.Relocation == nil || !data.Relocation.Scout {
				t.Fatalf("fallback was not a durable scout intent: mission=%+v data=%+v", mission, data)
			}
			h.missingMission(recoveryShipBlastoise)
		})
	}
}

func TestScoutInsuranceAcceptsLowerRankedTargetAndSkipsBlockedSmallestHull(t *testing.T) {
	for _, destination := range []string{"amsterdam", "uncovered-target"} {
		t.Run(destination, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			runner.stateCache.InsurancePolicies = []InsurancePolicy{{
				ID: "scout-policy", Status: "open", InsuredCompanyID: h.account.CompanyID,
				ShipID: recoveryShipBlastoise, OriginPortID: "hamburg", DestPortID: destination,
			}}
			runner.meta["insurance"] = runner.meta["ships"]
			id, target := recoveryShipBlastoise, destination
			if destination == "uncovered-target" {
				id, target = recoveryShipCharizard, "flushing"
			}
			changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, id))
			if err != nil || !changed {
				t.Fatalf("insurance prevented an otherwise ready scout: changed=%v reason=%q err=%v", changed, reason, err)
			}
			_, data := h.mission(id)
			if data.Relocation == nil || !data.Relocation.Scout || data.Plan.Destination != target {
				t.Fatalf("wrong insurance-safe scout intent: %+v", data)
			}
			if id != recoveryShipBlastoise {
				h.missingMission(recoveryShipBlastoise)
			}
		})
	}
}

func TestScoutDesignationSkipsShipWithAcceptedButInsuranceBlockedCharter(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	cutter, _ := findHull(runner.stateCache.Catalog, "hoy")
	cutter.ID = "cutter"
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, cutter)
	for key, route := range runner.stateCache.Routes {
		if route.ShipClass != "hoy" {
			continue
		}
		route.ShipClass = cutter.ID
		cutterKey := routeKey(route.FromPort, route.ToPort, cutter.ID)
		runner.stateCache.Routes[cutterKey] = route
		runner.meta["routes/"+cutterKey] = runner.meta["routes/"+key]
	}
	for i := range runner.stateCache.Ships {
		if runner.stateCache.Ships[i].Ship.ID == recoveryShipCharizard {
			runner.stateCache.Ships[i].Ship.ShipClass = cutter.ID
		}
	}
	for i := range runner.stateCache.Catalog.Ports {
		if runner.stateCache.Catalog.Ports[i].ID == "amsterdam" {
			runner.stateCache.Catalog.Ports[i].Manufactures = []string{"sawn_planks"}
		}
	}
	runner.stateCache.Catalog.Commodities = append(runner.stateCache.Catalog.Commodities, Commodity{ID: "sawn_planks"})
	charter := sourcingReviewCharter("scout-designation-charter")
	charter.OriginPortID = "portsmouth"
	runner.stateCache.Contracts = []Contract{charter}
	for _, pair := range [][2]string{{"amsterdam", "portsmouth"}, {"portsmouth", "amsterdam"}} {
		key := routeKey(pair[0], pair[1], "hoy")
		runner.stateCache.Routes[key] = sourcingRoute(pair[0], pair[1], 30)
		runner.meta["routes/"+key] = runner.meta["ships"]
	}
	runner.stateCache.InsurancePolicies = []InsurancePolicy{{
		ID: "charter-blocking-policy", Status: "open", InsuredCompanyID: h.account.CompanyID,
		ShipID: recoveryShipBlastoise, OriginPortID: "hamburg", DestPortID: "flushing",
	}}
	runner.meta["insurance"] = runner.meta["ships"]
	blocked := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.sourcingInput(h.ctx, blocked)
	if err != nil {
		t.Fatal(err)
	}
	if plans := sourcingPairs(input); len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("smaller ship needs an accepted charter: %+v", plans)
	}
	if changed, _, err := runner.considerSourcing(h.ctx, blocked); err != nil || changed {
		t.Fatalf("insurance-blocked charter unexpectedly started: changed=%v err=%v", changed, err)
	}
	scout := harnessShip(t, runner, recoveryShipCharizard)
	input, err = runner.sourcingInput(h.ctx, scout)
	if err != nil {
		t.Fatal(err)
	}
	if plans := sourcingPairs(input); len(plans) == 0 || plans[0].Accepted {
		t.Fatalf("scout's hull must reject the charter continuation: %+v", plans)
	}
	if changed, reason, err := runner.considerSourcing(h.ctx, scout); err != nil || !changed {
		t.Fatalf("charter-only ship reserved scouting designation: changed=%v reason=%q err=%v", changed, reason, err)
	}
	_, data := h.mission(scout.Ship.ID)
	if data.Relocation == nil || !data.Relocation.Scout {
		t.Fatalf("eligible ship did not receive the scout: %+v", data)
	}
	h.missingMission(blocked.Ship.ID)
}

func TestScoutRejectedCommonHeadlinePreservesExactOrdinarySourcingAndScoutingReasons(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	runner.opts.Execute = false
	observed := make(map[string]time.Time)
	for _, port := range runner.stateCache.Catalog.Ports {
		observed[port.ID] = runner.stateCache.Clock.RealUTC
	}
	runner.snapshot["_book_observed"] = encode(observed)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := PlanVoyages(input)
	input, err = runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	charters := planSourcingRelocation(input)
	scouts := scoutCandidates(input, runner.scoutBlockedPorts(input, nil))
	if len(ordinary) == 0 || ordinary[0].Accepted || len(charters) == 0 || charters[0].Accepted || scouts[0].Accepted {
		t.Fatalf("headline fixture needs all three rejection paths: ordinary=%+v charters=%+v scouts=%+v", ordinary, charters, scouts)
	}
	want := "no profitable executable voyage from hamburg; best evaluated alternative " + ordinary[0].Commodity + " to " + ordinary[0].Destination +
		": " + ordinary[0].Reason + "; sourcing: " + charters[0].Reason + "; scouting: " + scouts[0].Reason
	for pass := range 3 {
		runner.holding = nil
		clear(runner.assessedHoldIDs)
		if _, err := runner.work(h.ctx); err != nil {
			t.Fatal(err)
		}
		var reasons []string
		for _, hold := range runner.holding {
			if hold.ShipID == ship.Ship.ID {
				reasons = append(reasons, hold.Reason)
			}
		}
		if len(reasons) != 1 || reasons[0] != want || strings.Contains(reasons[0], "expected cash net") {
			t.Fatalf("pass %d: want exactly %q, got %+v", pass, want, reasons)
		}
	}
}

func TestScoutRechecksTargetBeforeFundingProvisionAndFirstSend(t *testing.T) {
	for _, phase := range []string{"sourcing_fund", "provision"} {
		for _, invalidation := range []string{"occupied", "hint removed"} {
			t.Run(phase+"/"+invalidation, func(t *testing.T) {
				h, runner := scoutAcceptanceRunner(t)
				mission, data := scoutAcceptanceAdmit(t, h, runner)
				mission.Phase = phase
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if invalidation == "occupied" {
					runner.stateCache.Ships[0].Ship.CurrentPortID = data.Plan.Destination
				} else {
					home := runner.stateCache.Ports["hamburg"]
					home.CoffeeHouseGossip = nil
					runner.stateCache.Ports["hamburg"] = home
				}
				ship := harnessShip(t, runner, recoveryShipBlastoise)
				if err := runner.relocationSpendingReadyAtPrice(h.ctx, ship, &data, 0, 0); err == nil {
					t.Fatal("first-send spending guard accepted an invalidated scout target")
				}
				for range 3 {
					mission, data, err := runner.loadMission(h.ctx, ship.Ship.ID)
					if err != nil {
						t.Fatal(err)
					}
					if mission == nil {
						break
					}
					if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
						t.Fatal(err)
					}
				}
				h.missingMission(ship.Ship.ID)
				for _, path := range []string{"/v1/treasury/remit", "/v1/market/place-order", "/v1/fleet/depart"} {
					if h.api.mutationCount(path) != 0 {
						t.Fatalf("invalidated scout sent %s before cancellation", path)
					}
				}
			})
		}
	}
}

func TestScoutTransientEvidenceFaultsHoldFundedIntent(t *testing.T) {
	for _, phase := range []string{"sourcing_fund", "provision", "depart"} {
		for _, fault := range []string{"stale book", "stale reporter", "history not ready"} {
			t.Run(phase+"/"+fault, func(t *testing.T) {
				h, runner := scoutAcceptanceRunner(t)
				mission, data := scoutAcceptanceAdmit(t, h, runner)
				mission.Phase, data.FeesPence = phase, 100
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "stale book":
					meta := runner.meta["books/hamburg"]
					meta.Stale = true
					runner.meta["books/hamburg"] = meta
				case "stale reporter":
					home := runner.stateCache.Ports["hamburg"]
					reporter := runner.stateCache.Ports["amsterdam"]
					for i, report := range home.CoffeeHouseGossip {
						if report.SubjectPortID == data.Plan.Destination {
							reporter.CoffeeHouseGossip = append(reporter.CoffeeHouseGossip, report)
							home.CoffeeHouseGossip = append(home.CoffeeHouseGossip[:i], home.CoffeeHouseGossip[i+1:]...)
							break
						}
					}
					runner.stateCache.Ports["hamburg"], runner.stateCache.Ports["amsterdam"] = home, reporter
					meta := runner.meta["ports/amsterdam"]
					meta.Stale = true
					runner.meta["ports/amsterdam"] = meta
				case "history not ready":
					runner.sourcingHistory.Ready = false
				}
				ship := harnessShip(t, runner, recoveryShipBlastoise)
				if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || changed {
					t.Fatalf("transient fault changed a funded scout: changed=%v err=%v", changed, err)
				}
				retained, retainedData := h.mission(ship.Ship.ID)
				if retained.Phase != phase || retainedData.Plan.Destination != data.Plan.Destination || retainedData.FeesPence != 100 {
					t.Fatalf("funded scout lost its purpose or paid fees: mission=%+v data=%+v", retained, retainedData)
				}
				if retainedData.Relocation.TransientHoldSince.IsZero() {
					t.Fatal("transient hold did not persist its real-time bound")
				}
				for _, hold := range runner.holding {
					if strings.Contains(hold.Reason, "cancelled before further spending") {
						t.Fatalf("transient evidence fault was treated as invalidation: %+v", hold)
					}
				}
				if h.api.mutationCount("/v1/treasury/remit")+h.api.mutationCount("/v1/market/place-order")+h.api.mutationCount("/v1/fleet/depart") != 0 {
					t.Fatal("transient evidence fault authorized spending or departure")
				}
			})
		}
	}
}

func TestScoutMissionLookupFailureDoesNotInvalidateFundedIntent(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	mission, data := scoutAcceptanceAdmit(t, h, runner)
	data.FeesPence = 100
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	var ownLookup string
	if err := admin.QueryRowContext(h.ctx, "SELECT ship_lookup FROM missions WHERE account_id = $1", h.account.ID).Scan(&ownLookup); err != nil {
		t.Fatal(err)
	}
	h.putMission(recoveryShipLapras, "scout-lookup-fault", "maintenance_done", missionData{
		Plan: Plan{Kind: "maintenance", ShipID: recoveryShipLapras, Origin: "hamburg", Destination: "hamburg"},
	})
	var ciphertext []byte
	if err := admin.QueryRowContext(h.ctx, "SELECT data_cipher FROM missions WHERE account_id = $1 AND ship_lookup <> $2", h.account.ID, ownLookup).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	corrupted := append([]byte(nil), ciphertext...)
	corrupted[len(corrupted)-1] ^= 1
	if _, err := admin.ExecContext(h.ctx, "UPDATE missions SET data_cipher = $1 WHERE account_id = $2 AND ship_lookup <> $3", corrupted, h.account.ID, ownLookup); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipLapras); err == nil {
		t.Fatal("fixture did not fail the other mission lookup")
	}
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || changed {
		t.Fatalf("production lookup failure cancelled a funded scout: changed=%v err=%v", changed, err)
	}
	retained, retainedData := h.mission(ship.Ship.ID)
	if retained.Phase != "sourcing_fund" || retainedData.FeesPence != 100 || retainedData.Relocation.TransientHoldSince.IsZero() {
		t.Fatalf("lookup failure did not preserve the funded bounded intent: mission=%+v data=%+v", retained, retainedData)
	}
	if _, err := admin.ExecContext(h.ctx, "UPDATE missions SET data_cipher = $1 WHERE account_id = $2 AND ship_lookup <> $3", ciphertext, h.account.ID, ownLookup); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.driveMission(h.ctx, ship, retained, &retainedData); err != nil || !changed {
		t.Fatalf("recovered lookup did not resume the scout: changed=%v err=%v", changed, err)
	}
	resumed, resumedData := h.mission(ship.Ship.ID)
	if resumed.Phase != "provision" || !resumedData.Relocation.TransientHoldSince.IsZero() {
		t.Fatalf("successful progress did not clear the transient hold: mission=%+v data=%+v", resumed, resumedData)
	}
}

func TestScoutCommittedSupplyAgeAndCurrentCashSpendingCap(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	for i := range runner.stateCache.Catalog.Ports {
		if runner.stateCache.Catalog.Ports[i].ID == "flushing" {
			runner.stateCache.Catalog.Ports[i].Produces = nil
		}
	}
	home := runner.stateCache.Ports["hamburg"]
	at, ask := runner.stateCache.Clock.RealUTC.Add(-time.Minute), int64(25_743)
	home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, Gossip{
		SubjectPortID: "flushing", SnapshotAt: at,
		Commodities: []GossipCommodity{{CommodityID: "provisions", SnapshotAt: at, BestAskPence: &ask}},
	})
	runner.stateCache.Ports["hamburg"] = home
	mission, data := scoutAcceptanceAdmit(t, h, runner)
	if data.Plan.Destination != "flushing" {
		t.Fatalf("fixture did not admit the gossip-supplied target: %+v", data.Plan)
	}
	home = runner.stateCache.Ports["hamburg"]
	staleAsk := &home.CoffeeHouseGossip[len(home.CoffeeHouseGossip)-1]
	staleAsk.SnapshotAt = at.Add(-6 * time.Minute)
	staleAsk.Commodities[0].SnapshotAt = staleAsk.SnapshotAt
	runner.stateCache.Ports["hamburg"] = home
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	if admission := scoutExposure(input, data.Plan); !admission.Accepted {
		t.Fatalf("expired provisions supply gossip still blocked a both-way provisioned scout: %+v", admission)
	}
	costed := costCommittedRelocation(input, &data)
	if !costed.Accepted {
		t.Fatalf("committed costing rejected a both-way provisioned scout: %+v", costed)
	}
	if err := runner.scoutTargetReady(h.ctx, input, data.Plan); err != nil {
		t.Fatalf("committed target recheck reapplied provisions admission: %v", err)
	}
	if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("aged supply gossip prevented the committed scout from progressing: changed=%v err=%v", changed, err)
	}
	retained, retainedData := h.mission(ship.Ship.ID)
	if retained.Phase != "provision" {
		t.Fatalf("committed scout lost its purpose after admission gossip expired: %+v", retained)
	}
	target, _ := findPort(input.Catalog, data.Plan.Destination)
	input.ReserveByRegion = sourcingReserves(input, target)
	exposure := -costed.Economics.ExpectedNetPence
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.BankingRegion == "north" {
			vault.BalancePence = input.ReserveByRegion["north"] + 5*exposure
		}
	}
	input, err = runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	input.ReserveByRegion = sourcingReserves(input, target)
	if scoutAllowance(input) >= exposure {
		t.Fatal("fixture did not lower the runner's current risk capital below committed exposure")
	}
	if err := runner.scoutTargetReady(h.ctx, input, data.Plan); err != nil {
		t.Fatalf("target evidence incorrectly includes the spending allowance: %v", err)
	}
	retained.Phase = "depart"
	if err := runner.saveMission(h.ctx, retained, &retainedData); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		mission, data, err := runner.loadMission(h.ctx, ship.Ship.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			break
		}
		if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	h.missingMission(ship.Ship.ID)
	found := false
	for _, hold := range runner.holding {
		found = found || strings.Contains(hold.Reason, "cancelled before further spending: ") && strings.Contains(hold.Reason, "current scout allowance")
	}
	if !found {
		t.Fatalf("current cash shortfall did not cancel through the production spending gate: %+v", runner.holding)
	}
	if h.api.mutationCount("/v1/fleet/depart") != 0 {
		t.Fatal("current cash shortfall authorized departure")
	}
}

func TestScoutSpendingGateFaultsHoldWithoutAdvancing(t *testing.T) {
	for _, stage := range []string{"funding", "provision", "dock"} {
		t.Run(stage, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			for i := range runner.stateCache.Ships {
				runner.stateCache.Ships[i].Stores.ProvisionsTons = 0
				if stage == "provision" {
					runner.stateCache.Ships[i].Ship.Status = "berthed"
				}
			}
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			if stage != "funding" {
				mission.Phase = "provision"
			}
			phase := mission.Phase
			data.FeesPence = 100
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			meta := runner.meta["ledger"]
			meta.Stale = true
			runner.meta["ledger"] = meta
			for range 2 {
				mission, data = h.mission(recoveryShipBlastoise)
				if changed, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil || changed {
					t.Fatalf("%s spending fault advanced or cancelled the scout: changed=%v err=%v", stage, changed, err)
				}
			}
			retained, retainedData := h.mission(recoveryShipBlastoise)
			if retained.Phase != phase || retainedData.FeesPence != 100 || retainedData.Action != nil ||
				retainedData.Relocation.TransientHoldSince.IsZero() || retainedData.Plan.Destination != data.Plan.Destination {
				t.Fatalf("%s spending fault lost its bounded funded purpose: mission=%+v data=%+v", stage, retained, retainedData)
			}
			found := false
			for _, hold := range runner.holding {
				if strings.Contains(hold.Reason, "cancelled before further spending") {
					t.Fatalf("%s transient spending fault aborted: %+v", stage, hold)
				}
				found = found || strings.Contains(hold.Reason, "risk capital requires fresh ledger")
			}
			if !found {
				t.Fatalf("%s fixture did not reach its production spending gate: %+v", stage, runner.holding)
			}
			if h.api.mutationCount("/v1/treasury/remit")+h.api.mutationCount("/v1/market/place-order")+h.api.mutationCount("/v1/fleet/dock") != 0 {
				t.Fatal("stale spending authority authorized a mutation")
			}
		})
	}
}

func TestScoutTransientHoldExpiresAfterThirtyRealMinutesAcrossReload(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(fmt.Sprint(recovered), func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			data.FeesPence = 100
			destinationSeedAction(t, h, "scout-paid-remit", "/v1/treasury/remit", `{"from_region":"south","to_region":"north","gross_amount_pence":10000}`, `{"fee_pence":100}`)
			data.LastActionID = "scout-paid-remit"
			meta := runner.meta["books/hamburg"]
			meta.Stale = true
			runner.meta["books/hamburg"] = meta
			if changed, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil || changed {
				t.Fatalf("initial transient did not hold: changed=%v err=%v", changed, err)
			}
			mission, data = h.mission(recoveryShipBlastoise)
			if data.Relocation.TransientHoldSince.IsZero() {
				t.Fatal("hold deadline was not durable")
			}
			data.Relocation.TransientHoldSince = time.Now().UTC().Add(-31 * time.Minute)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if recovered {
				meta.Stale = false
				runner.meta["books/hamburg"] = meta
			}
			for range 3 {
				mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
				if err != nil {
					t.Fatal(err)
				}
				if mission == nil {
					break
				}
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			h.missingMission(recoveryShipBlastoise)
			found := false
			for _, hold := range runner.holding {
				found = found || strings.Contains(hold.Reason, "cancelled before further spending: scouting evidence remained unavailable for more than 30 real minutes")
			}
			if !found || h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatalf("expired scout hold was not cancelled without departure: %+v", runner.holding)
			}
		})
	}
}

func TestScoutPermanentSpendingFailuresAbortInsteadOfHolding(t *testing.T) {
	for _, fault := range []string{"expense cap", "depart stores", "relocation cost", "negative customs", "funding ordinary priority"} {
		t.Run(fault, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			data.FeesPence = 100
			destinationSeedAction(t, h, "scout-paid-remit", "/v1/treasury/remit", `{"from_region":"south","to_region":"north","gross_amount_pence":10000}`, `{"fee_pence":100}`)
			data.LastActionID = "scout-paid-remit"
			var reason string
			switch fault {
			case "expense cap":
				data.Relocation.ExpenseCapPence = 1
				reason = "durable exploration cap"
			case "depart stores":
				mission.Phase = "depart"
				data.Plan.TargetStoresTons = 9
				reason = "both-way safe stores"
			case "relocation cost":
				mission.Phase = "provision"
				target := runner.stateCache.Ports[data.Plan.Destination]
				target.EffectiveBerths = 0
				runner.stateCache.Ports[data.Plan.Destination] = target
				reason = "berth"
			case "negative customs":
				mission.Phase = "depart"
				book := runner.stateCache.Markets["hamburg"]
				book.CustomsBPS = -1
				runner.stateCache.Markets["hamburg"] = book
				reason = "valid buyer customs"
			case "funding ordinary priority":
				target := data.Plan.Destination
				runner.stateCache.Markets[target] = sourcingFixture[Market](t, fmt.Sprintf(`{"port_id":%q,"visibility":"quay","customs_bps":0,"books":[{"commodity_id":%q,"depth":{"asks":[],"bids":[{"order_id":"new-demand","price_pence":200000,"remaining_quantity":100}]}}]}`, target, data.Plan.Commodity))
				runner.meta["books/"+target] = runner.meta["books/hamburg"]
				input, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
				if err != nil {
					t.Fatal(err)
				}
				plans := PlanVoyages(input)
				if len(plans) == 0 || !plans[0].Accepted {
					t.Fatalf("fixture has no accepted ordinary voyage: %+v", plans)
				}
				reason = ordinarySourcingPriorityReason
			}
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				mission, data, err := runner.loadMission(h.ctx, recoveryShipBlastoise)
				if err != nil {
					t.Fatal(err)
				}
				if mission == nil {
					break
				}
				if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
					t.Fatal(err)
				}
			}
			h.missingMission(recoveryShipBlastoise)
			found := false
			for _, hold := range runner.holding {
				found = found || strings.Contains(hold.Reason, "cancelled before further spending: ") && strings.Contains(hold.Reason, reason)
			}
			if !found {
				t.Fatalf("permanent %s failure did not reach its abort path: %+v", fault, runner.holding)
			}
			if h.api.mutationCount("/v1/treasury/remit")+h.api.mutationCount("/v1/market/place-order")+h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("permanent failure authorized more spending or departure")
			}
		})
	}
}

func bookRefreshRunner(t *testing.T, idleHours int64) (*recoveryHarness, *Runner) {
	t.Helper()
	h, runner := scoutAcceptanceRunner(t)
	home := runner.stateCache.Ports["hamburg"]
	home.CoffeeHouseGossip = nil
	runner.stateCache.Ports["hamburg"] = home
	idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
	for id, observed := range idle {
		observed.EarnedGameHours = idleHours
		idle[id] = observed
	}
	runner.snapshot["_ship_idle"] = encode(idle)
	return h, runner
}

func TestBookRefreshRanksImportOverlapBookAgeThenExposure(t *testing.T) {
	for _, ranking := range []string{"import overlap", "absent book", "oldest book", "lower exposure"} {
		t.Run(ranking, func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			in.BookObservedAt = make(map[string]time.Time)
			for i := range in.Catalog.Ports {
				in.Catalog.Ports[i].Imports = []string{"provisions"}
				in.BookObservedAt[in.Catalog.Ports[i].ID] = in.Now.Add(-8 * time.Minute)
			}
			want := "amsterdam"
			switch ranking {
			case "import overlap":
				in.Catalog.Ports[1].Imports = append(in.Catalog.Ports[1].Imports, "copper_sheathing")
				delete(in.BookObservedAt, "portsmouth")
			case "absent book":
				delete(in.BookObservedAt, "portsmouth")
				want = "portsmouth"
			case "oldest book":
				in.BookObservedAt["amsterdam"] = in.Now.Add(-9 * time.Minute)
			case "lower exposure":
				for _, pair := range [][2]string{{"hamburg", "portsmouth"}, {"portsmouth", "hamburg"}} {
					in.Routes[routeKey(pair[0], pair[1], "hoy")] = sourcingRoute(pair[0], pair[1], 5)
				}
				want = "portsmouth"
			}
			plans := bookRefreshCandidates(in, nil)
			if len(plans) == 0 || !plans[0].Accepted || plans[0].Destination != want {
				t.Fatalf("%s did not rank %s first: %+v", ranking, want, plans)
			}
			if !isBookRefresh(plans[0]) || plans[0].Quantity != 0 || plans[0].Economics.RevenuePence != 0 ||
				!strings.Contains(plans[0].Reason, "no profit evidence; buys a fresh quay book") ||
				!strings.Contains(plans[0].Reason, "scout allowance") {
				t.Fatalf("refresh invented profit evidence or lost its reason: %+v", plans[0])
			}
		})
	}
}

func TestBookRefreshPreservesLegalBlockedAllowanceAndFailureGuards(t *testing.T) {
	for _, guard := range []string{"blocked", "illegal route", "fleet failures", "ship failures", "unknown cost", "cap exact", "cap one penny short"} {
		t.Run(guard, func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			blocked := make(map[string]string)
			want := false
			wantReason := ""
			switch guard {
			case "blocked":
				wantReason = "another ship is already heading to amsterdam"
				blocked["amsterdam"] = wantReason
			case "illegal route":
				key := routeKey("hamburg", "amsterdam", "hoy")
				route := in.Routes[key]
				route.Legs = append([]RouteLeg(nil), route.Legs...)
				route.Legs[0].EdgeClass = "oceanic"
				in.Routes[key] = route
			case "fleet failures":
				in.SourcingProbeHistory.ScoutFailures = 2
				wantReason = failedFleetScoutReason
			case "ship failures":
				in.SourcingProbeHistory.Ships[in.Ship.Ship.ID] = probeShipHistory{Failures: 2}
				wantReason = failedProbeStreakReason
			case "unknown cost":
				in.SourcingProbeCostsUnknown = true
			case "cap exact", "cap one penny short":
				baseline := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam")
				if !baseline.Accepted {
					t.Fatalf("baseline refresh rejected: %+v", baseline)
				}
				allowance := -baseline.Economics.ExpectedNetPence
				want = guard == "cap exact"
				if !want {
					allowance--
				}
				in.Balances["north"] = in.ReserveByRegion["north"] + 50*allowance
			}
			plan := scoutAcceptancePlan(t, bookRefreshCandidates(in, blocked), "amsterdam")
			if plan.Accepted != want || wantReason != "" && plan.Reason != wantReason {
				t.Fatalf("%s refresh gate: accepted=%v want=%v plan=%+v", guard, plan.Accepted, want, plan)
			}
			if (guard == "unknown cost" || guard == "cap one penny short") && !strings.Contains(plan.Reason, "scout allowance") {
				t.Fatalf("refresh budget rejection hid the scout allowance: %+v", plan)
			}
		})
	}
}

func TestBookRefreshSixHourBoundaryCreatesOnlyTheDesignatedMission(t *testing.T) {
	for _, hours := range []int64{5, 6} {
		t.Run(fmt.Sprint(hours), func(t *testing.T) {
			h, runner := bookRefreshRunner(t, hours)
			for _, id := range []string{recoveryShipLapras, recoveryShipCharizard, recoveryShipBlastoise} {
				changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, id))
				want := hours == 6 && id == recoveryShipBlastoise
				if err != nil || changed != want {
					t.Fatalf("%dh refresh admission for %s: changed=%v want=%v reason=%q err=%v", hours, id, changed, want, reason, err)
				}
			}
			if got := scoutAcceptanceMissionCount(t, h, runner); got != int(hours-5) {
				t.Fatalf("%dh refresh pass created %d missions", hours, got)
			}
			if hours == 5 {
				return
			}
			mission, data := h.mission(recoveryShipBlastoise)
			if mission.Phase != "sourcing_fund" || data.Relocation == nil || !data.Relocation.Scout || !isBookRefresh(data.Plan) ||
				data.Relocation.ExpenseCapPence < data.Relocation.ModeledExpensePence {
				t.Fatalf("six-hour pass did not persist the bounded refresh: mission=%+v data=%+v", mission, data)
			}
			if err := runner.relocationSpendingReadyAtPrice(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), &data, 0, 0); err != nil {
				t.Fatalf("committed six-hour refresh incorrectly required twelve hours or a hint: %v", err)
			}
			for _, id := range []string{recoveryShipCharizard, recoveryShipLapras} {
				if changed, _, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, id)); err != nil || changed {
					t.Fatalf("a second fleet refresh was admitted in flight: changed=%v err=%v", changed, err)
				}
			}
		})
	}
}

func TestBookRefreshRequiresTwoMissionFreeStationaryShips(t *testing.T) {
	for _, scenario := range []string{"one stationary", "other ships have missions", "two stationary"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := bookRefreshRunner(t, 6)
			switch scenario {
			case "one stationary":
				for i := range runner.stateCache.Ships {
					if runner.stateCache.Ships[i].Ship.ID != recoveryShipBlastoise {
						runner.stateCache.Ships[i].Ship.Status = "sailing"
					}
				}
			case "other ships have missions":
				for _, id := range []string{recoveryShipLapras, recoveryShipCharizard} {
					h.putMission(id, "refresh-unavailable-"+id, "maintenance", missionData{
						Plan: Plan{Kind: "maintenance", ShipID: id, Origin: "hamburg", Destination: "hamburg"}, CostsKnown: true,
					})
				}
			case "two stationary":
				runner.stateCache.Ships = runner.stateCache.Ships[1:]
			}
			changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil || changed != (scenario == "two stationary") {
				t.Fatalf("%s refresh admission: changed=%v reason=%q err=%v", scenario, changed, reason, err)
			}
		})
	}
}

func TestBookRefreshNeverPreemptsAnAcceptedHintedScout(t *testing.T) {
	for _, designatedHours := range []int64{6, 12} {
		t.Run(fmt.Sprint(designatedHours), func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
			observed := idle[recoveryShipBlastoise]
			observed.EarnedGameHours = designatedHours
			idle[recoveryShipBlastoise] = observed
			runner.snapshot["_ship_idle"] = encode(idle)
			changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil {
				t.Fatal(err)
			}
			if designatedHours == 6 {
				if changed || !strings.Contains(reason, "designated scout") {
					t.Fatalf("six-hour refresh preempted another ship's accepted hinted scout: changed=%v reason=%q", changed, reason)
				}
				return
			}
			if !changed {
				t.Fatalf("accepted hinted scout was not admitted: %s", reason)
			}
			_, data := h.mission(recoveryShipBlastoise)
			if isBookRefresh(data.Plan) || len(runner.snapshot["_book_refresh"]) != 0 {
				t.Fatal("a hinted scout consumed the book refresh cooldown")
			}
		})
	}
}

func TestBookRefreshCooldownPersistsAndEndsAtTwentyFourGameDays(t *testing.T) {
	h, runner := bookRefreshRunner(t, 6)
	_, first := scoutAcceptanceAdmit(t, h, runner)
	if !isBookRefresh(first.Plan) {
		t.Fatalf("fixture admitted a hinted scout instead of refresh: %+v", first.Plan)
	}
	saved, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := decode[bookRefreshState](saved, "_book_refresh")
	if last.StartedAt.IsZero() || last.StartedGameHour != runner.stateCache.Clock.Calendar.TotalGameHours {
		t.Fatalf("refresh admission did not persist its game-hour cooldown: %+v", last)
	}
	// Clear the durable mission as if the attempt ended. The retained snapshot,
	// not an in-memory mission or target cooldown, must bound the next refresh.
	if err := h.db.DeleteMission(h.ctx, h.account.ID, recoveryShipBlastoise); err != nil {
		t.Fatal(err)
	}
	runner.snapshot = saved
	for _, elapsed := range []int64{24*24 - 1, 24 * 24} {
		runner.stateCache.Clock.Calendar.TotalGameHours = last.StartedGameHour + elapsed
		idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
		for id, observed := range idle {
			observed.LastValidatedGameHour = runner.stateCache.Clock.Calendar.TotalGameHours
			idle[id] = observed
		}
		runner.snapshot["_ship_idle"] = encode(idle)
		changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
		if err != nil || changed != (elapsed == 24*24) {
			t.Fatalf("refresh at +%dgh: changed=%v reason=%q err=%v", elapsed, changed, reason, err)
		}
		if !changed && !strings.Contains(reason, "24 game days") {
			t.Fatalf("refresh cooldown lost its game-time explanation: %q", reason)
		}
	}
}

func TestBookRefreshSendsBallastAndCountsAsUnconvertedScout(t *testing.T) {
	h, runner := bookRefreshRunner(t, 6)
	_, data := scoutAcceptanceAdmit(t, h, runner)
	target, lane := data.Plan.Destination, data.ProvenLane
	scoutAcceptanceServeDeparture(t, h, runner)
	for range 10 {
		mission, current := h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &current); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	if h.api.mutationCount("/v1/fleet/depart") != 1 {
		t.Fatalf("no-hint refresh failed to send exactly one ballast departure: holds=%+v", runner.holding)
	}
	for i := range runner.stateCache.Ships {
		if runner.stateCache.Ships[i].Ship.ID == recoveryShipBlastoise {
			ship := &runner.stateCache.Ships[i]
			ship.Ship.CurrentPortID, ship.Ship.Status, ship.Voyage = target, "anchored_roadstead", nil
			ship.Stores.ProvisionsTons -= 0.1
		}
	}
	runner.stateCache.Clock.Calendar.TotalGameHours++
	for range 2 {
		mission, current := h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &current); err != nil {
			t.Fatal(err)
		}
	}
	rows := destinationSettlements(t, h, lane)
	if len(rows) != 1 || rows[0].ProceedsPence != 0 || rows[0].QuantityTons != 0 {
		t.Fatalf("refresh did not settle as a zero-income ballast scout: %+v", rows)
	}
	history := probeHistoryFixture()
	history.settlement(rows[0], nil)
	if history.ScoutFailures != 1 {
		t.Fatalf("refresh escaped the fleet nonconversion bound: %+v", history)
	}
	history.departure("trader", "hamburg", rows[0].CompletedAt.Add(time.Second), true, shipIdleState{})
	if history.ScoutFailures != 1 {
		t.Fatal("a laden departure elsewhere released the refresh failure")
	}
	history.departure("trader", target, rows[0].CompletedAt.Add(2*time.Second), true, shipIdleState{})
	if history.ScoutFailures != 0 {
		t.Fatal("a laden departure from the refreshed port did not release the fleet bound")
	}
}

func TestBookRefreshWaitsForAcceptedHintAtAnotherFleetPort(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
	for id, observed := range idle {
		observed.EarnedGameHours = 6
		idle[id] = observed
	}
	other := harnessShip(t, runner, recoveryShipBlastoise)
	other.Ship.ID = "44444444-4444-4444-8444-000000000001"
	other.Ship.CurrentPortID = "amsterdam"
	runner.stateCache.Ships = append(runner.stateCache.Ships, other)
	idle[other.Ship.ID] = shipIdleState{
		PortID: "amsterdam", EarnedGameHours: 12, Validated: true,
		LastValidatedGameHour: runner.stateCache.Clock.Calendar.TotalGameHours, LastClockUTC: runner.stateCache.Clock.RealUTC,
	}
	runner.snapshot["_ship_idle"] = encode(idle)
	book := runner.stateCache.Markets["hamburg"]
	book.PortID = "amsterdam"
	runner.stateCache.Markets["amsterdam"] = book
	runner.meta["books/amsterdam"] = runner.meta["books/hamburg"]
	for _, pair := range [][2]string{{"amsterdam", "flushing"}, {"flushing", "amsterdam"}} {
		key := routeKey(pair[0], pair[1], "hoy")
		runner.stateCache.Routes[key] = sourcingRoute(pair[0], pair[1], 30)
		runner.meta["routes/"+key] = runner.meta["routes/"+routeKey("hamburg", "flushing", "hoy")]
	}
	input, err := runner.sourcingInput(h.ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if got := scoutAcceptancePlan(t, scoutCandidates(input, runner.scoutBlockedPorts(input, nil)), "flushing"); !got.Accepted {
		t.Fatalf("fixture has no accepted hinted scout at the other port: %+v", got)
	}
	changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil || changed || !strings.Contains(reason, "hinted scout on another ship takes priority") {
		t.Fatalf("book refresh preempted an accepted hinted fleet scout: changed=%v reason=%q err=%v", changed, reason, err)
	}
}

func TestBookRefreshDesignationSkipsInadmissibleAndNonidleShips(t *testing.T) {
	for _, blocker := range []string{"convoy", "sealed freight", "not idle", "no observed idle", "regressed idle clock"} {
		t.Run(blocker, func(t *testing.T) {
			h, runner := bookRefreshRunner(t, 6)
			switch blocker {
			case "convoy":
				for i := range runner.stateCache.Ships {
					if runner.stateCache.Ships[i].Ship.ID == recoveryShipBlastoise {
						runner.stateCache.Ships[i].Ship.ConvoyID = "unavailable-convoy"
					}
				}
			case "sealed freight":
				runner.stateCache.Contracts = []Contract{{ID: "unavailable-freight", ContractType: "freight", Status: "active",
					CarrierCompanyID: recoveryCompanyID, CarrierShipID: recoveryShipBlastoise}}
			case "not idle", "no observed idle", "regressed idle clock":
				idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
				observed := idle[recoveryShipBlastoise]
				switch blocker {
				case "not idle":
					observed.EarnedGameHours = sourcingIdleGameHours - 1
				case "no observed idle":
					observed.EarnedGameHours = 0
				case "regressed idle clock":
					observed.ClockRegressed = true
				}
				idle[recoveryShipBlastoise] = observed
				runner.snapshot["_ship_idle"] = encode(idle)
			}
			for _, id := range []string{recoveryShipLapras, recoveryShipCharizard} {
				changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, id))
				if err != nil || changed != (id == recoveryShipCharizard) {
					t.Fatalf("%s: refresh for %s changed=%v reason=%q err=%v", blocker, id, changed, reason, err)
				}
			}
			_, data := h.mission(recoveryShipCharizard)
			if !isBookRefresh(data.Plan) || scoutAcceptanceMissionCount(t, h, runner) != 1 {
				t.Fatalf("%s: admissible replacement did not exclusively own the refresh: %+v", blocker, data)
			}
			h.missingMission(recoveryShipBlastoise)
		})
	}
}

func TestBookRefreshWithoutEligibleShipDoesNotDesignateOne(t *testing.T) {
	for _, blocker := range []string{"convoy", "not idle"} {
		t.Run(blocker, func(t *testing.T) {
			h, runner := bookRefreshRunner(t, sourcingIdleGameHours-1)
			if blocker == "convoy" {
				for i := range runner.stateCache.Ships {
					runner.stateCache.Ships[i].Ship.ConvoyID = "unavailable-convoy"
				}
			}
			for _, ship := range runner.stateCache.Ships {
				in, err := runner.sourcingInput(h.ctx, ship)
				if err != nil {
					t.Fatal(err)
				}
				plans, reason, err := runner.scoutingPlans(h.ctx, in, nil)
				if err != nil || strings.Contains(reason, "designated") {
					t.Fatalf("%s: an ineligible ship blocked %s by designation: plans=%+v reason=%q err=%v", blocker, ship.Ship.ID, plans, reason, err)
				}
				for _, plan := range plans {
					if plan.Accepted {
						t.Fatalf("%s: no eligible ship may start a refresh: %+v", blocker, plan)
					}
				}
			}
			if scoutAcceptanceMissionCount(t, h, runner) != 0 {
				t.Fatal("an ineligible designation created a mission")
			}
		})
	}
}

func TestBookRefreshRollingUnknownCostsAndRefusedSpendPreserveCooldownAndFailures(t *testing.T) {
	for _, refusal := range []string{"none", "new unknown cost", "reduced cap", "reduced cap without prior"} {
		t.Run(refusal, func(t *testing.T) {
			h, runner := bookRefreshRunner(t, 6)
			now := time.Now().UTC()
			window := time.Duration(sourcingBudgetGameDays*24*int64(time.Hour)) / time.Duration(runner.stateCache.Clock.Calendar.TimeScale)
			idleFallbackSettlement(t, h, store.Settlement{ID: "refresh-aged-unknown", Kind: "relocation", CostsKnown: false,
				CompletedAt: now.Add(-window - time.Second)})
			lifetime, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
			if err != nil || !lifetime.UnknownProbeCosts {
				t.Fatalf("fixture has no lifetime unknown-cost probe: %+v %v", lifetime, err)
			}
			runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
			runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
			if runner.sourcingHistory.UnknownCost {
				t.Fatal("old unknown probe entered the rolling admission flag")
			}
			previous := bookRefreshState{StartedAt: now.Add(-48 * time.Hour),
				StartedGameHour: runner.stateCache.Clock.Calendar.TotalGameHours - bookRefreshCooldownGameHours}
			if refusal == "reduced cap without prior" {
				previous = bookRefreshState{}
			} else {
				runner.snapshot["_book_refresh"] = encode(previous)
			}
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			if !isBookRefresh(data.Plan) {
				t.Fatal("fixture did not admit a book refresh")
			}
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			if err := runner.relocationSpendingReadyAtPrice(h.ctx, ship, &data, 0, 0); err != nil {
				t.Fatalf("aged unknown probe vetoed an admitted refresh: %v", err)
			}
			if refusal == "none" {
				return
			}
			if refusal == "new unknown cost" {
				idleFallbackSettlement(t, h, store.Settlement{ID: "refresh-current-unknown", Kind: "relocation", CostsKnown: false})
			} else {
				data.Relocation.ExpenseCapPence = 1
			}
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				mission, current, err := runner.loadMission(h.ctx, ship.Ship.ID)
				if err != nil {
					t.Fatal(err)
				}
				if mission == nil {
					break
				}
				if _, err := runner.driveMission(h.ctx, ship, mission, &current); err != nil {
					t.Fatal(err)
				}
			}
			h.missingMission(ship.Ship.ID)
			saved, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			last := decode[bookRefreshState](saved, "_book_refresh")
			if last.StartedGameHour != previous.StartedGameHour || !last.StartedAt.Equal(previous.StartedAt) {
				t.Fatalf("refused spend burned the prior refresh cooldown: got %+v want %+v", last, previous)
			}
			if previous.StartedAt.IsZero() && len(saved["_book_refresh"]) != 0 {
				t.Fatal("refused first refresh left a durable cooldown reservation")
			}
			outcomes, err := runner.readProbeOutcomes(h.ctx, runner.planningInput(ship))
			if err != nil || outcomes.ScoutFailures != 0 {
				t.Fatalf("refused refresh counted as an unconverted scout: %+v %v", outcomes, err)
			}
			if h.api.mutationCount("/v1/fleet/depart")+h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatal("refused refresh sent a game mutation")
			}
		})
	}
}
