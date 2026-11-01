package engine

import (
	"testing"
	"time"
)

func TestExpansionUnevaluatedPaidDismissalsEscalateOncePerAttempt(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	for attempt, delay := range []time.Duration{24 * time.Hour, 48 * time.Hour, 96 * time.Hour, 96 * time.Hour} {
		h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston", "new_york")
		overseasPairSyncFactors(h, runner)
		selectedAt := time.Now().UTC().Add(-4 * time.Hour).Add(time.Duration(attempt) * time.Second)
		hiredAt := selectedAt.Add(time.Hour)
		// Seed the next confirmed paid pair, retaining the prior attempt's
		// outcome exactly as selection does when a regional retry is admitted.
		outcomes := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")
		if outcomes == nil {
			outcomes = make(map[string]expansionOutcome)
		}
		outcome := outcomes["america"]
		outcome.AttemptAt, outcome.Reason = selectedAt, ""
		outcomes["america"] = outcome
		runner.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
		overseasPairSave(t, h, runner, []expansionAgent{
			{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: selectedAt, HiredAt: hiredAt},
			{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: selectedAt, HiredAt: hiredAt},
		})
		before := time.Now().UTC()
		if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
			t.Fatalf("attempt %d first dismissal: %t %v", attempt+1, changed, err)
		}
		snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		outcome = decode[map[string]expansionOutcome](snapshot, "_expansion_agent_outcomes")["america"]
		if outcome.UnevaluatedAttempts != attempt+1 || outcome.Losses != 0 || !outcome.RetryAt.IsZero() ||
			outcome.UnevaluatedRetryAt.Before(before.Add(delay)) || outcome.UnevaluatedRetryAt.After(time.Now().Add(delay)) {
			t.Fatalf("attempt %d did not receive its separate %s retry: %+v", attempt+1, delay, outcome)
		}
		deadline := outcome.UnevaluatedRetryAt
		for member := 0; member < 2; member++ {
			mission, data := h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "dismiss-factor" {
				t.Fatalf("attempt %d member %d was not selected for dismissal: %+v", attempt+1, member+1, data.Action)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("attempt %d member %d dismissal dispatch: %t %v", attempt+1, member+1, changed, err)
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			overseasPairSyncFactors(h, runner)
			if member == 0 {
				if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
					t.Fatalf("attempt %d partner dismissal: %t %v", attempt+1, changed, err)
				}
			}
		}
		outcome = decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
		if outcome.UnevaluatedAttempts != attempt+1 || !outcome.UnevaluatedRetryAt.Equal(deadline) ||
			!runner.expansionRetryUntil("america").Equal(deadline) {
			t.Fatalf("attempt %d counted its second dismissal twice: %+v", attempt+1, outcome)
		}
	}
}

func TestExpansionExpiredUnhiredPartnerCountsOnePaidUnevaluatedAttempt(t *testing.T) {
	h, runner, _ := overseasPairFixture(t)
	h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston")
	overseasPairSyncFactors(h, runner)
	at := time.Now().UTC().Add(-3 * time.Hour)
	overseasPairSave(t, h, runner, []expansionAgent{
		{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: at.Add(-time.Minute), HiredAt: at},
		{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: at.Add(-time.Minute)},
	})
	before := time.Now().UTC()
	if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
		t.Fatalf("expired unpaid partner and paid yard dismissal: %t %v", changed, err)
	}
	agents := persistedExpansionAgents(t, h)
	if len(agents) != 1 || agents[0].PortID != "boston" || agents[0].Partner != "" {
		t.Fatalf("expired unpaid partner was not released: %+v", agents)
	}
	outcome := decode[map[string]expansionOutcome](runner.snapshot, "_expansion_agent_outcomes")["america"]
	if outcome.UnevaluatedAttempts != 1 || outcome.Losses != 0 || !outcome.RetryAt.IsZero() ||
		outcome.UnevaluatedRetryAt.Before(before.Add(24*time.Hour)) || outcome.UnevaluatedRetryAt.After(time.Now().Add(24*time.Hour)) {
		t.Fatalf("partial paid pair was fee-free or counted twice: %+v", outcome)
	}
}
