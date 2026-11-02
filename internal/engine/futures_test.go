package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func futuresStagedAccept(t *testing.T) (*recoveryHarness, *futuresMoneyAPI, *Runner, *store.Mission, missionData) {
	t.Helper()
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	futuresCounterpartyFixture(runner, "buyer")
	listing := futuresListingFixture(runner, "covered-listing", "buyer", "bid", 1700, 1)
	p.rows[listing.ID] = map[string]any{"id": listing.ID, "creator_company_id": listing.CreatorCompanyID,
		"port_id": listing.PortID, "commodity_id": listing.CommodityID, "side": listing.Side, "lot_tons": listing.LotTons,
		"strike_price_pence": listing.StrikePricePence, "status": listing.Status, "created_at": listing.CreatedAt,
		"updated_at": listing.UpdatedAt, "matures_at": listing.MaturesAt, "buyer_company_id": listing.CreatorCompanyID,
		"seller_margin_pence": int64(0), "buyer_margin_pence": int64(0)}
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{listing})
	priced, err := runner.evaluateFuturesListing(h.ctx, listing, nil, 100_000, 1)
	if err != nil || priced == nil {
		t.Fatalf("covered listing failed real pricing: %+v %v", priced, err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "covered-stock", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{}}
	if changed, err := runner.stageFuturesAdmission(h.ctx, mission, &data, []store.FuturesIntent{*priced}); err != nil || !changed || data.Action == nil {
		t.Fatalf("covered acceptance did not stage: %t %v, holds=%v", changed, err, runner.holding)
	}
	return h, p, runner, mission, data
}

func futuresOwnedIntent(t *testing.T, h *recoveryHarness, runner *Runner, side, cohort string) store.FuturesIntent {
	t.Helper()
	_, upper, _ := runner.serverBounds()
	price, margin, cash, tons := int64(1700), int64(425), int64(425), int64(1)
	if side == "bid" {
		price, margin, cash, tons = 2300, 575, 2937, 0
	}
	row := store.FuturesIntent{AccountID: h.account.ID, IntentID: "owned-" + side, ActionID: "created-" + side, CohortID: cohort,
		PortID: "amsterdam", CommodityID: "water_beer", Side: side, LotTons: 1, Status: "intent",
		StrikePricePence: price, ReservedTons: tons, MarginPence: margin, ReservedCashPence: cash, ExposurePence: cash,
		CreatedAt: upper.Add(-time.Hour), MaturesAt: upper.Add(-time.Minute)}
	if err := h.db.PutFuturesIntent(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
	company := h.account.CompanyID
	contract := gameapi.FuturesContract{ID: "contract-" + side, CreatorCompanyID: company, PortID: row.PortID, CommodityID: row.CommodityID,
		Side: side, LotTons: 1, StrikePricePence: price, Status: "active", CreatedAt: row.CreatedAt, MaturesAt: row.MaturesAt, UpdatedAt: upper}
	if side == "ask" {
		contract.SellerCompanyID, contract.SellerMarginPence = &company, margin
	} else {
		contract.BuyerCompanyID, contract.BuyerMarginPence = &company, margin
	}
	if err := h.db.ObserveFuturesContract(h.ctx, h.account.ID, row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
	row.FuturesID, row.Status, row.ObservedMaturesAt = contract.ID, "active", &contract.MaturesAt
	contracts := decode[[]gameapi.FuturesContract](runner.snapshot, "futures")
	runner.snapshot["futures"] = encode(append(contracts, contract))
	return row
}

func TestFuturesSettledEventWithoutDefaultAmountsAdvancesLegacyWindDown(t *testing.T) {
	for _, kind := range []string{"valid", "missing cash evidence", "wrong notional", "too many tons", "foreign party", "default missing amounts"} {
		t.Run(kind, func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			p.quantity, p.leased = 0, 2
			runner := futuresMoneyRunner(t, h)
			refreshMarqueExecution(t, h, runner)
			row := futuresOwnedIntent(t, h, runner, "ask", legacyFuturesCohort)
			runner.snapshot["futures"] = encode([]gameapi.FuturesContract{})
			if kind != "missing cash evidence" {
				if err := h.db.RecordFuturesCashFlow(h.ctx, h.account.ID, row.IntentID, "sale-principal", 1700,
					encode(map[string]any{"reason": "principal", "metadata": map[string]string{"futures_id": row.FuturesID}})); err != nil {
					t.Fatal(err)
				}
			}
			status := "settled"
			payload := map[string]any{"futures_id": row.FuturesID, "status": status, "settled_tons": 1,
				"notional_pence": 1700, "port_id": row.PortID, "commodity_id": row.CommodityID, "seller_company_id": h.account.CompanyID}
			switch kind {
			case "wrong notional":
				payload["notional_pence"] = 1701
			case "too many tons":
				payload["settled_tons"] = 2
			case "foreign party":
				payload["seller_company_id"] = "someone-else"
			case "default missing amounts":
				status = "defaulted"
				payload["status"], payload["settled_tons"] = status, 0
			}
			event := encode(map[string]any{"id": "private-outcome", "company_id": h.account.CompanyID, "port_id": row.PortID,
				"timestamp": p.now, "payload": payload})
			if err := h.db.RecordEvent(h.ctx, h.account.ID, "private-outcome", "futures_"+status, event); err != nil {
				t.Fatal(err)
			}
			mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: legacyFuturesCohort, Phase: "wind-down"}
			data := missionData{Futures: &futuresState{LeaseOwned: true, ProcurementPaid: 1700, InitialLease: 0}}
			if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("legacy outcome ledger = %+v, error = %v", rows, err)
			}
			if kind != "valid" {
				if rows[0].Status != "active" || data.Action != nil {
					t.Fatalf("invalid private evidence released a physical obligation: %+v %+v", rows[0], data.Action)
				}
				return
			}
			if rows[0].Status != "settled" || data.Action == nil || data.Action.Stage != "futures-close-lease" {
				t.Fatalf("successful omitted-default receipt did not unblock rent closure: %+v %+v", rows[0], data.Action)
			}
			if mission.Phase == "futures_done" {
				t.Fatal("legacy mission finished before its actual lease returned to baseline")
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if p.leased != 0 {
				t.Fatalf("legacy wind-down left %d rented tons", p.leased)
			}
		})
	}
}

func TestFuturesLegacyDoneRequiresObservedInitialLease(t *testing.T) {
	for _, owned := range []bool{false, true} {
		for _, phase := range []string{"wind-down", "futures_done"} {
			t.Run(map[bool]string{false: "lost ownership flag", true: "owned lease"}[owned]+"/"+phase, func(t *testing.T) {
				h, p := newFuturesMoneyAPI(t)
				p.quantity, p.leased = 0, 7
				runner := futuresMoneyRunner(t, h)
				refreshMarqueExecution(t, h, runner)
				mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: legacyFuturesCohort, Phase: phase}
				data := missionData{Futures: &futuresState{LeaseOwned: owned, InitialLease: 5}}
				if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if mission.Phase == "futures_done" || mission.Generation != legacyFuturesCohort || data.Action == nil ||
					data.Action.Stage != "futures-close-lease" || !jsonEqual(data.Action.Body, encode(map[string]any{"port_id": "amsterdam", "target_leased_tonnage": 5})) {
					t.Fatalf("unclosed actual lease escaped legacy wind-down: %+v %+v", mission, data)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if p.leased != 5 {
					t.Fatalf("wind-down changed unrelated baseline lease: %d", p.leased)
				}
				meta := runner.meta["warehouses"]
				meta.Stale = true
				runner.meta["warehouses"] = meta
				refreshMarqueExecution(t, h, runner)
				if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if data.Action != nil || mission.Phase != "futures_done" && mission.Generation == legacyFuturesCohort {
					t.Fatalf("observed baseline lease did not finish wind-down: %+v %+v", mission, data)
				}
			})
		}
	}
}

func TestFuturesMatureSettlementDoesNotDependOnLiveQuayBook(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	row := futuresOwnedIntent(t, h, runner, "ask", "covered-sale")
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "controller", Phase: "committed"}
	data := missionData{Futures: &futuresState{IntentID: row.IntentID, PortID: row.PortID, CommodityID: row.CommodityID}}
	meta := runner.meta["books/amsterdam"]
	meta.Stale, meta.Error = true, "ship left the quay"
	runner.meta["books/amsterdam"] = meta
	if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil || data.Action == nil || data.Action.Stage != "futures-settle" {
		t.Fatalf("mature settlement required an irrelevant live price: %v %+v", err, data.Action)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if len(p.settles) != 1 || p.settles[0] != row.FuturesID {
		t.Fatalf("mature settlement was not sent exactly once: %+v", p.settles)
	}
}

func TestFuturesDefaultMissingReceiptRetainsCashAndLease(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity, p.leased = 0, 2
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	row := futuresOwnedIntent(t, h, runner, "bid", legacyFuturesCohort)
	if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "defaulted",
		encode(map[string]any{"futures_id": row.FuturesID, "status": "defaulted", "settled_tons": 0})); err != nil {
		t.Fatal(err)
	}
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: legacyFuturesCohort, Phase: "wind-down"}
	data := missionData{Futures: &futuresState{LeaseOwned: true}}
	if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil || mission.Phase == "futures_done" ||
		data.Action == nil || data.Action.Stage != "futures-settle" ||
		!jsonEqual(data.Action.Body, encode(map[string]string{"futures_id": row.FuturesID})) || p.leased != 2 {
		t.Fatalf("default without claim and penalty evidence lost receipt recovery or released its lease: %v %+v", err, data)
	}
	if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != 2362 {
		t.Fatalf("unknown default released principal and customs reserve: %d %v", reserve, err)
	}
}

func TestFuturesConflictingTerminalReceiptHoldsWithoutFailingAccount(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	row := futuresOwnedIntent(t, h, runner, "ask", "covered-sale")
	defaulted := encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "defaulted", PenaltyPaidPence: 425})
	if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "defaulted", defaulted); err != nil {
		t.Fatal(err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "controller", Phase: "committed"}
	data := missionData{Futures: &futuresState{IntentID: row.IntentID, PortID: row.PortID, CommodityID: row.CommodityID}}
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "futures-settle", "/v1/future/settle", map[string]string{"futures_id": row.FuturesID}); err != nil {
		t.Fatal(err)
	}
	action := data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	settled := encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "settled", SettledTons: 1})
	if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, settled, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || data.Action == nil || data.Action.ID != action.ID {
		t.Fatalf("conflicting terminal evidence failed the account or discarded its receipt: %v %+v", err, data.Action)
	}
	if len(runner.holding) == 0 || !strings.Contains(runner.holding[len(runner.holding)-1].Reason, "futures") {
		t.Fatal("conflicting settlement evidence lacks its optional futures hold")
	}
}

func TestFuturesAcceptancePreflightRejectsChangedBodyAndCustomsShortfall(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.cash = 100_000_000
	h.api.setVaultBalance("north_sea", p.cash)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	p.quantity = 2
	refreshMarqueExecution(t, h, runner)
	sale := futuresListingFixture(runner, "buyer-offer", "buyer", "bid", 2000, 2)
	listing := futuresListingFixture(runner, "supplier-offer", "supplier", "ask", 500, 2)
	sale.MaturesAt = listing.MaturesAt.Add(time.Minute)
	futuresCounterpartyFixture(runner, sale.CreatorCompanyID)
	futuresCounterpartyFixture(runner, listing.CreatorCompanyID)
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{sale, listing})
	pair, err := runner.evaluateFuturesCross(h.ctx, sale, listing, nil, 100_000)
	if err != nil || len(pair) != 2 {
		t.Fatalf("covered cross fixture was not admitted: %+v %v", pair, err)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "cross", Phase: "evaluating"}
	data := missionData{Futures: &futuresState{}}
	if _, err := runner.stageFuturesAdmission(h.ctx, mission, &data, pair); err != nil {
		t.Fatal(err)
	}
	if err := runner.futuresPreflight(h.ctx, &data); err != nil {
		t.Fatalf("fully reserved immutable acceptance failed: %v", err)
	}
	body := bytes.Clone(data.Action.Body)
	data.Action.Body = encode(map[string]string{"futures_id": "different-listing"})
	if err := runner.futuresPreflight(h.ctx, &data); !errors.Is(err, errFuturesTerms) {
		t.Fatalf("changed immutable acceptance was not rejected: %v", err)
	}
	data.Action.Body = body
	market := runner.stateCache.Markets[listing.PortID]
	market.CustomsBPS = 1
	runner.stateCache.Markets[listing.PortID] = market
	if err := runner.futuresPreflight(h.ctx, &data); !errors.Is(err, errFuturesTerms) {
		t.Fatalf("one-penny customs shortfall became a recoverable delay: %v", err)
	}
}

func TestFuturesLedgerCashFlowAndPairPnL(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: "measured-pair", Phase: "committed"}
	data := missionData{Futures: &futuresState{PortID: "amsterdam", CommodityID: "water_beer"}}
	rows := []store.FuturesIntent{
		futuresOwnedIntent(t, h, runner, "ask", "measured-pair"),
		futuresOwnedIntent(t, h, runner, "bid", "measured-pair"),
	}
	runner.stateCache.Vaults = append(runner.stateCache.Vaults,
		Vault{AccountID: "pilot-vault", AccountType: "vault", BankingRegion: "north_sea"},
		Vault{AccountID: "pilot-escrow", AccountType: "escrow", BankingRegion: "north_sea"})
	// Match the real vault snapshot's object shape; decoding it as []Vault
	// must not silently replace the authoritative stateCache vault inventory.
	runner.snapshot["vaults"] = encode(map[string]any{"vaults": runner.stateCache.Vaults})
	var ledger []map[string]any
	for _, row := range rows {
		receipt := encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "settled", SettledTons: 1})
		if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "settled", receipt); err != nil {
			t.Fatal(err)
		}
		debit, credit, amount := "counterparty", "pilot-vault", int64(1700)
		if row.Side == "bid" {
			debit, credit, amount = "pilot-vault", "counterparty", 2362
		}
		ledger = append(ledger,
			map[string]any{"id": "principal-" + row.Side, "debit_account_id": debit, "credit_account_id": credit, "amount_pence": amount, "metadata": map[string]string{"futures_id": row.FuturesID}},
			map[string]any{"id": "margin-" + row.Side, "debit_account_id": "pilot-escrow", "credit_account_id": "pilot-vault", "amount_pence": row.MarginPence, "metadata": map[string]string{"futures_id": row.FuturesID}})
	}
	runner.snapshot["ledger"] = encode(ledger)
	if err := runner.observeFutures(h.ctx, rows); err != nil {
		t.Fatal(err)
	}
	if err := runner.recordFuturesPnL(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	measured, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pnl int64
	for _, row := range measured {
		want := int64(1700)
		if row.Side == "bid" {
			want = -2362
		}
		if row.CashDeltaPence == nil || *row.CashDeltaPence != want || row.RealizedPnLPence == nil {
			t.Fatalf("ledger did not establish measured cash and P&L: %+v", row)
		}
		pnl += *row.RealizedPnLPence
	}
	if pnl != -662 {
		t.Fatalf("pair P&L = %d, want signed loss -662", pnl)
	}
}

func TestFuturesLegacyAccountingSurvivesAutomaticControllerCutover(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity, p.leased = 0, 0
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	for _, side := range []string{"ask", "bid"} {
		row := futuresOwnedIntent(t, h, runner, side, legacyFuturesCohort)
		cash := int64(1700)
		if side == "bid" {
			cash = -2362
		}
		if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "settled",
			encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "settled", SettledTons: 1})); err != nil {
			t.Fatal(err)
		}
		if err := h.db.RecordFuturesCashFlow(h.ctx, h.account.ID, row.IntentID, "legacy-"+side, cash, encode(map[string]string{"futures_id": row.FuturesID})); err != nil {
			t.Fatal(err)
		}
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: legacyFuturesCohort, Phase: "futures_done"}
	data := missionData{Futures: &futuresState{LeaseOwned: true, ProcurementPaid: 2000, LiquidationPaid: 1000, LiquidatedTons: 1}}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || !data.Futures.LegacyClosed || mission.Generation == legacyFuturesCohort {
		t.Fatalf("completed legacy wind-down did not cut over with its accounting: %t %v %+v", changed, err, data.Futures)
	}
	for range 2 {
		if err := runner.recordFuturesPnL(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var pnl int64
	for _, row := range rows {
		if row.RealizedPnLPence == nil {
			t.Fatalf("automatic controller discarded measured legacy costs: %+v", row)
		}
		pnl += *row.RealizedPnLPence
	}
	if pnl != -1662 {
		t.Fatalf("legacy P&L lost procurement or liquidation after cutover: %d", pnl)
	}
}

func TestFuturesTransientUnsentAcceptReleasesActionKeepsCoverage(t *testing.T) {
	h, _, runner, mission, data := futuresStagedAccept(t)
	meta := runner.meta["books/amsterdam"]
	meta.Stale = true
	runner.meta["books/amsterdam"] = meta
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action != nil || !data.CooldownUntil.After(time.Now()) {
		t.Fatalf("never-sent acceptance remained attached: %t %v %+v", changed, err, data.Action)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != "intent" {
		t.Fatalf("transient precondition discarded the immutable admission: %+v %v", rows, err)
	}
	if reserved, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserved == 0 {
		t.Fatalf("transient admission lost its protected %dp: %v", reserved, err)
	}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || changed || data.Action != nil {
		t.Fatalf("unchanged stale evidence hot-looped a new action: %t %v %+v", changed, err, data.Action)
	}
	refreshMarqueExecution(t, h, runner)
	data.CooldownUntil = time.Time{}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil {
		t.Fatalf("fresh authority did not recover transient admission: %t %v", changed, err)
	}
}

func TestFuturesRetryableAcceptRejectionsKeepAdmission(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedAccept(t)
			action := *data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(time.Hour)
			var retryAfter *time.Time
			if status == http.StatusTooManyRequests {
				retryAfter = &deadline
			}
			if err := h.db.CompleteAction(h.ctx, action.ID, status, json.RawMessage(`{"code":"retry_later"}`), retryAfter); err != nil {
				t.Fatal(err)
			}
			_, err := runner.dispatch(h.ctx, mission, &data)
			if data.Action != nil || status == 429 && err != nil || status != 429 && (err == nil || !strings.Contains(err.Error(), "execution stopped")) {
				t.Fatalf("retryable refusal lost fail-stop or completed-request handling: %v %+v", err, data.Action)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || len(rows) != 1 || rows[0].Status != "intent" || rows[0].ActionID == action.ID {
				t.Fatalf("retryable refusal discarded the immutable admission: %+v %v", rows, err)
			}
			if status == 429 && data.CooldownUntil.Before(deadline) {
				t.Fatal("429 lost durable Retry-After")
			}
		})
	}
}

func TestFuturesOrderPreflightRejectsRetiredBuysAndUnboundedSales(t *testing.T) {
	for _, test := range []struct {
		name, side      string
		price, quantity int64
	}{
		{"overflow order", "ask", math.MaxInt64, 2},
		{"retired procurement", "bid", 2100, 1},
		{"zero quantity", "ask", 2100, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &Runner{}
			action := &durableAction{Stage: "futures-close-stock", Method: http.MethodPost, Path: "/v1/market/place-order",
				Body: encode(map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": test.side, "time_in_force": "ioc", "quantity": test.quantity, "price_pence": test.price})}
			if err := runner.futuresOrderPreflight(action); err == nil {
				t.Fatal("unbounded or retired warehouse order passed preflight")
			}
		})
	}
}

func TestFuturesUnresolvedAndMalformedEvidenceNeverBlockFleet(t *testing.T) {
	h, p, runner, mission, data := futuresStagedAccept(t)
	delete(p.rows, "covered-listing")
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{})
	action := *data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil); err != nil {
		t.Fatal(err)
	}
	h.api.setFleet([]map[string]any{berthedLaprasRow()})
	futuresMoneyPasses(t, h, runner, 6)
	shipMission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipLapras)
	if err != nil || shipMission == nil {
		t.Fatalf("optional futures action blocked ordinary ship work: %+v %v holds=%v", shipMission, err, runner.holding)
	}
	mission, data = h.mission("__futures")
	if data.Action == nil {
		t.Fatal("unresolved acceptance without listing evidence was guessed away")
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner.snapshot["futures"] = json.RawMessage(`[{"creator_company_id":"` + h.account.CompanyID + `","seller_margin_pence":9223372036854775808,"buyer_margin_pence":0,"strike_price_pence":1700,"lot_tons":1}]`)
	if err := runner.observeFutures(h.ctx, rows); err == nil {
		t.Fatal("overflowing list finance was accepted as zero")
	}
	data.Action = nil
	if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
		t.Fatalf("optional invalid evidence stopped account work: %v", err)
	}
	if len(runner.holding) == 0 || !strings.Contains(runner.holding[len(runner.holding)-1].Reason, "futures") {
		t.Fatal("invalid optional evidence has no disclosed hold")
	}
	runner.assessShip("__futures")
	if runner.holdScope != "__futures" || runner.holding[len(runner.holding)-1].ShipID != "" {
		t.Fatal("internal futures hold was attributed to a synthetic ship")
	}
}

func TestFuturesLeaseRefusalRequiresChangedWarehouseNotExitQuote(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	data := missionData{Futures: &futuresState{RejectedEvidence: map[string]string{}}}
	evidence, err := runner.futuresRejectionEvidence(h.ctx, &data, "futures-close-lease")
	if err != nil {
		t.Fatal(err)
	}
	data.Futures.RejectedEvidence["futures-close-lease"] = evidence
	market := runner.stateCache.Markets["amsterdam"]
	market.Books[0].Depth.Bids[0].PricePence++
	if refused, err := runner.futuresRefused(h.ctx, &data, "futures-close-lease"); err != nil || !refused {
		t.Fatal("unrelated exit quote re-enabled a definitive lease-close refusal")
	}
	warehouse, _ := runner.futuresWarehouse("amsterdam", "water_beer")
	warehouse.LeasedTonnage++
	runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
	if refused, err := runner.futuresRefused(h.ctx, &data, "futures-close-lease"); err != nil || refused {
		t.Fatal("changed warehouse authority did not permit lease-close reconsideration")
	}
}

func TestFuturesCommittedUnknownStockUsesInventoryWithoutInventingCash(t *testing.T) {
	for _, stage := range []string{"futures-close-stock"} {
		t.Run(stage, func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedAccept(t)
			data.Action = nil
			side, delta := "ask", int64(-1)
			if err := runner.stage(h.ctx, mission, &data, ShipState{}, stage, "/v1/market/place-order",
				map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": side, "time_in_force": "ioc",
					"quantity": 1, "price_pence": 2100}); err != nil {
				t.Fatal(err)
			}
			data.Action.FuturesObserveAfter = time.Now().Add(-time.Hour)
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil); err != nil {
				t.Fatal(err)
			}
			warehouse, _ := runner.futuresWarehouse("amsterdam", "water_beer")
			warehouse.Inventory[0].Quantity += delta
			runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("authoritative inventory did not resolve committed stock: %t %v %+v", changed, err, data.Action)
			}
			if !data.Futures.LiquidationUnknown || data.Futures.LiquidationPaid != 0 || data.Futures.LiquidatedTons != 1 {
				t.Fatalf("inventory observation invented cash or failed safe wind-down: %+v phase=%s", data.Futures, mission.Phase)
			}
			if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 0 {
				t.Fatal("committed stock was sent again")
			}
		})
	}
}

func TestFuturesErrorBoundaryPreservesJournalIntegrityAndDatabaseFailures(t *testing.T) {
	t.Run("journal integrity", func(t *testing.T) {
		h, _, runner, mission, data := futuresStagedAccept(t)
		action := data.Action
		if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
			t.Fatal(err)
		}
		data.Action.Body = append(append([]byte(nil), action.Body...), ' ')
		if _, err := runner.dispatch(h.ctx, mission, &data); !errors.Is(err, store.ErrActionConflict) {
			t.Fatalf("immutable journal mismatch became an optional hold: %v", err)
		}
	})
	t.Run("database", func(t *testing.T) {
		h, _, runner, mission, data := futuresStagedAccept(t)
		if err := h.db.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := runner.dispatch(h.ctx, mission, &data); err == nil {
			t.Fatal("closed database dispatch became an optional hold")
		}
		if _, err := runner.driveFutures(h.ctx, mission, &data); err == nil {
			t.Fatal("closed database observation became an optional hold")
		}
	})
}

func TestFuturesObservedAcceptSurvivesLaterRejectionReceipt(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, p, runner, mission, data := futuresStagedAccept(t)
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(action.Method, action.Path, bytes.NewReader(action.Body))
			request.Header.Set("Idempotency-Key", action.ID)
			capture := httptest.NewRecorder()
			p.ServeHTTP(capture, request)
			if capture.Code != http.StatusOK {
				t.Fatalf("committed-timeout fixture did not accept listing: %d %s", capture.Code, capture.Body.String())
			}
			meta := runner.meta["futures"]
			meta.Stale = true
			runner.meta["futures"] = meta
			refreshMarqueExecution(t, h, runner)
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.observeFutures(h.ctx, rows); err != nil {
				t.Fatal(err)
			}
			var retryAfter *time.Time
			if status == http.StatusTooManyRequests {
				deadline := time.Now().Add(time.Minute)
				retryAfter = &deadline
			}
			if err := h.db.CompleteAction(h.ctx, action.ID, status, json.RawMessage(`{"code":"retry_later"}`), retryAfter); err != nil {
				t.Fatal(err)
			}
			_, err = runner.dispatch(h.ctx, mission, &data)
			authFailure := status == 401
			if authFailure && (err == nil || !strings.Contains(err.Error(), "execution stopped")) ||
				!authFailure && err != nil || data.Action != nil {
				t.Fatalf("observation race lost fail-stop or reconciliation: status=%d err=%v action=%+v", status, err, data.Action)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || len(rows) != 1 || rows[0].FuturesID == "" || rows[0].Status != "active" || rows[0].ActionID != action.ID {
				t.Fatalf("receipt rejected or readmitted an already observed listing: %+v %v", rows, err)
			}
			wantReserve := max(int64(0), rows[0].ReservedCashPence-rows[0].MarginPence)
			if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != wantReserve || rows[0].ReservedTons != 1 {
				t.Fatalf("live listing lost covered cash or stock: reserve=%d want=%d tons=%d %v", reserve, wantReserve, rows[0].ReservedTons, err)
			}
		})
	}
}

func TestFuturesClockPreconditionsHoldWithoutStoppingAccount(t *testing.T) {
	for _, failure := range []string{"stale", "paused", "zero scale"} {
		t.Run(failure, func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedAccept(t)
			switch failure {
			case "stale":
				meta := runner.meta["clock"]
				meta.Stale = true
				runner.meta["clock"] = meta
			case "paused":
				runner.stateCache.Clock.Paused = true
			case "zero scale":
				runner.stateCache.Clock.Calendar.TimeScale = 0
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); !errors.Is(err, errFuturesPrecondition) {
				t.Fatalf("clock failure escaped futures precondition classification: %v", err)
			}
			ordinary := missionData{Action: &durableAction{Stage: "settings"}}
			if err := runner.newActionPreflight(h.ctx, &store.Mission{ShipID: "__treasury"}, &ordinary); err == nil || errors.Is(err, errFuturesPrecondition) {
				t.Fatalf("ordinary action clock behavior changed: %v", err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action != nil ||
				!data.CooldownUntil.After(time.Now()) {
				t.Fatalf("optional clock hold stopped or consumed the account pass: changed=%t err=%v action=%+v phase=%s", changed, err, data.Action, mission.Phase)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || rows[0].Status != "intent" {
				t.Fatalf("clock hold discarded admission coverage: %+v %v", rows, err)
			}
		})
	}
}

func TestFuturesPreconditionReleaseAllowsFleetWorkInSamePass(t *testing.T) {
	h, _, runner, mission, data := futuresStagedAccept(t)
	h.api.setFleet([]map[string]any{berthedLaprasRow()})
	refreshMarqueExecution(t, h, runner)
	data.Action.FinancialEpoch, data.Action.ControlEpoch = runner.capturedFinancial, runner.capturedControl
	data.Action.FinancialSession = runner.financialSession
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	meta := runner.meta["books/amsterdam"]
	meta.Stale = true
	runner.meta["books/amsterdam"] = meta
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	_, saved := h.mission("__futures")
	if saved.Action != nil || !saved.CooldownUntil.After(time.Now()) {
		t.Fatalf("transient action was not durably released with a cooldown: %+v", saved)
	}
	shipMission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipLapras)
	if err != nil || shipMission == nil {
		t.Fatalf("futures release consumed the fleet's pass: %+v %v holds=%v", shipMission, err, runner.holding)
	}
}

func TestFuturesMalformedThirdPartyWarningIsRateLimited(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	runner := New(nil, nil, store.Account{CompanyID: "ours"}, zap.New(core), Options{})
	runner.generation = "futures-warning"
	runner.meta["futures"] = DatasetMeta{Generation: runner.generation, UpdatedAt: time.Now(), Complete: true}
	runner.snapshot["futures"] = json.RawMessage(`[{"id":"foreign-invalid","creator_company_id":"other","seller_margin_pence":null}]`)
	for range 6 {
		if _, err := runner.futuresContracts(); err != nil {
			t.Fatal(err)
		}
	}
	if warnings := logs.FilterMessage("skipping malformed third-party futures records").Len(); warnings != 1 {
		t.Fatalf("one public decode capture emitted %d warnings", warnings)
	}
	runner.futuresWarningAfter = time.Now().Add(-time.Second)
	if _, err := runner.futuresContracts(); err != nil {
		t.Fatal(err)
	}
	if warnings := logs.FilterMessage("skipping malformed third-party futures records").Len(); warnings != 2 {
		t.Fatalf("warning did not reappear after its bounded window: %d", warnings)
	}
}

func TestFuturesExpiredOrphanAdmissionReleasesOnlyNeverSentCoverage(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "never sent", true: "pending"}[pending], func(t *testing.T) {
			h, p, runner, mission, data := futuresStagedAccept(t)
			action := data.Action
			if pending {
				if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			data.Action = nil
			p.mu.Lock()
			p.now = rows[0].MaturesAt.Add(time.Hour)
			p.quantity = 0
			p.mu.Unlock()
			meta := runner.meta["warehouses"]
			meta.Stale = true
			runner.meta["warehouses"] = meta
			refreshMarqueExecution(t, h, runner)
			if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
			if err != nil || !pending && (rows[0].Status != "rejected" || reserve != 0) ||
				pending && (rows[0].Status != "intent" || reserve == 0 || data.Action == nil || data.Action.ID != action.ID) {
				t.Fatalf("expiry confused no-send proof with ambiguous execution: pending=%t row=%+v reserve=%d phase=%s action=%+v err=%v",
					pending, rows, reserve, mission.Phase, data.Action, err)
			}
		})
	}
}

func futuresCreditDefault(t *testing.T, h *recoveryHarness, now time.Time) {
	t.Helper()
	row := store.FuturesIntent{AccountID: h.account.ID, IntentID: "credit-default", ActionID: "credit-create", CohortID: "credit-default-cohort",
		PortID: "amsterdam", CommodityID: "water_beer", Side: "bid", LotTons: 1, Status: "intent",
		StrikePricePence: 2300, MarginPence: 575, ReservedCashPence: 2937, ExposurePence: 2937,
		CreatedAt: now, MaturesAt: now.Add(30 * time.Minute)}
	if err := h.db.PutFuturesIntent(h.ctx, &row); err != nil {
		t.Fatal(err)
	}
	company := h.account.CompanyID
	contract := gameapi.FuturesContract{ID: "defaulted-pilot", CreatorCompanyID: company, BuyerCompanyID: &company, Side: "bid",
		PortID: row.PortID, CommodityID: row.CommodityID, LotTons: 1, StrikePricePence: row.StrikePricePence,
		BuyerMarginPence: row.MarginPence, Status: "defaulted", CreatedAt: row.CreatedAt, MaturesAt: row.MaturesAt, UpdatedAt: now}
	if err := h.db.ObserveFuturesContract(h.ctx, h.account.ID, row.IntentID, contract); err != nil {
		t.Fatal(err)
	}
}

func TestFuturesOwnCreditCaptureRequiresMatchingBoundedClaims(t *testing.T) {
	for _, kind := range []string{"valid", "foreign company", "missing claims", "negative claims", "overflow claims"} {
		t.Run(kind, func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h)
			refreshMarqueExecution(t, h, runner)
			futuresCreditDefault(t, h, p.now.Add(-time.Minute))
			report := map[string]any{"company_id": h.account.CompanyID, "futures_claims_open_pence": 125}
			switch kind {
			case "foreign company":
				report["company_id"] = "other-company"
			case "missing claims":
				delete(report, "futures_claims_open_pence")
			case "negative claims":
				report["futures_claims_open_pence"] = -1
			case "overflow claims":
				report["futures_claims_open_pence"] = int64(1_000_000_000_001)
			}
			reads := 0
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/v1/loan/credit" && request.URL.Query().Get("company_id") == h.account.CompanyID {
					reads++
					h.api.writeJSON(w, http.StatusOK, report)
					return
				}
				p.ServeHTTP(w, request)
			})
			if err := runner.refreshFuturesCredit(h.ctx); err != nil {
				t.Fatalf("optional credit capture stopped the account: %v", err)
			}
			want := int64(2362)
			if kind == "valid" {
				want = 125
			}
			if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reads != 1 || reserve != want {
				t.Fatalf("own-credit authority: reads=%d reserve=%d want=%d err=%v", reads, reserve, want, err)
			}
		})
	}
}

func TestFuturesOwnCreditReadsOnlyForDefaultsAndThrottleSurvivesRestartAndFailure(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	reads, status, claims := 0, http.StatusOK, int64(125)
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/credit" && request.URL.Query().Get("company_id") == h.account.CompanyID {
			reads++
			h.api.writeJSON(w, status, map[string]any{"company_id": h.account.CompanyID, "futures_claims_open_pence": claims})
			return
		}
		p.ServeHTTP(w, request)
	})
	refreshMarqueExecution(t, h, runner)
	if reads != 0 {
		t.Fatalf("ordinary refresh fetched own credit without a default: %d", reads)
	}
	futuresCreditDefault(t, h, p.now.Add(-time.Minute))
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 1 {
		t.Fatalf("default did not request one own-credit capture: reads=%d err=%v", reads, err)
	}
	runner = futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 1 {
		t.Fatalf("restart lost the persisted five-minute allowance: reads=%d err=%v", reads, err)
	}
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	allowRead := func() {
		t.Helper()
		if _, err := admin.ExecContext(h.ctx, "UPDATE futures_credit_evidence SET last_attempt_at = now() - interval '5 minutes 1 second' WHERE account_id = $1", h.account.ID); err != nil {
			t.Fatal(err)
		}
	}
	allowRead()
	status = http.StatusServiceUnavailable
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 2 {
		t.Fatalf("failed optional read blocked the account or retried HTTP: reads=%d err=%v", reads, err)
	}
	if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != 125 {
		t.Fatalf("failed credit refresh released an unpaid claim: reserve=%d err=%v", reserve, err)
	}
	runner = futuresMoneyRunner(t, h)
	refreshMarqueExecution(t, h, runner)
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 2 {
		t.Fatalf("failure refunded the allowance after restart: reads=%d err=%v", reads, err)
	}
	allowRead()
	status, claims = http.StatusOK, 0
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 3 {
		t.Fatalf("later valid zero did not refresh: reads=%d err=%v", reads, err)
	}
	if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != 0 {
		t.Fatalf("post-default zero retained paid liability: reserve=%d err=%v", reserve, err)
	}
	allowRead()
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || reads != 3 {
		t.Fatalf("resolved default continued fetching own credit: reads=%d err=%v", reads, err)
	}
}

func TestFuturesOwnCreditReservationRecoveryClearsUnneededHold(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	admin, err := sql.Open("pgx", h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE futures_intents RENAME TO futures_intents_unavailable"); err != nil {
		t.Fatal(err)
	}
	refreshErr := runner.refreshFuturesCredit(h.ctx)
	unavailable := runner.futuresCreditUnavailable
	if _, err := admin.ExecContext(h.ctx, "ALTER TABLE futures_intents_unavailable RENAME TO futures_intents"); err != nil {
		t.Fatal(err)
	}
	if refreshErr != nil || !unavailable {
		t.Fatalf("optional reservation failure: unavailable=%t err=%v", unavailable, refreshErr)
	}
	if err := runner.refreshFuturesCredit(h.ctx); err != nil || runner.futuresCreditUnavailable {
		t.Fatalf("successful no-default pass retained a stale credit hold: unavailable=%t err=%v", runner.futuresCreditUnavailable, err)
	}
}

func TestFuturesOwnCreditFailureHoldSurvivesRunStatusReset(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h)
	runner.opts.Execute = false
	futuresCreditDefault(t, h, p.now.Add(-time.Minute))
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/credit" && request.URL.Query().Get("company_id") == h.account.CompanyID {
			h.api.writeProblem(w, http.StatusServiceUnavailable, "unavailable", "credit unavailable")
			return
		}
		p.ServeHTTP(w, request)
	})
	running := startRunner(t, runner, h.ctx)
	replayRecorderWait(t, func() bool {
		snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
		if err != nil {
			t.Fatal(err)
		}
		var status struct {
			Holds []currentHold `json:"holds"`
		}
		if json.Unmarshal(snapshot["_engine"], &status) != nil {
			return false
		}
		for _, hold := range status.Holds {
			if strings.HasPrefix(hold.Reason, "own-credit evidence unavailable;") {
				return true
			}
		}
		return false
	})
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("optional credit failure stopped the account: %v", err)
	}
}
