package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpansionFundingResumesSelectedPairAcrossChangingRankings(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.api.setVaultBalance("north", reserves["north"]+50_000)
	h.api.setVaultBalance("america", 0)
	h.api.setVaultBalance("z-donor", 50_000_000)
	runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{BankingRegion: "z-donor", AccountType: "vault"})
	runner.stateCache.Rates = append(runner.stateCache.Rates, Rate{FromRegion: "z-donor", ToRegion: "america", FeeBPS: 100})
	overseasPairSyncFactors(h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("initial exploration funding: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" || mission.Destination != "america" {
		t.Fatalf("fixture did not stage the first partial American transfer: %+v %+v", mission, data.Action)
	}
	agents := persistedExpansionAgents(t, h)
	if len(agents) != 2 || agents[0].PortID != "boston" || agents[0].Partner != "new_york" || !agents[0].HiredAt.IsZero() {
		t.Fatalf("first remittance lacks its durable selected pair: %+v", agents)
	}
	committed, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	if committed.ReserveByRegion["america"] < runner.opts.ReservePence+2*expansionAgentHireFeePence {
		t.Fatalf("other consumers can spend the selected pair's pending fees: %+v", committed.ReserveByRegion)
	}
	if !decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() || h.api.mutationCount("/v1/treasury/remit") != 0 {
		t.Fatal("staging an unpaid intent stamped a paid start or sent before journaling")
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("first partial remittance: %t %v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	overseasPairSyncFactors(h, runner)

	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
		Port{ID: "changing-yard", BankingRegion: "africa", SeaZoneID: "america-water", HasShipyard: true, Phase: 2},
		Port{ID: "changing-partner", BankingRegion: "africa", SeaZoneID: "america-water", Phase: 2})
	ask, bid := int64(100), int64(1_000_000)
	at := time.Now().UTC().Add(-time.Second)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
		Gossip{SubjectPortID: "changing-yard", SnapshotAt: at, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: at, BestAskPence: &ask}}},
		Gossip{SubjectPortID: "changing-partner", SnapshotAt: at, Commodities: []GossipCommodity{{CommodityID: recoveryGrain, SnapshotAt: at, BestBidPence: &bid}}})
	runner.stateCache.Ports[recoveryHomePort] = detail
	if pairs := expansionAgentYards(runner.stateCache, runner.planningInput(ShipState{})); len(pairs) == 0 || pairs[0].yard.ID != "changing-yard" {
		t.Fatalf("gossip did not change the independent ranking: %+v", pairs)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("changed ranking redirected an existing funding intent: %t %v", changed, err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("selected pair did not resume remaining funding: %t %v", changed, err)
	}
	mission, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "remit" || mission.Destination != "america" {
		t.Fatalf("second transfer followed new gossip instead of the selected pair: %+v %+v", mission, data.Action)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("complete selected-pair funding: %t %v", changed, err)
	}
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	overseasPairSyncFactors(h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("completed funding allowed a different unpaid pair: %t %v", changed, err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("completed funding did not resume the selected yard hire: %t %v", changed, err)
	}
	_, data = h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "boston") ||
		h.api.mutationCount("/v1/treasury/remit") != 2 || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatalf("funding churned between regions instead of reaching the recorded hire: %+v", data.Action)
	}
	if !decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() {
		t.Fatal("completed funding, without a hire, consumed the paid-start clock")
	}
}

func TestExpansionUnsentHireExpiresWithoutPaidStartOrLoss(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage an unpaid exploration: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("fixture did not reach the first hire: %+v", data.Action)
	}
	at := time.Now().UTC().Add(-3 * time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: at},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: at},
	})
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("expire a never-sent hire: %t %v", changed, err)
	}
	if agents := persistedExpansionAgents(t, h); len(agents) != 0 {
		t.Fatalf("expired unpaid records still guard exploration: %+v", agents)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 ||
		!decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() {
		t.Fatal("an expired unpaid intent sent a hire or consumed the global paid-start clock")
	}
	outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
	if outcome.Losses != 0 || !outcome.RetryAt.IsZero() || outcome.UnevaluatedAttempts != 0 || !outcome.UnevaluatedRetryAt.IsZero() ||
		!runner.expansionRetryUntil("america").Equal(at.Add(24*time.Hour)) {
		t.Fatalf("expiry became a loss or restarted the regional retry clock: %+v", outcome)
	}
}

func TestExpansionDismissalCountsOnlyFreshEvaluatedLoss(t *testing.T) {
	for _, evaluated := range []string{"never", "at pair time", "after pair time"} {
		t.Run(evaluated, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
			overseasPairSyncFactors(h, runner)
			at := time.Now().UTC().Add(-3 * time.Hour)
			evaluatedAt := time.Time{}
			if evaluated == "at pair time" {
				evaluatedAt = at
			} else if evaluated == "after pair time" {
				evaluatedAt = at.Add(time.Nanosecond)
			}
			overseasPairSave(t, h, runner, []expansionAgent{
				{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: at.Add(-time.Minute), HiredAt: at, EvaluatedAt: evaluatedAt},
				{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: at.Add(-time.Minute), HiredAt: at},
			})
			before := time.Now().UTC()
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("unused pair dismissal: %t %v", changed, err)
			}
			_, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "dismiss-factor" {
				t.Fatalf("pair was not selected for dismissal: %+v", data.Action)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			outcome := decode[map[string]expansionOutcome](snapshot, "_expansion_agent_outcomes")["america"]
			if evaluated == "after pair time" {
				if outcome.Losses != 1 || outcome.RetryAt.IsZero() || outcome.UnevaluatedAttempts != 0 {
					t.Fatalf("evaluated losing pair did not advance loss backoff: %+v", outcome)
				}
			} else if outcome.Losses != 0 || !outcome.RetryAt.IsZero() || outcome.UnevaluatedAttempts != 1 ||
				outcome.UnevaluatedRetryAt.Before(before.Add(24*time.Hour)) || outcome.UnevaluatedRetryAt.After(time.Now().Add(24*time.Hour)) {
				t.Fatalf("unevaluated paid attempt did not receive its separate retry: %+v", outcome)
			}
		})
	}
}

func TestExpansionCandidateLotDoesNotScaleWithEuropeanFleet(t *testing.T) {
	h, runner, yard, _, _ := overseasCapacityFixture(t)
	partner, _ := findPort(runner.stateCache.Catalog, "new_york")
	_, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	input.SurplusPence, input.LotCashPence, input.FleetSize = 100_000_000, 1_000, 1
	first, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, time.Now().UTC())
	if err != nil || !first.Accepted || first.ExpansionEstimate == nil {
		t.Fatalf("overseas working-cash control: %+v %v", first, err)
	}
	input.FleetSize = 6
	larger, _, _, err := runner.expansionBuildEstimate(h.ctx, yard, partner, input, time.Now().UTC())
	if err != nil || !larger.Accepted || larger.QuotedHullPence != first.QuotedHullPence ||
		larger.WorkingCapitalPence-first.WorkingCapitalPence != 5*input.LotCashPence {
		t.Fatalf("overseas gossip lot was multiplied by European fleet size: small=%+v larger=%+v err=%v", first, larger, err)
	}
	europe := capacityGrowthAcceptanceInput()
	decision := EvaluateCapacityGrowth(europe)
	want := max(europe.ReservePence, ceilBPS(europe.HullPricePence, 1_500)) + int64(europe.FleetSize+1)*europe.LotCashPence
	if !decision.Accepted || decision.WorkingCapitalPence != want {
		t.Fatalf("ordinary European capacity buffer changed: %+v want=%dp", decision, want)
	}
}

func TestExpansionRatedHullObservationFailureIsTransientAndBackedOff(t *testing.T) {
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage initial pair: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	for i := range runner.stateCache.Catalog.ShipClasses {
		if runner.stateCache.Catalog.ShipClasses[i].ID == recoveryHull {
			runner.stateCache.Catalog.ShipClasses[i].HullCategory = "coastal"
		}
	}
	hull.ID, hull.HullCategory = "cutter", "regional"
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
	for key, quote := range runner.stateCache.Quotes {
		if quote.ShipClass != recoveryHull {
			continue
		}
		quote.ShipClass = hull.ID
		ratedKey := quote.PortID + "|" + hull.ID
		runner.stateCache.Quotes[ratedKey] = quote
		runner.meta["quotes/"+ratedKey] = runner.meta["quotes/"+key]
	}
	base := h.api.server.Config.Handler
	var requests atomic.Int64
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/world/route" && req.URL.Query().Get("ship_class") == hull.ID {
			requests.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		base.ServeHTTP(w, req)
	})
	err := runner.newActionPreflight(h.ctx, mission, &data)
	var definitive *expansionAdmissionError
	if err == nil || errors.As(err, &definitive) || strings.Contains(err.Error(), "hull rating") {
		t.Fatalf("unrated cheap hull masked the rated hull's transient observation failure: %v", err)
	}
	firstRequests := requests.Load()
	if firstRequests < 2 {
		t.Fatalf("rated pair route requests=%d; want an attempt for both directions", firstRequests)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("transient rated observation dispatch guard: %t %v", changed, err)
	}
	if requests.Load() != firstRequests || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("failed public routes were fetched again immediately or sent a paid hire")
	}
	agents := persistedExpansionAgents(t, h)
	if len(agents) != 2 || !slices.ContainsFunc(agents, func(agent expansionAgent) bool { return agent.PortID == "yard-a" && agent.Partner == "demand" }) {
		t.Fatalf("transient evidence failure erased the selected pair: %+v", agents)
	}
	if outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["caribbean"]; outcome.Losses != 0 || !outcome.RetryAt.IsZero() {
		t.Fatalf("missing observations counted as a regional loss: %+v", outcome)
	}
	var persisted map[string]DatasetMeta
	if err := json.Unmarshal(runner.snapshot["_meta"], &persisted); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"yard-a", "demand"}, {"demand", "yard-a"}} {
		meta := persisted["routes/"+routeKey(pair[0], pair[1], hull.ID)]
		if !meta.Stale || meta.AttemptedAt.IsZero() {
			t.Fatalf("failed route retry clock was not durable: %+v", meta)
		}
	}
}
