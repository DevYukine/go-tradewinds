package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func growthControlRows(t *testing.T, w *growthExecutionWorld, generation string) []store.Settlement {
	t.Helper()
	rows, err := w.h.db.SettlementHistory(w.h.ctx, w.h.account.ID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	var matched []store.Settlement
	for _, row := range rows {
		if row.MissionID == generation {
			matched = append(matched, row)
		}
	}
	return matched
}

func TestGrowthSunkCommissioningPermanentlyRetainsOperatingCash(t *testing.T) {
	t.Parallel()
	for _, unknown := range []bool{false, true} {
		name := "known"
		if unknown {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				control, data, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				return control != nil && control.Phase == "commissioning" && data.Action == nil
			})
			control, data := w.h.mission("__growth")
			before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			var paidStores int64
			for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
				var request growthExecutionRequest
				if json.Unmarshal(call.Body, &request) != nil || !request.IntoStores {
					continue
				}
				w.h.api.mu.Lock()
				receipt := append([]byte(nil), w.receipts[call.IdempotencyKey]...)
				w.h.api.mu.Unlock()
				var result tradeResult
				if json.Unmarshal(receipt, &result) != nil || !validTrade(result) {
					t.Fatal("commissioning provision receipt was not measured")
				}
				paidStores += *result.TotalGrossPence + *result.CustomsDutyPence
			}
			if paidStores <= 0 || w.h.api.mutationCount("/v1/shipyard/modify-ship") != 1 {
				t.Fatal("fixture did not actually pay stores and hardware before the hull loss")
			}
			if unknown {
				data.CostsKnown = false
				if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
					t.Fatal(err)
				}
			}
			w.h.api.mu.Lock()
			w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
			w.h.api.mu.Unlock()
			runner = w.h.runner(primaryTestKey)
			for range 12 {
				w.tick(runner)
				remaining, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				if remaining == nil {
					break
				}
			}
			w.h.missingMission("__growth")
			rows := growthControlRows(t, w, control.Generation)
			if len(rows) != 1 || rows[0].ShipID != recoveryBuiltShipID || rows[0].Kind != "aborted" ||
				rows[0].QuantityTons != 0 || rows[0].ProvisionsPence != paidStores || rows[0].AcquisitionPence != 0 ||
				rows[0].ExpensesPence != 0 || rows[0].ProceedsPence != 0 || rows[0].CostsKnown == unknown {
				t.Fatalf("sunk commissioning lost its operating cash or expensed hull/refit capital: %+v", rows)
			}
			after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			wantNet := -paidStores
			if unknown {
				wantNet = 0
			}
			if after.CommercialNetPence-before.CommercialNetPence != wantNet || after.UnknownCosts != unknown ||
				after.PositiveCycles != before.PositiveCycles || after.PositiveQuantityTons != before.PositiveQuantityTons {
				t.Fatalf("sunk commissioning produced incorrect permanent evidence: before %+v after %+v", before, after)
			}
			runner = w.h.runner(primaryTestKey)
			for range 3 {
				w.tick(runner)
			}
			if replayed := growthControlRows(t, w, control.Generation); len(replayed) != 1 ||
				replayed[0].ProvisionsPence != paidStores || replayed[0].CostsKnown != rows[0].CostsKnown {
				t.Fatalf("sunk-control restart duplicated or rewrote paid costs: %+v", replayed)
			}
		})
	}
}

func TestGrowthUnbuiltTerminalControlSettlesPreviouslyPaidTransferFee(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"growth_rejected", "growth_build"} {
		t.Run(phase, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			runner.generation = uuid.NewString()
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			started := time.Now().UTC()
			baseline := runner.ledgerIDs()
			// Pay an actual company transfer through the production journal.
			// The retained legacy control below owns that measured growth fee;
			// neither a successful action nor a built hull is invented.
			treasury := &store.Mission{AccountID: w.h.account.ID, ShipID: "__treasury", Phase: "remit", Generation: uuid.NewString()}
			transfer := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
			if err := runner.stage(w.h.ctx, treasury, &transfer, ShipState{}, "remit", "/v1/treasury/remit", map[string]any{
				"from_region": "north", "to_region": "south", "gross_amount_pence": int64(10_000),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.dispatch(w.h.ctx, treasury, &transfer); err != nil {
				t.Fatal(err)
			}
			calls := w.h.api.mutationsFor("/v1/treasury/remit")
			if len(calls) != 1 || transfer.LastActionID == "" || transfer.FeesPence <= 0 {
				t.Fatal("fixture did not actually commit a measured transfer fee")
			}
			var receipt struct {
				Fee int64 `json:"fee_pence"`
			}
			w.h.api.mu.Lock()
			response := append([]byte(nil), w.receipts[transfer.LastActionID]...)
			w.source = nil
			w.h.api.mu.Unlock()
			if json.Unmarshal(response, &receipt) != nil || receipt.Fee <= 0 {
				t.Fatal("actual transfer has no fee receipt")
			}
			if err := w.h.db.DeleteMission(w.h.ctx, w.h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			generation := uuid.NewString()
			control := missionData{
				Plan:       Plan{Kind: "spot", Origin: recoveryHomePort, Destination: recoveryFarPort, Commodity: recoveryGrain},
				GrowthLane: &growthLaneIntent{AdmissionID: generation, InitialProcurementComplete: true},
				FeesPence:  receipt.Fee, CostsKnown: true, LastActionID: transfer.LastActionID,
				StartedAt: started, StartedGameHours: runner.stateCache.Clock.Calendar.TotalGameHours, InitialLedgerIDs: baseline,
			}
			w.h.putMission("__growth", generation, phase, control)
			before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			runner = w.h.runner(primaryTestKey)
			for range 12 {
				w.tick(runner)
				remaining, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				if remaining == nil {
					break
				}
			}
			w.h.missingMission("__growth")
			rows := growthControlRows(t, w, generation)
			if len(rows) != 1 || rows[0].ShipID != "" || rows[0].Kind != "aborted" || rows[0].FeesPence != receipt.Fee ||
				rows[0].QuantityTons != 0 || rows[0].AcquisitionPence != 0 || rows[0].ProvisionsPence != 0 || rows[0].ExpensesPence != 0 {
				t.Fatalf("unbuilt terminal control dropped paid fee or invented a hull/capital expense: %+v", rows)
			}
			after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil || after.CommercialNetPence-before.CommercialNetPence != -receipt.Fee || after.PositiveCycles != before.PositiveCycles {
				t.Fatalf("unbuilt terminal fee missing from permanent history: before %+v after %+v error %v", before, after, err)
			}
		})
	}
}

func TestGrowthCommissioningBuildsActualArmamentBeforeStoresAndDeparture(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		return w.built
	})
	_, control := w.h.mission("__growth")
	w.h.api.mu.Lock()
	unarmed := w.builtRow()["ship"].(map[string]any)["cannons_count"].(int64)
	w.h.api.mu.Unlock()
	if unarmed != 0 || control.TargetCannons <= unarmed {
		t.Fatal("fixture must start with a genuinely unarmed paid hull")
	}
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		return w.h.api.mutationCount("/v1/fleet/depart") > 0
	})
	w.h.api.mu.Lock()
	calls := append([]recordedMutation(nil), w.h.api.mutations...)
	armed := w.builtRow()["ship"].(map[string]any)["cannons_count"].(int64)
	status := w.builtRow()["ship"].(map[string]any)["status"].(string)
	w.h.api.mu.Unlock()
	refit, stores, buy, departure := -1, -1, -1, -1
	for index, call := range calls {
		var request growthExecutionRequest
		if err := json.Unmarshal(call.Body, &request); err != nil {
			t.Fatal(err)
		}
		switch {
		case call.Path == "/v1/shipyard/modify-ship":
			refit = index
		case call.Path == "/v1/market/place-order" && request.IntoStores:
			stores = index
		case call.Path == "/v1/market/place-order" && request.Side == "bid":
			buy = index
		case call.Path == "/v1/fleet/depart":
			departure = index
		}
	}
	if refit < 0 || stores <= refit || buy <= stores || departure <= buy || armed != control.TargetCannons || status != "sailing" {
		t.Fatalf("production commissioning did not refit, provision, buy and actually depart in order: refit %d stores %d buy %d depart %d guns %d status %q",
			refit, stores, buy, departure, armed, status)
	}
	w.h.missingMission("__growth")
}

func TestGrowthCapitalOnlyHullLossRetainsNewCommissioningBillsOnce(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		return w.built
	})
	control, data := w.h.mission("__growth")
	if data.LastActionID == "" || data.ProvisionsPence != 0 || data.FeesPence != 0 || data.ExpensesPence != 0 {
		t.Fatal("fixture must have paid only the actual hull before its new berth bill")
	}
	w.h.api.mu.Lock()
	w.bill("lost-commissioning-berth", "north", 37)
	w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	for range 12 {
		w.tick(runner)
		remaining, _, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if remaining == nil {
			break
		}
	}
	w.h.missingMission("__growth")
	rows := growthControlRows(t, w, control.Generation)
	if len(rows) != 1 || rows[0].ExpensesPence != 37 || rows[0].AcquisitionPence != 0 ||
		rows[0].ProvisionsPence != 0 || rows[0].FeesPence != 0 || rows[0].QuantityTons != 0 || !rows[0].CostsKnown {
		t.Fatalf("capital-only control deletion erased its real bill or expensed hull capital: %+v", rows)
	}
	runner = w.h.runner(primaryTestKey)
	for range 3 {
		w.tick(runner)
	}
	if replayed := growthControlRows(t, w, control.Generation); len(replayed) != 1 || replayed[0].ExpensesPence != 37 {
		t.Fatalf("commissioning bill duplicated after the control disappeared: %+v", replayed)
	}
}

func TestGrowthMissingCommissioningTimeRetainsUnknownSettlement(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		return w.built
	})
	control, data := w.h.mission("__growth")
	data.StartedAt = time.Time{}
	baseline := string(encode(data.InitialLedgerIDs))
	terminalAction := data.LastActionID
	if terminalAction == "" || data.InitialLedgerIDs == nil || !data.CostsKnown {
		t.Fatal("fixture lacks a real paid build and known ledger boundary")
	}
	if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
		t.Fatal(err)
	}
	w.h.api.mu.Lock()
	w.bill("missing-time-new-berth-bill", "north", 37)
	w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	var frozen string
	for range 12 {
		w.tick(runner)
		current, currentData, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if current == nil {
			break
		}
		if string(encode(currentData.InitialLedgerIDs)) != baseline {
			t.Fatal("missing-time normalization replaced a present ledger boundary")
		}
		if currentData.Settlement == nil {
			continue
		}
		candidate := *currentData.Settlement
		// Store insertion supplies CreatedAt; all frozen mission accounting
		// and completion fields must otherwise survive the crash unchanged.
		candidate.CreatedAt = time.Time{}
		if current.Phase == "settling" {
			frozen = string(encode(candidate))
			runner = w.h.runner(primaryTestKey)
		}
		if current.Phase == "settled" && string(encode(candidate)) != frozen {
			t.Fatal("settlement restart changed immutable economic evidence")
		}
	}
	w.h.missingMission("__growth")
	rows := growthControlRows(t, w, control.Generation)
	if len(rows) != 1 || rows[0].CostsKnown || rows[0].ActionID != terminalAction ||
		rows[0].Kind != "aborted" || rows[0].QuantityTons != 0 || rows[0].ExpensesPence != 37 ||
		rows[0].AcquisitionPence != 0 || rows[0].ProvisionsPence != 0 || rows[0].FeesPence != 0 {
		t.Fatalf("missing commissioning time lost uncertainty, actual bills or capital separation: %+v", rows)
	}
	history, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
	if err != nil || !history.UnknownCosts {
		t.Fatalf("missing-time uncertainty disappeared from consumer history: %+v error %v", history, err)
	}
}

func TestGrowthWithdrawnRoadsteadSuccessorWithoutAnchorRetainsUnknownBasis(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		return w.built
	})
	control, data := w.h.mission("__growth")
	data.GrowthLane = &growthLaneIntent{AdmissionID: control.Generation, InitialProcurementComplete: true}
	data.CostsKnown = false
	data.LastActionID = ""
	data.InitialLedgerIDs = nil
	if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
		t.Fatal(err)
	}
	w.h.api.mu.Lock()
	w.bill("withdrawn-before-normalized-boundary", "north", 999)
	row := w.builtRow()
	row["ship"].(map[string]any)["status"] = "anchored_roadstead"
	row["ship"].(map[string]any)["dock_queued_at"] = nil
	w.source = nil
	beforeMutations := len(w.h.api.mutations)
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	var baseline string
	for range 8 {
		w.tick(runner)
		current, currentData, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		if current == nil {
			continue
		}
		if currentData.CostsKnown || currentData.LastActionID != "" || currentData.Action != nil ||
			currentData.Settlement != nil || !currentData.InitialLedgerIDs["withdrawn-before-normalized-boundary"] {
			t.Fatalf("withdrawal invented terminal evidence or erased missing historical costs: %+v", currentData)
		}
		baseline = string(encode(currentData.InitialLedgerIDs))
	}
	w.h.missingMission("__growth")
	if baseline == "" {
		t.Fatal("withdrawn control never reached its actual hull's durable closing mission")
	}
	w.h.api.mu.Lock()
	w.bill("withdrawn-after-normalized-boundary", "north", 17)
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	for range 4 {
		w.tick(runner)
		current, currentData, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		if current == nil || current.Phase != "closing" || currentData.LastActionID != "" || currentData.Action != nil ||
			currentData.CostsKnown || string(encode(currentData.InitialLedgerIDs)) != baseline {
			t.Fatal("unknown unanchored roadstead successor was deleted, rebased or assigned invented evidence")
		}
	}
	w.h.api.mu.Lock()
	afterMutations := len(w.h.api.mutations)
	w.h.api.mu.Unlock()
	if afterMutations != beforeMutations || len(w.settlementRows()) != 0 {
		t.Fatal("unknown roadstead closeout issued an incidental action or fabricated a settlement")
	}
}

func TestGrowthAnchorlessCapitalIndicationsCannotBecomeNoEffectCancellation(t *testing.T) {
	t.Parallel()
	for _, retainIdentity := range []bool{false, true} {
		name := "paid-capital-only"
		if retainIdentity {
			name = "built-identity-only"
		}
		t.Run(name, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				w.h.api.mu.Lock()
				defer w.h.api.mu.Unlock()
				return w.built
			})
			control, data := w.h.mission("__growth")
			data.LastActionID = ""
			control.Phase = "growth_rejected"
			if retainIdentity {
				data.BuildCostPence = 0
			} else {
				data.BuiltShipID = ""
			}
			if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
				t.Fatal(err)
			}
			w.h.api.mu.Lock()
			w.builtRow()["ship"].(map[string]any)["status"] = "sunk"
			w.h.api.mu.Unlock()
			runner = w.h.runner(primaryTestKey)
			for range 4 {
				w.tick(runner)
			}
			current, currentData, err := runner.loadMission(w.h.ctx, "__growth")
			if err != nil {
				t.Fatal(err)
			}
			if current == nil || current.Generation != control.Generation || currentData.LastActionID != "" ||
				currentData.Action != nil || len(w.settlementRows()) != 0 ||
				w.h.api.mutationCount("/v1/shipyard/modify-ship") != 0 ||
				w.h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatal("built-hull or capital exposure was erased as a no-effect cancellation")
			}
		})
	}
}
