package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestInstallmentConsumerFollowupArrivalUsesActualReservePhaseOnset(t *testing.T) {
	t.Parallel()
	input, state, snapshot := installmentIncomeFixture(t, time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC))
	loan := state.Loans[0]
	lead := gameDuration(loanServiceLeadGameDays, input.Clock.Calendar.TimeScale)
	hour := gameDuration(1.0/24, input.Clock.Calendar.TimeScale)
	onset := loan.NextPaymentDueAt.Add(-lead - serverClockStaleness)
	if len(input.DeferredLoanReserves) != 1 || !input.DeferredLoanReserves[0].ReserveFrom.Equal(onset) {
		t.Fatalf("arrival cutoff does not match the earliest reserve phase: %+v, want %s", input.DeferredLoanReserves, onset)
	}
	plan := priceIncomePlan(input, incomeFundingBest(t, input), 0)
	for _, test := range []struct {
		name    string
		arrival time.Time
		allowed bool
	}{
		{"exact one-game-hour slack", onset.Add(-hour), true},
		{"one nanosecond late", onset.Add(-hour + time.Nanosecond), false},
		{"old nominal cutoff", loan.NextPaymentDueAt.Add(-lead - hour), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := plan
			candidate.ExpectedArrival = test.arrival
			quote := fundVoyage(input, candidate)
			if quote.plan.Accepted != test.allowed {
				t.Fatalf("arrival %s at actual onset %s: allowed=%t quote=%+v", test.arrival, onset, test.allowed, quote)
			}
		})
	}
	for _, offset := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		state.Clock.RealUTC = onset.Add(offset)
		required, deferred := obligationReservePlan(state, snapshot, ProtectedReservePence(state))
		want := int64(0)
		if offset >= 0 {
			want = *loan.NextPaymentPence
		}
		if required[loan.BankingRegion] != want || (len(deferred) == 1) != (offset < 0) {
			t.Fatalf("actual reserve onset offset %s: reserve=%d want=%d deferred=%+v", offset, required[loan.BankingRegion], want, deferred)
		}
	}
}

func TestInstallmentConsumerFollowupInspectFundingPreservesDonorReserve(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, delta := range []int64{-1, 0} {
			t.Run(fmt.Sprintf("deferred=%t/delta=%d", deferred, delta), func(t *testing.T) {
				h, runner, ship, _ := incomeReviewFixture(t, "charter", true, 4)
				// Keep the delivery in the source region; the unrelated south
				// vault is solely a loan-backed donor, not a destination floor.
				for index := range runner.stateCache.Catalog.Ports {
					if runner.stateCache.Catalog.Ports[index].ID == recoveryFarPort {
						runner.stateCache.Catalog.Ports[index].BankingRegion = "north"
					}
				}
				contract := runner.stateCache.Contracts[0]
				contract.RemainingQuantity = 1
				contract.RewardPerTonPence = new(int64(100_000))
				runner.stateCache.Contracts = []Contract{contract}
				if deferred {
					due := runner.stateCache.Clock.RealUTC.Add(gameDuration(10, runner.stateCache.Clock.Calendar.TimeScale))
					loan := installmentFundingLoan(due, 500_000)
					loan.BankingRegion, loan.BorrowerCompanyID = "south", runner.account.CompanyID
					runner.stateCache.Loans = []gameapi.Loan{loan}
					runner.snapshot["loans"] = encode(runner.stateCache.Loans)
				}
				input, err := runner.sourcingInput(h.ctx, ship)
				if err != nil || len(input.DeferredLoanReserves) != map[bool]int{false: 0, true: 1}[deferred] {
					t.Fatalf("inspection loan fixture: deferred=%+v err=%v", input.DeferredLoanReserves, err)
				}
				probe := Plan{Kind: "relocation", ShipID: ship.Ship.ID, Origin: recoveryFarPort, Destination: recoveryHomePort,
					Commodity: recoveryGrain, ContractID: contract.ID, TargetStoresTons: 4, Economics: Economics{GameDays: 0.25}}
				source, _ := findPort(input.Catalog, probe.Destination)
				bounded := input
				bounded.ReserveByRegion, bounded.DeferredLoanReserves = sourcingReserves(input, source), nil
				candidate := sizeRelocationCargo(bounded, probe, contract, 0)
				if !candidate.Accepted || candidate.Quantity != 1 {
					t.Fatalf("inspection lacks a positive single-ton control: %+v", candidate)
				}
				if deferred && !candidate.ExpectedArrival.Add(gameDuration(1.0/24, input.Clock.Calendar.TimeScale)).Before(input.DeferredLoanReserves[0].ReserveFrom) {
					t.Fatal("inspection fixture does not exercise otherwise deferred cargo funding")
				}
				required := cargoFundingRequired(bounded, candidate)
				need := required["north"] - bounded.ReserveByRegion["north"]
				if need <= 0 {
					t.Fatalf("inspection funding requirement: need=%d", need)
				}
				// The server floors its fee. Use the exact minimum donor
				// amount, not GrossRemittance's conservative extra penny.
				gross := (need-1)*10_000/(10_000-100) + 1
				balances := clonePence(required)
				balances["north"] -= need
				balances["south"] += gross + delta
				setInstallmentConsumerBalances(runner, balances)
				for region, balance := range balances {
					h.api.setVaultBalance(region, balance)
				}
				mission, data := runner.newVoyage(ship, probe)
				mission.Phase = "inspect"
				data.Relocation = &relocationIntent{TargetContractID: contract.ID, TargetCommodity: recoveryGrain,
					TargetDestination: recoveryFarPort, ExpenseCapPence: 20_000,
					InspectUntilGameHour: runner.stateCache.Clock.Calendar.TotalGameHours + 6,
					StoreBasis:           &relocationStoreBasis{InitialTons: ship.Stores.ProvisionsTons, ReplacementPencePerTon: 51}}
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				if _, err := runner.driveRelocation(h.ctx, ship, mission, &data); err != nil {
					t.Fatal(err)
				}
				if delta < 0 {
					if data.SourcingHandoff != nil || data.Action != nil && data.Action.Stage == "sourcing-remit" || len(h.api.mutationsFor("/v1/treasury/remit")) != 0 {
						t.Fatalf("inspection staged unaffordable loan-backed funding: action=%+v handoff=%+v", data.Action, data.SourcingHandoff)
					}
					return
				}
				var request struct {
					From  string `json:"from_region"`
					To    string `json:"to_region"`
					Gross int64  `json:"gross_amount_pence"`
				}
				if data.Action == nil || data.Action.Stage != "sourcing-remit" || json.Unmarshal(data.Action.Body, &request) != nil ||
					request.From != "south" || request.To != "north" || request.Gross != gross {
					t.Fatalf("exact reserve-safe inspection did not stage its measured funding: %+v request=%+v", data.Action, request)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("inspection admission and first-send funding disagree: %v", err)
				}
				if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				calls := h.api.mutationsFor("/v1/treasury/remit")
				if len(calls) != 1 || calls[0].Status != http.StatusOK || data.Action != nil {
					t.Fatalf("funded inspection failed to execute its remit once: calls=%+v action=%+v", calls, data.Action)
				}
				h.api.mu.Lock()
				donor := h.api.vaultBalance["south"]
				h.api.mu.Unlock()
				if donor != required["south"] {
					t.Fatalf("executed inspection remit spent donor reserves: got=%d want=%d", donor, required["south"])
				}
			})
		}
	}
}

func TestInstallmentConsumerFollowupPassengerSafeStoresPreserveFullReserve(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, delta := range []int64{-1, 0} {
			t.Run(fmt.Sprintf("deferred=%t/delta=%d", deferred, delta), func(t *testing.T) {
				h, runner, _ := passengerRunnerFixture(t)
				runner.stateCache.CompanyID = runner.account.CompanyID
				runner.stateCache.Ships[0].Stores.ProvisionsTons = 0
				ship := runner.stateCache.Ships[0]
				if deferred {
					installmentConsumerLoan(t, h, runner, "channel")
				}
				input, err := runner.committedPlanningInput(h.ctx, ship)
				if err != nil {
					t.Fatal(err)
				}
				groups := runner.passengerGroups(ship.Ship.CurrentPortID)
				control, err := quotePassage(input, "destination", groups, nil, gameapi.PassengerFareSummary{}, nil)
				if err != nil || control.Plan.StoresToBuy <= 0 || control.Plan.Economics.StoreCashPence <= 0 {
					t.Fatalf("passage lacks its required store purchase: %+v err=%v", control, err)
				}
				input.Balances = fullLoanReserves(input)
				input.Balances["channel"] += control.Plan.Economics.StoreCashPence + delta
				setInstallmentConsumerBalances(runner, input.Balances)
				quote, err := quotePassage(input, "destination", groups, nil, gameapi.PassengerFareSummary{}, nil)
				if (err == nil) != (delta == 0) || delta < 0 && !strings.Contains(err.Error(), "protected regional reserves") {
					t.Fatalf("passage store boundary delta=%d: quote=%+v err=%v", delta, quote, err)
				}
				changed, err := runner.considerPassage(h.ctx, ship, input, nil)
				if err != nil || changed != (delta == 0) {
					t.Fatalf("passage admission spent its deferred installment: changed=%t err=%v", changed, err)
				}
				if delta < 0 {
					h.missingMission(ship.Ship.ID)
					return
				}
				_, data := h.mission(ship.Ship.ID)
				if data.Passage == nil || data.Plan.StoresToBuy != control.Plan.StoresToBuy || data.Plan.Economics.StoreCashPence != control.Plan.Economics.StoreCashPence {
					t.Fatalf("exact full-reserve passage lost its safe stores: %+v", data)
				}
			})
		}
	}
}

func TestInstallmentConsumerFollowupVoyagePremiumPreparationAndFirstSend(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, boundary := range []string{"prepare", "first-send"} {
			for _, delta := range []int64{-1, 0} {
				t.Run(fmt.Sprintf("deferred=%t/%s/delta=%d", deferred, boundary, delta), func(t *testing.T) {
					h, runner, ship, hull, route := voyageInsuranceCandidate(t)
					if deferred {
						installmentConsumerLoan(t, h, runner, "north")
					}
					input, err := runner.committedPlanningInput(h.ctx, ship)
					if err != nil {
						t.Fatal(err)
					}
					required := fullLoanReserves(input)
					premium := *voyageCoverTestQuote().LloydsPremiumPence
					balances := clonePence(required)
					balances["north"] += premium
					if boundary == "prepare" {
						balances["north"] += delta
					}
					setInstallmentConsumerBalances(runner, balances)
					for region, balance := range balances {
						h.api.setVaultBalance(region, balance)
					}
					base := h.api.server.Config.Handler
					h.api.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
						if request.Method != http.MethodPost || request.URL.Path != "/v1/insurance/create" {
							base.ServeHTTP(w, request)
							return
						}
						body, err := io.ReadAll(request.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						var offer gameapi.CreateInsurancePolicyRequest
						if json.Unmarshal(body, &offer) != nil || offer.ShipID != ship.Ship.ID || offer.DestPortID != route.ToPort ||
							offer.InsuredValuePence != 1_000_000 || offer.PremiumBPS != 100 || request.Header.Get("Idempotency-Key") == "" {
							t.Errorf("invalid premium mutation: %s", body)
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						now := time.Now().UTC()
						policy := InsurancePolicy{ID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InsuredCompanyID: runner.account.CompanyID,
							ShipID: ship.Ship.ID, OriginPortID: route.FromPort, DestPortID: route.ToPort, InsuredValuePence: 1_000_000,
							HullValuePence: 900_000, PremiumBPS: 100, TotalPremiumPence: premium, Status: "open",
							CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour)}
						h.api.mu.Lock()
						h.api.vaultBalance["north"] -= premium
						h.api.record(request, body, http.StatusCreated)
						h.api.mu.Unlock()
						h.api.writeJSON(w, http.StatusCreated, struct {
							InsurancePolicy
							LloydsPremiumBPS int64 `json:"lloyds_premium_bps"`
						}{InsurancePolicy: policy, LloydsPremiumBPS: 100})
					})
					mission, data := runner.newVoyage(ship, Plan{Kind: "recovery", ShipID: ship.Ship.ID, Origin: route.FromPort, Destination: route.ToPort})
					mission.Phase = "depart"
					handled, changed, err := runner.prepareVoyageInsurance(h.ctx, ship, hull, route, mission, &data)
					if boundary == "prepare" && delta < 0 {
						if err != nil || handled || changed || data.Action != nil || len(data.VoyageInsurance) != 0 || h.api.mutationCount("/v1/insurance/create") != 0 {
							t.Fatalf("premium preparation spent deferred loan cash: handled=%t changed=%t action=%+v err=%v", handled, changed, data.Action, err)
						}
						return
					}
					if err != nil || !handled || !changed || data.Action == nil || data.Action.Stage != "voyage-insure" {
						t.Fatalf("exact full-reserve premium was not staged: handled=%t changed=%t action=%+v err=%v", handled, changed, data.Action, err)
					}
					if boundary == "first-send" {
						balances["north"] += delta
						setInstallmentConsumerBalances(runner, balances)
						h.api.setVaultBalance("north", balances["north"])
					}
					preflightErr := runner.newActionPreflight(h.ctx, mission, &data)
					if (preflightErr == nil) != (delta == 0) || delta < 0 && !strings.Contains(preflightErr.Error(), "protected operating cash") {
						t.Fatalf("premium first-send boundary delta=%d: %v", delta, preflightErr)
					}
					if _, err := runner.dispatch(h.ctx, mission, &data); err != nil {
						t.Fatal(err)
					}
					calls := h.api.mutationsFor("/v1/insurance/create")
					if delta < 0 {
						if len(calls) != 0 || len(h.pending()) != 0 || data.FeesPence != 0 {
							t.Fatalf("unfunded first-send premium reached transport: calls=%+v fees=%d", calls, data.FeesPence)
						}
						return
					}
					h.api.mu.Lock()
					remaining := h.api.vaultBalance["north"]
					h.api.mu.Unlock()
					if len(calls) != 1 || calls[0].Status != http.StatusCreated || data.Action != nil || data.FeesPence != premium ||
						len(data.VoyageInsurance) != 1 || data.VoyageInsurance[0].PolicyID == "" || remaining != required["north"] {
						t.Fatalf("premium did not execute once above full reserves: calls=%+v data=%+v remaining=%d want=%d", calls, data, remaining, required["north"])
					}
				})
			}
		}
	}
}
