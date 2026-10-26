package engine

import (
	"bytes"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestGrowthCycle4UnanchoredWithdrawnExposureSurvivesActualHullHandoff(t *testing.T) {
	t.Parallel()
	for _, exposure := range []string{"paid_capital", "post_baseline_bill_only", "built_id_only", "existing_markerless_successor"} {
		t.Run(exposure, func(t *testing.T) {
			w, runner := newGrowthExecutionWorld(t)
			w.until(runner, func(_ *store.Mission, _ missionData) bool {
				w.h.api.mu.Lock()
				defer w.h.api.mu.Unlock()
				return w.built
			})
			control, data := w.h.mission("__growth")
			if data.BuildCostPence <= 0 || data.BuiltShipID != recoveryBuiltShipID || data.LastActionID == "" ||
				!data.CostsKnown || data.InitialLedgerIDs == nil || data.ProvisionsPence != 0 || data.FeesPence != 0 {
				t.Fatal("fixture lacks a real paid hull with a known accounting boundary before operating purchases")
			}
			terminalAction := data.LastActionID
			before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			const baselineBillID = "cycle4-pre-baseline-bill"
			const priorBillID = "cycle4-already-owned-bill"
			const priorOwner = "cycle4-prior-accounting-episode"
			const billID = "cycle4-unanchored-harbour-bill"
			w.h.api.mu.Lock()
			w.bill(baselineBillID, "north", 11)
			w.h.api.mu.Unlock()
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			data.InitialLedgerIDs = runner.ledgerIDs()
			w.h.api.mu.Lock()
			w.bill(priorBillID, "north", 19)
			w.h.api.mu.Unlock()
			if err := runner.refresh(w.h.ctx); err != nil {
				t.Fatal(err)
			}
			claimed, ids := runner.claimBillsSince(&missionData{InitialLedgerIDs: data.InitialLedgerIDs}, priorOwner)
			if claimed != 19 || len(ids) != 1 || ids[0] != priorBillID {
				t.Fatalf("fixture did not establish an independent prior bill allocation: %d %v", claimed, ids)
			}
			if err := w.h.db.SaveSnapshot(w.h.ctx, w.h.account.ID, runner.snapshot); err != nil {
				t.Fatal(err)
			}
			// Reproduce the restored control's missing terminal-action link,
			// not an ambiguous pending request or an unknown ledger boundary.
			data.LastActionID = ""
			data.Plan = Plan{Kind: "aborted", ShipID: recoveryBuiltShipID, Destination: recoveryHomePort}
			data.GrowthLane = &growthLaneIntent{
				AdmissionID: control.Generation, InitialProcurementComplete: true,
			}
			data.GrowthWorkingCapitalPence = 0
			control.Phase = "commissioning"
			billPosted := exposure == "post_baseline_bill_only"
			w.h.api.mu.Lock()
			// Already being at roadstead is important: a fresh offshore action
			// would supply a genuine new anchor and mask the lost exposure.
			w.builtRow()["ship"].(map[string]any)["status"] = "anchored_roadstead"
			w.source = nil
			w.provisions[recoveryHomePort] = nil
			if exposure != "paid_capital" {
				// The actual hull remains committed even when a legacy record
				// retained no amount from its successful build receipt.
				data.BuildCostPence = 0
			}
			if billPosted {
				w.bill(billID, "north", 37)
			}
			w.h.api.mu.Unlock()
			if err := runner.saveMission(w.h.ctx, control, &data); err != nil {
				t.Fatal(err)
			}
			successorGeneration := growthHandoffGeneration(control.Generation, recoveryBuiltShipID)
			if exposure == "existing_markerless_successor" {
				if err := runner.refresh(w.h.ctx); err != nil {
					t.Fatal(err)
				}
				ship, _ := runner.ship(recoveryBuiltShipID)
				// Seed the already-written historical shape atomically without
				// requiring today's handoff controller to allow an unsafe handoff.
				successor, old := runner.newVoyage(ship, data.Plan)
				successor.Generation = successorGeneration
				successor.Phase = "closing"
				old.GrowthLane = data.GrowthLane
				old.BuyDone = true
				old.CostsKnown = data.CostsKnown
				old.InitialLedgerIDs = data.InitialLedgerIDs
				old.StartedAt = data.StartedAt
				old.StartedGameHours = data.StartedGameHours
				successor.Data = encode(old)
				if _, err := w.h.db.HandoffGrowthMission(w.h.ctx, w.h.account.ID, control.Generation, successor); err != nil {
					t.Fatal(err)
				}
			}
			buildsBefore := w.h.api.mutationCount("/v1/shipyard/build-ship")
			marketBefore := w.h.api.mutationCount("/v1/market/place-order")
			runner = w.h.runner(primaryTestKey)
			var retained *store.Mission
			var retainedData missionData
			for pass := range 12 {
				w.tick(runner)
				original, originalData, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				successor, successorData, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				retained = nil
				if original != nil && original.Generation == control.Generation {
					retained, retainedData = original, originalData
				} else if successor != nil && successor.Generation == successorGeneration {
					retained, retainedData = successor, successorData
				}
				if retained == nil {
					t.Fatalf("unanchored %s vanished after controller pass %d instead of remaining in the original control or actual-hull successor", exposure, pass+1)
				}
				if retainedData.Action != nil || retainedData.LastActionID != "" || retainedData.Settlement != nil {
					t.Fatalf("unanchored closing manufactured a new request or settlement anchor: %+v", retainedData)
				}
				amount, eligibleIDs := runner.unclaimedBillsSince(&retainedData)
				expectedBill := int64(0)
				expectedIDs := 0
				if billPosted {
					expectedBill = 37
					expectedIDs = 1
				}
				if amount != expectedBill || len(eligibleIDs) != expectedIDs ||
					runner.expenseOwners[billID] != "" || runner.expenseOwners[priorBillID] != priorOwner ||
					retainedData.ExpensesPence != 0 {
					t.Fatalf("read-only unanchored hold claimed or lost bills: amount %d ids %v expenses %d", amount, eligibleIDs, retainedData.ExpensesPence)
				}
				if billPosted && eligibleIDs[0] != billID {
					t.Fatal("unanchored hold substituted an unrelated bill")
				}
				if rows := w.settlementRows(); len(rows) != 0 {
					t.Fatalf("unanchored exposure fabricated permanent economic evidence: %+v", rows)
				}
				if pass == 2 || pass == 7 {
					runner = w.h.runner(primaryTestKey)
				}
			}
			if w.h.api.mutationCount("/v1/shipyard/build-ship") != buildsBefore ||
				w.h.api.mutationCount("/v1/market/place-order") != marketBefore ||
				w.h.api.mutationCount("/v1/fleet/offshore") != 0 || len(w.h.pending()) != 0 {
				t.Error("unanchored exposure was replaced by new discretionary spending or a manufactured closing action")
			}
			heldHistory, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if heldHistory.CommercialNetPence != before.CommercialNetPence || heldHistory.PositiveCycles != before.PositiveCycles ||
				heldHistory.PositiveQuantityTons != before.PositiveQuantityTons || heldHistory.UnknownCosts != before.UnknownCosts {
				t.Error("retaining unanchored exposure changed permanent realized evidence without a genuine terminal action")
			}
			// Restore the genuine completed build action, not a newly invented
			// settlement anchor. Exactly the eligible bill may now be claimed.
			if !billPosted {
				w.h.api.mu.Lock()
				w.bill(billID, "north", 37)
				w.h.api.mu.Unlock()
			}
			retainedData.LastActionID = terminalAction
			if err := runner.saveMission(w.h.ctx, retained, &retainedData); err != nil {
				t.Fatal(err)
			}
			runner = w.h.runner(primaryTestKey)
			for range 18 {
				w.tick(runner)
				original, _, err := runner.loadMission(w.h.ctx, "__growth")
				if err != nil {
					t.Fatal(err)
				}
				successor, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
				if err != nil {
					t.Fatal(err)
				}
				if len(w.settlementRows()) == 1 && original == nil && successor == nil {
					break
				}
			}
			rows := w.settlementRows()
			if len(rows) != 1 {
				t.Fatalf("restored genuine terminal evidence did not settle the retained episode once: %+v", rows)
			}
			row := rows[0]
			if row.ActionID != terminalAction || row.QuantityTons != 0 || !row.CostsKnown ||
				row.AcquisitionPence != 0 || row.ProceedsPence != 0 || row.FeesPence != 0 ||
				row.ProvisionsPence != 0 || row.ExpensesPence != 37 {
				t.Fatalf("anchored closeout lost the eligible bill, included baseline/preclaimed bills or expensed hull capital: %+v", row)
			}
			runner = w.h.runner(primaryTestKey)
			for range 3 {
				w.tick(runner)
			}
			if !bytes.Equal(encode(rows), encode(w.settlementRows())) {
				t.Error("restart duplicated or altered the anchored settlement")
			}
			if runner.expenseOwners[billID] != row.MissionID || runner.expenseOwners[priorBillID] != priorOwner {
				t.Error("anchored closeout failed to retain exact bill ownership across restart")
			}
			after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if after.CommercialNetPence-before.CommercialNetPence != -37 || after.UnknownCosts ||
				after.PositiveCycles != before.PositiveCycles || after.PositiveQuantityTons != before.PositiveQuantityTons {
				t.Errorf("anchored bill-only loss changed permanent execution evidence incorrectly: before %+v after %+v", before, after)
			}
		})
	}

}

func TestGrowthCycle4UnsentBuildCancellationIgnoresUnrelatedUpkeepLedgerDebit(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	w.until(runner, func(_ *store.Mission, _ missionData) bool {
		control, data, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		return control != nil && data.Action != nil && data.Action.Stage == "build"
	})
	control, staged := w.h.mission("__growth")
	discardedID := staged.Action.ID
	if staged.BuiltShipID != "" || staged.BuildCostPence != 0 || !staged.CostsKnown ||
		staged.LastActionID != "" || staged.InitialLedgerIDs == nil || staged.AcquisitionPence != 0 ||
		staged.ProvisionsPence != 0 || staged.FeesPence != 0 || staged.ExpensesPence != 0 {
		t.Fatal("fixture is not a known-zero-effect, unbuilt, never-sent build")
	}
	if hasAction, err := w.h.db.HasAction(w.h.ctx, w.h.account.ID, discardedID); err != nil || hasAction {
		t.Fatalf("initial build was already attempted: %t %v", hasAction, err)
	}
	before, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	const upkeepID = "cycle4-company-upkeep-between-stage-and-send"
	const upkeepPence int64 = 123
	w.h.api.mu.Lock()
	initialNorth := w.h.api.vaultBalance["north"]
	w.bill(upkeepID, "north", upkeepPence)
	w.ledger[len(w.ledger)-1]["reason"] = "company upkeep"
	w.h.api.mu.Unlock()
	// Match the production notification/capture relationship, while also
	// serving the actual debit through the authoritative ledger endpoint.
	runner.publishedFinancial.Add(1)
	runner.capturedFinancial = runner.publishedFinancial.Load()
	w.tick(runner)
	capturedDebit := false
	for _, row := range decode[[]struct {
		ID     string `json:"id"`
		Amount int64  `json:"amount_pence"`
	}](runner.snapshot, "ledger") {
		capturedDebit = capturedDebit || row.ID == upkeepID && row.Amount == upkeepPence
	}
	if !capturedDebit {
		t.Fatal("financial invalidation did not capture the real company-upkeep ledger row")
	}
	canceled, canceledData, err := runner.loadMission(w.h.ctx, "__growth")
	if err != nil {
		t.Fatal(err)
	}
	if canceled != nil && canceled.Generation == control.Generation &&
		(canceledData.LastActionID != "" || canceledData.BuiltShipID != "" || !canceledData.CostsKnown) {
		t.Fatal("proven-unsent cancellation invented paid, built or unknown exposure")
	}
	if canceled != nil && canceledData.Action != nil && canceledData.Action.ID == discardedID {
		t.Fatal("invalidated original build action remained dispatchable")
	}
	if w.h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("discarded pre-event build reached HTTP transport")
	}
	runner = w.h.runner(primaryTestKey)
	freshBodies := make(map[string][]byte)
	for range 18 {
		current, data, err := runner.loadMission(w.h.ctx, "__growth")
		if err != nil {
			t.Fatal(err)
		}
		if current != nil && data.Action != nil && data.Action.Stage == "build" {
			if data.Action.ID == discardedID {
				t.Fatal("replacement planning reused the discarded build action identity")
			}
			freshBodies[data.Action.ID] = bytes.Clone(data.Action.Body)
		}
		w.tick(runner)
		w.h.api.mu.Lock()
		built := w.built
		w.h.api.mu.Unlock()
		if built {
			break
		}
	}
	calls := w.h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 {
		t.Fatalf("unrelated upkeep trapped the no-effect control instead of allowing one fresh valid build: calls %d holds %v", len(calls), runner.holding)
	}
	call := calls[0]
	if call.IdempotencyKey == discardedID || len(freshBodies[call.IdempotencyKey]) == 0 ||
		!bytes.Equal(call.Body, freshBodies[call.IdempotencyKey]) {
		t.Error("executed build did not use its freshly persisted immutable action")
	}
	if hasAction, err := w.h.db.HasAction(w.h.ctx, w.h.account.ID, discardedID); err != nil || hasAction {
		t.Errorf("proven-unsent discarded action acquired attempted journal evidence: %t %v", hasAction, err)
	}
	if runner.expenseOwners[upkeepID] != "" {
		t.Error("no-effect cancellation claimed unrelated company overhead")
	}
	snapshot, err := w.h.db.Snapshot(w.h.ctx, w.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decode[map[string]string](snapshot, "_expense_allocations")[upkeepID] != "" {
		t.Error("no-effect cancellation persisted ownership of unrelated company overhead")
	}
	if rows := w.settlementRows(); len(rows) != 0 {
		t.Fatalf("unbuilt cancellation fabricated a hull settlement to consume company overhead: %+v", rows)
	}
	after, err := w.h.db.EconomicHistory(w.h.ctx, w.h.account.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.CommercialNetPence != before.CommercialNetPence || after.UnknownCosts != before.UnknownCosts ||
		after.PositiveCycles != before.PositiveCycles {
		t.Error("proven-no-effect cancellation changed permanent economic evidence")
	}
	w.h.api.mu.Lock()
	actualNorth := w.h.api.vaultBalance["north"]
	w.h.api.mu.Unlock()
	if actualNorth != initialNorth-upkeepPence-20_000 {
		t.Errorf("actual cash did not pay exactly the company debit and one replacement hull: got %d want %d", actualNorth, initialNorth-upkeepPence-20_000)
	}

}
