package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err := r.hold(ctx, mission.ShipID, "never-sent action's funds were invalidated; authoritative resynchronization and replanning precede new spending"); err != nil {
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
	if firstSend && !r.stagedFundsValid(action) {
		return r.replanUnsentAction(ctx, mission, data)
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
		if evidence.Kind == neverSentFinancialEvent {
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
			return nil
		})
		if errors.Is(err, errFinancialCaptureInvalidated) {
			// The first-attempt preflight runs after the rate-limit wait and
			// before transport. This newly created row is proven never sent;
			// old pending rows never take this path, even after a restart.
			evidence := encode(map[string]any{
				"kind":                      neverSentFinancialEvent,
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
	case "remit":
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
	if action != nil && r.decisionHashes[key] != action.ID {
		r.log.Warn("action unresolved; retained immutable request for recovery",
			zap.String("action_id", action.ID),
			zap.String("stage", action.Stage),
			zap.String("ship_id", shipID),
		)
		r.decisionHashes[key] = action.ID
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
	ship, exists := r.ship(mission.ShipID)
	if exists && data.Action != nil && data.Action.Stage == "depart" && departureObserved(data.Action.Before, ship, mission.Destination) {
		evidence := encode(map[string]any{"kind": "authoritative_departure", "ship": ship, "observed_at": r.meta["ships"].UpdatedAt})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
		data.CostsKnown = false
		return r.applyReconciliation(ctx, mission, data)
	}
	if data.Action != nil && data.Action.Stage == "build" {
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
	return false, r.holdUnresolvedAction(ctx, mission.ShipID, data.Action, "idempotency safety deadline expired: unresolved action is quarantined; no old-key or new-key resend without authoritative effect evidence")
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
	switch data.Action.Stage {
	case "depart":
		data.DepartedCargo = data.Action.Before.CargoManifest
		mission.Phase = "sailing"
		if data.ReturnJourney {
			mission.Phase = "return_sailing"
		}
	case "build":
		mission.Phase = "growth_refit"
	default:
		return false, r.hold(ctx, mission.ShipID, "reconciled outcome has no safe mission transition")
	}
	data.LastActionID = data.Action.ID
	data.Sequence++
	data.Action = nil
	return true, r.saveMission(ctx, mission, data)
}
