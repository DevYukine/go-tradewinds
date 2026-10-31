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
	ID     string `json:"id"`
	Stage  string `json:"stage"`
	Method string `json:"method"`
	Path   string `json:"path"`
	// Nested JSON marshaling must not normalize an already-journaled request.
	// The mission stores bytes; the action journal still sends the original JSON.
	Body                []byte    `json:"body"`
	CreatedAt           time.Time `json:"created_at"`
	Before              ShipState `json:"before"`
	FinancialSession    string    `json:"financial_session"`
	FinancialEpoch      uint64    `json:"financial_epoch"`
	ControlEpoch        uint64    `json:"control_epoch"`
	MaintenanceDock     bool      `json:"maintenance_dock,omitempty"`
	FuturesStockBefore  *int64    `json:"futures_stock_before,omitempty"`
	FuturesObserveAfter time.Time `json:"futures_observe_after,omitempty"`
}

var errFinancialCaptureInvalidated = errors.New("financial event invalidated the captured funds")
var errControlCaptureInvalidated = errors.New("stream control invalidated the captured state")

const neverSentFinancialEvent = "never_sent_financial_event"

const neverSentPrecondition = "never_sent_precondition"
const neverSentControlEvent = "never_sent_control_event"
const neverSentFuturesTerms = "never_sent_futures_terms"
const neverSentMarqueQuote = "never_sent_marque_quote_increase"

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

// ActionID is stable across retries of one logical account mutation.
func ActionID(accountID, generation, stage string, sequence int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("tradewinds:%s:%s:%s:%d", accountID, generation, stage, sequence))).String()
}

func (r *Runner) stage(ctx context.Context, mission *store.Mission, data *missionData, ship ShipState, stage, path string, body any) error {
	return r.stageAction(ctx, mission, data, ship, stage, path, body, false)
}

func (r *Runner) stageAction(ctx context.Context, mission *store.Mission, data *missionData, ship ShipState, stage, path string, body any, maintenanceDock bool) error {
	if data.Action != nil {
		return errors.New("cannot replace an unresolved persisted action")
	}
	if r.capturedControl != r.publishedControl.Load() {
		return r.hold(ctx, mission.ShipID, "stream control invalidated the captured state; authoritative resynchronization precedes staging")
	}
	if r.capturedFinancial != r.publishedFinancial.Load() {
		return r.hold(ctx, mission.ShipID, "financial event invalidated the captured funds; authoritative resynchronization precedes staging")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if r.capturedControl != r.publishedControl.Load() {
		return r.hold(ctx, mission.ShipID, "stream control invalidated the captured state; authoritative resynchronization precedes staging")
	}
	if r.capturedFinancial != r.publishedFinancial.Load() {
		return r.hold(ctx, mission.ShipID, "financial event invalidated the captured funds; authoritative resynchronization precedes staging")
	}
	method := http.MethodPost
	if path == "/v1/company/settings" {
		method = http.MethodPatch
	}
	actionID := ActionID(r.account.ID, mission.Generation, stage, data.Sequence)
	if data.Futures != nil && stage == "futures-create" {
		intent, err := r.futuresIntent(ctx, data)
		if err != nil {
			return err
		}
		actionID = intent.ActionID
	}
	data.Action = &durableAction{
		ID:               actionID,
		Stage:            stage,
		Method:           method,
		Path:             path,
		Body:             payload,
		CreatedAt:        time.Now().UTC(),
		Before:           ship,
		FinancialSession: r.financialSession,
		FinancialEpoch:   r.capturedFinancial,
		ControlEpoch:     r.capturedControl,
		MaintenanceDock:  maintenanceDock,
	}
	if data.Futures != nil && (stage == "futures-stock" || stage == "futures-close-stock") {
		_, unlocked := r.futuresWarehouse()
		data.Action.FuturesStockBefore = &unlocked
	}
	if stage == "provision" || stage == "dock" {
		if err := r.incomeActionReady(ctx, ship, data, data.Action); err != nil {
			data.Action = nil
			return r.hold(ctx, mission.ShipID, err.Error())
		}
	}
	if stage == "depart" {
		data.FirstArrivalETA = time.Time{}
	}
	if err := r.saveMission(ctx, mission, data); err != nil {
		return err
	}
	r.replayLink(ctx, mission, data)
	return nil
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
	if err := r.db.PutMission(ctx, mission); err != nil {
		return err
	}
	r.replayPlan(ctx, mission, data)
	return nil
}

func (r *Runner) stagedFundsValid(action *durableAction) bool {
	return action.FinancialSession == r.financialSession &&
		action.FinancialEpoch == r.capturedFinancial &&
		action.FinancialEpoch == r.publishedFinancial.Load()
}

func (r *Runner) stagedControlValid(action *durableAction) bool {
	return action.ControlEpoch == r.capturedControl &&
		action.ControlEpoch == r.publishedControl.Load()
}

// Only an intent without a journal entry, or with committed local no-send
// proof, may be released. Retain the mission's actual cargo and obligations so
// its existing stage planner can re-evaluate fresh state before a new request.
func (r *Runner) replanUnsentAction(ctx context.Context, mission *store.Mission, data *missionData, kind string) (bool, error) {
	if data.Futures != nil {
		exists, err := r.db.HasAction(ctx, r.account.ID, data.Action.ID)
		if err != nil {
			return false, err
		}
		if (kind == neverSentFinancialEvent || kind == neverSentControlEvent) && !exists {
			data.Action.FinancialSession = r.financialSession
			data.Action.FinancialEpoch = r.capturedFinancial
			data.Action.ControlEpoch = r.capturedControl
			return true, r.saveMission(ctx, mission, data)
		}
		if data.Action.Stage == "futures-create" {
			if kind == neverSentFuturesTerms {
				if err := r.db.RecordFuturesOutcome(ctx, r.account.ID, data.Futures.IntentID, "rejected", encode(map[string]string{"kind": kind})); err != nil {
					return false, err
				}
			} else if exists {
				nextID := ActionID(r.account.ID, mission.Generation, data.Action.Stage, data.Sequence+1)
				if err := r.db.ReadmitFuturesIntent(ctx, r.account.ID, data.Futures.IntentID, nextID); err != nil {
					return false, err
				}
			}
		}
		if kind == neverSentFuturesTerms {
			mission.Phase = "futures_stopped"
		} else if data.Action.Stage == "futures-lease" {
			data.Futures.SetupAttempted = false
		}
		data.Action = nil
		data.Sequence++
		if kind == neverSentPrecondition || kind == neverSentFuturesTerms {
			data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(5*time.Second))
			return false, r.saveMission(ctx, mission, data)
		}
		return true, r.saveMission(ctx, mission, data)
	}
	if err := r.hold(ctx, mission.ShipID, "never-sent action's preconditions were invalidated; authoritative resynchronization and replanning precede new spending"); err != nil {
		return false, err
	}
	if data.Plan.Kind == "maintenance" && data.Action.Stage == "offshore" {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if kind == neverSentMarqueQuote {
		// The issue endpoint has no price cap. Retire the frozen admission and
		// wait out the cached quote's TTL before normal admission can spend again.
		data.Marque = nil
		mission.Phase = "done"
		data.CooldownUntil = maxTime(data.CooldownUntil, time.Now().UTC().Add(marqueQuoteInterval))
	}
	data.Action = nil
	data.Sequence++
	return true, r.saveMission(ctx, mission, data)
}

// dispatch always uses the journal's immutable request, including on the first
// attempt. It never reconstructs a request from a newly calculated plan.
func (r *Runner) dispatch(ctx context.Context, mission *store.Mission, data *missionData) (changed bool, err error) {
	if data.Futures != nil {
		defer r.futuresErrorHold(ctx, &changed, &err)
	}
	if data.Passage == nil && data.Action != nil && passengerStage(data.Action.Stage) {
		return r.quarantinePassage(ctx, mission, data, errors.New("passenger journal action lacks its durable passage intent"))
	}
	if data.Passage != nil {
		defer r.quarantinePassageError(ctx, mission, data, &changed, &err)
		if data.Passage.QuarantineReason != "" {
			return false, nil
		}
	}
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
	if firstSend && data.Futures != nil && action.Stage == "futures-settle" {
		if changed, err := r.reconcileFuturesAction(ctx, mission, data, nil); changed || err != nil {
			return changed, err
		}
	}
	if firstSend && time.Now().Before(data.CooldownUntil) {
		return false, r.hold(ctx, mission.ShipID, "unresolved staged action waiting for persisted retry cooldown")
	}
	if firstSend && (!r.stagedFundsValid(action) || !r.stagedControlValid(action)) {
		kind := neverSentFinancialEvent
		if !r.stagedControlValid(action) {
			kind = neverSentControlEvent
		}
		return r.replanUnsentAction(ctx, mission, data, kind)
	}
	if firstSend {
		if err := r.newActionPreflight(ctx, mission, data); err != nil {
			if data.Futures != nil && !errors.Is(err, errFuturesPrecondition) && !errors.Is(err, errFuturesTerms) {
				return false, err
			}
			if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
				return false, holdErr
			}
			kind := neverSentPrecondition
			if errors.Is(err, errFuturesTerms) {
				kind = neverSentFuturesTerms
			}
			if errors.Is(err, errMarqueQuoteIncreased) {
				kind = neverSentMarqueQuote
			}
			return r.replanUnsentAction(ctx, mission, data, kind)
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
			Kind   string         `json:"kind"`
			Ship   ShipState      `json:"ship"`
			Marque letterOfMarque `json:"marque"`
			Loan   gameapi.Loan   `json:"loan"`
		}
		if err := json.Unmarshal(persisted.Response, &evidence); err != nil {
			return false, fmt.Errorf("invalid reconciliation evidence for %s: %w", persisted.ID, err)
		}
		if evidence.Kind == neverSentFinancialEvent || evidence.Kind == neverSentPrecondition ||
			evidence.Kind == neverSentControlEvent || evidence.Kind == neverSentFuturesTerms || evidence.Kind == neverSentMarqueQuote {
			return r.replanUnsentAction(ctx, mission, data, evidence.Kind)
		}
		if data.Futures != nil {
			return r.reconcileFuturesAction(ctx, mission, data, persisted)
		}
		if passengerStage(action.Stage) {
			return r.reconcilePassengerAction(ctx, mission, data, persisted)
		}
		if action.Stage == "build" && (evidence.Kind == "authoritative_named_build" || evidence.Kind == "authoritative_named_mortgage_build") &&
			evidence.Ship.Ship.ID != "" && evidence.Ship.Ship.Name == data.BuildName && evidence.Ship.Ship.ShipClass == data.BuildClass {
			if data.Capacity != nil && data.Capacity.Mortgage != nil &&
				(evidence.Kind != "authoritative_named_mortgage_build" || !evidence.Loan.IsMortgage ||
					evidence.Loan.CollateralShipID == nil || *evidence.Loan.CollateralShipID != evidence.Ship.Ship.ID) {
				return false, r.hold(ctx, mission.ShipID, "mortgaged build reconciliation requires its named hull and mortgage loan")
			}
			data.BuiltShipID = evidence.Ship.Ship.ID
		}
		if action.Stage == "marque" && evidence.Kind == "authoritative_marque" && data.Marque != nil &&
			validMarque(evidence.Marque) && evidence.Marque.SeaZoneID == data.Marque.Quote.SeaZoneID && evidence.Marque.AddedPiracyBPS == 500 {
			data.Marque.IssuedID, data.Marque.ExpiresAt = evidence.Marque.ID, evidence.Marque.ExpiresAt
		}
		if action.Stage == "loan-accept" {
			if evidence.Kind != "authoritative_loan" || !r.acceptLoanResult(data, encode(evidence.Loan)) {
				return false, r.hold(ctx, mission.ShipID, "reconciled borrowing lacks its exact quote-specific loan receipt")
			}
		}
		if action.Stage == "loan-repay" && (evidence.Kind != "authoritative_loan" || !loanRepaymentObserved(evidence.Loan, data.LoanRepayment)) {
			return false, r.hold(ctx, mission.ShipID, "reconciled repayment lacks its named paid debt")
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
		if passengerStage(action.Stage) && (data.Passage == nil || !data.Passage.RetryOffer) {
			return false, r.holdUnresolvedAction(ctx, mission.ShipID, action, "passenger action awaits bounded request-specific GET evidence before replay")
		}
	}
	if data.Futures != nil && !r.opts.FuturesPilotEnabled &&
		(action.Stage == "futures-create" || action.Stage == "futures-lease" || action.Stage == "futures-stock") {
		return false, r.hold(ctx, mission.ShipID, "futures pilot disabled; unresolved request and coverage retained without a resend")
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
	if entry := r.log.Check(zap.DebugLevel, "dispatching durable action"); entry != nil {
		fields := []zap.Field{zap.String("action_id", action.ID), zap.String("stage", action.Stage)}
		entry.Write(r.scrubOutcomeFields(ctx, fields, actionOutcomeTexts(mission, data, action))...)
	}
	var response json.RawMessage
	var preflightFailure error
	r.lastMutationStatus.Store(0)
	if firstSend {
		err = r.api.DoWithPreflight(requestContext, persisted.Method, persisted.Path, persisted.Body, persisted.ID, &response, func() error {
			if !r.stagedControlValid(action) {
				return errControlCaptureInvalidated
			}
			if !r.stagedFundsValid(action) {
				return errFinancialCaptureInvalidated
			}
			if err := r.newActionPreflight(requestContext, mission, data); err != nil {
				if data.Futures != nil && !errors.Is(err, errFuturesPrecondition) && !errors.Is(err, errFuturesTerms) {
					preflightFailure = err
					return err
				}
				return fmt.Errorf("%w: %w", errActionPrecondition, err)
			}
			if !r.stagedControlValid(action) {
				return errControlCaptureInvalidated
			}
			if !r.stagedFundsValid(action) {
				return errFinancialCaptureInvalidated
			}
			return nil
		})
		if errors.Is(err, errFinancialCaptureInvalidated) || errors.Is(err, errControlCaptureInvalidated) || errors.Is(err, errActionPrecondition) {
			// The first-attempt preflight runs after the rate-limit wait and
			// before transport. This newly created row is proven never sent;
			// old pending rows never take this path, even after a restart.
			kind := neverSentFinancialEvent
			if errors.Is(err, errControlCaptureInvalidated) {
				kind = neverSentControlEvent
			}
			if errors.Is(err, errActionPrecondition) {
				kind = neverSentPrecondition
				if errors.Is(err, errFuturesTerms) {
					kind = neverSentFuturesTerms
				}
				if errors.Is(err, errMarqueQuoteIncreased) {
					kind = neverSentMarqueQuote
				}
				if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
					return false, holdErr
				}
			}
			evidence := encode(map[string]any{
				"kind":                      kind,
				"captured_financial_epoch":  action.FinancialEpoch,
				"published_financial_epoch": r.publishedFinancial.Load(),
				"captured_control_epoch":    action.ControlEpoch,
				"published_control_epoch":   r.publishedControl.Load(),
			})
			if err := r.db.ReconcileAction(ctx, persisted.ID, evidence); err != nil {
				return false, err
			}
			return r.replanUnsentAction(ctx, mission, data, kind)
		}
	} else {
		err = r.api.DoWithPreflight(requestContext, persisted.Method, persisted.Path, persisted.Body, persisted.ID, &response, func() error {
			if r.capturedControl != r.publishedControl.Load() {
				return errControlCaptureInvalidated
			}
			if data.Futures != nil {
				if err := r.futuresAuthority(); err != nil {
					return err
				}
				if action.Stage == "futures-stock" {
					if !r.fresh("books/amsterdam") {
						return fmt.Errorf("%w: pending procurement requires current bounded customs", errFuturesPrecondition)
					}
					return r.futuresOrderPreflight(action)
				}
				if action.Stage == "futures-settle" || action.Stage == "futures-close-stock" || action.Stage == "futures-close-lease" {
					if err := r.futuresPreflight(requestContext, data); err != nil {
						if !errors.Is(err, errFuturesPrecondition) && !errors.Is(err, errFuturesTerms) {
							preflightFailure = err
						}
						return err
					}
				}
			}
			return nil
		})
	}
	if preflightFailure != nil {
		return false, preflightFailure
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
	changed, err = r.consumeResult(ctx, mission, data, status, response)
	if status >= 400 && status < 500 {
		r.logRejectedAction(ctx, mission, data, action, status, response)
	}
	if err != nil {
		return changed, err
	}
	// Completed-action replay takes the earlier branch and never emits a new
	// outcome. Console output is not transactional with either durable write.
	if status >= 200 && status < 300 {
		if changed && data.Action == nil && data.LastActionID == action.ID {
			if outcome == actionSuccess {
				if action.Stage == "build" && data.Capacity != nil && data.BuildCostPence > data.Capacity.QuoteLimitPence {
					if entry := r.log.Check(zap.WarnLevel, "capacity build receipt exceeded the admitted turnkey quote ceiling"); entry != nil {
						fields := []zap.Field{
							zap.Int64("quote_limit_pence", data.Capacity.QuoteLimitPence),
							zap.Int64("actual_cost_pence", data.BuildCostPence),
						}
						texts := []outcomeText{
							{field: "ship_id", value: data.BuiltShipID},
							{field: "build_port", value: data.BuildPort},
							{field: "build_class", value: data.BuildClass},
						}
						entry.Write(r.scrubOutcomeFields(ctx, fields, texts)...)
					}
				}
				r.logCommittedAction(ctx, mission, data, action, response)
			}
		} else if entry := r.log.Check(zap.WarnLevel, "committed action requires authoritative reconciliation"); entry != nil {
			fields := []zap.Field{zap.String("action_id", action.ID), zap.String("stage", action.Stage)}
			entry.Write(r.scrubOutcomeFields(ctx, fields, actionOutcomeTexts(mission, data, action))...)
		}
	}
	return changed, nil
}

// outcomeText is one upstream-derived string field awaiting batch redaction.
type outcomeText struct {
	field string
	value string
}

func actionOutcomeTexts(mission *store.Mission, data *missionData, action *durableAction) []outcomeText {
	if action.Stage == "underwrite" && data != nil && data.Underwriting != nil {
		policy := data.Underwriting.Policy
		return []outcomeText{
			{field: "policy_id", value: policy.ID},
			{field: "insured_ship_id", value: policy.ShipID},
			{field: "origin_port_id", value: policy.OriginPortID},
			{field: "destination_port_id", value: policy.DestPortID},
		}
	}
	shipID := action.Before.Ship.ID
	if shipID == "" {
		shipID = mission.ShipID
	}
	shipName := action.Before.Ship.Name
	portID := action.Before.Ship.CurrentPortID
	if action.Stage == "build" {
		if data == nil {
			var request struct {
				Name   string `json:"name"`
				PortID string `json:"port_id"`
			}
			_ = json.Unmarshal(action.Body, &request)
			shipName, portID = request.Name, request.PortID
		} else {
			if data.BuiltShipID != "" {
				shipID = data.BuiltShipID
			}
			shipName = data.BuildName
			portID = data.BuildPort
		}
	}
	texts := []outcomeText{{field: "ship_id", value: shipID}}
	if shipName != "" {
		texts = append(texts, outcomeText{field: "ship", value: shipName})
	}
	if portID != "" {
		texts = append(texts, outcomeText{field: "port_id", value: portID})
	}
	return texts
}

// Scrub complete values before applying a Unicode-safe console bound. Cutting
// a credential first would prevent the exact-secret and token patterns matching.
func (r *Runner) scrubOutcomeFields(ctx context.Context, fields []zap.Field, texts []outcomeText) []zap.Field {
	values := make([]string, len(texts))
	for i, text := range texts {
		values[i] = text.value
	}
	cleaned := r.db.ScrubOutcomeTexts(ctx, r.account.ID, values)
	for i, text := range texts {
		value := cleaned[i]
		characters := 0
		for offset := range value {
			if characters == 512 {
				value = value[:offset] + "..."
				break
			}
			characters++
		}
		fields = append(fields, zap.String(text.field, value))
	}
	return fields
}

// Only fresh dispatch calls this after validating the receipt and durably
// consuming its mission transition. Replays do not announce another commit.
func (r *Runner) logCommittedAction(ctx context.Context, mission *store.Mission, data *missionData, action *durableAction, response json.RawMessage) {
	var envelope struct {
		Uninterpreted bool `json:"committed_uninterpreted"`
	}
	_ = json.Unmarshal(response, &envelope)
	if envelope.Uninterpreted {
		return
	}
	entry := r.log.Check(zap.InfoLevel, "action committed")
	if entry == nil {
		return
	}
	fields := []zap.Field{
		zap.String("action_id", action.ID),
		zap.String("stage", action.Stage),
	}
	upstream := actionOutcomeTexts(mission, data, action)
	switch action.Stage {
	case "underwrite":
		var result struct {
			Underwritings []struct {
				ID         string `json:"id"`
				CompanyID  string `json:"underwriter_company_id"`
				SliceBPS   int64  `json:"slice_bps"`
				Collateral int64  `json:"collateral_locked_pence"`
				Premium    int64  `json:"premium_share_pence"`
			} `json:"underwritings"`
		}
		_ = json.Unmarshal(response, &result)
		for _, receipt := range result.Underwritings {
			if receipt.CompanyID == r.account.CompanyID {
				upstream = append(upstream, outcomeText{field: "underwriting_id", value: receipt.ID})
				fields = append(fields, zap.Int64("slice_bps", receipt.SliceBPS),
					zap.Int64("collateral_locked_pence", receipt.Collateral), zap.Int64("premium_entitlement_pence", receipt.Premium))
				break
			}
		}
	case "buy", "sell", "provision":
		var result tradeResult
		_ = json.Unmarshal(response, &result)
		var request struct {
			Side     string `json:"side"`
			Quantity int64  `json:"quantity"`
			Price    int64  `json:"price_pence"`
		}
		_ = json.Unmarshal(action.Body, &request)
		upstream = append(upstream,
			outcomeText{field: "commodity", value: result.CommodityID},
			outcomeText{field: "side", value: request.Side},
		)
		fields = append(fields,
			zap.Int64("requested_quantity_tons", request.Quantity),
			zap.Int64("limit_price_pence", request.Price),
		)
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
		var result struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(response, &result)
		for _, obligation := range data.Freight {
			if obligation.ContractID != result.ContractID {
				continue
			}
			upstream = append(upstream,
				outcomeText{field: "contract_id", value: obligation.ContractID},
				outcomeText{field: "commodity", value: obligation.Commodity},
				outcomeText{field: "destination", value: obligation.Destination},
			)
			fields = append(fields,
				zap.Int64("quantity_tons", obligation.Quantity),
				zap.Int64("reward_pence", obligation.RewardPence),
				zap.Int64("collateral_pence", obligation.CollateralPence),
			)
			if obligation.Deadline != nil {
				fields = append(fields, zap.Time("delivery_deadline", *obligation.Deadline))
			}
			break
		}
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
		var request struct {
			FromRegion string `json:"from_region"`
			ToRegion   string `json:"to_region"`
		}
		_ = json.Unmarshal(action.Body, &request)
		upstream = append(upstream,
			outcomeText{field: "from_region", value: request.FromRegion},
			outcomeText{field: "to_region", value: request.ToRegion},
		)
		fields = append(fields, zap.Int64("gross_pence", result.Gross), zap.Int64("fee_pence", result.Fee), zap.Int64("net_credited_pence", result.Net))
	case "depart":
		var result struct {
			Destination string `json:"dest_port_id"`
			BerthDues   int64  `json:"berth_dues_pence"`
			Debt        int64  `json:"berth_dues_debt_pence"`
		}
		_ = json.Unmarshal(response, &result)
		upstream = append(upstream,
			outcomeText{field: "origin_port_id", value: action.Before.Ship.CurrentPortID},
			outcomeText{field: "destination", value: result.Destination},
		)
		var cargoTons int64
		for _, quantity := range action.Before.CargoManifest {
			cargoTons += quantity
		}
		fields = append(fields, zap.Int64("cargo_tons", cargoTons))
		fields = append(fields, zap.Int64("berth_dues_pence", result.BerthDues), zap.Int64("berth_dues_debt_pence", result.Debt))
	case "offshore":
		var result struct {
			BerthDues *int64 `json:"berth_dues_pence"`
			Debt      *int64 `json:"berth_dues_debt_pence"`
		}
		_ = json.Unmarshal(response, &result)
		if result.BerthDues != nil && *result.BerthDues >= 0 {
			fields = append(fields, zap.Int64("berth_dues_pence", *result.BerthDues))
		}
		if result.Debt != nil && *result.Debt >= 0 {
			fields = append(fields, zap.Int64("berth_dues_debt_pence", *result.Debt))
		}
	case "refit":
		var request struct {
			Cannons int64 `json:"target_cannons_count"`
		}
		_ = json.Unmarshal(action.Body, &request)
		upstream = append(upstream, outcomeText{field: "built_ship_id", value: data.BuiltShipID})
		fields = append(fields, zap.Int64("target_cannons_count", request.Cannons), zap.Bool("cost_known", false))
	}
	entry.Write(r.scrubOutcomeFields(ctx, fields, upstream)...)
}

// Reconciliation proves the observed state, not the missing receipt's cash
// movements. No monetary outcome is inferred from a successful recovery.
func (r *Runner) logRecoveredAction(ctx context.Context, mission *store.Mission, data *missionData, action *durableAction) {
	entry := r.log.Check(zap.InfoLevel, "action recovered")
	if entry == nil {
		return
	}
	fields := []zap.Field{
		zap.String("action_id", action.ID),
		zap.String("stage", action.Stage),
		zap.String("outcome_source", "authoritative_reconciliation"),
	}
	upstream := actionOutcomeTexts(mission, data, action)
	switch action.Stage {
	case "depart":
		fields = append(fields, zap.String("observed_outcome", "departure observed"))
		var request struct {
			Destination string `json:"dest_port_id"`
		}
		_ = json.Unmarshal(action.Body, &request)
		upstream = append(upstream,
			outcomeText{field: "origin_port_id", value: action.Before.Ship.CurrentPortID},
			outcomeText{field: "destination", value: request.Destination},
		)
	case "build":
		fields = append(fields, zap.String("observed_outcome", "matching named ship observed"))
		upstream = append(upstream,
			outcomeText{field: "built_ship_id", value: data.BuiltShipID},
			outcomeText{field: "ship_name", value: data.BuildName},
			outcomeText{field: "ship_class", value: data.BuildClass},
		)
	case "offshore":
		fields = append(fields, zap.String("observed_outcome", "offshore move observed"))
	case "dock":
		fields = append(fields, zap.String("observed_outcome", "berthing or queue entry observed"))
	case "refit":
		fields = append(fields, zap.String("observed_outcome", "target cannon count observed"))
		var request struct {
			Cannons int64 `json:"target_cannons_count"`
		}
		_ = json.Unmarshal(action.Body, &request)
		fields = append(fields, zap.Int64("target_cannons_count", request.Cannons))
	case "accept-freight", "deliver-freight":
		observed := "active freight obligation observed"
		if action.Stage == "deliver-freight" {
			observed = "terminal freight contract observed"
		}
		fields = append(fields, zap.String("observed_outcome", observed))
		var request struct {
			ContractID string `json:"contract_id"`
		}
		_ = json.Unmarshal(action.Body, &request)
		upstream = append(upstream, outcomeText{field: "contract_id", value: request.ContractID})
		if contract, exists := r.contract(request.ContractID); exists && r.fresh("contracts") && !contract.Stale {
			upstream = append(upstream,
				outcomeText{field: "contract_status", value: contract.Status},
				outcomeText{field: "origin_port_id", value: contract.OriginPortID},
				outcomeText{field: "destination", value: contract.DestPortID},
			)
		}
	}
	entry.Write(r.scrubOutcomeFields(ctx, fields, upstream)...)
}

func (r *Runner) logRejectedAction(ctx context.Context, mission *store.Mission, data *missionData, action *durableAction, status int, response json.RawMessage) {
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(response, &problem)
	level := zap.WarnLevel
	if status == http.StatusTooManyRequests {
		level = zap.DebugLevel
	} else if status == http.StatusConflict {
		switch problem.Code {
		case "insufficient_provisions":
			if action.Stage == "depart" {
				level = zap.DebugLevel
			}
		case "ship_not_berthed":
			switch action.Stage {
			case "buy", "sell", "provision", "accept-freight", "refit":
				level = zap.DebugLevel
			}
		case "conflict":
			var request struct {
				ContractID string `json:"contract_id"`
			}
			_ = json.Unmarshal(action.Body, &request)
			if action.Stage == "deliver-freight" {
				for _, obligation := range data.Freight {
					if obligation.ContractID == request.ContractID && obligation.TenderRefused {
						level = zap.DebugLevel
						break
					}
				}
			} else if action.Stage == "deliver-charter" {
				if _, known := data.RejectedCharterTerms[request.ContractID]; known && data.RejectedCharterIDs[request.ContractID] {
					level = zap.DebugLevel
				}
			}
		}
	}
	entry := r.log.Check(level, "action rejected")
	if entry == nil {
		return
	}
	fields := []zap.Field{
		zap.String("action_id", action.ID),
		zap.String("stage", action.Stage),
		zap.Int("status", status),
	}
	if !data.CooldownUntil.IsZero() {
		fields = append(fields, zap.Time("retry_at", data.CooldownUntil))
	}
	upstream := append(actionOutcomeTexts(mission, data, action),
		outcomeText{field: "code", value: problem.Code},
		outcomeText{field: "reason", value: problem.Detail},
	)
	if problem.Code == "" && problem.Detail == "" {
		// Without a problem document the body is the only evidence of the
		// cause. It is redacted in full before the log cap applies.
		upstream = append(upstream, outcomeText{field: "response", value: r.db.ScrubOutcomeResponse(ctx, r.account.ID, response)})
	}
	entry.Write(r.scrubOutcomeFields(ctx, fields, upstream)...)
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
		if entry := r.log.Check(zap.WarnLevel, "action unresolved; retained immutable request for recovery"); entry != nil {
			fields := []zap.Field{zap.String("action_id", action.ID), zap.String("stage", action.Stage)}
			upstream := append(actionOutcomeTexts(&store.Mission{ShipID: shipID}, nil, action), outcomeText{field: "reason", value: reason})
			entry.Write(r.scrubOutcomeFields(ctx, fields, upstream)...)
		}
		r.rememberDecision(key, decisionFingerprint{hash: fingerprint, writtenAt: now})
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
	if data.Action != nil && data.Action.Stage == "build" && r.fresh("ships") && (data.Capacity == nil || data.Capacity.Mortgage == nil) {
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
	action := data.Action
	data.CostsKnown = false
	switch data.Action.Stage {
	case "loan-accept", "loan-repay":
		loanResultPhase(mission, data)
	case "marque":
		mission.Phase = "done"
	case "depart":
		data.DepartedCargo = data.Action.Before.CargoManifest
		recordDepartureCargoCost(data, data.DepartedCargo)
		mission.Phase = "sailing"
		if data.ReturnJourney {
			mission.Phase = "return_sailing"
		}
	case "build":
		mission.Phase = "growth_refit"
		if err := r.clearCapacitySavingsAfterBuild(ctx); err != nil {
			return false, err
		}
	case "offshore":
		if mission.Phase == "closing" {
			data.ClosingObservedAt = time.Now()
		} else if data.Passage != nil {
			mission.Phase = "passenger_loading"
		} else if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance_done"
		}
	case "dock":
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance"
		}
	case "refit":
		if data.Capacity != nil {
			data.Capacity.RefitAttempts++
		}
		if data.Plan.Kind == "maintenance" {
			mission.Phase = "maintenance"
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
	if err := r.saveMission(ctx, mission, data); err != nil {
		return true, err
	}
	r.logRecoveredAction(ctx, mission, data, action)
	return true, nil
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
	if data.Futures != nil {
		return r.futuresPreflight(ctx, data)
	}
	if _, _, ok := r.serverBounds(); !ok || r.stateCache.Clock.Paused || r.stateCache.Clock.Calendar.TimeScale <= 0 {
		return errors.New("new action requires current successful server clock authority")
	}
	action := data.Action
	if passengerStage(action.Stage) {
		return r.passengerActionReady(ctx, mission, data)
	}
	if action.Stage == "hire-factor" {
		target, err := r.liveCapacitySavingsTarget(ctx)
		if err != nil {
			return err
		}
		if target.PortID != "" {
			return errors.New(capacitySavingsReason)
		}
		for _, dataset := range []string{"catalog", "ships", "factors", "vaults", "rates", "profile"} {
			if !r.fresh(dataset) {
				return errors.New("expansion agent requires current successful " + dataset + " authority")
			}
		}
		var hire struct {
			PortID string `json:"port_id"`
		}
		if json.Unmarshal(action.Body, &hire) != nil {
			return errors.New("expansion agent has an invalid immutable request")
		}
		yard, exists := findPort(r.stateCache.Catalog, hire.PortID)
		if !exists || !yard.HasShipyard || yard.IsCove || !regionalWater(r.stateCache.Catalog, yard) {
			return errors.New(expansionAgentAdmissionReason)
		}
		evidence := growthEvidence{}
		if reason, err := r.growthCashFunding(ctx, &evidence); err != nil {
			return err
		} else if reason != "" {
			return errors.New(reason)
		}
		input, err := r.committedPlanningInput(ctx, ShipState{})
		if err != nil {
			return err
		}
		evidence.available = 0
		for region, balance := range input.Balances {
			evidence.available = growthCashTotal(evidence.available, max(0, balance-input.ReserveByRegion[region]))
		}
		capacity, reason, err := r.capacityFleetInput(ctx, &evidence)
		if err != nil {
			return err
		}
		if reason != "" {
			return errors.New(reason)
		}
		if build, _, _ := r.expansionBuildEstimate(yard, capacity); !build.Accepted {
			return errors.New(build.Reason)
		}
	}
	if action.Stage == "loan-accept" || action.Stage == "loan-repay" {
		return r.loanActionReady(ctx, mission, data)
	}
	if mission.ShipID == "__growth" {
		if data.Capacity != nil {
			if data.GrowthLane != nil || data.ProvenLane != "" || action.Stage != "build" && action.Stage != "refit" && action.Stage != "dock" {
				return errors.New("capacity commissioning only builds and arms its own hull without a lane allocation")
			}
		} else if data.GrowthLane == nil || data.GrowthLane.AdmissionID != mission.Generation || data.GrowthLane.withdrawn() {
			return errors.New("new commissioning spend requires its active admitted allocation; withdrawn intents only resolve attempted actions and close")
		}
	}
	if action.Stage == "underwrite" {
		return r.underwritingPreflight(ctx, data, true)
	}
	if action.Stage == "marque" {
		return r.marquePreflight(ctx, data, true)
	}
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
		Mortgage    bool   `json:"mortgage"`
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
		shipID := mission.ShipID
		if shipID == "__growth" {
			shipID = data.BuiltShipID
		}
		ship, _ := r.ship(shipID)
		if ordinaryIncomePending(data) {
			if ship.Ship.ID == "" || mission.ShipID != ship.Ship.ID {
				return errors.New("new income remittance requires its matching ship-owned mission")
			}
			if err := r.incomeActionReady(ctx, ship, data, action); err != nil {
				return err
			}
		}
		if data.GrowthLane != nil && mission.ShipID != "__treasury" {
			if err := r.growthAllocationReady(ctx, ship, data, action); err != nil {
				return err
			}
		}
		var input PlanningInput
		var err error
		if action.Stage == "sourcing-remit" {
			if ship.Ship.ID == "" || !r.fresh("ships") {
				return errors.New("new sourcing remittance requires its matching fresh ship")
			}
			if err := r.insuranceActionReady(ship, "depart", mission.Destination); err != nil {
				return err
			}
			input, err = r.sourcingInput(ctx, ship)
		} else if mission.ShipID == "__loans" {
			if !r.loanAuthority() || data.LoanRepayment == nil {
				return errors.New("loan reserve funding requires current named debt authority")
			}
			loan, exists := r.loanByID(data.LoanRepayment.Before.ID)
			if !exists || loan.Status != "active" || loan.OwedPence <= 0 || request.ToRegion != loan.BankingRegion ||
				loan.ArrearsPence <= 0 && (!loan.IsMortgage || loan.NextPaymentDueAt == nil) {
				return errors.New("loan reserve funding is no longer required by this active loan")
			}
			input = r.planningInput(ship)
			input.ReserveByRegion, err = r.treasuryReserves(ctx)
		} else {
			input, err = r.committedPlanningInput(ctx, ship)
		}
		if err != nil {
			return err
		}
		if data.Plan.Kind == "maintenance" {
			hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
			if !exists || !r.fresh("ships") || ship.Voyage != nil || ship.Ship.ConvoyID != "" ||
				ship.Ship.Status != "berthed" || ship.Ship.CurrentPortID != data.Plan.Origin {
				return errors.New("new store funding requires its matching fresh independent berthed ship")
			}
			quote, err := quoteProvisions(input, 1+float64(hull.CrewRequired)/200*2)
			if err != nil {
				return err
			}
			transfer, err := provisionFunding(input, quote)
			if err != nil {
				return err
			}
			if request.FromRegion != transfer.Rate.FromRegion || request.ToRegion != transfer.Rate.ToRegion || request.Gross != transfer.Gross {
				return errors.New("new store funding no longer matches the current reserve-safe replenishment quote")
			}
		}
		required := input.ReserveByRegion
		if input.Balances[request.FromRegion]-request.Gross < required[request.FromRegion] {
			return errors.New("new remittance would spend protected donor reserves or incumbent commitments")
		}
		fee := request.Gross/10_000*rate.FeeBPS + request.Gross%10_000*rate.FeeBPS/10_000
		if action.Stage == "sourcing-remit" {
			if err := r.relocationSpendingReady(ctx, ship, data, fee); err != nil {
				return err
			}
		}
		if action.Stage == "sourcing-remit" && (data.Relocation == nil || !r.relocationBudget(data, fee)) {
			return errors.New("new sourcing remittance exceeds the immutable exploration expense cap")
		}
	}
	if action.Stage == "build" {
		if mission.ShipID != "__growth" || data.BuiltShipID != "" || request.PortID != data.BuildPort ||
			request.ShipClass != data.BuildClass || request.Name == "" || request.Name != data.BuildName ||
			request.BuildMode != "turnkey_cash" {
			return errors.New("new build requires its matching immutable growth intent")
		}
		if request.Mortgage != (data.Capacity != nil && data.Capacity.Mortgage != nil) {
			return errors.New("new build mortgage flag disagrees with its immutable capacity admission")
		}
		if data.Capacity != nil {
			return r.capacityBuildReady(ctx, data)
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
	if action.Path == "/v1/fleet/list-ship" && r.mortgagedShip(ship.Ship.ID) {
		return errors.New("a mortgaged ship cannot be listed or sold until its mortgage is repaid")
	}
	if action.Path == "/v1/fleet/depart" && (ship.PassengerHoldTons > 0 || data.Passage != nil) {
		if err := r.passageDepartureReady(ship, request.Destination, data); err != nil {
			return err
		}
	}
	if err := r.insuranceActionReady(ship, action.Stage, request.Destination); err != nil {
		return err
	}
	if action.Stage == "buy" || action.Stage == "provision" || action.Stage == "depart" || action.Stage == "dock" {
		if err := r.growthAllocationReady(ctx, ship, data, action); err != nil {
			return err
		}
	}
	if err := r.incomeActionReady(ctx, ship, data, action); err != nil {
		return err
	}
	if data.Relocation != nil && action.Stage == "dock" && data.Relocation.IdlePilot && ship.Ship.CurrentPortID == data.Plan.Origin && r.loanAuthority() {
		if err := r.relocationSpendingReady(ctx, ship, data, 0); err != nil {
			return err
		}
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
		requiredStores := route.SafeProvisionsTons + 1
		if data.Passage != nil {
			requiredStores = route.SafeProvisionsTons
		}
		if !exists || !r.fresh("routes/"+key) || !r.fresh("zones") || ship.Stores.ProvisionsTons+1e-9 < requiredStores {
			return errors.New("new departure requires fresh legal route, hazards and safe stores")
		}
		if err := ValidateRoute(route, hull, r.stateCache.Catalog.SeaZones); err != nil {
			return err
		}
		if err := r.validateFreightDeparture(ship, request.Destination, data, hull); err != nil {
			return err
		}
		if data.Passage != nil {
			return r.passageDepartureReady(ship, request.Destination, data)
		}
		if data.Relocation != nil {
			return r.relocationDepartureReady(ctx, ship, data)
		}
		proceed, err := r.committedProcurementDepartureDecision(ctx, ship, request.Destination, data)
		if err != nil {
			return err
		}
		if !proceed {
			return errors.New("fresh executable local liquidation recovers at least as much as continuing")
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
				input, err := r.committedPlanningInput(ctx, ship)
				if err != nil {
					return err
				}
				if input.Balances[port.BankingRegion]-contract.CollateralPence < input.ReserveByRegion[port.BankingRegion] {
					return errors.New("new freight acceptance would spend protected regional reserves")
				}
			}
		}
	case "refit":
		if data.Capacity != nil && (request.ShipID != data.BuiltShipID || ship.Ship.CurrentPortID != data.Capacity.BuildPort ||
			ship.Ship.ShipClass != data.Capacity.BuildClass || request.Cannons != data.TargetCannons) {
			return errors.New("capacity refit requires its actual built hull, admitted yard and cannon target")
		}
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
		input, err := r.committedPlanningInput(ctx, ship)
		if err != nil {
			return err
		}
		workingCapital := data.GrowthWorkingCapitalPence
		if data.Capacity != nil {
			workingCapital = 0
		}
		if input.Balances[port.BankingRegion]-added*15_000-workingCapital < input.ReserveByRegion[port.BankingRegion] {
			return errors.New("new refit would spend protected regional reserves or commissioning working capital")
		}
	case "buy", "sell", "provision":
		if ship.Voyage != nil || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" ||
			request.PortID != ship.Ship.CurrentPortID || !r.fresh("books/"+request.PortID) || r.stateCache.Markets[request.PortID].Visibility != "quay" {
			return errors.New("new market action requires matching stationary presence and a fresh executable local quay")
		}
		var input PlanningInput
		var err error
		if action.Stage == "buy" && data.SourcingHandoff != nil || action.Stage == "provision" && data.Relocation != nil {
			input, err = r.sourcingInput(ctx, ship)
		} else if action.Stage != "sell" {
			input, err = r.committedPlanningInput(ctx, ship)
		}
		if err != nil {
			return err
		}
		if action.Stage == "buy" {
			if _, err := procurementDestinationRevenue(input, data.Plan, request.Quantity); err != nil {
				return fmt.Errorf("new cargo purchase lost its destination evidence: %w", err)
			}
		}
		if action.Stage != "sell" {
			port, err := r.spendingPort(ship)
			if err != nil {
				return err
			}
			if request.Quantity <= 0 || request.Price <= 0 || request.Price > math.MaxInt64/request.Quantity {
				return errors.New("new purchase exceeds integer-pence funding bounds")
			}
			if action.Stage == "provision" && data.Plan.Kind == "maintenance" {
				hull, exists := findHull(input.Catalog, ship.Ship.ShipClass)
				if !exists || ship.Ship.Status != "berthed" ||
					request.Quantity != max(0, int64(math.Ceil(1+float64(hull.CrewRequired)/200*2-ship.Stores.ProvisionsTons))) {
					return errors.New("new idle provision order no longer matches the current store deficit")
				}
			}
			entry := bookCommodity(input.Markets[request.PortID], request.Commodity)
			if entry == nil || entry.Depth == nil {
				return errors.New("new purchase lacks uncommitted executable source asks")
			}
			fill, err := WalkDepth(entry.Depth.Asks, request.Quantity, true)
			if err != nil || fill.LimitPence > request.Price {
				return errors.New("new purchase would consume source asks committed to another mission")
			}
			if action.Stage != "buy" || data.SourcingHandoff == nil {
				cost := request.Quantity * request.Price
				customs := ceilBPS(cost, r.stateCache.Markets[request.PortID].CustomsBPS) + request.Quantity
				var futureBills int64
				if action.Stage == "buy" {
					futureBills = data.Plan.Economics.BerthDuesPence + data.Plan.Economics.UpkeepPence + data.Plan.Economics.CollateralPence + data.Plan.Economics.TollPence
				}
				if action.Stage == "provision" && data.Relocation != nil {
					if err := r.relocationProvisionReady(ctx, ship, data, request.Quantity, request.Price); err != nil {
						return err
					}
					if !r.relocationProvisionBudget(data, ship, input, request.Quantity, request.Price) {
						return errors.New("new sourcing provisions exceed the immutable exploration expense cap")
					}
				}
				if input.Balances[port.BankingRegion]-cost-customs-futureBills < input.ReserveByRegion[port.BankingRegion] {
					return errors.New("new purchase would spend protected regional bills reserves")
				}
			}
		}
		if action.Stage == "buy" && data.SourcingHandoff != nil {
			contract, exists := r.contract(data.SourcingHandoff.TargetContractID)
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
	if data.Action == nil || action == nil {
		return false, nil
	}
	if passengerStage(data.Action.Stage) {
		return r.reconcilePassengerAction(ctx, mission, data, action)
	}
	if data.Futures != nil {
		return r.reconcileFuturesAction(ctx, mission, data, action)
	}
	if data.Action.Stage == "loan-accept" || data.Action.Stage == "loan-repay" {
		return r.reconcileLoanAction(ctx, mission, data, action)
	}
	if data.Action.Stage == "marque" {
		return r.reconcileMarque(ctx, mission, data, action)
	}
	if data.Action.Stage == "build" && data.Capacity != nil && data.Capacity.Mortgage != nil {
		return r.reconcileMortgageBuild(ctx, mission, data, action)
	}
	if !r.fresh("ships") {
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
