package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestExpansionGossipAdmissionDoesNotBorrowEuropeanEarnings(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	capacity, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" || capacity.MeasuredRatePencePerGameDay != 0 {
		t.Fatalf("idle European rate fixture: %+v %q %v", capacity, reason, err)
	}
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("profitable regional gossip could not stage a hire with European R=0: %t %v", changed, err)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "yard-a") {
		t.Fatalf("gossip estimate did not stage the ranked regional yard: %+v", data.Action)
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range history.([]store.Decision) {
		if row.Kind != "expansion_agent" {
			continue
		}
		var details struct {
			Estimate expansionRateEstimate `json:"estimate"`
		}
		if err := json.Unmarshal(row.Details, &details); err != nil {
			t.Fatal(err)
		}
		estimate := details.Estimate
		if estimate.Basis != "gossip estimate" || estimate.Commodity == "" || estimate.SpreadPence <= 0 ||
			estimate.HaircutBPS != 5_000 || estimate.RoundTripGameDays <= 0 || estimate.RatePencePerGameDay <= 0 {
			t.Fatalf("paid exploration lost its labelled gossip estimate inputs: %s", row.Details)
		}
		if h.api.mutationCount("/v1/company/hire-factor") != 0 {
			t.Fatal("planning bypassed the immutable hire journal")
		}
		return
	}
	t.Fatal("gossip-admitted hire has no persisted expansion estimate")
}

func TestExpansionFeeFreeRejectionFallsThroughToNextRankedYard(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	unsafe := Zone{ID: "test-unsafe-yard", ZoneClass: "regional", BaseStormBPS: 100}
	runner.stateCache.Catalog.SeaZones = append(runner.stateCache.Catalog.SeaZones, unsafe)
	for i := range runner.stateCache.Catalog.Ports {
		if runner.stateCache.Catalog.Ports[i].ID == "yard-a" {
			runner.stateCache.Catalog.Ports[i].SeaZoneID = unsafe.ID
		}
	}
	zones := decode[[]ZoneObservation](runner.snapshot, "zones")
	runner.snapshot["zones"] = encode(append(zones, ZoneObservation{SeaZoneID: unsafe.ID, NetPiracyBPS: 51}))
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("fee-free unsafe best yard starved the safe next yard: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != "yard-b" || data.Action == nil || !stringsContainPort(data.Action.Body, "yard-b") {
		t.Fatalf("the safe lower-ranked yard did not win the same pass: %+v %+v", mission, data.Action)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("the rejected best yard consumed a fee before fallback")
	}
}

func TestExpansionBlockedRegionDoesNotStarveAnotherRegion(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
		Port{ID: "next-yard", BankingRegion: "america", SeaZoneID: "fixture_sea", HasShipyard: true, Phase: 2},
		Port{ID: "next-demand", BankingRegion: "america", SeaZoneID: "fixture_sea", Phase: 2},
	)
	runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{
		AccountID: "test-america-vault", BankingRegion: "america", AccountType: "vault", BalancePence: 500_000,
	})
	ask, bid := int64(100), int64(10_000)
	at := time.Now().UTC().Add(-time.Second)
	detail := runner.stateCache.Ports[recoveryHomePort]
	detail.CoffeeHouseGossip = append(detail.CoffeeHouseGossip,
		Gossip{SubjectPortID: "next-yard", SnapshotAt: at, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: at, BestAskPence: &ask}}},
		Gossip{SubjectPortID: "next-demand", SnapshotAt: at, Commodities: []GossipCommodity{{CommodityID: "x", SnapshotAt: at, BestBidPence: &bid}}},
	)
	runner.stateCache.Ports[recoveryHomePort] = detail
	runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
		"caribbean": {Losses: 3, RetryAt: time.Now().Add(96 * time.Hour), Reason: "repeated regional loss"},
	})
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("blocked top region starved an admissible other region: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != "next-yard" || data.Action == nil || !stringsContainPort(data.Action.Body, "next-yard") {
		t.Fatalf("region backoff did not fall through to the next region: %+v %+v", mission, data.Action)
	}
}

func TestExpansionRetainedWonPairAllowsNextRegionButGlobalStartStillWaits(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newMeasuredExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
		Port{ID: "retained-yard", BankingRegion: "north", SeaZoneID: "zone-north", HasShipyard: true},
		Port{ID: "retained-partner", BankingRegion: "north", SeaZoneID: "zone-north"},
	)
	runner.stateCache.Factors = append(runner.stateCache.Factors, "retained-yard", "retained-partner")
	created, hired := time.Now().UTC().Add(-26*time.Hour), time.Now().UTC().Add(-25*time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: "retained-yard", Partner: "retained-partner", Region: "north", CreatedAt: created, HiredAt: hired, EvaluatedAt: time.Now().UTC()},
		{PortID: "retained-partner", Partner: "retained-yard", Region: "north", CreatedAt: created, HiredAt: hired, EvaluatedAt: time.Now().UTC()},
	})
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("retained won pair indefinitely blocked another region: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	if mission.Destination != "yard-a" || data.Action == nil || data.Action.Stage != "hire-factor" {
		t.Fatalf("next-region exploration was not staged: %+v %+v", mission, data.Action)
	}
	if !decode[time.Time](runner.snapshot, "_expansion_last_started_at").IsZero() {
		t.Fatal("an unpaid staged pair consumed the global paid-start clock")
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("confirm the first paid hire: %t %v", changed, err)
	}
	// Removing the control and awaiting records cannot bypass the durable global minimum.
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{})
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || changed {
		t.Fatalf("a second paid pair started inside 24 real hours: %t %v", changed, err)
	}
	if row := expansionFactorDecision(t, h, "unavailable"); !strings.Contains(row.Reason, "one paid pair per 24") {
		t.Fatalf("global paid-start minimum did not persist its reason: %+v", row)
	}
	h.missingMission("__treasury")
}

func TestExpansionPureCashHirePreflightKeepsPairAndLossSchedule(t *testing.T) {
	t.Parallel()
	h, runner, _ := overseasPairFixture(t)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage the admissible yard hire: %t %v", changed, err)
	}
	mission, data := h.mission("__treasury")
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil {
		t.Fatal(err)
	}
	required, err := runner.expansionRequired(h.ctx, input, "america", 2*expansionAgentHireFeePence)
	if err != nil {
		t.Fatal(err)
	}
	fundedVaults := slices.Clone(runner.stateCache.Vaults)
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.AccountType == "vault" {
			vault.BalancePence = required[vault.BankingRegion]
		}
	}
	if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "cash lot") {
		t.Fatalf("fixture did not reach a pure hull cash shortfall after retaining regional fees: %v", err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("pure cash first-send refusal: %t %v", changed, err)
	}
	if h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("cash shortfall reached the paid hire API")
	}
	if agents := persistedExpansionAgents(t, h); len(agents) != 2 || agents[0].Partner == "" || agents[1].Partner == "" {
		t.Fatalf("cash shortfall discarded the pending pair: %+v", agents)
	}
	if outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]; outcome.Losses != 0 || !outcome.RetryAt.IsZero() {
		t.Fatalf("pure cash shortfall counted as a definitive regional loss: %+v", outcome)
	}
	if retries := decode[map[string]time.Time](runner.snapshot, "_expansion_agent_retries"); !retries["america"].IsZero() {
		t.Fatalf("pure cash shortfall latched a regional retry: %+v", retries)
	}
	runner.stateCache.Vaults = fundedVaults
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
		t.Fatalf("restored cash bypassed the transient hire retry deadline: %t %v", changed, err)
	}
	agents := persistedExpansionAgents(t, h)
	for i := range agents {
		agents[i].RetryAt = time.Now().UTC().Add(-time.Second)
	}
	overseasPairSave(t, h, runner, agents)
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("restored cash could not retry the same pending pair: %t %v", changed, err)
	}
	_, retried := h.mission("__treasury")
	if retried.Action == nil || !stringsContainPort(retried.Action.Body, "boston") {
		t.Fatalf("cash recovery did not restage the original yard hire: %+v", retried.Action)
	}
}

func TestExpansionLossesResetOnlyAfterConfirmedOrReconciledBuild(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"confirmed", "reconciled", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, outcome == "refused")
			retryAt := time.Now().UTC().Add(96 * time.Hour)
			runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
				"north":   {Losses: 3, RetryAt: retryAt, Reason: "old loss"},
				"america": {Losses: 2, RetryAt: retryAt, Reason: "unrelated loss"},
			})
			evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
			if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
				t.Fatalf("stage regional capacity build: %t %v", changed, err)
			}
			mission, data := h.mission("__growth")
			if losses := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["north"].Losses; losses != 3 {
				t.Fatalf("admitting or staging a build reset losses before a hull landed: %d", losses)
			}
			var changed bool
			var err error
			if outcome == "reconciled" {
				built := runner.stateCache.Ships[0]
				built.Ship.ID, built.Ship.Name, built.Ship.ShipClass = recoveryBuiltShipID, data.BuildName, data.BuildClass
				built.Ship.CurrentPortID = data.BuildPort
				runner.stateCache.Ships = append(runner.stateCache.Ships, built)
				changed, err = runner.reconcileCommitted(h.ctx, mission, &data)
			} else {
				changed, err = runner.dispatch(h.ctx, mission, &data)
			}
			if err != nil || !changed {
				t.Fatalf("%s build result: %t %v", outcome, changed, err)
			}
			results := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")
			if want := map[bool]int{true: 3, false: 0}[outcome == "refused"]; results["north"].Losses != want {
				t.Fatalf("%s build reset at the wrong boundary: %+v", outcome, results)
			}
			if results["america"].Losses != 2 || !results["america"].RetryAt.Equal(retryAt) {
				t.Fatalf("a build reset another region's loss schedule: %+v", results)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted := decode[map[string]expansionOutcome](snapshot, "_expansion_agent_outcomes"); persisted["north"].Losses != results["north"].Losses {
				t.Fatalf("%s regional reset was not durable: %+v", outcome, persisted)
			}
		})
	}
}

func TestExpansionQuietDecisionDeduplicatesChurnButNotCandidateChanges(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	details := map[string]any{"region": "america", "port_id": "boston", "partner_port_id": "new_york", "hull_class": "hoy", "needed_cash_pence": int64(1_000_000)}
	for range 3 {
		if err := runner.decision(h.ctx, "", "expansion_agent:america", "capacity growth marginal fleet upkeep consumes the candidate voyage earning rate", details); err != nil {
			t.Fatal(err)
		}
		details["needed_cash_pence"] = details["needed_cash_pence"].(int64) + 1
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil || len(history.([]store.Decision)) != 1 {
		t.Fatalf("quiet expansion detail churn flooded decisions: %v %v", history, err)
	}
	details["hull_class"] = "cutter"
	if err := runner.decision(h.ctx, "", "expansion_agent:america", "capacity growth marginal fleet upkeep consumes the candidate voyage earning rate", details); err != nil {
		t.Fatal(err)
	}
	history, err = h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil || len(history.([]store.Decision)) != 2 {
		t.Fatalf("a changed exploration candidate was suppressed: %v %v", history, err)
	}
	for key, fingerprint := range runner.decisionHashes {
		fingerprint.writtenAt = fingerprint.writtenAt.Add(-decisionClassRefresh)
		runner.decisionHashes[key] = fingerprint
	}
	details["needed_cash_pence"] = int64(2_000_000)
	if err := runner.decision(h.ctx, "", "expansion_agent:america", "capacity growth marginal fleet upkeep consumes the candidate voyage earning rate", details); err != nil {
		t.Fatal(err)
	}
	history, err = h.db.History(h.ctx, h.account.ID, "decisions", store.MaxHistoryLimit)
	if err != nil || len(history.([]store.Decision)) != 3 {
		t.Fatalf("quiet expansion evaluation did not refresh after its long interval: %v %v", history, err)
	}
}

func TestExpansionEquivalentQuoteSelectionIgnoresCaptureCompletionOrder(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	yard, _ := findPort(runner.stateCache.Catalog, "yard-a")
	partner, _ := findPort(runner.stateCache.Catalog, "demand")
	evidence := capacityMeasuredEvidence(t, h, runner, time.Now().UTC())
	capacity, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("capacity quote fixture: %q %v", reason, err)
	}
	var quote YardQuote
	for _, candidate := range runner.stateCache.Quotes {
		if candidate.ShipClass == recoveryHull && candidate.BuildMode == "turnkey_cash" && candidate.Available {
			quote = candidate
			break
		}
	}
	if quote.PortID == "" {
		t.Fatal("fixture has no regional reference quote")
	}
	for _, portID := range []string{"quote-a", "quote-b"} {
		runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, Port{ID: portID, BankingRegion: "north", HasShipyard: true})
		book := runner.stateCache.Markets[recoveryHomePort]
		book.PortID, book.Visibility = portID, "quay"
		runner.stateCache.Markets[portID] = book
		runner.stateCache.Factors = append(runner.stateCache.Factors, portID)
		runner.meta["books/"+portID] = runner.meta["ships"]
	}
	first, second := quote, quote
	first.PortID, second.PortID = "quote-a", "quote-b"
	runner.stateCache.Quotes = map[string]YardQuote{"quote-a|" + recoveryHull: first, "quote-b|" + recoveryHull: second}
	for _, newest := range []string{"quote-a", "quote-b"} {
		for _, portID := range []string{"quote-a", "quote-b"} {
			meta := runner.meta["ships"]
			if portID != newest {
				meta.UpdatedAt = meta.UpdatedAt.Add(-time.Second)
				meta.AttemptedAt = meta.AttemptedAt.Add(-time.Second)
			}
			runner.meta["yard_quotes/"+portID+"|"+recoveryHull] = meta
		}
		decision, selected, fresh, err := runner.expansionBuildEstimate(h.ctx, yard, partner, capacity, evidence.asOf)
		if err != nil || !decision.Accepted || !fresh || selected.PortID != "quote-a" {
			t.Fatalf("capture order changed equivalent economic quote selection (newest=%s): %+v %+v %t %v", newest, decision, selected, fresh, err)
		}
	}
}

func TestExpansionRegionalRetryGateRunsBeforeEconomicQueries(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	newExpansionFactorAPI(h)
	runner := h.runner(primaryTestKey)
	refreshExpansionFactor(t, h, runner)
	until := time.Now().UTC().Add(48 * time.Hour)
	runner.snapshot["_expansion_agent_outcomes"] = encode(map[string]expansionOutcome{
		"caribbean": {Losses: 2, RetryAt: until, Reason: "no profitable executable plan"},
	})
	// The blocked region needs no settlement, valuation, reserve or mission read.
	runner.db = nil
	admission, reason, err := runner.expansionAdmission(h.ctx)
	if err != nil || !strings.Contains(reason, "retry deadline") || admission.region != "caribbean" ||
		!admission.retryUntil.Equal(until) {
		t.Fatalf("region retry was not an early query-free gate: %+v %q %v", admission, reason, err)
	}
}
