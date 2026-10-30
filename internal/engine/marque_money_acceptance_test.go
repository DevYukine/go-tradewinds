package engine

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestMarqueMoneyOwnerBoundaries(t *testing.T) {
	base := MarqueInput{Quote: MarqueQuote{SeaZoneID: "the_sound", CostPence: 10_000,
		ExpectedNetPrizePence: 15_000, AddedPiracyBPS: 500, DurationGameHours: 60, LadenPassagesPerDay: 2}, LiquidPence: 50_000_000}
	for _, tc := range []struct {
		name   string
		change func(*MarqueInput)
		want   bool
	}{
		{"exactly_1_5", nil, true},
		{"1_4999_rejected", func(in *MarqueInput) { in.Quote.ExpectedNetPrizePence = 14_999 }, false},
		{"Sound_measured_quote", func(in *MarqueInput) { in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 93_750, 154_426 }, true},
		{"one_active_is_enough_to_stop", func(in *MarqueInput) { in.InForceCostPence = 1 }, false},
		{"800_plus_300_Cr", func(in *MarqueInput) {
			in.LosingCount, in.ExpiredCostPence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 1, 80_000, 30_000, 45_000
		}, false},
		{"800_plus_200_Cr", func(in *MarqueInput) {
			in.LosingCount, in.ExpiredCostPence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 1, 80_000, 20_000, 30_000
		}, true},
		{"prizes_reduce_lifetime_unrecovered", func(in *MarqueInput) {
			in.LosingCount, in.ExpiredCostPence, in.ExpiredPrizePence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 2, 120_000, 40_000, 20_000, 30_000
		}, true},
		// Owner rule: three individually losing marques stop the pilot.
		{"three_expired_at_99_percent_stop", func(in *MarqueInput) { in.LosingCount, in.ExpiredCostPence, in.ExpiredPrizePence = 3, 90_000, 89_100 }, false},
		{"three_expired_at_cost_continue", func(in *MarqueInput) { in.LosingCount, in.ExpiredCostPence, in.ExpiredPrizePence = 0, 90_000, 90_000 }, true},
		{"two_expired_at_99_percent_continue", func(in *MarqueInput) { in.LosingCount, in.ExpiredCostPence, in.ExpiredPrizePence = 2, 90_000, 89_100 }, true},
		{"one_large_loser_among_three_continue", func(in *MarqueInput) { in.LosingCount, in.ExpiredCostPence, in.ExpiredPrizePence = 1, 90_000, 30_000 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.change != nil {
				tc.change(&in)
			}
			got := EvaluateMarque(in)
			if got.Accepted != tc.want || !tc.want && strings.TrimSpace(got.Reason) == "" {
				t.Fatalf("owner admission=%+v, want accepted=%t for %+v", got, tc.want, in)
			}
		})
	}
}

func TestMarqueMoneyDisabledMakesNoQuoteRequests(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	h.marques = false
	runner := h.runner(primaryTestKey)
	for range 2 {
		refreshMarqueExecution(t, h, runner)
	}
	proxy.mu.Lock()
	reads := proxy.quoteReads
	proxy.mu.Unlock()
	if reads != 0 {
		t.Fatalf("MARQUES_ENABLED=false still spent %d quote reads", reads)
	}
}

func TestMarqueMoneyNoSendAboveAdmittedCost(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	proxy.mu.Lock()
	proxy.quote.CostPence++
	proxy.mu.Unlock()
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if got := h.api.mutationCount("/v1/fleet/issue-marque"); got != 0 {
		t.Fatalf("a one-penny increase above the admitted cost sent %d issues", got)
	}
}

func TestMarqueMoneySailingContinuationFetchHoldsAndCountsZone(t *testing.T) {
	for _, next := range []bool{false, true} {
		name := "return"
		if next {
			name = "NextPlan"
		}
		t.Run(name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			proxy.missingReturnRoute = true
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["current_port_id"] = nil
			h.api.setFleet([]map[string]any{row})
			data := missionData{Plan: Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort}, BuyDone: true, CostsKnown: true}
			if next {
				data.FinishAtDestination = true
				data.GrowthLane = &growthLaneIntent{AdmissionID: "money-next", NextPlan: &Plan{Kind: "spot", Origin: recoveryFarPort, Destination: recoveryHomePort}}
			}
			h.putMission(recoveryShipBlastoise, "money-continuation", "sailing", data)
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			proxy.mu.Lock()
			reads := proxy.returnRouteReads
			proxy.mu.Unlock()
			if reads == 0 {
				t.Error("the sailing arrival's uncached committed route was never requested")
			}
			if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
				t.Fatalf("missing committed route admitted risk: changed=%t err=%v", changed, err)
			}
			h.missingMission("__marque")
			proxy.mu.Lock()
			proxy.missingReturnRoute = false
			proxy.mu.Unlock()
			// Put the candidate zone on the return route, without changing its endpoints.
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/v1/world/route" && req.URL.Query().Get("from") == recoveryFarPort && req.URL.Query().Get("to") == recoveryHomePort {
					h.api.writeJSON(w, http.StatusOK, Route{FromPort: recoveryFarPort, ToPort: recoveryHomePort, ShipClass: recoveryHull,
						Legs: []RouteLeg{{FromNode: recoveryFarPort, ToNode: marqueExecutionZone, DistanceNM: 12, EdgeClass: "coastal"}, {FromNode: marqueExecutionZone, ToNode: recoveryHomePort, DistanceNM: 12, EdgeClass: "coastal"}}})
					return
				}
				downstream.ServeHTTP(w, req)
			})
			refreshMarqueExecution(t, h, runner)
			_, zones, err := runner.marqueEvidence(h.ctx)
			if err != nil || !zones[marqueExecutionZone] {
				t.Fatalf("fresh continuation geometry did not protect its zone: %v %v", zones, err)
			}
			if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
				t.Fatalf("our return/NextPlan zone admitted a marque: %t %v", changed, err)
			}
		})
	}
}

func TestMarqueMoneyPendingZoneRequiresFreshMatchingList(t *testing.T) {
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	now := time.Now().UTC()
	issued := letterOfMarque{ID: uuid.NewString(), SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
		CostPence: 10_000, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
	runner.snapshot["_marque_pending"] = encode(marquePending{SeaZoneID: marqueExecutionZone, IssuedID: issued.ID, ExpiresAt: issued.ExpiresAt})
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !runner.planningInput(ShipState{}).MarqueZones[marqueExecutionZone] {
		t.Fatal("a fresh list missing the issued marque retired the pending zone")
	}
	runner.snapshot["marques"] = encode([]letterOfMarque{issued})
	meta := runner.meta["marques"]
	meta.Stale = true
	runner.meta["marques"] = meta
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !runner.planningInput(ShipState{}).MarqueZones[marqueExecutionZone] {
		t.Fatal("stale matching list retired the pending zone")
	}
	meta.Stale, meta.Generation = false, runner.generation
	runner.meta["marques"] = meta
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if pending := decode[marquePending](runner.snapshot, "_marque_pending"); pending.SeaZoneID != "" {
		t.Fatalf("fresh matching list did not retire pending guard: %+v", pending)
	}
}

func TestMarqueMoneyLifecycleReconcileExpiryAndNextAdmission(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	proxy.badReceipt = true
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	key := data.Action.ID
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed {
		t.Fatalf("ambiguous committed issue=%t %v", changed, err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	mission, data = h.mission("__marque")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("REST reconcile=%t %v", changed, err)
	}
	mission, data = h.mission("__marque")
	if data.Action != nil || data.LastActionID != key {
		t.Fatalf("reconcile lost immutable issue: %+v", data)
	}
	if _, err := runner.driveMarque(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("second in-force marque admitted: %t %v", changed, err)
	}
	proxy.mu.Lock()
	issued := proxy.issued[key]
	issued.UpdatedAt = issued.ExpiresAt.Add(time.Hour)
	issued.PrizeEarnedPence = issued.CostPence
	proxy.issued[key] = issued
	proxy.mu.Unlock()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/world/clock" {
			h.api.mu.Lock()
			doc := h.api.clockDocument()
			h.api.mu.Unlock()
			doc["real_utc"] = issued.ExpiresAt.Add(time.Hour)
			h.api.writeJSON(w, http.StatusOK, doc)
			return
		}
		downstream.ServeHTTP(w, req)
	})
	refreshMarqueExecution(t, h, runner)
	outcomes := decode[map[string]marqueOutcome](runner.snapshot, "_marque_outcomes")
	if outcomes[issued.ID].PrizePence != issued.CostPence {
		t.Fatalf("expiry did not permanently record realized prize: %+v", outcomes)
	}
	proxy.mu.Lock()
	proxy.hideMarques = true
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || !changed {
		t.Fatalf("settled expiry did not release next admission: %t %v", changed, err)
	}
	if h.api.mutationCount("/v1/fleet/issue-marque") != 1 {
		t.Fatal("reconcile/expiry duplicated the paid issue")
	}
}

func TestMarqueMoneyExpiredNeverListedIssueReconciles(t *testing.T) {
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	mission, data := stageMarqueExecution(t, h, runner)
	key := data.Action.ID
	// Crash after journaling and before transport: the restarted runner cannot
	// know that this request was never sent. Its full historical list is empty.
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: key, AccountID: h.account.ID,
		Method: data.Action.Method, Path: data.Action.Path, Body: data.Action.Body}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-4 * 24 * time.Hour)
	data.Marque.NotBefore = old
	h.putMission(mission.ShipID, mission.Generation, mission.Phase, data)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	// Like store's insertOldPending, construct an already-aged journal row.
	// Reuse the seeded ciphertext without changing its ID/AAD or bypassing
	// the production immutability trigger.
	result, err := admin.ExecContext(h.ctx, `WITH seeded AS (
		DELETE FROM actions WHERE account_id=$2 AND id=$3 AND state='pending'
		RETURNING id, account_id, company_id, method, path, body_cipher
	)
	INSERT INTO actions (id, account_id, company_id, method, path, body_cipher, state, created_at, updated_at)
	SELECT id, account_id, company_id, method, path, body_cipher, 'pending', $1, $1 FROM seeded`,
		old, h.account.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("seed aged pending action: rows=%d err=%v", count, err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	mission, data = h.mission("__marque")
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("expired never-listed issue remains unresolved: %t %v", changed, err)
	}
	if pending := h.pending(); len(pending) != 0 {
		t.Fatalf("expired issue still blocks later growth mutations: %+v", pending)
	}
	mission, data = h.mission("__marque")
	if data.Action != nil || data.LastActionID != key {
		t.Fatalf("expired recovery discarded its original identity: %+v", data)
	}
	if got := h.api.mutationCount("/v1/fleet/issue-marque"); got != 0 {
		t.Fatalf("expired issue was sent again instead of reconciled: posts=%d", got)
	}
}

func TestMarqueMoneyThreeLosingExpiriesStopAfterRestartAndListOmission(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	now := time.Now().UTC()
	proxy.mu.Lock()
	for range 3 {
		id := uuid.NewString()
		proxy.issued[id] = letterOfMarque{ID: id, SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
			CostPence: 10_000, PrizeEarnedPence: 9_900, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), UpdatedAt: now}
	}
	proxy.mu.Unlock()
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if outcomes := decode[map[string]marqueOutcome](runner.snapshot, "_marque_outcomes"); len(outcomes) != 3 {
		t.Fatalf("expired outcomes were not recorded: %+v", outcomes)
	}
	proxy.mu.Lock()
	proxy.hideMarques = true
	proxy.mu.Unlock()
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("three losing expiries stopped being permanent after restart/list omission: %t %v", changed, err)
	}
	if got := h.api.mutationCount("/v1/fleet/issue-marque"); got != 0 {
		t.Fatalf("permanently stopped account sent %d paid issues", got)
	}
}
