package engine

import (
	"database/sql"
	"fmt"
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
		{"800_plus_300_Cr_now_fits", func(in *MarqueInput) {
			in.ExpiredCostPence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 80_000, 30_000, 45_000
		}, true},
		{"2800_plus_200_Cr_exact_cap", func(in *MarqueInput) {
			in.ExpiredCostPence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 280_000, 20_000, 30_000
		}, true},
		{"one_penny_above_unrecovered_cap", func(in *MarqueInput) {
			in.ExpiredCostPence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 280_001, 20_000, 30_000
		}, false},
		{"prizes_reduce_lifetime_unrecovered", func(in *MarqueInput) {
			in.ExpiredCostPence, in.ExpiredPrizePence, in.Quote.CostPence, in.Quote.ExpectedNetPrizePence = 320_000, 40_000, 20_000, 30_000
		}, true},
		{"exactly_five_percent_liquidity", func(in *MarqueInput) { in.LiquidPence = 200_000 }, true},
		{"one_penny_short_of_liquidity_share", func(in *MarqueInput) { in.LiquidPence = 199_999 }, false},
		{"manual_owner_stop_overrides_returns", func(in *MarqueInput) {
			in.Stopped, in.Quote.ExpectedNetPrizePence = true, 100_000
		}, false},
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

func TestMarqueMoneyManualStopSurvivesRefreshAndRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	newMarqueExecutionAPI(h)
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	runner.snapshot["_marque_stopped"] = encode(true)
	if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
		t.Fatal(err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("manual owner stop allowed new admission: %t %v", changed, err)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !decode[bool](snapshot, "_marque_stopped") || h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
		t.Fatal("refresh or restart cleared the manual stop or spent on a marque")
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
	issued.PrizeEarnedPence = issued.CostPence * 105 / 100
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
	if outcomes[issued.ID].PrizePence != issued.PrizeEarnedPence || outcomes[issued.ID].SeaZoneID != issued.SeaZoneID ||
		!outcomes[issued.ID].ExpiredAt.Equal(issued.ExpiresAt) {
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

func TestMarqueMoneyZoneBackoffProbeAndNegativeCompletionSurviveRestart(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	proxy.clockOffset, proxy.timeScale = 48*time.Hour, 10
	proxy.quote.ExpectedNetPrizePence = 20_000
	now := time.Now().UTC().Add(proxy.clockOffset)
	for range 3 {
		id := uuid.NewString()
		proxy.issued[id] = letterOfMarque{ID: id, SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
			CostPence: 10_000, PrizeEarnedPence: 9_900, CreatedAt: now.Add(-9 * time.Hour),
			ExpiresAt: now.Add(-8 * time.Hour), UpdatedAt: now}
	}
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	until := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]
	serverNow, _, valid := runner.serverBounds()
	duration := gameDuration(7, proxy.timeScale)
	if !valid || until.Before(serverNow.Add(duration-time.Second)) || until.After(serverNow.Add(duration)) {
		t.Fatalf("zone loss backoff is not seven server game days: server=%s until=%s", serverNow, until)
	}
	proxy.mu.Lock()
	proxy.hideMarques = true
	proxy.mu.Unlock()
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if got := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]; !got.Equal(until) {
		t.Fatalf("restart or omitted list moved the backoff: %s -> %s", until, got)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("failed zone reentered during its backoff: %t %v", changed, err)
	}
	data := missionData{Marque: &marqueIntent{Quote: proxy.quote, BankingRegion: "north"}}
	if err := runner.marquePreflight(h.ctx, &data, false); err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("send preflight ignored the zone backoff: %v", err)
	}
	proxy.mu.Lock()
	proxy.clockOffset = until.Add(time.Second).Sub(time.Now().UTC())
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.considerMarque(h.ctx); err != nil || !changed {
		t.Fatalf("elapsed zone backoff did not allow one qualifying probe: %t %v", changed, err)
	}
	refreshMarqueExecution(t, h, runner)
	mission, data := h.mission("__marque")
	if changed, err := runner.driveMarque(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("stage probe: %t %v", changed, err)
	}
	mission, data = h.mission("__marque")
	key := data.Action.ID
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("send probe: %t %v", changed, err)
	}
	refreshMarqueExecution(t, h, runner)
	mission, data = h.mission("__marque")
	if _, err := runner.driveMarque(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("an active probe admitted another issue: %t %v", changed, err)
	}
	proxy.mu.Lock()
	probe := proxy.issued[key]
	probe.PrizeEarnedPence = 9_900
	probe.UpdatedAt = probe.ExpiresAt.Add(time.Second)
	proxy.issued[key] = probe
	proxy.hideMarques = false
	proxy.clockOffset = probe.UpdatedAt.Sub(time.Now().UTC())
	proxy.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	nextUntil := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]
	serverNow, _, valid = runner.serverBounds()
	if !valid || !nextUntil.After(until) || nextUntil.Before(serverNow.Add(duration-time.Second)) || nextUntil.After(serverNow.Add(duration)) {
		t.Fatalf("negative probe did not start a new seven-game-day backoff: old=%s new=%s server=%s", until, nextUntil, serverNow)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("negative probe immediately reissued: %t %v", changed, err)
	}
	runner = h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	if got := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]; !got.Equal(nextUntil) {
		t.Fatalf("negative probe backoff changed after restart: %s -> %s", nextUntil, got)
	}
	if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
		t.Fatalf("restart reopened the negative probe: %t %v", changed, err)
	}
	if decode[bool](runner.snapshot, "_marque_stopped") || h.api.mutationCount("/v1/fleet/issue-marque") != 1 {
		t.Fatal("rolling zone control latched the manual stop or duplicated the paid probe")
	}
}

func TestMarqueMoneyWindowRolloutStartsFreshBackoffWithoutNewExpiry(t *testing.T) {
	for _, retainedLossBasis := range []bool{false, true} {
		t.Run(fmt.Sprintf("retained_loss_basis_%t", retainedLossBasis), func(t *testing.T) {
			h := newRecoveryHarness(t)
			proxy := newMarqueExecutionAPI(h)
			proxy.timeScale = 60
			proxy.quote.ExpectedNetPrizePence = 20_000
			proxy.hideMarques = true
			runner := h.runner(primaryTestKey)
			refreshMarqueExecution(t, h, runner)
			now, _, _ := runner.serverBounds()
			outcomes := make(map[string]marqueOutcome)
			for i := range 3 {
				id := uuid.NewString()
				outcomes[id] = marqueOutcome{ID: id, SeaZoneID: marqueExecutionZone,
					ExpiredAt: now.Add(-gameDuration(float64(i+1), 60)), CostPence: 10_000}
			}
			id := uuid.NewString()
			outcomes[id] = marqueOutcome{ID: id, SeaZoneID: marqueExecutionZone,
				ExpiredAt: now.Add(-gameDuration(31, 60)), CostPence: 10_000, PrizePence: 100_000}
			zones, _ := rollingMarqueReturns(outcomes, now, 60)
			oldUntil := now.Add(-time.Second)
			runner.snapshot["_marque_outcomes"] = encode(outcomes)
			runner.snapshot["_marque_backoffs"] = encode(map[string]time.Time{marqueExecutionZone: oldUntil})
			if retainedLossBasis {
				old := zones[marqueExecutionZone]
				old.PrizePence = old.CostPence - 1
				runner.snapshot["_marque_backoff_basis"] = encode(map[string]marqueBackoffBasis{
					marqueExecutionZone: {Returns: old, Until: oldUntil},
				})
			}
			if err := runner.recordMarques(h.ctx); err != nil {
				t.Fatal(err)
			}
			until := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]
			if until.Before(now.Add(gameDuration(7, 60))) {
				t.Fatalf("rolling-window loss reused an older wait: %s", until)
			}
			if changed, err := runner.considerMarque(h.ctx); err != nil || changed {
				t.Fatalf("window rollout admitted a paid probe before a fresh wait: %t %v", changed, err)
			}
			if h.api.mutationCount("/v1/fleet/issue-marque") != 0 {
				t.Fatal("window rollout sent a new paid issue")
			}
		})
	}
}

func TestMarqueMoneyFinalPrizeRequiresPostExpiryListAndCorrectedReturnReleasesLossBackoff(t *testing.T) {
	h := newRecoveryHarness(t)
	proxy := newMarqueExecutionAPI(h)
	proxy.timeScale = 60
	runner := h.runner(primaryTestKey)
	refreshMarqueExecution(t, h, runner)
	now, _, _ := runner.serverBounds()
	id := uuid.NewString()
	marque := letterOfMarque{ID: id, SeaZoneID: marqueExecutionZone, AddedPiracyBPS: 500,
		CostPence: 10_000, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Second), UpdatedAt: now}
	runner.snapshot["marques"] = encode([]letterOfMarque{marque})
	meta := runner.meta["marques"]
	meta.AttemptedAt = time.Now().Add(-2 * time.Second)
	runner.meta["marques"] = meta
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, exists := decode[map[string]marqueOutcome](runner.snapshot, "_marque_outcomes")[id]; exists {
		t.Fatal("a pre-expiry list was treated as the final prize")
	}
	if _, exists := decode[map[string]letterOfMarque](runner.snapshot, "_marque_active")[id]; !exists {
		t.Fatal("the pending final prize released its active cost")
	}
	marques := []letterOfMarque{marque}
	for range 2 {
		next := marque
		next.ID = uuid.NewString()
		marques = append(marques, next)
	}
	meta.AttemptedAt = time.Now()
	runner.meta["marques"] = meta
	runner.snapshot["marques"] = encode(marques)
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if until := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]; !now.Before(until) {
		t.Fatal("three authoritative losing expiries did not start a wait")
	}
	for i := range marques {
		marques[i].PrizeEarnedPence = marques[i].CostPence
	}
	runner.snapshot["marques"] = encode(marques)
	if err := runner.recordMarques(h.ctx); err != nil {
		t.Fatal(err)
	}
	if until := decode[map[string]time.Time](runner.snapshot, "_marque_backoffs")[marqueExecutionZone]; !until.IsZero() {
		t.Fatal("corrected final prize left a purely loss-driven backoff in place")
	}
}
