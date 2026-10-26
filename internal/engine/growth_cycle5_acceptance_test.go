package engine

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestGrowthCycle5ActionlessPostSettlementCancellationReleasesDespiteCompanyUpkeep(t *testing.T) {
	t.Parallel()
	w, runner := newGrowthExecutionWorld(t)
	first, _ := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Phase == "settled" && data.GrowthLane != nil &&
			data.GrowthLane.NextPlan != nil && data.ReturnJourney && data.Settlement != nil
	})
	firstRows := w.settlementRows()
	if len(firstRows) != 1 || firstRows[0].MissionID != first.Generation || firstRows[0].ExpensesPence != 17 ||
		!firstRows[0].CostsKnown || firstRows[0].QuantityTons <= 0 {
		t.Fatalf("fixture did not actually deliver, pay its return and permanently settle the first voyage: %+v", firstRows)
	}
	priorSettlement := encode(firstRows)
	fresh, data := w.until(runner, func(mission *store.Mission, data missionData) bool {
		return mission != nil && mission.Generation != first.Generation && mission.Phase == "provision" && data.Action == nil
	})
	ship, exists := runner.ship(recoveryBuiltShipID)
	if !exists || ship.Ship.Status != "anchored_roadstead" || ship.Ship.CurrentPortID != recoveryHomePort ||
		ship.Voyage != nil || ship.Ship.CannonsCount < 4 || ship.Stores.ProvisionsTons < data.Plan.TargetStoresTons {
		t.Fatal("fixture lacks a genuinely returned source-roadstead hull with adequate paid stores and guns")
	}
	if data.GrowthLane == nil || data.GrowthLane.NextPlan != nil || !data.GrowthLane.InitialProcurementComplete ||
		data.BuiltShipID != "" || data.BuildCostPence != 0 || data.Action != nil || data.LastActionID != "" ||
		data.Settlement != nil || !data.CostsKnown || data.InitialLedgerIDs == nil || data.AcquisitionPence != 0 ||
		data.ProceedsPence != 0 || data.ProvisionsPence != 0 || data.FeesPence != 0 || data.ExpensesPence != 0 {
		t.Fatal("production continuation did not open a distinct, known, zero-effect second accounting episode")
	}
	quantityLimit := data.GrowthLane.QuantityLimit
	if quantityLimit <= 0 {
		t.Fatal("fixture lacks a real selected continuation lot")
	}
	paths := []string{
		"/v1/shipyard/build-ship", "/v1/shipyard/modify-ship", "/v1/market/place-order",
		"/v1/fleet/dock", "/v1/fleet/offshore", "/v1/fleet/depart", "/v1/treasury/remit",
	}
	beforeMutations := make(map[string]int)
	for _, path := range paths {
		beforeMutations[path] = w.h.api.mutationCount(path)
	}
	const upkeepID = "cycle5-unrelated-post-settlement-upkeep"
	const upkeepPence int64 = 123
	w.h.api.mu.Lock()
	northBefore := w.h.api.vaultBalance["north"]
	w.source = nil
	w.bill(upkeepID, "north", upkeepPence)
	w.ledger[len(w.ledger)-1]["reason"] = "company upkeep"
	w.h.api.mu.Unlock()
	runner.publishedFinancial.Add(1)
	runner.capturedFinancial = runner.publishedFinancial.Load()
	// The production provision controller must observe lost executable depth
	// before staging any dock, buy, provision or other action for this episode.
	w.tick(runner)
	canceled, canceledData, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled != nil && (canceled.Generation != fresh.Generation || canceledData.Plan.Kind != "aborted" ||
		canceledData.Action != nil || canceledData.LastActionID != "" || canceledData.Settlement != nil ||
		!canceledData.CostsKnown || canceledData.AcquisitionPence != 0 || canceledData.ProvisionsPence != 0 ||
		canceledData.FeesPence != 0 || canceledData.ExpensesPence != 0) {
		t.Fatal("lost liquidity did not unwind or release the production continuation without an action")
	}
	capturedUpkeep := false
	for _, row := range decode[[]struct {
		ID     string `json:"id"`
		Amount int64  `json:"amount_pence"`
	}](runner.snapshot, "ledger") {
		capturedUpkeep = capturedUpkeep || row.ID == upkeepID && row.Amount == upkeepPence
	}
	if !capturedUpkeep {
		t.Fatal("fixture did not capture the actual post-baseline company-upkeep debit")
	}
	for pass := range 8 {
		if pass == 2 {
			runner = w.h.runner(primaryTestKey)
		}
		w.tick(runner)
		if !bytes.Equal(priorSettlement, encode(w.settlementRows())) {
			t.Fatal("actionless cancellation fabricated a settlement or altered the completed paid voyage")
		}
		if runner.expenseOwners[upkeepID] != "" {
			t.Fatal("actionless continuation cancellation claimed unrelated company overhead")
		}
		for _, path := range paths {
			if w.h.api.mutationCount(path) != beforeMutations[path] {
				t.Fatalf("known no-effect cancellation manufactured a mutation instead of releasing quietly: %s", path)
			}
		}
	}
	current, _, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
	if err != nil {
		t.Fatal(err)
	}
	if current != nil && current.Generation == fresh.Generation {
		t.Errorf("post-settlement actionless cancellation remained stranded across restart after unrelated upkeep: phase %s", current.Phase)
	}
	// Restored real liquidity must be usable by fresh planning, not by an
	// invented terminal action or settlement on the canceled accounting episode.
	w.h.api.mu.Lock()
	w.source = []Level{{OrderID: "cycle5-restored-executable-source", PricePence: 100, RemainingQuantity: quantityLimit}}
	w.h.api.mu.Unlock()
	runner = w.h.runner(primaryTestKey)
	stagedBuys := make(map[string][]byte)
	var resumed *store.Mission
	var resumedData missionData
	for range 28 {
		mission, candidate, err := runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		if mission != nil && candidate.Action != nil && candidate.Action.Stage == "buy" {
			if mission.Generation == fresh.Generation {
				t.Error("fresh purchase reused the canceled no-effect accounting episode")
			}
			stagedBuys[candidate.Action.ID] = bytes.Clone(candidate.Action.Body)
		}
		w.tick(runner)
		resumed, resumedData, err = runner.loadMission(w.h.ctx, recoveryBuiltShipID)
		if err != nil {
			t.Fatal(err)
		}
		if resumed != nil && resumed.Generation != fresh.Generation && resumed.Generation != first.Generation &&
			resumedData.BuyDone && resumedData.BoughtQuantity > 0 && resumedData.Action == nil {
			break
		}
	}
	if resumed == nil || resumed.Generation == fresh.Generation || resumed.Generation == first.Generation ||
		!resumedData.BuyDone || resumedData.BoughtQuantity <= 0 || resumedData.Action != nil {
		t.Fatalf("restored source liquidity could not fund one fresh valid procurement after cancellation: holds %v", runner.holding)
	}
	var freshBuys int
	var freshCash int64
	for _, call := range w.h.api.mutationsFor("/v1/market/place-order") {
		var request growthExecutionRequest
		if err := json.Unmarshal(call.Body, &request); err != nil {
			t.Fatal(err)
		}
		if request.IntoStores || request.Side != "bid" {
			continue
		}
		if _, isFresh := stagedBuys[call.IdempotencyKey]; !isFresh {
			continue
		}
		freshBuys++
		if request.Quantity <= 0 || request.Quantity > quantityLimit || !bytes.Equal(call.Body, stagedBuys[call.IdempotencyKey]) {
			t.Error("fresh procurement exceeded restored depth or changed its persisted immutable body")
		}
		var receipt struct {
			Gross   int64 `json:"total_gross_pence"`
			Customs int64 `json:"customs_duty_pence"`
		}
		w.h.api.mu.Lock()
		body := bytes.Clone(w.receipts[call.IdempotencyKey])
		w.h.api.mu.Unlock()
		if err := json.Unmarshal(body, &receipt); err != nil {
			t.Fatal(err)
		}
		freshCash += receipt.Gross + receipt.Customs
	}
	if freshBuys != 1 || freshCash <= 0 {
		t.Fatalf("recovered planning did not execute exactly one freshly journaled funded cargo purchase: buys %d cash %d", freshBuys, freshCash)
	}
	if !bytes.Equal(priorSettlement, encode(w.settlementRows())) {
		t.Error("resumed planning changed the previous immutable settlement or fabricated a cancellation settlement")
	}
	if runner.expenseOwners[upkeepID] != "" || len(w.h.pending()) != 0 {
		t.Error("resumed procurement claimed unrelated upkeep or retained an unresolved mutation")
	}
	snapshot, err := w.h.db.Snapshot(w.h.ctx, w.h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if decode[map[string]string](snapshot, "_expense_allocations")[upkeepID] != "" {
		t.Error("cancellation persisted a false claim on unrelated company overhead")
	}
	w.h.api.mu.Lock()
	northAfter := w.h.api.vaultBalance["north"]
	w.h.api.mu.Unlock()
	if northAfter != northBefore-upkeepPence-freshCash {
		t.Errorf("post-settlement cash identity lost or duplicated an expense: got %d want %d", northAfter, northBefore-upkeepPence-freshCash)
	}

}
