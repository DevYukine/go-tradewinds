package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestFuturesReceivingCapacityProtectsEveryCommodity(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity, p.leased = 0, 5
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	rows := []store.FuturesIntent{
		{PortID: "amsterdam", CommodityID: "grain", Side: "bid", Status: "active", LotTons: 2},
		{PortID: "amsterdam", CommodityID: "coal", Side: "bid", Status: "intent", LotTons: 2},
		{PortID: "london", Side: "bid", Status: "active", LotTons: 50},
	}
	if free := runner.futuresReceivingSpace(rows, "amsterdam"); free != 1 {
		t.Fatalf("cross-commodity receiving space = %d, want 1", free)
	}
	rows[1].Status = "cancelled"
	if free := runner.futuresReceivingSpace(rows, "amsterdam"); free != 3 {
		t.Fatalf("cancelled purchase kept receiving space: %d", free)
	}
}

func TestFuturesIgnoresShipCargoAndNeverPostsOwnQuotes(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	ship := ShipState{CargoManifest: map[string]int64{"water_beer": 6}}
	ship.Ship.ID, ship.Ship.CurrentPortID, ship.Ship.Status = recoveryShipLapras, "amsterdam", "berthed"
	runner.stateCache.Ships = []ShipState{ship}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: ship.Ship.ID, Generation: "inbound", Phase: "arrival"}
	cargo := missionData{Plan: Plan{Accepted: true, Destination: "amsterdam", Commodity: "water_beer", Quantity: 6}, BuyDone: true}
	if err := runner.saveMission(h.ctx, mission, &cargo); err != nil {
		t.Fatal(err)
	}
	futuresCounterpartyFixture(runner, "buyer")
	listing := futuresListingFixture(runner, "cargo-buyer", "buyer", "bid", 8000, 1)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{listing})
	control := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "stock-only", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{LegacyClosed: true}}
	if changed, err := runner.driveFuturesController(h.ctx, control, &data, nil); err != nil || changed || data.Action != nil {
		t.Fatalf("ship cargo admitted futures work: changed=%t err=%v action=%+v", changed, err, data.Action)
	}
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{})
	if changed, err := runner.driveFuturesController(h.ctx, control, &data, nil); err != nil || changed || p.mutations != 0 {
		t.Fatalf("empty market created an own quote: changed=%t err=%v mutations=%d", changed, err, p.mutations)
	}
}

func TestFuturesRejectsOtherCompaniesOfOurPlayer(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	runner.snapshot["player"] = encode(map[string]any{"companies": []any{map[string]string{"id": "sibling-company"}}})
	for _, side := range []string{"bid", "ask"} {
		listing := futuresListingFixture(runner, "sibling-"+side, "sibling-company", side, 2000, 1)
		if runner.futuresForeignListing(listing) {
			t.Fatalf("same-player %s was considered a foreign listing", side)
		}
	}
	listing := futuresListingFixture(runner, "foreign", "foreign-company", "bid", 2000, 1)
	if !runner.futuresForeignListing(listing) {
		t.Fatal("valid foreign listing was excluded with our companies")
	}
}

func TestFuturesForbiddenAdmissionIsTerminalWithoutStoppingFleet(t *testing.T) {
	h, _, runner, mission, data := futuresStagedAccept(t)
	action := *data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusForbidden, json.RawMessage(`{"code":"forbidden","detail":"same player"}`), nil); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("listing refusal stopped execution: changed=%t err=%v action=%+v", changed, err, data.Action)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != "rejected" || rows[0].ActionID != action.ID {
		t.Fatalf("forbidden listing was readmitted: %+v %v", rows, err)
	}
	if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != 0 {
		t.Fatalf("refused admission retained cash: %d %v", reserve, err)
	}
}

func TestFuturesLegacyCoverSubtractsBothDeliverySides(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 3
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	state := &futuresState{ProcurementPaid: 1000}
	rows := []store.FuturesIntent{
		{IntentID: "legacy-buy", CohortID: legacyFuturesCohort, PortID: "amsterdam", CommodityID: "water_beer", Side: "bid", Status: "settled", FuturesID: "buy", LotTons: 1,
			Outcome: encode(gameapi.SettleFuturesResult{FuturesID: "buy", Status: "settled", SettledTons: 1})},
		{IntentID: "legacy-sale", CohortID: legacyFuturesCohort, PortID: "amsterdam", CommodityID: "water_beer", Side: "ask", Status: "settled", FuturesID: "sale", LotTons: 1,
			Outcome: encode(gameapi.SettleFuturesResult{FuturesID: "sale", Status: "settled", SettledTons: 1})},
	}
	if remaining, known := futuresRemainingStock(rows, legacyFuturesCohort, state); !known || remaining != 1 {
		t.Fatalf("legacy physical balance = %d, known=%t", remaining, known)
	}
	if cover := runner.futuresCover(rows, "amsterdam", "water_beer", state); cover != 2 {
		t.Fatalf("legacy ask delivery over-reserved warehouse cover: %d", cover)
	}
}

func TestFuturesOrphanedPurchaseLiquidatesWithoutAnOwningShip(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	row := futuresOwnedIntent(t, h, runner, "bid", "orphaned-purchase")
	row.Status, row.Outcome = "settled", encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "settled", SettledTons: 1})
	if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, row.Status, row.Outcome); err != nil {
		t.Fatal(err)
	}
	control := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "surplus", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{LegacyClosed: true, Leases: map[string]futuresLeaseHolding{"amsterdam": {BaselineTons: 0, TargetTons: 20}}}}
	if changed, err := runner.closeFuturesSurplus(h.ctx, control, &data, []store.FuturesIntent{row}); err != nil || !changed || data.Action == nil || data.Action.Stage != "futures-close-stock" {
		t.Fatalf("orphaned forward stock had no exit: changed=%t err=%v action=%+v", changed, err, data.Action)
	}
	if err := runner.futuresPreflight(h.ctx, &data); err != nil {
		t.Fatalf("orphaned stock exit failed preflight: %v", err)
	}
	response := encode(map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": "ask", "filled_quantity": 1,
		"total_gross_pence": 1000, "customs_duty_pence": 0})
	if changed, err := runner.consumeFuturesResult(h.ctx, control, &data, http.StatusOK, response); err != nil || !changed || data.Action != nil {
		t.Fatalf("stock exit receipt failed: changed=%t err=%v action=%+v", changed, err, data.Action)
	}
	control, data = h.mission("__futures")
	if remaining, known := futuresRemainingStock([]store.FuturesIntent{row}, row.CohortID, data.Futures); !known || remaining != 0 {
		t.Fatalf("restart lost durable orphan exit: %d %t", remaining, known)
	}
	warehouse, _ := runner.futuresWarehouse("amsterdam", "water_beer")
	warehouse.Inventory[0].Quantity, warehouse.UsedTonnage, warehouse.FreeTonnage = 0, 0, 20
	runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
	meta := runner.meta["warehouses"]
	meta.Stale, meta.UpdatedAt = false, time.Now()
	runner.meta["warehouses"] = meta
	if changed, err := runner.releaseFuturesLease(h.ctx, control, &data, []store.FuturesIntent{row}); err != nil || !changed || data.Action.Stage != "futures-release-lease" || data.Futures.LeaseChange.TargetTons != 0 {
		t.Fatalf("liquidated orphan kept charging rent: %t %v %+v", changed, err, data.Futures.LeaseChange)
	}
}

func TestFuturesExpiredAcceptUsesOnlyFreshRequestSpecificNegativeProof(t *testing.T) {
	for _, batch := range []bool{false, true} {
		for _, outcome := range []string{"open", "accepted by other", "missing", "stale", "GET started before observation", "different terms"} {
			t.Run(map[bool]string{false: "single", true: "atomic pair"}[batch]+"/"+outcome, func(t *testing.T) {
				staged := futuresStagedAccept
				if batch {
					staged = futuresStagedCross
				}
				h, p, runner, mission, data := staged(t)
				seedAgedBulletinFixture(t, h, runner, mission, &data, store.ActionReplayWindow+time.Hour)
				action := *data.Action
				data.Action.FuturesAttemptedAt = time.Now().Add(-time.Hour)
				contracts := decode[[]gameapi.FuturesContract](runner.snapshot, "futures")
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action == nil || data.Action.FuturesObserveAfter.IsZero() {
					t.Fatalf("expired acceptance did not request post-attempt evidence: %t %v %+v", changed, err, data.Action)
				}
				meta := runner.meta["futures"]
				meta.Complete, meta.Stale, meta.Error = true, false, ""
				meta.AttemptedAt, meta.UpdatedAt = time.Now().UTC(), time.Now().UTC()
				switch outcome {
				case "accepted by other":
					other := "another-acceptor"
					contracts = contracts[:1]
					contract := &contracts[0]
					contract.Status = "active"
					if contract.Side == "bid" {
						contract.SellerCompanyID, contract.SellerMarginPence = &other, contract.StrikePricePence*int64(contract.LotTons)/4
					} else {
						contract.BuyerCompanyID, contract.BuyerMarginPence = &other, contract.StrikePricePence*int64(contract.LotTons)/4
					}
				case "missing":
					contracts = []gameapi.FuturesContract{}
				case "stale":
					meta.Stale = true
				case "GET started before observation":
					meta.AttemptedAt = data.Action.FuturesObserveAfter.Add(-time.Second)
				case "different terms":
					for i := range contracts {
						contracts[i].StrikePricePence++
					}
				}
				runner.snapshot["futures"], runner.meta["futures"] = encode(contracts), meta
				beforeResolve := data
				rejected := outcome == "open" || outcome == "accepted by other"
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed != rejected || (data.Action == nil) != rejected {
					t.Fatalf("negative proof confused absence or stale evidence with rejection: changed=%t rejected=%t err=%v action=%+v", changed, rejected, err, data.Action)
				}
				rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.ActionID != action.ID || (row.Status == "rejected") != rejected || row.FuturesID != "" {
						t.Fatalf("negative proof split the atomic request or changed its identity: %+v", row)
					}
				}
				reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
				if err != nil || (reserve == 0) != rejected || p.accepts != 0 || p.mutations != 0 {
					t.Fatalf("expired request was resent or lost its reservation: reserve=%d accepts=%d mutations=%d err=%v", reserve, p.accepts, p.mutations, err)
				}
				if !rejected {
					return
				}
				// Reproduce a crash after recording the authoritative proof, but
				// before clearing the mission. Recovery needs no later listing.
				data = beforeResolve
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				runner.snapshot["futures"] = json.RawMessage(`{"unavailable":true}`)
				if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || p.accepts != 0 {
					t.Fatalf("durable rejection proof was lost across mission recovery: %t %v %+v", changed, err, data.Action)
				}
			})
		}
	}
}

func TestFuturesNegativeProofSurvivesCompletedReceiptAndLostMissionSave(t *testing.T) {
	h, _, runner, mission, data := futuresStagedAccept(t)
	action := *data.Action
	data.Action.FuturesAttemptedAt = time.Now().Add(-time.Hour)
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil); err != nil {
		t.Fatal(err)
	}
	contracts := runner.snapshot["futures"]
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	meta := runner.meta["futures"]
	meta.Complete, meta.Stale, meta.Error = true, false, ""
	meta.AttemptedAt, meta.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	runner.snapshot["futures"], runner.meta["futures"] = contracts, meta
	beforeResolve := data
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("uninterpretable HTTP receipt did not resolve from authoritative negative proof: %t %v %+v", changed, err, data.Action)
	}
	data = beforeResolve
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["futures"] = json.RawMessage(`{"unavailable":true}`)
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
		t.Fatalf("permanent intent proof was lost behind the immutable HTTP receipt: %t %v %+v", changed, err, data.Action)
	}
}

func TestFuturesControllerFiltersHistoryBeforePlanningReserves(t *testing.T) {
	h, _, runner, mission, data := futuresStagedAccept(t)
	// Retire the fixture's unsent group so its listing is not reusable.
	if _, err := runner.replanUnsentAction(h.ctx, mission, &data, neverSentFuturesTerms); err != nil {
		t.Fatal(err)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	history := futuresListingFixture(runner, "settled-history", "foreign-history", "bid", 1700, 1)
	history.Status = "settled"
	used := futuresListingFixture(runner, "covered-listing", "buyer", "bid", 1700, 1)
	ours := futuresListingFixture(runner, "own-open", h.account.CompanyID, "bid", 1700, 1)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{history, used, ours})
	runner.db = nil
	if changed, err := runner.driveFuturesController(h.ctx, mission, &data, rows); err != nil || changed || data.Action != nil {
		t.Fatalf("ineligible history reached reserve loading or staged work: %t %v %+v", changed, err, data.Action)
	}
	runner.db = h.db
	futuresCounterpartyFixture(runner, "fresh-buyer")
	candidate := futuresListingFixture(runner, "new-covered-sale", "fresh-buyer", "bid", 1700, 1)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{history, used, ours, candidate})
	if changed, err := runner.driveFuturesController(h.ctx, mission, &data, rows); err != nil || !changed || data.Action == nil ||
		data.Action.Stage != "futures-accept" || !jsonEqual(data.Action.Body, encode(map[string]string{"futures_id": candidate.ID})) {
		t.Fatalf("filtered history prevented a viable warehouse sale: %t %v %+v", changed, err, data.Action)
	}
}
