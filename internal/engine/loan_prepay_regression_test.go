package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func TestLoanPrepayAccruedInterestDoesNotWedgeService(t *testing.T) {
	h, runner, api, _ := moneyPrepayFixture(t, []int64{563_785, 563_789}, 2_000_000, false)
	base := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/v1/loan/repay" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			var payload gameapi.RepayLoanRequest
			if err := json.Unmarshal(body, &payload); err != nil || payload.AmountPence != nil {
				t.Errorf("full prepayment must omit amount: %s, %v", body, err)
			}
			api.mu.Lock()
			for i := range api.loans {
				if api.loans[i].ID == payload.LoanID {
					api.loans[i].OwedPence += 100
					api.loans[i].AccruedInterestPence += 100
				}
			}
			api.mu.Unlock()
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		base.ServeHTTP(writer, request)
	})
	moneyLoanWork(t, h, runner, 16)
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, loan := range api.loans {
		if loan.OwedPence != 0 || api.repayments[loan.ID] != 1 {
			t.Fatalf("accrued interest left service wedged or blocked the next loan: %+v, payments=%v", loan, api.repayments)
		}
	}
	mission, err := h.db.GetMission(h.ctx, h.account.ID, "__loans")
	if err != nil || mission != nil || len(h.pending()) != 0 {
		t.Fatalf("completed prepayment did not release loan service: mission=%+v err=%v", mission, err)
	}
}

func TestLoanPrepayObservedLegacyPaymentReleasesService(t *testing.T) {
	h, runner, api, _ := moneyPrepayFixture(t, []int64{563_785}, 1_000_000, false)
	before := runner.stateCache.Loans[0]
	api.mu.Lock()
	api.loans[0].InterestPaidPence += 100
	api.loans[0].AccruedInterestPence -= 100
	api.loans[0].OwedPence -= 100
	api.mu.Unlock()
	h.putMission("__loans", "legacy-prepay", "loan_prepay_done", missionData{
		LoanRepayment: &loanRepaymentIntent{Before: before, AmountPence: 100},
	})
	if changed, err := runner.serviceLoans(h.ctx); err != nil || !changed {
		t.Fatalf("observed legacy repayment wedged loan service: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	if mission, err := h.db.GetMission(h.ctx, h.account.ID, "__loans"); err != nil || mission != nil {
		t.Fatalf("observed repayment retained its service mission: mission=%+v err=%v", mission, err)
	}
}

func TestLoanPrepayContinuesOnUnrelatedGrowthHold(t *testing.T) {
	for _, dataset := range []string{"zones", "vaults"} {
		t.Run(dataset, func(t *testing.T) {
			h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 1_000_000, false)
			meta := runner.meta[dataset]
			meta.Complete = false
			runner.meta[dataset] = meta
			changed, err := runner.considerGrowth(h.ctx)
			want := dataset == "zones" // Zone authority is needed for a hull, not a regional payoff.
			if err != nil || changed != want {
				t.Fatalf("unrelated growth hold payoff: changed=%t want=%t err=%v holds=%v", changed, want, err, runner.holding)
			}
			if want {
				if h.api.mutationCount("/v1/loan/repay") != 1 {
					t.Fatal("growth hold did not execute its funded prepayment")
				}
			} else if len(h.pending()) != 0 {
				t.Fatal("prepayment bypassed missing cash authority")
			}
		})
	}
}

func TestGrowthLaneAndPreflightRejectNonCheapHulls(t *testing.T) {
	for _, class := range []string{"hoy", "barque", "fluyt", "frigate", "packet"} {
		t.Run(class, func(t *testing.T) {
			h := newRecoveryHarness(t)
			h.seedLaneProofs()
			runner := growthWindowRunner(t, h)
			original := runner.stateCache.Catalog.ShipClasses[0]
			hull := original
			hull.ID = class
			runner.stateCache.Catalog.ShipClasses = []Hull{hull}
			for _, port := range runner.stateCache.Catalog.Ports {
				oldKey := port.ID + "|" + original.ID
				if quote, ok := runner.stateCache.Quotes[oldKey]; ok {
					key := port.ID + "|" + class
					quote.ShipClass = class
					runner.stateCache.Quotes[key] = quote
					runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+oldKey]
				}
				for _, destination := range runner.stateCache.Catalog.Ports {
					oldKey := routeKey(port.ID, destination.ID, original.ID)
					if route, ok := runner.stateCache.Routes[oldKey]; ok {
						key := routeKey(port.ID, destination.ID, class)
						route.ShipClass = class
						runner.stateCache.Routes[key] = route
						runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
					}
				}
			}
			changed, err := runner.considerGrowth(h.ctx)
			if err != nil || changed != (class == "hoy") {
				t.Fatalf("lane hull gate: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			if class == "hoy" {
				_, data := h.mission("__growth")
				if data.Capacity != nil || data.GrowthLane == nil {
					t.Fatal("control must exercise lane growth, not fallback capacity growth")
				}
			} else {
				err := runner.growthBuildReady(h.ctx, &missionData{BuildClass: class})
				if err == nil || !strings.Contains(err.Error(), "cheap reinvestment hull") {
					t.Fatalf("non-cheap staged build escaped hull preflight: %v", err)
				}
				if len(h.pending()) != 0 {
					t.Fatalf("non-cheap %s left a build intent", class)
				}
			}
		})
	}
}

func TestLoanPrepayWaitsForPendingExemptMissionActions(t *testing.T) {
	for _, owner := range []string{"__marque", "__underwriting", "__futures", recoveryShipBlastoise} {
		t.Run(owner, func(t *testing.T) {
			h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 2_000_000, false)
			action, err := h.db.BeginAction(h.ctx, &store.Action{
				ID: uuid.NewString(), AccountID: h.account.ID, Method: http.MethodPost,
				Path: "/v1/market/place-order", Body: encode(map[string]any{"quantity": 1}),
			})
			if err != nil {
				t.Fatal(err)
			}
			h.putMission(owner, uuid.NewString(), "pending", missionData{
				Action: &durableAction{ID: action.ID, Stage: "buy", Path: action.Path, Body: action.Body},
			})
			if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
				t.Fatalf("unknown exempt action cash effect permitted spending: changed=%t err=%v", changed, err)
			}
			h.missingMission("__loans")
			h.missingMission("__growth")
			if h.api.mutationCount("/v1/loan/repay") != 0 || len(h.pending()) != 1 {
				t.Fatal("capacity hold spent cash before the pending action was resolved")
			}
		})
	}
}

func TestGrowthWaitsForStagedFuturesControlAction(t *testing.T) {
	h := newRecoveryHarness(t)
	h.seedLaneProofs()
	runner := growthWindowRunner(t, h)
	h.putMission("__futures", uuid.NewString(), "futures_lease", missionData{
		Action: &durableAction{ID: uuid.NewString(), Stage: "futures-lease"},
	})
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("staged futures spend permitted growth: changed=%t err=%v", changed, err)
	}
	h.missingMission("__growth")
	if err := h.db.DeleteMission(h.ctx, h.account.ID, "__futures"); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("control did not have an otherwise admissible growth package: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
}

func TestLoanPrepayPreservesDominantRegionalFloor(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra int64
	}{
		{name: "one penny short", extra: -1},
		{name: "exact funded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, _, _ := moneyPrepayFixture(t, []int64{563_785}, 0, false)
			port := runner.stateCache.Ports[recoveryHomePort]
			port.BerthDuesPerDayPence = 100_000
			runner.stateCache.Ports[recoveryHomePort] = port
			h.putMission(recoveryShipBlastoise, uuid.NewString(), "sailing", missionData{
				Plan:    Plan{Kind: "spot", Origin: recoveryFarPort, Destination: recoveryHomePort},
				BuyDone: true,
			})
			// The mission's 3 × 100,000p berth floor dominates debt reserves; two 10,000p cash lots remain protected.
			protected := runner.opts.ReservePence + 300_000 + 20_000
			for i := range runner.stateCache.Vaults {
				vault := &runner.stateCache.Vaults[i]
				if vault.BankingRegion == "north_sea" {
					vault.BalancePence = protected + 563_785 + 468 + test.extra
					h.api.setVaultBalance("north", vault.BalancePence)
				}
			}
			changed, err := runner.prepayLoans(h.ctx)
			if err != nil || changed != (test.extra == 0) {
				t.Fatalf("dominant floor prepayment boundary: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			if test.extra == 0 {
				h.api.mu.Lock()
				cash := h.api.vaultBalance["north"]
				h.api.mu.Unlock()
				if cash != protected+468 {
					t.Fatalf("payoff spent the dominant floor: cash=%d want=%d", cash, protected+468)
				}
			} else if h.api.mutationCount("/v1/loan/repay") != 0 {
				t.Fatal("payoff spent a penny of the protected regional floor")
			}
		})
	}
}
