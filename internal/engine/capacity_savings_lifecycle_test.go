package engine

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacitySavingsUnevaluatedYardPreservesMarker(t *testing.T) {
	t.Parallel()
	for _, observation := range []string{"stale quote", "missing quote", "stale book", "missing book", "non-quay"} {
		t.Run(observation, func(t *testing.T) {
			h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
			if target.HullID != "brigantine" {
				t.Fatalf("positive control did not establish the target: %+v", target)
			}
			target.ExpiresAt = time.Now().UTC().Add(time.Minute)
			runner.snapshot["_capacity_savings_target"] = encode(target)
			quoteKey := recoveryHomePort + "|brigantine"
			switch observation {
			case "stale quote", "stale book":
				key := "yard_quotes/" + quoteKey
				if observation == "stale book" {
					key = "books/" + recoveryHomePort
				}
				meta := runner.meta[key]
				meta.Stale = true
				runner.meta[key] = meta
			case "missing quote":
				delete(runner.stateCache.Quotes, quoteKey)
			case "missing book":
				delete(runner.stateCache.Markets, recoveryHomePort)
				delete(runner.meta, "books/"+recoveryHomePort)
			case "non-quay":
				book := runner.stateCache.Markets[recoveryHomePort]
				book.Visibility = "gossip"
				runner.stateCache.Markets[recoveryHomePort] = book
			}
			for range 2 {
				if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
					t.Fatalf("unevaluated %s spent savings: changed=%t err=%v", observation, changed, err)
				}
				got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
				if got != target {
					t.Fatalf("unevaluated %s changed or extended the marker: got %+v want %+v", observation, got, target)
				}
			}
			h.missingMission("__growth")
			h.missingMission("__treasury")
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 || h.api.mutationCount("/v1/company/hire-factor") != 0 {
				t.Fatal("unevaluated target admitted an optional spend")
			}
		})
	}
}

func TestCapacitySavingsFreshViableTargetWithoutAffordableAlternative(t *testing.T) {
	t.Parallel()
	h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.HullID != "brigantine" {
		t.Fatalf("positive control did not establish the target: %+v", target)
	}
	target.ExpiresAt = time.Now().UTC().Add(time.Minute)
	runner.snapshot["_capacity_savings_target"] = encode(target)
	smallKey := recoveryHomePort + "|" + recoveryHull
	small := runner.stateCache.Quotes[smallKey]
	small.Available = false
	runner.stateCache.Quotes[smallKey] = small
	largeKey := recoveryHomePort + "|brigantine"
	large := runner.stateCache.Quotes[largeKey]
	price := *large.TotalCostPence + 100
	large.TotalCostPence = &price
	runner.stateCache.Quotes[largeKey] = large
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("fresh viable target without affordable A: changed=%t err=%v", changed, err)
	}
	got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if got.PortID != target.PortID || got.HullID != target.HullID || got.CapitalPence <= target.CapitalPence ||
		got.RequiredPence <= target.RequiredPence || got.ExpiresAt.After(target.ExpiresAt) {
		t.Fatalf("fresh viable target was erased, not refreshed, or extended: before %+v after %+v", target, got)
	}
	for _, spend := range []struct {
		name string
		run  func() (bool, error)
	}{
		{"agent", func() (bool, error) { return runner.considerExpansionAgent(h.ctx) }},
		{"underwriting", func() (bool, error) { return runner.considerUnderwriting(h.ctx) }},
		{"marque", func() (bool, error) { return runner.considerMarque(h.ctx) }},
	} {
		if changed, err := spend.run(); err != nil || changed {
			t.Fatalf("fresh viable target admitted optional %s spend: changed=%t err=%v", spend.name, changed, err)
		}
	}
	for _, id := range []string{"__growth", "__treasury", "__underwriting", "__marque"} {
		h.missingMission(id)
	}
}

func TestCapacitySavingsRepeatedFreshWinsNeverExtendExpiry(t *testing.T) {
	t.Parallel()
	h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.HullID != "brigantine" {
		t.Fatalf("positive control did not establish the target: %+v", target)
	}
	// A later quote raises the wait without changing the saved port/hull.
	key := recoveryHomePort + "|brigantine"
	quote := runner.stateCache.Quotes[key]
	price := *quote.TotalCostPence + 100
	quote.TotalCostPence = &price
	runner.stateCache.Quotes[key] = quote
	target.ExpiresAt = time.Now().UTC().Add(30 * time.Second)
	runner.snapshot["_capacity_savings_target"] = encode(target)
	runner.snapshot["_capacity_financing_backoffs"] = encode(map[string]time.Time{key: time.Now().UTC().Add(time.Hour)})
	for range 3 {
		if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
			t.Fatalf("fresh winning comparison spent savings: changed=%t err=%v", changed, err)
		}
		got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
		if got.HullID != target.HullID || got.PortID != target.PortID || got.ExpiresAt.After(target.ExpiresAt) {
			t.Fatalf("same target's fresh win extended expiry: before %+v after %+v", target, got)
		}
		target = got
	}
	h.missingMission("__growth")
}

func TestCapacitySavingsStoredComparisonHonorsCurrentWindow(t *testing.T) {
	t.Parallel()
	h, runner, buy, _ := capacitySavingsRunnerFixture(t, true)
	// Keep the affordable alternative's return positive but slow enough that
	// the unbounded savings equation still wins beyond the history window.
	buy.IncrementalNetPencePerGameDay = 1
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now().UTC())
	if err != nil || rate <= 0 {
		t.Fatalf("positive savings rate: %v, %v", rate, err)
	}
	window := capacityHistoryWindow.Seconds() * float64(runner.stateCache.Clock.Calendar.TimeScale) / 86_400
	target := capacitySavingsTarget{
		PortID: recoveryFarPort, HullID: "collier", CapitalPence: 200_000,
		RequiredPence: int64(math.Ceil(rate * (window + 1))), DepthNetPencePerGameDay: 1e15,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if _, margin, wins := capacitySavingsMargin(buy, GrowthDecision{
		QuotedHullPence: target.CapitalPence, CashShortfallPence: target.RequiredPence,
		DepthNetPencePerGameDay: target.DepthNetPencePerGameDay,
	}, rate, 0); !wins {
		t.Fatalf("window test must otherwise prefer saving: margin=%v", margin)
	}
	for _, affordable := range []bool{true, false} {
		buy.Accepted = affordable
		if wait, err := runner.capacitySavingsWait(h.ctx, target, buy, 0, rate, 0); err != nil || wait {
			t.Fatalf("stored target waited beyond current %.0f-game-day window with affordable A=%t: wait=%t err=%v", window, affordable, wait, err)
		}
	}
}

func TestCapacitySavingsStoredComparisonPreservesDepthPreference(t *testing.T) {
	t.Parallel()
	h, runner, buy, _ := capacitySavingsRunnerFixture(t, true)
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now().UTC())
	if err != nil || rate <= 0 || buy.IncrementalNetPencePerGameDay <= 0 {
		t.Fatalf("positive comparison economics: buy %+v rate=%v err=%v", buy, rate, err)
	}
	buy.Competition.PreferenceShips = 1
	for _, preference := range []float64{0, 100} {
		target := capacitySavingsTarget{
			PortID: recoveryFarPort, HullID: "collier", RequiredPence: 1,
			CapitalPence:            growthCashTotal(buy.QuotedHullPence, buy.SlippageReservePence, buy.CannonsPence, buy.InitialStoresPence),
			DepthNetPencePerGameDay: 1.04 * buy.IncrementalNetPencePerGameDay,
			DepthPreferenceShips:    preference, ExpiresAt: time.Now().UTC().Add(time.Hour),
		}
		if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
			t.Fatal(err)
		}
		stored := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
		if stored.DepthPreferenceShips != preference {
			t.Fatalf("marker lost depth preference %v: %+v", preference, stored)
		}
		wait, err := runner.capacitySavingsWait(h.ctx, stored, buy, 0, rate, 0)
		if err != nil || wait != (preference == 0) {
			t.Fatalf("comparable stored depth preference %v: wait=%t err=%v", preference, wait, err)
		}
	}
}

func TestCapacitySavingsLaneComparisonIgnoresCapacityOnlyPause(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"positive lane control", "lane beats marker", "marker beats lane"} {
		t.Run(outcome, func(t *testing.T) {
			h := newRecoveryHarness(t)
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * time.Hour)
			h.api.setFleet([]map[string]any{row})
			h.seedLaneProofs()
			runner := growthWindowRunner(t, h)
			runner.stateCache.Factors = []string{recoveryHomePort}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "lifecycle-lane-income", Kind: "charter", CostsKnown: true,
				QuantityTons: 1, GameSeconds: 3_600, ProceedsPence: 60_000_000,
				CompletedAt: time.Now().UTC().Add(-time.Hour),
			})
			runner.snapshot["_capacity_growth_paused_until"] = encode(time.Now().UTC().Add(time.Hour))
			if outcome != "positive lane control" {
				evidence := growthEvidence{workingByRegion: make(map[string]int64)}
				if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("lane funding: %s, %v", reason, err)
				}
				net := float64(1)
				if outcome == "marker beats lane" {
					net = 100_000_000
				}
				if err := runner.setCapacitySavingsTarget(h.ctx, capacitySavingsTarget{
					PortID: recoveryFarPort, HullID: "collier", RequiredPence: evidence.available + 1_000,
					CapitalPence: 200_000, DepthNetPencePerGameDay: net, ExpiresAt: time.Now().UTC().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			if outcome == "marker beats lane" {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); got.HullID != "collier" {
					t.Fatalf("losing lane erased the marker: %+v", got)
				}
				history, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
				if err != nil {
					t.Fatal(err)
				}
				decisions, ok := history.([]store.Decision)
				if !ok {
					t.Fatalf("unexpected decision history %T", history)
				}
				found := false
				for _, decision := range decisions {
					found = found || decision.Kind == "hold" && decision.Reason == "capacity growth saves for a better hull"
				}
				if !found || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
					t.Fatal("losing lane did not record the exact savings reason without starting capacity A")
				}
				return
			}
			mission, data := h.mission("__growth")
			if data.Capacity != nil || data.GrowthLane == nil || data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("capacity-only pause blocked or replaced the winning lane package: %+v", data)
			}
			if got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); got.PortID != "" {
				t.Fatalf("winning lane retained the live marker: %+v", got)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
				t.Fatalf("winning lane did not dispatch its build: changed=%t err=%v", changed, err)
			}
		})
	}
}

func TestCapacitySavingsBuildReceiptClearsDurableMarker(t *testing.T) {
	t.Parallel()
	h, runner := capacityExecutionFixture(t, false)
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("stage real capacity build: changed=%t err=%v", changed, err)
	}
	mission, data := h.mission("__growth")
	runner.snapshot["_capacity_savings_expired"] = encode(map[string]time.Time{
		recoveryHomePort + "|expired-hull": time.Now().UTC().Add(capacityHistoryWindow),
	})
	if err := runner.setCapacitySavingsTarget(h.ctx, capacitySavingsTarget{
		PortID: recoveryFarPort, HullID: "collier", RequiredPence: 1_000_000,
		CapitalPence: 200_000, DepthNetPencePerGameDay: 1_000_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
		t.Fatalf("consume typed build receipt: changed=%t err=%v", changed, err)
	}
	mission, data = h.mission("__growth")
	if mission.Phase != "growth_refit" || data.BuiltShipID != recoveryBuiltShipID || data.Action != nil || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatalf("real build receipt did not complete its transition: mission %+v data %+v", mission, data)
	}
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target"); got.PortID != "" ||
		len(decode[map[string]time.Time](snapshot, "_capacity_savings_expired")) != 0 {
		t.Fatalf("typed build receipt retained durable savings marker or expired identities: %+v", snapshot)
	}
}

func TestCapacitySavingsNamedBuildReconciliationClearsDurableMarker(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending expiry", "committed uninterpreted", "persisted reconciliation"} {
		t.Run(state, func(t *testing.T) {
			h := newRecoveryHarness(t)
			runner := h.runner(primaryTestKey)
			mission, data := stageLoggingGrowth(t, h, runner)
			action := data.Action
			persisted, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
			})
			if err != nil {
				t.Fatal(err)
			}
			built := berthedLaprasRow()
			actor := built["ship"].(map[string]any)
			actor["id"], actor["name"], actor["ship_class"] = recoveryBuiltShipID, data.BuildName, data.BuildClass
			actor["current_port_id"] = data.BuildPort
			decoy := berthedLaprasRow()
			decoy["ship"].(map[string]any)["name"] = "Charizard"
			h.api.setFleet([]map[string]any{decoy, built})
			refreshLoggingState(t, h, runner)
			runner.snapshot["_capacity_savings_expired"] = encode(map[string]time.Time{
				recoveryHomePort + "|expired-hull": time.Now().UTC().Add(capacityHistoryWindow),
			})
			if err := runner.setCapacitySavingsTarget(h.ctx, capacitySavingsTarget{
				PortID: recoveryFarPort, HullID: "collier", RequiredPence: 1_000_000,
				CapitalPence: 200_000, DepthNetPencePerGameDay: 1_000_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			observed, _ := runner.ship(recoveryBuiltShipID)
			switch state {
			case "committed uninterpreted":
				err = h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil)
			case "persisted reconciliation":
				err = h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]any{"kind": "authoritative_named_build", "ship": observed}))
			}
			if err != nil {
				t.Fatal(err)
			}
			var changed bool
			if state == "pending expiry" {
				changed, err = runner.reconcileExpired(h.ctx, mission, &data, persisted)
			} else {
				changed, err = runner.dispatch(h.ctx, mission, &data)
			}
			if err != nil || !changed {
				t.Fatalf("recover named build: changed=%t err=%v", changed, err)
			}
			mission, data = h.mission("__growth")
			if mission.Phase != "growth_refit" || data.BuiltShipID != recoveryBuiltShipID || data.Action != nil || data.LastActionID != action.ID {
				t.Fatalf("named build was not durably reconciled: mission %+v data %+v", mission, data)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target"); got.PortID != "" ||
				len(decode[map[string]time.Time](snapshot, "_capacity_savings_expired")) != 0 ||
				h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatalf("reconciliation retained savings state or resent construction: %+v", snapshot)
			}
		})
	}
}

func TestCapacitySavingsBuildRefusalDropsOnlyMatchingTarget(t *testing.T) {
	t.Parallel()
	for _, targetKind := range []string{"matching", "different port", "different hull"} {
		t.Run(targetKind, func(t *testing.T) {
			h, runner := capacityExecutionFixture(t, true)
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("stage refused build: changed=%t err=%v", changed, err)
			}
			mission, data := h.mission("__growth")
			target := capacitySavingsTarget{
				PortID: data.BuildPort, HullID: data.BuildClass, RequiredPence: 1_000_000,
				CapitalPence: 200_000, DepthNetPencePerGameDay: 1_000_000, ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			switch targetKind {
			case "different port":
				target.PortID = recoveryFarPort
			case "different hull":
				target.HullID = "collier"
			}
			if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
				t.Fatal(err)
			}
			before := time.Now().UTC()
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("consume definitive refusal: changed=%t err=%v", changed, err)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			got := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target")
			if targetKind == "matching" && got.PortID != "" || targetKind != "matching" && got != target {
				t.Fatalf("refusal cleared the wrong target: kind=%s got %+v want %+v", targetKind, got, target)
			}
			until := decode[map[string]time.Time](snapshot, "_growth_build_backoffs")[data.BuildPort+"|"+data.BuildClass]
			if until.Before(before.Add(6*time.Hour)) || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
				t.Fatalf("refusal did not retain its real build backoff: %s", until)
			}
		})
	}
}
