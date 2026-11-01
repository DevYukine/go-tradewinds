package engine

import (
	"testing"
	"time"
)

// restartedExpansionRunner simulates a process restart: a fresh runner reloads
// its durable snapshot while inheriting the observed world of the old one.
func restartedExpansionRunner(t *testing.T, h *recoveryHarness, runner *Runner) *Runner {
	t.Helper()
	restarted := h.runner(primaryTestKey)
	restarted.stateCache, restarted.meta, restarted.generation = runner.stateCache, runner.meta, runner.generation
	return restarted
}

func persistedExpansionAgents(t *testing.T, h *recoveryHarness) []expansionAgent {
	t.Helper()
	snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	return decode[[]expansionAgent](snapshot, "_expansion_agents")
}

func confirmedExpansionHire(t *testing.T, h *recoveryHarness, portID string) time.Time {
	t.Helper()
	for _, agent := range persistedExpansionAgents(t, h) {
		if agent.PortID == portID {
			return agent.HiredAt
		}
	}
	t.Fatalf("no expansion agent record for %s", portID)
	return time.Time{}
}

// SEC-01: a hire whose request stays unresolved for hours starts its
// observation floor at the confirmed receipt, and that confirmed time
// survives later restarts instead of being re-derived from the staged action.
func TestOverseasHireFloorStartsAtConfirmedReceiptAcrossRestart(t *testing.T) {
	t.Parallel()
	h, runner, _ := overseasPairFixture(t)
	if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
		t.Fatalf("stage the yard hire: %t %v", changed, err)
	}
	_, data := h.mission("__treasury")
	if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "boston") {
		t.Fatalf("yard hire was not staged for a delayed receipt: %+v", data.Action)
	}
	// The staged request replays hours later on a restarted runner.
	stagedAt := time.Now().UTC()
	data.Action.CreatedAt = stagedAt.Add(-3 * time.Hour)
	restarted := restartedExpansionRunner(t, h, runner)
	before := time.Now().UTC()
	if err := restarted.recordExpansionAgent(h.ctx, &data); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	confirmedAt := confirmedExpansionHire(t, h, "boston")
	if confirmedAt.Before(before) || confirmedAt.After(after) {
		t.Fatalf("delayed receipt counted the staged request as the hire: staged=%s confirmed=%s window=[%s,%s]",
			data.Action.CreatedAt, confirmedAt, before, after)
	}
	for _, agent := range persistedExpansionAgents(t, h) {
		if agent.PortID == "new_york" && !agent.HiredAt.IsZero() {
			t.Fatalf("partner was confirmed without its own receipt: %+v", agent)
		}
	}
	// A second restart reads the persisted confirmation, not the staged time.
	again := restartedExpansionRunner(t, h, runner)
	for _, agent := range decode[[]expansionAgent](again.snapshot, "_expansion_agents") {
		if agent.PortID == "boston" && !agent.HiredAt.Equal(confirmedAt) {
			t.Fatalf("restart moved the confirmed hire time: first=%s restarted=%s", confirmedAt, agent.HiredAt)
		}
	}
	if startedAt := decode[time.Time](again.snapshot, "_expansion_last_started_at"); !startedAt.Equal(confirmedAt) {
		t.Fatalf("a delayed first paid hire did not advance the durable global start: confirmed=%s start=%s", confirmedAt, startedAt)
	}
	// The confirmed floor is still open, so the partner hire resumes instead
	// of the pair being treated as expired because of the old staged time.
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
		t.Fatal(err)
	}
	if changed, err := again.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("a three-hour staged receipt exhausted the confirmed floor across restart: %t %v", changed, err)
	}
	_, partner := h.mission("__treasury")
	if partner.Action == nil || partner.Action.Stage != "hire-factor" || !stringsContainPort(partner.Action.Body, "new_york") {
		t.Fatalf("confirmed floor did not resume the partner hire: %+v", partner.Action)
	}
}

// rev-F10/arch-F7: a hired yard whose reciprocal mate record was deleted by a
// refused or preflight-failed partner hire is solo and holds no growth.
func TestOverseasHiredYardWithoutReciprocalMateDoesNotHoldGrowth(t *testing.T) {
	t.Parallel()
	h, runner, _ := overseasPairFixture(t)
	yard, _ := findPort(runner.stateCache.Catalog, "boston")
	hiredAt := time.Now().UTC().Add(-30 * time.Minute)
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	// The partner hire was refused and cleared only its own record.
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, CreatedAt: hiredAt, HiredAt: hiredAt},
	})
	if runner.expansionAwaitingEvaluation() {
		t.Fatal("a hired yard whose reciprocal mate is missing held every growth decision")
	}
	if handled, _, err := runner.holdExpansionGrowth(h.ctx, false, false); err != nil || handled || len(runner.holding) != 0 {
		t.Fatalf("dangling mate held growth: handled=%t holding=%v err=%v", handled, runner.holding, err)
	}
	// Control: the complete un-evaluated pair inside its floor still holds.
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, CreatedAt: hiredAt, HiredAt: hiredAt},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: hiredAt, HiredAt: hiredAt},
	})
	if !runner.expansionAwaitingEvaluation() {
		t.Fatal("complete pair without a fresh evaluation released growth inside its floor")
	}
}

// arch-F6: transient clock or stale-authority failures of a first hire send
// keep the awaiting pair records for an in-floor retry instead of latching the
// 24-hour region retry that only definitive admission rejections may apply.
func TestOverseasTransientHirePreflightKeepsAwaitingPairInsideFloor(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"stale clock authority", "stale ships authority"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			if changed, err := runner.considerExpansionAgent(h.ctx); err != nil || !changed {
				t.Fatalf("stage the yard hire: %t %v", changed, err)
			}
			mission, data := h.mission("__treasury")
			dataset := "clock"
			if mode == "stale ships authority" {
				dataset = "ships"
			}
			meta := runner.meta[dataset]
			meta.Stale = true
			runner.meta[dataset] = meta
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("transient %s first-send failure: changed=%t err=%v", mode, changed, err)
			}
			if h.api.mutationCount("/v1/company/hire-factor") != 0 {
				t.Fatal("transient preflight reached the factor API")
			}
			agents := decode[[]expansionAgent](runner.snapshot, "_expansion_agents")
			if len(agents) != 2 {
				t.Fatalf("transient %s failure discarded the awaiting pair: %+v", mode, agents)
			}
			for _, agent := range agents {
				if agent.Partner == "" || !agent.HiredAt.IsZero() {
					t.Fatalf("awaiting pair record lost its mate or hire state: %+v", agent)
				}
			}
			if retries := decode[map[string]time.Time](runner.snapshot, "_expansion_agent_retries"); !retries["america"].IsZero() {
				t.Fatalf("transient %s failure latched the 24-hour region retry: %+v", mode, retries)
			}
			if outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]; outcome.Losses != 0 || !outcome.RetryAt.IsZero() {
				t.Fatalf("transient %s failure recorded a regional loss: %+v", mode, outcome)
			}
			meta.Stale = false
			runner.meta[dataset] = meta
			// Released intents retry after their short delay, still inside the floor.
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || changed {
				t.Fatalf("transient %s failure bypassed its retry deadline: %t %v", mode, changed, err)
			}
			for i := range agents {
				agents[i].RetryAt = time.Now().UTC().Add(-time.Second)
			}
			overseasPairSave(t, h, runner, agents)
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("awaiting pair did not retry its hire inside the floor after %s: %t %v", mode, changed, err)
			}
			_, data = h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "boston") {
				t.Fatalf("transient failure did not restage the pending yard hire: %+v", data.Action)
			}
		})
	}
}

// rev-F5/arch-F9: a capacity_candidate row that carries a quote error or no
// route-backed evaluated plan is not a fresh pair evaluation, so the hold
// stays and no explicit evaluation state is recorded.
func TestOverseasQuoteErrorOrRoutelessYardVerdictIsNotAFreshEvaluation(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"quote error", "route missing"} {
		t.Run(mode, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			yard, _ := findPort(runner.stateCache.Catalog, "boston")
			pairTime := time.Now().UTC().Add(-30 * time.Minute)
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			overseasPairSave(t, h, runner, []expansionAgent{
				{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, CreatedAt: pairTime, HiredAt: pairTime},
				{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: pairTime, HiredAt: pairTime},
			})
			switch mode {
			case "quote error":
				runner.snapshot["_growth_build_backoffs"] = encode(map[string]time.Time{
					yard.ID + "|" + recoveryHull: time.Now().UTC().Add(time.Hour),
				})
			case "route missing":
				for _, destination := range []string{"new_york", yard.ID} {
					delete(runner.stateCache.Routes, routeKey(yard.ID, destination, recoveryHull))
				}
			}
			evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
			if _, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, yard.ID, recoveryHull); err != nil {
				t.Fatal(err)
			}
			overseasCapacityDecision(t, h, yard.ID, recoveryHull)
			if !runner.expansionAwaitingEvaluation() {
				t.Fatalf("%s candidate pass released the pair's fresh-evaluation hold", mode)
			}
			for _, agent := range persistedExpansionAgents(t, h) {
				if !agent.EvaluatedAt.IsZero() {
					t.Fatalf("%s candidate pass recorded explicit pair evaluation: %+v", mode, agent)
				}
			}
		})
	}
}

// rev-F5/F6/arch-F9: a genuine route-backed yard verdict counts as the pair's
// evaluation, is stored explicitly per pair record, and keeps releasing the
// hold after the decision dedup cache is evicted or the process restarts.
func TestOverseasYardVerdictEvaluationSurvivesCacheEvictionAndRestart(t *testing.T) {
	t.Parallel()
	h, runner, _ := overseasPairFixture(t)
	yard, _ := findPort(runner.stateCache.Catalog, "boston")
	pairTime := time.Now().UTC().Add(-30 * time.Minute)
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, CreatedAt: pairTime, HiredAt: pairTime},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: pairTime, HiredAt: pairTime},
	})
	evidence, _ := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if _, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, yard.ID, recoveryHull); err != nil {
		t.Fatal(err)
	}
	if handled, _, err := runner.holdExpansionGrowth(h.ctx, false, false); err != nil || handled {
		t.Fatalf("route-backed yard verdict did not release the pair hold: handled=%t err=%v", handled, err)
	}
	var evaluatedAt time.Time
	for _, agent := range persistedExpansionAgents(t, h) {
		if agent.PortID == yard.ID {
			evaluatedAt = agent.EvaluatedAt
		}
	}
	if evaluatedAt.IsZero() || evaluatedAt.Before(pairTime) {
		t.Fatalf("genuine yard verdict left no durable evaluated-at after the pair time: %s", evaluatedAt)
	}
	// Eviction: the release must not depend on the dedup cache's memory.
	runner.decisionHashes = map[string]decisionFingerprint{}
	if handled, _, err := runner.holdExpansionGrowth(h.ctx, false, false); err != nil || handled {
		t.Fatalf("evaluation state lived only in the decision dedup cache: handled=%t err=%v", handled, err)
	}
	// Restart: a fresh runner reads the durable evaluation from the snapshot.
	restarted := restartedExpansionRunner(t, h, runner)
	if handled, _, err := restarted.holdExpansionGrowth(h.ctx, false, false); err != nil || handled {
		t.Fatalf("restart re-held an evaluated pair from an empty decision cache: handled=%t err=%v", handled, err)
	}
}
