package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// planner.md D10: when the realized global marque window is losing money, a
// fresh optimistic quote may no longer overrule the realized outcome evidence.
// One remeasurement issue per real day stays allowed; positive realized net
// keeps ordinary issuance.
func TestEvaluateMarqueRealizedLossDailyPause(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	quote := MarqueQuote{SeaZoneID: "zone", CostPence: 10_000, ExpectedNetPrizePence: 20_000,
		LadenPassagesPerDay: 2, AddedPiracyBPS: 500, DurationGameHours: 60}
	loser := func(expired time.Time) marqueOutcome {
		return marqueOutcome{ID: uuid.NewString(), SeaZoneID: "zone", ExpiredAt: expired,
			CostPence: 10_000, PrizePence: 9_000}
	}
	input := func(scale int64, outcomes ...marqueOutcome) MarqueInput {
		rows := make(map[string]marqueOutcome, len(outcomes))
		for _, outcome := range outcomes {
			rows[outcome.ID] = outcome
		}
		zones, global := rollingMarqueReturns(rows, now, scale)
		return MarqueInput{Quote: quote, Now: now, LiquidPence: 1_000_000, ZoneReturns: zones, GlobalReturns: global}
	}
	t.Run("losing window inside 24 real hours refuses even a twice-cost quote", func(t *testing.T) {
		// Scale 60 keeps both losing expiries inside the 24-real-hour global
		// window, so the latest expiry is less than one real day old.
		in := input(60, loser(now.Add(-2*time.Hour)), loser(now.Add(-time.Hour)))
		if global := in.GlobalReturns; global.Count < 2 || global.PrizePence >= global.CostPence ||
			!now.Before(global.LatestExpiry.Add(24*time.Hour)) {
			t.Fatalf("fixture lost its fresh losing global window: %+v", global)
		}
		decision := EvaluateMarque(in)
		if decision.Accepted {
			t.Fatalf("negative realized global net must refuse a new issue despite the 2x re-entry quote: %+v", decision)
		}
		if strings.TrimSpace(decision.Reason) == "" {
			t.Fatal("realized-loss refusal has no reason")
		}
	})
	t.Run("losing window older than 24 real hours allows one remeasurement issue", func(t *testing.T) {
		// Scale 30 widens the global window to 48 real hours while both losing
		// expiries sit more than 24 real hours back.
		in := input(30, loser(now.Add(-26*time.Hour)), loser(now.Add(-25*time.Hour)))
		if global := in.GlobalReturns; global.Count < 2 || global.PrizePence >= global.CostPence ||
			now.Before(global.LatestExpiry.Add(24*time.Hour)) {
			t.Fatalf("fixture lost its aged losing global window: %+v", global)
		}
		if decision := EvaluateMarque(in); !decision.Accepted {
			t.Fatalf("losing expiries older than a real day must allow the day's one remeasurement issue: %+v", decision)
		}
	})
	t.Run("positive realized net keeps normal issuance", func(t *testing.T) {
		winner := loser(now.Add(-time.Hour))
		winner.PrizePence = 30_000
		in := input(60, winner)
		if global := in.GlobalReturns; global.Count != 1 || global.PrizePence < global.CostPence {
			t.Fatalf("fixture lost its winning global window: %+v", global)
		}
		if decision := EvaluateMarque(in); !decision.Accepted {
			t.Fatalf("positive realized global net must keep ordinary issuance: %+v", decision)
		}
	})
}
