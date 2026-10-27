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

func TestScoutHistoryZeroAllowanceAndCurrentOriginDoNotLoadScoutLanes(t *testing.T) {
	for _, zeroAllowance := range []bool{false, true} {
		t.Run(map[bool]string{false: "origin excluded", true: "zero allowance"}[zeroAllowance], func(t *testing.T) {
			h := newRecoveryHarness(t)
			if zeroAllowance {
				idleFallbackExhaustSeed(t, h)
			}
			in := scoutAcceptanceInput(t)
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
			if zeroAllowance {
				if sourcingAllowance(in) != 0 || len(in.SourcingProbeLanes) != 0 || len(in.SourcingProbeCompletions) != 0 {
					t.Fatalf("zero allowance still loaded scout history: %+v", in.SourcingProbeCompletions)
				}
			} else if in.SourcingProbeCompletions[scoutLaneKey("amsterdam", "hamburg", "copper_sheathing")].IsZero() {
				t.Fatal("positive allowance omitted the eligible scout lane")
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
	runner := &Runner{stateCache: accountState{Ships: []ShipState{self}}}
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
	if scoutExposure(input, data.Plan).Accepted {
		t.Fatal("fixture did not age out admission-only provisions gossip")
	}
	costed := costCommittedRelocation(input, &data)
	if !costed.Accepted {
		t.Fatalf("committed costing reapplied provisions admission: %+v", costed)
	}
	if err := runner.scoutTargetReady(h.ctx, input, data.Plan.Destination); err != nil {
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
	if sourcingAllowance(input) >= exposure {
		t.Fatal("fixture did not lower the runner's current risk capital below committed exposure")
	}
	if err := runner.scoutTargetReady(h.ctx, input, data.Plan.Destination); err != nil {
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
		found = found || strings.Contains(hold.Reason, "cancelled before further spending: ") && strings.Contains(hold.Reason, "current idle exploration risk capital")
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
