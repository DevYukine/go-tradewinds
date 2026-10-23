package engine

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// logStatus runs only after a persisted reconciliation. Counts describe the
// captured fleet, not speculative results of mutations during this cycle.
func (r *Runner) logStatus(ctx context.Context, now time.Time) {
	if !r.lastStatusAt.IsZero() && now.Sub(r.lastStatusAt) < time.Minute {
		return
	}
	entry := r.log.Check(zap.InfoLevel, "fleet status")
	if entry == nil {
		return
	}

	mode := "observe"
	if r.opts.Execute {
		mode = "execute"
	}
	fleetFresh := r.fresh("ships")
	fields := make([]zap.Field, 0, 12)
	fields = append(fields,
		zap.String("mode", mode),
		zap.Time("reconciled_at", now),
		zap.Bool("fleet_fresh", fleetFresh),
		zap.Int("holding_reasons", len(r.holding)),
	)
	if observedAt := r.meta["ships"].UpdatedAt; !observedAt.IsZero() {
		fields = append(fields, zap.Time("fleet_observed_at", observedAt))
	}
	if fleetFresh {
		sailing, inPort, sunk, other := 0, 0, 0, 0
		for _, ship := range r.stateCache.Ships {
			switch ship.Ship.Status {
			case "sailing":
				sailing++
			case "berthed", "anchored_roadstead":
				inPort++
			case "sunk":
				sunk++
			default:
				other++
			}
		}
		fields = append(fields,
			zap.Int("ships", len(r.stateCache.Ships)),
			zap.Int("sailing", sailing),
			zap.Int("in_port", inPort),
			zap.Int("sunk", sunk),
			zap.Int("other", other),
		)
	}
	if len(r.holding) > 0 {
		// Scrub the entire reason before capping it: a cap must not expose a
		// credential prefix that no longer matches the full secret.
		reason := r.db.ScrubOutcomeTexts(ctx, r.account.ID, r.holding[:1])[0]
		characters := 0
		for offset := range reason {
			if characters == 512 {
				reason = reason[:offset]
				fields = append(fields, zap.Bool("holding_reason_truncated", true))
				break
			}
			characters++
		}
		fields = append(fields, zap.String("holding_reason", reason))
	}
	entry.Write(fields...)
	r.lastStatusAt = now
}
