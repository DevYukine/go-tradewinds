package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func idleExecutionReviewRecordClock(t *testing.T, w *idleFallbackWorld, runner *Runner, hour int64, serverUTC time.Time, gap bool) {
	t.Helper()
	previous := runner.planningInputFrom(ShipState{}, runner.stateCache)
	previous.Metadata = make(map[string]DatasetMeta, len(runner.meta))
	for key, meta := range runner.meta {
		previous.Metadata[key] = meta
	}
	if gap {
		for _, key := range []string{"clock", "ships", "contracts"} {
			meta := previous.Metadata[key]
			meta.UpdatedAt = time.Now().Add(-10 * time.Minute)
			meta.AttemptedAt = meta.UpdatedAt
			previous.Metadata[key] = meta
		}
	}
	state := runner.stateCache
	state.Clock.RealUTC = serverUTC
	state.Clock.Calendar.TotalGameHours = hour
	now := time.Now().UTC()
	for _, key := range []string{"clock", "ships", "contracts"} {
		runner.meta[key] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: runner.generation, Complete: true}
	}
	// Each frame is a coherent REST capture, including executable quote times.
	for key, meta := range runner.meta {
		if meta.UpstreamAsOf != nil {
			meta.UpstreamAsOf = &serverUTC
			meta.UpdatedAt = now
			meta.AttemptedAt = now
			runner.meta[key] = meta
		}
	}
	for port, market := range state.Markets {
		if market.AsOf != nil {
			market.AsOf = &serverUTC
			state.Markets[port] = market
		}
	}
	runner.snapshot["books"] = encode(state.Markets)
	runner.stateCache = state
	runner.snapshot["clock"] = encode(state.Clock)
	runner.recordShipIdle(state, previous)
	runner.refreshSourcingHistory(w.h.ctx, runner.planningInput(ShipState{}))
	runner.refreshSourcingBudget(w.h.ctx, runner.planningInput(ShipState{}))
}

func TestIdleExecutionReviewCachedClockAfterGapCannotCreditDowntime(t *testing.T) {
	t.Parallel()
	w := newIdleFallbackWorld(t)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	// Fetch genuine public routes before the clock-only frames exhaust normal allowance.
	idleFallbackExhaustSeed(t, w.h)
	serverUTC := runner.stateCache.Clock.RealUTC.Add(time.Second)
	idleExecutionReviewRecordClock(t, w, runner, 100, serverUTC, false)
	idleExecutionReviewRecordClock(t, w, runner, 100, serverUTC, true)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
	idleExecutionReviewRecordClock(t, w, runner, 106, serverUTC.Add(time.Hour), false)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
	idleExecutionReviewRecordClock(t, w, runner, 112, serverUTC.Add(2*time.Hour), false)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
}

func TestIdleExecutionReviewBackwardUTCRecoversFromCoherentNewBaseline(t *testing.T) {
	t.Parallel()
	w := newIdleFallbackWorld(t)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	// Keep the fetched route evidence while requiring the independent idle budget.
	idleFallbackExhaustSeed(t, w.h)
	oldUTC := runner.stateCache.Clock.RealUTC.Add(2 * time.Hour)
	idleExecutionReviewRecordClock(t, w, runner, 100, oldUTC, false)
	idleExecutionReviewRecordClock(t, w, runner, 90, oldUTC.Add(-time.Hour), false)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
	idleExecutionReviewRecordClock(t, w, runner, 91, oldUTC.Add(-59*time.Minute), false)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), false)
	idleExecutionReviewRecordClock(t, w, runner, 97, oldUTC.Add(-53*time.Minute), false)
	idleFallbackExpect(t, w.input(t, runner, recoveryShipBlastoise), true)
}

func idleExecutionReviewSetFree(t *testing.T, w *idleFallbackWorld, runner *Runner, free int64) {
	t.Helper()
	ship, exists := runner.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("pilot ship missing")
	}
	in, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		balance := in.ReserveByRegion[vault.BankingRegion]
		if vault.BankingRegion == "north" {
			balance += free
		}
		vault.BalancePence = balance
		w.h.api.setVaultBalance(vault.BankingRegion, balance)
	}
}

func idleExecutionReviewPilot(t *testing.T) (*idleFallbackWorld, *Runner, *store.Mission, missionData) {
	t.Helper()
	w := newIdleFallbackWorld(t)
	w.h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 1.5, nil, nil)})
	idleFallbackExhaustSeed(t, w.h)
	runner := w.h.runner(primaryTestKey)
	w.observe(t, runner, 100)
	idleExecutionReviewSetFree(t, w, runner, 1_000_000)
	w.observe(t, runner, 106)
	ship, _ := runner.ship(recoveryShipBlastoise)
	started, _, err := runner.considerSourcing(w.h.ctx, ship)
	if err != nil || !started {
		t.Fatalf("select the liquidity-backed pilot: started=%v err=%v holds=%v", started, err, runner.holding)
	}
	mission, data := w.h.mission(recoveryShipBlastoise)
	if data.Relocation == nil || !data.Relocation.IdlePilot || data.Relocation.ExpenseCapPence != 20_000 || data.StartedGameHours != 106 {
		t.Fatalf("fixture did not select a bounded pilot at game hour 106: %+v", data)
	}
	return w, runner, mission, data
}

func idleExecutionReviewStageStores(t *testing.T, w *idleFallbackWorld, runner *Runner, mission *store.Mission, data *missionData) durableAction {
	t.Helper()
	ship, _ := runner.ship(recoveryShipBlastoise)
	quantity := int64(math.Ceil(data.Plan.TargetStoresTons - ship.Stores.ProvisionsTons))
	if quantity <= 0 {
		t.Fatal("pilot fixture has no discretionary store deficit")
	}
	limit := data.Plan.StoresLimitPence
	if err := runner.stage(w.h.ctx, mission, data, ship, "provision", "/v1/market/place-order", orderBody(ship, "provisions", "bid", quantity, limit, true)); err != nil {
		t.Fatal(err)
	}
	if data.Action == nil {
		t.Fatal("store order was not durably staged")
	}
	return *data.Action
}

// Keep actual request capture and filled-store accounting instead of treating
// an attempted mutation as a successful no-op. All endpoints remain local.
func idleExecutionReviewTransport(t *testing.T, w *idleFallbackWorld, runner *Runner) {
	t.Helper()
	downstream := w.h.api.server.Config.Handler
	w.h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/test/idle-write-pause" {
			writer.Header().Set("X-Ratelimit-Bucket", "write")
			writer.Header().Set("Retry-After", "1")
			w.h.api.writeJSON(writer, http.StatusOK, map[string]any{})
			return
		}
		if request.Method != http.MethodPost {
			downstream.ServeHTTP(writer, request)
			return
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read synthetic pilot request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		var order struct {
			ShipID     string `json:"ship_id"`
			Quantity   int64  `json:"quantity"`
			Price      int64  `json:"price_pence"`
			IntoStores bool   `json:"into_stores"`
		}
		if err := json.Unmarshal(body, &order); err != nil {
			t.Errorf("decode synthetic pilot request: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		w.h.api.mu.Lock()
		defer w.h.api.mu.Unlock()
		if request.URL.Path == "/v1/fleet/offshore" {
			w.h.api.fleet[0]["ship"].(map[string]any)["status"] = "anchored_roadstead"
			w.h.api.record(request, body, http.StatusOK)
			w.h.api.writeJSON(writer, http.StatusOK, map[string]any{"ship_id": order.ShipID, "status": "anchored_roadstead", "berth_dues_pence": 0, "berth_dues_debt_pence": 0})
			return
		}
		if request.URL.Path != "/v1/market/place-order" || !order.IntoStores || order.Quantity <= 0 {
			w.h.api.record(request, body, http.StatusConflict)
			w.h.api.writeProblem(writer, http.StatusConflict, "unexpected_pilot_mutation", "only real store fills or safe offshore transitions are modeled")
			return
		}
		market := runner.stateCache.Markets[recoveryHomePort]
		entry := bookCommodity(market, "provisions")
		if entry == nil || entry.Depth == nil {
			t.Error("pilot request has no actual executable store book")
			writer.WriteHeader(http.StatusConflict)
			return
		}
		fill, err := WalkDepth(entry.Depth.Asks, order.Quantity, true)
		if err != nil || fill.LimitPence > order.Price {
			t.Errorf("pilot IOC exceeded actual observed asks: %+v %v", fill, err)
			writer.WriteHeader(http.StatusConflict)
			return
		}
		customs := ceilBPS(fill.GrossPence, market.CustomsBPS)
		w.h.api.vaultBalance["north"] -= fill.GrossPence + customs
		w.h.api.fleet[0]["stores"].(map[string]any)["provisions_tons"] = runner.stateCache.Ships[0].Stores.ProvisionsTons + float64(order.Quantity)
		w.h.api.record(request, body, http.StatusOK)
		w.h.api.writeJSON(writer, http.StatusOK, map[string]any{"filled_quantity": order.Quantity, "total_gross_pence": fill.GrossPence, "customs_duty_pence": customs, "commodity_id": "provisions"})
	})
}

func idleExecutionReviewNoSend(t *testing.T, w *idleFallbackWorld, runner *Runner, mission *store.Mission, data *missionData, action durableAction) {
	t.Helper()
	cap := data.Relocation.ExpenseCapPence
	if _, err := runner.dispatch(w.h.ctx, mission, data); err != nil {
		t.Fatal(err)
	}
	if calls := w.h.api.mutationsFor(action.Path); len(calls) != 0 {
		t.Fatalf("invalidated discretionary pilot crossed transport: %+v", calls)
	}
	_, retained := w.h.mission(recoveryShipBlastoise)
	if retained.Action != nil || len(w.h.pending()) != 0 || retained.Relocation == nil || retained.Relocation.ExpenseCapPence != cap || retained.AcquisitionPence != data.AcquisitionPence || retained.FeesPence != data.FeesPence {
		t.Fatalf("no-send veto changed committed accounting/cap or retained unsent intent: %+v", retained)
	}
}

func TestIdleExecutionReviewOriginFirstSendHonorsClockRebase(t *testing.T) {
	t.Parallel()
	w, runner, mission, data := idleExecutionReviewPilot(t)
	idleExecutionReviewTransport(t, w, runner)
	w.observe(t, runner, 110)
	action := idleExecutionReviewStageStores(t, w, runner, mission, &data)
	w.observe(t, runner, 109)
	if runner.shipIdleEligible(w.input(t, runner, recoveryShipBlastoise)) {
		t.Fatal("regressed observation unexpectedly remains eligible")
	}
	idleExecutionReviewNoSend(t, w, runner, mission, &data, action)
}

func idleExecutionReviewIncome(t *testing.T, w *idleFallbackWorld, runner *Runner) {
	t.Helper()
	runner.stateCache.Markets[recoveryFarPort] = sourcingFixture[Market](t, `{"port_id":"portb","visibility":"quay","customs_bps":200,"books":[{"commodity_id":"provisions","depth":{"asks":[],"bids":[{"order_id":"idle-new-income","price_pence":5000,"remaining_quantity":100}]}}]}`)
	now := time.Now().UTC()
	runner.meta["books/"+recoveryFarPort] = DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: runner.generation, Complete: true}
	ship, _ := runner.ship(recoveryShipBlastoise)
	in, err := runner.committedPlanningInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	if plans := PlanVoyages(in); len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("new funded ordinary opportunity is not executable: %+v", plans)
	}
}

func TestIdleExecutionReviewNewOrdinaryIncomeWinsBeforeFirstSend(t *testing.T) {
	t.Parallel()
	w, runner, mission, data := idleExecutionReviewPilot(t)
	idleExecutionReviewTransport(t, w, runner)
	action := idleExecutionReviewStageStores(t, w, runner, mission, &data)
	idleExecutionReviewIncome(t, w, runner)
	idleExecutionReviewNoSend(t, w, runner, mission, &data, action)
}

func TestIdleExecutionReviewPostLimiterRechecksCashAndPermanentCosts(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"shrinking cash", "unknown probe cost", "new ordinary income"} {
		t.Run(change, func(t *testing.T) {
			w, runner, mission, data := idleExecutionReviewPilot(t)
			idleExecutionReviewTransport(t, w, runner)
			action := idleExecutionReviewStageStores(t, w, runner, mission, &data)
			if err := runner.newActionPreflight(w.h.ctx, mission, &data); err != nil {
				t.Fatalf("unchanged pilot first send must initially be admissible: %v", err)
			}
			injected := false
			core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
			runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				if injected || entry.Level != zap.DebugLevel {
					return nil
				}
				injected = true
				var response json.RawMessage
				if err := runner.api.Get(w.h.ctx, "/test/idle-write-pause", &response); err != nil {
					return err
				}
				switch change {
				case "shrinking cash":
					idleExecutionReviewSetFree(t, w, runner, 1_000)
				case "unknown probe cost":
					idleFallbackSettlement(t, w.h, store.Settlement{ID: "post-limiter-unpriced-probe", Kind: "relocation", CostsKnown: false})
				case "new ordinary income":
					idleExecutionReviewIncome(t, w, runner)
				}
				return nil
			}))
			idleExecutionReviewNoSend(t, w, runner, mission, &data, action)
			if !injected {
				t.Fatal("dispatch did not cross the real limiter boundary")
			}
			journaled, err := w.h.db.BeginAction(w.h.ctx, &store.Action{ID: action.ID, AccountID: w.h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body})
			if err != nil || journaled.State != store.ActionReconciled || journaled.Status != 0 {
				t.Fatalf("postwait veto has no durable known-unsent proof: %+v %v", journaled, err)
			}
		})
	}
}

func TestIdleExecutionReviewTieredStoreIOCUsesCurrentReducedCap(t *testing.T) {
	t.Parallel()
	w, runner, mission, data := idleExecutionReviewPilot(t)
	idleExecutionReviewTransport(t, w, runner)
	idleExecutionReviewSetFree(t, w, runner, 100_000)
	market := runner.stateCache.Markets[recoveryHomePort]
	stores := bookCommodity(market, "provisions")
	stores.Depth.Asks = []Level{{OrderID: "cheap-first-ton", PricePence: 50, RemainingQuantity: 1}, {OrderID: "expensive-next-ton", PricePence: 40_000, RemainingQuantity: 100}}
	runner.stateCache.Markets[recoveryHomePort] = market
	ship, _ := runner.ship(recoveryShipBlastoise)
	in, err := runner.sourcingInput(w.h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	plan := costRelocation(in, data.Plan)
	if !plan.Accepted || plan.StoresToBuy != 2 || plan.StoresLimitPence != 40_000 {
		t.Fatalf("fixture must fund the two-tier store IOC: %+v", plan)
	}
	unit := int64(40_000) + ceilBPS(40_000, market.CustomsBPS)
	bounded := data
	intent := *data.Relocation
	intent.ExpenseCapPence = sourcingAllowance(in)
	bounded.Relocation = &intent
	if !runner.relocationProjectedBudget(&bounded, ship, in, plan, 0) || runner.relocationProjectedBudget(&bounded, ship, in, plan, unit) || !runner.relocationProjectedBudget(&data, ship, in, plan, unit) {
		t.Fatalf("fixture must separate cheap consumed-store valuation, current cap, and IOC ceiling: cap=%d plan=%+v", intent.ExpenseCapPence, plan)
	}
	data.Plan = plan
	action := idleExecutionReviewStageStores(t, w, runner, mission, &data)
	idleExecutionReviewNoSend(t, w, runner, mission, &data, action)
}

func TestIdleExecutionReviewActiveCostsAndBillsCountOnceWithoutGrowingCap(t *testing.T) {
	t.Parallel()
	w, runner, mission, data := idleExecutionReviewPilot(t)
	w.h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", data.Plan.Destination, "berthed", 1.5, nil, nil)})
	mission.Phase = "inspect"
	if err := runner.saveMission(w.h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	w.observe(t, runner, 107)
	idleExecutionReviewSetFree(t, w, runner, 50_000)
	ship, _ := runner.ship(recoveryShipBlastoise)
	data.FeesPence, data.ExpensesPence = 100, 200
	runner.snapshot["ledger"] = encode([]map[string]any{{"id": "idle-unclaimed-bill", "debit_account_id": "vault-north", "credit_account_id": "harbour", "amount_pence": 700, "reason": "berth_dues"}})
	cap := data.Relocation.ExpenseCapPence
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, 0); err != nil {
		t.Fatalf("300p active expense plus 700p unclaimed bill must fit exactly the current 1000p cap at the committed source: %v", err)
	}
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, 1); err == nil {
		t.Fatal("one penny beyond current capital was allowed")
	}
	amount, ids := runner.claimBillsSince(&data, mission.Generation)
	if amount != 700 || len(ids) != 1 {
		t.Fatalf("claim actual overhead once: amount=%d ids=%v", amount, ids)
	}
	data.ExpensesPence += amount
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, 0); err != nil {
		t.Fatalf("claimed bill was charged a second time: %v", err)
	}
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, 1); err == nil {
		t.Fatal("claiming the bill erased its risk debit")
	}
	idleExecutionReviewSetFree(t, w, runner, 10_000_000)
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, cap-1000); err != nil {
		t.Fatalf("unchanged durable cap should retain its remaining measured risk: %v", err)
	}
	if err := runner.relocationSpendingReady(w.h.ctx, ship, &data, cap-999); err == nil {
		t.Fatal("higher cash enlarged the existing pilot's durable cap")
	}
	if data.Relocation.ExpenseCapPence != cap {
		t.Fatal("risk revalidation mutated the immutable cap")
	}
}

func TestIdleExecutionReviewAttemptedIdentityAndSafetyRemainRecoverable(t *testing.T) {
	t.Parallel()
	for _, safety := range []bool{false, true} {
		t.Run(fmt.Sprintf("safety=%v", safety), func(t *testing.T) {
			w, runner, mission, data := idleExecutionReviewPilot(t)
			idleExecutionReviewTransport(t, w, runner)
			var action durableAction
			if safety {
				ship, _ := runner.ship(recoveryShipBlastoise)
				if err := runner.stage(w.h.ctx, mission, &data, ship, "offshore", "/v1/fleet/offshore", map[string]any{"ship_id": ship.Ship.ID}); err != nil {
					t.Fatal(err)
				}
				action = *data.Action
			} else {
				action = idleExecutionReviewStageStores(t, w, runner, mission, &data)
				if _, err := w.h.db.BeginAction(w.h.ctx, &store.Action{ID: action.ID, AccountID: w.h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body}); err != nil {
					t.Fatal(err)
				}
			}
			idleFallbackSettlement(t, w.h, store.Settlement{ID: "new-unpriced-probe", Kind: "relocation", CostsKnown: false})
			cap := data.Relocation.ExpenseCapPence
			if _, err := runner.dispatch(w.h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := w.h.api.mutationsFor(action.Path)
			if len(calls) != 1 || calls[0].IdempotencyKey != action.ID || !bytes.Equal(calls[0].Body, action.Body) {
				t.Fatalf("new uncertainty stranded necessary safety or replaced an attempted immutable request: %+v", calls)
			}
			_, retained := w.h.mission(recoveryShipBlastoise)
			if retained.Relocation == nil || retained.Relocation.ExpenseCapPence != cap {
				t.Fatalf("recovery enlarged or discarded the durable pilot cap: %+v", retained)
			}
		})
	}
}
