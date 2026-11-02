package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const shipRenameMission = "__rename"
const shipRenameRetry = 5 * time.Minute

type shipRenameIntent struct {
	ShipID string `json:"ship_id"`
	Name   string `json:"name"`
}

type shipRenameRefusal struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func (r *Runner) recordShipRenameRefusal(ctx context.Context, data *missionData) error {
	if data.Rename == nil {
		return errors.New("refused ship rename has no durable naming intent")
	}
	ship, exists := r.ship(data.Rename.ShipID)
	if !exists {
		ship = data.Action.Before
	}
	refusals := decode[map[string]shipRenameRefusal](r.snapshot, "_ship_rename_refusals")
	if refusals == nil {
		refusals = make(map[string]shipRenameRefusal)
	}
	refusals[data.Rename.ShipID] = shipRenameRefusal{Name: ship.Ship.Name, Status: ship.Ship.Status}
	r.snapshot["_ship_rename_refusals"] = encode(refusals)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) occupiedShipNames(ctx context.Context) ([]string, error) {
	used := decode[[]string](r.snapshot, "_used_ship_names")
	occupied := make(map[string]bool, len(used))
	result := make([]string, 0, len(used)+len(r.stateCache.Ships))
	add := func(name string) {
		if name != "" && !occupied[name] {
			occupied[name] = true
			result = append(result, name)
		}
	}
	for _, name := range used {
		add(name)
	}
	for _, ship := range r.stateCache.Ships {
		add(ship.Ship.Name)
		for _, former := range ship.FormerNames {
			add(former.Name)
		}
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return nil, err
	}
	for _, mission := range missions {
		var data struct {
			BuildName string `json:"build_name"`
			Rename    *struct {
				Name string `json:"name"`
			} `json:"rename"`
		}
		if err := json.Unmarshal(mission.Data, &data); err != nil {
			return nil, err
		}
		add(data.BuildName)
		if data.Rename != nil {
			add(data.Rename.Name)
		}
	}
	return result, nil
}

// Reserve before staging: even a rejected build or rename never recycles a name.
// An invalidated capture defers allocation until the next authoritative refresh.
func (r *Runner) reserveShipName(ctx context.Context, subjectID string) (string, bool, error) {
	if r.capturedControl != r.publishedControl.Load() {
		return "", false, r.hold(ctx, subjectID, "stream control invalidated the captured state; authoritative resynchronization precedes staging")
	}
	if r.capturedFinancial != r.publishedFinancial.Load() {
		return "", false, r.hold(ctx, subjectID, "financial event invalidated the captured funds; authoritative resynchronization precedes staging")
	}
	used, err := r.occupiedShipNames(ctx)
	if err != nil {
		return "", false, err
	}
	name := names.Next(used)
	r.snapshot["_used_ship_names"] = encode(append(used, name))
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
		return "", false, err
	}
	return name, true, nil
}

func renameEligible(ship ShipState) bool {
	if ship.Ship.ID == "" || names.IsAssigned(ship.Ship.Name) || ship.Ship.Fate != "" {
		return false
	}
	switch ship.Ship.Status {
	case "berthed", "anchored_roadstead", "sailing":
		return true
	default:
		return false
	}
}

func (r *Runner) shipListedForSale(shipID string) bool {
	for _, listing := range decode[[]struct {
		ShipID string `json:"ship_id"`
		Status string `json:"status"`
	}](r.snapshot, "ship_listings") {
		if listing.ShipID == shipID && listing.Status == "open" {
			return true
		}
	}
	return false
}

func (r *Runner) driveShipRename(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute || !r.fresh("ships") || !r.fresh("ship_listings") || r.stateCache.Clock.Paused {
		return false, nil
	}
	if mission == nil {
		refusals := decode[map[string]shipRenameRefusal](r.snapshot, "_ship_rename_refusals")
		changed := false
		for id, refusal := range refusals {
			ship, exists := r.ship(id)
			if !exists || ship.Ship.Name != refusal.Name || ship.Ship.Status != refusal.Status {
				delete(refusals, id)
				changed = true
			}
		}
		if changed {
			r.snapshot["_ship_rename_refusals"] = encode(refusals)
			if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
				return false, err
			}
		}
		for _, ship := range r.stateCache.Ships {
			if !renameEligible(ship) || r.shipListedForSale(ship.Ship.ID) {
				continue
			}
			if refusal, exists := refusals[ship.Ship.ID]; exists && refusal.Name == ship.Ship.Name && refusal.Status == ship.Ship.Status {
				continue
			}
			name, ready, err := r.reserveShipName(ctx, shipRenameMission)
			if err != nil || !ready {
				return false, err
			}
			mission = &store.Mission{
				AccountID: r.account.ID, ShipID: shipRenameMission,
				Phase: "rename", Generation: uuid.NewString(),
			}
			*data = missionData{Rename: &shipRenameIntent{ShipID: ship.Ship.ID, Name: name}}
			if err := r.stage(ctx, mission, data, ship, "rename", "/v1/fleet/rename-ship", data.Rename); err != nil || data.Action == nil {
				return data.Action != nil, err
			}
			return true, r.decision(ctx, shipRenameMission, "rename", "acquired hull receives its reserved Pokemon catalog name",
				map[string]string{"ship_id": ship.Ship.ID, "former_name": ship.Ship.Name, "name": name})
		}
		return false, nil
	}
	if data.Rename == nil {
		return false, errors.New("persisted ship rename has no immutable naming intent")
	}
	if data.Action != nil {
		return false, nil
	}
	ship, exists := r.ship(data.Rename.ShipID)
	if mission.Phase == "done" {
		// A receipt can precede the next fleet capture. Keep its control row
		// until REST agrees so the old recorded name cannot stage a second rename.
		if exists && renameEligible(ship) && !shipRenameObserved(ship, data.Rename.Name) {
			return false, r.holdExpected(ctx, shipRenameMission, "completed ship rename waits for authoritative fleet name history")
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, nil
	}
	if !exists || !renameEligible(ship) || r.shipListedForSale(ship.Ship.ID) {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	return true, r.stage(ctx, mission, data, ship, "rename", "/v1/fleet/rename-ship", data.Rename)
}

func (r *Runner) shipRenameReady(mission *store.Mission, data *missionData) error {
	if mission.ShipID != shipRenameMission || data.Rename == nil || !names.IsAssigned(data.Rename.Name) ||
		data.Action.Path != "/v1/fleet/rename-ship" || !r.fresh("ships") || !r.fresh("ship_listings") {
		return errors.New("ship rename needs its immutable catalog name and fresh owned fleet and sale listings")
	}
	var request shipRenameIntent
	if json.Unmarshal(data.Action.Body, &request) != nil || request != *data.Rename {
		return errors.New("ship rename request disagrees with its durable naming intent")
	}
	ship, exists := r.ship(request.ShipID)
	if !exists || !renameEligible(ship) || ship.Ship.Name != data.Action.Before.Ship.Name || r.shipListedForSale(ship.Ship.ID) {
		return errors.New("ship rename requires its matching afloat, non-catalog-named ship without an open sale listing")
	}
	return nil
}

func validShipRenameResult(data *missionData, response json.RawMessage) bool {
	var result struct {
		ShipID      string                   `json:"ship_id"`
		Name        string                   `json:"name"`
		FormerNames []gameapi.FormerShipName `json:"former_names"`
	}
	if data.Rename == nil || json.Unmarshal(response, &result) != nil ||
		result.ShipID != data.Rename.ShipID || result.Name != data.Rename.Name || len(result.FormerNames) == 0 {
		return false
	}
	return result.FormerNames[0].Name == data.Action.Before.Ship.Name && !result.FormerNames[0].RenamedAt.IsZero()
}

// Reservation excluded this name from every recorded hull history. A matching
// dated former name proves the rename without comparing server and host clocks.
func shipRenameObserved(ship ShipState, name string) bool {
	if ship.Ship.Name == name {
		return true
	}
	for _, former := range ship.FormerNames {
		if former.Name == name && !former.RenamedAt.IsZero() {
			return true
		}
	}
	return false
}

func (r *Runner) reconcileShipRename(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if data.Rename == nil || !r.fresh("ships") {
		return false, nil
	}
	ship, exists := r.ship(data.Rename.ShipID)
	moot := !exists || ship.Ship.Fate != "" || ship.Ship.Status == "sunk"
	if !moot && !shipRenameObserved(ship, data.Rename.Name) {
		return false, nil
	}
	if action.State != store.ActionComplete {
		kind := "authoritative_ship_rename"
		if moot {
			kind = "authoritative_ship_rename_moot"
		}
		evidence := encode(map[string]any{
			"kind": kind, "ship_id": data.Rename.ShipID, "ship_missing": !exists,
			"ship": ship, "observed_at": r.meta["ships"].UpdatedAt,
		})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	if moot {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	return r.applyReconciliation(ctx, mission, data)
}
