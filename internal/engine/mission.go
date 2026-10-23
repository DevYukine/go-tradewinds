package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type freightObligation struct {
	ContractID      string     `json:"contract_id"`
	Commodity       string     `json:"commodity"`
	Quantity        int64      `json:"quantity"`
	Destination     string     `json:"destination"`
	Deadline        *time.Time `json:"deadline"`
	RewardPence     int64      `json:"reward_pence"`
	CollateralPence int64      `json:"collateral_pence"`
	TenderRefused   bool       `json:"tender_refused"`
	Settled         bool       `json:"settled"`
}

type missionData struct {
	Plan                      Plan                `json:"plan"`
	ProvenLane                string              `json:"proven_lane"`
	Action                    *durableAction      `json:"action,omitempty"`
	Sequence                  int64               `json:"sequence"`
	StartedAt                 time.Time           `json:"started_at"`
	StartedGameHours          int64               `json:"started_game_hours"`
	InitialLedgerIDs          map[string]bool     `json:"initial_ledger_ids"`
	Freight                   []freightObligation `json:"freight"`
	Eligible                  map[string]int64    `json:"eligible"`
	DepartedCargo             map[string]int64    `json:"departed_cargo"`
	BoughtQuantity            int64               `json:"bought_quantity"`
	DeliveredQuantity         int64               `json:"delivered_quantity"`
	BuyDone                   bool                `json:"buy_done"`
	ReturnJourney             bool                `json:"return_journey"`
	AcquisitionPence          int64               `json:"acquisition_pence"`
	ProceedsPence             int64               `json:"proceeds_pence"`
	FeesPence                 int64               `json:"fees_pence"`
	ProvisionsPence           int64               `json:"provisions_pence"`
	ExpensesPence             int64               `json:"expenses_pence"`
	CostsKnown                bool                `json:"costs_known"`
	LastActionID              string              `json:"last_action_id"`
	Settlement                *store.Settlement   `json:"settlement,omitempty"`
	RejectedCharterIDs        map[string]bool     `json:"rejected_charter_ids"`
	SettlementExpenseIDs      []string            `json:"settlement_expense_ids"`
	CooldownUntil             time.Time           `json:"cooldown_until"`
	BuildName                 string              `json:"build_name,omitempty"`
	BuildClass                string              `json:"build_class,omitempty"`
	BuiltShipID               string              `json:"built_ship_id,omitempty"`
	BuildPort                 string              `json:"build_port,omitempty"`
	TargetCannons             int64               `json:"target_cannons,omitempty"`
	BuildCostPence            int64               `json:"build_cost_pence"`
	GrowthWorkingCapitalPence int64               `json:"growth_working_capital_pence"`
}

func (r *Runner) ship(id string) (ShipState, bool) {
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.ID == id {
			return ship, true
		}
	}
	return ShipState{}, false
}

func (r *Runner) loadMission(ctx context.Context, shipID string) (*store.Mission, missionData, error) {
	mission, err := r.db.GetMission(ctx, r.account.ID, shipID)
	if err != nil || mission == nil {
		return mission, missionData{}, err
	}
	var data missionData
	if err := json.Unmarshal(mission.Data, &data); err != nil {
		return nil, data, fmt.Errorf("invalid durable mission %s: %w", shipID, err)
	}
	return mission, data, nil
}

func (r *Runner) work(ctx context.Context) (bool, error) {
	r.nextDeadline = time.Time{}
	for _, key := range []string{"ships", "contracts", "vaults", "clock", "catalog", "factors", "warehouses", "orders", "fills", "ledger", "rates", "settings"} {
		if !r.fresh(key) {
			return false, r.hold(ctx, "", "mutation gate: missing, incomplete or stale "+key)
		}
	}
	if r.stateCache.Clock.Paused {
		return false, r.hold(ctx, "", "world paused; observation continues without mutations")
	}
	// Restore every durable plan before replaying even one request. An orphaned
	// pending request blocks all new account mutations rather than being guessed.
	missions := make(map[string]*store.Mission)
	dataByShip := make(map[string]missionData)
	ids := make([]string, 0, len(r.stateCache.Ships)+2)
	for _, ship := range r.stateCache.Ships {
		ids = append(ids, ship.Ship.ID)
	}
	ids = append(ids, "__treasury", "__growth")
	attached := make(map[string]bool)
	for _, id := range ids {
		mission, data, err := r.loadMission(ctx, id)
		if err != nil {
			return false, err
		}
		if mission != nil {
			missions[id] = mission
			r.wakeBy(data.CooldownUntil)
			for _, obligation := range data.Freight {
				if obligation.Deadline != nil {
					r.wakeBy(*obligation.Deadline)
				}
			}
			changed := false
			for _, contract := range r.activeFreight(id) {
				known := false
				for _, obligation := range data.Freight {
					known = known || obligation.ContractID == contract.ID
				}
				if !known {
					data.Freight = append(data.Freight, obligationFromContract(contract))
					data.CostsKnown = false
					changed = true
				}
			}
			if changed {
				if err := r.saveMission(ctx, mission, &data); err != nil {
					return false, err
				}
			}
			dataByShip[id] = data
			for _, ledgerID := range data.SettlementExpenseIDs {
				if owner := r.expenseOwners[ledgerID]; owner != "" && owner != mission.Generation {
					return false, fmt.Errorf("ledger expense %s has conflicting durable mission ownership", ledgerID)
				}
				r.expenseOwners[ledgerID] = mission.Generation
			}
			r.snapshot["_expense_allocations"] = encode(r.expenseOwners)
			if data.Action != nil {
				attached[data.Action.ID] = true
			}
		}
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	for _, action := range pending {
		if !attached[action.ID] {
			return false, r.hold(ctx, "", "orphaned unresolved journal request "+action.ID+"; original mission must be restored before any retry")
		}
	}
	for _, id := range ids {
		data := dataByShip[id]
		if data.Action != nil {
			if time.Now().Before(data.CooldownUntil) {
				return false, r.hold(ctx, id, "unresolved action waiting for persisted retry cooldown")
			}
			return r.dispatch(ctx, missions[id], &data)
		}
	}
	if mission := missions["__growth"]; mission != nil {
		data := dataByShip["__growth"]
		changed, err := r.driveGrowth(ctx, mission, &data)
		if changed || err != nil {
			return changed, err
		}
	}
	if mission := missions["__treasury"]; mission != nil {
		data := dataByShip["__treasury"]
		if time.Now().Before(data.CooldownUntil) {
			if err := r.hold(ctx, "", "treasury waits for persisted retry-after/replanning cooldown"); err != nil {
				return false, err
			}
		} else {
			// An attached or orphaned unresolved action was handled above.
			// Release completed or legacy rejected control intents for a fresh
			// treasury plan rather than holding every account mutation forever.
			return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
		}
	}
	if r.opts.Execute && missions["__treasury"] == nil {
		changed, err := r.manageTreasury(ctx)
		if changed || err != nil {
			return changed, err
		}
	}
	if r.opts.Execute && missions["__growth"] == nil && missions["__treasury"] == nil {
		changed, err := r.considerGrowth(ctx)
		if changed || err != nil {
			return changed, err
		}
	}
	for _, ship := range r.stateCache.Ships {
		if growth := dataByShip["__growth"]; growth.BuiltShipID != "" && growth.BuiltShipID == ship.Ship.ID {
			continue
		}
		mission := missions[ship.Ship.ID]
		data := dataByShip[ship.Ship.ID]
		if mission != nil {
			changed, err := r.driveMission(ctx, ship, mission, &data)
			if changed || err != nil {
				return changed, err
			}
			continue
		}
		if ship.Ship.Status == "sunk" {
			continue
		}
		if ship.Ship.Status == "sailing" || ship.CargoUsedTons > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
			if r.opts.Execute {
				return r.recoverShip(ctx, ship)
			}
			if err := r.hold(ctx, ship.Ship.ID, "observe mode: in-flight/cargo obligations recorded; no recovery mutation"); err != nil {
				return false, err
			}
			continue
		}
		if r.opts.Execute {
			changed, err := r.maintain(ctx, ship, nil, nil)
			if changed || err != nil {
				return changed, err
			}
		}
		plans := PlanVoyages(r.planningInput(ship))
		for i, plan := range plans {
			kind := "rejected"
			if plan.Accepted {
				kind = "ranked"
			}
			if i == 0 && plan.Accepted {
				kind = "selected"
			}
			if err := r.decision(ctx, ship.Ship.ID, kind+":"+plan.Kind+":"+plan.Destination+":"+plan.Commodity+":"+plan.ContractID, plan.Reason, plan); err != nil {
				return false, err
			}
		}
		if len(plans) > 0 && plans[0].Accepted {
			if !r.opts.Execute {
				if err := r.hold(ctx, ship.Ship.ID, "observe mode: profitable measured plan is not executed"); err != nil {
					return false, err
				}
				continue
			}
			return r.startVoyage(ctx, ship, plans[0])
		}
		reason := "no executable purchase-and-voyage alternative from " + ship.Ship.CurrentPortID
		if len(plans) > 0 {
			best := plans[0]
			reason = fmt.Sprintf("no profitable executable voyage from %s; best evaluated alternative %dt %s to %s: %s", ship.Ship.CurrentPortID, best.Quantity, best.Commodity, best.Destination, best.Reason)
			if best.Economics.GameDays > 0 {
				reason += fmt.Sprintf(" (expected cash net %dp; %.0fp per game day)", best.Economics.ExpectedNetPence, best.Economics.NetPencePerGameDay)
			}
		}
		if err := r.hold(ctx, ship.Ship.ID, reason); err != nil {
			return false, err
		}
		if r.opts.Execute && ship.Ship.Status == "berthed" && unlocked(ship, time.Now()) {
			return r.startMaintenance(ctx, ship, "offshore", "/v1/fleet/offshore", map[string]any{"ship_id": ship.Ship.ID})
		}
	}
	return false, r.decision(ctx, "", "excluded_liabilities", "futures, insurance underwriting, marques, patrols and extra companies rejected: no measured positive incremental return with modeled liabilities; no speculative side effects", nil)
}

func unlocked(ship ShipState, now time.Time) bool {
	return ship.Ship.TurnaroundUnlocksAt == nil || !ship.Ship.TurnaroundUnlocksAt.After(now)
}

func (r *Runner) startVoyage(ctx context.Context, ship ShipState, plan Plan) (bool, error) {
	mission := &store.Mission{
		AccountID:   r.account.ID,
		ShipID:      ship.Ship.ID,
		Phase:       "provision",
		Destination: plan.Destination,
		Commodity:   plan.Commodity,
		ContractID:  plan.ContractID,
		Quantity:    plan.Quantity,
		PricePence:  plan.BuyLimitPence,
		Generation:  uuid.NewString(),
	}
	data := missionData{
		Plan:             plan,
		ProvenLane:       laneKey(plan),
		StartedAt:        time.Now().UTC(),
		StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
		InitialLedgerIDs: r.ledgerIDs(),
		CostsKnown:       true,
		Eligible:         make(map[string]int64),
	}
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) recoverShip(ctx context.Context, ship ShipState) (bool, error) {
	destination := ship.Ship.CurrentPortID
	phase := "arrival"
	if ship.Ship.Status == "sailing" {
		phase = "sailing"
		if len(ship.RouteLegs) == 0 {
			return false, r.hold(ctx, ship.Ship.ID, "in-flight ship has no authoritative route; cannot infer its destination")
		}
		destination = ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
	}
	mission := &store.Mission{
		AccountID:   r.account.ID,
		ShipID:      ship.Ship.ID,
		Phase:       phase,
		Destination: destination,
		Generation:  uuid.NewString(),
	}
	data := missionData{
		Plan:             Plan{Kind: "recovery", ShipID: ship.Ship.ID, Destination: destination},
		StartedAt:        time.Now().UTC(),
		StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
		CostsKnown:       false,
		Eligible:         make(map[string]int64),
		InitialLedgerIDs: r.ledgerIDs(),
	}
	for _, contract := range r.activeFreight(ship.Ship.ID) {
		data.Freight = append(data.Freight, obligationFromContract(contract))
	}
	if ship.Ship.Status != "sailing" {
		stops := freightStops(data.Freight)
		if len(stops) > 0 && stops[0].Destination != ship.Ship.CurrentPortID {
			mission.Destination = stops[0].Destination
			data.Plan.Destination = stops[0].Destination
			mission.Phase = "depart"
			data.Plan.Origin = ship.Ship.CurrentPortID
		}
	}
	return true, r.saveMission(ctx, mission, &data)
}

func obligationFromContract(contract Contract) freightObligation {
	var metadata struct {
		DeliveryDeadline *time.Time `json:"delivery_deadline"`
	}
	_ = json.Unmarshal(contract.Metadata, &metadata)
	return freightObligation{
		ContractID:      contract.ID,
		Commodity:       contract.CommodityID,
		Quantity:        contract.QuantityRequired,
		Destination:     contract.DestPortID,
		Deadline:        metadata.DeliveryDeadline,
		RewardPence:     contract.RewardPence,
		CollateralPence: contract.CollateralPence,
	}
}

func (r *Runner) activeFreight(shipID string) []Contract {
	var result []Contract
	for _, contract := range r.stateCache.Contracts {
		if contract.ContractType == "freight" && contract.Status == "active" && contract.CarrierCompanyID == r.account.CompanyID && contract.CarrierShipID == shipID {
			result = append(result, contract)
		}
	}
	return result
}

func (r *Runner) driveMission(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute {
		return false, r.hold(ctx, ship.Ship.ID, "observe mode: durable mission retained without mutation")
	}
	if time.Now().Before(data.CooldownUntil) {
		return false, r.hold(ctx, ship.Ship.ID, "mission waiting for persisted retry cooldown")
	}
	if ship.Ship.Status == "sunk" {
		data.CostsKnown = false
		return r.finishMission(ctx, ship, mission, data)
	}
	if ship.Ship.ConvoyID != "" {
		return false, r.hold(ctx, ship.Ship.ID, "ship is bound to a convoy; independent mission navigation is unsafe")
	}
	if mission.Phase == "maintenance_done" {
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if mission.Phase == "settling" || mission.Phase == "settled" {
		return r.finishMission(ctx, ship, mission, data)
	}
	if mission.Phase == "sailing" || mission.Phase == "return_sailing" {
		if ship.Ship.Status == "sailing" {
			return false, r.hold(ctx, ship.Ship.ID, "actual fleet confirms voyage in flight; waiting for authoritative arrival")
		}
		destination := data.Plan.Destination
		if mission.Phase == "return_sailing" {
			destination = data.Plan.Origin
		}
		if ship.Ship.CurrentPortID != destination {
			return false, r.hold(ctx, ship.Ship.ID, "mission destination and actual fleet disagree; no stale-plan dispatch")
		}
		if mission.Phase == "return_sailing" {
			return r.finishMission(ctx, ship, mission, data)
		}
		data.Eligible = arrivalEligibility(data.DepartedCargo, ship.CargoManifest, sealedReservations(data.Freight), data.Plan.Origin != "" && data.Plan.Origin != destination)
		mission.Phase = "arrival"
		return true, r.saveMission(ctx, mission, data)
	}
	deliveryReady := false
	if ship.Ship.CurrentPortID == data.Plan.Destination {
		for _, obligation := range data.Freight {
			if !obligation.Settled && obligation.Destination == ship.Ship.CurrentPortID {
				deliveryReady = true
			}
		}
		for _, contract := range r.stateCache.Contracts {
			if contract.ContractType == "charter" && contract.Status == "open" && !contract.Stale &&
				contract.OriginPortID == ship.Ship.CurrentPortID && contract.RemainingQuantity > 0 &&
				data.Eligible[contract.CommodityID] > 0 && !data.RejectedCharterIDs[contract.ID] &&
				(contract.ExpiresAt == nil || contract.ExpiresAt.After(time.Now())) {
				deliveryReady = true
			}
		}
	}
	// Tender at the roadstead before joining a potentially long berth queue
	// for maintenance. A refused tender performs its idle maintenance below.
	if ship.Ship.Status != "sailing" && mission.Phase != "provision" && !deliveryReady {
		changed, err := r.maintain(ctx, ship, mission, data)
		if changed || err != nil {
			return changed, err
		}
	}
	if mission.Phase == "arrival" || mission.Phase == "tender_wait" {
		return r.deliverAtArrival(ctx, ship, mission, data)
	}
	if mission.Phase == "return" || mission.Phase == "return_depart" {
		if data.Plan.Origin == "" || data.Plan.Kind == "recovery" {
			return r.finishMission(ctx, ship, mission, data)
		}
		data.ReturnJourney = true
		mission.Destination = data.Plan.Origin
		return r.prepareDeparture(ctx, ship, mission, data)
	}
	if mission.Phase == "provision" {
		if ship.Ship.Status != "berthed" {
			return r.dockForMission(ctx, ship, mission, data)
		}
		if ship.Stores.ProvisionsTons+1e-9 < data.Plan.TargetStoresTons {
			return r.provisionMission(ctx, ship, mission, data, data.Plan.TargetStoresTons)
		}
		mission.Phase = "buy"
		return true, r.saveMission(ctx, mission, data)
	}
	if mission.Phase == "buy" {
		if data.Plan.Kind == "freight" || data.BuyDone {
			mission.Phase = "freight"
			return true, r.saveMission(ctx, mission, data)
		}
		if ship.Ship.Status != "berthed" {
			return r.dockForMission(ctx, ship, mission, data)
		}
		port := ship.Ship.CurrentPortID
		if !r.fresh("books/" + port) {
			return false, r.hold(ctx, ship.Ship.ID, "cargo purchase requires fresh local depth and customs")
		}
		input := r.planningInput(ship)
		origin, exists := findPort(input.Catalog, port)
		if !exists || data.Plan.Quantity <= 0 || data.Plan.BuyLimitPence <= 0 || data.Plan.BuyLimitPence > math.MaxInt64/data.Plan.Quantity {
			return false, r.hold(ctx, ship.Ship.ID, "persisted procurement lacks a valid port or integer-pence funding bound")
		}
		worstPurchase := data.Plan.Quantity * data.Plan.BuyLimitPence
		worstCustoms := ceilBPS(worstPurchase, r.stateCache.Markets[port].CustomsBPS) + data.Plan.Quantity
		futureBills := data.Plan.Economics.BerthDuesPence + data.Plan.Economics.UpkeepPence + data.Plan.Economics.CollateralPence + data.Plan.Economics.TollPence
		if input.Balances[origin.BankingRegion]-worstPurchase-worstCustoms-futureBills < input.ReserveByRegion[origin.BankingRegion] {
			return r.unwindProcurement(ctx, ship, mission, data, "fresh funds cannot preserve regional reserve at the persisted IOC limit; release the unbought plan")
		}
		// A new request is permitted here only before this stage has ever had a
		// successful IOC. A partial fill permanently completes this buy stage.
		body := orderBody(ship, data.Plan.Commodity, "bid", data.Plan.Quantity, data.Plan.BuyLimitPence, false)
		return true, r.stage(ctx, mission, data, ship, "buy", "/v1/market/place-order", body)
	}
	if mission.Phase == "freight" {
		for _, id := range data.Plan.FreightIDs {
			accepted := false
			for _, obligation := range data.Freight {
				accepted = accepted || obligation.ContractID == id
			}
			if accepted {
				continue
			}
			contract, exists := r.contract(id)
			if !exists || contract.Status != "open" || contract.Stale || contract.DestPortID != data.Plan.Destination {
				removeFreightCandidate(&data.Plan, id)
				if err := r.decision(ctx, ship.Ship.ID, "freight-reselection", "unaccepted freight changed; retain actual cargo and re-evaluate the remaining voyage", map[string]any{"contract_id": id}); err != nil {
					return false, err
				}
				return true, r.saveMission(ctx, mission, data)
			}
			if ship.Ship.Status != "berthed" {
				return r.dockForMission(ctx, ship, mission, data)
			}
			if contract.QuantityRequired > ship.EffectiveHoldCapacityTons-ship.CargoUsedTons {
				removeFreightCandidate(&data.Plan, id)
				if err := r.decision(ctx, ship.Ship.ID, "freight-reselection", "unaccepted freight no longer fits the actual hold; no sealed obligation was created", map[string]any{"contract_id": id}); err != nil {
					return false, err
				}
				return true, r.saveMission(ctx, mission, data)
			}
			if contract.CollateralPence > 0 {
				input := r.planningInput(ship)
				port, exists := findPort(input.Catalog, ship.Ship.CurrentPortID)
				if !exists || input.Balances[port.BankingRegion]-contract.CollateralPence < input.ReserveByRegion[port.BankingRegion] {
					removeFreightCandidate(&data.Plan, id)
					if err := r.decision(ctx, ship.Ship.ID, "freight-reselection", "unaccepted freight collateral cannot preserve the fresh regional reserve; retain actual cargo and accepted obligations", map[string]any{"contract_id": id}); err != nil {
						return false, err
					}
					return true, r.saveMission(ctx, mission, data)
				}
			}
			return true, r.stage(ctx, mission, data, ship, "accept-freight", "/v1/contract/accept-freight", map[string]any{"contract_id": id, "ship_id": ship.Ship.ID})
		}
		mission.Phase = "depart"
		return true, r.saveMission(ctx, mission, data)
	}
	if mission.Phase == "depart" {
		return r.prepareDeparture(ctx, ship, mission, data)
	}
	return false, r.hold(ctx, ship.Ship.ID, "unknown persisted mission phase: "+mission.Phase)
}

func orderBody(ship ShipState, commodity, side string, quantity, price int64, stores bool) map[string]any {
	body := map[string]any{"ship_id": ship.Ship.ID, "port_id": ship.Ship.CurrentPortID, "commodity_id": commodity, "side": side, "quantity": quantity, "price_pence": price, "time_in_force": "ioc"}
	if stores {
		body["into_stores"] = true
	}
	return body
}

func (r *Runner) contract(id string) (Contract, bool) {
	for _, contract := range r.stateCache.Contracts {
		if contract.ID == id {
			return contract, true
		}
	}
	return Contract{}, false
}

func (r *Runner) dockForMission(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if ship.Ship.Status != "anchored_roadstead" || ship.Ship.DockQueuedAt != nil {
		return false, r.hold(ctx, ship.Ship.ID, "berth-only operation waits for authoritative docking; already queued or unavailable")
	}
	return true, r.stage(ctx, mission, data, ship, "dock", "/v1/fleet/dock", map[string]any{"ship_id": ship.Ship.ID})
}

func (r *Runner) provisionMission(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, target float64) (bool, error) {
	if ship.Ship.Status != "berthed" {
		return r.dockForMission(ctx, ship, mission, data)
	}
	quantity := max(0, int64(math.Ceil(target-ship.Stores.ProvisionsTons)))
	if quantity == 0 {
		return false, nil
	}
	port := ship.Ship.CurrentPortID
	if !r.fresh("books/" + port) {
		return false, r.hold(ctx, ship.Ship.ID, "provisions need fresh local depth")
	}
	book := r.stateCache.Markets[port]
	entry := bookCommodity(book, "provisions")
	if entry == nil || entry.Depth == nil {
		return false, r.hold(ctx, ship.Ship.ID, "provisions have no executable local asks")
	}
	fill, err := WalkDepth(entry.Depth.Asks, quantity, true)
	if err != nil {
		return false, r.hold(ctx, ship.Ship.ID, "provisions: "+err.Error())
	}
	input := r.planningInput(ship)
	catalogPort, exists := findPort(input.Catalog, port)
	if fill.LimitPence > math.MaxInt64/quantity {
		return false, r.hold(ctx, ship.Ship.ID, "store order exceeds integer-pence funding bounds")
	}
	worstCost := quantity * fill.LimitPence
	if !exists || input.Balances[catalogPort.BankingRegion]-worstCost-ceilBPS(worstCost, book.CustomsBPS)-quantity < input.ReserveByRegion[catalogPort.BankingRegion] {
		return false, r.hold(ctx, ship.Ship.ID, "cannot replenish stores without spending protected regional bills reserve")
	}
	if quantity > ship.EffectiveHoldCapacityTons-ship.CargoUsedTons {
		return false, r.hold(ctx, ship.Ship.ID, "safe stores do not fit beside actual cargo and sealed reservations")
	}
	return true, r.stage(ctx, mission, data, ship, "provision", "/v1/market/place-order", orderBody(ship, "provisions", "bid", quantity, fill.LimitPence, true))
}

func (r *Runner) prepareDeparture(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if ship.Ship.Status == "sailing" {
		data.CostsKnown = false
		mission.Phase = "sailing"
		if data.ReturnJourney {
			mission.Phase = "return_sailing"
		}
		return true, r.saveMission(ctx, mission, data)
	}
	if !unlocked(ship, time.Now()) {
		return false, r.hold(ctx, ship.Ship.ID, "turnaround lock active; purchases are complete and will not be repeated")
	}
	hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	if !exists {
		return false, r.hold(ctx, ship.Ship.ID, "departure hull is absent from catalog")
	}
	key := routeKey(ship.Ship.CurrentPortID, mission.Destination, hull.ID)
	route, exists := r.stateCache.Routes[key]
	if !exists || !r.fresh("routes/"+key) || !r.fresh("zones") {
		return false, r.hold(ctx, ship.Ship.ID, "departure requires a current route and live hazard observations")
	}
	if err := ValidateRoute(route, hull, r.stateCache.Catalog.SeaZones); err != nil {
		return false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	// Always recheck the live stores immediately before departure, including
	// idle consumption, partial IOC fills and hazard losses on earlier legs.
	target := route.SafeProvisionsTons + 1
	if ship.Stores.ProvisionsTons+1e-9 < target {
		return r.provisionMission(ctx, ship, mission, data, target)
	}
	if err := r.validateFreightDeparture(ship, mission.Destination, data, hull); err != nil {
		return false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	if !data.ReturnJourney && data.Plan.Kind != "recovery" {
		input := r.planningInput(ship)
		proceeds, reason := acquiredRevenue(input, data.Plan, data.BoughtQuantity, data.Freight)
		if acquiredNet(data, proceeds) <= 0 {
			if reason == "" {
				reason = "actual IOC fill has non-positive expected cash net after current reward evidence and measured operating costs"
			}
			return r.unwindProcurement(ctx, ship, mission, data, reason)
		}
	}
	if data.Plan.Kind != "recovery" && !data.ReturnJourney && data.BoughtQuantity == 0 && len(data.Freight) == 0 {
		return r.finishMission(ctx, ship, mission, data)
	}
	return true, r.stage(ctx, mission, data, ship, "depart", "/v1/fleet/depart", map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": mission.Destination})
}

func sealedReservations(obligations []freightObligation) map[string]int64 {
	reserved := make(map[string]int64)
	for _, obligation := range obligations {
		if !obligation.Settled {
			reserved[obligation.Commodity] += obligation.Quantity
		}
	}
	return reserved
}

// arrivalEligibility is established only by an evidenced inter-port voyage.
// An unknown post-loss allocation remains reserved for sealed freight first.
func arrivalEligibility(departed, actual, sealed map[string]int64, evidenced bool) map[string]int64 {
	eligible := make(map[string]int64)
	if !evidenced {
		return eligible
	}
	for commodity, quantity := range actual {
		eligible[commodity] = max(0, min(quantity, departed[commodity])-sealed[commodity])
	}
	return eligible
}

func (r *Runner) deliverAtArrival(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if ship.Ship.CurrentPortID != data.Plan.Destination {
		return false, r.hold(ctx, ship.Ship.ID, "delivery requires the actual destination port")
	}
	for i := range data.Freight {
		obligation := &data.Freight[i]
		if obligation.Settled {
			continue
		}
		contract, exists := r.contract(obligation.ContractID)
		if exists && contract.Status != "active" {
			// Terminal REST state proves the seal is gone, but cannot invent an
			// unobserved payment. Exclude this recovery from growth evidence.
			obligation.Settled = true
			data.CostsKnown = false
			if contract.Status == "defaulted" {
				data.ExpensesPence += obligation.CollateralPence
			}
			return true, r.saveMission(ctx, mission, data)
		}
		if obligation.Destination != ship.Ship.CurrentPortID {
			continue
		}
		if obligation.TenderRefused {
			changed, err := r.maintain(ctx, ship, mission, data)
			if changed || err != nil {
				return changed, err
			}
			return false, r.hold(ctx, ship.Ship.ID, "freight tender refused for issuer capacity: remain at destination, preserve seal and await breach/completion/default")
		}
		return true, r.stage(ctx, mission, data, ship, "deliver-freight", "/v1/contract/deliver-freight", map[string]any{"contract_id": obligation.ContractID, "ship_id": ship.Ship.ID})
	}
	reserved := sealedReservations(data.Freight)
	for commodity, quantity := range ship.CargoManifest {
		free := max(0, quantity-reserved[commodity])
		if free == 0 {
			continue
		}
		eligible := min(free, data.Eligible[commodity])
		var charter *Contract
		for i := range r.stateCache.Contracts {
			contract := &r.stateCache.Contracts[i]
			if data.RejectedCharterIDs[contract.ID] {
				continue
			}
			if contract.ContractType == "charter" &&
				contract.OriginPortID == ship.Ship.CurrentPortID &&
				contract.CommodityID == commodity &&
				contract.Status == "open" &&
				!contract.Stale &&
				contract.RemainingQuantity > 0 &&
				contract.RewardPerTonPence != nil &&
				eligible > 0 &&
				(contract.ExpiresAt == nil || contract.ExpiresAt.After(time.Now())) {
				if charter == nil || *contract.RewardPerTonPence > *charter.RewardPerTonPence {
					charter = contract
				}
			}
		}
		book := r.stateCache.Markets[ship.Ship.CurrentPortID]
		entry := bookCommodity(book, commodity)
		var bid DepthFill
		if r.fresh("books/"+ship.Ship.CurrentPortID) && entry != nil && entry.Depth != nil {
			bid, _ = WalkDepth(entry.Depth.Bids, free, false)
		}
		if charter != nil {
			amount := min(eligible, charter.RemainingQuantity)
			charterPay := amount * *charter.RewardPerTonPence
			var marketPay int64
			if entry != nil && entry.Depth != nil {
				if fill, err := WalkDepth(entry.Depth.Bids, amount, false); err == nil {
					marketPay = fill.GrossPence
				}
			}
			if charterPay >= marketPay {
				return true, r.stage(ctx, mission, data, ship, "deliver-charter", "/v1/contract/deliver-charter", map[string]any{"contract_id": charter.ID, "ship_id": ship.Ship.ID, "quantity": amount})
			}
		}
		if ship.Ship.Status != "berthed" {
			return r.dockForMission(ctx, ship, mission, data)
		}
		if bid.Quantity == 0 || bid.LimitPence <= 0 {
			return false, r.hold(ctx, ship.Ship.ID, "unsealed cargo cannot be sold without measured executable local bid depth")
		}
		return true, r.stage(ctx, mission, data, ship, "sell", "/v1/market/place-order", orderBody(ship, commodity, "ask", min(free, bid.Quantity), bid.LimitPence, false))
	}
	if stops := freightStops(data.Freight); len(stops) > 0 {
		mission.Destination = stops[0].Destination
		data.Plan.Destination = stops[0].Destination
		mission.Phase = "depart"
		return true, r.saveMission(ctx, mission, data)
	}
	if data.Plan.Origin != "" && data.Plan.Origin != ship.Ship.CurrentPortID && data.Plan.Kind != "recovery" {
		mission.Phase = "return"
		return true, r.saveMission(ctx, mission, data)
	}
	return r.finishMission(ctx, ship, mission, data)
}

func (r *Runner) ledgerIDs() map[string]bool {
	ids := make(map[string]bool)
	for _, row := range decode[[]struct {
		ID string `json:"id"`
	}](r.snapshot, "ledger") {
		ids[row.ID] = true
	}
	return ids
}

func (r *Runner) finishMission(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if len(r.activeFreight(ship.Ship.ID)) > 0 {
		return false, r.hold(ctx, ship.Ship.ID, "cannot settle mission while actual active sealed freight remains")
	}
	if data.Plan.Kind == "recovery" || data.Plan.Kind == "maintenance" {
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if mission.Phase == "settled" {
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if data.LastActionID == "" {
		if data.AcquisitionPence != 0 || data.ProceedsPence != 0 || data.FeesPence != 0 || data.ProvisionsPence != 0 || data.ExpensesPence != 0 {
			return false, r.hold(ctx, ship.Ship.ID, "actual cash movement lacks a durable terminal action; cannot fabricate a settlement")
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if data.Settlement == nil {
		// Persist the complete immutable settlement before inserting it. A
		// crash between those writes must replay identical timestamps/costs.
		bills, expenseIDs := r.claimBillsSince(data, mission.Generation)
		data.ExpensesPence += bills
		data.SettlementExpenseIDs = expenseIDs
		// Hour snapshots bound elapsed time; adding an hour avoids overstating
		// realized throughput by rounding a partial game hour downward.
		gameSeconds := max(3_600, (r.stateCache.Clock.Calendar.TotalGameHours-data.StartedGameHours+1)*3_600)
		data.Settlement = &store.Settlement{
			ID:               mission.Generation,
			AccountID:        r.account.ID,
			ShipID:           ship.Ship.ID,
			MissionID:        mission.Generation,
			ActionID:         data.LastActionID,
			Lane:             data.ProvenLane,
			AcquisitionPence: data.AcquisitionPence,
			ProceedsPence:    data.ProceedsPence,
			FeesPence:        data.FeesPence,
			ProvisionsPence:  data.ProvisionsPence,
			ExpensesPence:    data.ExpensesPence,
			QuantityTons:     data.DeliveredQuantity,
			GameSeconds:      gameSeconds,
			Kind:             data.Plan.Kind,
			Origin:           data.Plan.Origin,
			Destination:      data.Plan.Destination,
			Commodity:        data.Plan.Commodity,
			CostsKnown:       data.CostsKnown,
			StartedAt:        data.StartedAt,
			CompletedAt:      time.Now().UTC(),
		}
		mission.Phase = "settling"
		return true, r.saveMission(ctx, mission, data)
	}
	if err := r.db.PutSettlement(ctx, data.Settlement); err != nil {
		return false, err
	}
	// Settlements, rather than potentially repeated metric posts, are the
	// permanent source of realized P&L and growth evidence.
	mission.Phase = "settled"
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	// The phase save suppresses ordinary replay logs. A crash between durable
	// writes and console output can still omit a line; this is not an outbox.
	if entry := r.log.Check(zap.InfoLevel, "mission settlement recorded"); entry != nil {
		settlement := data.Settlement
		fields := []zap.Field{
			zap.String("settlement_id", settlement.ID),
			zap.String("ship_id", settlement.ShipID),
			zap.String("lane", settlement.Lane),
			zap.String("kind", settlement.Kind),
			zap.Int64("quantity_tons", settlement.QuantityTons),
			zap.Int64("proceeds_pence", settlement.ProceedsPence),
			zap.Int64("acquisition_pence", settlement.AcquisitionPence),
			zap.Int64("fees_pence", settlement.FeesPence),
			zap.Int64("provisions_pence", settlement.ProvisionsPence),
			zap.Int64("expenses_pence", settlement.ExpensesPence),
			zap.Bool("costs_known", settlement.CostsKnown),
		}
		if settlement.CostsKnown {
			net := settlement.ProceedsPence - settlement.AcquisitionPence - settlement.FeesPence - settlement.ProvisionsPence - settlement.ExpensesPence
			fields = append(fields, zap.Int64("net_profit_pence", net))
		}
		entry.Write(fields...)
	}
	return true, nil
}

func laneKey(plan Plan) string {
	return plan.Origin + "|" + plan.Destination + "|" + plan.Commodity + "|" + strings.Split(plan.Kind, "+")[0]
}

// claimBillsSince assigns each actual overhead debit exactly once across
// overlapping missions. The claim is included in the durable settling mission
// before it is published to the snapshot and before the settlement is inserted.
func (r *Runner) claimBillsSince(data *missionData, generation string) (int64, []string) {
	owned := make(map[string]bool)
	for _, vault := range r.stateCache.Vaults {
		owned[vault.AccountID] = true
	}
	var amount int64
	ids := make([]string, 0)
	for _, row := range decode[[]struct {
		ID     string `json:"id"`
		Debit  string `json:"debit_account_id"`
		Credit string `json:"credit_account_id"`
		Amount int64  `json:"amount_pence"`
		Reason string `json:"reason"`
	}](r.snapshot, "ledger") {
		if data.InitialLedgerIDs[row.ID] || !owned[row.Debit] || owned[row.Credit] || r.expenseOwners[row.ID] != "" {
			continue
		}
		for _, reason := range []string{"upkeep", "rent", "berth", "toll"} {
			if strings.Contains(row.Reason, reason) {
				amount += row.Amount
				ids = append(ids, row.ID)
				r.expenseOwners[row.ID] = generation
				break
			}
		}
	}
	r.snapshot["_expense_allocations"] = encode(r.expenseOwners)
	return amount, ids
}

var errUninterpretableResult = errors.New("committed action response is incomplete; authoritative reconciliation required")
