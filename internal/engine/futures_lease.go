package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

type futuresLeaseReservation struct {
	ReceivingTons int64 `json:"receiving_tons"`
	TargetTons    int64 `json:"target_tons"`
}

type futuresLeaseHolding struct {
	BaselineTons int64 `json:"baseline_tons"`
	TargetTons   int64 `json:"target_tons"`
}

type futuresLeaseChange struct {
	PortID       string `json:"port_id"`
	BeforeTons   int64  `json:"before_tons"`
	TargetTons   int64  `json:"target_tons"`
	BaselineTons int64  `json:"baseline_tons"`
	IntentID     string `json:"intent_id,omitempty"`
	ActionID     string `json:"action_id"`
}

func futuresLeaseStage(stage string) bool {
	return stage == "futures-resize-lease" || stage == "futures-release-lease"
}

func (change futuresLeaseChange) body() any {
	return struct {
		PortID string `json:"port_id"`
		Target int64  `json:"target_leased_tonnage"`
	}{change.PortID, change.TargetTons}
}

func (r *Runner) futuresLeaseAvailable(port string) int64 {
	var detail struct {
		Capacity *int64 `json:"total_warehouse_cap"`
		Leased   *int64 `json:"total_leased_warehouse"`
	}
	ports := decode[map[string]json.RawMessage](r.snapshot, "ports")
	if !r.fresh("ports/"+port) || json.Unmarshal(ports[port], &detail) != nil ||
		detail.Capacity == nil || detail.Leased == nil || *detail.Capacity < 0 || *detail.Leased < 0 {
		return 0
	}
	return max(0, *detail.Capacity-*detail.Leased)
}

// Admission reserves rentable space; acceptance requires the rented capacity.
func (r *Runner) futuresReceivingCapacity(rows []store.FuturesIntent, port string) int64 {
	return max(0, r.futuresReceivingSpace(rows, port)+r.futuresLeaseAvailable(port))
}

func (r *Runner) reserveFuturesLease(intent *store.FuturesIntent, admission *futuresAdmission, rows []store.FuturesIntent, receiving int64) bool {
	if receiving > r.futuresReceivingCapacity(rows, intent.PortID) {
		return false
	}
	reserved := futuresReceivingReserved(rows, intent.PortID)
	warehouse, _ := r.futuresWarehouse(intent.PortID, "")
	occupied := max(warehouse.UsedTonnage+warehouse.ReservedTonnage, warehouse.LeasedTonnage-warehouse.FreeTonnage)
	admission.Lease = &futuresLeaseReservation{ReceivingTons: receiving,
		TargetTons: max(warehouse.LeasedTonnage, occupied+reserved+receiving)}
	// Rent is a labelled 50p/ton/game-day carry estimate, not an API quote.
	// Protect all of that allowance as cash, even for an already rented lot.
	intent.ReservedCashPence += admission.RentPence
	intent.ExposurePence += admission.RentPence
	return true
}

func (r *Runner) futuresCapacityReady(ctx context.Context, selected []store.FuturesIntent) error {
	all, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	for _, row := range selected {
		var admission futuresAdmission
		if json.Unmarshal(row.Admission, &admission) != nil {
			return fmt.Errorf("%w: lease admission is invalid", errFuturesTerms)
		}
		space := r.futuresReceivingSpace(all, row.PortID)
		if space < 0 {
			return fmt.Errorf("%w: admission waits for its reserved receiving lease", errFuturesPrecondition)
		}
	}
	return nil
}

func (r *Runner) stageFuturesCapacity(ctx context.Context, mission *store.Mission, data *missionData, selected []store.FuturesIntent) (bool, error) {
	if len(selected) == 0 || data.Action != nil || data.Futures.LeaseChange != nil {
		return false, nil
	}
	all, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	port := selected[0].PortID
	warehouse, _ := r.futuresWarehouse(port, "")
	reserved := futuresReceivingReserved(all, port)
	occupied := max(warehouse.UsedTonnage+warehouse.ReservedTonnage, warehouse.LeasedTonnage-warehouse.FreeTonnage)
	target := occupied + reserved
	if target > warehouse.LeasedTonnage {
		limit, rent := int64(0), int64(0)
		for _, row := range selected {
			var admission futuresAdmission
			if row.Status != "intent" || row.FuturesID != "" || row.PortID != port ||
				json.Unmarshal(row.Admission, &admission) != nil || admission.Lease == nil {
				return false, fmt.Errorf("%w: lease requires retained unsent admission", errFuturesTerms)
			}
			limit = max(limit, admission.Lease.TargetTons)
			rent += admission.RentPence
		}
		if target > limit || target-warehouse.LeasedTonnage > r.futuresLeaseAvailable(port) || rent <= 0 {
			for _, row := range selected {
				rejected, err := r.db.RejectUnsentFuturesIntent(ctx, r.account.ID, row.IntentID)
				if err != nil {
					return false, err
				}
				if !rejected {
					return false, fmt.Errorf("%w: capacity refusal cannot release an ambiguous admission", errFuturesEvidence)
				}
			}
			mission.Phase = "evaluating"
			return true, r.saveMission(ctx, mission, data)
		}
		baseline := warehouse.LeasedTonnage
		if holding, exists := data.Futures.Leases[port]; exists {
			baseline = holding.BaselineTons + max(0, warehouse.LeasedTonnage-holding.TargetTons)
		} else if port == "amsterdam" && !data.Futures.LegacyClosed && mission.Generation == legacyFuturesCohort {
			baseline = data.Futures.InitialLease
		}
		change := futuresLeaseChange{PortID: port, BeforeTons: warehouse.LeasedTonnage, TargetTons: target,
			BaselineTons: baseline, IntentID: selected[0].IntentID}
		return r.stageFuturesLease(ctx, mission, data, change)
	}
	stage, path, body := futuresAdmissionStage(selected)
	if err := r.stage(ctx, mission, data, ShipState{}, stage, path, body); err != nil {
		return false, err
	}
	return data.Action != nil, nil
}

func (r *Runner) stageFuturesLease(ctx context.Context, mission *store.Mission, data *missionData, change futuresLeaseChange) (bool, error) {
	stage := "futures-resize-lease"
	if change.TargetTons < change.BeforeTons {
		stage = "futures-release-lease"
	}
	change.ActionID = ActionID(r.account.ID, mission.Generation, stage, data.Sequence)
	data.Futures.LeaseChange = &change
	data.Futures.PortID = change.PortID
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, err
	}
	if err := r.stage(ctx, mission, data, ShipState{}, stage, "/v1/warehouse/set-lease", change.body()); err != nil {
		return false, err
	}
	return data.Action != nil, nil
}

func (r *Runner) futuresLeaseFloor(data *missionData, rows []store.FuturesIntent, port string) int64 {
	warehouse, _ := r.futuresWarehouse(port, "")
	reserved := futuresReceivingReserved(rows, port)
	baseline := int64(0)
	if holding, exists := data.Futures.Leases[port]; exists {
		baseline = holding.BaselineTons + max(0, warehouse.LeasedTonnage-holding.TargetTons)
	} else if port == "amsterdam" && !data.Futures.LegacyClosed {
		baseline = data.Futures.InitialLease
	}
	floor := max(baseline, warehouse.UsedTonnage+warehouse.ReservedTonnage+reserved)
	if change := data.Futures.LeaseChange; change != nil && change.PortID == port {
		floor = max(floor, change.TargetTons)
	}
	return floor
}

func (r *Runner) releaseFuturesLease(ctx context.Context, mission *store.Mission, data *missionData, rows []store.FuturesIntent) (bool, error) {
	if data.Action != nil {
		return false, nil
	}
	if change := data.Futures.LeaseChange; change != nil {
		pending, err := r.db.PendingActions(ctx, r.account.ID)
		if err != nil {
			return false, err
		}
		for _, action := range pending {
			if action.ID == change.ActionID {
				return false, r.hold(ctx, "__futures", "ambiguous warehouse capacity mutation retains its original journal key")
			}
		}
		// A locally retired no-send action may leave its immutable terms behind.
		data.Futures.LeaseChange = nil
		return true, r.saveMission(ctx, mission, data)
	}
	ports := make([]string, 0, len(data.Futures.Leases))
	for port := range data.Futures.Leases {
		ports = append(ports, port)
	}
	slices.Sort(ports)
	for _, port := range ports {
		holding := data.Futures.Leases[port]
		warehouse, _ := r.futuresWarehouse(port, "")
		target := r.futuresLeaseFloor(data, rows, port)
		if target >= warehouse.LeasedTonnage {
			continue
		}
		baseline := holding.BaselineTons + max(0, warehouse.LeasedTonnage-holding.TargetTons)
		return r.stageFuturesLease(ctx, mission, data, futuresLeaseChange{PortID: port, BeforeTons: warehouse.LeasedTonnage,
			TargetTons: target, BaselineTons: baseline})
	}
	return false, nil
}

func (r *Runner) futuresLeasePreflight(ctx context.Context, data *missionData) error {
	change := data.Futures.LeaseChange
	if change == nil || data.Action.ID != change.ActionID || !futuresLeaseStage(data.Action.Stage) ||
		data.Action.Method != http.MethodPost || data.Action.Path != "/v1/warehouse/set-lease" ||
		!jsonEqual(data.Action.Body, encode(change.body())) {
		return fmt.Errorf("%w: lease mutation changed immutable intent", errFuturesTerms)
	}
	warehouse, _ := r.futuresWarehouse(change.PortID, "")
	if warehouse.LeasedTonnage != change.BeforeTons {
		return fmt.Errorf("%w: lease baseline changed before first send", errFuturesTerms)
	}
	rows, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return err
	}
	if change.TargetTons < change.BeforeTons {
		copyData := *data
		copyState := *data.Futures
		copyState.LeaseChange = nil
		copyData.Futures = &copyState
		floor := r.futuresLeaseFloor(&copyData, rows, change.PortID)
		if change.TargetTons < floor || change.TargetTons < change.BaselineTons {
			return fmt.Errorf("%w: lease release would consume stock or receiving commitments", errFuturesPrecondition)
		}
		return nil
	}
	if change.IntentID == "" || change.TargetTons-change.BeforeTons > r.futuresLeaseAvailable(change.PortID) {
		return fmt.Errorf("%w: lease increase lacks retained admission or current port capacity", errFuturesTerms)
	}
	reserved := futuresReceivingReserved(rows, change.PortID)
	occupied := max(warehouse.UsedTonnage+warehouse.ReservedTonnage, warehouse.LeasedTonnage-warehouse.FreeTonnage)
	if occupied+reserved != change.TargetTons {
		return fmt.Errorf("%w: exact receiving capacity changed before first send", errFuturesTerms)
	}
	copyData := *data
	copyState := *data.Futures
	copyState.IntentID = change.IntentID
	copyData.Futures = &copyState
	selected, err := r.futuresActionIntents(ctx, &copyData)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("%w: lease increase has no admission", errFuturesTerms)
	}
	var rent, target int64
	for _, row := range selected {
		var admission futuresAdmission
		if row.Status != "intent" || row.FuturesID != "" || json.Unmarshal(row.Admission, &admission) != nil ||
			admission.Lease == nil ||
			row.ReservedCashPence < row.MarginPence+admission.RentPence {
			return fmt.Errorf("%w: lease no longer has its exact covered admission and rent", errFuturesTerms)
		}
		rent += admission.RentPence
		target = max(target, admission.Lease.TargetTons)
	}
	if rent <= 0 || target < change.TargetTons {
		return fmt.Errorf("%w: lease size or rent is not reserved", errFuturesTerms)
	}
	stage, path, body := futuresAdmissionStage(selected)
	copyData.Action = &durableAction{ID: selected[0].ActionID, Stage: stage, Path: path, Method: http.MethodPost, Body: encode(body)}
	return r.futuresAdmissionPreflight(ctx, &copyData, true)
}

func (r *Runner) finishFuturesLease(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	change := data.Futures.LeaseChange
	if data.Futures.Leases == nil {
		data.Futures.Leases = make(map[string]futuresLeaseHolding)
	}
	if change.TargetTons == change.BaselineTons {
		delete(data.Futures.Leases, change.PortID)
	} else {
		data.Futures.Leases[change.PortID] = futuresLeaseHolding{BaselineTons: change.BaselineTons, TargetTons: change.TargetTons}
	}
	data.Futures.LeaseChange = nil
	r.invalidateFutures()
	data.LastActionID, data.Action = data.Action.ID, nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) rejectFuturesLeaseAdmission(ctx context.Context, data *missionData) error {
	change := data.Futures.LeaseChange
	if change == nil || change.IntentID == "" {
		return nil
	}
	selectedData := *data
	state := *data.Futures
	state.IntentID = change.IntentID
	selectedData.Futures = &state
	selected, err := r.futuresActionIntents(ctx, &selectedData)
	if err != nil {
		return err
	}
	for _, row := range selected {
		if row.Status == "rejected" {
			continue
		}
		rejected, err := r.db.RejectUnsentFuturesIntent(ctx, r.account.ID, row.IntentID)
		if err != nil {
			return err
		}
		if !rejected {
			return fmt.Errorf("%w: lease refusal cannot release an ambiguous admission", errFuturesEvidence)
		}
	}
	return nil
}

func (r *Runner) consumeFuturesLease(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	change := data.Futures.LeaseChange
	if change == nil {
		return false, fmt.Errorf("%w: lease response has no original mutation", errFuturesEvidence)
	}
	if status >= 200 && status < 300 {
		var warehouse futuresWarehouse
		if json.Unmarshal(response, &warehouse) != nil || warehouse.PortID != change.PortID || warehouse.LeasedTonnage != change.TargetTons {
			return r.reconcileCommitted(ctx, mission, data)
		}
		return r.finishFuturesLease(ctx, mission, data)
	}
	if status < 400 || status >= 500 {
		return false, r.hold(ctx, "__futures", "non-definitive warehouse lease outcome retains its admission, payload and journal key")
	}
	var problem struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(response, &problem)
	if status == http.StatusUnauthorized || status == http.StatusTooManyRequests || status == http.StatusForbidden && problem.Code != "no_port_presence" {
		data.Futures.LeaseChange = nil
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	if err := r.rejectFuturesLeaseAdmission(ctx, data); err != nil {
		return false, err
	}
	data.Futures.LeaseChange = nil
	data.LastActionID, data.Action = data.Action.ID, nil
	data.Sequence++
	data.CooldownUntil = maxTime(data.CooldownUntil, r.gameDelay(1))
	r.invalidateFutures()
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) reconcileFuturesLease(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	change := data.Futures.LeaseChange
	if change == nil || data.Action.ID != change.ActionID || !jsonEqual(data.Action.Body, encode(change.body())) {
		return false, fmt.Errorf("%w: lease recovery has lost immutable mutation terms", errFuturesEvidence)
	}
	if data.Action.FuturesObserveAfter.IsZero() {
		data.Action.FuturesObserveAfter = time.Now().UTC()
		return true, r.saveMission(ctx, mission, data)
	}
	warehouse, _ := r.futuresWarehouse(change.PortID, "")
	if !r.fresh("warehouses") || !r.meta["warehouses"].UpdatedAt.After(data.Action.FuturesObserveAfter) || warehouse.LeasedTonnage != change.TargetTons {
		return false, nil
	}
	if action != nil && action.State == store.ActionPending {
		if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_futures_capacity", "warehouse": warehouse})); err != nil {
			return false, err
		}
	}
	return r.finishFuturesLease(ctx, mission, data)
}
