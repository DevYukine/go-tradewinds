package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type financialStageBody struct {
	payload    json.RawMessage
	invalidate func()
}

func (body financialStageBody) MarshalJSON() ([]byte, error) {
	body.invalidate()
	return body.payload, nil
}

func TestFinancialStageRejectsEventPublishedWhilePreparingIntent(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("stage funded control: changed %t error %v holds %v", changed, err, runner.holding)
	}
	mission, data := h.mission("__growth")
	payload := append(json.RawMessage(nil), data.Action.Body...)
	data.Action = nil
	if err := h.db.DeleteMission(h.ctx, h.account.ID, mission.ShipID); err != nil {
		t.Fatal(err)
	}
	body := financialStageBody{
		payload: payload,
		invalidate: func() {
			h.api.setVaultBalance("north", 75_000)
			runner.publishedFinancial.Add(1)
		},
	}
	if err := runner.stage(h.ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", body); err != nil {
		t.Fatalf("hold invalidated stage: %v", err)
	}
	h.missingMission("__growth")
	if len(h.pending()) != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("invalidated staging created a journal request or sent a build")
	}
	if _, err := h.tick(runner); err != nil {
		t.Fatalf("authoritative post-event planning: %v", err)
	}
	h.missingMission("__growth")
}

func TestRunReplansNeverSentGrowthAfterEventBetweenStageAndDispatch(t *testing.T) {
	for _, eventType := range []string{"company_upkeep_billed", progressOrdinaryEvent} {
		t.Run(eventType, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setVaultBalance("north", 500_000)
			h.api.setVaultBalance("south", 100_000)
			h.seedLaneProofs()
			runner := h.runner(primaryTestKey)
			second := make(chan struct{})
			last := make(chan struct{})
			stream := interceptEventStream(t, h.api, []progressFrame{{id: "connected", eventType: progressOrdinaryEvent}}, 0)
			stream.frameFor = func(int32) progressFrame { return progressFrame{} }
			open := make(chan struct{})
			close(open)
			stream.gateFor = func(index int32) <-chan struct{} {
				switch index {
				case 2:
					return second
				case 4:
					return last
				default:
					return open
				}
			}
			running := startRunner(t, runner, h.ctx)
			stream.waitReady(t, 2)
			_, staged := h.mission("__growth")
			if staged.Action == nil || staged.Action.Stage != "build" || len(h.pending()) != 0 {
				t.Fatalf("first capture did not stage a never-sent funded build: %+v", staged.Action)
			}
			originalID := staged.Action.ID
			if eventType == "company_upkeep_billed" {
				h.api.setVaultBalance("north", 75_000)
			}
			if !stream.emit(h.ctx, progressFrame{id: "between-stage-and-send", eventType: eventType}) {
				t.Fatal("event stream ended before the staged-intent invalidation")
			}
			if eventType == "company_upkeep_billed" {
				waitPublishedFinancial(t, runner, 1)
			}
			close(second)
			stream.waitReady(t, 4)
			running.cancel()
			if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("stop runner: %v", err)
			}
			calls := h.api.mutationsFor("/v1/shipyard/build-ship")
			if eventType == progressOrdinaryEvent {
				if len(calls) != 1 || calls[0].Status != http.StatusOK || calls[0].IdempotencyKey != originalID {
					t.Fatalf("ordinary notification starved or replaced the staged build: %+v", calls)
				}
				return
			}
			if len(calls) != 0 || len(h.pending()) != 0 {
				t.Fatalf("financial event allowed the stale build to cross its first-send boundary: %+v", calls)
			}
			mission, err := h.db.GetMission(h.ctx, h.account.ID, "__growth")
			if err != nil {
				t.Fatal(err)
			}
			if mission != nil {
				var retained missionData
				if err := json.Unmarshal(mission.Data, &retained); err != nil {
					t.Fatal(err)
				}
				if retained.Action != nil {
					t.Fatalf("authoritative resync retained stale never-sent funds: %+v", retained.Action)
				}
			}
		})
	}
}

func TestFinancialRestartReplansNeverSentGrowthFromFreshFunds(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	original := h.runner(primaryTestKey)
	if _, err := h.tick(original); err != nil {
		t.Fatal(err)
	}
	_, staged := h.mission("__growth")
	if staged.Action == nil || len(h.pending()) != 0 {
		t.Fatalf("expected a never-sent build: %+v", staged.Action)
	}
	originalID := staged.Action.ID
	// The process dies before observing the bill's SSE frame. A new process
	// cannot equate its reset in-memory epoch with the staged capture's epoch.
	h.api.setVaultBalance("north", 75_000)
	restarted := h.runner(primaryTestKey)
	for range 2 {
		if _, err := h.tick(restarted); err != nil {
			t.Fatalf("resync and release unsent build: %v", err)
		}
	}
	h.missingMission("__growth")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatal("restart dispatched an unjournaled build funded by the previous process's capture")
	}
	h.api.setVaultBalance("north", 500_000)
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	_, replanned := h.mission("__growth")
	if replanned.Action == nil || replanned.Action.ID == originalID {
		t.Fatalf("restored funds did not receive a freshly evaluated identity: %+v", replanned.Action)
	}
	if _, err := h.tick(restarted); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(calls) != 1 || calls[0].Status != http.StatusOK || calls[0].IdempotencyKey != replanned.Action.ID {
		t.Fatalf("freshly replanned build did not resume: %+v", calls)
	}
}

func TestFinancialEventAndRestartPreserveUnresolvedGrowthRequest(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	original := h.runner(primaryTestKey)
	if _, err := h.tick(original); err != nil {
		t.Fatal(err)
	}
	var transportLost atomic.Bool
	transportLost.Store(true)
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/v1/shipyard/build-ship" && transportLost.Load() {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read transport-lost build: %v", err)
				return
			}
			h.api.mu.Lock()
			h.api.record(request, body, 0)
			h.api.mu.Unlock()
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("drop build transport: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	mission, staged := h.mission("__growth")
	id := staged.Action.ID
	if _, err := original.dispatch(h.ctx, mission, &staged); err != nil {
		t.Fatalf("ambiguous build attempt: %v", err)
	}
	pending := h.pending()
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("ambiguous request was not durably retained: %+v", pending)
	}
	before := h.api.mutationsFor("/v1/shipyard/build-ship")
	if len(before) == 0 {
		t.Fatal("transport-lost request did not reach the game")
	}
	for _, call := range before {
		if call.Status != 0 || call.IdempotencyKey != id || !bytes.Equal(call.Body, pending[0].Body) {
			t.Fatalf("unresolved fixture did not lose the original build transport: %+v", before)
		}
	}
	h.api.setVaultBalance("north", 75_000)
	transportLost.Store(false)
	h.expireCooldown("__growth")
	restarted := h.runner(primaryTestKey)
	restarted.publishedFinancial.Add(1)
	restarted.capturedFinancial = 1
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("replay unresolved request after financial resync: %v", err)
	}
	calls := h.api.mutationsFor("/v1/shipyard/build-ship")
	last := calls[len(calls)-1]
	if len(calls) != len(before)+1 || last.Status != http.StatusOK || last.IdempotencyKey != id || !bytes.Equal(last.Body, pending[0].Body) {
		t.Fatalf("financial invalidation changed unresolved request identity or payload: %+v", calls)
	}
	_, completed := h.mission("__growth")
	if completed.Action != nil || completed.LastActionID != id || len(h.pending()) != 0 {
		t.Fatalf("stable replay did not consume its original outcome: %+v", completed)
	}
}

func waitPublishedFinancial(t *testing.T, runner *Runner, want uint64) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for runner.publishedFinancial.Load() < want {
		select {
		case <-deadline:
			t.Fatalf("financial event was not published: got %d want %d", runner.publishedFinancial.Load(), want)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRunAbandonsOnlyNewJournalRequestVetoedBeforeFirstSend(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	stream := interceptEventStream(t, h.api, []progressFrame{{id: "connected", eventType: progressOrdinaryEvent}}, 3)
	stream.frameFor = func(int32) progressFrame { return progressFrame{} }
	var injected atomic.Bool
	var staged durableAction
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message != "dispatching durable action" || !injected.CompareAndSwap(false, true) {
			return nil
		}
		_, data := h.mission("__growth")
		if data.Action == nil {
			return errors.New("new journal request lost its durable mission")
		}
		staged = *data.Action
		h.api.setVaultBalance("north", 75_000)
		if !stream.emit(h.ctx, progressFrame{id: "before-first-send", eventType: "company_upkeep_billed"}) {
			return errors.New("event stream ended before first-send invalidation")
		}
		waitPublishedFinancial(t, runner, 1)
		return nil
	}))
	running := startRunner(t, runner, h.ctx)
	stream.waitReady(t, 3)
	running.cancel()
	if err := running.wait(t, 15*time.Second); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("stop runner: %v", err)
	}
	stream.releaseGates()
	if !injected.Load() || h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatal("a known financial invalidation crossed the first transport attempt")
	}
	mission, data := h.mission("__growth")
	if data.Action != nil {
		t.Fatalf("preflight veto retained its never-sent action: %+v", data.Action)
	}
	// Recreate the crash window after the no-send evidence committed but before
	// the actionless mission save. Restart must release, not invent a build.
	id := staged.ID
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: id, AccountID: h.account.ID, Method: staged.Method,
		Path: staged.Path, Body: staged.Body,
	})
	if err != nil || journaled == nil || journaled.State != store.ActionReconciled || journaled.Status != 0 {
		t.Fatalf("no-send evidence is not durable: %+v error %v", journaled, err)
	}
	data.Sequence--
	data.Action = &durableAction{
		ID: id, Stage: "build", Method: journaled.Method, Path: journaled.Path,
		Body: journaled.Body, CreatedAt: journaled.CreatedAt,
	}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	restarted := h.runner(primaryTestKey)
	if _, err := h.tick(restarted); err != nil {
		t.Fatalf("recover committed no-send evidence: %v", err)
	}
	_, recovered := h.mission("__growth")
	if recovered.Action != nil || recovered.BuiltShipID != "" || recovered.LastActionID == id ||
		h.api.mutationCount("/v1/shipyard/build-ship") != 0 || len(h.pending()) != 0 {
		t.Fatalf("no-send recovery was treated as a committed build: %+v", recovered)
	}
}

func TestFinancialReplanningRechecksUnsentBuyAndFreightCollateral(t *testing.T) {
	for _, stage := range []string{"buy", "accept-freight"} {
		t.Run(stage, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.api.setVaultBalance("north", 500_000)
			h.api.setVaultBalance("south", 100_000)
			berthed := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			berthed["ship"].(map[string]any)["status"] = "berthed"
			berthed["ship"].(map[string]any)["cannons_count"] = 4
			berthed["effective_hold_capacity_tons"] = 52
			berthed["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
			berthed["route_legs"] = []any{}
			berthed["voyage"] = nil
			if stage == "accept-freight" {
				berthed["cargo_manifest"] = map[string]any{recoveryGrain: 2}
				berthed["cargo_used_tons"] = 2
			}
			h.api.setFleet([]map[string]any{
				berthed,
				sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort),
			})
			candidate := Contract{
				ID: "unaccepted-collateral", ContractType: "freight", Status: "open",
				OriginPortID: recoveryHomePort, DestPortID: recoveryFarPort,
				CommodityID: recoveryGrain, QuantityRequired: 1, CollateralPence: 40_000,
			}
			downstream := h.api.server.Config.Handler
			h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method == http.MethodGet && request.URL.Path == "/v1/contract/list" {
					contracts := []Contract{}
					if stage == "accept-freight" && request.URL.Query().Get("status") == "open" {
						contracts = append(contracts, candidate)
					}
					h.api.writeJSON(writer, http.StatusOK, contracts)
					return
				}
				downstream.ServeHTTP(writer, request)
			})
			runner := h.runner(primaryTestKey)
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			ship, exists := runner.ship(recoveryShipBlastoise)
			if !exists {
				t.Fatal("missing berthed ship")
			}
			data := missionData{
				Plan: Plan{
					Kind: "spot", ShipID: ship.Ship.ID, Origin: recoveryHomePort,
					Destination: recoveryFarPort, Commodity: recoveryGrain,
					Quantity: 10, BuyLimitPence: 1_000, TargetStoresTons: 4,
				},
				CostsKnown: true,
			}
			phase := "buy"
			if stage == "accept-freight" {
				phase = "freight"
				data.Plan.FreightIDs = []string{candidate.ID}
				data.BoughtQuantity = 2
				data.BuyDone = true
				data.Freight = []freightObligation{{
					ContractID: "already-accepted", Commodity: recoveryGrain,
					Quantity: 1, Destination: recoveryFarPort,
				}}
			}
			h.putMission(ship.Ship.ID, "financial-restage-"+stage, phase, data)
			mission, data := h.mission(ship.Ship.ID)
			if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
				t.Fatalf("stage funded %s control: changed %t error %v holds %v", stage, changed, err, runner.holding)
			}
			mission, data = h.mission(ship.Ship.ID)
			if data.Action == nil || data.Action.Stage != stage {
				t.Fatalf("funded control did not stage %s: %+v", stage, data.Action)
			}
			oldID := data.Action.ID
			h.api.setVaultBalance("north", 75_000)
			runner.publishedFinancial.Add(1)
			runner.capturedFinancial = 1
			runner.generation = uuid.NewString()
			if err := runner.refresh(h.ctx); err != nil {
				t.Fatal(err)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("release invalidated %s: changed %t error %v", stage, changed, err)
			}
			mission, data = h.mission(ship.Ship.ID)
			ship, _ = runner.ship(ship.Ship.ID)
			if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
				t.Fatalf("replan %s from fresh funds: changed %t error %v holds %v", stage, changed, err, runner.holding)
			}
			mission, data = h.mission(ship.Ship.ID)
			if data.Action != nil || data.LastActionID == oldID || len(h.pending()) != 0 {
				t.Fatalf("fresh planning restaged an unaffordable %s or claimed a mutation: %+v", stage, data)
			}
			if stage == "buy" {
				if mission.Phase != "arrival" || data.Plan.Kind != "aborted" || data.BoughtQuantity != 0 || data.BuyDone {
					t.Fatalf("unbought procurement did not unwind safely: phase %s data %+v", mission.Phase, data)
				}
			} else if len(data.Plan.FreightIDs) != 0 || data.BoughtQuantity != 2 || !data.BuyDone ||
				len(data.Freight) != 1 || data.Freight[0].ContractID != "already-accepted" ||
				data.Freight[0].Settled || ship.CargoManifest[recoveryGrain] != 2 {
				t.Fatalf("collateral replanning changed acquired cargo or accepted obligations: %+v ship %+v", data, ship)
			}
			if h.api.mutationCount("/v1/market/place-order") != 0 || h.api.mutationCount("/v1/contract/accept-freight") != 0 {
				t.Fatal("financial replanning sent an unaffordable request")
			}
		})
	}
}

func TestFinancialEventDuringWorkCannotStagePreEventGrowth(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.setVaultBalance("north", 500_000)
	h.api.setVaultBalance("south", 100_000)
	h.seedLaneProofs()
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	// The runner admitted this REST capture before the financial event. Work
	// still sees its old vaults, so the staging fence must reject the plan.
	h.api.setVaultBalance("north", 75_000)
	runner.publishedFinancial.Add(1)
	if _, err := runner.work(h.ctx); err != nil {
		t.Fatalf("work invalidated after capture admission: %v", err)
	}
	h.missingMission("__growth")
	if len(h.pending()) != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
		t.Fatal("work staged or sent growth from the pre-event vault capture")
	}
}
