package engine

import (
	"context"
	"time"

	"go.uber.org/zap"
)

func (r *Runner) assessShip(shipID string) {
	if shipID == "" {
		return
	}
	if _, exists := r.ship(shipID); !exists {
		return
	}
	for _, assessed := range r.assessedShipIDs {
		if assessed == shipID {
			return
		}
	}
	r.assessedShipIDs = append(r.assessedShipIDs, shipID)
}

// logStatus runs after a persisted reconciliation. Only complete authoritative
// observations can establish the startup baseline or a later ship transition.
func (r *Runner) logStatus(ctx context.Context, now time.Time) {
	observation := PlanningInput{Now: now, Clock: r.stateCache.Clock, Metadata: r.meta, Generation: r.generation}
	if !observation.fresh("ships") {
		return
	}
	if r.lastShipStatus == nil {
		r.lastShipStatus = make(map[string]string)
	}
	if !r.fleetObserved {
		r.logStartupFleet(ctx, now)
		r.fleetObserved = true
	}
	for _, ship := range r.stateCache.Ships {
		previous, observed := r.lastShipStatus[ship.Ship.ID]
		r.lastShipStatus[ship.Ship.ID] = ship.Ship.Status
		if !observed || previous == ship.Ship.Status {
			continue
		}
		message, level := "", zap.InfoLevel
		switch {
		case previous == "sailing" && (ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead"):
			message = "ship arrived"
		case ship.Ship.Status == "sunk":
			message, level = "ship sunk", zap.WarnLevel
		default:
			continue
		}
		entry := r.log.Check(level, message)
		if entry == nil {
			continue
		}
		texts := r.db.ScrubOutcomeTexts(ctx, r.account.ID, []string{
			ship.Ship.ID, ship.Ship.Name, ship.Ship.CurrentPortID,
		})
		entry.Write(
			zap.String("ship_id", texts[0]),
			zap.String("ship", texts[1]),
			zap.String("port_id", texts[2]),
			zap.Time("observed_at", r.meta["ships"].UpdatedAt),
		)
	}
	// Missing rows do not prove loss and must not erase a known sailing state.
}

func (r *Runner) logStartupFleet(ctx context.Context, now time.Time) {
	entry := r.log.Check(zap.InfoLevel, "fleet status")
	if entry == nil {
		return
	}
	mode := "observe"
	if r.opts.Execute {
		mode = "execute"
	}
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
	fields := []zap.Field{
		zap.String("mode", mode),
		zap.Time("reconciled_at", now),
		zap.Bool("fleet_fresh", true),
		zap.Time("fleet_observed_at", r.meta["ships"].UpdatedAt),
		zap.Int("ships", len(r.stateCache.Ships)),
		zap.Int("sailing", sailing),
		zap.Int("in_port", inPort),
		zap.Int("sunk", sunk),
		zap.Int("other", other),
		zap.Int("holding_reasons", len(r.holding)),
	}
	if len(r.holding) > 0 {
		// Scrub before truncation so the cap cannot expose a partial credential.
		reason := r.db.ScrubOutcomeTexts(ctx, r.account.ID, []string{r.holding[0].Reason})[0]
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
}
