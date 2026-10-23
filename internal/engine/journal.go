package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type durableAction struct {
	ID               string          `json:"id"`
	Stage            string          `json:"stage"`
	Method           string          `json:"method"`
	Path             string          `json:"path"`
	Body             json.RawMessage `json:"body"`
	CreatedAt        time.Time       `json:"created_at"`
	Before           ShipState       `json:"before"`
	FinancialSession string          `json:"financial_session"`
	FinancialEpoch   uint64          `json:"financial_epoch"`
}

var errFinancialCaptureInvalidated = errors.New("financial event invalidated the captured funds")

const neverSentFinancialEvent = "never_sent_financial_event"

const neverSentPrecondition = "never_sent_precondition"

var errActionPrecondition = errors.New("new action preconditions changed")

type actionDisposition int

const (
	actionUnresolved actionDisposition = iota
	actionSuccess
	actionRejected
	actionIntegrityFailure
	actionCommittedUnknown
)

func disposition(err error) actionDisposition {
	if err == nil {
		return actionSuccess
	}
	var upstream *gameapi.APIError
	if !errors.As(err, &upstream) {
		return actionUnresolved
	}
	if upstream.Code == "idempotency_key_reused" {
		return actionIntegrityFailure
	}
	if upstream.Status >= 200 && upstream.Status < 300 {
		return actionCommittedUnknown
	}
	if upstream.Status >= 400 && upstream.Status < 500 && upstream.Code != "idempotency_request_in_flight" {
		return actionRejected
	}
	return actionUnresolved
}

func actionID(accountID, generation, stage string, sequence int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("tradewinds:%s:%s:%s:%d", accountID, generation, stage, sequence))).String()
}

func (r *Runner) stage(ctx context.Context, mission *store.Mission, data *missionData, ship ShipState, stage, path string, body any) error {
	if data.Action != nil {
		return errors.New("cannot replace an unresolved persisted action")
	}
	if r.capturedFinancial != r.publishedFinancial.Load() {
		return r.hold(ctx, mission.ShipID, "financial event invalidated the captured funds; authoritative resynchronization precedes staging")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if r.capturedFinancial != r.publishedFinancial.Load() {
		return r.hold(ctx, mission.ShipID, "financial event invalidated the captured funds; authoritative resynchronization precedes staging")
	}
	method := http.MethodPost
	if path == "/v1/company/settings" {
		method = http.MethodPatch
	}
	data.Action = &durableAction{
		ID:               actionID(r.account.ID, mission.Generation, stage, data.Sequence),
		Stage:            stage,
		Method:           method,
		Path:             path,
		Body:             payload,
		CreatedAt:        time.Now().UTC(),
		Before:           ship,
		FinancialSession: r.financialSession,
		FinancialEpoch:   r.capturedFinancial,
	}
	return r.saveMission(ctx, mission, data)
}

func (r *Runner) saveMission(ctx context.Context, mission *store.Mission, data *missionData) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	mission.Data = payload
	return r.db.PutMission(ctx, mission)
}

func (r *Runner) stagedFundsValid(action *durableAction) bool {
	return action.FinancialSession == r.financialSession &&
		action.FinancialEpoch == r.capturedFinancial &&
		action.FinancialEpoch == r.publishedFinancial.Load()
}

// Only an intent without a journal entry, or with committed local no-send
// proof, may be released. Retain the mission's actual cargo and obligations so
// its existing stage planner can re-evaluate fresh state before a new request.
func (r *Runner) replanUnsentAction(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if err := r.hold(ctx, mission.ShipID, "never-sent action's preconditions were invalidated; authoritative resynchronization and replanning precede new spending"); err != nil {
		return false, err
	}
	if data.Plan.Kind == "maintenance" {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	data.Action = nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

// dispatch always uses the journal's immutable request, including on the first
// attempt. It never reconstructs a request from a newly calculated plan.
func (r *Runner) dispatch(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if data.Action == nil {
		return false, nil
	}
	if !r.opts.Execute {
		return false, r.hold(ctx, mission.ShipID, "observe mode: persisted action is not dispatched")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	action := data.Action
	exists, err := r.db.HasAction(ctx, r.account.ID, action.ID)
	if err != nil {
		return false, err
	}
	// Account ownership serializes dispatch: an absent exact journal entry
	// means this call, not an earlier process, will create the pending row.
	firstSend := !exists
	if firstSend && time.Now().Before(data.CooldownUntil) {
		return false, r.hold(ctx, mission.ShipID, "unresolved staged action waiting for persisted retry cooldown")
	}
	if firstSend && !r.stagedFundsValid(action) {
		return r.replanUnsentAction(ctx, mission, data)
	}
	if firstSend {
		if err := r.newActionPreflight(ctx, mission, data); err != nil {
			if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
				return false, holdErr
			}
			return r.replanUnsentAction(ctx, mission, data)
		}
	}
	persisted, err := r.db.BeginAction(ctx, &store.Action{ID: action.ID, AccountID: r.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
	if errors.Is(err, store.ErrActionExpired) {
		return r.reconcileExpired(ctx, mission, data, persisted)
	}
	if err != nil {
		return false, err
	}
	if persisted.State == store.ActionReconciled {
		var evidence struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(persisted.Response, &evidence); err != nil {
			return false, fmt.Errorf("invalid reconciliation evidence for %s: %w", persisted.ID, err)
		}
		if evidence.Kind == neverSentFinancialEvent || evidence.Kind == neverSentPrecondition {
			return r.replanUnsentAction(ctx, mission, data)
		}
		return r.applyReconciliation(ctx, mission, data)
	}
	if persisted.State == store.ActionComplete {
		if persisted.Status == http.StatusTooManyRequests {
			if persisted.RetryAfter == nil || persisted.RetryAfter.IsZero() {
				return false, errors.New("stored HTTP 429 lacks a durable retry deadline; reconcile before retrying")
			}
			data.CooldownUntil = maxTime(data.CooldownUntil, *persisted.RetryAfter)
		}
		return r.consumeResult(ctx, mission, data, persisted.Status, persisted.Response)
	}
	if !firstSend {
		if changed, err := r.reconcileObservedAction(ctx, mission, data, persisted); changed || err != nil {
			return changed, err
		}
	}
	if time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, r.hold(ctx, mission.ShipID, "unresolved action waiting for persisted retry cooldown")
	}
	deadline := persisted.CreatedAt.Add(store.ActionReplayWindow)
	if time.Until(deadline) <= time.Minute {
		return r.reconcileExpired(ctx, mission, data, persisted)
	}
	// All transport retries must end before the upstream's idempotency window.
	requestContext, cancel := context.WithDeadline(ctx, deadline.Add(-time.Minute))
	defer cancel()
	r.log.Debug("dispatching durable action", zap.String("action_id", action.ID), zap.String("stage", action.Stage), zap.String("ship_id", mission.ShipID))
	var response json.RawMessage
	r.lastMutationStatus.Store(0)
	if firstSend {
		err = r.api.DoWithPreflight(requestContext, persisted.Method, persisted.Path, persisted.Body, persisted.ID, &response, func() error {
			if !r.stagedFundsValid(action) {
				return errFinancialCaptureInvalidated
			}
			if err := r.newActionPreflight(requestContext, mission, data); err != nil {
				return fmt.Errorf("%w: %s", errActionPrecondition, err)
			}
			if !r.stagedFundsValid(action) {
				return errFinancialCaptureInvalidated
			}
			return nil
		})
		if errors.Is(err, errFinancialCaptureInvalidated) || errors.Is(err, errActionPrecondition) {
			// The first-attempt preflight runs after the rate-limit wait and
			// before transport. This newly created row is proven never sent;
			// old pending rows never take this path, even after a restart.
			kind := neverSentFinancialEvent
			if errors.Is(err, errActionPrecondition) {
				kind = neverSentPrecondition
				if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
					return false, holdErr
				}
			}
			evidence := encode(map[string]any{
				"kind":                      kind,
				"captured_financial_epoch":  action.FinancialEpoch,
				"published_financial_epoch": r.publishedFinancial.Load(),
			})
			if err := r.db.ReconcileAction(ctx, persisted.ID, evidence); err != nil {
				return false, err
			}
			return r.replanUnsentAction(ctx, mission, data)
		}
	} else {
		err = r.api.Do(requestContext, persisted.Method, persisted.Path, persisted.Body, persisted.ID, &response)
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	outcome := disposition(err)
	if outcome == actionIntegrityFailure {
		return false, fmt.Errorf("journal integrity failure for %s: %w", persisted.ID, err)
	}
	if outcome == actionUnresolved {
		data.CooldownUntil = time.Now().UTC().Add(5 * time.Second)
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
		return false, r.holdUnresolvedAction(ctx, mission.ShipID, action, "action remains unresolved; retained request and key: "+errorText(err))
	}
	status := int(r.lastMutationStatus.Load())
	var retryAfter *time.Time
	if err != nil {
		var upstream *gameapi.APIError
		_ = errors.As(err, &upstream)
		status = upstream.Status
		response = upstream.Body
		data.CooldownUntil = maxTime(data.CooldownUntil, upstream.RetryAfter)
		if upstream.Status == http.StatusTooManyRequests {
			retryAfter = &upstream.RetryAfter
		}
		if outcome == actionCommittedUnknown {
			response = encode(map[string]any{"committed_uninterpreted": true, "raw_body": []byte(upstream.Body), "status": upstream.Status})
		}
	}
	if status < 200 || status >= 500 {
		return false, errors.New("API observer did not provide a definitive HTTP status")
	}
	if status != http.StatusTooManyRequests && !json.Valid(response) {
		response = encode(map[string]any{"raw_body": []byte(response)})
	}
	if err := r.db.CompleteAction(ctx, persisted.ID, status, response, retryAfter); err != nil {
		return false, err
	}
	changed, err := r.consumeResult(ctx, mission, data, status, response)
	if err != nil {
		return changed, err
	}
	// Completed-action replay takes the earlier branch and never emits a new
	// outcome. Console output is not transactional with either durable write.
	if status >= 200 && status < 300 {
		if outcome == actionSuccess && changed && data.Action == nil && data.LastActionID == action.ID {
			r.logCommittedAction(ctx, mission, data, action, response)
		} else {
			r.log.Warn("committed action requires authoritative reconciliation",
				zap.String("action_id", action.ID),
				zap.String("stage", action.Stage),
				zap.String("ship_id", mission.ShipID),
			)
		}
	} else {
		r.log.Warn("action rejected", zap.String("action_id", action.ID), zap.String("stage", action.Stage), zap.String("ship_id", mission.ShipID), zap.Int("status", status))
	}
	return changed, nil
}

// outcomeText is one upstream-derived string field awaiting batch redaction.
type outcomeText struct {
	field string
	value string
}

// Only the fresh dispatch path calls this after response validation and the
// resulting mission transition have both completed durably. Numeric outcomes
// are always retained; every string derived from upstream data is scrubbed
// against the account's current credentials and fails closed to the
// redaction marker when those credentials cannot be read.
func (r *Runner) logCommittedAction(ctx context.Context, mission *store.Mission, data *missionData, action *durableAction, response json.RawMessage) {
	level := zap.InfoLevel
	if action.Stage == "dock" || action.Stage == "offshore" {
		level = zap.DebugLevel
	}
	entry := r.log.Check(level, "action committed")
	if entry == nil {
		return
	}
	fields := []zap.Field{
		zap.String("action_id", action.ID),
		zap.String("stage", action.Stage),
	}
	// action_id and stage are generated internally; everything below is
	// upstream-derived and redacted in one batch before it reaches the console.
	upstream := []outcomeText{{field: "ship_id", value: mission.ShipID}}
	switch action.Stage {
	case "buy", "sell", "provision":
		var result tradeResult
		_ = json.Unmarshal(response, &result)
		upstream = append(upstream, outcomeText{field: "commodity", value: result.CommodityID})
		fields = append(fields,
			zap.Int64("quantity_tons", *result.FilledQuantity),
			zap.Int64("gross_pence", *result.TotalGrossPence),
			zap.Int64("customs_pence", *result.CustomsDutyPence),
		)
	case "deliver-freight", "deliver-charter":
		var result struct {
			ContractID string `json:"contract_id"`
			Quantity   int64  `json:"delivered_quantity"`
			Reward     int64  `json:"reward_paid_pence"`
		}
		_ = json.Unmarshal(response, &result)
		upstream = append(upstream, outcomeText{field: "contract_id", value: result.ContractID})
		fields = append(fields, zap.Int64("quantity_tons", result.Quantity), zap.Int64("reward_pence", result.Reward))
	case "accept-freight":
		obligation := data.Freight[len(data.Freight)-1]
		upstream = append(upstream,
			outcomeText{field: "contract_id", value: obligation.ContractID},
			outcomeText{field: "destination", value: obligation.Destination},
		)
		fields = append(fields,
			zap.Int64("quantity_tons", obligation.Quantity),
			zap.Int64("reward_pence", obligation.RewardPence),
			zap.Int64("collateral_pence", obligation.CollateralPence),
		)
	case "build":
		upstream = append(upstream,
			outcomeText{field: "built_ship_id", value: data.BuiltShipID},
			outcomeText{field: "ship_name", value: data.BuildName},
			outcomeText{field: "ship_class", value: data.BuildClass},
		)
		fields = append(fields, zap.Int64("cost_pence", data.BuildCostPence))
	case "settings":
		var result struct {
			Enabled bool  `json:"auto_remit_for_bills"`
			Reserve int64 `json:"auto_remit_min_reserve_pence"`
		}
		_ = json.Unmarshal(response, &result)
		fields = append(fields, zap.Bool("auto_remit_for_bills", result.Enabled), zap.Int64("reserve_pence", result.Reserve))
	case "remit", "sourcing-remit":
		var result struct {
			Gross int64 `json:"gross_amount_pence"`
			Fee   int64 `json:"fee_pence"`
			Net   int64 `json:"net_credited_pence"`
		}
		_ = json.Unmarshal(response, &result)
		fields = append(fields, zap.Int64("gross_pence", result.Gross), zap.Int64("fee_pence", result.Fee), zap.Int64("net_credited_pence", result.Net))
	case "depart":
		var result struct {
			BerthDues int64 `json:"berth_dues_pence"`
			Debt      int64 `json:"berth_dues_debt_pence"`
		}
		_ = json.Unmarshal(response, &result)
		upstream = append(upstream, outcomeText{field: "destination", value: mission.Destination})
		fields = append(fields, zap.Int64("berth_dues_pence", result.BerthDues), zap.Int64("berth_dues_debt_pence", result.Debt))
	case "refit":
		upstream = append(upstream, outcomeText{field: "built_ship_id", value: data.BuiltShipID})
		fields = append(fields, zap.Bool("cost_known", false))
	}
	values := make([]string, len(upstream))
	for i, text := range upstream {
		values[i] = text.value
	}
	cleaned := r.db.ScrubOutcomeTexts(ctx, r.account.ID, values)
	for i, text := range upstream {
		fields = append(fields, zap.String(text.field, cleaned[i]))
	}
	entry.Write(fields...)
}

func (r *Runner) holdUnresolvedAction(ctx context.Context, shipID string, action *durableAction, reason string) error {
	if err := r.hold(ctx, shipID, reason); err != nil {
		return err
	}
	key := shipID + "|action_unresolved"
	if action == nil {
		return nil
	}
	fingerprint := sha256.Sum256([]byte(action.ID))
	now := time.Now()
	previous := r.decisionHashes[key]
	if previous.hash != fingerprint || now.Sub(previous.writtenAt) >= 7*24*time.Hour {
		r.log.Warn("action unresolved; retained immutable request for recovery",
			zap.String("action_id", action.ID),
			zap.String("stage", action.Stage),
			zap.String("ship_id", shipID),
		)
		r.rememberDecision(key, fingerprint, now)
	}
	return nil
}

func errorText(err error) string {
	if err == nil {
		return "unknown transport outcome"
	}
	return err.Error()
}

func (r *Runner) reconcileExpired(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if action == nil {
		return false, r.holdUnresolvedAction(ctx, mission.ShipID, data.Action, "expired action has no persisted immutable request")
	}
	if changed, err := r.reconcileObservedAction(ctx, mission, data, action); changed || err != nil {
		return changed, err
	}
	if data.Action != nil && data.Action.Stage == "build" && r.fresh("ships") {
		for _, candidate := range r.stateCache.Ships {
			if candidate.Ship.Name == data.BuildName && candidate.Ship.ShipClass == data.BuildClass {
				evidence := encode(map[string]any{"kind": "authoritative_named_build", "ship": candidate, "observed_at": r.meta["ships"].UpdatedAt})
				if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
					return false, err
				}
				data.BuiltShipID = candidate.Ship.ID
				data.CostsKnown = false
				return r.applyReconciliation(ctx, mission, data)
			}
		}
	}
	return false, r.holdUnresolvedAction(ctx, mission.ShipID, data.Action, fmt.Sprintf("idempotency safety deadline expired: unresolved action %s (%s %s) is quarantined; restore authoritative request-specific outcome evidence before any resend; unrelated cash-credit-only deliveries may continue", action.ID, action.Method, action.Path))
}

func departureObserved(before, actual ShipState, destination string) bool {
	if actual.Ship.ID != before.Ship.ID || actual.Ship.Status == "sunk" {
		return false
	}
	if actual.Ship.Status == "sailing" && len(actual.RouteLegs) > 0 {
		return actual.RouteLegs[len(actual.RouteLegs)-1].ToNode == destination
	}
	return actual.Ship.CurrentPortID == destination && before.Ship.CurrentPortID != destination
}

func (r *Runner) applyReconciliation(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	data.CostsKnown = false
	switch data.Action.Stage {
	case "depart":
		data.DepartedCargo = data.Action.Before.CargoManifest
		mission.Phase = "sailing"
		if data.ReturnJourney {
			mission.Phase = "return_sailing"
		}
	case "build":
		mission.Phase = "growth_refit"
	case "offshore":
		if mission.Phase == "closing" {
			data.ClosingObservedAt = time.Now()
		} else if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance_done"
		}
	case "dock":
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance_done"
		}
	case "refit":
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance_done"
		} else if mission.ShipID == "__growth" {
			mission.Phase = "growth_provision"
		}
	case "accept-freight":
		var request struct {
			ContractID string `json:"contract_id"`
		}
		if err := json.Unmarshal(data.Action.Body, &request); err != nil {
			return false, err
		}
		contract, exists := r.contract(request.ContractID)
		if !exists || !r.fresh("contracts") || contract.Stale {
			return false, r.hold(ctx, mission.ShipID, "reconciled freight acceptance waits for its fresh REST record")
		}
		known := false
		for _, obligation := range data.Freight {
			known = known || obligation.ContractID == request.ContractID
		}
		if !known {
			data.Freight = append(data.Freight, obligationFromContract(contract))
		}
		mission.Phase = "freight"
	case "deliver-freight":
		mission.Phase = "arrival"
	default:
		return false, r.hold(ctx, mission.ShipID, "reconciled outcome has no safe mission transition")
	}
	data.LastActionID = data.Action.ID
	data.Sequence++
	data.Action = nil
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) spendingPort(ship ShipState) (Port, error) {
	for _, dataset := range []string{"vaults", "catalog", "factors", "profile"} {
		if !r.fresh(dataset) {
			return Port{}, fmt.Errorf("new spending requires fresh %s for regional funds and upkeep reserves", dataset)
		}
	}
	port, exists := findPort(r.stateCache.Catalog, ship.Ship.CurrentPortID)
	home, homeExists := findPort(r.stateCache.Catalog, r.stateCache.HomePortID)
	if !exists || port.BankingRegion == "" || !homeExists || home.BankingRegion == "" {
		return Port{}, errors.New("new spending requires authoritative current and home banking regions")
	}
	return port, nil
}

// This applies only to a proven first send. Attempted requests and stored
// outcomes must retain their exact journal identity despite later state changes.
func (r *Runner) newActionPreflight(ctx context.Context, mission *store.Mission, data *missionData) error {
	if _, _, ok := r.serverBounds(); !ok || r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return errors.New("new action requires current successful server clock authority")
	}
	action := data.Action
	var request struct {
		ShipID      string `json:"ship_id"`
		ContractID  string `json:"contract_id"`
		Destination string `json:"dest_port_id"`
		Quantity    int64  `json:"quantity"`
		Commodity   string `json:"commodity_id"`
		PortID      string `json:"port_id"`
		Price       int64  `json:"price_pence"`
		FromRegion  string `json:"from_region"`
		ToRegion    string `json:"to_region"`
		Gross       int64  `json:"gross_amount_pence"`
		ShipClass   string `json:"ship_class"`
		BuildMode   string `json:"build_mode"`
		Name        string `json:"name"`
		Cannons     int64  `json:"target_cannons_count"`
	}
	if err := json.Unmarshal(action.Body, &request); err != nil {
		return errors.New("new action has an invalid immutable request")
	}
	if action.Stage == "remit" || action.Stage == "sourcing-remit" {
		if request.Gross <= 0 || request.FromRegion == "" || request.FromRegion == request.ToRegion ||
			!r.fresh("vaults") || !r.fresh("rates") {
			return errors.New("new remittance requires fresh balances, observed rates and a valid immutable transfer")
		}
		var rate Rate
		found := false
		for _, candidate := range r.stateCache.Rates {
			if candidate.FromRegion == request.FromRegion && candidate.ToRegion == request.ToRegion && candidate.FeeBPS >= 0 && candidate.FeeBPS < 10_000 {
				rate, found = candidate, true
				break
			}
		}
		if !found {
			return errors.New("new remittance lacks its observed regional fee")
		}
		ship, _ := r.ship(mission.ShipID)
		input := r.planningInput(ship)
		required, err := r.treasuryReserves(ctx)
		if err != nil {
			return err
		}
		if action.Stage == "sourcing-remit" {
			input, err = r.sourcingInput(ctx, ship)
			if err != nil {
				return err
			}
			required = input.ReserveByRegion
		}
		if input.Balances[request.FromRegion]-request.Gross < required[request.FromRegion] {
			return errors.New("new remittance would spend protected donor reserves or incumbent commitments")
		}
		fee := request.Gross/10_000*rate.FeeBPS + request.Gross%10_000*rate.FeeBPS/10_000
		if action.Stage == "sourcing-remit" && (data.Relocation == nil || !r.relocationBudget(data, fee)) {
			return errors.New("new sourcing remittance exceeds the immutable exploration expense cap")
		}
	}
	if action.Stage == "build" {
		if mission.ShipID != "__growth" || data.BuiltShipID != "" || request.PortID != data.BuildPort ||
			request.ShipClass != data.BuildClass || request.Name == "" || request.Name != data.BuildName || request.BuildMode != "turnkey_cash" {
			return errors.New("new build requires its matching immutable growth intent")
		}
		return r.growthBuildReady(ctx, data)
	}
	if request.ShipID == "" {
		return nil
	}
	ship, exists := r.ship(request.ShipID)
	if !exists || !r.fresh("ships") || request.ShipID != mission.ShipID && mission.ShipID != "__growth" {
		return errors.New("new action requires the matching fresh ship")
	}
	switch action.Stage {
	case "depart", "offshore":
		if !r.shipUnlocked(ship) {
			return errors.New("new navigation action waits for the turnaround lock")
		}
		if ship.Voyage != nil || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			return errors.New("new navigation action requires stationary authoritative presence")
		}
		if action.Stage == "offshore" {
			if ship.Ship.Status != "berthed" {
				return errors.New("new offshore action requires a berthed ship")
			}
			return nil
		}
		hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
		key := routeKey(ship.Ship.CurrentPortID, request.Destination, ship.Ship.ShipClass)
		route := r.stateCache.Routes[key]
		if !exists || !r.fresh("routes/"+key) || !r.fresh("zones") || ship.Stores.ProvisionsTons+1e-9 < route.SafeProvisionsTons+1 {
			return errors.New("new departure requires fresh legal route, hazards and safe stores")
		}
		if err := ValidateRoute(route, hull, r.stateCache.Catalog.SeaZones); err != nil {
			return err
		}
		if err := r.validateFreightDeparture(ship, request.Destination, data, hull); err != nil {
			return err
		}
		if data.Relocation != nil {
			return r.relocationDepartureReady(ctx, ship, data)
		}
		if !data.ReturnJourney && data.Plan.Kind != "recovery" {
			revenue, reason := acquiredRevenue(r.planningInput(ship), data.Plan, data.BoughtQuantity, data.Freight)
			if acquiredNet(data, revenue) <= 0 {
				return fmt.Errorf("new departure lost its current profit case: %s", reason)
			}
		}
	case "deliver-freight", "deliver-charter", "accept-freight":
		contract, exists := r.contract(request.ContractID)
		if !r.fresh("contracts") || !exists || contract.Stale || ship.Voyage != nil ||
			ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			return errors.New("new contract action requires its matching fresh REST record and stationary ship")
		}
		if action.Stage == "deliver-freight" {
			for _, obligation := range data.Freight {
				if obligation.ContractID == request.ContractID && !obligation.Settled && !obligation.TenderRefused &&
					contract.ContractType == "freight" && contract.Status == "active" &&
					contract.CarrierShipID == ship.Ship.ID && contract.CarrierCompanyID == r.account.CompanyID &&
					ship.Ship.CurrentPortID == obligation.Destination && ship.CargoManifest[obligation.Commodity] >= obligation.Quantity &&
					r.deadlineOpen(obligation.Deadline) {
					return nil
				}
			}
			return errors.New("new freight tender lacks a full matching active seal, cargo or delivery_deadline window")
		}
		if contract.Status != "open" || !r.deadlineOpen(contract.ExpiresAt) {
			return errors.New("new contract action lacks an open conservative deadline window")
		}
		if action.Stage == "deliver-charter" && (contract.ContractType != "charter" ||
			contract.OriginPortID != ship.Ship.CurrentPortID || request.Quantity <= 0 ||
			request.Quantity > min(ship.CargoManifest[contract.CommodityID]-sealedReservations(data.Freight)[contract.CommodityID], data.Eligible[contract.CommodityID])) {
			return errors.New("new charter tender lacks matching carried-in unsealed cargo")
		}
		if action.Stage == "accept-freight" {
			if contract.ContractType != "freight" || contract.OriginPortID != ship.Ship.CurrentPortID {
				return errors.New("new freight acceptance requires its matching origin and freight terms")
			}
			if contract.CollateralPence > 0 {
				port, err := r.spendingPort(ship)
				if err != nil {
					return err
				}
				input := r.planningInput(ship)
				if input.Balances[port.BankingRegion]-contract.CollateralPence < input.ReserveByRegion[port.BankingRegion] {
					return errors.New("new freight acceptance would spend protected regional reserves")
				}
			}
		}
	case "refit":
		port, err := r.spendingPort(ship)
		if err != nil {
			return err
		}
		hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
		if ship.Voyage != nil || ship.Ship.Status != "berthed" || !port.HasShipyard || !exists ||
			request.Cannons <= ship.Ship.CannonsCount || request.Cannons > hull.MaxCannons {
			return errors.New("new refit requires matching berth, shipyard and legal added cannons")
		}
		added := request.Cannons - ship.Ship.CannonsCount
		if added > math.MaxInt64/15_000 || added > (ship.EffectiveHoldCapacityTons-ship.CargoUsedTons)/2 {
			return errors.New("new refit exceeds integer-pence bounds or actual cannon displacement")
		}
		input := r.planningInput(ship)
		if input.Balances[port.BankingRegion]-added*15_000-data.GrowthWorkingCapitalPence < input.ReserveByRegion[port.BankingRegion] {
			return errors.New("new refit would spend protected regional reserves or commissioning working capital")
		}
	case "buy", "sell", "provision":
		if ship.Voyage != nil || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" ||
			request.PortID != ship.Ship.CurrentPortID || !r.fresh("books/"+request.PortID) || r.stateCache.Markets[request.PortID].Visibility != "quay" {
			return errors.New("new market action requires matching stationary presence and a fresh executable local quay")
		}
		if action.Stage != "sell" {
			port, err := r.spendingPort(ship)
			if err != nil {
				return err
			}
			if request.Quantity <= 0 || request.Price <= 0 || request.Price > math.MaxInt64/request.Quantity {
				return errors.New("new purchase exceeds integer-pence funding bounds")
			}
			if action.Stage != "buy" || data.SourcingHandoff == nil {
				cost := request.Quantity * request.Price
				customs := ceilBPS(cost, r.stateCache.Markets[request.PortID].CustomsBPS) + request.Quantity
				var futureBills int64
				if action.Stage == "buy" {
					futureBills = data.Plan.Economics.BerthDuesPence + data.Plan.Economics.UpkeepPence + data.Plan.Economics.CollateralPence + data.Plan.Economics.TollPence
				}
				input := r.planningInput(ship)
				if input.Balances[port.BankingRegion]-cost-customs-futureBills < input.ReserveByRegion[port.BankingRegion] {
					return errors.New("new purchase would spend protected regional bills reserves")
				}
			}
		}
		if action.Stage == "provision" && data.Relocation != nil {
			cost := request.Quantity * request.Price
			if cost > data.Relocation.ExpenseCapPence || request.Quantity > data.Relocation.ExpenseCapPence ||
				!r.relocationBudget(data, cost+ceilBPS(cost, r.stateCache.Markets[request.PortID].CustomsBPS)+request.Quantity) {
				return errors.New("new sourcing provisions exceed the immutable exploration expense cap")
			}
		}
		if action.Stage == "buy" && data.SourcingHandoff != nil {
			contract, exists := r.contract(data.SourcingHandoff.TargetContractID)
			input, err := r.sourcingInput(ctx, ship)
			if err != nil {
				return err
			}
			input.PaidProvisionsPence = data.ProvisionsPence
			probe := data.Plan
			probe.Economics.GameDays = data.SourcingGameDays
			plan := sizeRelocationCargo(input, probe, contract, data.SourcingSpentPence)
			if !exists || !plan.Accepted || plan.Quantity != request.Quantity || plan.BuyLimitPence != request.Price {
				return errors.New("new sourcing purchase lost its fresh positive combined-cycle price case")
			}
			for region, required := range cargoFundingRequired(input, plan) {
				if input.Balances[region] < required {
					return errors.New("new sourcing purchase would consume unfunded reserves or incumbent commitments")
				}
			}
		}
	}
	return nil
}

func (r *Runner) reconcileObservedAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if data.Action == nil || action == nil || !r.fresh("ships") {
		return false, nil
	}
	var request struct {
		ShipID      string `json:"ship_id"`
		ContractID  string `json:"contract_id"`
		Destination string `json:"dest_port_id"`
		Cannons     int64  `json:"target_cannons_count"`
	}
	if err := json.Unmarshal(action.Body, &request); err != nil {
		return false, err
	}
	ship, exists := r.ship(request.ShipID)
	if !exists || request.ShipID != mission.ShipID && mission.ShipID != "__growth" {
		return false, nil
	}
	kind := ""
	switch data.Action.Stage {
	case "depart":
		if departureObserved(data.Action.Before, ship, request.Destination) {
			kind = "authoritative_departure"
		}
	case "offshore":
		if data.Action.Before.Ship.Status == "berthed" && ship.Ship.Status == "anchored_roadstead" &&
			ship.Ship.CurrentPortID == data.Action.Before.Ship.CurrentPortID && ship.Voyage == nil {
			kind = "authoritative_offshore"
		}
	case "dock":
		if data.Action.Before.Ship.Status == "anchored_roadstead" && ship.Voyage == nil &&
			ship.Ship.CurrentPortID == data.Action.Before.Ship.CurrentPortID &&
			(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead" && ship.Ship.DockQueuedAt != nil && data.Action.Before.Ship.DockQueuedAt == nil) {
			kind = "authoritative_docking"
		}
	case "refit":
		if request.Cannons > data.Action.Before.Ship.CannonsCount && ship.Ship.CannonsCount == request.Cannons {
			kind = "authoritative_refit"
		}
	case "accept-freight", "deliver-freight":
		contract, exists := r.contract(request.ContractID)
		if !r.fresh("contracts") || !exists || contract.Stale || contract.ContractType != "freight" ||
			contract.CarrierShipID != request.ShipID || contract.CarrierCompanyID != r.account.CompanyID {
			break
		}
		if data.Action.Stage == "accept-freight" && contract.Status == "active" && obligationFromContract(contract).Deadline != nil {
			kind = "authoritative_freight_acceptance"
		}
		if data.Action.Stage == "deliver-freight" && (contract.Status == "completed" || contract.Status == "cancelled" || contract.Status == "defaulted") {
			kind = "authoritative_terminal_freight"
		}
	}
	if kind == "" {
		return false, nil
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": kind, "ship": ship, "contract_id": request.ContractID, "observed_at": r.meta["ships"].UpdatedAt})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	data.CostsKnown = false
	return r.applyReconciliation(ctx, mission, data)
}
