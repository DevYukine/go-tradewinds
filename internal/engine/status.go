package engine

import (
	"context"
	"regexp"
	"time"

	"go.uber.org/zap"
)

var blockingReasonNumbers = regexp.MustCompile(`[0-9.,]+`)

type blockingStatus struct {
	reason        string
	reasonClass   string
	classSince    time.Time
	classLogged   bool
	logged        bool
	blockedSince  time.Time
	lastLogged    time.Time
	currentReason string
	observed      bool
}

// assessHold also identifies the owner of legacy account-wide hold calls.
// Dashboard ship assessment remains restricted to actual fleet members.
func (r *Runner) assessHold(subjectID string) {
	if r.assessedHoldIDs == nil {
		r.assessedHoldIDs = make(map[string]bool)
	}
	r.assessedHoldIDs[subjectID] = true
	r.holdScope = subjectID
}

func (r *Runner) assessShip(shipID string) {
	if shipID == "" {
		return
	}
	switch shipID {
	case "__treasury", "__growth", "__loans", "__underwriting", "__marque":
		r.assessHold(shipID)
		return
	}
	if _, exists := r.ship(shipID); !exists {
		return
	}
	r.assessHold(shipID)
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
	fleetFresh := observation.fresh("ships")
	r.logBlockingStatus(ctx, now, fleetFresh)
	if !fleetFresh {
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

// logBlockingStatus compares completed work passes, not individual hold calls:
// several alternatives can reject the same ship in one reconciliation.
func (r *Runner) logBlockingStatus(ctx context.Context, now time.Time, fleetFresh bool) {
	const (
		transitionDelay = 5 * time.Minute
		heartbeat       = 30 * time.Minute
	)
	if r.blockingStatuses == nil {
		r.blockingStatuses = make(map[string]blockingStatus)
	}
	for id, status := range r.blockingStatuses {
		status.observed = false
		r.blockingStatuses[id] = status
	}
	for _, hold := range r.holding {
		id := hold.subjectID
		if id == "" {
			id = hold.ShipID
		}
		status := r.blockingStatuses[id]
		status.currentReason = hold.Reason
		status.observed = true
		r.blockingStatuses[id] = status
	}
	for id, status := range r.blockingStatuses {
		switch id {
		case "", "__growth", "__treasury", "__loans", "__underwriting", "__marque":
		default:
			if _, exists := r.ship(id); fleetFresh && !exists {
				delete(r.blockingStatuses, id)
				continue
			}
		}
		message := ""
		switch {
		case status.observed:
			if status.blockedSince.IsZero() {
				status.blockedSince = now
			}
			if status.reason != status.currentReason {
				class := blockingReasonNumbers.ReplaceAllString(status.currentReason, "#")
				if status.reasonClass != class {
					status.reasonClass = class
					status.classSince = now
					status.classLogged = false
				}
				status.reason = status.currentReason
			}
			if !status.classLogged && now.Sub(status.classSince) >= transitionDelay {
				message = "work blocked"
				status.classLogged = true
				status.logged = true
			}
		case r.assessedHoldIDs[id]:
			if status.logged {
				r.logBlockingReason(ctx, "work unblocked", id, status, now)
			}
			delete(r.blockingStatuses, id)
			continue
		}
		if message == "" && status.classLogged && now.Sub(status.lastLogged) >= heartbeat {
			message = "work still blocked"
		}
		if message != "" {
			r.logBlockingReason(ctx, message, id, status, now)
			status.lastLogged = now
		}
		// Unassessed subjects retain their last blocker: work may have
		// returned after another ship or control made progress.
		r.blockingStatuses[id] = status
	}
}

func (r *Runner) logBlockingReason(ctx context.Context, message, id string, status blockingStatus, now time.Time) {
	entry := r.log.Check(zap.InfoLevel, message)
	if entry == nil {
		return
	}
	name, port := id, ""
	if ship, exists := r.ship(id); exists {
		name, port = ship.Ship.Name, ship.Ship.CurrentPortID
	}
	texts := r.db.ScrubOutcomeTexts(ctx, r.account.ID, []string{id, name, port, status.reason})
	entry.Write(
		zap.String("ship_id", texts[0]),
		zap.String("ship", texts[1]),
		zap.String("port_id", texts[2]),
		zap.String("reason", texts[3]),
		zap.Time("blocked_since", status.blockedSince.UTC()),
		zap.Duration("blocked_for", now.Sub(status.blockedSince)),
	)
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
