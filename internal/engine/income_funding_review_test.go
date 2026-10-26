package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func incomeReviewRefresh(t *testing.T, h *recoveryHarness, runner *Runner) {
	t.Helper()
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
}

func incomeReviewFixture(t *testing.T, kind string, locallyFunded bool, stores float64) (*recoveryHarness, *Runner, ShipState, Plan) {
	t.Helper()
	h, runner := incomeFundingRunner(t)
	h.api.mu.Lock()
	h.api.fleet[0]["stores"] = map[string]any{"provisions_tons": stores, "sea_days_remaining": 5}
	h.api.mu.Unlock()
	if locallyFunded {
		h.api.setVaultBalance("north", 1_000_000)
	}
	expires := time.Now().UTC().Add(7 * 24 * time.Hour)
	reward := int64(1_000)
	contracts := []Contract{{
		ID: "review-charter", ContractType: "charter", Status: "open", OriginPortID: recoveryFarPort,
		CommodityID: recoveryGrain, QuantityRequired: 1_000, RemainingQuantity: 1_000,
		RewardPerTonPence: &reward, ExpiresAt: &expires,
	}}
	if kind == "freight" || strings.HasSuffix(kind, "+freight") {
		if kind == "freight" {
			contracts = nil
		} else {
			// Cargo alone loses money after customs and harbour bills. Its
			// admission must not borrow the reward of an unusable freight lot.
			reward = 100
		}
		contracts = append(contracts, Contract{
			ID: "review-freight", ContractType: "freight", Status: "open", OriginPortID: recoveryHomePort,
			DestPortID: recoveryFarPort, CommodityID: recoveryGrain, IssuerCompanyID: "other-company",
			QuantityRequired: 1, RemainingQuantity: 1, RewardPence: 20_000, CollateralPence: 40_000,
			ExpiresAt: &expires,
		})
	}
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/contract/list" {
			rows := []Contract{}
			if request.URL.Query().Get("status") == "open" {
				rows = contracts
			}
			h.api.writeJSON(writer, http.StatusOK, rows)
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	incomeReviewRefresh(t, h, runner)
	ship, ok := runner.ship(recoveryShipBlastoise)
	if !ok {
		t.Fatal("ordinary income ship missing")
	}
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range PlanVoyages(input) {
		if plan.Accepted && plan.Kind == kind {
			return h, runner, ship, plan
		}
	}
	t.Fatalf("fixture lacks the requested live funded %s: %+v", kind, PlanVoyages(input))
	return nil, nil, ShipState{}, Plan{}
}

func incomeReviewStage(t *testing.T, h *recoveryHarness, runner *Runner, ship ShipState, plan Plan, stage string) (*store.Mission, missionData) {
	t.Helper()
	mission, data := runner.newVoyage(ship, plan)
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
			t.Fatal(err)
		}
		mission, data = h.mission(ship.Ship.ID)
		if data.Action != nil {
			if data.Action.Stage != stage {
				t.Fatalf("expected %s before dispatch, got %+v", stage, data.Action)
			}
			return mission, data
		}
	}
	t.Fatalf("live ordinary opportunity did not stage %s: %+v", stage, data)
	return nil, missionData{}
}

func incomeReviewExpire(runner *Runner, contractType string) {
	expired := time.Now().UTC().Add(-time.Second)
	for i := range runner.stateCache.Contracts {
		if runner.stateCache.Contracts[i].ContractType == contractType {
			runner.stateCache.Contracts[i].ExpiresAt = &expired
		}
	}
}

// The existing debug hook runs after the initial preflight and journal creation.
// It publishes real write-bucket response headers, without depending on log text.
func incomeReviewDuringWait(t *testing.T, h *recoveryHarness, runner *Runner, change func()) *bool {
	t.Helper()
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/test/income-review-write-pause" {
			writer.Header().Set("X-Ratelimit-Bucket", "write")
			writer.Header().Set("Retry-After", "1")
			h.api.writeJSON(writer, http.StatusOK, map[string]any{})
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	injected := false
	core := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.DebugLevel)
	runner.log = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if injected || entry.Level != zap.DebugLevel {
			return nil
		}
		injected = true
		var response json.RawMessage
		if err := runner.api.Get(h.ctx, "/test/income-review-write-pause", &response); err != nil {
			return err
		}
		change()
		return nil
	}))
	return &injected
}

func incomeReviewRequireNoSend(t *testing.T, h *recoveryHarness, action durableAction) {
	t.Helper()
	if h.api.mutationCount(action.Path) != 0 || len(h.pending()) != 0 {
		t.Fatalf("invalid opportunity crossed transport or retained an ambiguous request: calls=%+v pending=%+v", h.api.mutationsFor(action.Path), h.pending())
	}
	journaled, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
	})
	if err != nil {
		t.Fatal(err)
	}
	if journaled.State != store.ActionReconciled || journaled.Status != 0 || !json.Valid(journaled.Response) {
		t.Fatalf("post-wait veto lacks durable no-send proof: %+v", journaled)
	}
}

func TestIncomeFundingReviewExpiredFreightCannotFundPlanning(t *testing.T) {
	t.Parallel()
	for _, mixed := range []bool{false, true} {
		name := "freight"
		if mixed {
			name = "charter+freight"
		}
		t.Run(name, func(t *testing.T) {
			in := incomeFundingInput(t, "freight")
			if mixed {
				charter := incomeFundingInput(t, "charter").Contracts[0]
				reward := int64(900)
				charter.RewardPerTonPence = &reward
				in.Contracts = append(in.Contracts, charter)
				in.Contracts[0].QuantityRequired = 1
				in.Contracts[0].RewardPence = 20_000
			}
			var pinned Plan
			for _, plan := range PlanVoyages(in) {
				if plan.Accepted && plan.Kind == name {
					pinned = plan
					break
				}
			}
			if !pinned.Accepted {
				t.Fatalf("live freight subsidy control unavailable: %+v", PlanVoyages(in))
			}
			expired := in.Now.Add(-time.Second)
			in.Contracts[0].ExpiresAt = &expired
			for _, plan := range PlanVoyages(in) {
				if plan.Accepted && len(plan.FreightIDs) > 0 {
					t.Fatalf("expired freight supplied spendable reward or collateral funding: %+v", plan)
				}
			}
			if quote := quoteIncomePlan(in, pinned); quote.plan.Accepted || len(quote.transfers) != 0 {
				t.Fatalf("pinned expired freight still funds an ordinary shipment: %+v", quote)
			}
		})
	}
}

func TestIncomeFundingReviewExpiredFreightCannotStageRemittance(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"freight", "charter+freight"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, ship, plan := incomeReviewFixture(t, kind, false, 4)
			mission, data := runner.newVoyage(ship, plan)
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			incomeReviewExpire(runner, "freight")
			if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
				t.Fatal(err)
			}
			_, retained := h.mission(ship.Ship.ID)
			if retained.Action != nil || h.api.mutationCount("/v1/treasury/remit") != 0 || len(h.pending()) != 0 {
				t.Fatalf("expired freight staged new spending: %+v", retained)
			}
		})
	}
}

func TestIncomeFundingReviewFreightExpiryDuringLimiterWaitStopsFundingAndSubsidizedBuy(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		kind  string
		stage string
	}{
		{kind: "freight", stage: "remit"},
		{kind: "charter+freight", stage: "remit"},
		{kind: "charter+freight", stage: "buy"},
	} {
		t.Run(scenario.kind+"/"+scenario.stage, func(t *testing.T) {
			h, runner, ship, plan := incomeReviewFixture(t, scenario.kind, scenario.stage == "buy", 4)
			mission, data := incomeReviewStage(t, h, runner, ship, plan, scenario.stage)
			action := *data.Action
			injected := incomeReviewDuringWait(t, h, runner, func() { incomeReviewExpire(runner, "freight") })
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if !*injected {
				t.Fatal("live control never reached the post-preflight limiter boundary")
			}
			incomeReviewRequireNoSend(t, h, action)
		})
	}
}

func TestIncomeFundingReviewStaleVoyageCannotProvisionOrDock(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"provision", "dock"} {
		for _, postWait := range []bool{false, true} {
			name := stage + "/before-dispatch"
			if postWait {
				name = stage + "/during-wait"
			}
			t.Run(name, func(t *testing.T) {
				stores := float64(4)
				if stage == "provision" {
					// Above independent idle safety stock, below this voyage's target.
					stores = 1.2
				}
				h, runner, ship, plan := incomeReviewFixture(t, "charter", true, stores)
				if stage == "dock" {
					h.api.mu.Lock()
					h.api.fleet[0]["ship"].(map[string]any)["status"] = "anchored_roadstead"
					h.api.mu.Unlock()
					incomeReviewRefresh(t, h, runner)
					ship, _ = runner.ship(ship.Ship.ID)
				}
				mission, data := incomeReviewStage(t, h, runner, ship, plan, stage)
				action := *data.Action
				var injected *bool
				if postWait {
					injected = incomeReviewDuringWait(t, h, runner, func() { incomeReviewExpire(runner, "charter") })
				} else {
					incomeReviewExpire(runner, "charter")
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if postWait {
					if !*injected {
						t.Fatal("live voyage did not reach limiter boundary")
					}
					incomeReviewRequireNoSend(t, h, action)
				} else if h.api.mutationCount(action.Path) != 0 || len(h.pending()) != 0 {
					t.Fatalf("stale opportunity bought voyage stores or opened a discretionary berth: %+v", h.api.mutationsFor(action.Path))
				}
			})
		}
	}
}

func TestIncomeFundingReviewIndependentSafetyMaintenanceRemainsAvailable(t *testing.T) {
	t.Parallel()
	for _, roadstead := range []bool{false, true} {
		t.Run(map[bool]string{false: "provision", true: "dock"}[roadstead], func(t *testing.T) {
			h, runner, _, reserves := idleMaintenanceFixture(t, roadstead)
			h.api.setVaultBalance("north", reserves["north"]+100_000)
			incomeReviewRefresh(t, h, runner)
			ship, _ := runner.ship(recoveryShipBlastoise)
			if _, err := runner.maintain(h.ctx, ship, nil, nil); err != nil {
				t.Fatal(err)
			}
			mission, data := h.mission(ship.Ship.ID)
			if data.Action == nil {
				if _, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				mission, data = h.mission(ship.Ship.ID)
			}
			if data.Action == nil {
				t.Fatalf("necessary independent maintenance did not stage: %+v", data)
			}
			path := data.Action.Path
			if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor(path)
			if len(calls) != 1 || calls[0].Status != http.StatusOK {
				t.Fatalf("ordinary-opportunity guards disabled independent safety maintenance: %+v", calls)
			}
		})
	}
}

func TestIncomeFundingReviewAccruedBerthDuesAllowReserveSafePartialFundingProgress(t *testing.T) {
	t.Parallel()
	h, runner, ship, plan := incomeReviewFixture(t, "charter", false, 4)
	mission, data := incomeReviewStage(t, h, runner, ship, plan, "remit")
	action := *data.Action
	berthedAt := time.Now().UTC().Add(-60 * time.Hour)
	h.api.mu.Lock()
	h.api.fleet[0]["ship"].(map[string]any)["berthed_at"] = berthedAt.Format(time.RFC3339Nano)
	h.api.mu.Unlock()
	incomeReviewRefresh(t, h, runner)
	ship, _ = runner.ship(ship.Ship.ID)
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor("/v1/treasury/remit")
	if len(calls) != 1 || calls[0].Status != http.StatusOK || !bytes.Equal(calls[0].Body, action.Body) {
		t.Fatalf("accruing harbour dues discarded a still-needed safe partial remittance: %+v", calls)
	}
	ready := false
	for range 12 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		mission, data = h.mission(ship.Ship.ID)
		if data.Action != nil && data.Action.Stage == "buy" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("funding chased changing exact shortfalls instead of reaching procurement: %+v", data)
	}
	var fees int64
	for _, call := range h.api.mutationsFor("/v1/treasury/remit") {
		var request struct {
			From  string `json:"from_region"`
			To    string `json:"to_region"`
			Gross int64  `json:"gross_amount_pence"`
		}
		if err := json.Unmarshal(call.Body, &request); err != nil {
			t.Fatal(err)
		}
		if call.Status != http.StatusOK || request.From != "south" || request.To != "north" || request.Gross <= 0 {
			t.Fatalf("partial funding escaped the observed donor route: %+v", call)
		}
		fees += request.Gross * 100 / 10_000
	}
	if fees <= 0 || data.FeesPence != fees || data.Plan.Economics.FundingFeesPence != fees || data.Plan.Economics.ExpectedNetPence <= 0 {
		t.Fatalf("partial funding erased margin or lost/double-charged paid fees: %+v actual fees=%d", data, fees)
	}
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	for region, reserve := range input.ReserveByRegion {
		if input.Balances[region] < reserve {
			t.Fatalf("partial funding spent protected %s cash: balance=%d reserve=%d", region, input.Balances[region], reserve)
		}
	}
}

func TestIncomeFundingReviewAttemptedFundingRetainsImmutableReplay(t *testing.T) {
	t.Parallel()
	h, runner, ship, plan := incomeReviewFixture(t, "freight", false, 4)
	mission, data := incomeReviewStage(t, h, runner, ship, plan, "remit")
	action := *data.Action
	if _, err := h.db.BeginAction(h.ctx, &store.Action{
		ID: action.ID, AccountID: h.account.ID, Method: action.Method, Path: action.Path, Body: action.Body,
	}); err != nil {
		t.Fatal(err)
	}
	incomeReviewExpire(runner, "freight")
	if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	calls := h.api.mutationsFor(action.Path)
	if len(calls) != 1 || calls[0].IdempotencyKey != action.ID || !bytes.Equal(calls[0].Body, action.Body) {
		t.Fatalf("expired opportunity rewrote or suppressed an already-attempted immutable remittance: %+v", calls)
	}
}
