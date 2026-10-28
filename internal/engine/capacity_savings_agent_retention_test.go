package engine

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestCapacitySavingsRetainsAgedAgentOnlyPresence(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"without marker", "live marker", "expired marker"} {
		t.Run(scenario, func(t *testing.T) {
			h := newRecoveryHarness(t)
			newMeasuredExpansionFactorAPI(h)
			runner := h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
				t.Fatalf("hire staging: changed=%t err=%v", changed, err)
			}
			mission, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "hire-factor" || mission.Destination != "yard-a" {
				t.Fatalf("fixture did not stage a real yard agent: %+v %+v", mission, data.Action)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("hire dispatch: changed=%t err=%v", changed, err)
			}
			if h.api.mutationCount("/v1/company/hire-factor") != 1 {
				t.Fatal("fixture must actually hire its expansion agent")
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 1 || agents[0].PortID != "yard-a" {
				t.Fatalf("hire did not persist the yard agent: %+v", agents)
			}
			agents[0].HiredAt = time.Now().UTC().Add(-3 * time.Hour)
			runner.snapshot["_expansion_agents"] = encode(agents)
			if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
				t.Fatal(err)
			}
			runner = h.runner(primaryTestKey)
			refreshExpansionFactor(t, h, runner)
			agents = decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 1 || !agents[0].HiredAt.Add(2*time.Hour).Before(time.Now().UTC()) ||
				!slices.Contains(runner.stateCache.Factors, "yard-a") || !runner.fresh("factors") {
				t.Fatalf("fixture needs an aged, currently present agent after restart: %+v", agents)
			}
			for _, ship := range runner.stateCache.Ships {
				if ship.Ship.CurrentPortID == "yard-a" || len(ship.RouteLegs) > 0 && ship.RouteLegs[len(ship.RouteLegs)-1].ToNode == "yard-a" {
					t.Fatalf("ship presence or arrival would explain yard retention: %+v", ship)
				}
			}
			missions, err := h.db.Missions(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(missions) != 0 || len(h.pending()) != 0 {
				t.Fatalf("another mission must not explain yard retention: %+v", missions)
			}
			port, found := findPort(runner.stateCache.Catalog, "yard-a")
			if !found {
				t.Fatal("agent yard is missing from the catalog")
			}
			hull, found := findHull(runner.stateCache.Catalog, recoveryHull)
			if !found {
				t.Fatal("savings hull is missing from the catalog")
			}
			quote, err := runner.growthQuote(port, hull)
			if err != nil {
				t.Fatalf("agent-only yard must have a fresh valid executable quote: %v", err)
			}
			target := capacitySavingsTarget{
				PortID: port.ID, HullID: hull.ID, CapitalPence: *quote.TotalCostPence,
				RequiredPence: 1_000_000, DepthNetPencePerGameDay: 10_000,
				ExpiresAt: time.Now().UTC().Add(time.Hour),
			}
			if scenario == "expired marker" {
				target.ExpiresAt = time.Now().UTC().Add(-time.Second)
			}
			if scenario != "without marker" {
				if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
					t.Fatal(err)
				}
			}
			if got := decode[string](runner.snapshot, "_capacity_growth_port"); got != "" {
				t.Fatalf("fixture must not publish an accepted build port: %q", got)
			}
			changed, err := runner.manageExpansionAgents(h.ctx)
			if err != nil || changed == (scenario == "live marker") {
				t.Fatalf("aged agent management: changed=%t err=%v", changed, err)
			}
			persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range []map[string]json.RawMessage{runner.snapshot, persisted} {
				if got := decode[string](snapshot, "_capacity_growth_port"); got != "" {
					t.Fatalf("agent management published an unstaged build port: %q", got)
				}
				got := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target")
				if scenario == "live marker" && got != target {
					t.Fatalf("live savings marker must remain durable and unchanged: got %+v want %+v", got, target)
				}
				if scenario != "live marker" && got.PortID != "" {
					t.Fatalf("ordinary or expired control retained a live marker: %+v", got)
				}
			}
			if h.api.mutationCount("/v1/company/dismiss-factor") != 0 {
				t.Fatal("agent management sent a dismissal before dispatch")
			}
			if scenario == "live marker" {
				h.missingMission("__treasury")
				if len(h.pending()) != 0 {
					t.Fatal("live savings marker journaled a dismissal")
				}
				return
			}
			mission, data = h.mission("__treasury")
			if mission.Destination != port.ID || data.Action == nil || data.Action.Stage != "dismiss-factor" {
				t.Fatalf("control did not journal genuine yard dismissal: %+v %+v", mission, data.Action)
			}
			key := data.Action.ID
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("dismiss dispatch: changed=%t err=%v", changed, err)
			}
			calls := h.api.mutationsFor("/v1/company/dismiss-factor")
			if len(calls) != 1 || calls[0].IdempotencyKey != key {
				t.Fatalf("control did not send exactly its durable dismissal: %+v", calls)
			}
			mission, data = h.mission("__treasury")
			if mission.Phase != "done" || data.Action != nil {
				t.Fatalf("control dismissal did not finish: %+v %+v", mission, data.Action)
			}
			refreshExpansionFactor(t, h, runner)
			if slices.Contains(runner.stateCache.Factors, port.ID) {
				t.Fatal("ordinary or expired control left its dismissed factor at the yard")
			}
		})
	}
}
