package engine

import (
	"math"
	"time"
)

// The request envelope brackets the observed server reading. Elapsed time is
// real time; the game time scale applies only when converting game durations.
func serverBounds(hostNow time.Time, clock Clock, meta DatasetMeta, generation string) (lower, upper time.Time, ok bool) {
	if clock.RealUTC.IsZero() || meta.AttemptedAt.IsZero() || meta.UpdatedAt.IsZero() ||
		!meta.Complete || meta.Stale || meta.Error != "" || generation == "" || meta.Generation != generation ||
		meta.AttemptedAt.After(meta.UpdatedAt) || meta.UpdatedAt.After(hostNow) || hostNow.Sub(meta.UpdatedAt) > 2*time.Minute {
		return time.Time{}, time.Time{}, false
	}
	return clock.RealUTC.Add(hostNow.Sub(meta.UpdatedAt)), clock.RealUTC.Add(hostNow.Sub(meta.AttemptedAt)), true
}

func (in PlanningInput) serverBounds() (time.Time, time.Time, bool) {
	return serverBounds(in.Now, in.Clock, in.Metadata["clock"], in.Generation)
}

func (r *Runner) serverBounds() (time.Time, time.Time, bool) {
	return serverBounds(time.Now(), r.stateCache.Clock, r.meta["clock"], r.generation)
}

func (r *Runner) shipUnlocked(ship ShipState) bool {
	lower, _, ok := r.serverBounds()
	return ok && unlocked(ship, lower)
}

func (r *Runner) deadlineOpen(deadline *time.Time) bool {
	_, upper, ok := r.serverBounds()
	return ok && (deadline == nil || upper.Before(*deadline))
}

func gameDuration(days float64, scale int64) time.Duration {
	return time.Duration(math.Ceil(days*86_400/float64(scale))) * time.Second
}

func (r *Runner) wakeAtServer(deadline time.Time, unlock bool) {
	now := time.Now()
	lower, upper, ok := serverBounds(now, r.stateCache.Clock, r.meta["clock"], r.generation)
	if !ok {
		return
	}
	bound := upper
	if unlock {
		bound = lower
	}
	r.wakeBy(now.Add(max(0, deadline.Sub(bound))))
}
