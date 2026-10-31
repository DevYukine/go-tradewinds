package engine

// Fix-cycle acceptance contracts for the sourcing review loop against the real
// Runner seams, the fake game API and an isolated PostgreSQL schema.
//
// 1. Diagnostic volume: every equivalent planning pass must write bounded
//    diagnostics — at most three sourcing candidate rows plus exactly one
//    summary row per ship per pass — and rolling timestamps (the plan's
//    ExpectedArrival derives from the server clock upper bound captured by
//    every refresh) must be normalized out of the dedup fingerprint so an
//    unchanged decision is never rewritten. Candidate churn keeps the row
//    growth bounded and retains the newest changed decision.
// 2. Return-store regression: a relocation departure gate must re-verify the
//    freshly re-cost both-way store target (route hazards can raise it after
//    the plan was persisted) and must not depart while physical stores are
//    short; sufficient stores still depart. The staged immutable request must
//    be replayed byte-identically even when the fresh world changed after
//    staging.
// 3. Decision cache cap: direct decision writes must evict so the per-ship
//    fingerprint cache stays bounded at 2048 entries instead of growing with
//    every distinct candidate.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func sourcingReviewShipRow() map[string]any {
	return destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 5, nil, nil)
}

func sourcingReviewCharter(id string) Contract {
	reward := int64(3000)
	expires := time.Now().UTC().Add(30 * 24 * time.Hour)
	return Contract{
		ID:                id,
		ContractType:      "charter",
		Status:            "open",
		OriginPortID:      recoveryFarPort,
		CommodityID:       "sawn_planks",
		RemainingQuantity: 1000,
		RewardPerTonPence: &reward,
		ExpiresAt:         &expires,
	}
}

func sourcingReviewCharters() []Contract {
	ids := []string{"charter-a", "charter-b", "charter-c", "charter-d", "charter-e", "charter-f"}
	charters := make([]Contract, 0, len(ids))
	for _, id := range ids {
		charters = append(charters, sourcingReviewCharter(id))
	}
	return charters
}

// sourcingReviewInject projects one candidate world into the refreshed runner
// state: six live charters whose commodity is manufactured by an injected
// phase-1 source port with fresh public routes in both directions. The fake
// home book's profitable grain opportunity is silenced so the ordinary
// opportunity gate does not preempt sourcing review.
func sourcingReviewInject(t *testing.T, runner *Runner, charters []Contract) {
	t.Helper()
	fresh := DatasetMeta{UpdatedAt: time.Now(), AttemptedAt: time.Now(), Generation: runner.generation, Complete: true}
	runner.stateCache.Contracts = charters
	runner.meta["contracts"] = fresh
	runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports, Port{
		ID:            "amsterdam",
		BankingRegion: "north",
		SeaZoneID:     "zone-north",
		Phase:         1,
		Manufactures:  []string{"sawn_planks"},
	})
	runner.stateCache.Ports["amsterdam"] = PortDetail{
		ID:                   "amsterdam",
		EffectiveBerths:      10,
		BerthDuesPerDayPence: 1,
	}
	runner.meta["ports/amsterdam"] = fresh
	for _, pair := range [][2]string{{"laport", "amsterdam"}, {"amsterdam", "laport"}, {"amsterdam", "portb"}, {"portb", "amsterdam"}} {
		key := routeKey(pair[0], pair[1], recoveryHull)
		runner.stateCache.Routes[key] = Route{
			FromPort:                 pair[0],
			ToPort:                   pair[1],
			ShipClass:                recoveryHull,
			TotalDistanceNM:          24,
			SafeProvisionsTons:       1,
			PortProvisionsTonsPerDay: 0.05,
			Legs: []RouteLeg{{
				FromNode:    pair[0],
				ToNode:      pair[1],
				DistanceNM:  24,
				EdgeClass:   "coastal",
				IsSoundToll: false,
			}},
		}
		runner.meta["routes/"+key] = fresh
	}
	if grain := bookCommodity(runner.stateCache.Markets["laport"], recoveryGrain); grain != nil && grain.Depth != nil {
		grain.Depth.Asks = nil
	}
}

// sourcingReviewPass re-refreshes the world (moving the server clock upper
// bound that rolls ExpectedArrival), re-injects the candidate state and runs
// one diagnostic review pass without ever promoting a durable probe
// (Execute=false).
func sourcingReviewPass(t *testing.T, h *recoveryHarness, runner *Runner, charters []Contract) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh the sourcing review world: %v", err)
	}
	sourcingReviewInject(t, runner, charters)
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("review ship missing from the refreshed fleet")
	}
	if _, _, err := runner.considerSourcing(h.ctx, ship); err != nil {
		t.Fatalf("run sourcing review: %v", err)
	}
}

func sourcingReviewCounts(t *testing.T, h *recoveryHarness) (details, summaries int) {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 500)
	if err != nil {
		t.Fatalf("load decision history: %v", err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history payload %T", rows)
	}
	for _, row := range decisions {
		if row.ShipID != recoveryShipBlastoise {
			continue
		}
		switch {
		case strings.HasPrefix(row.Kind, "sourcing:"):
			details++
		case row.Kind == "sourcing":
			summaries++
		}
	}
	return details, summaries
}

func TestSourcingReviewDiagnosticsStayBoundedAcrossEquivalentPasses(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	runner := h.runner(primaryTestKey)
	// Diagnostics only: the review loop must never promote a durable probe.
	runner.opts.Execute = false
	charters := sourcingReviewCharters()

	sourcingReviewPass(t, h, runner, charters)
	details, summaries := sourcingReviewCounts(t, h)
	if details > 3 {
		t.Fatalf("one review pass wrote %d sourcing candidate rows; at most three detailed rows per pass", details)
	}
	if summaries != 1 {
		t.Fatalf("one review pass wrote %d summary rows; exactly one summary per ship per pass", summaries)
	}

	before := details + summaries
	for range 2 {
		sourcingReviewPass(t, h, runner, charters)
	}
	details, summaries = sourcingReviewCounts(t, h)
	if grown := details + summaries - before; grown != 0 {
		t.Fatalf("equivalent replans with only rolling clock timestamps grew decision rows by %d; unchanged decisions must dedupe without rewriting", grown)
	}

	// Churn a retained, ranked candidate: with six equal-economics charters the
	// displayed top three are charter-a/b/c by the deterministic tiebreak, so a
	// materially changed charter-a (new deadline and reward) must rewrite its
	// own row once its class refresh interval has passed, while every other
	// unchanged candidate stays deduped.
	for key, cached := range runner.decisionHashes {
		cached.writtenAt = cached.writtenAt.Add(-decisionClassRefresh)
		runner.decisionHashes[key] = cached
	}
	churned := sourcingReviewCharter("charter-a")
	churnedDeadline := time.Now().UTC().Add(10 * 24 * time.Hour)
	churned.ExpiresAt = &churnedDeadline
	churnedReward := int64(2500)
	churned.RewardPerTonPence = &churnedReward
	charters[0] = churned
	sourcingReviewPass(t, h, runner, charters)
	details, summaries = sourcingReviewCounts(t, h)
	if grown := details + summaries - before; grown < 1 || grown > 2 {
		t.Fatalf("one genuinely changed candidate grew decision rows by %d; churn must write the changed candidate once within the per-pass bound", grown)
	}
	if len(runner.decisionHashes) > 2048 {
		t.Fatalf("decision fingerprint cache holds %d entries; the cache must stay bounded", len(runner.decisionHashes))
	}
}

func TestDecisionCacheEvictsBeyondCapacity(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	runner := h.runner(primaryTestKey)
	runner.opts.Execute = false
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh the cache-cap world: %v", err)
	}
	for i := range 2060 {
		if err := runner.decision(h.ctx, recoveryShipBlastoise, fmt.Sprintf("sourcing:amsterdam:cache-probe-%d", i), "synthetic churn candidate", nil); err != nil {
			t.Fatalf("record synthetic decision %d: %v", i, err)
		}
	}
	if len(runner.decisionHashes) > 2048 {
		t.Fatalf("decision fingerprint cache grew to %d entries; the cap must evict", len(runner.decisionHashes))
	}
}

// sourcingDepartureSeed persists a relocation mission already staged at its
// departure gate: the persisted plan carries the smaller pre-hazard store
// target (2t) plus the targeted charter id, while the relocation intent, the
// finish-at-destination flag and the expense budget are already durable.
func sourcingDepartureSeed(t *testing.T) string {
	t.Helper()
	startedAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	return fmt.Sprintf(`{
		"plan":{"kind":"relocation","ship_id":%q,"origin":"laport","destination":"portb","commodity":"sawn_planks","quantity":1000,"contract_id":"charter-x","target_stores_tons":2},
		"proven_lane":"portb|laport|sawn_planks|charter",
		"started_at":%q,"started_game_hours":2398,
		"initial_ledger_ids":{},"eligible":{},"rejected_charter_ids":{},
		"finish_at_destination":true,"costs_known":true,
		"relocation":{
			"target_contract_id":"charter-x","target_commodity":"sawn_planks","target_destination":"laport",
			"expense_cap_pence":20000,"modeled_expense_pence":1500,"funding_required_pence":0
		}
	}`, recoveryShipBlastoise, startedAt)
}

// sourcingReviewServeHazardRoute raises every served route's safe-store
// requirement above the persisted plan target and above the ship's physical
// stores, without touching any other endpoint.
func sourcingReviewServeHazardRoute(t *testing.T, h *recoveryHarness) {
	t.Helper()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/world/route" {
			from := request.URL.Query().Get("from")
			to := request.URL.Query().Get("to")
			h.api.writeJSON(writer, http.StatusOK, map[string]any{
				"from_port":                    from,
				"to_port":                      to,
				"ship_class":                   recoveryHull,
				"total_distance_nm":            24,
				"expected_provisions_tons":     0.2,
				"safe_provisions_tons":         3,
				"port_provisions_tons_per_day": 0.05,
				"legs": []any{map[string]any{
					"from_node": from, "to_node": to, "distance_nm": 24,
					"edge_class": "coastal", "is_sound_toll": false,
				}},
			})
			return
		}
		downstream.ServeHTTP(writer, request)
	})
}

// sourcingReviewTargetCharter is the live charter the seeded relocation
// targets. Fresh relocation admission re-costs the plan against the charter,
// so the fake world must serve it even though the fake API lists no contracts.
// Delivery is the home port laport, matching the seed's relocation intent: the
// laden leg sails source portb back to laport, since a portb->portb self-route
// never exists in the route dataset.
func sourcingReviewTargetCharter() Contract {
	reward := int64(3000)
	return Contract{
		ID:                "charter-x",
		ContractType:      "charter",
		Status:            "open",
		OriginPortID:      recoveryHomePort,
		CommodityID:       "sawn_planks",
		RemainingQuantity: 1000,
		RewardPerTonPence: &reward,
	}
}

// sourcingPutDepartureMission persists the raw departure-gate seed with the
// mission columns the runner's saveMission convention maintains. The
// Destination column is a prerequisite of the departure route gate
// (mission.Destination forms the laport|portb|hoy route key); a raw PutMission
// without it strands the mission on a route hold before any store check.
func sourcingPutDepartureMission(t *testing.T, h *recoveryHarness, shipID, generation, phase, data string) {
	t.Helper()
	if err := h.db.PutMission(h.ctx, &store.Mission{
		AccountID:   h.account.ID,
		ShipID:      shipID,
		Phase:       phase,
		Generation:  generation,
		Destination: "portb",
		Commodity:   "sawn_planks",
		Quantity:    1000,
		Data:        json.RawMessage(data),
	}); err != nil {
		t.Fatalf("persist seeded departure mission %s: %v", shipID, err)
	}
}

// sourcingDepartureTick mirrors the harness tick but projects the live target
// charter (and its fresh contracts metadata) into the refreshed state so the
// relocation departure gate's fresh re-cost can admit the mission. The
// generation change matches h.tick: refresh stamps every refreshed dataset's
// metadata with the new generation, so routes and zones stay fresh for work.
func sourcingDepartureTick(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatalf("refresh the relocation departure world: %v", err)
	}
	if err := runner.verifyIdentity(); err != nil {
		t.Fatalf("verify identity for the relocation departure world: %v", err)
	}
	runner.stateCache.Contracts = []Contract{sourcingReviewTargetCharter()}
	runner.meta["contracts"] = DatasetMeta{UpdatedAt: time.Now(), AttemptedAt: time.Now(), Generation: runner.generation, Complete: true}
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatalf("relocation departure work tick: %v", err)
	}
}

// sourcingReviewDecisionReasons returns the ship's recorded decision rows for
// failure diagnostics.
func sourcingReviewDecisionReasons(t *testing.T, h *recoveryHarness) string {
	t.Helper()
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 50)
	if err != nil {
		return fmt.Sprintf("decision history unavailable: %v", err)
	}
	decisions, ok := rows.([]store.Decision)
	if !ok {
		return fmt.Sprintf("unexpected decision payload %T", rows)
	}
	reasons := make([]string, 0, len(decisions))
	for _, row := range decisions {
		if row.ShipID == recoveryShipBlastoise {
			reasons = append(reasons, row.Kind+": "+row.Reason)
		}
	}
	return strings.Join(reasons, "; ")
}

func TestRelocationDepartureWaitsForFreshlyRequiredStores(t *testing.T) {
	t.Parallel()
	t.Run("hazard raises the fresh target above physical stores", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
		sourcingPutDepartureMission(t, h, recoveryShipBlastoise, "rel-depart-hazard", "depart", sourcingDepartureSeed(t))
		sourcingReviewServeHazardRoute(t, h)
		runner := h.runner(primaryTestKey)
		for range 2 {
			sourcingDepartureTick(t, h, runner)
		}
		if departs := h.api.mutationCount("/v1/fleet/depart"); departs != 0 {
			t.Fatalf("relocation departed %d time(s) while freshly required both-way stores exceed the ship's physical stores; decisions: %s", departs, sourcingReviewDecisionReasons(t, h))
		}
	})

	t.Run("stores above the fresh target still depart", func(t *testing.T) {
		h := newRecoveryHarness(t)
		h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
		sourcingPutDepartureMission(t, h, recoveryShipBlastoise, "rel-depart-clear", "depart", sourcingDepartureSeed(t))
		runner := h.runner(primaryTestKey)
		for range 2 {
			sourcingDepartureTick(t, h, runner)
		}
		if departs := h.api.mutationCount("/v1/fleet/depart"); departs < 1 {
			t.Fatalf("relocation with stores above the freshly required target never departed; decisions: %s", sourcingReviewDecisionReasons(t, h))
		}
	})
}
