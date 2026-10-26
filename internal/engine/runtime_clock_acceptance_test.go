package engine

// Acceptance tests for plan R1: server timestamps and host time stay in their
// own domains. Unlock and expiry decisions must project server time from the
// latest clock observation (serverBounds), never read a server deadline with
// the host clock, and never stretch real elapsed time by the game time scale.
// These tests are written before the implementation; serverBounds is the
// agreed interface from the architecture review (agent://RuntimePlanChallenge
// R1.clock-api) and intentionally does not exist yet.

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// hostServerSkew is the deterministic clock disagreement used across the
// acceptance scenarios. It is far larger than any scheduling jitter in the
// harness, so every expectation below is exact rather than timing based.
const hostServerSkew = 34 * time.Second

func coherentClockMeta(hostNow time.Time, generation string) DatasetMeta {
	return DatasetMeta{
		UpdatedAt:   hostNow,
		AttemptedAt: hostNow,
		Generation:  generation,
		Complete:    true,
	}
}

func TestServerBoundsProjectConservativeServerInterval(t *testing.T) {
	t.Parallel()
	const generation = "clock-generation"
	hostNow := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

	newClock := func(realUTC time.Time) Clock {
		clock := Clock{RealUTC: realUTC, UnlockedPhase: 1}
		// A large time scale proves the projection never multiplies real
		// elapsed host time by the game time scale.
		clock.Calendar.TimeScale = 3600
		clock.Calendar.Month = 5
		clock.Calendar.Day = 10
		return clock
	}

	t.Run("observed request envelope brackets the server interval", func(t *testing.T) {
		cases := []struct {
			name      string
			updated   time.Duration
			attempted time.Duration
			realUTC   time.Time
			wantLower time.Duration
			wantUpper time.Duration
		}{
			{
				name:    "simultaneous capture pins one instant",
				realUTC: hostNow.Add(-hostServerSkew),
			},
			{
				name:      "response latency opens the envelope backwards",
				attempted: -250 * time.Millisecond,
				realUTC:   hostNow.Add(-hostServerSkew),
				wantUpper: 250 * time.Millisecond,
			},
			{
				name:      "elapsed host time projects forward without time-scale scaling",
				updated:   -90 * time.Second,
				attempted: -90 * time.Second,
				realUTC:   hostNow.Add(-hostServerSkew),
				wantLower: 90 * time.Second,
				wantUpper: 90 * time.Second,
			},
			{
				name:      "host behind the server still projects the observed reading",
				realUTC:   hostNow.Add(hostServerSkew),
				wantLower: 0,
				wantUpper: 0,
			},
			{
				name:      "host freshness edge stays inside the window",
				updated:   -2*time.Minute + time.Second,
				attempted: -2*time.Minute + time.Second,
				realUTC:   hostNow.Add(-hostServerSkew),
				wantLower: 2*time.Minute - time.Second,
				wantUpper: 2*time.Minute - time.Second,
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				meta := coherentClockMeta(hostNow.Add(test.updated), generation)
				meta.AttemptedAt = hostNow.Add(test.attempted)
				clock := newClock(test.realUTC)

				lower, upper, ok := serverBounds(hostNow, clock, meta, generation)
				if !ok {
					t.Fatalf("coherent observation was rejected: meta %+v", meta)
				}
				if lower.After(upper) {
					t.Fatalf("inverted envelope: lower %s after upper %s", lower, upper)
				}
				if !lower.Equal(test.realUTC.Add(-test.updated)) {
					t.Fatalf("lower %s does not project host elapsed from UpdatedAt: want %s", lower, test.realUTC.Add(-test.updated))
				}
				if !upper.Equal(test.realUTC.Add(-test.attempted)) {
					t.Fatalf("upper %s does not project host elapsed from AttemptedAt: want %s", upper, test.realUTC.Add(-test.attempted))
				}
				if !lower.Equal(test.realUTC.Add(test.wantLower)) || !upper.Equal(test.realUTC.Add(test.wantUpper)) {
					t.Fatalf("envelope [%s, %s] drifted from the documented projection", lower, upper)
				}
			})
		}
	})

	t.Run("rejects incoherent or missing observations", func(t *testing.T) {
		serverNow := hostNow.Add(-hostServerSkew)
		cases := []struct {
			name    string
			meta    DatasetMeta
			realUTC time.Time
		}{
			{
				name:    "stale refresh is not clock authority",
				meta:    DatasetMeta{UpdatedAt: hostNow, AttemptedAt: hostNow, Generation: generation, Complete: true, Stale: true},
				realUTC: serverNow,
			},
			{
				name:    "failed refresh is not clock authority",
				meta:    DatasetMeta{UpdatedAt: hostNow, AttemptedAt: hostNow, Generation: generation, Complete: true, Error: "connection reset"},
				realUTC: serverNow,
			},
			{
				name:    "incomplete refresh is not clock authority",
				meta:    DatasetMeta{UpdatedAt: hostNow, AttemptedAt: hostNow, Generation: generation},
				realUTC: serverNow,
			},
			{
				name:    "foreign generation is not clock authority",
				meta:    DatasetMeta{UpdatedAt: hostNow, AttemptedAt: hostNow, Generation: "older-generation", Complete: true},
				realUTC: serverNow,
			},
			{
				name:    "missing capture instant",
				meta:    DatasetMeta{AttemptedAt: hostNow, Generation: generation, Complete: true},
				realUTC: serverNow,
			},
			{
				name:    "missing attempt instant",
				meta:    DatasetMeta{UpdatedAt: hostNow, Generation: generation, Complete: true},
				realUTC: serverNow,
			},
			{
				name: "missing server reading",
				meta: coherentClockMeta(hostNow, generation),
			},
			{
				name:    "attempt instant after the capture instant",
				meta:    DatasetMeta{UpdatedAt: hostNow.Add(-time.Second), AttemptedAt: hostNow, Generation: generation, Complete: true},
				realUTC: serverNow,
			},
			{
				name:    "capture stamped in the host future",
				meta:    DatasetMeta{UpdatedAt: hostNow.Add(2 * time.Second), AttemptedAt: hostNow.Add(2 * time.Second), Generation: generation, Complete: true},
				realUTC: serverNow,
			},
			{
				name:    "capture beyond the host freshness window",
				meta:    DatasetMeta{UpdatedAt: hostNow.Add(-2*time.Minute - time.Second), AttemptedAt: hostNow.Add(-2*time.Minute - time.Second), Generation: generation, Complete: true},
				realUTC: serverNow,
			},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				clock := newClock(test.realUTC)
				lower, upper, ok := serverBounds(hostNow, clock, test.meta, generation)
				if ok {
					t.Fatalf("incoherent observation produced authority [%s, %s]", lower, upper)
				}
			})
		}
	})

}

// The turnaround lock timestamp is a server instant. A host clock ahead of the
// server must not release the ship early (the historical bug sent premature
// departures), and a host clock behind must not keep the ship parked after the
// server has already unlocked it. The persisted retry cooldown, in contrast,
// remains a purely host-domain deadline.
func TestDepartureUnlockFollowsConservativeServerTime(t *testing.T) {
	t.Parallel()
	capture := time.Now().UTC()
	cases := []struct {
		name       string
		realUTC    time.Time
		unlockAt   *time.Time
		cooldown   time.Duration
		wantHold   string
		wantDepart bool
	}{
		{
			name:     "host ahead of the server must not unlock the turnaround early",
			realUTC:  capture.Add(-hostServerSkew),
			unlockAt: new(capture.Add(-5 * time.Second)),
			wantHold: "turnaround lock",
		},
		{
			name:       "host behind the server must not stay locked past server time",
			realUTC:    capture.Add(hostServerSkew),
			unlockAt:   new(capture.Add(5 * time.Second)),
			wantDepart: true,
		},
		{
			name:     "persisted retry cooldown stays in the host domain",
			realUTC:  capture.Add(hostServerSkew),
			cooldown: time.Hour,
			wantHold: "cooldown",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{
				stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 12, 3),
			})
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatalf("refresh harness state: %v", err)
			}
			clock := runner.stateCache.Clock
			clock.RealUTC = test.realUTC
			runner.stateCache.Clock = clock

			data := missionData{
				Plan:          Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
				Eligible:      map[string]int64{},
				CooldownUntil: capture.Add(test.cooldown),
			}
			h.putMission(recoveryShipBlastoise, "unlock-generation", "depart", data)
			mission, data := h.mission(recoveryShipBlastoise)
			ship, exists := runner.ship(recoveryShipBlastoise)
			if !exists {
				t.Fatal("harness ship vanished from the refreshed fleet")
			}
			ship.Ship.TurnaroundUnlocksAt = test.unlockAt

			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatalf("drive departure mission: %v", err)
			}
			holding := joinedHoldReasons(runner.holding)
			if test.wantDepart {
				if data.Action == nil || data.Action.Stage != "depart" {
					t.Fatalf("server time had passed the unlock, yet no departure was staged: holding %q", holding)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatalf("dispatch unlocked departure: %v", err)
				}
				if h.api.mutationCount("/v1/fleet/depart") != 1 {
					t.Fatalf("unlocked departure did not reach the wire exactly once: %+v", h.api.mutationsFor("/v1/fleet/depart"))
				}
				return
			}
			if data.Action != nil {
				t.Fatalf("held mission produced a departure intent: %+v", data.Action)
			}
			if !strings.Contains(holding, test.wantHold) {
				t.Fatalf("expected a %q hold, got %q", test.wantHold, holding)
			}
			if h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("a held mission still sent a departure request")
			}
		})
	}

}

// Freight admission compares a server delivery deadline against the projected
// arrival. With the host trailing the server by hostServerSkew, the host-clock
// projection admits a voyage whose server-upper arrival is already past the
// deadline. The fake's route (24 nm at 6.8 effective knots) plus one handling
// day at time scale 1 projects 99,106 seconds out, so the two cases below sit
// either side of that projection plus the skew.
func TestFreightDeadlineAdmissionUsesConservativeServerUpper(t *testing.T) {
	t.Parallel()
	capture := time.Now().UTC()
	cases := []struct {
		name       string
		deadlineIn time.Duration
		wantAdmit  bool
	}{
		{
			name:       "deadline inside the server uncertainty envelope is not admitted",
			deadlineIn: 99_130 * time.Second,
		},
		{
			name:       "deadline beyond the projected upper admits the voyage",
			deadlineIn: 99_160 * time.Second,
			wantAdmit:  true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setFleet([]map[string]any{
				stationaryShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 24, 3),
			})
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatalf("refresh harness state: %v", err)
			}
			clock := runner.stateCache.Clock
			clock.RealUTC = capture.Add(hostServerSkew)
			runner.stateCache.Clock = clock

			data := missionData{
				Plan: Plan{Kind: "recovery", ShipID: recoveryShipBlastoise, Destination: recoveryFarPort},
				Freight: []freightObligation{{
					ContractID:      "freight-a",
					Commodity:       recoveryGrain,
					Quantity:        24,
					Destination:     recoveryFarPort,
					Deadline:        new(capture.Add(test.deadlineIn)),
					RewardPence:     9_000,
					CollateralPence: 5_000,
				}},
				Eligible: map[string]int64{},
			}
			h.putMission(recoveryShipBlastoise, "deadline-generation", "depart", data)
			mission, data := h.mission(recoveryShipBlastoise)
			ship, exists := runner.ship(recoveryShipBlastoise)
			if !exists {
				t.Fatal("harness ship vanished from the refreshed fleet")
			}

			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatalf("drive deadline mission: %v", err)
			}
			holding := joinedHoldReasons(runner.holding)
			if test.wantAdmit {
				if data.Action == nil || data.Action.Stage != "depart" {
					t.Fatalf("server upper projection cleared the deadline, yet departure was held: %q", holding)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatalf("dispatch admitted departure: %v", err)
				}
				if h.api.mutationCount("/v1/fleet/depart") != 1 {
					t.Fatalf("admitted departure did not reach the wire exactly once: %+v", h.api.mutationsFor("/v1/fleet/depart"))
				}
				return
			}
			if data.Action != nil {
				t.Fatalf("deadline inside the uncertainty envelope still staged a departure: %+v", data.Action)
			}
			if !strings.Contains(holding, "delivery_deadline") {
				t.Fatalf("expected a delivery_deadline hold, got %q", holding)
			}
			if h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("an inadmissible voyage still sent a departure request")
			}
		})
	}

}
