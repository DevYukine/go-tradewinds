package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

const passengerQuarantinePrefix = store.PassengerQuarantinePrefix

// Quarantined accounting is retained separately from operating missions. It
// must not reserve the real hull or become a measured zero-fare trip.
func (r *Runner) archiveQuarantinedPassage(ctx context.Context, mission *store.Mission, data *missionData) error {
	archiveID := passengerQuarantinePrefix + mission.Generation
	existing, err := r.db.PassengerQuarantineArchive(ctx, r.account.ID, mission.Generation)
	if err != nil {
		return err
	}
	if existing == nil {
		archived := *mission
		archived.ShipID, archived.Phase = archiveID, "passage_quarantined"
		archivedData, passage := *data, *data.Passage
		if passage.QuarantineReason == "" {
			passage.QuarantineReason = passage.RecoveryReason
		}
		archivedData.Passage = &passage
		archived.Data = encode(archivedData)
		if err := r.db.PutPassengerQuarantineArchive(ctx, mission.ShipID, &archived); err != nil {
			return err
		}
		existing, err = r.db.PassengerQuarantineArchive(ctx, r.account.ID, mission.Generation)
		if err != nil {
			return err
		}
		if existing == nil {
			return store.ErrNotFound
		}
	} else if existing.Generation != mission.Generation {
		return errors.New("passenger quarantine archive has conflicting mission identity")
	}
	if err := r.recordPassengerQuarantineSettlement(ctx, existing); err != nil {
		return err
	}
	// Retirement labels the intent as quarantined/unknown, not a successful
	// mutation. The archive retains its original immutable request and costs.
	if data.Action != nil {
		pending, err := r.db.PendingActions(ctx, r.account.ID)
		if err != nil {
			return err
		}
		for _, action := range pending {
			if action.ID == data.Action.ID {
				if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "quarantined_passenger_intent", "archive_id": archiveID, "outcome": "unknown"})); err != nil {
					return err
				}
			}
		}
	}
	if r.expenseOwners == nil {
		r.expenseOwners = make(map[string]string)
	}
	for _, id := range data.SettlementExpenseIDs {
		if owner := r.expenseOwners[id]; owner != "" && owner != mission.Generation {
			return errors.New("quarantined passenger expense has conflicting ownership")
		}
		r.expenseOwners[id] = mission.Generation
	}
	r.snapshot["_expense_allocations"] = encode(r.expenseOwners)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) ensurePassengerBoardingHint(ctx context.Context, shipID string) error {
	hints := decode[map[string]bool](r.snapshot, "_passenger_boarding_ships")
	if hints[shipID] {
		return nil
	}
	if hints == nil {
		hints = make(map[string]bool)
	}
	previous, existed := r.snapshot["_passenger_boarding_ships"]
	hints[shipID] = true
	r.snapshot["_passenger_boarding_ships"] = encode(hints)
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		if existed {
			r.snapshot["_passenger_boarding_ships"] = previous
		} else {
			delete(r.snapshot, "_passenger_boarding_ships")
		}
		return err
	}
	return nil
}

func (r *Runner) quarantineAboard(ship ShipState) ([]gameapi.PassengerAboardGroup, bool) {
	if !r.fresh("ships") || !r.fresh("passenger_aboard/"+ship.Ship.ID) {
		return nil, false
	}
	raw := decode[map[string]json.RawMessage](r.snapshot, "passenger_aboard")
	var groups []gameapi.PassengerAboardGroup
	if json.Unmarshal(raw[ship.Ship.ID], &groups) != nil || groups == nil {
		return nil, false
	}
	var hold int64
	for _, group := range groups {
		if group.HoldTons <= 0 || hold > math.MaxInt64-group.HoldTons {
			return nil, false
		}
		hold += group.HoldTons
	}
	return groups, hold == ship.PassengerHoldTons
}

// Retire the immutable quarantined intent before the account's unresolved
// spending gate. Its private request remains archived with an unknown outcome.
func (r *Runner) retireQuarantinedPassageAction(ctx context.Context, mission *store.Mission, data *missionData) error {
	if err := r.ensurePassengerBoardingHint(ctx, mission.ShipID); err != nil {
		return err
	}
	if err := r.archiveQuarantinedPassage(ctx, mission, data); err != nil {
		return err
	}
	next := *data
	next.LastActionID = next.Action.ID
	next.Action = nil
	next.Sequence++
	if err := r.saveMission(ctx, mission, &next); err != nil {
		return err
	}
	*data = next
	return nil
}

func (r *Runner) driveQuarantinedPassage(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (changed bool, err error) {
	defer r.quarantinePassageError(ctx, mission, data, &changed, &err)
	groups, ok := r.quarantineAboard(ship)
	if !ok || !r.fresh("passenger_offers") {
		if !ok {
			if err := r.ensurePassengerBoardingHint(ctx, ship.Ship.ID); err != nil {
				return false, err
			}
		}
		return false, r.hold(ctx, ship.Ship.ID, "quarantined passage awaits authoritative aboard and offers before hull recovery")
	}
	if len(groups) == 0 && !r.hasOpenPassengerOffer(ship.Ship.ID) {
		return r.releaseQuarantinedPassage(ctx, ship, mission, data)
	}
	if err := r.archiveQuarantinedPassage(ctx, mission, data); err != nil {
		return false, err
	}
	reason := data.Passage.QuarantineReason
	destination := data.Plan.Destination
	if len(groups) > 0 {
		destination = groups[0].DestPortID
	}
	next, nextData := *mission, *data
	next.Phase, next.Destination = "passenger_recovery", destination
	nextData.Plan.Destination = destination
	nextData.CostsKnown = false
	nextData.Settlement = nil
	if nextData.Action != nil {
		nextData.Action = nil
		nextData.Sequence++
	}
	nextData.Passage = &passageIntent{RecoveryOnly: true, RecoveryReason: reason, Groups: groups, Abandon: true}
	if err := r.saveMission(ctx, &next, &nextData); err != nil {
		return false, err
	}
	*mission, *data = next, nextData
	return r.drivePassengerRecovery(ctx, ship, mission, data)
}

func (r *Runner) hasOpenPassengerOffer(shipID string) bool {
	for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
		if offer.ShipID == shipID && offer.Status == "open" {
			return true
		}
	}
	return false
}

func (r *Runner) releaseQuarantinedPassage(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if err := r.archiveQuarantinedPassage(ctx, mission, data); err != nil {
		return false, err
	}
	if data.Passage.RecoveryOnly {
		if err := r.recordPassengerRecoverySettlement(ctx, mission, data); err != nil {
			return false, err
		}
	}
	hints := decode[map[string]bool](r.snapshot, "_passenger_boarding_ships")
	delete(hints, ship.Ship.ID)
	r.snapshot["_passenger_boarding_ships"] = encode(hints)
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		return false, err
	}
	if err := r.db.ReleaseQuarantinedPassengerHull(ctx, r.account.ID, ship.Ship.ID); err != nil {
		return false, err
	}
	// Releasing a private mission is not a game mutation: other ships may work
	// in this pass, and this hull is available to the normal planner next pass.
	return false, nil
}

// Recovery performs navigation only; damaged receipt evidence never runs
// through syncPassage again or becomes new income. Fresh aboard groups, not
// malformed historical boarding timestamps, determine the safe destination.
func (r *Runner) drivePassengerRecovery(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	groups, ok := r.quarantineAboard(ship)
	if !ok || !r.fresh("passenger_offers") {
		return false, r.hold(ctx, ship.Ship.ID, "passenger recovery awaits authoritative aboard and offers")
	}
	data.Passage.Groups = groups
	if ship.Ship.Status == "sailing" {
		return false, r.holdVoyageArrival(ctx, ship, mission, data, "quarantined passenger accounting retained; hull recovery waits for arrival")
	}
	for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
		if offer.ShipID == ship.Ship.ID && offer.Status == "open" {
			data.Passage.OfferID = offer.ID
			return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID})
		}
	}
	if len(groups) == 0 {
		return r.releaseQuarantinedPassage(ctx, ship, mission, data)
	}
	destination := groups[0].DestPortID
	seen := make(map[string]bool, len(groups))
	for _, group := range groups {
		if group.ID == "" || seen[group.ID] || group.Size <= 0 || destination == "" || group.DestPortID != destination {
			return false, r.hold(ctx, ship.Ship.ID, "passenger recovery requires uniquely identified groups sharing a destination")
		}
		seen[group.ID] = true
	}
	data.Plan.Destination, mission.Destination = destination, destination
	if ship.Ship.CurrentPortID == destination {
		return false, r.hold(ctx, ship.Ship.ID, "passenger recovery arrived; waiting for authoritative disembarkation")
	}
	if !r.shipUnlocked(ship) {
		return false, r.hold(ctx, ship.Ship.ID, "passenger recovery waits for turnaround unlock")
	}
	if err := r.passageDepartureReady(ship, destination, data); err != nil {
		return false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	route := r.stateCache.Routes[routeKey(ship.Ship.CurrentPortID, destination, hull.ID)]
	if ship.Stores.ProvisionsTons+1e-9 < route.SafeProvisionsTons {
		return r.provisionMission(ctx, ship, mission, data, route.SafeProvisionsTons)
	}
	return true, r.stage(ctx, mission, data, ship, "depart", "/v1/fleet/depart", map[string]string{"ship_id": ship.Ship.ID, "dest_port_id": destination})
}
