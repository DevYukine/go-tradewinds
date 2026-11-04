package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestInstallmentJournalDelayedReturnPurchasesKeepDeferredCapital(t *testing.T) {
	for _, kind := range []string{"charter", "freight"} {
		t.Run(kind, func(t *testing.T) {
			h, runner, ship, plan := incomeReviewFixture(t, kind, true, 4)
			port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
			loan := installGrowthDeferredLoan(runner, port.BankingRegion, "ship_lien", false)
			input, err := runner.committedPlanningInput(h.ctx, ship)
			if err != nil || len(input.DeferredLoanReserves) != 1 {
				t.Fatalf("delayed return fixture: input=%+v err=%v", input, err)
			}
			plan.ExpectedArrival = input.DeferredLoanReserves[0].ReserveFrom.Add(time.Hour)
			plan.Quantity = 1
			mission, data := runner.newVoyage(ship, plan)
			data.ReturnJourney = true
			stage := "buy"
			path := "/v1/market/place-order"
			body := map[string]any{
				"ship_id": ship.Ship.ID, "port_id": plan.Origin, "commodity_id": plan.Commodity,
				"quantity": plan.Quantity, "price_pence": plan.BuyLimitPence, "side": "buy",
			}
			spend := growthCashTotal(iocCash(plan.Quantity, plan.BuyLimitPence, input.Markets[plan.Origin].CustomsBPS),
				plan.Economics.BerthDuesPence, plan.Economics.UpkeepPence, plan.Economics.CollateralPence,
				plan.Economics.TollPence, plan.Economics.VoyagePremiumPence)
			if kind == "freight" {
				stage, path = "accept-freight", "/v1/contract/accept-freight"
				body = map[string]any{"ship_id": ship.Ship.ID, "contract_id": plan.ContractID}
				contract, exists := runner.contract(plan.ContractID)
				if !exists || contract.CollateralPence <= 0 {
					t.Fatalf("freight fixture lacks its refundable collateral: %+v", contract)
				}
				spend = contract.CollateralPence
			}
			if ordinaryIncomePending(&data) {
				t.Fatal("return purchase unexpectedly used the ordinary-income first-send gate")
			}
			if err := runner.stage(h.ctx, mission, &data, ship, stage, path, body); err != nil {
				t.Fatal(err)
			}
			balances := fullLoanReserves(input)
			balances[port.BankingRegion] += spend
			setInstallmentConsumerBalances(runner, balances)
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("exact full-reserve return purchase was vetoed: %v", err)
			}
			balances[port.BankingRegion]--
			setInstallmentConsumerBalances(runner, balances)
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected regional") {
				t.Fatalf("delayed return purchase spent deferred installment capital: %v", err)
			}
			balances[port.BankingRegion] -= loanReserve(loan) - 1
			setInstallmentConsumerBalances(runner, balances)
			runner.stateCache.Loans = nil
			runner.snapshot["loans"] = json.RawMessage("[]")
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("loan-free return purchase changed its old funding boundary: %v", err)
			}
			if h.api.mutationCount(path) != 0 || len(h.pending()) != 0 {
				t.Fatal("preflight-only purchase checks sent an action")
			}
		})
	}
}

func TestInstallmentJournalOrdinaryFreightStagesDeferredCollateral(t *testing.T) {
	for _, timing := range []string{"in time", "late", "unknown"} {
		t.Run(timing, func(t *testing.T) {
			h, runner, ship, plan := incomeReviewFixture(t, "freight", true, 4)
			port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
			loan := installGrowthDeferredLoan(runner, port.BankingRegion, "ship_lien", false)
			input, err := runner.committedPlanningInput(h.ctx, ship)
			if err != nil || len(input.DeferredLoanReserves) != 1 {
				t.Fatalf("ordinary freight fixture lacks its deferred installment: input=%+v err=%v", input, err)
			}
			plan = priceIncomePlan(input, plan, 0)
			if !plan.Accepted || plan.ExpectedArrival.IsZero() ||
				plan.ExpectedArrival.Add(gameDuration(1.0/24, input.Clock.Calendar.TimeScale)).After(input.DeferredLoanReserves[0].ReserveFrom) {
				t.Fatalf("ordinary freight does not return before its reserve window: %+v", plan)
			}
			balances := cargoFundingRequired(input, plan)
			if balances[port.BankingRegion]-plan.Economics.CollateralPence >= input.ReserveByRegion[port.BankingRegion]+loanReserve(loan) {
				t.Fatal("ordinary freight fixture does not need its deferred installment capital")
			}
			if timing == "late" {
				due := plan.ExpectedArrival.Add(gameDuration(3, input.Clock.Calendar.TimeScale) +
					serverClockStaleness + gameDuration(1.0/48, input.Clock.Calendar.TimeScale))
				loan.NextPaymentDueAt = &due
				for index := range runner.stateCache.Loans {
					if runner.stateCache.Loans[index].ID == loan.ID {
						runner.stateCache.Loans[index] = loan
					}
				}
				runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			} else if timing == "unknown" {
				delete(runner.stateCache.Routes, routeKey(plan.Origin, plan.Destination, ship.Ship.ShipClass))
			}
			setInstallmentConsumerBalances(runner, balances)
			mission, data := runner.newVoyage(ship, plan)
			mission.Phase = "freight"
			if !ordinaryIncomePending(&data) {
				t.Fatal("freight fixture bypassed ordinary-income admission")
			}
			if err := runner.saveMission(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			allowed := timing == "in time"
			quote, err := runner.incomeProcurementQuote(h.ctx, ship, &data)
			if err != nil || quote.plan.Accepted != allowed || allowed && len(quote.transfers) != 0 {
				t.Fatalf("ordinary collateral freight admission allowed=%t quote=%+v err=%v", allowed, quote, err)
			}
			input, err = runner.committedPlanningInput(h.ctx, ship)
			if err != nil || len(input.DeferredLoanReserves) != 1 {
				t.Fatalf("freight timing lost the deferred reserve: input=%+v err=%v", input, err)
			}
			wantReserve := input.ReserveByRegion[port.BankingRegion]
			if !allowed {
				wantReserve += loanReserve(loan)
			}
			if timing == "unknown" && !quote.plan.ExpectedArrival.IsZero() {
				t.Fatal("missing route retained an authoritative freight arrival")
			}
			if got := cargoLoanReserves(input, quote.plan)[port.BankingRegion]; got != wantReserve {
				t.Fatalf("ordinary freight timing restored the wrong reserve: got=%d want=%d", got, wantReserve)
			}
			if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
				t.Fatalf("ordinary freight made no safe staging transition: allowed=%t changed=%t err=%v", allowed, changed, err)
			}
			mission, data = h.mission(ship.Ship.ID)
			if allowed {
				if data.Action == nil || data.Action.Stage != "accept-freight" {
					t.Fatalf("in-time ordinary freight did not stage its collateral acceptance: %+v", data)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("admitted ordinary freight entered a collateral stage/veto loop: %v", err)
				}
			} else if data.Action != nil || data.Plan.Kind != "aborted" || mission.Phase != "arrival" {
				t.Fatalf("late or unknown ordinary freight staged instead of unwinding: phase=%s data=%+v", mission.Phase, data)
			}
			if h.api.mutationCount("/v1/contract/accept-freight") != 0 || len(h.pending()) != 0 {
				t.Fatal("staging and first-send checks sent an unrequested freight action")
			}
		})
	}
}

func TestInstallmentJournalReturnStagingMatchesArrivalReserveBoundary(t *testing.T) {
	for _, kind := range []string{"charter", "freight"} {
		for _, timing := range []string{"unknown", "before", "exact", "after"} {
			t.Run(kind+"/"+timing, func(t *testing.T) {
				h, runner, ship, plan := incomeReviewFixture(t, kind, true, 4)
				port, _ := findPort(runner.stateCache.Catalog, ship.Ship.CurrentPortID)
				loan := installGrowthDeferredLoan(runner, port.BankingRegion, "ship_lien", false)
				input, err := runner.committedPlanningInput(h.ctx, ship)
				if err != nil || len(input.DeferredLoanReserves) != 1 {
					t.Fatalf("return staging fixture lacks its deferred installment: input=%+v err=%v", input, err)
				}
				boundary := input.DeferredLoanReserves[0].ReserveFrom.Add(-gameDuration(1.0/24, input.Clock.Calendar.TimeScale))
				plan.Quantity = 1
				switch timing {
				case "unknown":
					plan.ExpectedArrival = time.Time{}
				case "before":
					plan.ExpectedArrival = boundary.Add(-time.Nanosecond)
				case "exact":
					plan.ExpectedArrival = boundary
				case "after":
					plan.ExpectedArrival = boundary.Add(time.Nanosecond)
				}
				allowed := timing == "before" || timing == "exact"
				mission, data := runner.newVoyage(ship, plan)
				data.ReturnJourney = true
				mission.Phase = "buy"
				stage, path := "buy", "/v1/market/place-order"
				body := orderBody(ship, plan.Commodity, "bid", plan.Quantity, plan.BuyLimitPence, false)
				spend := growthCashTotal(iocCash(plan.Quantity, plan.BuyLimitPence, input.Markets[plan.Origin].CustomsBPS),
					plan.Economics.BerthDuesPence, plan.Economics.UpkeepPence, plan.Economics.CollateralPence,
					plan.Economics.TollPence, plan.Economics.VoyagePremiumPence)
				if kind == "freight" {
					mission.Phase = "freight"
					stage, path = "accept-freight", "/v1/contract/accept-freight"
					body = map[string]any{"ship_id": ship.Ship.ID, "contract_id": plan.ContractID}
					contract, exists := runner.contract(plan.ContractID)
					if !exists || contract.CollateralPence <= 0 {
						t.Fatalf("return freight fixture lacks collateral: %+v", contract)
					}
					spend = contract.CollateralPence
				}
				if ordinaryIncomePending(&data) {
					t.Fatal("return staging fixture used ordinary-income admission")
				}
				if err := runner.saveMission(h.ctx, mission, &data); err != nil {
					t.Fatal(err)
				}
				input, err = runner.committedPlanningInput(h.ctx, ship)
				if err != nil {
					t.Fatal(err)
				}
				wantReserve := input.ReserveByRegion[port.BankingRegion]
				if !allowed {
					wantReserve += loanReserve(loan)
				}
				if got := cargoLoanReserves(input, data.Plan)[port.BankingRegion]; got != wantReserve {
					t.Fatalf("arrival boundary restored the wrong loan reserve: got=%d want=%d", got, wantReserve)
				}
				balances := clonePence(input.ReserveByRegion)
				balances[port.BankingRegion] += spend
				setInstallmentConsumerBalances(runner, balances)
				candidate := data
				candidate.Action = &durableAction{Stage: stage, Path: path, Body: encode(body), Before: ship}
				if err := runner.newActionPreflight(h.ctx, mission, &candidate); (err == nil) != allowed {
					t.Fatalf("return first-send boundary allowed=%t err=%v", allowed, err)
				}
				if changed, err := runner.driveMission(h.ctx, ship, mission, &data); err != nil || !changed {
					t.Fatalf("return staging boundary made no safe transition: allowed=%t changed=%t err=%v", allowed, changed, err)
				}
				mission, data = h.mission(ship.Ship.ID)
				if allowed {
					if data.Action == nil || data.Action.Stage != stage {
						t.Fatalf("in-time return procurement did not stage: %+v", data)
					}
					if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
						t.Fatalf("staged in-time return procurement entered a veto loop: %v", err)
					}
				} else {
					if data.Action != nil {
						t.Fatalf("late or unknown return procurement staged an unfunded action: %+v", data.Action)
					}
					if kind == "freight" {
						if slices.Contains(data.Plan.FreightIDs, plan.ContractID) {
							t.Fatal("unfunded return freight retained its collateral candidate")
						}
					} else if data.Plan.Kind != "aborted" || mission.Phase != "arrival" {
						t.Fatalf("unfunded return buy did not unwind for reselection: phase=%s plan=%+v", mission.Phase, data.Plan)
					}
				}
				if h.api.mutationCount(path) != 0 || len(h.pending()) != 0 {
					t.Fatal("arrival-boundary staging checks sent an action")
				}
			})
		}
	}
}

func TestExpansionPartnerRemittanceFinishesUnderWholePayoffHold(t *testing.T) {
	for _, mortgage := range []bool{false, true} {
		name := "secured reserve"
		if mortgage {
			name = "mortgage reserve"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, _ := overseasPairFixture(t)
			h.api.setFactorPorts(recoveryHomePort, recoveryFarPort, "boston")
			overseasPairSyncFactors(h, runner)
			at := time.Now().UTC().Add(-time.Hour)
			agents := []expansionAgent{
				{PortID: "boston", Partner: "new_york", Region: "america", CreatedAt: at, HiredAt: at},
				{PortID: "new_york", Partner: "boston", Region: "america", CreatedAt: at},
			}
			overseasPairSave(t, h, runner, agents)
			installDebtServiceHoldLoan(runner, "north")
			installGrowthDeferredLoan(runner, "north", "ship_lien", mortgage)
			input, err := runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil || len(input.DeferredLoanReserves) == 0 {
				t.Fatalf("partner funding fixture lacks a deferred secured reserve: input=%+v err=%v", input, err)
			}
			required, err := runner.expansionRequired(h.ctx, input, "america", expansionAgentHireFeePence)
			if err != nil {
				t.Fatal(err)
			}
			rate := Rate{FromRegion: "north", ToRegion: "america", FeeBPS: 100}
			runner.stateCache.Rates = []Rate{rate}
			h.api.setRates(rate)
			gross, err := GrossRemittance(required["america"], rate.FeeBPS)
			if err != nil {
				t.Fatal(err)
			}
			balances := clonePence(required)
			balances["america"] = 0
			balances["north"] += gross
			setInstallmentConsumerBalances(runner, balances)
			hold := runner.debtServiceHoldPence()
			if hold <= 0 || growthFreeCash(balances, runner.growthLoanReserves(input.ReserveByRegion)) >= hold {
				t.Fatal("partner remittance fixture is not under the whole unsecured payoff hold")
			}
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("partner funding did not stage: changed=%t err=%v", changed, err)
			}
			mission, data := h.mission("__treasury")
			if !data.GrowthFunding || !data.GrowthPartnerHire || data.Action == nil || data.Action.Stage != "remit" {
				t.Fatalf("partner exemption was not persisted on its growth remittance: %+v", data)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("admitted partner remittance entered a stage/veto loop: %v", err)
			}
			balances["north"]--
			setInstallmentConsumerBalances(runner, balances)
			if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "protected donor reserves") {
				t.Fatalf("partner exemption spent the secured or mortgage full reserve: %v", err)
			}
			balances["north"]++
			setInstallmentConsumerBalances(runner, balances)
			factors := slices.Clone(runner.stateCache.Factors)
			for _, invalid := range []string{"missing mate", "unhired mate", "absent factor", "already hired partner", "already current partner", "expired pair", "other region"} {
				t.Run(invalid, func(t *testing.T) {
					current := slices.Clone(agents)
					runner.stateCache.Factors = slices.Clone(factors)
					switch invalid {
					case "missing mate":
						current = current[1:]
					case "unhired mate":
						current[0].HiredAt = time.Time{}
					case "absent factor":
						runner.stateCache.Factors = slices.DeleteFunc(runner.stateCache.Factors, func(port string) bool { return port == "boston" })
					case "already hired partner":
						current[1].HiredAt = at
					case "already current partner":
						runner.stateCache.Factors = append(runner.stateCache.Factors, "new_york")
					case "expired pair":
						current[0].HiredAt = time.Now().UTC().Add(-3 * time.Hour)
					case "other region":
						current[1].Region = "south"
					}
					runner.snapshot["_expansion_agents"] = encode(current)
					invalidInput, err := runner.committedPlanningInput(h.ctx, ShipState{})
					if err != nil {
						t.Fatal(err)
					}
					invalidBalances := clonePence(balances)
					// A newly current factor also raises ordinary upkeep.
					// Fund that real bill so this checks the payoff hold.
					protected := runner.growthLoanReserves(invalidInput.ReserveByRegion)
					invalidBalances["north"] += max(0, protected["north"]-required["north"])
					setInstallmentConsumerBalances(runner, invalidBalances)
					if err := runner.newActionPreflight(h.ctx, mission, &data); err == nil || !strings.Contains(err.Error(), "unsecured loan payoff") {
						t.Fatalf("arbitrary partner flag bypassed the whole-payoff hold: %v", err)
					}
				})
			}
			runner.snapshot["_expansion_agents"] = encode(agents)
			runner.stateCache.Factors = factors
			setInstallmentConsumerBalances(runner, balances)
			ordinary := data
			ordinary.GrowthPartnerHire = false
			var remit struct {
				From  string `json:"from_region"`
				To    string `json:"to_region"`
				Gross int64  `json:"gross_amount_pence"`
			}
			if err := json.Unmarshal(data.Action.Body, &remit); err != nil {
				t.Fatal(err)
			}
			input, err = runner.committedPlanningInput(h.ctx, ShipState{})
			if err != nil {
				t.Fatal(err)
			}
			projected := clonePence(input.Balances)
			projected[remit.From] -= remit.Gross
			projected[remit.To] += remit.Gross - remit.Gross/100
			boundary := clonePence(balances)
			boundary[remit.From] += hold - growthFreeCash(projected, runner.growthLoanReserves(input.ReserveByRegion))
			setInstallmentConsumerBalances(runner, boundary)
			if err := runner.newActionPreflight(h.ctx, mission, &ordinary); err != nil {
				t.Fatalf("ordinary growth remittance rejected the exact whole-payoff boundary: %v", err)
			}
			boundary[remit.From]--
			setInstallmentConsumerBalances(runner, boundary)
			if err := runner.newActionPreflight(h.ctx, mission, &ordinary); err == nil || !strings.Contains(err.Error(), "unsecured loan payoff") {
				t.Fatalf("ordinary growth remittance spent one penny of the whole payoff: %v", err)
			}
			setInstallmentConsumerBalances(runner, balances)
			for region, balance := range balances {
				h.api.setVaultBalance(region, balance)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("partner funding failed its real first send: changed=%t err=%v", changed, err)
			}
			if h.api.mutationCount("/v1/treasury/remit") != 1 {
				t.Fatal("partner funding did not send exactly one remittance")
			}
			if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
				t.Fatal(err)
			}
			overseasPairSyncFactors(h, runner)
			if changed, err := runner.manageExpansionAgents(h.ctx); err != nil || !changed {
				t.Fatalf("funded partner did not resume its hire: changed=%t err=%v", changed, err)
			}
			mission, data = h.mission("__treasury")
			if data.Action == nil || data.Action.Stage != "hire-factor" || !stringsContainPort(data.Action.Body, "new_york") {
				t.Fatalf("funding restaged instead of advancing to the paired hire: %+v", data.Action)
			}
			if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
				t.Fatalf("funded partner hire lost its existing exemption: %v", err)
			}
		})
	}
}
