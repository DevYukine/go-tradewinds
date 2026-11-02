package engine

import (
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestFuturesTerminalListWithoutReceiptRecoversThroughJournaledSettle(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	row := futuresOwnedIntent(t, h, runner, "ask", "receipt-recovery")
	contract := decode[[]gameapi.FuturesContract](runner.snapshot, "futures")[0]
	contract.Status = "settled"
	if err := h.db.ObserveFuturesContract(h.ctx, h.account.ID, row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	// A complete later page need not retain the old contract. Its immutable
	// party and identity were already authenticated before the terminal status.
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "receipt-recovery", Phase: "committed"}
	data := missionData{Futures: &futuresState{}}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "futures-settle" {
		t.Fatalf("terminal list without receipt was not recovered: %t %v %+v", changed, err, data.Action)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || len(p.settles) != 1 || futuresNeedsReceipt(rows[0]) {
		t.Fatalf("settlement did not persist exact physical receipt: %v %+v sends=%v", err, rows, p.settles)
	}
}

func TestFuturesLegacyExitWaitDoesNotBlockUnrelatedCoveredSale(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	warehouse := p.warehouse()
	inventory := warehouse["inventory"].([]any)
	warehouse["inventory"] = append(inventory, map[string]any{"company_id": recoveryCompanyID, "port_id": "amsterdam", "commodity_id": "grain", "quantity": 1, "locked_quantity": 0})
	warehouse["used_tonnage"], warehouse["free_tonnage"] = int64(2), p.leased-2
	runner.snapshot["warehouses"] = encode([]any{warehouse})
	market := runner.stateCache.Markets["amsterdam"]
	grain := market.Books[0]
	grain.CommodityID = "grain"
	market.Books = []CommodityBook{grain}
	runner.stateCache.Markets["amsterdam"] = market
	listing := futuresListingFixture(runner, "grain-buyer", "grain-counterparty", "bid", 8_000, 1)
	listing.CommodityID = "grain"
	futuresCounterpartyFixture(runner, listing.CreatorCompanyID)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{listing})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: legacyFuturesCohort, Phase: "wind-down"}
	data := missionData{Futures: &futuresState{LeaseOwned: true, ProcurementPaid: 1_700}}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "futures-accept" {
		t.Fatalf("unquoted legacy exit blocked another covered good: %t %v %+v", changed, err, data.Action)
	}
	if mission.Phase == "futures_done" || data.Futures.LegacyClosed {
		t.Fatal("continuing ordinary futures work falsely completed legacy wind-down")
	}
}

func TestFuturesPublicListingEventsDoNotFenceFleetCash(t *testing.T) {
	for _, event := range []string{"futures_listed", "exchange_posted", "exchange_withdrawn"} {
		if !futuresDatasetEvent(event) || financialEvents[event] {
			t.Fatalf("public %s must refresh derivatives without vetoing unrelated cash work", event)
		}
	}
	for _, event := range []string{"futures_accepted", "futures_settled", "futures_defaulted", "futures_claim_paid"} {
		if !financialEvents[event] {
			t.Fatalf("own cash-changing %s lost its first-send fence", event)
		}
	}
}
