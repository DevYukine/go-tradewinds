package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestPhaseOneCapacityBuildWaitsUntilOverseasExhaustion(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		phase    int
		noGossip bool
		loss     bool
		wantHold bool
	}{
		{name: "Phase 1 remains open to local builds", phase: 1},
		{name: "unexplored retry holds Europe", phase: 2, wantHold: true},
		{name: "missing gossip still holds Europe", phase: 2, noGossip: true, wantHold: true},
		{name: "paid loss backoff opens Europe", phase: 2, loss: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h, runner, yard, hull, _ := overseasCapacityFixture(t)
			runner.stateCache.Clock.UnlockedPhase = scenario.phase
			retry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
			outcome := expansionOutcome{RetryAt: retry}
			if scenario.loss {
				outcome.Losses = 1
			}
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{yard.BankingRegion: outcome})
			if scenario.noGossip {
				for portID, detail := range runner.stateCache.Ports {
					detail.CoffeeHouseGossip = nil
					runner.stateCache.Ports[portID] = detail
				}
			}
			evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			home, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			local, _, err := runner.capacityAtYard(h.ctx, home, hull, runner.stateCache.Quotes[home.ID+"|"+hull.ID], input, &evidence)
			if err != nil || !local.Accepted {
				t.Fatalf("local build must be admissible before overseas preference: %+v, %v", local, err)
			}
			if scenario.wantHold {
				admission, reason, err := runner.expansionAdmission(h.ctx)
				if err != nil || admission.region != yard.BankingRegion || !admission.retryUntil.Equal(retry) ||
					reason != "expansion region waits for its retry deadline" {
					t.Fatalf("overseas region must wait without being exhausted: %+v, %q, %v", admission, reason, err)
				}
			}
			changed, err := runner.considerGrowth(h.ctx)
			if err != nil || changed == scenario.wantHold {
				t.Fatalf("growth changed=%t, want hold=%t, error=%v, holds=%+v", changed, scenario.wantHold, err, runner.holding)
			}
			if scenario.wantHold {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				requireReviewHold(t, runner, "Phase 1 growth waits: new hulls go overseas", true)
				requireReviewHold(t, runner, "earliest overseas retry "+retry.Format(time.RFC3339), true)
			} else {
				_, data := h.mission("__growth")
				if data.BuildPort != home.ID || data.Capacity == nil || data.Action == nil || data.Action.Stage != "build" {
					t.Fatalf("exhausted overseas or Phase 1 world must stage local capacity: %+v", data)
				}
			}
			if got := h.api.mutationCount("/v1/shipyard/build-ship"); got != 0 {
				t.Fatalf("admission sent %d builds instead of only staging", got)
			}
		})
	}
}

func TestUnhiredExpansionPairRetriesWithinHours(t *testing.T) {
	attempt := time.Date(2026, time.November, 1, 9, 0, 0, 0, time.UTC)
	for _, scenario := range []struct {
		name    string
		retry   time.Time
		outcome expansionOutcome
		want    time.Time
	}{
		{name: "neither factor hired", retry: attempt, outcome: expansionOutcome{AttemptAt: attempt}, want: attempt.Add(3 * time.Hour)},
		{name: "factor hired", retry: attempt.Add(10 * time.Minute), outcome: expansionOutcome{AttemptAt: attempt}, want: attempt.Add(10*time.Minute + 24*time.Hour)},
		{name: "loss keeps daily floor", retry: attempt, outcome: expansionOutcome{AttemptAt: attempt, Losses: 1}, want: attempt.Add(24 * time.Hour)},
		{name: "unevaluated pair keeps daily floor", retry: attempt, outcome: expansionOutcome{AttemptAt: attempt, UnevaluatedAttemptAt: attempt}, want: attempt.Add(24 * time.Hour)},
		{name: "explicit later retry survives", retry: attempt, outcome: expansionOutcome{AttemptAt: attempt, RetryAt: attempt.Add(6 * time.Hour)}, want: attempt.Add(6 * time.Hour)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runner := &Runner{snapshot: map[string]json.RawMessage{
				"_expansion_agent_outcomes": encode(map[string]expansionOutcome{"america": scenario.outcome}),
				"_expansion_agent_retries":  encode(map[string]time.Time{"america": scenario.retry}),
			}}
			if got := runner.expansionRetryUntil("america"); !got.Equal(scenario.want) {
				t.Fatalf("retry deadline=%s, want %s", got, scenario.want)
			}
		})
	}
}

func TestViableOverseasCandidateWinsOverPhaseOneYard(t *testing.T) {
	for _, saving := range []bool{false, true} {
		name := "without savings target"
		if saving {
			name = "later overseas savings target survives local candidate filtering"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			overseasMetric(t, h, 10_000_000, time.Now().UTC())
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			bookCommodity(runner.stateCache.Markets["new_york"], recoveryGrain).Depth.Bids[0].PricePence = bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence
			cost := *quote.TotalCostPence * 2
			quote.TotalCostPence = &cost
			runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
			evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			home, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			evaluate := func(port Port, quote YardQuote) GrowthDecision {
				t.Helper()
				grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
				candidateInput := input
				candidateInput.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
				decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, candidateInput, &evidence)
				if err != nil {
					t.Fatal(err)
				}
				return decision
			}
			local := evaluate(home, runner.stateCache.Quotes[home.ID+"|"+hull.ID])
			overseas := evaluate(yard, quote)
			if !local.Accepted || !overseas.Accepted || !overseas.EconomicallyViable || !betterCapacityCandidate(local, overseas) {
				t.Fatalf("both builds must be viable while local scores higher: local=%+v overseas=%+v", local, overseas)
			}
			if saving {
				capital := growthCashTotal(overseas.QuotedHullPence, overseas.SlippageReservePence, overseas.CannonsPence, overseas.InitialStoresPence)
				target := capacitySavingsTarget{
					PortID: yard.ID, HullID: hull.ID, CapitalPence: capital,
					RequiredPence:           growthCashTotal(capital, overseas.WorkingCapitalPence),
					DepthNetPencePerGameDay: overseas.DepthNetPencePerGameDay,
					ExpiresAt:               time.Now().UTC().Add(time.Hour),
				}
				if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
					t.Fatal(err)
				}
			}
			if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
				t.Fatalf("overseas growth did not stage: changed=%t error=%v holds=%+v", changed, err, runner.holding)
			}
			_, data := h.mission("__growth")
			if data.BuildPort != yard.ID || data.BuildClass != hull.ID || data.Capacity == nil || data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("higher-scoring local yard displaced viable overseas build: %+v", data)
			}
			if saving {
				if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
					t.Fatalf("staged overseas savings target was not consumed: %+v", target)
				}
			}
		})
	}
}

func removeExpansionGossip(runner *Runner) {
	for portID, detail := range runner.stateCache.Ports {
		detail.CoffeeHouseGossip = nil
		runner.stateCache.Ports[portID] = detail
	}
}

func TestOverseasProbeFundsAndHiresBothFactorsWithoutGossipOrBuildEstimate(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	removeExpansionGossip(runner)
	runner.stateCache.Quotes = nil
	h.api.setVaultBalance("america", 0)
	overseasPairSyncFactors(h, runner)
	if pairs := expansionAgentYards(runner.stateCache, runner.planningInput(ShipState{})); len(pairs) != 0 {
		t.Fatalf("probe fixture unexpectedly has a gossip spread: %+v", pairs)
	}
	admission, reason, err := runner.expansionAdmission(h.ctx)
	if err != nil || reason != "" || !admission.probe || admission.yard.ID != "boston" || admission.partner.ID != "new_york" ||
		admission.quote.PortID != "" || admission.build.Accepted || admission.build.ExpansionEstimate != nil {
		t.Fatalf("missing gossip or quotes prevented an evidence-only probe: %+v %q %v", admission, reason, err)
	}
	_, fees, transfers, funded := measuredFunding(admission.input.Balances, admission.required, admission.input.Rates)
	if !funded || fees <= 0 || len(transfers) == 0 ||
		admission.required["america"] < runner.opts.ReservePence+2*expansionAgentHireFeePence {
		t.Fatalf("probe did not protect both hire fees, reserves and transfer fees: %+v fees=%d transfers=%+v", admission.required, fees, transfers)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("probe did not stage its regional funding: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" || mission.Destination != "america" {
		t.Fatalf("probe bypassed regional funding: %+v %+v", mission, data.Action)
	}
	agents := persistedExpansionAgents(t, h)
	if len(agents) != 2 || !agents[0].Probe || !agents[1].Probe || agents[0].Partner != agents[1].PortID ||
		agents[1].Partner != agents[0].PortID || !agents[0].HiredAt.IsZero() || !agents[1].HiredAt.IsZero() {
		t.Fatalf("probe selection was not durable before funding: %+v", agents)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("probe remittance failed: %t %v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	overseasPairSyncFactors(h, runner)
	for _, portID := range []string{"boston", "new_york"} {
		if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
			t.Fatalf("probe did not resume %s after funding: %t %v", portID, changed, err)
		}
		mission, data = h.mission("__treasury")
		if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, portID) {
			t.Fatalf("probe changed its recorded hire order: %+v", data.Action)
		}
		if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
			t.Fatalf("probe %s failed first-send admission without a quote: %t %v", portID, changed, err)
		}
		if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
			t.Fatal(err)
		}
		overseasPairSyncFactors(h, runner)
	}
	if runner.planningInput(ShipState{}).Balances["america"] < runner.opts.ReservePence ||
		h.api.mutationCount("/v1/company/hire-factor") != 2 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 ||
		decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() {
		t.Fatal("probe spent its protected reserve, skipped a hire, built without evidence or omitted its paid-start floor")
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range history.([]store.Decision) {
		if row.Kind == "expansion_agent" && row.Reason == expansionProbeReason {
			found = true
		}
	}
	if !found {
		t.Fatal("probe admission did not disclose its evidence-only reason")
	}
	h.missingMission("__growth")
}

func TestOverseasProbeRetainsSafetyAndFundingGates(t *testing.T) {
	for _, guard := range []string{"arrival hazard", "unrated water", "nine calibration samples", "stale zones", "stale net worth", "eleven factors", "unfunded fees"} {
		t.Run(guard, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			removeExpansionGossip(runner)
			switch guard {
			case "arrival hazard":
				zones := decode[[]ZoneObservation](runner.snapshot, "zones")
				for index := range zones {
					if zones[index].SeaZoneID == "america-water" {
						zones[index].NetPiracyBPS = 51
					}
				}
				runner.snapshot["zones"] = encode(zones)
			case "unrated water":
				for index := range runner.stateCache.Catalog.SeaZones {
					if runner.stateCache.Catalog.SeaZones[index].ID == "america-water" {
						runner.stateCache.Catalog.SeaZones[index].ZoneClass = "regional"
					}
				}
			case "nine calibration samples":
				delete(runner.stateCache.HazardQuotes, "overseas-sample-09")
			case "stale zones":
				meta := runner.meta["zones"]
				meta.Stale = true
				runner.meta["zones"] = meta
			case "stale net worth":
				overseasMetric(t, h, 10_000_000, time.Now().UTC().Add(-6*time.Hour-time.Second))
			case "eleven factors":
				for len(runner.stateCache.Factors) < 11 {
					runner.stateCache.Factors = append(runner.stateCache.Factors, "occupied-"+strings.Repeat("x", len(runner.stateCache.Factors)))
				}
			case "unfunded fees":
				required, err := runner.treasuryReserves(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				for index := range runner.stateCache.Vaults {
					vault := &runner.stateCache.Vaults[index]
					vault.BalancePence = required[vault.BankingRegion]
				}
			}
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
				t.Fatalf("%s admitted a paid probe: %t %v", guard, changed, err)
			}
			h.missingMission("__treasury")
			if len(decode[[]expansionAgent](runner.snapshot, "_expansion_agents")) != 0 {
				t.Fatal("rejected probe retained chargeable factor intents")
			}
			if guard != "arrival hazard" && guard != "unrated water" && guard != "eleven factors" && runner.overseasExhausted() {
				t.Fatalf("%s incorrectly released Europe", guard)
			}
			if guard == "eleven factors" && !runner.overseasExhausted() {
				t.Fatal("without a live pair or two free slots, no overseas yard can be probed")
			}
		})
	}
}

func TestPhaseOneGrowthOpensAfterAmericanLossAndCaribbeanHazardRejection(t *testing.T) {
	h, runner, yard, _, _ := overseasCapacityFixture(t)
	removeExpansionGossip(runner)
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
		Port{ID: "havana", BankingRegion: "caribbean", SeaZoneID: "caribbean-water", Phase: 2, HasShipyard: true},
		Port{ID: "port_royal", BankingRegion: "caribbean", SeaZoneID: "caribbean-water", Phase: 2})
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones,
		Zone{ID: "caribbean-water", ZoneClass: "coastal", BaseStormBPS: 151})
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	runner.snapshot["zones"] = encode(append(zones, ZoneObservation{SeaZoneID: "caribbean-water"}))
	if runner.overseasExhausted() {
		t.Fatal("hazard-rejected Caribbean hid probe-able unexplored America")
	}
	if handled, changed, err := runner.holdExpansionGrowth(h.ctx, false, true); err != nil || !handled || changed {
		t.Fatalf("Europe did not wait for probe-able America: %t %t %v", handled, changed, err)
	}
	requireReviewHold(t, runner, "america: probe-able overseas yard", true)
	runner.markExpansionLoss(expansionAgent{
		PortID: yard.ID, Region: yard.BankingRegion, HiredAt: time.Now().UTC().Add(-3 * time.Hour),
	}, "expansion pair has no active profitable plan or saved build")
	if !runner.overseasExhausted() {
		t.Fatal("paid American loss and measured Caribbean hazard did not exhaust overseas options")
	}
	if handled, _, err := runner.holdExpansionGrowth(h.ctx, false, true); err != nil || handled {
		t.Fatalf("exhausted overseas regions retained the European hold: %t %v", handled, err)
	}
	runner.holding = nil
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("exhausted overseas regions prevented European construction: %t %v holds=%v", changed, err, runner.holding)
	}
	_, build := h.mission("__growth")
	if build.BuildPort != recoveryHomePort || build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("overseas exhaustion did not release the safe European build: %+v", build)
	}
}

func TestOverseasProbeRanksDiscountYardsAndRetainedPartnerObservations(t *testing.T) {
	state, input := overseasPairRankingFixture()
	state.Catalog.SeaZones = []Zone{
		{ID: "safe", ZoneClass: "coastal", BaseStormBPS: 100},
		{ID: "riskier", ZoneClass: "coastal", BaseStormBPS: 150},
	}
	discount := &gameapi.YardDiscount{LabourDiscountBPS: 3000, TurnkeyMarkupWaived: true}
	state.Catalog.Ports = []Port{
		{ID: "ordinary", BankingRegion: "america", SeaZoneID: "safe", HasShipyard: true, Phase: 2},
		{ID: "discount_riskier", BankingRegion: "america", SeaZoneID: "riskier", HasShipyard: true, Phase: 2, YardDiscount: discount},
		{ID: "discount_safe", BankingRegion: "america", SeaZoneID: "safe", HasShipyard: true, Phase: 2, YardDiscount: discount},
		{ID: "partner_rich", BankingRegion: "america", SeaZoneID: "riskier", Phase: 2},
		{ID: "partner_safe", BankingRegion: "america", SeaZoneID: "safe", Phase: 2},
		{ID: "a_partner_safe", BankingRegion: "america", SeaZoneID: "safe", Phase: 2},
	}
	bid := int64(100)
	for _, mode := range []string{"most observations", "lower hazard tie", "port identity tie"} {
		t.Run(mode, func(t *testing.T) {
			wantPartner := "partner_rich"
			gossip := []Gossip{
				{SubjectPortID: "partner_rich", SnapshotAt: input.Now, Commodities: []GossipCommodity{
					{CommodityID: "x", SnapshotAt: input.Now, BestBidPence: &bid},
					{CommodityID: "y", SnapshotAt: input.Now, BestBidPence: &bid},
				}},
			}
			if mode != "most observations" {
				wantPartner = "partner_safe"
				gossip = append(gossip, Gossip{SubjectPortID: "partner_safe", SnapshotAt: input.Now, Commodities: gossip[0].Commodities})
			}
			if mode == "port identity tie" {
				wantPartner = "a_partner_safe"
				gossip = append(gossip, Gossip{SubjectPortID: wantPartner, SnapshotAt: input.Now, Commodities: gossip[0].Commodities})
			}
			input.Ports = map[string]PortDetail{recoveryHomePort: {CoffeeHouseGossip: gossip}}
			input.Catalog = state.Catalog
			input.Zones = []ZoneObservation{{SeaZoneID: "safe"}, {SeaZoneID: "riskier"}}
			pairs := expansionProbeYards(state, input, false)
			if len(pairs) != 3 || pairs[0].yard.ID != "discount_safe" || pairs[1].yard.ID != "discount_riskier" ||
				pairs[0].partner.ID != wantPartner {
				t.Fatalf("probe ranking ignored discounts, hazard or retained prices: %+v", pairs)
			}
		})
	}
}

func TestOverseasExhaustionWaitsForExistingExplorationWithoutRecordedLoss(t *testing.T) {
	h, runner, yard, _, _ := overseasCapacityFixture(t)
	now := time.Now().UTC()
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: now.Add(-time.Hour), EvaluatedAt: now},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: now.Add(-time.Hour), EvaluatedAt: now},
	})
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	for index := range zones {
		if zones[index].SeaZoneID == "america-water" {
			zones[index].NetPiracyBPS = 51
		}
	}
	runner.snapshot["zones"] = encode(zones)
	if runner.overseasExhausted() {
		t.Fatal("a changed pre-exploration hazard gate bypassed an existing paid pair's observation floor")
	}
	if handled, changed, err := runner.holdExpansionGrowth(h.ctx, false, true); err != nil || !handled || changed {
		t.Fatalf("existing exploration released Europe before its loss was recorded: %t %t %v", handled, changed, err)
	}
	requireReviewHold(t, runner, "existing overseas exploration is still within its observation floor", true)
}

func TestOverseasRegionWithoutProbeableYardIsExhausted(t *testing.T) {
	for _, topology := range []string{"no yard", "cove yard", "oceanic yard", "no partner"} {
		t.Run(topology, func(t *testing.T) {
			_, runner, yard, _, _ := overseasCapacityFixture(t)
			for index := range runner.stateCache.Catalog.Ports {
				port := &runner.stateCache.Catalog.Ports[index]
				if port.ID == yard.ID {
					switch topology {
					case "no yard":
						port.HasShipyard = false
					case "cove yard":
						port.IsCove = true
					case "oceanic yard":
						port.SeaZoneID = "unprobeable-ocean"
					}
				}
				if topology == "no partner" && port.ID == "new_york" {
					port.IsCove = true
				}
			}
			runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones,
				Zone{ID: "unprobeable-ocean", ZoneClass: "oceanic"})
			if !runner.overseasExhausted() {
				t.Fatalf("%s left an impossible probe holding Europe", topology)
			}
		})
	}
}

func TestMeasuredUnbuildableOverseasYardExhaustsRegionUntilRetry(t *testing.T) {
	for _, rejection := range []string{"unprofitable depth", "unavailable hull", "stale quay waits"} {
		t.Run(rejection, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			overseasMetric(t, h, 10_000_000, time.Now().UTC())
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			switch rejection {
			case "unprofitable depth":
				bookCommodity(runner.stateCache.Markets["new_york"], recoveryGrain).Depth.Bids[0].PricePence = 1
			case "unavailable hull":
				quote.Available = false
				runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
			case "stale quay waits":
				meta := runner.meta["books/"+yard.ID]
				meta.Stale = true
				runner.meta["books/"+yard.ID] = meta
			}
			evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			changed, err := runner.considerCapacityGrowth(h.ctx, &evidence)
			if err != nil {
				t.Fatal(err)
			}
			outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")[yard.BankingRegion]
			if rejection == "stale quay waits" {
				if runner.overseasExhausted() || !outcome.CapacityRetryAt.IsZero() || changed {
					t.Fatalf("missing authority exhausted overseas capacity: %+v, changed=%t", outcome, changed)
				}
				h.missingMission("__growth")
				return
			}
			if !changed || !runner.overseasExhausted() || outcome.CapacityReason == "" ||
				!outcome.CapacityRetryAt.After(time.Now().UTC().Add(23*time.Hour)) || outcome.Losses != 0 {
				t.Fatalf("measured overseas rejection did not release Europe with a persisted retry: %+v, changed=%t", outcome, changed)
			}
			_, build := h.mission("__growth")
			if build.BuildPort != recoveryHomePort || build.Action == nil || build.Action.Stage != "build" {
				t.Fatalf("measured overseas exhaustion did not stage safe local capacity: %+v", build)
			}
			persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			runner.snapshot = persisted
			if !runner.overseasExhausted() {
				t.Fatal("measured capacity exhaustion was lost on snapshot reload")
			}
			outcome.CapacityRetryAt = time.Now().UTC().Add(-time.Second)
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{yard.BankingRegion: outcome})
			if runner.overseasExhausted() {
				t.Fatal("expired capacity retry kept Europe open without another overseas evaluation")
			}
		})
	}
}

func TestOverseasPresenceAwayFromYardStillAdmitsEvidenceProbe(t *testing.T) {
	for _, presence := range []string{"berthed ship", "arriving ship", "factor"} {
		t.Run(presence, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			removeExpansionGossip(runner)
			port, _ := findPort(runner.stateCache.Catalog, "new_york")
			port.ID = "zz-america-non-yard-presence"
			runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, port)
			if presence == "factor" {
				runner.stateCache.Factors = append(runner.stateCache.Factors, port.ID)
			} else {
				ship := runner.stateCache.Ships[0]
				ship.Ship.ID = "overseas-non-yard-ship"
				ship.Ship.CurrentPortID, ship.Ship.Status = port.ID, "berthed"
				ship.Voyage = nil
				if presence == "arriving ship" {
					ship.Ship.CurrentPortID, ship.Ship.Status = "", "sailing"
					ship.RouteLegs = []RouteLeg{{ToNode: port.ID}}
				}
				runner.stateCache.Ships = append(runner.stateCache.Ships, ship)
			}
			exhausted, detail, _ := runner.overseasGrowthStatus()
			if exhausted || !strings.Contains(detail, "probe-able overseas yard awaits quay evidence") {
				t.Fatalf("presence away from a yard released Europe before evidence: exhausted=%t detail=%q", exhausted, detail)
			}
			admission, reason, err := runner.expansionAdmission(h.ctx)
			if err != nil || reason != "" || !admission.probe || admission.yard.ID != "boston" ||
				admission.partner.ID != "new_york" {
				t.Fatalf("regional presence prevented an otherwise funded yard probe: %+v %q %v", admission, reason, err)
			}
			if _, _, _, funded := measuredFunding(admission.input.Balances, admission.required, admission.input.Rates); !funded {
				t.Fatal("probe with regional presence was not funded")
			}
			if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || !changed {
				t.Fatalf("probe with regional presence was not staged: %t %v", changed, err)
			}
			agents := persistedExpansionAgents(t, h)
			if len(agents) != 2 || !agents[0].Probe || !agents[1].Probe {
				t.Fatalf("regional presence bypassed durable paired probing: %+v", agents)
			}
			h.missingMission("__growth")
		})
	}
}

func TestOverseasProbeRetainsOlderFactorUntilDismissalWithoutFailingNewPair(t *testing.T) {
	for _, evaluated := range []bool{false, true} {
		t.Run(map[bool]string{false: "older factor was unevaluated", true: "older factor was evaluated"}[evaluated], func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			removeExpansionGossip(runner)
			partner, _ := findPort(runner.stateCache.Catalog, "new_york")
			partner.ID = "philadelphia"
			runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, partner)
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "new_york")
			overseasPairSyncFactors(h, runner)
			old := expansionAgent{
				PortID: "new_york", Region: "america",
				CreatedAt: time.Now().UTC().Add(-26 * time.Hour),
				HiredAt:   time.Now().UTC().Add(-25 * time.Hour),
			}
			if evaluated {
				old.EvaluatedAt = old.HiredAt.Add(time.Hour)
			}
			overseasPairSave(t, h, runner, []expansionAgent{old})
			admission, reason, err := runner.expansionAdmission(h.ctx)
			if err != nil || reason != "" || !admission.probe || admission.yard.ID != "boston" ||
				admission.partner.ID != partner.ID {
				t.Fatalf("existing non-yard factor was reused or blocked the new probe: %+v %q %v", admission, reason, err)
			}
			if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || !changed {
				t.Fatalf("new probe was not staged beside its retained older factor: %t %v", changed, err)
			}
			agents := persistedExpansionAgents(t, h)
			seen := make(map[string]bool)
			for _, agent := range agents {
				if seen[agent.PortID] {
					t.Fatalf("new probe duplicated a retained factor record: %+v", agents)
				}
				seen[agent.PortID] = true
				if agent.PortID == old.PortID && agent != old {
					t.Fatalf("new probe changed its older factor's identity or dismissal floor: %+v", agent)
				}
			}
			if len(agents) != 3 || !seen[old.PortID] || !seen[admission.yard.ID] || !seen[admission.partner.ID] {
				t.Fatalf("new probe orphaned a still-hired factor or omitted a selected port: %+v", agents)
			}
			attempt := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"].AttemptAt
			mission, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "hire-factor" || mission.Destination != "boston" {
				t.Fatalf("new probe did not stage the new yard's hire: %+v", data)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("new yard hire failed beside the retained factor: %t %v", changed, err)
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			overseasPairSyncFactors(h, runner)
			agents = decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			for index := range agents {
				if agents[index].PortID == partner.ID {
					// A cooled partner hire lets unrelated factor cleanup run
					// while the new paid pair is within its observation floor.
					agents[index].RetryAt = time.Now().UTC().Add(time.Minute)
				}
			}
			overseasPairSave(t, h, runner, agents)
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("retained older factor became permanently unmanaged: %t %v", changed, err)
			}
			mission, data = h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "dismiss-factor" || !stringsContainPort(data.Action.Body, old.PortID) {
				t.Fatalf("older factor cleanup changed the new pair instead: %+v", data)
			}
			outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
			if !outcome.AttemptAt.Equal(attempt) || outcome.Losses != 0 || outcome.UnevaluatedAttempts != 0 ||
				outcome.Reason != "" || !outcome.RetryAt.IsZero() || !outcome.UnevaluatedRetryAt.IsZero() {
				t.Fatalf("older factor dismissal failed the new pair's attempt: %+v", outcome)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("retained older factor could not be dismissed: %t %v", changed, err)
			}
			overseasPairSyncFactors(h, runner)
			for _, portID := range runner.stateCache.Factors {
				if portID == old.PortID {
					t.Fatal("dismissed older factor retained permanent upkeep")
				}
			}
			remaining := persistedExpansionAgents(t, h)
			if len(remaining) != 3 {
				t.Fatalf("old factor dismissal lost the retained history or the new pair: %+v", remaining)
			}
			seen = make(map[string]bool)
			for _, agent := range remaining {
				if seen[agent.PortID] || agent.PortID != old.PortID && !agent.Probe {
					t.Fatalf("old factor dismissal duplicated or changed the new pair: %+v", remaining)
				}
				seen[agent.PortID] = true
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
				t.Fatalf("dismissed factor's retained history triggered another cleanup: %t %v", changed, err)
			}
			if runner.overseasExhausted() || h.api.mutationCount("/v1/company/dismiss-factor") != 1 {
				t.Fatal("old factor dismissal prematurely exhausted or repeatedly dismissed the new exploration")
			}
		})
	}
}

func TestUnpaidExpansionCannotInheritAnOlderRetainedFactorLoss(t *testing.T) {
	now := time.Now().UTC()
	agents := []expansionAgent{
		{PortID: "new_york", Region: "america", CreatedAt: now.Add(-26 * time.Hour), HiredAt: now.Add(-25 * time.Hour)},
		{PortID: "boston", Partner: "philadelphia", Region: "america", CreatedAt: now},
		{PortID: "philadelphia", Partner: "boston", Region: "america", CreatedAt: now},
	}
	runner := &Runner{snapshot: map[string]json.RawMessage{
		"_expansion_agents":         encode(agents),
		"_expansion_agent_outcomes": encode(map[string]expansionOutcome{"america": {AttemptAt: now}}),
	}}
	runner.markExpansionLoss(expansionAgent{Region: "america"}, "expansion hire definitively rejected")
	outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
	if outcome.Losses != 0 || outcome.Reason != "" || !outcome.RetryAt.IsZero() {
		t.Fatalf("unpaid new pair inherited an older retained factor's fee loss: %+v", outcome)
	}
	agents[1].HiredAt = now.Add(time.Second)
	runner.snapshot["_expansion_agents"] = encode(agents)
	runner.markExpansionLoss(expansionAgent{Region: "america"}, "expansion hire definitively rejected")
	outcome = decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
	if outcome.Losses != 1 || !outcome.AttemptAt.Equal(now) || !outcome.RetryAt.After(now) {
		t.Fatalf("a paid member of the actual new pair did not retain economic-loss protection: %+v", outcome)
	}
}

func TestPaidUnevaluatedOverseasPairExhaustsRegionDuringItsRetry(t *testing.T) {
	for _, probe := range []bool{false, true} {
		t.Run(map[bool]string{false: "gossip-backed pair", true: "evidence-only probe"}[probe], func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			started := time.Now().UTC().Add(-3 * time.Hour)
			agents := []expansionAgent{
				{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: started, HiredAt: started, Probe: probe},
				{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: started, HiredAt: started, Probe: probe},
			}
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
			overseasPairSyncFactors(h, runner)
			overseasPairSave(t, h, runner, agents)
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("paid unevaluated pair did not finish after its observation floor: %t %v", changed, err)
			}
			outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
			if outcome.Losses != 0 || outcome.UnevaluatedAttempts != 1 ||
				!outcome.UnevaluatedRetryAt.After(time.Now().UTC()) || !runner.overseasExhausted() {
				t.Fatalf("paid pair without quay evaluation did not exhaust its retry window: %+v", outcome)
			}
			persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			runner.snapshot = persisted
			if !runner.overseasExhausted() {
				t.Fatal("paid pair exhaustion did not survive snapshot reload")
			}
			_, detail, retry := runner.overseasGrowthStatus()
			if !strings.Contains(detail, "paid overseas pair ended without a capacity evaluation") ||
				retry.Before(outcome.UnevaluatedRetryAt) {
				t.Fatalf("paid pair exhaustion lost its reason or deadline: %q %v", detail, retry)
			}
			outcome.UnevaluatedRetryAt = time.Now().UTC().Add(-time.Second)
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{"america": outcome})
			if runner.overseasExhausted() {
				t.Fatal("paid-start floor alone exhausted a pair after its failure retry expired")
			}
		})
	}
}

func TestOverseasSpreadWithoutAdmissibleBuildFallsBackToEvidenceProbe(t *testing.T) {
	for _, rejection := range []string{"unprofitable estimate", "no regional hull quote", "cash shortfall waits"} {
		t.Run(rejection, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			switch rejection {
			case "unprofitable estimate":
				for id, detail := range runner.stateCache.Ports {
					for index, report := range detail.CoffeeHouseGossip {
						if report.SubjectPortID == "new_york" {
							bid := int64(101)
							for commodity := range report.Commodities {
								detail.CoffeeHouseGossip[index].Commodities[commodity].BestBidPence = &bid
							}
						}
					}
					runner.stateCache.Ports[id] = detail
				}
			case "no regional hull quote":
				runner.stateCache.Quotes = nil
			case "cash shortfall waits":
				required, err := runner.treasuryReserves(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				for index := range runner.stateCache.Vaults {
					vault := &runner.stateCache.Vaults[index]
					vault.BalancePence = required[vault.BankingRegion]
					if vault.BankingRegion == "america" {
						vault.BalancePence = max(required[vault.BankingRegion], runner.opts.ReservePence) + 2*expansionAgentHireFeePence + 1
					}
				}
			}
			if len(expansionAgentYards(runner.stateCache, runner.planningInput(ShipState{}))) == 0 {
				t.Fatal("fixture lost the positive spread whose build admission is being rejected")
			}
			admission, reason, err := runner.expansionAdmission(h.ctx)
			if err != nil {
				t.Fatal(err)
			}
			if rejection == "cash shortfall waits" {
				if reason == "" || admission.probe || !admission.build.EconomicallyViable || admission.build.CashShortfallPence <= 0 ||
					!strings.Contains(reason, "Cr cash shortfall") {
					t.Fatalf("pure cash shortage bought a probe instead of saving for the overseas build: %+v %q", admission, reason)
				}
				hold := runner.overseasGrowthHoldReason(reason)
				if !strings.Contains(hold, reason) || runner.overseasExhausted() {
					t.Fatalf("cash-only overseas wait released Europe or hid the shortfall: %q", hold)
				}
				return
			}
			if reason != "" || !admission.probe || admission.probeReason != expansionEstimateProbeReason {
				t.Fatalf("non-cash estimate rejection did not fall back to a funded probe: %+v %q", admission, reason)
			}
			if changed, err := runner.stageExpansionAgent(h.ctx, admission, reason); err != nil || !changed {
				t.Fatalf("fallback probe was not staged in the same pass: %t %v", changed, err)
			}
			h.missingMission("__growth")
			_, hire := h.mission("__treasury")
			if hire.Action == nil || hire.Action.Stage != "hire-factor" {
				t.Fatalf("fallback probe staged a hull rather than quay evidence: %+v", hire)
			}
		})
	}
}

func openProbeableAmericaForFinancing(runner *Runner) {
	runner.stateCache.Clock.UnlockedPhase = 2
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
		Port{ID: "overseas-yard", BankingRegion: "america", SeaZoneID: "overseas-water", Phase: 2, HasShipyard: true},
		Port{ID: "overseas-partner", BankingRegion: "america", SeaZoneID: "overseas-water", Phase: 2})
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones,
		Zone{ID: "overseas-water", ZoneClass: "coastal", BaseStormBPS: 1})
	delete(runner.snapshot, "_expansion_agent_outcomes")
}

func TestPhaseOnePlayerRequestAndApprovalRetireBeforeNewDebt(t *testing.T) {
	for _, posted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsubmitted request", true: "open quoted request"}[posted], func(t *testing.T) {
			var h *recoveryHarness
			var runner *Runner
			if posted {
				h, runner = playerLoanAcceptancePosted(t)
				quote := playerLoanAcceptanceQuote(t, h, runner, 3_000)
				playerLoanAcceptancePublishQuote(t, h, runner, quote)
			} else {
				h, runner = playerLoanAcceptanceFixture(t)
				playerLoanAcceptanceAdmit(t, h, runner)
			}
			mission, data := h.mission("__growth")
			requestID := data.PlayerLoan.RequestID
			openProbeableAmericaForFinancing(runner)
			if runner.overseasExhausted() {
				t.Fatal("newly unlocked America must still hold Europe")
			}
			requestsBefore := h.api.mutationCount("/v1/loan/request")
			if changed, err := runner.drivePlayerLoan(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("obsolete Phase 1 player financing was not retired before staging: %t %v", changed, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/request") != requestsBefore || h.api.mutationCount("/v1/loan/approve") != 0 ||
				h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
				t.Fatal("overseas-first financing retirement sent or journaled a new debt/build mutation")
			}
			if posted {
				request := runner.recordedPlayerRequest(&playerLoanIntent{RequestID: requestID})
				if request == nil || request.Status != "open" {
					t.Fatalf("existing remote request must remain open to lapse, not be approved or fabricated closed: %+v", request)
				}
			}
			requireReviewHold(t, runner, "open requests lapse unapproved", true)
		})
	}
}

func TestPhaseOneFinancingYieldsOnlyBeforeItsFirstAcceptedLoan(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "before first acceptance", true: "before construction after acceptance"}[accepted], func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
				t.Fatalf("seed European financing before the overseas world opens: %t %v", changed, err)
			}
			if accepted {
				mission, data := h.mission("__growth")
				if changed, err := runner.driveCapacityFinancing(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("stage original debt: %t %v", changed, err)
				}
				mission, data = h.mission("__growth")
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
					t.Fatalf("accept original debt: %t %v", changed, err)
				}
				loanCapacityRefresh(t, h, runner)
			}
			openProbeableAmericaForFinancing(runner)
			mission, data := h.mission("__growth")
			if changed, err := runner.driveCapacityFinancing(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("Phase 1 financing did not apply the first-acceptance boundary: %t %v", changed, err)
			}
			if accepted {
				mission, build := h.mission("__growth")
				if build.Capacity.Financing.Cancel || build.Action == nil || build.Action.Stage != "build" ||
					len(build.Capacity.Financing.Loans) != 1 {
					t.Fatalf("accepted debt was abandoned instead of completing its admitted build: %+v", build)
				}
				if changed, err := runner.dispatch(h.ctx, mission, &build); err != nil || !changed {
					t.Fatalf("accepted European financing could not finish its build: %t %v", changed, err)
				}
				if _, built := h.mission("__growth"); built.BuiltShipID != recoveryBuiltShipID {
					t.Fatalf("accepted debt lost its built hull: %+v", built)
				}
			} else {
				h.missingMission("__growth")
			}
			wantAccepts := 0
			if accepted {
				wantAccepts = 1
			}
			if h.api.mutationCount("/v1/loan/accept-quote") != wantAccepts ||
				h.api.mutationCount("/v1/shipyard/build-ship") != wantAccepts || h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("overseas-first retirement duplicated debt or repaid a still-valid financed build")
			}
		})
	}
}

func TestPhaseOneFinancingIsNotRetiredOnStaleOverseasAuthority(t *testing.T) {
	for _, dataset := range []string{"catalog", "clock", "ships", "factors", "zones"} {
		t.Run(dataset, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			port, hull, decision, required, evidence := loanCapacityDecision(t, h, runner)
			if changed, err := runner.considerCapacityFinancing(h.ctx, port, hull, decision, required, evidence); err != nil || !changed {
				t.Fatalf("seed financing: %t %v", changed, err)
			}
			openProbeableAmericaForFinancing(runner)
			meta := runner.meta[dataset]
			meta.Stale = true
			runner.meta[dataset] = meta
			mission, data := h.mission("__growth")
			before := string(encode(data))
			handled, changed, err := runner.retirePhaseOneFinancing(h.ctx, mission, &data)
			if err != nil || handled || changed || before != string(encode(data)) {
				t.Fatalf("stale %s authority retired a valid financing intent: handled=%t changed=%t err=%v", dataset, handled, changed, err)
			}
			h.mission("__growth")
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("waiting for fresh overseas authority sent a mutation")
			}
		})
	}
}

func TestPhaseOneAcceptedPlayerLoanSurvivesOverseasRetryAndStaleAuthority(t *testing.T) {
	for _, disturbance := range []string{"expired overseas loss retry", "stale overseas authority"} {
		t.Run(disturbance, func(t *testing.T) {
			h, runner := playerLoanApprovedFixture(t)
			openProbeableAmericaForFinancing(runner)
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
				"america": {Losses: 1, RetryAt: time.Now().UTC().Add(-time.Second)},
			})
			if disturbance == "stale overseas authority" {
				meta := runner.meta["ships"]
				meta.Stale = true
				runner.meta["ships"] = meta
			}
			mission, data := h.mission("__growth")
			handled, changed, err := runner.retirePhaseOneFinancing(h.ctx, mission, &data)
			if err != nil || handled || changed || data.Capacity.Financing.Cancel || len(data.Capacity.Financing.Loans) != 1 {
				t.Fatalf("accepted player debt was retired after %s: handled=%t changed=%t err=%v data=%+v", disturbance, handled, changed, err, data)
			}
			if disturbance == "expired overseas loss retry" {
				playerLoanAcceptanceDrive(t, h, runner)
				playerLoanAcceptanceDispatch(t, h, runner)
				if _, built := h.mission("__growth"); built.BuiltShipID != recoveryBuiltShipID {
					t.Fatalf("accepted player debt did not complete its admitted hull: %+v", built)
				}
			}
			if h.api.mutationCount("/v1/loan/approve") != 1 || h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("overseas retry or stale authority duplicated approval or forced repayment")
			}
		})
	}
}

func TestPhaseOneSavingsAndGrowthMarkersDoNotBlockOverseasProbe(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	removeExpansionGossip(runner)
	runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
		PortID: recoveryHomePort, HullID: recoveryHull, CapitalPence: 100_000, RequiredPence: 200_000,
		DepthNetPencePerGameDay: 1_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	runner.snapshot["_capacity_growth_port"] = encode(recoveryHomePort)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("obsolete local savings blocked overseas evidence: %t %v", changed, err)
	}
	for _, key := range []string{"_capacity_savings_target", "_capacity_growth_port"} {
		if _, exists := runner.snapshot[key]; exists {
			t.Fatalf("overseas admission retained obsolete local marker %s", key)
		}
	}
}

func TestUnpaidExpansionRejectionRetriesWithoutExhaustingOverseas(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(map[bool]string{false: "no fee was paid", true: "first fee was paid"}[paid], func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			now := time.Now().UTC()
			agents := []expansionAgent{
				{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: now, Probe: true},
				{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: now, Probe: true},
			}
			if paid {
				agents[0].HiredAt = now
			}
			runner.snapshot["_expansion_agents"] = encode(agents)
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{"america": {AttemptAt: now}})
			if err := runner.clearAwaitingExpansion(h.ctx, encode(map[string]string{"port_id": "new_york"}), true); err != nil {
				t.Fatal(err)
			}
			outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
			if (outcome.Losses > 0) != paid || runner.overseasExhausted() != paid || !runner.expansionRetryUntil("america").After(now) {
				t.Fatalf("hire refusal confused never-paid retry with a realized fee loss: paid=%t outcome=%+v", paid, outcome)
			}
		})
	}
}

func TestNeverSentPhaseOneFinancingRetiresBeforeMissionDispatch(t *testing.T) {
	for _, journaled := range []bool{false, true} {
		t.Run(map[bool]string{false: "local intent only", true: "possibly sent journal action"}[journaled], func(t *testing.T) {
			h, runner := playerLoanAcceptanceFixture(t)
			playerLoanAcceptanceAdmit(t, h, runner)
			mission, data := h.mission("__growth")
			action := *data.Action
			if journaled {
				if _, err := h.db.BeginAction(h.ctx, &store.Action{
					ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
				}); err != nil {
					t.Fatal(err)
				}
			}
			openProbeableAmericaForFinancing(runner)
			if journaled {
				if handled, changed, err := runner.retirePhaseOneFinancing(h.ctx, mission, &data); err != nil || handled || changed || data.Action == nil {
					t.Fatalf("overseas cutover erased ambiguous immutable debt: %t %t %v", handled, changed, err)
				}
				return
			}
			if changed, err := runner.work(h.ctx); err != nil || !changed {
				t.Fatalf("mission scheduler did not retire the never-sent financing intent: %t %v", changed, err)
			}
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/request") != 0 || len(h.pending()) != 0 {
				t.Fatal("retired Phase 1 financing reached dispatch or created a veto/restage journal loop")
			}
		})
	}
}

func TestExhaustedOverseasCashBlockedCandidateDoesNotHideFundedEuropeanCapacity(t *testing.T) {
	h, runner, yard, hull, quote := overseasCapacityFixture(t)
	overseasMetric(t, h, 1_000_000_000, time.Now().UTC())
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
		yard.BankingRegion: {Losses: 1, RetryAt: time.Now().UTC().Add(24 * time.Hour)},
	})
	evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	price := evidence.available + 100_000
	quote.TotalCostPence, quote.Mortgage = &price, nil
	runner.stateCache.Quotes[yard.ID+"|"+hull.ID] = quote
	grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(yard, hull))
	input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
	overseas, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
	if err != nil || overseas.Accepted || !overseas.EconomicallyViable || overseas.CashShortfallPence <= 0 {
		t.Fatalf("fixture needs economically viable but unfundable overseas capacity: %+v %v", overseas, err)
	}
	if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
		t.Fatalf("exhausted unfunded overseas candidate hid the funded local build: %t %v", changed, err)
	}
	_, build := h.mission("__growth")
	if build.BuildPort != recoveryHomePort || build.Action == nil || build.Action.Stage != "build" {
		t.Fatalf("cash-blocked overseas alternative displaced executable European capacity after exhaustion: %+v", build)
	}
}
