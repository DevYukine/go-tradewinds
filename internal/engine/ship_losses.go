package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type shipLossObservation struct {
	Ship struct {
		ID            string  `json:"id"`
		CompanyID     string  `json:"company_id"`
		Name          string  `json:"name"`
		ShipClass     string  `json:"ship_class"`
		Status        string  `json:"status"`
		Fate          *string `json:"fate"`
		CurrentPortID *string `json:"current_port_id"`
		CurrentLegIdx int     `json:"current_leg_idx"`
	} `json:"ship"`
	CargoManifest           json.RawMessage `json:"cargo_manifest"`
	Stores                  json.RawMessage `json:"stores"`
	CargoUnavailableReason  string          `json:"cargo_unavailable_reason,omitempty"`
	StoresUnavailableReason string          `json:"stores_unavailable_reason,omitempty"`
	RouteLegs               []struct {
		FromNode string `json:"from_node"`
		ToNode   string `json:"to_node"`
	} `json:"route_legs"`
	ObservedAt time.Time `json:"observed_at"`
}

type retainedShipLossObservations struct {
	Latest map[string]shipLossObservation `json:"latest"`
	Afloat map[string]shipLossObservation `json:"afloat"`
}

// retainedLossShips reads the last good observations before a capture replaces
// them. Child observations can be newer than the fleet list.
func (r *Runner) retainedLossShips(ctx context.Context) (map[string]shipLossObservation, map[string]shipLossObservation) {
	if _, exists := r.snapshot["_ship_loss_verified_ships"]; exists {
		verified := decode[map[string]shipLossObservation](r.snapshot, "_ship_loss_verified_ships")
		for id, ship := range verified {
			if id != ship.Ship.ID || ship.Ship.CompanyID != r.account.CompanyID || ship.Ship.Status == "sunk" {
				delete(verified, id)
			} else {
				verified[id] = r.sanitizeLossObservation(ctx, ship)
			}
		}
		r.snapshot["_ship_loss_verified_ships"] = encode(verified)
		return verified, verified
	}
	if _, exists := r.snapshot["_ship_loss_legacy_ships"]; exists {
		retained := decode[retainedShipLossObservations](r.snapshot, "_ship_loss_legacy_ships")
		for _, rows := range [2]map[string]shipLossObservation{retained.Latest, retained.Afloat} {
			for id, ship := range rows {
				if id != ship.Ship.ID || ship.Ship.CompanyID != r.account.CompanyID {
					delete(rows, id)
				} else {
					rows[id] = r.sanitizeLossObservation(ctx, ship)
				}
			}
		}
		r.snapshot["_ship_loss_legacy_ships"] = encode(retained)
		return retained.Latest, retained.Afloat
	}
	var ships []shipLossObservation
	_ = json.Unmarshal(r.snapshot["ships"], &ships)
	latest := make(map[string]shipLossObservation, len(ships))
	afloat := make(map[string]shipLossObservation, len(ships))
	remember := func(ship shipLossObservation) {
		if ship.Ship.ID == "" || ship.Ship.CompanyID != r.account.CompanyID {
			return
		}
		ship = r.sanitizeLossObservation(ctx, ship)
		previous, exists := latest[ship.Ship.ID]
		if !exists || ship.ObservedAt.After(previous.ObservedAt) {
			latest[ship.Ship.ID] = ship
		}
		previous, exists = afloat[ship.Ship.ID]
		if ship.Ship.Status != "sunk" && (!exists || ship.ObservedAt.After(previous.ObservedAt)) {
			afloat[ship.Ship.ID] = ship
		}
	}
	for _, ship := range ships {
		ship.ObservedAt = r.meta["ships"].UpdatedAt
		remember(ship)
	}
	for id, raw := range decode[map[string]json.RawMessage](r.snapshot, "ship_details") {
		var ship shipLossObservation
		if json.Unmarshal(raw, &ship) != nil || ship.Ship.ID != id {
			continue
		}
		ship.ObservedAt = r.meta["ship_details/"+id].UpdatedAt
		remember(ship)
	}
	return latest, afloat
}

func lossText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func (r *Runner) shipLossEvidence(ctx context.Context, kind string, raw json.RawMessage, previousReason string) (json.RawMessage, string) {
	normalized, err := store.NormalizeShipLossEvidence(raw)
	if err != nil {
		r.shipLossWarning(ctx, kind, err)
		return nil, "Last observed evidence was invalid or exceeded recording limits."
	}
	if normalized != nil {
		return normalized, ""
	}
	if previousReason != "" {
		return nil, "Last observed evidence was invalid or exceeded recording limits."
	}
	return nil, ""
}

func (r *Runner) sanitizeLossObservation(ctx context.Context, ship shipLossObservation) shipLossObservation {
	ship.CargoManifest, ship.CargoUnavailableReason = r.shipLossEvidence(ctx, "cargo", ship.CargoManifest, ship.CargoUnavailableReason)
	ship.Stores, ship.StoresUnavailableReason = r.shipLossEvidence(ctx, "stores", ship.Stores, ship.StoresUnavailableReason)
	return ship
}

func (r *Runner) newShipLoss(ctx context.Context, ship shipLossObservation, previous shipLossObservation, observedAt time.Time) store.ShipLoss {
	fate := "unknown"
	if ship.Ship.Fate != nil && *ship.Ship.Fate != "" {
		fate = *ship.Ship.Fate
	}
	loss := store.ShipLoss{
		AccountID: r.account.ID, ShipID: ship.Ship.ID, ShipName: ship.Ship.Name,
		ShipClass: ship.Ship.ShipClass, Fate: fate, Status: "sunk", ObservedAt: observedAt,
	}
	if previous.Ship.ID == "" || previous.Ship.Status == "sunk" {
		return loss
	}
	if loss.ShipName == "" {
		loss.ShipName = previous.Ship.Name
	}
	if loss.ShipClass == "" {
		loss.ShipClass = previous.Ship.ShipClass
	}
	if !previous.ObservedAt.IsZero() {
		loss.LastObservedAt = &previous.ObservedAt
	}
	if previous.Ship.Status != "sailing" && previous.Ship.CurrentPortID != nil {
		loss.LastPortID = lossText(*previous.Ship.CurrentPortID)
	}
	loss.CargoManifest, loss.CargoUnavailableReason = r.shipLossEvidence(ctx, "cargo", previous.CargoManifest, previous.CargoUnavailableReason)
	loss.Stores, loss.StoresUnavailableReason = r.shipLossEvidence(ctx, "stores", previous.Stores, previous.StoresUnavailableReason)
	if previous.Ship.Status == "sailing" && len(previous.RouteLegs) > 0 {
		loss.VoyageOriginID = lossText(previous.RouteLegs[0].FromNode)
		loss.VoyageDestinationID = lossText(previous.RouteLegs[len(previous.RouteLegs)-1].ToNode)
	}
	catalog := decode[Catalog](r.snapshot, "catalog")
	if index := previous.Ship.CurrentLegIdx; previous.Ship.Status == "sailing" && index >= 0 && index < len(previous.RouteLegs) {
		node := previous.RouteLegs[index].FromNode
		for _, zone := range catalog.SeaZones {
			if zone.ID == node {
				loss.LastSeaZoneID = lossText(node)
				break
			}
		}
		if loss.LastSeaZoneID == nil {
			for _, port := range catalog.Ports {
				if port.ID == node {
					loss.LastSeaZoneID = lossText(port.SeaZoneID)
					break
				}
			}
		}
	}
	for _, port := range catalog.Ports {
		if loss.LastSeaZoneID == nil && loss.LastPortID != nil && port.ID == *loss.LastPortID {
			loss.LastSeaZoneID = lossText(port.SeaZoneID)
		}
	}
	return loss
}

// observeShipLosses runs before applyCapture replaces the prior observation.
// History failures retain a snapshot-backed retry and never stop fleet work.
func (r *Runner) observeShipLosses(ctx context.Context, key string, raw json.RawMessage, observedAt time.Time) {
	if r.db == nil {
		return
	}
	// Loss rows use PostgreSQL microsecond precision. Keep snapshot-backed
	// retry deadlines on the same clock so persisted observations compare
	// identically before and after a restart.
	observedAt = observedAt.UTC().Truncate(time.Microsecond)
	previous, afloat := r.retainedLossShips(ctx)
	if _, verified := r.snapshot["_ship_loss_verified_ships"]; !verified && len(previous) > 0 {
		r.snapshot["_ship_loss_legacy_ships"] = encode(retainedShipLossObservations{Latest: previous, Afloat: afloat})
	}
	completeFleet := key == "ships"
	var ships []shipLossObservation
	if completeFleet {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			r.shipLossWarning(ctx, "observation", errors.New("ship loss fleet observation is not an array"))
			return
		}
		if err := json.Unmarshal(raw, &ships); err != nil {
			r.shipLossWarning(ctx, "observation", err)
			return
		}
	} else {
		var ship shipLossObservation
		if err := json.Unmarshal(raw, &ship); err != nil {
			r.shipLossWarning(ctx, "observation", err)
			return
		}
		if ship.Ship.ID != strings.TrimPrefix(key, "ship_details/") {
			r.shipLossWarning(ctx, "row_identity", errors.New("ship loss detail identity does not match its request"))
			return
		}
		ships = []shipLossObservation{ship}
	}
	present := make(map[string]bool, len(ships))
	owned := ships[:0]
	for _, ship := range ships {
		if ship.Ship.ID != "" {
			present[ship.Ship.ID] = true
		}
		if ship.Ship.ID == "" || ship.Ship.CompanyID != r.account.CompanyID {
			r.shipLossWarning(ctx, "row_identity", errors.New("ship loss fleet row does not match the configured company"))
			continue
		}
		owned = append(owned, ship)
	}
	ships = owned
	losses := r.shipLosses
	if losses == nil || r.shipLossesGeneration != r.generation {
		if err := r.verifyIdentity(); err != nil {
			r.shipLossWarning(ctx, "identity", err)
			return
		}
		if !observedAt.Before(decode[time.Time](r.snapshot, "_ship_loss_load_retry_after")) {
			known, err := r.db.ShipLosses(ctx, r.account.ID)
			if err != nil {
				r.shipLossWarning(ctx, "load", err)
				r.snapshot["_ship_loss_load_retry_after"] = encode(observedAt.Add(15 * time.Minute))
			} else {
				losses = make(map[string]store.ShipLoss, len(known))
				durable := make(map[string]store.ShipLoss, len(known))
				for _, loss := range known {
					losses[loss.ShipID], durable[loss.ShipID] = loss, loss
				}
				r.shipLossesDurable = durable
				if !r.shipLossesLoaded && len(known) > 0 {
					// Recover a crash after the loss commit but before the
					// snapshot committed its event-backfill obligation.
					r.snapshot["_ship_loss_events_backfill"] = encode(true)
				}
				r.shipLossesLoaded = true
				delete(r.snapshot, "_ship_loss_load_retry_after")
			}
		}
		if losses == nil {
			losses = make(map[string]store.ShipLoss)
		}
		r.shipLosses, r.shipLossesGeneration = losses, r.generation
	}
	if r.shipLossesDurable == nil {
		r.shipLossesDurable = make(map[string]store.ShipLoss)
	}
	changes := decode[map[string]store.ShipLoss](r.snapshot, "_pending_ship_losses")
	if changes == nil {
		changes = make(map[string]store.ShipLoss)
	}
	retryAfter := decode[map[string]time.Time](r.snapshot, "_ship_loss_retry_after")
	if retryAfter == nil {
		retryAfter = make(map[string]time.Time)
	}
	for id, pending := range changes {
		if durable, exists := r.shipLossesDurable[id]; exists &&
			(durable.RecoveredAt != nil && !pending.ObservedAt.After(*durable.RecoveredAt) ||
				durable.LastObservedAt != nil && durable.LastObservedAt.After(pending.ObservedAt)) {
			delete(changes, id)
			delete(retryAfter, id)
			continue
		}
		pending.CargoManifest, pending.CargoUnavailableReason = r.shipLossEvidence(ctx, "cargo", pending.CargoManifest, pending.CargoUnavailableReason)
		pending.Stores, pending.StoresUnavailableReason = r.shipLossEvidence(ctx, "stores", pending.Stores, pending.StoresUnavailableReason)
		if pending.HazardCause != nil {
			if hazard, err := store.NormalizeShipLossHazard(*pending.HazardCause); err != nil {
				r.shipLossWarning(ctx, "hazard", err)
				pending.HazardCause = nil
				pending.HazardUnavailableReason = "The reported hazard was invalid or exceeded recording limits."
			} else {
				pending.HazardCause = hazard
			}
		}
		losses[id], changes[id] = pending, pending
	}
	missingSince := decode[map[string]time.Time](r.snapshot, "_ship_loss_missing_since")
	if missingSince == nil {
		missingSince = make(map[string]time.Time)
	}
	for id := range present {
		delete(missingSince, id)
	}
	backfill := false
	for _, ship := range ships {
		id := ship.Ship.ID
		loss, exists := losses[id]
		if ship.Ship.Status != "sunk" {
			if exists && loss.Fate == "missing" && loss.RecoveredAt == nil && observedAt.After(loss.ObservedAt) {
				recoveredAt := observedAt
				loss.RecoveredAt = &recoveredAt
				losses[id], changes[id] = loss, loss
			}
			continue
		}
		if !exists || loss.RecoveredAt != nil && observedAt.After(*loss.RecoveredAt) {
			loss = r.newShipLoss(ctx, ship, afloat[id], observedAt)
		} else if loss.RecoveredAt == nil && (loss.Fate == "missing" || loss.Fate == "unknown") {
			fate := "unknown"
			if ship.Ship.Fate != nil && *ship.Ship.Fate != "" {
				fate = *ship.Ship.Fate
			}
			if fate == loss.Fate {
				continue
			}
			loss.Fate = fate
		} else {
			continue
		}
		backfill = true
		losses[id], changes[id] = loss, loss
	}
	if completeFleet {
		if len(ships) == 0 {
			// An empty or entirely unowned list cannot confirm disappearance.
			clear(missingSince)
		} else {
			for id, ship := range previous {
				if present[id] {
					continue
				}
				if loss, exists := losses[id]; exists && (loss.RecoveredAt == nil || !observedAt.After(*loss.RecoveredAt)) {
					delete(missingSince, id)
					continue
				}
				if ship.Ship.Status != "sunk" {
					first, exists := missingSince[id]
					if !exists {
						missingSince[id] = observedAt
						continue
					}
					if observedAt.Before(first.Add(5 * time.Minute)) {
						continue
					}
				}
				loss := r.newShipLoss(ctx, ship, afloat[id], observedAt)
				if ship.Ship.Status != "sunk" {
					loss.Fate = "missing"
				}
				backfill = true
				losses[id], changes[id] = loss, loss
				delete(missingSince, id)
			}
		}
	}
	r.snapshot["_ship_loss_missing_since"] = encode(missingSince)
	// Keep omitted afloat evidence while absence is being confirmed. Only a
	// terminal observation replaces it; an empty list must not erase it.
	verified := make(map[string]shipLossObservation, len(previous)+len(ships))
	for id, ship := range previous {
		if ship.Ship.Status != "sunk" {
			verified[id] = ship
		}
	}
	for _, ship := range ships {
		if ship.Ship.Status == "sunk" {
			delete(verified, ship.Ship.ID)
		} else {
			ship.ObservedAt = observedAt
			verified[ship.Ship.ID] = r.sanitizeLossObservation(ctx, ship)
		}
	}
	r.snapshot["_ship_loss_verified_ships"] = encode(verified)
	delete(r.snapshot, "_ship_loss_legacy_ships")
	if backfill {
		r.snapshot["_ship_loss_events_backfill"] = encode(true)
	}
	if completeFleet || len(changes) > 0 {
		r.enrichObservedShipLosses(ctx, losses, changes, observedAt, backfill)
	}
	r.saveObservedShipLosses(ctx, changes, retryAfter, observedAt)
}

func shipLossHasVoyageFate(fate string) bool {
	return fate == "lost_at_sea" || fate == "captured" || fate == "unknown"
}

func (r *Runner) enrichObservedShipLosses(ctx context.Context, losses, changes map[string]store.ShipLoss, observedAt time.Time, newLoss bool) {
	needsEvents := false
	for _, loss := range losses {
		if loss.RecoveredAt == nil && ((shipLossHasVoyageFate(loss.Fate) && (loss.InsuranceClaimPaidPence == nil || loss.HazardCause == nil)) || loss.Fate == "missing") {
			needsEvents = true
			break
		}
	}
	if !needsEvents {
		delete(r.snapshot, "_ship_loss_events_backfill")
		return
	}
	if !newLoss && observedAt.Before(decode[time.Time](r.snapshot, "_ship_loss_events_retry_after")) {
		return
	}
	cursor := decode[int64](r.snapshot, "_ship_loss_event_cursor")
	afterID := cursor
	if decode[bool](r.snapshot, "_ship_loss_events_backfill") {
		afterID = 0
	}
	events, err := r.db.ShipLossEvents(ctx, r.account.ID, afterID)
	if err != nil {
		r.shipLossWarning(ctx, "events", err)
		r.snapshot["_ship_loss_events_retry_after"] = encode(observedAt.Add(15 * time.Minute))
		return
	}
	r.enrichShipLosses(ctx, losses, changes, events)
	for _, event := range events {
		cursor = max(cursor, event.ID)
	}
	// Pending enriched rows are snapshot-backed separately. A failed row save
	// must not cause the same successful event backfill on every child capture.
	r.snapshot["_ship_loss_event_cursor"] = encode(cursor)
	delete(r.snapshot, "_ship_loss_events_backfill")
	delete(r.snapshot, "_ship_loss_events_retry_after")
}

func (r *Runner) saveObservedShipLosses(ctx context.Context, changes map[string]store.ShipLoss, retryAfter map[string]time.Time, observedAt time.Time) {
	rows := make([]store.ShipLoss, 0, len(changes))
	for id, loss := range changes {
		if !observedAt.Before(retryAfter[id]) {
			rows = append(rows, loss)
		}
	}
	saved := func(loss store.ShipLoss) {
		r.shipLossesDurable[loss.ShipID] = loss
		delete(changes, loss.ShipID)
		delete(retryAfter, loss.ShipID)
	}
	for start := 0; start < len(rows); start += store.MaxShipLossBatch {
		batch := rows[start:min(start+store.MaxShipLossBatch, len(rows))]
		if err := r.db.SaveShipLosses(ctx, r.account.ID, batch); err != nil {
			r.shipLossWarning(ctx, "save", err)
			if !errors.Is(err, store.ErrInvalidInput) && !errors.Is(err, store.ErrResponseTooLarge) {
				// One unavailable database must not cost one timeout per hull.
				for _, loss := range rows[start:] {
					retryAfter[loss.ShipID] = observedAt.Add(15 * time.Minute)
				}
				break
			}
			for _, loss := range batch {
				if err := r.db.SaveShipLosses(ctx, r.account.ID, []store.ShipLoss{loss}); err == nil {
					saved(loss)
				} else {
					retryAfter[loss.ShipID] = observedAt.Add(15 * time.Minute)
				}
			}
			continue
		}
		for _, loss := range batch {
			saved(loss)
		}
	}
	if len(changes) > 0 {
		r.snapshot["_pending_ship_losses"] = encode(changes)
		r.snapshot["_ship_loss_retry_after"] = encode(retryAfter)
		if len(rows) > 0 {
			r.snapshot["_ship_loss_events_retry_after"] = encode(observedAt.Add(15 * time.Minute))
		}
	} else {
		delete(r.snapshot, "_pending_ship_losses")
		delete(r.snapshot, "_ship_loss_retry_after")
	}
}

func (r *Runner) shipLossWarning(ctx context.Context, cause string, err error) {
	if err == nil {
		return
	}
	if r.shipLossesWarnedAt == nil {
		r.shipLossesWarnedAt = make(map[string]time.Time)
	}
	if time.Since(r.shipLossesWarnedAt[cause]) < time.Hour {
		return
	}
	r.shipLossesWarnedAt[cause] = time.Now()
	r.log.Warn("ship loss history not recorded", r.scrubOutcomeFields(ctx, nil, []outcomeText{
		{field: "operation", value: cause},
		{field: "cause", value: err.Error()},
	})...)
}

type shipLossEvent struct {
	ShipID            string `json:"ship_id"`
	Hazard            string `json:"hazard"`
	Sunk              bool   `json:"sunk"`
	Captured          bool   `json:"captured"`
	InsuredCompanyID  string `json:"insured_company_id"`
	Status            string `json:"status"`
	LossBPS           int64  `json:"loss_bps"`
	ClaimPaidPence    *int64 `json:"claim_paid_pence"`
	OriginPortID      string `json:"origin_port_id"`
	DestinationPortID string `json:"dest_port_id"`
}

func (r *Runner) enrichShipLosses(ctx context.Context, losses, changes map[string]store.ShipLoss, events []store.Event) {
	for _, event := range events {
		var envelope struct {
			CompanyID string `json:"company_id"`
		}
		if json.Unmarshal(event.Payload, &envelope) != nil || envelope.CompanyID != "" && envelope.CompanyID != r.account.CompanyID {
			continue
		}
		var payload shipLossEvent
		if _, ok := decodeEventPayload(event.Payload, &payload); !ok {
			continue
		}
		loss, exists := losses[payload.ShipID]
		if !exists || loss.RecoveredAt != nil {
			continue
		}
		// Event.CreatedAt is the recorder's real-time receipt. Event envelope
		// timestamps use the accelerated world clock and cannot order captures.
		if event.CreatedAt.IsZero() || loss.LastObservedAt != nil && event.CreatedAt.Before(*loss.LastObservedAt) {
			continue
		}
		changed := false
		switch event.EventType {
		case "voyage_hazard":
			if (!shipLossHasVoyageFate(loss.Fate) && loss.Fate != "missing") || payload.Sunk == payload.Captured {
				continue
			}
			fate := "lost_at_sea"
			if payload.Captured {
				fate = "captured"
			}
			if loss.Fate != "missing" && loss.Fate != "unknown" && loss.Fate != fate {
				continue
			}
			if loss.Fate == "missing" || loss.Fate == "unknown" {
				loss.Fate = fate
				changed = true
			}
			if payload.Hazard != "" && loss.HazardCause == nil {
				hazard, err := store.NormalizeShipLossHazard(payload.Hazard)
				if err != nil {
					r.shipLossWarning(ctx, "hazard", err)
					loss.HazardUnavailableReason = "The reported hazard was invalid or exceeded recording limits."
				} else {
					loss.HazardCause = hazard
					loss.HazardUnavailableReason = ""
				}
				changed = true
			}
		case "insurance_claim_paid", "insurance_policy_closed":
			if !shipLossHasVoyageFate(loss.Fate) || payload.Status != "claimed_loss" || payload.LossBPS != 10_000 ||
				payload.InsuredCompanyID != r.account.CompanyID || payload.ClaimPaidPence == nil ||
				*payload.ClaimPaidPence < 0 || loss.InsuranceClaimPaidPence != nil {
				continue
			}
			if loss.VoyageOriginID != nil && payload.OriginPortID != "" && payload.OriginPortID != *loss.VoyageOriginID ||
				loss.VoyageDestinationID != nil && payload.DestinationPortID != "" && payload.DestinationPortID != *loss.VoyageDestinationID {
				continue
			}
			// A claim received before the loss needs matching voyage evidence;
			// otherwise it might describe a previous policy on the same hull.
			matchingVoyage := loss.VoyageOriginID != nil && loss.VoyageDestinationID != nil &&
				payload.OriginPortID == *loss.VoyageOriginID && payload.DestinationPortID == *loss.VoyageDestinationID
			if event.CreatedAt.Before(loss.ObservedAt) && !matchingVoyage {
				continue
			}
			loss.InsuranceClaimPaidPence = payload.ClaimPaidPence
			changed = true
		}
		if changed {
			losses[loss.ShipID], changes[loss.ShipID] = loss, loss
		}
	}
}
