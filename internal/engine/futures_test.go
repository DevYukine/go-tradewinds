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

func TestFuturesSettlementDoesNotDependOnLiveQuayBook(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	meta := runner.meta["books/amsterdam"]
	meta.Stale, meta.Error = true, "ship left the quay"
	runner.meta["books/amsterdam"] = meta
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.settles) != 1 {
		t.Fatalf("mature physical obligation depended on an unnecessary live price: settles=%d holds=%v", len(p.settles), runner.holding)
	}
}

func TestFuturesOwnWarehouseSetupAndWindDown(t *testing.T) {
	futuresWarehouseLifecycle(t, false, false, false, false)
}

func TestFuturesUnfilledProcurementReleasesLease(t *testing.T) {
	futuresWarehouseLifecycle(t, true, false, false, false)
}

func futuresWarehouseLifecycle(t *testing.T, noFill, defaults, disabled, unknownStock bool) {
	t.Helper()
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	leased := int64(0)
	var leaseTargets []int64
	var stockSides []string
	fills := make([]map[string]any, 0)
	warehouse := func() map[string]any {
		return map[string]any{"port_id": "amsterdam", "has_factor": true, "leased_tonnage": leased, "reserved_tonnage": 0,
			"used_tonnage": p.quantity, "free_tonnage": leased - p.quantity,
			"inventory": []any{map[string]any{"company_id": recoveryCompanyID, "port_id": "amsterdam", "commodity_id": "water_beer",
				"quantity": p.quantity, "locked_quantity": 0, "created_at": p.now, "updated_at": p.now}}}
	}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/warehouse/list":
			p.mu.Lock()
			defer p.mu.Unlock()
			h.api.writeJSON(w, http.StatusOK, []any{warehouse()})
		case "/v1/market/fills":
			p.mu.Lock()
			defer p.mu.Unlock()
			h.api.writeJSON(w, http.StatusOK, fills)
		case "/v1/future/settle":
			if !defaults {
				p.ServeHTTP(w, r)
				return
			}
			recorded := httptest.NewRecorder()
			p.ServeHTTP(recorded, r)
			var result gameapi.SettleFuturesResult
			if err := json.Unmarshal(recorded.Body.Bytes(), &result); err != nil {
				t.Error(err)
			}
			result.Status, result.SettledTons, result.PenaltyPaidPence = "defaulted", 0, 425
			p.mu.Lock()
			for _, row := range p.rows {
				if row["id"] == result.FuturesID {
					row["status"] = "defaulted"
				}
			}
			p.mu.Unlock()
			h.api.writeJSON(w, http.StatusOK, result)
		case "/v1/warehouse/set-lease":
			p.mu.Lock()
			defer p.mu.Unlock()
			var body struct {
				PortID string `json:"port_id"`
				Target int64  `json:"target_leased_tonnage"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.PortID != "amsterdam" || body.Target != 0 && body.Target != 2 || r.Header.Get("Idempotency-Key") == "" {
				t.Errorf("unsafe pilot lease: %+v", body)
			}
			if body.Target < p.quantity {
				t.Error("lease shrank below owned stock")
			}
			leased = body.Target
			leaseTargets = append(leaseTargets, leased)
			h.api.writeJSON(w, http.StatusOK, warehouse())
		case "/v1/market/place-order":
			p.mu.Lock()
			defer p.mu.Unlock()
			var body struct {
				PortID      string `json:"port_id"`
				CommodityID string `json:"commodity_id"`
				Side        string `json:"side"`
				Quantity    int64  `json:"quantity"`
				ShipID      string `json:"ship_id"`
				TimeInForce string `json:"time_in_force"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.PortID != "amsterdam" || body.CommodityID != "water_beer" || body.Quantity != 1 || body.ShipID != "" || body.TimeInForce != "ioc" {
				t.Errorf("unbounded or nonwarehouse pilot trade: %+v", body)
			}
			price, customs, filled := int64(2100), int64(57), int64(1)
			if noFill {
				price, customs, filled = 0, 0, 0
			} else if body.Side == "bid" {
				p.quantity++
			} else {
				price, customs = 1900, 0
				p.quantity--
			}
			stockSides = append(stockSides, body.Side)
			if filled > 0 {
				fills = append(fills, map[string]any{"id": "fill-" + body.Side, "port_id": body.PortID, "commodity_id": body.CommodityID,
					"side": body.Side, "is_maker": false, "quantity": filled, "price_pence": price, "customs_duty_pence": customs, "created_at": time.Now().UTC()})
			}
			if unknownStock {
				h.api.writeJSON(w, http.StatusOK, map[string]bool{"committed_uninterpreted": true})
				return
			}
			h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": body.PortID, "commodity_id": body.CommodityID, "side": body.Side,
				"requested_quantity": 1, "filled_quantity": filled, "resting_quantity": 0, "total_gross_pence": price, "customs_duty_pence": customs})
		default:
			p.ServeHTTP(w, r)
		}
	})
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 16)
	p.mu.Lock()
	if noFill {
		defer p.mu.Unlock()
		if len(p.creates) != 0 || leased != 0 || p.quantity != 0 || len(stockSides) != 1 ||
			len(leaseTargets) != 2 || leaseTargets[0] != 2 || leaseTargets[1] != 0 {
			t.Fatalf("unfilled procurement leaked a lease or created uncovered futures: creates=%d lease=%d quantity=%d trades=%v targets=%v holds=%v",
				len(p.creates), leased, p.quantity, stockSides, leaseTargets, runner.holding)
		}
		return
	}
	if len(p.rows) != 2 || leased != 2 || len(stockSides) != 1 || stockSides[0] != "bid" {
		p.mu.Unlock()
		t.Fatalf("covered setup incomplete: futures=%d lease=%d trades=%v holds=%v", len(p.rows), leased, stockSides, runner.holding)
	}
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	runner.opts.FuturesPilotEnabled = !disabled
	futuresMoneyPasses(t, h, runner, 16)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.creates) != 2 || len(p.settles) != 2 || p.quantity != 0 || leased != 0 || len(leaseTargets) != 2 || leaseTargets[0] != 2 || leaseTargets[1] != 0 || len(stockSides) != 2 || stockSides[1] != "ask" {
		t.Fatalf("pilot left obligations/rent or repeated its cohort: creates=%d settles=%d quantity=%d lease=%d targets=%v trades=%v holds=%v",
			len(p.creates), len(p.settles), p.quantity, leased, leaseTargets, stockSides, runner.holding)
	}
	if unknownStock {
		_, data := h.mission("__futures")
		if data.Action != nil || data.Futures.ProcurementPaid != 2157 || data.Futures.LiquidationPaid != 1900 ||
			data.Futures.ProcurementUnknown || data.Futures.LiquidationUnknown {
			t.Fatalf("unknown stock receipts lost measured fills: %+v", data)
		}
	}
}

func TestFuturesDefaultWindDownWhileDisabled(t *testing.T) {
	futuresWarehouseLifecycle(t, false, true, true, false)
}

func TestFuturesCommittedUnknownWarehouseOrdersReconcileFromInventoryAndFills(t *testing.T) {
	futuresWarehouseLifecycle(t, false, false, false, true)
}

func TestFuturesCommittedUnknownUnfilledProcurementReleasesLease(t *testing.T) {
	futuresWarehouseLifecycle(t, true, false, false, true)
}

func futuresStagedCreate(t *testing.T) (*recoveryHarness, *futuresMoneyAPI, *Runner, *store.Mission, missionData) {
	t.Helper()
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	data := missionData{}
	if changed, err := runner.driveFutures(h.ctx, nil, &data); err != nil || !changed {
		t.Fatalf("covered create did not stage: %t %v, holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__futures")
	if data.Action == nil || data.Action.Stage != "futures-create" {
		t.Fatalf("fixture did not stage a covered create: %+v", data.Action)
	}
	return h, p, runner, mission, data
}

func TestFuturesLedgerCashFlowAndPairPnL(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	mission, data := h.mission("__futures")
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("missing covered pair: %+v %v", rows, err)
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

func TestFuturesIntentRecoverySurvivesStageEpochHold(t *testing.T) {
	for _, control := range []bool{false, true} {
		t.Run(map[bool]string{false: "financial", true: "control"}[control], func(t *testing.T) {
			h, _ := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h, true)
			refreshMarqueExecution(t, h, runner)
			if control {
				runner.publishedControl.Add(1)
			} else {
				runner.publishedFinancial.Add(1)
			}
			data := missionData{}
			if changed, err := runner.driveFutures(h.ctx, nil, &data); err != nil || changed {
				t.Fatalf("invalid capture staged or failed instead of holding: %t %v", changed, err)
			}
			runner.capturedFinancial = runner.publishedFinancial.Load()
			runner.capturedControl = runner.publishedControl.Load()
			refreshMarqueExecution(t, h, runner)
			if changed, err := runner.driveFutures(h.ctx, nil, &data); err != nil || !changed {
				t.Fatalf("durable admission did not recover after authoritative refresh: %t %v", changed, err)
			}
			_, restored := h.mission("__futures")
			if restored.Action == nil || restored.Action.Stage != "futures-create" {
				t.Fatal("stage hold lost the admitted create")
			}
		})
	}
}

func TestFuturesNeverSentProofReadmitsUnlessImmutableTermsFail(t *testing.T) {
	for _, kind := range []string{neverSentFinancialEvent, neverSentControlEvent, neverSentPrecondition, neverSentFuturesTerms} {
		t.Run(kind, func(t *testing.T) {
			h, p, runner, mission, data := futuresStagedCreate(t)
			action := *data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			if err := h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]string{"kind": kind})); err != nil {
				t.Fatal(err)
			}
			wantChanged := kind == neverSentFinancialEvent || kind == neverSentControlEvent
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed != wantChanged || data.Action != nil {
				t.Fatalf("never-sent proof did not release old action: %t %v", changed, err)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if kind == neverSentFuturesTerms {
				if mission.Phase != "futures_stopped" || rows[0].Status != "rejected" {
					t.Fatalf("real precondition failure did not close the admission: %+v %+v", mission, rows)
				}
				return
			}
			if mission.Phase == "futures_stopped" || rows[0].Status != "intent" || rows[0].ActionID == action.ID {
				t.Fatalf("epoch race became a domain refusal: %+v %+v", mission, rows)
			}
			data.CooldownUntil = time.Time{}
			if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if data.Action == nil || data.Action.ID != rows[0].ActionID || !bytes.Equal(data.Action.Body, action.Body) {
				t.Fatal("readmission changed immutable create terms or lost the new durable key")
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if len(p.creates) != 1 {
				t.Fatalf("epoch recovery did not send exactly one logical create: %d", len(p.creates))
			}
		})
	}
}

func TestFuturesTransientUnsentCreateReleasesActionKeepsCoverage(t *testing.T) {
	h, _, runner, mission, data := futuresStagedCreate(t)
	meta := runner.meta["books/amsterdam"]
	meta.Stale = true
	runner.meta["books/amsterdam"] = meta
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action != nil || !data.CooldownUntil.After(time.Now()) {
		t.Fatalf("never-sent create remained attached: %t %v %+v", changed, err, data.Action)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || len(rows) != 1 || rows[0].Status != "intent" || mission.Phase == "futures_stopped" {
		t.Fatalf("transient precondition stopped the permanent admission: %+v %v", rows, err)
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

func TestFuturesCommittedReceiptsReconcileWithoutRepeatingMutations(t *testing.T) {
	for _, test := range []struct {
		name          string
		status        int
		uninterpreted bool
	}{
		{"non-201 success", http.StatusAccepted, false},
		{"committed unknown", http.StatusOK, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1/future/create" && request.URL.Path != "/v1/future/settle" {
					p.ServeHTTP(w, request)
					return
				}
				capture := httptest.NewRecorder()
				p.ServeHTTP(capture, request)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				if test.uninterpreted {
					_, _ = w.Write([]byte(`{"committed_uninterpreted":true}`))
				} else {
					_, _ = w.Write(capture.Body.Bytes())
				}
			})
			runner := futuresMoneyRunner(t, h, true)
			futuresMoneyPasses(t, h, runner, 12)
			p.mu.Lock()
			for _, row := range p.rows {
				row["status"] = "active"
				p.now = row["matures_at"].(time.Time).Add(time.Hour)
			}
			p.mu.Unlock()
			futuresMoneyPasses(t, h, runner, 12)
			_, data := h.mission("__futures")
			if len(p.creates) != 2 || len(p.settles) != 2 || data.Action != nil {
				t.Fatalf("committed outcome repeated or stayed attached: creates=%d settles=%d action=%+v holds=%v", len(p.creates), len(p.settles), data.Action, runner.holding)
			}
		})
	}
}

func TestFuturesRetryableCreateRejectionsKeepAdmission(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedCreate(t)
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
			if err != nil || rows[0].Status != "intent" || rows[0].ActionID == action.ID || mission.Phase == "futures_stopped" {
				t.Fatalf("retryable refusal stopped the permanent admission: %+v %v", rows, err)
			}
			if status == 429 && data.CooldownUntil.Before(deadline) {
				t.Fatal("429 lost durable Retry-After")
			}
		})
	}
}

func TestFuturesOrderPreflightBoundsArithmeticAndImmutablePayload(t *testing.T) {
	for _, test := range []struct {
		name                     string
		price, customs, quantity int64
	}{
		{"overflow ask", math.MaxInt64, 10000, 1},
		{"negative customs", 2100, -1, 1},
		{"overflow customs", 2100, math.MaxInt64, 1},
		{"oversized immutable lot", 2100, 268, 2},
		{"above inclusive spend cap", 15001, 10000, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &Runner{stateCache: AccountState{Markets: map[string]Market{"amsterdam": {CustomsBPS: test.customs}}}}
			action := &durableAction{Stage: "futures-stock", Method: http.MethodPost, Path: "/v1/market/place-order",
				Body: encode(map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": "bid", "time_in_force": "ioc", "quantity": test.quantity, "price_pence": test.price})}
			if err := runner.futuresOrderPreflight(action); err == nil {
				t.Fatal("unbounded upstream arithmetic or immutable order passed preflight")
			}
		})
	}
}

func TestFuturesUnresolvedAndMalformedEvidenceNeverBlockFleet(t *testing.T) {
	h, _, runner, mission, data := futuresStagedCreate(t)
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
		t.Fatal("unresolved create was guessed away")
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

func TestFuturesMatureSettlementPreemptsUnsentCreate(t *testing.T) {
	h, p, runner, mission, data := futuresStagedCreate(t)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed ||
		data.Action == nil || data.Action.Stage != "futures-create" {
		t.Fatalf("second covered side did not stage: %t %v %+v", changed, err, data.Action)
	}
	p.mu.Lock()
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	refreshMarqueExecution(t, h, runner)
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.observeFutures(h.ctx, rows); err != nil {
		t.Fatal(err)
	}
	// Refresh changes the epoch capture; the first pass refreshes this
	// never-journaled action, then the next pass tests physical priority.
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	if data.Action != nil {
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatal(err)
		}
	}
	if data.Action != nil {
		t.Fatal("unsent discretionary create retained the controller")
	}
	data.CooldownUntil = time.Time{}
	if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil || data.Action == nil || data.Action.Stage != "futures-settle" {
		t.Fatalf("mature physical obligation did not take priority: %v %+v", err, data.Action)
	}
	if len(p.creates) != 1 {
		t.Fatal("new listing was sent ahead of a mature obligation")
	}
}

func TestFuturesDefinitiveSettleRefusalRequiresNewEvidence(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	settles := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/future/settle" {
			settles++
			h.api.writeProblem(w, http.StatusConflict, "settlement_refused", "no changed contract authority")
			return
		}
		p.ServeHTTP(w, request)
	})
	futuresMoneyPasses(t, h, runner, 12)
	if settles != 2 {
		t.Fatalf("unchanged evidence repeated rejected settle mutations: %d", settles)
	}
	p.mu.Lock()
	for _, row := range p.rows {
		row["updated_at"] = p.now.Add(time.Second)
		break
	}
	p.mu.Unlock()
	futuresMoneyPasses(t, h, runner, 6)
	if settles != 3 {
		t.Fatalf("changed contract evidence did not permit one reconsideration: %d", settles)
	}
}

func TestFuturesCreateMatchingHasNoAdmissionTimeExpiry(t *testing.T) {
	h, _, runner, _, _ := futuresStagedCreate(t)
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	company := h.account.CompanyID
	contract := gameapi.FuturesContract{ID: "late-dispatch-contract", CreatorCompanyID: company,
		PortID: row.PortID, CommodityID: row.CommodityID, Side: row.Side, LotTons: 1,
		StrikePricePence: row.StrikePricePence, SellerCompanyID: &company, SellerMarginPence: row.MarginPence,
		Status: "open", CreatedAt: row.CreatedAt.Add(time.Hour), UpdatedAt: row.CreatedAt.Add(time.Hour),
		MaturesAt: row.CreatedAt.Add(90 * time.Minute)}
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{contract})
	if err := runner.observeFutures(h.ctx, rows); err != nil {
		t.Fatal(err)
	}
	observed, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil || observed[0].FuturesID != contract.ID {
		t.Fatalf("delayed dispatch lost its immutable owned listing: %+v %v", observed, err)
	}
}

func TestFuturesLeaseEpochReadmissionRetainsSetupEligibility(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_pilot"}
	data := missionData{Futures: &futuresPilotIntent{SetupAttempted: true}}
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "futures-lease", "/v1/warehouse/set-lease",
		map[string]any{"port_id": "amsterdam", "target_leased_tonnage": 2}); err != nil {
		t.Fatal(err)
	}
	action := data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]string{"kind": neverSentFinancialEvent})); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil || data.Futures.SetupAttempted || mission.Phase == "futures_stopped" {
		t.Fatalf("never-sent lease epoch proof disabled setup recovery: %t %v %+v", changed, err, data)
	}
}

func TestFuturesFinishedEmptyCohortDoesNotRepeatObservation(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_done"}
	data := missionData{Futures: &futuresPilotIntent{SetupAttempted: true}}
	runner.snapshot["futures"] = json.RawMessage(`not a list`)
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || changed || len(runner.holding) != 0 {
		t.Fatalf("finished empty cohort repeated optional observation: %t %v %v", changed, err, runner.holding)
	}
}

func TestFuturesLeaseRefusalRequiresChangedWarehouseNotExitQuote(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	data := missionData{Futures: &futuresPilotIntent{RejectedEvidence: map[string]string{}}}
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
	warehouse, _ := runner.futuresWarehouse()
	warehouse.LeasedTonnage++
	runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
	if refused, err := runner.futuresRefused(h.ctx, &data, "futures-close-lease"); err != nil || refused {
		t.Fatal("changed warehouse authority did not permit lease-close reconsideration")
	}
}

func TestFuturesPendingStockReplayRevalidatesImmutableCap(t *testing.T) {
	for _, test := range []struct {
		name            string
		price, quantity int64
		staleCustoms    bool
	}{
		{"price cap", 30001, 1, false}, {"customs inclusive cap", 30000, 1, false}, {"quantity", 2100, 2, false},
		{"stale customs", 2100, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedCreate(t)
			data.Action = nil
			if err := runner.stage(h.ctx, mission, &data, ShipState{}, "futures-stock", "/v1/market/place-order",
				map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": "bid", "time_in_force": "ioc",
					"quantity": test.quantity, "price_pence": test.price}); err != nil {
				t.Fatal(err)
			}
			data.Action.FuturesObserveAfter = time.Now().Add(-time.Hour)
			original := *data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: original.ID, AccountID: h.account.ID, Method: original.Method, Path: original.Path, Body: original.Body}); err != nil {
				t.Fatal(err)
			}
			if test.staleCustoms {
				meta := runner.meta["books/amsterdam"]
				meta.Stale = true
				runner.meta["books/amsterdam"] = meta
			}
			reserved, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			after, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
			if err != nil || after != reserved || data.Action == nil || data.Action.ID != original.ID || !bytes.Equal(data.Action.Body, original.Body) {
				t.Fatalf("unsafe pending order lost original key or coverage: %+v %d/%d %v", data.Action, after, reserved, err)
			}
			if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 0 {
				t.Fatalf("unsafe pending warehouse order reached transport: %+v", calls)
			}
		})
	}
}

func TestFuturesCommittedUnknownStockUsesInventoryWithoutInventingCash(t *testing.T) {
	for _, stage := range []string{"futures-stock", "futures-close-stock"} {
		t.Run(stage, func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedCreate(t)
			data.Action = nil
			side, delta := "bid", int64(1)
			if stage == "futures-close-stock" {
				side, delta = "ask", -1
			}
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
			warehouse, _ := runner.futuresWarehouse()
			warehouse.Inventory[0].Quantity += delta
			runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || data.Action != nil {
				t.Fatalf("authoritative inventory did not resolve committed stock: %t %v %+v", changed, err, data.Action)
			}
			if delta > 0 && (!data.Futures.ProcurementUnknown || data.Futures.ProcurementPaid != 0 || mission.Phase != "futures_stopped") ||
				delta < 0 && (!data.Futures.LiquidationUnknown || data.Futures.LiquidationPaid != 0 || data.Futures.LiquidatedTons != 1) {
				t.Fatalf("inventory observation invented cash or failed safe wind-down: %+v phase=%s", data.Futures, mission.Phase)
			}
			if calls := h.api.mutationsFor("/v1/market/place-order"); len(calls) != 0 {
				t.Fatal("committed stock was sent again")
			}
		})
	}
}

func TestFuturesStoppedOrDisabledAdmissionsReleaseOnlyUnsentCoverage(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		for _, proof := range []string{"absent", neverSentPrecondition, "pending", "committed"} {
			t.Run(map[bool]string{false: "disabled", true: "stopped"}[stopped]+"/"+proof, func(t *testing.T) {
				h, _, runner, mission, data := futuresStagedCreate(t)
				if proof != "absent" {
					action := data.Action
					if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
						t.Fatal(err)
					}
					if proof == neverSentPrecondition {
						if err := h.db.ReconcileAction(h.ctx, action.ID, encode(map[string]string{"kind": proof})); err != nil {
							t.Fatal(err)
						}
					} else if proof == "committed" {
						if err := h.db.CompleteAction(h.ctx, action.ID, http.StatusOK, json.RawMessage(`{"committed_uninterpreted":true}`), nil); err != nil {
							t.Fatal(err)
						}
					}
				}
				data.Action = nil
				runner.opts.FuturesPilotEnabled = stopped
				if stopped {
					mission.Phase = "futures_stopped"
				}
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				reserved, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				unsent := proof == "absent" || proof == neverSentPrecondition
				if unsent && (rows[0].Status != "rejected" || reserved != 0 || mission.Phase != "futures_done") ||
					!unsent && (rows[0].Status != "intent" || reserved == 0) {
					t.Fatalf("stop confused unsent proof with unknown execution: rows=%+v reserve=%d phase=%s", rows, reserved, mission.Phase)
				}
			})
		}
	}
}

func TestFuturesMalformedThirdPartyListDoesNotBlockOwnedSettlement(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/future/list" {
			p.ServeHTTP(w, request)
			return
		}
		capture := httptest.NewRecorder()
		p.ServeHTTP(capture, request)
		var records []json.RawMessage
		if err := json.Unmarshal(capture.Body.Bytes(), &records); err != nil {
			t.Error(err)
		}
		records = append([]json.RawMessage{json.RawMessage(`{"id":"other","creator_company_id":"third-party","seller_margin_pence":null}`)}, records...)
		h.api.writeJSON(w, http.StatusOK, records)
	})
	futuresMoneyPasses(t, h, runner, 12)
	if len(p.settles) != 2 {
		t.Fatalf("malformed unrelated public row blocked our settlement: %d holds=%v", len(p.settles), runner.holding)
	}
}

func TestFuturesSettleRefusalFingerprintUsesPermanentContractID(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	_, data := h.mission("__futures")
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	contracts, err := runner.futuresContracts()
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	var owned gameapi.FuturesContract
	for _, contract := range contracts {
		if contract.ID == row.FuturesID {
			owned = contract
		}
	}
	foreign := owned
	foreign.ID, foreign.CreatorCompanyID = "same-side-foreign", "other-company"
	foreign.SellerCompanyID, foreign.BuyerCompanyID = nil, nil
	runner.snapshot["futures"] = encode([]gameapi.FuturesContract{foreign, owned})
	data.Futures.IntentID = row.IntentID
	data.Action = &durableAction{Stage: "futures-settle", Body: encode(map[string]string{"futures_id": row.FuturesID})}
	staged, err := runner.futuresRejectionEvidence(h.ctx, &data, "futures-settle")
	if err != nil {
		t.Fatal(err)
	}
	data.Action = nil
	unstaged, err := runner.futuresRejectionEvidence(h.ctx, &data, "futures-settle")
	if err != nil || staged != unstaged || strings.Contains(unstaged, foreign.ID) {
		t.Fatalf("nil action selected the first same-side contract: %s / %s %v", staged, unstaged, err)
	}
}

func TestFuturesNonPartySettle403LatchesWithoutAuthorizationRetry(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	p.mu.Lock()
	for _, row := range p.rows {
		row["status"] = "active"
		p.now = row["matures_at"].(time.Time).Add(time.Hour)
	}
	p.mu.Unlock()
	settles := 0
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/future/settle" {
			settles++
			h.api.writeProblem(w, http.StatusForbidden, "forbidden", "The calling company is neither the buyer nor the seller")
			return
		}
		p.ServeHTTP(w, request)
	})
	futuresMoneyPasses(t, h, runner, 12)
	_, data := h.mission("__futures")
	if settles != 2 || len(data.Futures.RejectedEvidence) != 2 || data.Action != nil {
		t.Fatalf("non-party 403 failed to latch each exact contract: settles=%d data=%+v", settles, data)
	}
}

func TestFuturesErrorBoundaryPreservesJournalIntegrityAndDatabaseFailures(t *testing.T) {
	t.Run("journal integrity", func(t *testing.T) {
		h, _, runner, mission, data := futuresStagedCreate(t)
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
		h, _, runner, mission, data := futuresStagedCreate(t)
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

func TestFuturesObservedCreateSurvivesLaterRejectionReceipt(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h, p, runner, mission, data := futuresStagedCreate(t)
			action := data.Action
			if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(action.Method, action.Path, bytes.NewReader(action.Body))
			request.Header.Set("Idempotency-Key", action.ID)
			capture := httptest.NewRecorder()
			p.ServeHTTP(capture, request)
			if capture.Code != http.StatusCreated {
				t.Fatalf("committed-timeout fixture did not create listing: %d %s", capture.Code, capture.Body.String())
			}
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
			authFailure := status == 401 || status == 403
			if authFailure && (err == nil || !strings.Contains(err.Error(), "execution stopped")) ||
				!authFailure && err != nil || data.Action != nil {
				t.Fatalf("observation race lost fail-stop or reconciliation: status=%d err=%v action=%+v", status, err, data.Action)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || len(rows) != 1 || rows[0].FuturesID == "" || rows[0].Status != "open" || rows[0].ActionID != action.ID {
				t.Fatalf("receipt rejected or readmitted an already observed listing: %+v %v", rows, err)
			}
			wantReserve := max(int64(0), rows[0].ReservedCashPence-rows[0].MarginPence)
			if reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID); err != nil || reserve != wantReserve || rows[0].ReservedTons != 1 {
				t.Fatalf("live listing lost covered cash or stock: reserve=%d want=%d tons=%d %v", reserve, wantReserve, rows[0].ReservedTons, err)
			}
		})
	}
}

func TestFuturesConflictingTerminalReceiptHoldsWithoutFailingAccount(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 12)
	mission, data := h.mission("__futures")
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	defaulted := encode(gameapi.SettleFuturesResult{FuturesID: row.FuturesID, Status: "defaulted", PenaltyPaidPence: 425})
	if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "defaulted", defaulted); err != nil {
		t.Fatal(err)
	}
	data.Futures.IntentID = row.IntentID
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
		t.Fatalf("conflicting server evidence failed account or discarded receipt: %v %+v", err, data.Action)
	}
	if len(runner.holding) == 0 || !strings.Contains(runner.holding[len(runner.holding)-1].Reason, "futures") {
		t.Fatal("conflicting settlement evidence lacks its optional futures hold")
	}
}

func TestFuturesDisabledAfterSetupStillReleasesUnusedLease(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, false)
	refreshMarqueExecution(t, h, runner)
	runner.snapshot["warehouses"] = encode([]map[string]any{{"port_id": "amsterdam", "leased_tonnage": 2, "free_tonnage": 2, "inventory": []any{}}})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_pilot"}
	data := missionData{Futures: &futuresPilotIntent{LeaseOwned: true}}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil ||
		data.Action.Stage != "futures-close-lease" || !bytes.Contains(data.Action.Body, []byte(`"target_leased_tonnage":0`)) {
		t.Fatalf("disabled setup with no admissions retained its rented lease: %t %v %+v", changed, err, data.Action)
	}
}

func TestFuturesClockPreconditionsHoldWithoutStoppingAccount(t *testing.T) {
	for _, failure := range []string{"stale", "paused", "zero scale"} {
		t.Run(failure, func(t *testing.T) {
			h, _, runner, mission, data := futuresStagedCreate(t)
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
				!data.CooldownUntil.After(time.Now()) || mission.Phase == "futures_stopped" {
				t.Fatalf("optional clock hold stopped or consumed the account pass: changed=%t err=%v action=%+v phase=%s", changed, err, data.Action, mission.Phase)
			}
			rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil || rows[0].Status != "intent" {
				t.Fatalf("clock hold discarded admission coverage: %+v %v", rows, err)
			}
		})
	}
}

func TestFuturesStockStagingRequiresSameSpareAllowanceAsDispatch(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	runner.snapshot["warehouses"] = encode([]map[string]any{{"port_id": "amsterdam", "leased_tonnage": 2, "free_tonnage": 2, "inventory": []any{}}})
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_pilot"}
	data := missionData{Futures: &futuresPilotIntent{LeaseOwned: true}}
	free, err := runner.futuresFreeCash(h.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[i]
		if vault.BankingRegion == "north_sea" && vault.AccountType == "vault" {
			vault.BalancePence -= free - 2500
		}
	}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || changed || data.Action != nil {
		t.Fatalf("sub-allowance cash staged a stock/preflight livelock: %t %v %+v", changed, err, data.Action)
	}
	if len(runner.holding) == 0 || !strings.Contains(runner.holding[len(runner.holding)-1].Reason, "cash") {
		t.Fatal("insufficient spare setup allowance has no visible hold")
	}
}

func TestFuturesPreconditionReleaseAllowsFleetWorkInSamePass(t *testing.T) {
	h, _, runner, mission, data := futuresStagedCreate(t)
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

func TestFuturesLeasePresence403PausesAndRecoversPilotSetup(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	calls, leased := 0, int64(0)
	presence := false
	warehouse := func() map[string]any {
		return map[string]any{"port_id": "amsterdam", "has_factor": true, "leased_tonnage": leased,
			"reserved_tonnage": 0, "used_tonnage": 0, "free_tonnage": leased, "inventory": []any{}}
	}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/warehouse/list":
			h.api.writeJSON(w, http.StatusOK, []any{warehouse()})
		case "/v1/warehouse/set-lease":
			calls++
			if !presence {
				h.api.writeProblem(w, http.StatusForbidden, "no_port_presence", "no ship or factor remains at Amsterdam")
				return
			}
			leased = 2
			h.api.writeJSON(w, http.StatusOK, warehouse())
		default:
			p.ServeHTTP(w, request)
		}
	})
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_pilot"}
	data := missionData{Futures: &futuresPilotIntent{SetupAttempted: true}}
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "futures-lease", "/v1/warehouse/set-lease",
		map[string]any{"port_id": "amsterdam", "target_leased_tonnage": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || data.Action != nil || mission.Phase != "futures_pilot" ||
		data.Futures.SetupAttempted || data.CooldownUntil.Before(runner.gameDelay(1).Add(-time.Second)) {
		t.Fatalf("lease domain refusal ended setup instead of pausing it: %v %+v phase=%s", err, data, mission.Phase)
	}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || changed || data.Action != nil || calls != 1 {
		t.Fatalf("presence refusal hot-looped through its cooldown: %t %v calls=%d", changed, err, calls)
	}
	presence = true
	refreshMarqueExecution(t, h, runner)
	data.CooldownUntil = time.Time{}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil || data.Action.Stage != "futures-lease" {
		t.Fatalf("fresh port presence could not recover setup: %t %v %+v", changed, err, data)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || !data.Futures.LeaseOwned || calls != 2 {
		t.Fatalf("recovered setup did not finish its bounded lease: %v %+v calls=%d", err, data, calls)
	}
}

func TestFuturesOversizedExternalLeaseHoldsBeforeSettlementOrWindDownStaging(t *testing.T) {
	for _, stage := range []string{"settle", "wind-down"} {
		t.Run(stage, func(t *testing.T) {
			h, p := newFuturesMoneyAPI(t)
			runner := futuresMoneyRunner(t, h, true)
			futuresMoneyPasses(t, h, runner, 12)
			mission, data := h.mission("__futures")
			p.mu.Lock()
			for _, row := range p.rows {
				row["status"] = "active"
				p.now = row["matures_at"].(time.Time).Add(time.Hour)
				if stage == "wind-down" {
					row["status"] = "cancelled"
				}
			}
			p.mu.Unlock()
			refreshMarqueExecution(t, h, runner)
			data.Futures.LeaseOwned, data.Futures.ProcurementPaid = true, 2157
			warehouse, _ := runner.futuresWarehouse()
			warehouse.LeasedTonnage = 3
			runner.snapshot["warehouses"] = encode([]futuresWarehouse{warehouse})
			for range 3 {
				if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || changed || data.Action != nil {
					t.Fatalf("external lease increase restaged an invalid %s: %t %v %+v", stage, changed, err, data.Action)
				}
			}
			if len(runner.holding) == 0 || !strings.Contains(runner.holding[len(runner.holding)-1].Reason, "operator reconciliation") {
				t.Fatal("external lease increase lacks an operator-visible hold")
			}
		})
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

func TestFuturesImmutableBidCoverageCannotWaitThroughCustomsRise(t *testing.T) {
	h, _, runner, mission, data := futuresStagedCreate(t)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	refreshMarqueExecution(t, h, runner)
	if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil || data.Action == nil {
		t.Fatalf("bid did not stage after ask: %v %+v", err, data.Action)
	}
	market := runner.stateCache.Markets["amsterdam"]
	market.CustomsBPS = 10000
	runner.stateCache.Markets["amsterdam"] = market
	if err := runner.futuresPreflight(h.ctx, &data); !errors.Is(err, errFuturesTerms) {
		t.Fatalf("immutable shortfall treated as a recoverable market delay: %v", err)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || changed || data.Action != nil || mission.Phase != "futures_stopped" {
		t.Fatalf("immutable shortfall retained a live admission: %t %v %+v phase=%s", changed, err, data.Action, mission.Phase)
	}
	rows, err := h.db.FuturesIntents(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Side == "bid" && row.Status != "rejected" {
			t.Fatalf("immutable shortfall kept its reserve forever: %+v", row)
		}
	}
}

func TestFuturesExpiredOrphanAdmissionReleasesOnlyNeverSentCoverage(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "never sent", true: "pending"}[pending], func(t *testing.T) {
			h, p, runner, mission, data := futuresStagedCreate(t)
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
			p.mu.Unlock()
			refreshMarqueExecution(t, h, runner)
			if _, err := runner.driveFutures(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			rows, err = h.db.FuturesIntents(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			reserve, err := h.db.FuturesCashReserve(h.ctx, h.account.ID)
			if err != nil || !pending && (rows[0].Status != "rejected" || reserve != 0 || mission.Phase != "futures_done") ||
				pending && (rows[0].Status != "intent" || reserve == 0 || data.Action == nil || data.Action.ID != action.ID) {
				t.Fatalf("expiry confused no-send proof with ambiguous execution: pending=%t row=%+v reserve=%d phase=%s action=%+v err=%v",
					pending, rows, reserve, mission.Phase, data.Action, err)
			}
		})
	}
}

func futuresCreditDefault(t *testing.T, h *recoveryHarness, now time.Time) {
	t.Helper()
	row := store.FuturesIntent{AccountID: h.account.ID, IntentID: "credit-default", ActionID: "credit-create", CohortID: futuresPilotCohort,
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
			runner := futuresMoneyRunner(t, h, true)
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
	runner := futuresMoneyRunner(t, h, true)
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
	runner = futuresMoneyRunner(t, h, true)
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
	runner = futuresMoneyRunner(t, h, true)
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
	runner := futuresMoneyRunner(t, h, false)
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
	runner := futuresMoneyRunner(t, h, false)
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
