package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const passengerCompletionWaitKey = "_passenger_completion_wait"

type passengerCompletionWait struct {
	ShipID     string    `json:"ship_id"`
	GameHour   float64   `json:"game_hour"`
	ObservedAt time.Time `json:"observed_at"`
	TimeScale  int64     `json:"time_scale"`
}

type passengerCompletionContext struct {
	Mission store.Mission `json:"mission"`
	Data    missionData   `json:"data"`
}

// The caller first attempts complete terminal-event settlement. Once a fallback
// is prepared, it calls this hook before syncPassage so that a retry cannot
// replace the archived accounting identity with a second set of fare receipts.
func (r *Runner) reconcilePassengerCompletion(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if data.Settlement != nil && data.Settlement.ProceedsUnknown && (data.Passage == nil || len(data.Passage.Groups) == 0) {
		return r.quarantinePassage(ctx, mission, data, errors.New("prepared passenger completion lost its boarded batch"))
	}
	if data.Passage == nil || len(data.Passage.Groups) == 0 || data.Action != nil || data.Passage.ReceiptsPrepared || data.Passage.QuarantineReason != "" {
		return false, nil
	}
	waits := decode[map[string]passengerCompletionWait](r.snapshot, passengerCompletionWaitKey)
	if waits == nil {
		waits = make(map[string]passengerCompletionWait)
	}
	wait, waiting := waits[mission.Generation]
	prepared := data.Settlement != nil && data.Settlement.ProceedsUnknown
	if !prepared {
		var aboard []gameapi.PassengerAboardGroup
		raw := decode[map[string]json.RawMessage](r.snapshot, "passenger_aboard")
		empty := r.fresh("ships") && r.fresh("passenger_aboard/"+ship.Ship.ID) && hazardStationary(ship) &&
			ship.Ship.CurrentPortID == data.Plan.Destination && ship.PassengerHoldTons == 0 &&
			json.Unmarshal(raw[ship.Ship.ID], &aboard) == nil && aboard != nil && len(aboard) == 0
		rawClock := decode[gameapi.Clock](r.snapshot, "clock")
		lowerHour, upperHour, clockOK := passengerCaptureGameHours(time.Now(), r.stateCache.Clock,
			rawClock, r.meta["clock"], r.generation)
		clockOK = clockOK && !rawClock.RealUTC.IsZero() &&
			rawClock.Calendar.TotalGameHours == r.stateCache.Clock.Calendar.TotalGameHours &&
			rawClock.Calendar.Minute >= 0 && rawClock.Calendar.Minute < 60
		_, upper, serverOK := r.serverBounds()
		if !empty || !clockOK || !serverOK {
			if waiting {
				delete(waits, mission.Generation)
				r.snapshot[passengerCompletionWaitKey] = encode(waits)
				return true, r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
			}
			return false, nil
		}
		if !waiting || upperHour < wait.GameHour || wait.TimeScale <= 0 {
			wait = passengerCompletionWait{ShipID: ship.Ship.ID, GameHour: upperHour,
				ObservedAt: upper, TimeScale: r.stateCache.Clock.Calendar.TimeScale}
			waits[mission.Generation] = wait
			r.snapshot[passengerCompletionWaitKey] = encode(waits)
			return true, r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
		}
		if lowerHour-wait.GameHour < 2 {
			return false, nil
		}
		started := time.Time{}
		var hold int64
		seen := make(map[string]bool, len(data.Passage.Groups))
		for _, group := range data.Passage.Groups {
			if group.ID == "" || seen[group.ID] || group.BoardedAt == nil || group.BoardedAt.IsZero() || group.HoldTons <= 0 ||
				group.DestPortID != data.Plan.Destination || hold > math.MaxInt64-group.HoldTons {
				return r.quarantinePassage(ctx, mission, data, errors.New("passenger REST completion lacks its complete immutable boarded batch"))
			}
			seen[group.ID] = true
			hold += group.HoldTons
			if started.IsZero() || group.BoardedAt.Before(started) {
				started = *group.BoardedAt
			}
		}
		if started.IsZero() || wait.ObservedAt.Before(started) {
			return r.quarantinePassage(ctx, mission, data, errors.New("passenger REST completion precedes its boarding evidence"))
		}
		bills, expenseIDs := r.claimBillsSince(data, mission.Generation)
		data.ExpensesPence = growthCashTotal(data.ExpensesPence, bills)
		data.SettlementExpenseIDs = expenseIDs
		data.Settlement = &store.Settlement{
			ID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte("passage-rest-completion:"+r.account.ID+":"+mission.Generation)).String(),
			AccountID: r.account.ID, ShipID: ship.Ship.ID, MissionID: mission.Generation,
			Kind: "passage", Lane: data.Plan.Origin + "|" + data.Plan.Destination,
			Origin: data.Plan.Origin, Destination: data.Plan.Destination, QuantityTons: hold,
			ProceedsUnknown: true, CostsKnown: false, AcquisitionPence: data.AcquisitionPence,
			FeesPence: data.FeesPence, ProvisionsPence: data.ProvisionsPence, ExpensesPence: data.ExpensesPence,
			StartedAt: started, CompletedAt: wait.ObservedAt,
			GameSeconds:  max(int64(0), int64(wait.ObservedAt.Sub(started).Seconds()*float64(wait.TimeScale))),
			RevenueBasis: new("passenger_batch_allocation"),
		}
		if err := r.saveMission(ctx, mission, data); err != nil {
			return false, err
		}
	}
	if !waiting || wait.TimeScale <= 0 {
		return r.quarantinePassage(ctx, mission, data, errors.New("prepared passenger completion lost its durable clock observation"))
	}
	receipt := store.PassengerReceipt{Settlement: *data.Settlement, CostsKnown: data.CostsKnown,
		TimeScale: wait.TimeScale, Context: encode(passengerCompletionContext{Mission: *mission, Data: *data})}
	if err := r.db.PutPendingPassengerReceipt(ctx, &receipt); err != nil {
		if errors.Is(err, store.ErrActionConflict) || errors.Is(err, store.ErrInvalidInput) {
			return r.quarantinePassage(ctx, mission, data, err)
		}
		return false, err
	}
	_, err := r.releasePassengerReceiptMission(ctx, receipt)
	if errors.Is(err, store.ErrActionConflict) {
		return r.quarantinePassage(ctx, mission, data, err)
	}
	return true, err
}

func decodePassengerCompletion(receipt store.PassengerReceipt, accountID string) (passengerCompletionContext, error) {
	var archived passengerCompletionContext
	if json.Unmarshal(receipt.Context, &archived) != nil || archived.Mission.Generation != receipt.Settlement.MissionID ||
		archived.Mission.ShipID != receipt.Settlement.ShipID || archived.Mission.AccountID != accountID ||
		archived.Data.Passage == nil || len(archived.Data.Passage.Groups) == 0 || receipt.TimeScale <= 0 ||
		archived.Data.Plan.Origin != receipt.Settlement.Origin || archived.Data.Plan.Destination != receipt.Settlement.Destination {
		return archived, fmt.Errorf("invalid archived passenger mission identity or boarded batch: %w", store.ErrActionConflict)
	}
	seen := make(map[string]bool, len(archived.Data.Passage.Groups))
	var hold int64
	var started time.Time
	for _, group := range archived.Data.Passage.Groups {
		if group.ID == "" || seen[group.ID] || group.BoardedAt == nil || group.BoardedAt.IsZero() ||
			group.HoldTons <= 0 || hold > math.MaxInt64-group.HoldTons || group.DestPortID != receipt.Settlement.Destination {
			return archived, fmt.Errorf("archived passenger receipt has a malformed immutable boarded batch: %w", store.ErrActionConflict)
		}
		seen[group.ID] = true
		hold += group.HoldTons
		if started.IsZero() || group.BoardedAt.Before(started) {
			started = *group.BoardedAt
		}
	}
	if hold != receipt.Settlement.QuantityTons || !started.Equal(receipt.Settlement.StartedAt) ||
		receipt.Settlement.CompletedAt.Before(started) {
		return archived, fmt.Errorf("archived passenger receipt disagrees with its boarding evidence: %w", store.ErrActionConflict)
	}
	return archived, nil
}

func (r *Runner) releasePassengerReceiptMission(ctx context.Context, receipt store.PassengerReceipt) (bool, error) {
	archived, err := decodePassengerCompletion(receipt, r.account.ID)
	if err != nil {
		return false, err
	}
	if r.expenseOwners == nil {
		r.expenseOwners = make(map[string]string)
	}
	for _, id := range archived.Data.SettlementExpenseIDs {
		if owner := r.expenseOwners[id]; owner != "" && owner != archived.Mission.Generation {
			return false, fmt.Errorf("archived passenger expense has conflicting mission ownership: %w", store.ErrActionConflict)
		}
	}
	changed := false
	for _, id := range archived.Data.SettlementExpenseIDs {
		if r.expenseOwners[id] == "" {
			r.expenseOwners[id] = archived.Mission.Generation
			changed = true
		}
	}
	waits := decode[map[string]passengerCompletionWait](r.snapshot, passengerCompletionWaitKey)
	if _, exists := waits[archived.Mission.Generation]; exists {
		delete(waits, archived.Mission.Generation)
		r.snapshot[passengerCompletionWaitKey] = encode(waits)
		changed = true
	}
	mission, err := r.db.GetMission(ctx, r.account.ID, receipt.Settlement.ShipID)
	if err != nil {
		return false, err
	}
	matching := mission != nil && mission.Generation == archived.Mission.Generation
	if matching {
		hints := decode[map[string]bool](r.snapshot, "_passenger_boarding_ships")
		delete(hints, receipt.Settlement.ShipID)
		r.snapshot["_passenger_boarding_ships"] = encode(hints)
		changed = true
	}
	if changed {
		r.snapshot["_expense_allocations"] = encode(r.expenseOwners)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return false, err
		}
	}
	if matching {
		if err := r.db.DeleteMission(ctx, r.account.ID, receipt.Settlement.ShipID); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func (r *Runner) passengerReceiptTerminal(ctx context.Context, receipt store.PassengerReceipt) (*store.Settlement, []string, error) {
	archived, err := decodePassengerCompletion(receipt, r.account.ID)
	if err != nil {
		return nil, nil, err
	}
	events, err := r.db.PassengerEventsSince(ctx, r.account.ID, receipt.Settlement.StartedAt)
	if err != nil {
		return nil, nil, err
	}
	groups := make(map[string]gameapi.PassengerAboardGroup, len(archived.Data.Passage.Groups))
	for _, group := range archived.Data.Passage.Groups {
		groups[group.ID] = group
	}
	matched := make(map[string]bool)
	matchedEvents := make(map[string]bool)
	ids := make([]string, 0, len(groups))
	var total int64
	var completed time.Time
	wholeShipLoss := false
	allLost := true
	for _, event := range events {
		if event.EventType != "passengers_paid" && event.EventType != "passengers_lost" {
			continue
		}
		var payload passengerEventPayload
		if !decodePassengerEvent(event.Payload, &payload) || payload.ShipID != receipt.Settlement.ShipID ||
			payload.Destination != "" && payload.Destination != receipt.Settlement.Destination {
			continue
		}
		at, sourceOK := passengerTerminalAt(payload, event)
		if !sourceOK || at.Before(receipt.Settlement.StartedAt) ||
			payload.GroupID == "" && (!at.After(receipt.Settlement.StartedAt) || at.After(receipt.Settlement.CompletedAt)) {
			continue
		}
		groupID := payload.GroupID
		if groupID != "" {
			group, belongs := groups[groupID]
			if !belongs || at.Before(*group.BoardedAt) {
				continue
			}
		}
		if matchedEvents[event.EventID] {
			continue
		}
		if groupID != "" && matched[groupID] {
			return nil, nil, fmt.Errorf("archived passenger batch has conflicting terminal group events: %w", store.ErrActionConflict)
		}
		var proceeds int64
		if event.EventType == "passengers_paid" {
			if payload.FarePaidPence == nil || *payload.FarePaidPence < 0 {
				return nil, nil, fmt.Errorf("archived passenger payment lacks valid proceeds: %w", store.ErrActionConflict)
			}
			proceeds = *payload.FarePaidPence
			allLost = false
		} else if payload.Cause == "sunk" || payload.Cause == "captured" || payload.Cause == "seized" || payload.Cause == "ship_changed_hands" {
			wholeShipLoss = true
		}
		if groupID != "" {
			matched[groupID] = true
		}
		matchedEvents[event.EventID] = true
		if total > math.MaxInt64-proceeds {
			return nil, nil, fmt.Errorf("archived passenger receipts exceed integer pence: %w", store.ErrActionConflict)
		}
		total += proceeds
		ids = append(ids, event.EventID)
		if completed.IsZero() || at.After(completed) {
			completed = at
		}
	}
	if len(ids) > len(groups) {
		return nil, nil, fmt.Errorf("archived passenger batch has more terminal events than boarded groups: %w", store.ErrActionConflict)
	}
	if len(ids) == 0 || !(allLost && wholeShipLoss) && len(ids) != len(groups) {
		return nil, nil, nil
	}
	settlement := receipt.Settlement
	settlement.EventID = ids[0]
	settlement.ProceedsUnknown, settlement.ProceedsPence = false, total
	settlement.CostsKnown = receipt.CostsKnown
	settlement.CompletedAt = completed
	settlement.GameSeconds = max(int64(0), int64(completed.Sub(settlement.StartedAt).Seconds()*float64(receipt.TimeScale)))
	return &settlement, ids, nil
}

// Run before ordinary mission driving. An archived unknown completion no longer
// reserves its ship, but keeps its exact private accounting until real terminal
// passenger events can authenticate the one-way receipt upgrade.
func (r *Runner) reconcilePendingPassengerReceipts(ctx context.Context) (bool, error) {
	return r.reconcilePassengerReceiptsAt(ctx, time.Now().UTC())
}

func (r *Runner) reconcilePassengerReceiptsAt(ctx context.Context, now time.Time) (bool, error) {
	receipts, err := r.db.DuePassengerReceipts(ctx, r.account.ID, now, store.PassengerReceiptBatchLimit)
	if err != nil {
		return false, err
	}
	var failures []error
	changed := false
	quarantine := func(receipt store.PassengerReceipt, cause error) {
		closeErr := r.db.ClosePassengerReceipt(ctx, r.account.ID, receipt.Settlement.ID, "quarantined", now)
		kind, reason := "passenger_receipt_quarantined", "archived passenger receipt quarantined: "+cause.Error()
		if closeErr != nil {
			kind, reason = "passenger_receipt_quarantine_failed", "archived passenger receipt quarantine could not be persisted: "+closeErr.Error()+"; "+cause.Error()
		}
		decisionErr := r.decision(ctx, receipt.Settlement.ShipID, kind, reason, map[string]any{"receipt_id": receipt.Settlement.ID})
		if closeErr != nil || decisionErr != nil {
			failures = append(failures, errors.Join(cause, closeErr, decisionErr))
		}
	}
	failed := func(receipt store.PassengerReceipt, cause error) {
		if errors.Is(cause, store.ErrActionConflict) || errors.Is(cause, store.ErrInvalidInput) || errors.Is(cause, store.ErrNotFound) {
			quarantine(receipt, cause)
			return
		}
		decisionErr := r.decision(ctx, receipt.Settlement.ShipID, "passenger_receipt_deferred",
			"archived passenger receipt scan failed: "+cause.Error(), map[string]any{"receipt_id": receipt.Settlement.ID})
		failures = append(failures, errors.Join(cause, decisionErr))
	}
	for _, receipt := range receipts {
		if receipt.ReadError != nil {
			quarantine(receipt, receipt.ReadError)
			continue
		}
		if !now.Before(receipt.ExpiresAt) {
			if receipt.SettlementMissing {
				if err := r.db.PutSettlement(ctx, &receipt.Settlement); err != nil {
					failed(receipt, err)
					continue
				}
			}
			closeErr := r.db.ClosePassengerReceipt(ctx, r.account.ID, receipt.Settlement.ID, "expired", now)
			kind, reason := "passenger_receipt_expired", "passenger receipt closed after seven real days without terminal proof; proceeds remain unknown"
			if closeErr != nil {
				kind, reason = "passenger_receipt_deferred", "passenger receipt expiry could not be persisted: "+closeErr.Error()
			}
			decisionErr := r.decision(ctx, receipt.Settlement.ShipID, kind, reason, map[string]any{"receipt_id": receipt.Settlement.ID})
			if closeErr != nil || decisionErr != nil {
				failures = append(failures, errors.Join(closeErr, decisionErr))
			}
			continue
		}
		// Claim the next scan before doing recovery work. Repeated runner
		// passes and restarts neither reread events nor reencrypt this basis.
		if err := r.db.DeferPassengerReceipt(ctx, r.account.ID, receipt.Settlement.ID, now); err != nil {
			failed(receipt, err)
			continue
		}
		if receipt.SettlementMissing {
			if err := r.db.PutSettlement(ctx, &receipt.Settlement); err != nil {
				failed(receipt, err)
				continue
			}
		}
		released, err := r.releasePassengerReceiptMission(ctx, receipt)
		if err != nil {
			failed(receipt, err)
			continue
		}
		changed = changed || released
		settlement, events, err := r.passengerReceiptTerminal(ctx, receipt)
		if err != nil {
			failed(receipt, err)
			continue
		}
		if settlement != nil {
			if err := r.db.ResolvePassengerReceipt(ctx, settlement, events); err != nil {
				failed(receipt, err)
				continue
			}
			changed = true
		}
	}
	waits := decode[map[string]passengerCompletionWait](r.snapshot, passengerCompletionWaitKey)
	waitsChanged := false
	for generation, wait := range waits {
		mission, err := r.db.GetMission(ctx, r.account.ID, wait.ShipID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if mission == nil || mission.Generation != generation {
			delete(waits, generation)
			waitsChanged = true
		}
	}
	if waitsChanged {
		r.snapshot[passengerCompletionWaitKey] = encode(waits)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			failures = append(failures, err)
		} else {
			changed = true
		}
	}
	return changed, errors.Join(failures...)
}
