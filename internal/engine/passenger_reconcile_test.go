package engine

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func setPassengerCompletionClock(r *Runner, hour int64, minute int32) {
	setPassengerCaptureClock(r, hour, minute)
	raw := decode[gameapi.Clock](r.snapshot, "clock")
	r.stateCache.Clock.RealUTC = raw.RealUTC
	r.stateCache.Clock.Calendar.TotalGameHours = hour
	r.stateCache.Clock.Calendar.TimeScale = 60
	// Calendar ticks are controlled explicitly instead of waiting on wall time.
	r.stateCache.Clock.Paused = true
	r.meta["passenger_aboard/ship-a"] = r.meta["clock"]
	r.snapshot["_meta"] = encode(r.meta)
}

func passengerCompletionFixture(t *testing.T) (*recoveryHarness, *Runner, ShipState, *store.Mission, missionData) {
	t.Helper()
	h, r, in := passengerRunnerFixture(t)
	ship := in.Ship
	ship.Ship.CurrentPortID, ship.Ship.Status = "destination", "berthed"
	ship.PassengerHoldTons, ship.Voyage = 0, nil
	r.stateCache.Ships = []ShipState{ship}
	setPassengerCompletionClock(r, 100, 30)
	boarded, promised := time.Now().UTC().Add(-2*time.Minute), time.Now().UTC().Add(time.Hour)
	group := gameapi.PassengerAboardGroup{
		ID: "completion-group", Kind: "travellers", Size: 10, HoldTons: 10,
		OriginPortID: "origin", DestPortID: "destination", FarePerHeadPence: 200, FullFarePence: 2000,
		BoardedAt: &boarded, PromisedArrivalAt: &promised,
	}
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", Origin: "origin", Destination: "destination", ShipID: ship.Ship.ID})
	data.StartedAt, data.CostsKnown = boarded, true
	data.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
	data.ProvisionsPence, data.FeesPence, data.ExpensesPence = 100, 20, 30
	mission.Phase = "sailing"
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	return h, r, ship, mission, data
}

func completePassengerRESTFallback(t *testing.T, h *recoveryHarness, r *Runner, ship ShipState, mission *store.Mission, data *missionData) store.Settlement {
	t.Helper()
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, data); err != nil || !changed {
		t.Fatalf("record first authoritative empty arrival: changed=%t error=%v", changed, err)
	}
	setPassengerCompletionClock(r, 102, 30)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, data); err != nil || !changed {
		t.Fatalf("complete REST fallback after two game hours: changed=%t error=%v", changed, err)
	}
	h.missingMission(ship.Ship.ID)
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || !rows[0].ProceedsUnknown || rows[0].CostsKnown || rows[0].EventID != "" || rows[0].ActionID != "" {
		t.Fatalf("REST fallback invented proceeds or evidence: %+v %v", rows, err)
	}
	return rows[0]
}

func recordPassengerCompletionEvent(t *testing.T, h *recoveryHarness, id, kind, groupID string, at time.Time, proceeds int64) {
	t.Helper()
	payload := map[string]any{"ship_id": "ship-a", "dest_port_id": "destination", "fare_paid_pence": proceeds, "heads": 10}
	if groupID != "" {
		payload["group_id"] = groupID
	}
	if kind == "passengers_lost" {
		payload["cause"] = "overdue"
	}
	if err := h.db.RecordEvent(h.ctx, h.account.ID, id, kind, encode(map[string]any{"timestamp": at, "payload": payload})); err != nil {
		t.Fatal(err)
	}
}

func TestPassengerRESTCompletionRequiresFreshEmptyDestinationAndFullGrace(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("first empty capture did not start grace: %t %v", changed, err)
	}
	setPassengerCompletionClock(r, 102, 29)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || changed || data.Settlement != nil {
		t.Fatalf("fractional two-hour boundary shortened: %t %v %+v", changed, err, data.Settlement)
	}
	if existing, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID); err != nil || existing == nil {
		t.Fatalf("ship released before full grace: %+v %v", existing, err)
	}
	setPassengerCompletionClock(r, 102, 30)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("full two game hours did not release ship: %t %v", changed, err)
	}
	h.missingMission(ship.Ship.ID)
}

func TestPassengerRESTCompletionRejectsStaleOrIncompleteArrivalEvidence(t *testing.T) {
	for _, name := range []string{"stale ships", "stale aboard", "wrong destination", "hold occupied", "aboard occupied", "null aboard", "missing clock", "in flight"} {
		t.Run(name, func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			switch name {
			case "stale ships":
				meta := r.meta["ships"]
				meta.Stale = true
				r.meta["ships"] = meta
			case "stale aboard":
				meta := r.meta["passenger_aboard/ship-a"]
				meta.Stale = true
				r.meta["passenger_aboard/ship-a"] = meta
			case "wrong destination":
				ship.Ship.CurrentPortID = "origin"
			case "hold occupied":
				ship.PassengerHoldTons = 1
			case "aboard occupied":
				r.snapshot["passenger_aboard"] = encode(map[string]any{"ship-a": data.Passage.Groups})
			case "null aboard":
				r.snapshot["passenger_aboard"] = json.RawMessage(`{"ship-a":null}`)
			case "missing clock":
				delete(r.snapshot, "clock")
			case "in flight":
				ship.Ship.Status = "sailing"
			}
			if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || changed {
				t.Fatalf("unproven completion started fallback grace: %t %v", changed, err)
			}
			if pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID); err != nil || len(pending) != 0 {
				t.Fatalf("unproven arrival archived fake completion: %+v %v", pending, err)
			}
		})
	}
}

func TestPassengerRESTFallbackLatePaymentRestartUpgradesSameReceiptAndExpenseOnce(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	// This ledger sample is an actual overhead debit, not guessed passenger income.
	r.snapshot["ledger"] = json.RawMessage(`[{"id":"arrival-overhead","debit_account_id":"vault","credit_account_id":"crown","amount_pence":70,"reason":"berth dues"}]`)
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	if unknown.ExpensesPence != 100 || unknown.ProvisionsPence != 100 || unknown.FeesPence != 20 {
		t.Fatalf("fallback did not preserve exact single cost allocation: %+v", unknown)
	}
	encoded, err := json.Marshal(unknown)
	if err != nil || !bytes.Contains(encoded, []byte(`"proceeds_pence":null`)) {
		t.Fatalf("REST inferred a zero payment: %s %v", encoded, err)
	}
	replacement := h.runner(primaryTestKey)
	replacement.stateCache, replacement.generation = r.stateCache, r.generation
	if changed, err := replacement.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
		t.Fatalf("restart without terminal proof fabricated progress: %t %v", changed, err)
	}
	other := missionData{InitialLedgerIDs: map[string]bool{}}
	if amount, ids := replacement.claimBillsSince(&other, "another-voyage"); amount != 0 || len(ids) != 0 {
		t.Fatalf("archived overhead was claimed by a second mission: %d %+v", amount, ids)
	}
	recordPassengerCompletionEvent(t, h, "delayed-payment", "passengers_paid", "completion-group", unknown.CompletedAt, 1800)
	if changed, err := replacement.reconcilePassengerReceiptsAt(h.ctx, time.Now().Add(store.PassengerReceiptRetryInterval+time.Second)); err != nil || !changed {
		t.Fatalf("late retained payment did not upgrade unknown receipt: %t %v", changed, err)
	}
	if changed, err := replacement.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
		t.Fatalf("resolved payment replay changed state: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != unknown.ID || rows[0].ProceedsUnknown || !rows[0].CostsKnown ||
		rows[0].ProceedsPence != 1800 || rows[0].ExpensesPence != 100 || rows[0].ProvisionsPence != 100 || rows[0].FeesPence != 20 || rows[0].EventID != "delayed-payment" {
		t.Fatalf("late proof duplicated accounting identity or expenses: %+v %v", rows, err)
	}
	history, err := h.db.EconomicHistoryWindow(h.ctx, h.account.ID, "origin|destination", time.Now().Add(time.Second), time.Hour)
	if err != nil || history.UnknownCosts || history.PositiveCycles != 1 || history.NetPence != 1580 {
		t.Fatalf("late receipt did not restore only measured economics: %+v %v", history, err)
	}
}

func TestPassengerRESTFallbackLateLostProofRestoresOriginalUnknownCosts(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	data.CostsKnown = false
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	recordPassengerCompletionEvent(t, h, "delayed-loss", "passengers_lost", "completion-group", unknown.CompletedAt, 0)
	if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || !changed {
		t.Fatalf("late loss did not resolve receipt: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != unknown.ID || rows[0].ProceedsUnknown || rows[0].CostsKnown || rows[0].ProceedsPence != 0 || rows[0].EventID != "delayed-loss" {
		t.Fatalf("real loss invented a cost basis or changed identity: %+v %v", rows, err)
	}
}

func TestPassengerTerminalEventBeforeFallbackRetainsEventAnchoredSettlement(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("first empty arrival: %t %v", changed, err)
	}
	recordPassengerCompletionEvent(t, h, "on-time-before-fallback", "passengers_paid", "completion-group", time.Now().UTC(), 2000)
	setPassengerCompletionClock(r, 102, 30)
	for range 5 {
		mission, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID)
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			break
		}
		if err := json.Unmarshal(mission.Data, &data); err != nil {
			t.Fatal(err)
		}
		if changed, err := r.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
			t.Fatalf("terminal-event-first closure stalled: %t %v", changed, err)
		}
	}
	h.missingMission(ship.Ship.ID)
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ProceedsUnknown || rows[0].ProceedsPence != 2000 || rows[0].EventID != "on-time-before-fallback" {
		t.Fatalf("ordinary terminal evidence was replaced with a REST guess: %+v %v", rows, err)
	}
	if pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID); err != nil || len(pending) != 0 {
		t.Fatalf("event-first completion created a duplicate unknown receipt: %+v %v", pending, err)
	}
}

func TestPassengerPreparedRESTFallbackRestartCannotBecomeSecondEventSettlement(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("first empty arrival: %t %v", changed, err)
	}
	wait := decode[map[string]passengerCompletionWait](r.snapshot, passengerCompletionWaitKey)[mission.Generation]
	data.Settlement = &store.Settlement{
		ID: "prepared-rest-identity", AccountID: h.account.ID, ShipID: ship.Ship.ID, MissionID: mission.Generation,
		Kind: "passage", Lane: "origin|destination", Origin: "origin", Destination: "destination", QuantityTons: 10,
		ProceedsUnknown: true, CostsKnown: false, ProvisionsPence: data.ProvisionsPence, FeesPence: data.FeesPence,
		ExpensesPence: data.ExpensesPence, StartedAt: data.StartedAt, CompletedAt: wait.ObservedAt,
		GameSeconds:  int64(wait.ObservedAt.Sub(data.StartedAt).Seconds() * float64(wait.TimeScale)),
		RevenueBasis: new("passenger_batch_allocation"),
	}
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	recordPassengerCompletionEvent(t, h, "payment-after-preparation", "passengers_paid", "completion-group", wait.ObservedAt, 2000)
	replacement := h.runner(primaryTestKey)
	replacement.stateCache, replacement.generation = r.stateCache, r.generation
	mission, data = h.mission(ship.Ship.ID)
	if changed, err := replacement.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("prepared fallback did not resume before terminal sync: %t %v", changed, err)
	}
	h.missingMission(ship.Ship.ID)
	if changed, err := replacement.reconcilePendingPassengerReceipts(h.ctx); err != nil || !changed {
		t.Fatalf("late proof did not finish prepared identity: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != "prepared-rest-identity" || rows[0].EventID != "payment-after-preparation" || rows[0].ProceedsUnknown {
		t.Fatalf("prepared fallback was replaced by a second event-keyed accounting identity: %+v %v", rows, err)
	}
}

func TestPassengerArchivedReceiptRejectsUnattributableReusedShipPayment(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	if err := h.db.RecordEvent(h.ctx, h.account.ID, "missing-source-time", "passengers_paid", json.RawMessage(`{"ship_id":"ship-a","fare_paid_pence":2000}`)); err != nil {
		t.Fatal(err)
	}
	recordPassengerCompletionEvent(t, h, "different-voyage-time", "passengers_paid", "", unknown.CompletedAt.Add(time.Minute), 2000)
	if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
		t.Fatalf("unattributed ship payment was assigned to archived voyage: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || !rows[0].ProceedsUnknown || rows[0].EventID != "" {
		t.Fatalf("ambiguous income became realized evidence: %+v %v", rows, err)
	}
	recordPassengerCompletionEvent(t, h, "source-timed-original-payment", "passengers_paid", "", unknown.CompletedAt, 2000)
	if changed, err := r.reconcilePassengerReceiptsAt(h.ctx, time.Now().Add(store.PassengerReceiptRetryInterval+time.Second)); err != nil || !changed {
		t.Fatalf("source-timed original payment failed conservative attribution: %t %v", changed, err)
	}
}

func TestPassengerArchivedReceiptDoesNotDeleteReplacementMission(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	_ = completePassengerRESTFallback(t, h, r, ship, mission, &data)
	replacementMission, replacementData := r.newVoyage(ship, Plan{Kind: "relocation", Origin: "destination", Destination: "origin", ShipID: ship.Ship.ID})
	if err := r.saveMission(h.ctx, replacementMission, &replacementData); err != nil {
		t.Fatal(err)
	}
	pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("missing archived receipt: %+v %v", pending, err)
	}
	if _, err := r.releasePassengerReceiptMission(h.ctx, pending[0]); err != nil {
		t.Fatal(err)
	}
	remaining, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID)
	if err != nil || remaining == nil || remaining.Generation != replacementMission.Generation {
		t.Fatalf("archived completion deleted reused ship's new mission: %+v %v", remaining, err)
	}
}

func TestPassengerArchivedMultiGroupReceiptWaitsForCompleteProofWithoutLedgerAttribution(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	second := data.Passage.Groups[0]
	second.ID, second.Kind, second.Size, second.HoldTons = "completion-second-group", "emigrants", 20, 20
	second.FullFarePence = 4000
	data.Passage.Groups = append(data.Passage.Groups, second)
	if err := r.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	recordPassengerCompletionEvent(t, h, "batch-late-a", "passengers_paid", "completion-group", unknown.CompletedAt, 1800)
	if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
		t.Fatalf("partial group proof realized the whole batch: %t %v", changed, err)
	}
	recordPassengerCompletionEvent(t, h, "batch-late-b", "passengers_paid", "completion-second-group", unknown.CompletedAt, 3600)
	if changed, err := r.reconcilePassengerReceiptsAt(h.ctx, time.Now().Add(store.PassengerReceiptRetryInterval+time.Second)); err != nil || !changed {
		t.Fatalf("complete batch proof did not resolve archived accounting: %t %v", changed, err)
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "origin|destination", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != unknown.ID || rows[0].ProceedsPence != 5400 ||
		rows[0].QuantityTons != 30 || rows[0].ProvisionsPence != 100 || rows[0].FeesPence != 20 || rows[0].ExpensesPence != 30 {
		t.Fatalf("late group proofs invented per-group accounting or repeated batch expenses: %+v %v", rows, err)
	}
}

func TestPassengerRESTGraceObservationSurvivesRestartAndResetsOnStaleCapture(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("first empty capture: %t %v", changed, err)
	}
	replacement := h.runner(primaryTestKey)
	replacement.stateCache, replacement.generation = r.stateCache, r.generation
	setPassengerCompletionClock(replacement, 101, 30)
	mission, data = h.mission(ship.Ship.ID)
	if changed, err := replacement.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || changed {
		t.Fatalf("restart lost fractional grace boundary: %t %v", changed, err)
	}
	meta := replacement.meta["passenger_aboard/ship-a"]
	meta.Stale = true
	replacement.meta["passenger_aboard/ship-a"] = meta
	if changed, err := replacement.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("stale capture did not withdraw unproven grace: %t %v", changed, err)
	}
	setPassengerCompletionClock(replacement, 102, 30)
	if changed, err := replacement.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed || data.Settlement != nil {
		t.Fatalf("fresh capture reused stale grace to release ship: %t %v", changed, err)
	}
	setPassengerCompletionClock(replacement, 104, 29)
	if changed, err := replacement.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || changed {
		t.Fatalf("restarted grace completed before two full game hours: %t %v", changed, err)
	}
	setPassengerCompletionClock(replacement, 104, 30)
	if changed, err := replacement.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || !changed {
		t.Fatalf("fresh restarted grace did not release ship: %t %v", changed, err)
	}
	h.missingMission(ship.Ship.ID)
}

func TestPassengerMalformedRESTCompletionQuarantinesOnlyItsPassage(t *testing.T) {
	for _, name := range []string{"null boarded_at", "duplicate group", "nonpositive hold", "wrong destination"} {
		t.Run(name, func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			switch name {
			case "null boarded_at":
				data.Passage.Groups[0].BoardedAt = nil
			case "duplicate group":
				data.Passage.Groups = append(data.Passage.Groups, data.Passage.Groups[0])
			case "nonpositive hold":
				data.Passage.Groups[0].HoldTons = 0
			case "wrong destination":
				data.Passage.Groups[0].DestPortID = "elsewhere"
			}
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if _, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			setPassengerCompletionClock(r, 102, 30)
			if changed, err := r.reconcilePassengerCompletion(h.ctx, ship, mission, &data); err != nil || changed {
				t.Fatalf("bad batch stopped or monopolized account work: %t %v", changed, err)
			}
			_, persisted := h.mission(ship.Ship.ID)
			if persisted.Passage.QuarantineReason == "" || persisted.Settlement != nil {
				t.Fatalf("malformed batch was not durably quarantined without invented accounting: %+v", persisted)
			}
			rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows.([]store.Decision)) == 0 {
				t.Fatal("quarantine lacks a decision")
			}
		})
	}
}

func TestPassengerBadArchivedReceiptDoesNotBlockHealthyNextReceipt(t *testing.T) {
	for _, name := range []string{"null context", "null boarded_at", "duplicate group", "duplicate terminal", "used terminal"} {
		t.Run(name, func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
			pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID)
			if err != nil || len(pending) != 1 {
				t.Fatalf("missing healthy receipt: %+v %v", pending, err)
			}
			bad := pending[0]
			bad.Settlement.ID, bad.Settlement.MissionID = "bad-archive", "bad-archive-mission"
			bad.CreatedAt = bad.CreatedAt.Add(-time.Minute)
			var archived passengerCompletionContext
			if err := json.Unmarshal(bad.Context, &archived); err != nil {
				t.Fatal(err)
			}
			archived.Mission.Generation = bad.Settlement.MissionID
			archived.Data.Settlement = &bad.Settlement
			archived.Data.Passage.Groups[0].ID = "bad-archive-group"
			switch name {
			case "null boarded_at":
				archived.Data.Passage.Groups[0].BoardedAt = nil
			case "duplicate group":
				archived.Data.Passage.Groups = append(archived.Data.Passage.Groups, archived.Data.Passage.Groups[0])
			}
			bad.Context = encode(archived)
			if name == "null context" {
				bad.Context = json.RawMessage(`null`)
			}
			if err := h.db.PutPendingPassengerReceipt(h.ctx, &bad); err != nil {
				t.Fatal(err)
			}
			if name == "duplicate terminal" || name == "used terminal" {
				recordPassengerCompletionEvent(t, h, "bad-proof", "passengers_paid", "bad-archive-group", unknown.CompletedAt, 1900)
				if name == "duplicate terminal" {
					recordPassengerCompletionEvent(t, h, "bad-proof-duplicate", "passengers_paid", "bad-archive-group", unknown.CompletedAt, 1900)
				} else {
					claimed := bad.Settlement
					claimed.ID, claimed.EventID, claimed.ProceedsPence = "previous-owner", "bad-proof", 1900
					claimed.ProceedsUnknown, claimed.CostsKnown = false, true
					if err := h.db.PutSettlement(h.ctx, &claimed); err != nil {
						t.Fatal(err)
					}
				}
			}
			recordPassengerCompletionEvent(t, h, "healthy-proof", "passengers_paid", "completion-group", unknown.CompletedAt, 1800)
			replacement, replacementData := r.newVoyage(ship, Plan{Kind: "relocation", Origin: "destination", Destination: "origin", ShipID: ship.Ship.ID})
			if err := r.saveMission(h.ctx, replacement, &replacementData); err != nil {
				t.Fatal(err)
			}
			if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || !changed {
				t.Fatalf("bad first receipt blocked healthy next receipt: %t %v", changed, err)
			}
			history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			foundBad, foundHealthy := false, false
			for _, row := range history {
				if row.ID == bad.Settlement.ID {
					foundBad = row.ProceedsUnknown && row.EventID == ""
				}
				if row.ID == unknown.ID {
					foundHealthy = !row.ProceedsUnknown && row.ProceedsPence == 1800 && row.EventID == "healthy-proof"
				}
			}
			if !foundBad || !foundHealthy {
				t.Fatalf("quarantine fabricated bad income or skipped healthy income: %+v", history)
			}
			if due, err := h.db.DuePassengerReceipts(h.ctx, h.account.ID, time.Now().Add(time.Hour), 10); err != nil || len(due) != 0 {
				t.Fatalf("quarantined receipt reentered scans: %+v %v", due, err)
			}
			rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 20)
			if err != nil {
				t.Fatal(err)
			}
			quarantines := 0
			for _, decision := range rows.([]store.Decision) {
				if decision.Kind == "passenger_receipt_quarantined" {
					quarantines++
				}
			}
			if quarantines != 1 {
				t.Fatalf("bad receipt lacks its single quarantine decision: %+v", rows)
			}
			remaining, err := h.db.GetMission(h.ctx, h.account.ID, ship.Ship.ID)
			if err != nil || remaining == nil || remaining.Generation != replacement.Generation {
				t.Fatalf("receipt processing disturbed unrelated replacement work: %+v %v", remaining, err)
			}
			if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
				t.Fatalf("closed receipts still obstruct ordinary work: %t %v", changed, err)
			}
		})
	}
}

func TestPassengerReceiptBackoffSurvivesRestartAndWaitsForDueScan(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("missing receipt: %+v %v", pending, err)
	}
	now := pending[0].CreatedAt.Add(time.Second)
	if changed, err := r.reconcilePassengerReceiptsAt(h.ctx, now); err != nil || changed {
		t.Fatalf("missing terminal proof fabricated progress: %t %v", changed, err)
	}
	recordPassengerCompletionEvent(t, h, "backoff-proof", "passengers_paid", "completion-group", unknown.CompletedAt, 1800)
	replacement := h.runner(primaryTestKey)
	replacement.stateCache, replacement.generation = r.stateCache, r.generation
	beforeDue := now.Add(store.PassengerReceiptRetryInterval - time.Microsecond)
	for range 3 {
		if changed, err := replacement.reconcilePassengerReceiptsAt(h.ctx, beforeDue); err != nil || changed {
			t.Fatalf("receipt scan ignored its persisted backoff: %t %v", changed, err)
		}
	}
	history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil || len(history) != 1 || !history[0].ProceedsUnknown {
		t.Fatalf("backoff reread events before its due time: %+v %v", history, err)
	}
	if changed, err := replacement.reconcilePassengerReceiptsAt(h.ctx, now.Add(store.PassengerReceiptRetryInterval)); err != nil || !changed {
		t.Fatalf("due receipt did not resume terminal matching: %t %v", changed, err)
	}
}

func TestPassengerReceiptExpiresAsUnknownWithoutReadingLateProof(t *testing.T) {
	h, r, ship, mission, data := passengerCompletionFixture(t)
	unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
	pending, err := h.db.PendingPassengerReceipts(h.ctx, h.account.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("missing receipt: %+v %v", pending, err)
	}
	recordPassengerCompletionEvent(t, h, "expired-proof", "passengers_paid", "completion-group", unknown.CompletedAt, 1800)
	if changed, err := r.reconcilePassengerReceiptsAt(h.ctx, pending[0].ExpiresAt); err != nil || changed {
		t.Fatalf("expired receipt fabricated terminal completion or blocked work: %t %v", changed, err)
	}
	if due, err := h.db.DuePassengerReceipts(h.ctx, h.account.ID, pending[0].ExpiresAt.Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Fatalf("expired unknown still scans: %+v %v", due, err)
	}
	history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil || len(history) != 1 || history[0].ID != unknown.ID || !history[0].ProceedsUnknown || history[0].CostsKnown || history[0].EventID != "" {
		t.Fatalf("expiry rewrote unknown private accounting: %+v %v", history, err)
	}
	encoded, err := json.Marshal(history[0])
	if err != nil || !bytes.Contains(encoded, []byte(`"proceeds_pence":null`)) {
		t.Fatalf("expiry invented a zero receipt: %s %v", encoded, err)
	}
	rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decision := range rows.([]store.Decision) {
		found = found || decision.Kind == "passenger_receipt_expired"
	}
	if !found {
		t.Fatalf("expiry lacks its durable decision: %+v", rows)
	}
}

func TestPassengerArchivedAndCurrentPassageDoNotReuseUntaggedSourceInEitherOrder(t *testing.T) {
	for _, archivedFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "current first", true: "archive first"}[archivedFirst], func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
			nextMission, next := r.newVoyage(ship, data.Plan)
			boarded := unknown.CompletedAt
			group := data.Passage.Groups[0]
			group.ID, group.BoardedAt = "next-passenger-group", &boarded
			next.StartedAt, next.CostsKnown = boarded, true
			next.Passage = &passageIntent{Groups: []gameapi.PassengerAboardGroup{group}, FirstBoardedAt: boarded}
			if err := r.saveMission(h.ctx, nextMission, &next); err != nil {
				t.Fatal(err)
			}
			recordPassengerCompletionEvent(t, h, "old-source-replayed", "passengers_paid", "", unknown.CompletedAt, 1800)
			resolveArchive := func() {
				t.Helper()
				if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || !changed {
					t.Fatalf("owned old source proof failed: %t %v", changed, err)
				}
			}
			if archivedFirst {
				resolveArchive()
			}
			for range 4 {
				if _, err := r.syncPassage(h.ctx, ship, nextMission, &next); err != nil {
					t.Fatal(err)
				}
			}
			if next.Passage.ReceiptsPrepared || len(next.Passage.Receipts) != 0 || next.Passage.QuarantineReason != "" {
				t.Fatalf("recording-time replay was claimed or conflicted with a later passage: %+v", next.Passage)
			}
			if !archivedFirst {
				resolveArchive()
			}
			history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
			if err != nil || len(history) != 1 || history[0].ID != unknown.ID || history[0].EventID != "old-source-replayed" {
				t.Fatalf("terminal proof was reused across passage order: %+v %v", history, err)
			}
		})
	}
}

func TestPassengerArchivedMultiGroupAnonymousProofRetainsBatchAllocation(t *testing.T) {
	for _, firstTagged := range []bool{false, true} {
		t.Run(map[bool]string{false: "anonymous batch", true: "mixed group metadata"}[firstTagged], func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			second := data.Passage.Groups[0]
			second.ID, second.Kind, second.Size, second.HoldTons = "anonymous-second-group", "emigrants", 20, 20
			second.FullFarePence = 4000
			data.Passage.Groups = append(data.Passage.Groups, second)
			if err := r.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
			// Unrelated ledger income is not terminal passenger proof and must
			// not supply missing members or proceeds of the archived batch.
			r.snapshot["ledger"] = json.RawMessage(`[{"id":"unattributed-income","debit_account_id":"passengers","credit_account_id":"vault","amount_pence":999999,"reason":"passenger fare"}]`)
			firstGroup := ""
			if firstTagged {
				firstGroup = "completion-group"
			}
			recordPassengerCompletionEvent(t, h, "anonymous-batch-a", "passengers_paid", firstGroup, unknown.CompletedAt, 1800)
			if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
				t.Fatalf("partial anonymous proof or ledger income realized a complete batch: %t %v", changed, err)
			}
			history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
			if err != nil || len(history) != 1 || !history[0].ProceedsUnknown {
				t.Fatalf("partial batch lost unknown accounting: %+v %v", history, err)
			}
			recordPassengerCompletionEvent(t, h, "anonymous-batch-b", "passengers_paid", "", unknown.CompletedAt, 3600)
			if changed, err := r.reconcilePassengerReceiptsAt(h.ctx, time.Now().Add(store.PassengerReceiptRetryInterval+time.Second)); err != nil || !changed {
				t.Fatalf("complete source-proven anonymous batch was rejected: %t %v", changed, err)
			}
			history, err = h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
			if err != nil || len(history) != 1 || history[0].ID != unknown.ID || history[0].ProceedsUnknown ||
				history[0].ProceedsPence != 5400 || history[0].QuantityTons != 30 || history[0].ProvisionsPence != 100 ||
				history[0].FeesPence != 20 || history[0].ExpensesPence != 30 ||
				history[0].RevenueBasis == nil || *history[0].RevenueBasis != "passenger_batch_allocation" {
				t.Fatalf("anonymous proof invented group assignments, ledger proceeds or repeated batch costs: %+v %v", history, err)
			}
			if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
				t.Fatalf("resolved anonymous proof was processed twice: %t %v", changed, err)
			}
		})
	}
}

func TestPassengerArchivedExcessAnonymousTerminalsQuarantineIncludingWholeLoss(t *testing.T) {
	for _, wholeLoss := range []bool{false, true} {
		t.Run(map[bool]string{false: "payment", true: "whole ship loss"}[wholeLoss], func(t *testing.T) {
			h, r, ship, mission, data := passengerCompletionFixture(t)
			unknown := completePassengerRESTFallback(t, h, r, ship, mission, &data)
			for _, id := range []string{"excess-terminal-a", "excess-terminal-b"} {
				if wholeLoss {
					if err := h.db.RecordEvent(h.ctx, h.account.ID, id, "passengers_lost", encode(map[string]any{
						"timestamp": unknown.CompletedAt,
						"payload":   map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": "destination", "cause": "sunk"},
					})); err != nil {
						t.Fatal(err)
					}
				} else {
					recordPassengerCompletionEvent(t, h, id, "passengers_paid", "", unknown.CompletedAt, 1800)
				}
			}
			if changed, err := r.reconcilePendingPassengerReceipts(h.ctx); err != nil || changed {
				t.Fatalf("excess terminal proof stopped work or realized ambiguous income: %t %v", changed, err)
			}
			history, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
			if err != nil || len(history) != 1 || history[0].ID != unknown.ID || !history[0].ProceedsUnknown || history[0].EventID != "" {
				t.Fatalf("excess terminal proof rewrote unknown accounting: %+v %v", history, err)
			}
			if due, err := h.db.DuePassengerReceipts(h.ctx, h.account.ID, time.Now().Add(time.Hour), 10); err != nil || len(due) != 0 {
				t.Fatalf("excess terminal proof was not durably quarantined: %+v %v", due, err)
			}
			rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 20)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, decision := range rows.([]store.Decision) {
				found = found || decision.Kind == "passenger_receipt_quarantined"
			}
			if !found {
				t.Fatalf("excess terminal proof lacks its quarantine decision: %+v", rows)
			}
		})
	}
}
