package engine

// Scouting uses these production signatures:
//   func (*Runner) shipIdleFor(PlanningInput, int64) bool
//   func scoutCandidates(PlanningInput, map[string]string) []Plan
//   func scoutExposure(PlanningInput, Plan) Plan
//   func scoutLaneKey(target, origin, commodity string) string
//   relocationIntent.Scout bool `json:"scout,omitempty"`
// Existing Runner admission, durable action, settlement and probe-history seams
// remain authoritative; these tests add no implementation or executable gossip.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Prices and quantities are the recorded Hamburg evidence, not measured remote
// depth. Equal routes/dues deliberately isolate the ranking arithmetic.
func scoutAcceptanceInput(t *testing.T) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	in.CompanyID = recoveryCompanyID
	in.Ship.Ship.ID = recoveryShipBlastoise
	in.Ship.Ship.CurrentPortID = "hamburg"
	in.Ship.Ship.Status = "anchored_roadstead"
	in.Ship.EffectiveHoldCapacityTons = 30
	in.Contracts = nil
	in.Catalog.Ports = []Port{{ID: "hamburg", BankingRegion: "north", SeaZoneID: "english_channel", Phase: 1}}
	in.Catalog.Commodities = []Commodity{
		{ID: "copper_sheathing"},
		{ID: "salted_meat"},
		{ID: "ironwork_tools"},
		{ID: "provisions"},
	}
	in.Markets = map[string]Market{"hamburg": sourcingFixture[Market](t, `{
		"port_id":"hamburg","visibility":"quay","customs_bps":253,"books":[
		{"commodity_id":"copper_sheathing","depth":{"asks":[{"order_id":"hh-copper","price_pence":61264,"remaining_quantity":58}],"bids":[]}},
		{"commodity_id":"salted_meat","depth":{"asks":[{"order_id":"hh-meat","price_pence":27815,"remaining_quantity":65}],"bids":[]}},
		{"commodity_id":"ironwork_tools","depth":{"asks":[{"order_id":"hh-tools","price_pence":48537,"remaining_quantity":37}],"bids":[]}},
		{"commodity_id":"provisions","depth":{"asks":[{"order_id":"hh-stores","price_pence":25743,"remaining_quantity":32}],"bids":[]}}]}`)}
	in.Ports = map[string]PortDetail{"hamburg": {ID: "hamburg", EffectiveBerths: 10, BerthDuesPerDayPence: 100}}
	in.Routes = make(map[string]Route)
	in.Metadata = make(map[string]DatasetMeta)
	in.Balances = map[string]int64{"north": 7_512_076}
	in.ReserveByRegion = map[string]int64{"north": 100_000}
	in.SourcingProbeHistory = probeHistoryFixture()
	in.SourcingProbeCompletions = make(map[string]time.Time)
	in.SourcingIdleEligible = true
	for _, key := range []string{"ships", "catalog", "clock", "zones", "contracts", "vaults", "rates", "books/hamburg", "ports/hamburg"} {
		in.Metadata[key] = sourcingFreshMeta(in.Now)
	}
	for _, hint := range []struct {
		port, commodity string
		bid             int64
		age             time.Duration
	}{
		{"amsterdam", "copper_sheathing", 65_865, 80800 * time.Millisecond},
		{"flushing", "salted_meat", 31_619, 130 * time.Second},
		{"portsmouth", "ironwork_tools", 52_175, 60 * time.Second},
		{"calais", "provisions", 17_732, 80 * time.Second},
	} {
		port := Port{ID: hint.port, BankingRegion: "north", SeaZoneID: "english_channel", Phase: 1, Imports: []string{hint.commodity}}
		if hint.port != "calais" {
			port.Produces = []string{"provisions"}
		}
		in.Catalog.Ports = append(in.Catalog.Ports, port)
		in.Ports[hint.port] = PortDetail{ID: hint.port, EffectiveBerths: 10, BerthDuesPerDayPence: 100}
		in.Metadata["ports/"+hint.port] = sourcingFreshMeta(in.Now)
		for _, pair := range [][2]string{{"hamburg", hint.port}, {hint.port, "hamburg"}} {
			key := routeKey(pair[0], pair[1], "hoy")
			in.Routes[key] = sourcingRoute(pair[0], pair[1], 30)
			in.Metadata["routes/"+key] = sourcingFreshMeta(in.Now)
		}
		// One real minute is one game hour at scale 60.
		at := in.Now.Add(-hint.age * 60)
		bid := hint.bid
		home := in.Ports["hamburg"]
		home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, Gossip{
			SubjectPortID: hint.port, SnapshotAt: at,
			Commodities: []GossipCommodity{{CommodityID: hint.commodity, SnapshotAt: at, BestBidPence: &bid}},
		})
		in.Ports["hamburg"] = home
	}
	return in
}

func scoutAcceptancePlan(t *testing.T, plans []Plan, port string) Plan {
	t.Helper()
	for _, plan := range plans {
		if plan.Destination == port {
			return plan
		}
	}
	t.Fatalf("missing scout candidate %s: %+v", port, plans)
	return Plan{}
}

func TestScoutIdleRequiresTwelveCoherentHoursWithoutChangingSixHourSourcing(t *testing.T) {
	for _, hours := range []int64{11, 12} {
		t.Run(fmt.Sprint(hours), func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			runner := &Runner{snapshot: make(map[string]json.RawMessage)}
			observed := shipIdleState{PortID: "hamburg", EarnedGameHours: hours, Validated: true,
				LastValidatedGameHour: in.Clock.Calendar.TotalGameHours, LastClockUTC: in.Clock.RealUTC}
			runner.snapshot["_ship_idle"] = encode(map[string]shipIdleState{in.Ship.Ship.ID: observed})
			if runner.shipIdleFor(in, 12) != (hours >= 12) || !runner.shipIdleEligible(in) {
				t.Fatalf("%dh baseline must preserve six-hour sourcing and independently gate twelve-hour scouting", hours)
			}
		})
	}
	for _, invalid := range []string{"port mismatch", "regressed clock", "hour mismatch", "UTC mismatch", "unvalidated", "cargo"} {
		t.Run(invalid, func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			observed := shipIdleState{PortID: "hamburg", EarnedGameHours: 12, Validated: true,
				LastValidatedGameHour: in.Clock.Calendar.TotalGameHours, LastClockUTC: in.Clock.RealUTC}
			switch invalid {
			case "port mismatch":
				observed.PortID = "amsterdam"
			case "regressed clock":
				observed.ClockRegressed = true
			case "hour mismatch":
				observed.LastValidatedGameHour--
			case "UTC mismatch":
				observed.LastClockUTC = observed.LastClockUTC.Add(-time.Second)
			case "unvalidated":
				observed.Validated = false
			case "cargo":
				in.Ship.CargoUsedTons = 1
				in.Ship.CargoManifest = map[string]int64{"copper_sheathing": 1}
			}
			runner := &Runner{snapshot: map[string]json.RawMessage{"_ship_idle": encode(map[string]shipIdleState{in.Ship.Ship.ID: observed})}}
			if runner.shipIdleFor(in, 12) || runner.shipIdleEligible(in) {
				t.Fatal("incoherent or laden ship earned scouting or sourcing eligibility")
			}
		})
	}
}

func TestScoutHamburgRanksHintsWithoutInventingCargoOrRevenue(t *testing.T) {
	in := scoutAcceptanceInput(t)
	plans := scoutCandidates(in, nil)
	accepted := acceptedRelocations(plans)
	if len(accepted) != 3 {
		t.Fatalf("want three positive-spread scouts: %+v", plans)
	}
	for i, port := range []string{"flushing", "amsterdam", "portsmouth"} {
		plan := accepted[i]
		if plan.Destination != port || plan.Kind != "relocation" || plan.Quantity != 0 || plan.ContractID != "" || plan.Economics.RevenuePence != 0 {
			t.Errorf("rank %d must be a zero-cargo, zero-revenue %s scout: %+v", i, port, plan)
		}
		if !strings.Contains(plan.Reason, "game hours old") || !strings.Contains(plan.Reason, "not executable depth") {
			t.Errorf("scout presented stale hints as executable observations: %q", plan.Reason)
		}
	}
	calais := scoutAcceptancePlan(t, plans, "calais")
	if calais.Accepted || calais.Reason != "no age-labelled gossip hint shows a positive post-customs spread" {
		t.Fatalf("negative provisions spread was not rejected honestly: %+v", calais)
	}
	flushing := in.Ports["flushing"]
	flushing.BerthDuesPerDayPence += 30_000
	in.Ports["flushing"] = flushing
	repriced := scoutCandidates(in, nil)
	before := scoutAcceptancePlan(t, plans, "flushing")
	after := scoutAcceptancePlan(t, repriced, "flushing")
	if -after.Economics.ExpectedNetPence+before.Economics.ExpectedNetPence < 1479 {
		t.Fatal("fixture did not raise flushing exposure by at least 1,479p")
	}
	if repriced[0].Destination != "amsterdam" || !repriced[0].Accepted {
		t.Fatalf("ranking ignored upside minus exposure: %+v", repriced)
	}
}

// Pin the exact upside through its strict acceptance boundary, rather than
// requiring a new production Plan field or parsing implementation-specific text.
func TestScoutHamburgExactHintUpsidesBoundExposure(t *testing.T) {
	for _, tc := range []struct {
		port   string
		upside int64
	}{{"amsterdam", 91_530}, {"flushing", 93_008}, {"portsmouth", 72_300}} {
		t.Run(tc.port, func(t *testing.T) {
			for _, gap := range []int64{1, 0} {
				in := scoutAcceptanceInput(t)
				in.Balances["north"] = 100_000_000
				plan := scoutAcceptancePlan(t, scoutCandidates(in, nil), tc.port)
				if !plan.Accepted {
					t.Fatalf("baseline scout rejected: %+v", plan)
				}
				wantExposure := tc.upside - gap
				origin := in.Ports["hamburg"]
				// No BerthedAt: originDays is exactly 0.25; four pence of
				// daily dues therefore adds exactly one penny of exposure.
				origin.BerthDuesPerDayPence += 4 * (wantExposure + plan.Economics.ExpectedNetPence)
				in.Ports["hamburg"] = origin
				costed := scoutExposure(in, plan)
				if !costed.Accepted || -costed.Economics.ExpectedNetPence != wantExposure {
					t.Fatalf("exposure boundary fixture: want %dp, got %+v", wantExposure, costed)
				}
				got := scoutAcceptancePlan(t, scoutCandidates(in, nil), tc.port)
				if got.Accepted != (gap == 1) {
					t.Fatalf("hint upside must be exactly %dp and strictly exceed exposure %dp: %+v", tc.upside, wantExposure, got)
				}
			}
		})
	}
}

func TestScoutHintAgeUsesNewestReportAndCommodityTimestamps(t *testing.T) {
	for _, tc := range []struct {
		name                string
		reportAge, priceAge time.Duration
		want                bool
	}{
		{"exact 168 hours", 168 * time.Minute, 168 * time.Minute, true},
		{"old report", 168*time.Minute + 6*time.Second, 168 * time.Minute, false},
		{"old commodity", 168 * time.Minute, 168*time.Minute + 6*time.Second, false},
		{"future report", -time.Second, time.Minute, false},
		{"future commodity", time.Minute, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			home := in.Ports["hamburg"]
			home.CoffeeHouseGossip[0].SnapshotAt = in.Now.Add(-tc.reportAge)
			home.CoffeeHouseGossip[0].Commodities[0].SnapshotAt = in.Now.Add(-tc.priceAge)
			in.Ports["hamburg"] = home
			got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam")
			if got.Accepted != tc.want {
				t.Fatalf("hint age gate accepted=%v, want %v: %+v", got.Accepted, tc.want, got)
			}
		})
	}
	in := scoutAcceptanceInput(t)
	home := in.Ports["hamburg"]
	newer := home.CoffeeHouseGossip[0]
	newer.Commodities = append([]GossipCommodity(nil), newer.Commodities...)
	newer.SnapshotAt = in.Now.Add(-time.Minute)
	newer.Commodities[0].SnapshotAt = newer.SnapshotAt
	lowBid := int64(1)
	newer.Commodities[0].BestBidPence = &lowBid
	home.CoffeeHouseGossip = append(home.CoffeeHouseGossip, newer)
	in.Ports["hamburg"] = home
	if got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam"); got.Accepted {
		t.Fatalf("older high bid overrode newer adverse information: %+v", got)
	}
}

func TestScoutPlannerSafetyBudgetStreakAndCooldownGates(t *testing.T) {
	for _, gate := range []string{"blocked", "outward hull", "return hull", "stores", "berths", "hazards", "marque", "funding", "budget", "streak", "cooldown early", "cooldown exact", "phase locked", "provisions supply"} {
		t.Run(gate, func(t *testing.T) {
			in := scoutAcceptanceInput(t)
			base := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam")
			blocked := make(map[string]string)
			wantReason := ""
			want := false
			switch gate {
			case "blocked":
				wantReason = "our ship Bulbasaur is at amsterdam"
				blocked["amsterdam"] = wantReason
			case "outward hull", "return hull":
				from, to := "hamburg", "amsterdam"
				if gate == "return hull" {
					from, to = to, from
				}
				key := routeKey(from, to, "hoy")
				route := in.Routes[key]
				route.Legs = append([]RouteLeg(nil), route.Legs...)
				route.Legs[0].EdgeClass = "oceanic"
				in.Routes[key] = route
				hull, _ := findHull(in.Catalog, "hoy")
				wantReason = ValidateRoute(route, hull, in.Catalog.SeaZones).Error()
			case "stores":
				in.Ship.Stores.ProvisionsTons = 0
				route := in.Routes[routeKey("hamburg", "amsterdam", "hoy")]
				route.SafeProvisionsTons = 31
				in.Routes[routeKey("hamburg", "amsterdam", "hoy")] = route
				wantReason = "both-way safe sourcing stores do not fit the empty hold"
			case "berths":
				detail := in.Ports["amsterdam"]
				detail.EffectiveBerths = 0
				in.Ports["amsterdam"] = detail
			case "hazards":
				key := routeKey("hamburg", "amsterdam", "hoy")
				route := in.Routes[key]
				route.Legs = []RouteLeg{{FromNode: "hamburg", ToNode: "english_channel", EdgeClass: "coastal"}, {FromNode: "english_channel", ToNode: "amsterdam", EdgeClass: "coastal"}}
				in.Routes[key] = route
				in.Zones = nil
			case "marque":
				in.MarqueZones = map[string]bool{"english_channel": true}
			case "funding":
				in.Balances["north"] = 0
			case "budget":
				cap := -base.Economics.ExpectedNetPence - 1
				in.SourcingIdleEligible = false
				in.SourcingAllowancePence = &cap
				wantReason = sourcingBudgetReason(in, cap+1, cap)
			case "streak":
				in.SourcingProbeHistory.Ships[in.Ship.Ship.ID] = probeShipHistory{Failures: 2}
				wantReason = failedProbeStreakReason
			case "cooldown early", "cooldown exact":
				age := 24 * time.Minute
				if gate == "cooldown early" {
					age -= time.Second
				} else {
					want = true
				}
				in.SourcingProbeCompletions[scoutLaneKey("amsterdam", "hamburg", "copper_sheathing")] = in.Now.Add(-age)
			case "phase locked":
				in.Catalog.Ports[1].Phase = in.Clock.UnlockedPhase + 1
			case "provisions supply":
				in.Catalog.Ports[1].Produces = nil
				in.Catalog.Ports[1].Manufactures = nil
			}
			plans := scoutCandidates(in, blocked)
			if gate == "phase locked" {
				for _, plan := range plans {
					if plan.Destination == "amsterdam" && plan.Accepted {
						t.Fatalf("phase-locked scout accepted: %+v", plan)
					}
				}
				return
			}
			got := scoutAcceptancePlan(t, plans, "amsterdam")
			if got.Accepted != want || wantReason != "" && got.Reason != wantReason {
				t.Fatalf("%s: accepted=%v reason=%q, want accepted=%v reason=%q", gate, got.Accepted, got.Reason, want, wantReason)
			}
			if gate == "provisions supply" && !strings.Contains(got.Reason, "provisions") {
				t.Fatalf("target without provisions supply evidence lacks its rejection reason: %+v", got)
			}
		})
	}
	if got := scoutLaneKey("amsterdam", "hamburg", "copper_sheathing"); got != "amsterdam|hamburg|copper_sheathing|scout" {
		t.Fatalf("scout completion lane is not stable across missions: %q", got)
	}
}

// Keep real private observations from refresh, then project the recorded public
// Hamburg fixture like sourcingReviewInject. No live game or production DB is used.
func scoutAcceptanceRunner(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	runner := refreshedRunner(t, h)
	in := scoutAcceptanceInput(t)
	now := time.Now().UTC()
	delta := now.Sub(in.Now)
	in.Now, in.Clock.RealUTC = now, now
	in.Clock.Calendar.TotalGameHours = 1000
	for id, detail := range in.Ports {
		for i := range detail.CoffeeHouseGossip {
			gossip := &detail.CoffeeHouseGossip[i]
			gossip.SnapshotAt = gossip.SnapshotAt.Add(delta)
			for j := range gossip.Commodities {
				gossip.Commodities[j].SnapshotAt = gossip.Commodities[j].SnapshotAt.Add(delta)
			}
		}
		in.Ports[id] = detail
	}
	for key := range in.Metadata {
		runner.meta[key] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Complete: true, Generation: runner.generation}
	}
	runner.stateCache.Catalog, runner.stateCache.Clock = in.Catalog, in.Clock
	runner.stateCache.Ports, runner.stateCache.Markets, runner.stateCache.Routes = in.Ports, in.Markets, in.Routes
	runner.stateCache.Contracts, runner.stateCache.Loans, runner.stateCache.InsurancePolicies = nil, nil, nil
	runner.stateCache.HomePortID, runner.stateCache.Factors = "hamburg", nil
	runner.stateCache.Vaults = []Vault{{AccountID: "scout-vault", AccountType: "vault", BankingRegion: "north"}}
	idle := make(map[string]shipIdleState)
	runner.stateCache.Ships = nil
	for _, spec := range []struct {
		id, name    string
		hold, hours int64
	}{
		{recoveryShipLapras, "Bulbasaur", 35, 140},
		{recoveryShipCharizard, "Ivysaur", 30, 223},
		{recoveryShipBlastoise, "Lapras", 30, 253},
	} {
		ship := in.Ship
		ship.Ship.ID, ship.Ship.Name = spec.id, spec.name
		ship.EffectiveHoldCapacityTons = spec.hold
		runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
		idle[spec.id] = shipIdleState{PortID: "hamburg", EarnedGameHours: spec.hours, Validated: true, LastValidatedGameHour: 1000, LastClockUTC: now}
	}
	runner.snapshot["_ship_idle"] = encode(idle)
	runner.snapshot["_book_observed"] = encode(map[string]time.Time{"hamburg": now})
	runner.snapshot["zones"], runner.snapshot["clock"] = encode(in.Zones), encode(in.Clock)
	runner.snapshot["books"], runner.snapshot["ports"], runner.snapshot["routes"] = encode(in.Markets), encode(in.Ports), encode(in.Routes)
	runner.snapshot["orders"], runner.snapshot["ledger"], runner.snapshot["warehouses"] = encode([]any{}), encode([]any{}), encode([]any{})
	runner.snapshot["insurance"], runner.snapshot["loans"] = encode([]any{}), encode([]any{})
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	runner.stateCache.Vaults[0].BalancePence = reserves["north"] + 7_412_076
	runner.refreshSourcingHistory(h.ctx, runner.planningInput(ShipState{}))
	runner.refreshSourcingBudget(h.ctx, runner.planningInput(ShipState{}))
	return h, runner
}

func scoutAcceptanceMissionCount(t *testing.T, h *recoveryHarness, runner *Runner) int {
	t.Helper()
	count := 0
	for _, ship := range runner.stateCache.Ships {
		mission, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mission != nil {
			count++
		}
	}
	return count
}

func TestScoutAdmissionCreatesOneMissionOnSmallestHoldThenLowestID(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	core, logs := observer.New(zap.DebugLevel)
	runner.log = zap.New(core)
	// The larger and higher-ID ships are encountered first in the replay.
	for _, id := range []string{recoveryShipLapras, recoveryShipCharizard} {
		ship := harnessShip(t, runner, id)
		changed, reason, err := runner.considerSourcing(h.ctx, ship)
		if err != nil || changed || !strings.Contains(reason, "designated scout") {
			t.Fatalf("non-designated ship stole scout admission: changed=%v reason=%q err=%v", changed, reason, err)
		}
	}
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("admit scout: changed=%v err=%v holds=%+v", changed, err, runner.holding)
	}
	if count := scoutAcceptanceMissionCount(t, h, runner); count != 1 {
		t.Fatalf("one pass created %d missions, want exactly one", count)
	}
	mission, data := h.mission(recoveryShipBlastoise)
	input, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil {
		t.Fatal(err)
	}
	source, _ := findPort(input.Catalog, data.Plan.Destination)
	input.ReserveByRegion = sourcingReserves(input, source)
	if mission.Phase != "sourcing_fund" || data.Relocation == nil || !data.Relocation.Scout || !data.Relocation.IdlePilot ||
		data.Relocation.TargetDestination != "hamburg" || data.Relocation.TargetCommodity != data.Plan.Commodity ||
		data.Relocation.ExpenseCapPence != 148_241 || data.Relocation.ExpenseCapPence != sourcingAllowance(input) ||
		data.Relocation.ModeledExpensePence != -data.Plan.Economics.ExpectedNetPence || data.Relocation.StoreBasis == nil ||
		data.ProvenLane != scoutLaneKey(data.Plan.Destination, "hamburg", data.Plan.Commodity) {
		t.Fatalf("admission did not persist the bounded scout intent: mission=%+v data=%+v", mission, data)
	}
	var wire map[string]any
	if err := json.Unmarshal(mission.Data, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["relocation"].(map[string]any)["scout"] != true {
		t.Fatal("scout marker missing from durable JSON")
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatal(err)
	}
	details, summaries := 0, 0
	for _, row := range rows.([]store.Decision) {
		if row.ShipID != recoveryShipBlastoise {
			continue
		}
		if row.Kind == "scouting" {
			summaries++
		} else if strings.HasPrefix(row.Kind, "scouting:") {
			details++
		}
	}
	if summaries != 1 || details != 3 {
		t.Fatalf("admission diagnostics must contain one scouting summary and the top three: summaries=%d details=%d", summaries, details)
	}
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	if count := scoutAcceptanceMissionCount(t, h, runner); count != 1 {
		t.Fatalf("second pass created %d missions, want exactly one", count)
	}
	h.missingMission(recoveryShipCharizard)
	h.missingMission(recoveryShipLapras)
	for _, entry := range logs.FilterLevelExact(zap.InfoLevel).All() {
		switch entry.Message {
		case "action committed", "action recovered", "mission settlement recorded", "fleet status", "ship arrived", "ship departed", "ship built", "work blocked", "work still blocked", "work unblocked", "dataset refresh recovered", "request journal persistence recovered", "event stream reconnected", "underwriting settled":
		default:
			t.Errorf("scout added a new INFO message: %q", entry.Message)
		}
	}
}

func TestScoutAdmissionElevenHoursObserveModeAndRejectedHeadline(t *testing.T) {
	for _, scenario := range []string{"eleven hours", "observe", "rejected"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			switch scenario {
			case "eleven hours":
				idle := decode[map[string]shipIdleState](runner.snapshot, "_ship_idle")
				for id, state := range idle {
					state.EarnedGameHours = 11
					idle[id] = state
				}
				runner.snapshot["_ship_idle"] = encode(idle)
			case "observe":
				runner.opts.Execute = false
			case "rejected":
				runner.opts.Execute = false
				observed := make(map[string]time.Time)
				for _, port := range runner.stateCache.Catalog.Ports {
					observed[port.ID] = runner.stateCache.Clock.RealUTC
				}
				runner.snapshot["_book_observed"] = encode(observed)
			}
			if _, err := runner.work(h.ctx); err != nil {
				t.Fatal(err)
			}
			if count := scoutAcceptanceMissionCount(t, h, runner); count != 0 {
				t.Fatalf("%s admitted %d missions", scenario, count)
			}
			if scenario == "eleven hours" {
				rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, row := range rows.([]store.Decision) {
					if row.Kind == "scouting" && row.Reason == "scouting waits for twelve coherent idle game hours at this port" {
						found = true
					}
				}
				if !found {
					t.Fatal("eleven-hour pass omitted the exact scouting threshold decision")
				}
				return
			}
			found := false
			for _, hold := range runner.holding {
				if hold.ShipID != recoveryShipBlastoise {
					continue
				}
				if scenario == "observe" {
					found = hold.Reason == "observe mode: funded scouting relocation is not executed"
				} else {
					found = strings.HasPrefix(hold.Reason, "no profitable executable voyage from hamburg") && strings.Contains(hold.Reason, "; sourcing: ") && strings.Contains(hold.Reason, "; scouting: ")
				}
			}
			if !found {
				t.Fatalf("%s lost its required hold: %+v", scenario, runner.holding)
			}
		})
	}
}

func TestScoutAdmissionExcludesOccupiedInboundCommittedAndRecentlyObservedPorts(t *testing.T) {
	for _, scenario := range []string{"stationary", "inbound last leg", "unsettled mission", "recent book", "six-hour book"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			home := runner.stateCache.Ports["hamburg"]
			home.CoffeeHouseGossip = home.CoffeeHouseGossip[:1]
			runner.stateCache.Ports["hamburg"] = home
			switch scenario {
			case "stationary":
				runner.stateCache.Ships[0].Ship.CurrentPortID = "amsterdam"
			case "inbound last leg":
				runner.stateCache.Ships[0].Ship.Status = "sailing"
				runner.stateCache.Ships[0].RouteLegs = []RouteLeg{{ToNode: "calais"}, {ToNode: "amsterdam"}}
			case "unsettled mission":
				h.putMission(recoveryShipLapras, "scout-port-reservation", "buy", missionData{Plan: Plan{Kind: "spot", Origin: "hamburg", Destination: "amsterdam", ShipID: recoveryShipLapras}, CostsKnown: true})
			case "recent book":
				runner.snapshot["_book_observed"] = encode(map[string]time.Time{"amsterdam": time.Now().UTC().Add(-5 * time.Minute)})
				// A newer hint isolates the recent-book exclusion from
				// supersession; higher dues keep ordinary one-ton probes out.
				hintAt := runner.stateCache.Clock.RealUTC.Add(-4 * time.Minute)
				home.CoffeeHouseGossip[0].SnapshotAt = hintAt
				home.CoffeeHouseGossip[0].Commodities[0].SnapshotAt = hintAt
				home.BerthDuesPerDayPence += 20_000
				runner.stateCache.Ports["hamburg"] = home
			case "six-hour book":
				runner.snapshot["_book_observed"] = encode(map[string]time.Time{"amsterdam": time.Now().UTC().Add(-6 * time.Minute)})
				// Keep this boundary case's hint newer than our own book.
				// Older hints superseded by that book are tested separately.
				hintAt := runner.stateCache.Clock.RealUTC.Add(-5 * time.Minute)
				home.CoffeeHouseGossip[0].SnapshotAt = hintAt
				home.CoffeeHouseGossip[0].Commodities[0].SnapshotAt = hintAt
				// The fresh one-ton gossip probe must stay uneconomic so
				// ordinary trading does not preempt this scout-only boundary.
				home.BerthDuesPerDayPence += 20_000
				runner.stateCache.Ports["hamburg"] = home
			}
			changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
			if err != nil {
				t.Fatal(err)
			}
			if changed != (scenario == "six-hour book") {
				t.Fatalf("%s: changed=%v reason=%q holds=%+v", scenario, changed, reason, runner.holding)
			}
			if scenario == "recent book" && !strings.Contains(reason, "a complete quay book was observed here less than six game hours ago") {
				t.Fatalf("recent-book case was rejected by the wrong gate: %q", reason)
			}
			if !changed {
				h.missingMission(recoveryShipBlastoise)
			}
		})
	}
}

// The recovery fake's ordinary departure requires purchased grain. This local
// handler instead validates a real ballast request against the observed fleet,
// records its immutable body and returns the interpreted departure result.
func scoutAcceptanceServeDeparture(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/fleet/depart" {
			downstream.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			ShipID      string `json:"ship_id"`
			Destination string `json:"dest_port_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Error(err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		h.api.mu.Lock()
		defer h.api.mu.Unlock()
		if !h.api.authorized(request) {
			h.api.writeProblem(writer, http.StatusUnauthorized, "invalid_api_key", "synthetic credential rejected")
			return
		}
		ship, exists := runner.ship(payload.ShipID)
		mission, data, err := runner.loadMission(h.ctx, payload.ShipID)
		if !exists || err != nil || mission == nil || data.Relocation == nil || !data.Relocation.Scout ||
			!emptyCargo(ship) || ship.Ship.Status == "sailing" || payload.Destination != data.Plan.Destination || ship.Stores.ProvisionsTons < data.Plan.TargetStoresTons {
			t.Errorf("invalid ballast departure: body=%s mission=%+v err=%v", body, mission, err)
			h.api.writeProblem(writer, http.StatusConflict, "invalid_scout_departure", "ballast request disagrees with authoritative fleet or safe stores")
			return
		}
		h.api.record(request, body, http.StatusOK)
		h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": payload.ShipID, "dest_port_id": payload.Destination, "berth_dues_pence": 0, "berth_dues_debt_pence": 0})
	})
}

func scoutAcceptanceAdmit(t *testing.T, h *recoveryHarness, runner *Runner) (*store.Mission, missionData) {
	t.Helper()
	changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
	if err != nil || !changed {
		t.Fatalf("admit execution scout: changed=%v reason=%q err=%v holds=%+v", changed, reason, err, runner.holding)
	}
	return h.mission(recoveryShipBlastoise)
}

func TestScoutExecutesOneBallastDepartureAndSettlesArrivalWithoutInspectionWait(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	mission, data := scoutAcceptanceAdmit(t, h, runner)
	target, commodity, cap := data.Plan.Destination, data.Plan.Commodity, data.Relocation.ExpenseCapPence
	before, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	scoutAcceptanceServeDeparture(t, h, runner)
	for range 10 {
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
		if h.api.mutationCount("/v1/fleet/depart") > 0 {
			break
		}
	}
	calls := h.api.mutationsFor("/v1/fleet/depart")
	if len(calls) != 1 {
		t.Fatalf("scout sent %d departures, want one: holds=%+v", len(calls), runner.holding)
	}
	var sent struct {
		ShipID      string `json:"ship_id"`
		Destination string `json:"dest_port_id"`
	}
	if err := json.Unmarshal(calls[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.ShipID != recoveryShipBlastoise || sent.Destination != target || calls[0].IdempotencyKey == "" {
		t.Fatalf("wrong scout departure request: %+v", sent)
	}
	// Fresh authoritative stationary arrival, without advancing six game hours.
	for i := range runner.stateCache.Ships {
		if runner.stateCache.Ships[i].Ship.ID == recoveryShipBlastoise {
			runner.stateCache.Ships[i].Ship.CurrentPortID = target
			runner.stateCache.Ships[i].Ship.Status = "anchored_roadstead"
			runner.stateCache.Ships[i].Voyage = nil
			runner.stateCache.Ships[i].Stores.ProvisionsTons -= 0.1
		}
	}
	runner.stateCache.Clock.Calendar.TotalGameHours++
	for range 2 {
		mission, data = h.mission(recoveryShipBlastoise)
		if _, err := runner.driveMission(h.ctx, harnessShip(t, runner, recoveryShipBlastoise), mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	lane := scoutLaneKey(target, "hamburg", commodity)
	rows := destinationSettlements(t, h, lane)
	if len(rows) != 1 {
		t.Fatalf("arrival did not settle within two passes: rows=%+v holds=%+v", rows, runner.holding)
	}
	row := rows[0]
	if row.Kind != "relocation" || row.QuantityTons != 0 || row.ProceedsPence != 0 || row.AcquisitionPence != 0 || row.ProbeOutcome != "" || row.ExplorationExpensePence == nil || *row.ExplorationExpensePence < 0 || *row.ExplorationExpensePence > cap {
		t.Fatalf("scout fabricated trading income, inspection failure, or unbounded expense: %+v", row)
	}
	_, data = h.mission(recoveryShipBlastoise)
	if data.Relocation.SourceInspected {
		t.Fatal("scout was classified as an inspected charter source")
	}
	after, err := h.db.EconomicHistory(h.ctx, h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.ProbeExpensePence-before.ProbeExpensePence != *row.ExplorationExpensePence {
		t.Fatalf("permanent probe expense delta=%d, want %d", after.ProbeExpensePence-before.ProbeExpensePence, *row.ExplorationExpensePence)
	}
	if h.api.mutationCount("/v1/fleet/depart") != 1 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatal("scout bought speculative cargo or sailed an empty return")
	}
	for _, laden := range []bool{false, true} {
		history := probeHistoryFixture()
		history.settlement(row, nil)
		history.departure(row.ShipID, target, row.CompletedAt.Add(time.Second), laden, shipIdleState{})
		ship := history.Ships[row.ShipID]
		if !laden && ship.Failures != 1 {
			t.Fatalf("later empty departure did not count exactly one ship failure: %+v", ship)
		}
		if laden && ship.Failures != 0 {
			t.Fatalf("laden departure after scouting did not reset the ship streak: %+v", ship)
		}
		if _, exists := history.Sources[target+"|"+commodity]; exists {
			t.Fatal("scout departure polluted charter source history")
		}
	}
}

func TestScoutCancelsAtOriginBeforeDepartureWhenTargetOccupiedOrTradeExecutable(t *testing.T) {
	for _, scenario := range []string{"occupied target", "ordinary opportunity"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := scoutAcceptanceRunner(t)
			mission, data := scoutAcceptanceAdmit(t, h, runner)
			target := data.Plan.Destination
			mission.Phase = "depart"
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if scenario == "occupied target" {
				runner.stateCache.Ships[0].Ship.CurrentPortID = target
			} else {
				// A fresh measured destination book supersedes the stale hint.
				runner.stateCache.Markets[target] = sourcingFixture[Market](t, fmt.Sprintf(`{"port_id":%q,"visibility":"quay","customs_bps":0,"books":[{"commodity_id":%q,"depth":{"asks":[],"bids":[{"order_id":"new-demand","price_pence":200000,"remaining_quantity":100}]}}]}`, target, data.Plan.Commodity))
				runner.meta["books/"+target] = runner.meta["books/hamburg"]
				in, err := runner.sourcingInput(h.ctx, harnessShip(t, runner, recoveryShipBlastoise))
				if err != nil {
					t.Fatal(err)
				}
				ordinary := PlanVoyages(in)
				if len(ordinary) == 0 || !ordinary[0].Accepted {
					t.Fatalf("cancellation fixture has no executable ordinary plan: %+v", ordinary)
				}
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
			if h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("cancelled scout departed")
			}
			found := false
			for _, hold := range runner.holding {
				if hold.ShipID == recoveryShipBlastoise && strings.HasPrefix(hold.Reason, "sourcing relocation cancelled before further spending: ") {
					found = true
				}
			}
			if !found {
				t.Fatalf("cancellation omitted its actionable hold: %+v", runner.holding)
			}
			if ship := harnessShip(t, runner, recoveryShipBlastoise); ship.Ship.CurrentPortID != "hamburg" {
				t.Fatalf("cancellation moved the scout: %+v", ship)
			}
		})
	}
}

func TestScoutCooldownCoversEveryHintCommodityAtTheTarget(t *testing.T) {
	in := scoutAcceptanceInput(t)
	home := in.Ports["hamburg"]
	hint := home.CoffeeHouseGossip[0]
	bid := int64(30_000)
	hint.Commodities = append(hint.Commodities, GossipCommodity{
		CommodityID: "salted_meat", SnapshotAt: hint.SnapshotAt, BestBidPence: &bid,
	})
	home.CoffeeHouseGossip[0] = hint
	in.Ports["hamburg"] = home
	baseline := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam")
	if !baseline.Accepted || baseline.Commodity != "copper_sheathing" {
		t.Fatalf("fixture needs copper as the best Amsterdam hint: %+v", baseline)
	}
	in.SourcingProbeCompletions[scoutLaneKey("amsterdam", "hamburg", "salted_meat")] = in.Now.Add(-23*time.Minute - 59*time.Second)
	if got := scoutAcceptancePlan(t, scoutCandidates(in, nil), "amsterdam"); got.Accepted {
		t.Fatalf("changing to the best commodity evaded the target lane cooldown: %+v", got)
	}
}

func TestScoutCommittedStoreBudgetUsesScoutExposureWithoutACharter(t *testing.T) {
	h, runner := scoutAcceptanceRunner(t)
	for i := range runner.stateCache.Ships {
		if runner.stateCache.Ships[i].Ship.ID == recoveryShipBlastoise {
			runner.stateCache.Ships[i].Stores.ProvisionsTons = 0
		}
	}
	_, data := scoutAcceptanceAdmit(t, h, runner)
	ship := harnessShip(t, runner, recoveryShipBlastoise)
	input, err := runner.sourcingInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	quantity, limit := data.Plan.StoresToBuy, data.Plan.StoresLimitPence
	if quantity <= 0 || limit <= 0 || data.Plan.ContractID != "" {
		t.Fatalf("fixture needs a charter-free scout with a measured store deficit: %+v", data.Plan)
	}
	if !runner.relocationProvisionBudget(&data, ship, input, quantity, limit) {
		t.Fatal("committed scout provision budget incorrectly required a charter")
	}
	if err := runner.relocationSpendingReadyAtPrice(h.ctx, ship, &data, 0, limit); err != nil {
		t.Fatalf("fresh committed scout spending incorrectly required a charter: %v", err)
	}
	data.Relocation.ExpenseCapPence = 1
	if runner.relocationProvisionBudget(&data, ship, input, quantity, limit) {
		t.Fatal("scout provision path bypassed its immutable expense cap")
	}
	if err := runner.relocationSpendingReadyAtPrice(h.ctx, ship, &data, 0, limit); err == nil {
		t.Fatal("scout spending path bypassed its immutable expense cap")
	}
}
