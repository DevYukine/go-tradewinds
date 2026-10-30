package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestFuturesRejectedSetupRequiresExplicitRearmAfterWindDown(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	leased, rejectStock := int64(0), true
	var leaseTargets []int64
	var stockKeys []string
	warehouse := func() map[string]any {
		return map[string]any{"port_id": "amsterdam", "has_factor": true, "leased_tonnage": leased,
			"reserved_tonnage": p.locked, "used_tonnage": p.quantity, "free_tonnage": leased - p.quantity,
			"inventory": []any{map[string]any{"company_id": h.account.CompanyID, "port_id": "amsterdam", "commodity_id": "water_beer",
				"quantity": p.quantity, "locked_quantity": p.locked, "created_at": p.now, "updated_at": p.now}}}
	}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/warehouse/list":
			p.mu.Lock()
			defer p.mu.Unlock()
			h.api.writeJSON(w, http.StatusOK, []any{warehouse()})
		case "/v1/warehouse/set-lease":
			p.mu.Lock()
			defer p.mu.Unlock()
			var body struct {
				Target int64 `json:"target_leased_tonnage"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			leased = body.Target
			leaseTargets = append(leaseTargets, leased)
			h.api.writeJSON(w, http.StatusOK, warehouse())
		case "/v1/market/place-order":
			p.mu.Lock()
			defer p.mu.Unlock()
			var body struct {
				TimeInForce string `json:"time_in_force"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.TimeInForce != "ioc" {
				t.Errorf("pilot stock used an undocumented time_in_force: %q", body.TimeInForce)
			}
			stockKeys = append(stockKeys, request.Header.Get("Idempotency-Key"))
			if rejectStock {
				h.api.writeProblem(w, http.StatusUnprocessableEntity, "invalid_request", "rejected setup request")
				return
			}
			p.quantity++
			h.api.writeJSON(w, http.StatusOK, map[string]any{"port_id": "amsterdam", "commodity_id": "water_beer", "side": "bid",
				"requested_quantity": 1, "filled_quantity": 1, "resting_quantity": 0, "total_gross_pence": 2100, "customs_duty_pence": 57})
		default:
			p.ServeHTTP(w, request)
		}
	})
	runner := futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 16)
	mission, stopped := h.mission("__futures")
	if mission.Phase != "futures_done" || stopped.Action != nil || leased != 0 || len(stockKeys) != 1 || len(p.creates) != 0 {
		t.Fatalf("rejected stock did not finish wind-down: phase=%s data=%+v lease=%d keys=%v creates=%d", mission.Phase, stopped, leased, stockKeys, len(p.creates))
	}
	runner = futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 8)
	if len(stockKeys) != 1 || len(leaseTargets) != 2 {
		t.Fatalf("restart automatically repeated unused setup: keys=%v leases=%v", stockKeys, leaseTargets)
	}
	if err := RearmUnusedFuturesPilot(h.ctx, h.db, h.account.ID); err != nil {
		t.Fatalf("explicit re-arm rejected a never-filled, closed setup: %v", err)
	}
	mission, rearmed := h.mission("__futures")
	if mission.Phase != "futures_pilot" || mission.Generation != futuresPilotCohort || rearmed.Sequence != stopped.Sequence ||
		rearmed.LastActionID != stopped.LastActionID || rearmed.Futures.SetupAttempted || rearmed.Futures.LeaseOwned {
		t.Fatalf("re-arm changed journal identity or retained setup ownership: %+v %+v", mission, rearmed)
	}
	p.mu.Lock()
	rejectStock = false
	p.mu.Unlock()
	runner = futuresMoneyRunner(t, h, true)
	futuresMoneyPasses(t, h, runner, 16)
	if len(stockKeys) != 2 || stockKeys[0] == "" || stockKeys[0] == stockKeys[1] || len(p.creates) != 2 ||
		len(leaseTargets) != 3 || leaseTargets[0] != 2 || leaseTargets[1] != 0 || leaseTargets[2] != 2 {
		t.Fatalf("explicit recovery did not create exactly one bounded pair with new setup keys: keys=%v creates=%d leases=%v", stockKeys, len(p.creates), leaseTargets)
	}
	if err := RearmUnusedFuturesPilot(h.ctx, h.db, h.account.ID); !errors.Is(err, store.ErrActionConflict) {
		t.Fatalf("a pilot with created futures was re-armed: %v", err)
	}
}

func TestFuturesRearmAllowsNeverSentDisabledSetup(t *testing.T) {
	h, p := newFuturesMoneyAPI(t)
	p.quantity = 0
	leased, mutations := int64(0), 0
	var leaseKeys []string
	warehouse := func() map[string]any {
		return map[string]any{"port_id": "amsterdam", "has_factor": true,
			"leased_tonnage": leased, "reserved_tonnage": 0, "used_tonnage": 0, "free_tonnage": leased, "inventory": []any{}}
	}
	h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			mutations++
		}
		switch request.URL.Path {
		case "/v1/warehouse/list":
			h.api.writeJSON(w, http.StatusOK, []any{warehouse()})
		case "/v1/warehouse/set-lease":
			leased = 2
			leaseKeys = append(leaseKeys, request.Header.Get("Idempotency-Key"))
			h.api.writeJSON(w, http.StatusOK, warehouse())
		default:
			p.ServeHTTP(w, request)
		}
	})
	runner := futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	data := missionData{}
	if changed, err := runner.driveFutures(h.ctx, nil, &data); err != nil || !changed {
		t.Fatalf("unused lease setup did not stage: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__futures")
	if data.Action == nil || data.Action.Stage != "futures-lease" {
		t.Fatalf("expected a staged lease setup: %+v", data)
	}
	firstID := data.Action.ID
	runner = futuresMoneyRunner(t, h, false)
	refreshMarqueExecution(t, h, runner)
	for range 2 {
		if data.Action == nil {
			break
		}
		if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
			t.Fatalf("disabled setup dispatch failed: %v", err)
		}
	}
	if data.Action != nil || data.Sequence != 1 || data.LastActionID != "" {
		t.Fatalf("disabled setup was not retired as never sent: %+v", data)
	}
	if exists, err := h.db.HasAction(h.ctx, h.account.ID, firstID); err != nil || exists || mutations != 0 {
		t.Fatalf("never-sent setup reached the journal or network: exists=%t mutations=%d err=%v", exists, mutations, err)
	}
	data.CooldownUntil = time.Time{}
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || mission.Phase != "futures_done" {
		t.Fatalf("disabled unused setup did not finish wind-down: changed=%t phase=%s err=%v", changed, mission.Phase, err)
	}
	if err := RearmUnusedFuturesPilot(h.ctx, h.db, h.account.ID); err != nil {
		t.Fatalf("explicit re-arm required a sent action for a never-sent setup: %v", err)
	}
	mission, data = h.mission("__futures")
	if data.Sequence != 1 || data.LastActionID != "" || data.Action != nil {
		t.Fatalf("re-arm reset the retired action sequence or fabricated a sent action: %+v", data)
	}
	runner = futuresMoneyRunner(t, h, true)
	refreshMarqueExecution(t, h, runner)
	if changed, err := runner.driveFutures(h.ctx, mission, &data); err != nil || !changed || data.Action == nil ||
		data.Action.Stage != "futures-lease" || data.Action.ID == firstID || mutations != 0 {
		t.Fatalf("explicit re-arm failed to stage a fresh bounded lease: changed=%t data=%+v mutations=%d err=%v", changed, data, mutations, err)
	}
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil || data.Action != nil || !data.Futures.LeaseOwned ||
		mutations != 1 || len(leaseKeys) != 1 || leaseKeys[0] == firstID {
		t.Fatalf("explicit recovery failed to send exactly one fresh lease: data=%+v keys=%v mutations=%d err=%v", data, leaseKeys, mutations, err)
	}
}

func TestFuturesRearmRefusesUnfinishedPaidUnknownOrAdmittedPilot(t *testing.T) {
	for _, kind := range []string{"stopped", "staged", "paid", "unknown stock", "liquidated", "pending", "admitted", "terminal admission"} {
		t.Run(kind, func(t *testing.T) {
			h, _ := newFuturesMoneyAPI(t)
			mission := &store.Mission{AccountID: h.account.ID, ShipID: "__futures", Generation: futuresPilotCohort, Phase: "futures_done"}
			data := missionData{Futures: &futuresPilotIntent{SetupAttempted: true}, Sequence: 3, LastActionID: "finished-setup"}
			switch kind {
			case "stopped":
				mission.Phase = "futures_stopped"
			case "staged":
				data.Action = &durableAction{ID: "staged-setup", Stage: "futures-stock"}
			case "paid":
				data.Futures.ProcurementPaid = 2157
			case "unknown stock":
				data.Futures.ProcurementUnknown = true
			case "liquidated":
				data.Futures.LiquidatedTons = 1
			case "pending":
				if _, err := h.db.BeginAction(h.ctx, &store.Action{ID: "pending-setup", AccountID: h.account.ID,
					Method: http.MethodPost, Path: "/v1/market/place-order", Body: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
			case "admitted", "terminal admission":
				now := time.Now().UTC()
				row := store.FuturesIntent{AccountID: h.account.ID, IntentID: futuresPilotCohort + "/bid", ActionID: "create-bid",
					CohortID: futuresPilotCohort, PortID: "amsterdam", CommodityID: "water_beer", Side: "bid", LotTons: 1,
					StrikePricePence: 2300, MarginPence: 575, ReservedCashPence: 2937, ExposurePence: 2937,
					Status: "intent", CreatedAt: now, MaturesAt: now.Add(time.Hour)}
				if err := h.db.PutFuturesIntent(h.ctx, &row); err != nil {
					t.Fatal(err)
				}
				if kind == "terminal admission" {
					if err := h.db.RecordFuturesOutcome(h.ctx, h.account.ID, row.IntentID, "rejected", json.RawMessage(`{"kind":"never_sent_futures_terms"}`)); err != nil {
						t.Fatal(err)
					}
				}
			}
			mission.Data = encode(data)
			if err := h.db.PutMission(h.ctx, mission); err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(mission.Data)
			if err := RearmUnusedFuturesPilot(h.ctx, h.db, h.account.ID); !errors.Is(err, store.ErrActionConflict) {
				t.Fatalf("unsafe pilot was re-armed: %v", err)
			}
			saved, err := h.db.GetMission(h.ctx, h.account.ID, "__futures")
			if err != nil || saved.Phase != mission.Phase || !bytes.Equal(saved.Data, before) {
				t.Fatalf("refused re-arm changed durable state: %+v %v", saved, err)
			}
		})
	}
}

func TestFuturesRearmRequiresStoppedExecutor(t *testing.T) {
	h, _ := newFuturesMoneyAPI(t)
	_, release, err := h.db.AcquireAccountContext(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := RearmUnusedFuturesPilot(h.ctx, h.db, h.account.ID); !errors.Is(err, store.ErrAccountLocked) {
		t.Fatalf("operator re-arm bypassed the active executor lock: %v", err)
	}
}
