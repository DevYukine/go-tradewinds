package engine

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func installmentFundingLoan(due time.Time, next int64) gameapi.Loan {
	loan := snapshotTestLoan()
	loan.ID, loan.BankingRegion = "deferred-installment", "america"
	loan.CollateralKind, loan.CollateralStatus, loan.CollateralShipID = "unsecured", "none", nil
	loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = 10*next, 10*next, 10*next
	loan.InstallmentPence, loan.NextPaymentPence, loan.NextPaymentDueAt = &next, &next, &due
	return loan
}

func installmentIncomeFixture(t *testing.T, now time.Time) (PlanningInput, AccountState, map[string]json.RawMessage) {
	t.Helper()
	template := incomeFundingInput(t, "charter")
	template.Clock.RealUTC = now
	template.Clock.Calendar.TimeScale = 60
	for key := range template.Metadata {
		template.Metadata[key] = sourcingFreshMeta(now)
	}
	state := AccountState{
		CompanyID: recoveryCompanyID, Catalog: template.Catalog, Clock: template.Clock,
		Ships: []ShipState{template.Ship}, Markets: template.Markets, Ports: template.Ports,
		Routes: template.Routes, Contracts: template.Contracts, HomePortID: "le_havre",
		Loans: []gameapi.Loan{installmentFundingLoan(now.Add(4*time.Hour), 500_000)},
		Rates: []Rate{{FromRegion: "america", ToRegion: "channel", FeeBPS: 335}},
	}
	floors := obligationReserves(state, nil, ProtectedReservePence(state))
	state.Vaults = []Vault{
		{AccountType: "vault", BankingRegion: "channel", BalancePence: floors["channel"]},
		{AccountType: "vault", BankingRegion: "america", BalancePence: 750_000},
	}
	snapshot := map[string]json.RawMessage{
		"zones": encode(template.Zones), "loans": encode(state.Loans),
		"_revenue_calibration": encode(template.RevenueCalibration),
	}
	input := PlanningInputFor(now, state.Ships[0], state, snapshot, PlanningSources{
		CompanyID: state.CompanyID, Metadata: template.Metadata, Generation: template.Generation,
	})
	seedZeroHazardQuotes(&input)
	state.HazardQuotes = input.HazardQuotes
	return input, state, snapshot
}

func TestIncomeFundingInstallmentCashOnlyForCapitalBackBeforeReserveWindow(t *testing.T) {
	t.Parallel()
	t.Run("arrival boundaries", func(t *testing.T) {
		input, _, _ := installmentIncomeFixture(t, time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC))
		plan := priceIncomePlan(input, incomeFundingBest(t, input), 0)
		if len(input.DeferredLoanReserves) != 1 {
			t.Fatalf("known installment did not populate its planning deferral: %+v", input.DeferredLoanReserves)
		}
		deferred := input.DeferredLoanReserves[0]
		hour := gameDuration(1.0/24, input.Clock.Calendar.TimeScale)
		for _, test := range []struct {
			name    string
			arrival time.Time
			allowed bool
		}{
			{"one game-hour slack", deferred.ReserveFrom.Add(-hour), true},
			{"more than one game-hour slack", deferred.ReserveFrom.Add(-hour - time.Second), true},
			{"less than one game-hour slack", deferred.ReserveFrom.Add(-hour + time.Nanosecond), false},
			{"reserve-window boundary", deferred.ReserveFrom, false},
			{"inside reserve window", deferred.ReserveFrom.Add(time.Second), false},
			{"unknown arrival", time.Time{}, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				candidate := plan
				candidate.ExpectedArrival = test.arrival
				quote := fundVoyage(input, candidate)
				if quote.plan.Accepted != test.allowed {
					t.Fatalf("arrival %s reserve from %s: allowed=%t quote=%+v", test.arrival, deferred.ReserveFrom, test.allowed, quote)
				}
				required := cargoFundingRequired(input, candidate)
				if test.allowed {
					if len(quote.transfers) != 1 || quote.transfers[0].Rate.FromRegion != "america" || quote.transfers[0].Rate.ToRegion != "channel" || required["america"] != input.ReserveByRegion["america"] {
						t.Fatalf("returning capital did not use the deferred installment donor: %+v required=%v", quote, required)
					}
				} else if required["america"] != input.ReserveByRegion["america"]+deferred.FullReserve {
					t.Fatalf("late or unknown capital lost the full legacy installment reserve: %v", required)
				}
			})
		}
	})
	t.Run("first-send reprices arrival", func(t *testing.T) {
		h := newRecoveryHarness(t)
		input, state, snapshot := installmentIncomeFixture(t, time.Now().UTC())
		runner := h.runner(primaryTestKey)
		runner.stateCache, runner.snapshot = state, snapshot
		runner.meta, runner.generation = input.Metadata, input.Generation
		plan := priceIncomePlan(input, incomeFundingBest(t, input), 0)
		quote := fundVoyage(input, plan)
		if len(quote.transfers) != 1 || quote.transfers[0].Rate.FromRegion != "america" {
			t.Fatalf("fixture did not quote actual installment-backed income funding: %+v", quote)
		}
		transfer := quote.transfers[0]
		action := &durableAction{Stage: "remit", Body: encode(map[string]any{
			"from_region": transfer.Rate.FromRegion, "to_region": transfer.Rate.ToRegion,
			"gross_amount_pence": transfer.Gross,
		})}
		lead := gameDuration(3, state.Clock.Calendar.TimeScale)
		hour := gameDuration(1.0/24, state.Clock.Calendar.TimeScale)
		for _, test := range []struct {
			name    string
			slack   time.Duration
			unknown bool
			allowed bool
		}{
			{"over one game-hour slack", hour + 10*time.Second, false, true},
			{"less than one game-hour slack", hour - time.Second, false, false},
			{"reserve-window boundary", 0, false, false},
			{"inside reserve window", -hour, false, false},
			{"no authoritative arrival", hour + 10*time.Second, true, false},
		} {
			t.Run(test.name, func(t *testing.T) {
				due := plan.ExpectedArrival.Add(lead + serverClockStaleness + test.slack)
				runner.stateCache.Loans = []gameapi.Loan{installmentFundingLoan(due, 500_000)}
				runner.snapshot["loans"] = encode(runner.stateCache.Loans)
				key := routeKey(plan.Origin, plan.Destination, state.Ships[0].Ship.ShipClass)
				route := runner.stateCache.Routes[key]
				if test.unknown {
					delete(runner.stateCache.Routes, key)
					defer func() { runner.stateCache.Routes[key] = route }()
				}
				data := missionData{Plan: plan, CostsKnown: true}
				err := runner.incomeActionReady(h.ctx, state.Ships[0], &data, action)
				if (err == nil) != test.allowed {
					t.Fatalf("first-send arrival reprice allowed=%t error=%v", test.allowed, err)
				}
				if !test.allowed && !test.unknown && !strings.Contains(err.Error(), "protected donor cash") {
					t.Fatalf("first send rejected before checking the loan-backed donor: %v", err)
				}
				// Check procurement too, projecting the observed floor-rounded
				// receipt without sending a game mutation or inventing a journal.
				originalVaults := runner.stateCache.Vaults
				runner.stateCache.Vaults = append([]Vault(nil), originalVaults...)
				defer func() { runner.stateCache.Vaults = originalVaults }()
				fee := transfer.Gross * transfer.Rate.FeeBPS / 10_000
				for index := range runner.stateCache.Vaults {
					vault := &runner.stateCache.Vaults[index]
					if vault.BankingRegion == transfer.Rate.FromRegion {
						vault.BalancePence -= transfer.Gross
					} else if vault.BankingRegion == transfer.Rate.ToRegion {
						vault.BalancePence += transfer.Gross - fee
					}
				}
				data.FeesPence = fee
				buy := &durableAction{Stage: "buy", Body: encode(orderBody(state.Ships[0], plan.Commodity, "bid", plan.Quantity, plan.BuyLimitPence, false))}
				if err := runner.incomeActionReady(h.ctx, state.Ships[0], &data, buy); (err == nil) != test.allowed {
					t.Fatalf("first-send procurement arrival reprice allowed=%t error=%v", test.allowed, err)
				}
			})
		}
		if len(h.api.mutationsFor("/v1/treasury/remit")) != 0 || len(h.pending()) != 0 {
			t.Fatal("first-send quotation mutated the synthetic game or journal")
		}
	})
}

func TestIdleMaintenanceFundsStoresFromDeferredInstallment(t *testing.T) {
	t.Parallel()
	for _, inside := range []bool{false, true} {
		name := "outside reserve window"
		if inside {
			name = "inside reserve window"
		}
		t.Run(name, func(t *testing.T) {
			h, runner, proxy, _ := idleMaintenanceFixture(t, false)
			protected, donor := idleMaintenanceFundDonor(t, h, runner, proxy)
			now := time.Now().UTC()
			runner.stateCache.Clock.RealUTC = now
			runner.stateCache.Clock.Calendar.TimeScale = 60
			due := now.Add(4 * time.Hour)
			if inside {
				due = now.Add(gameDuration(3, 60) + 2*time.Minute)
			}
			loan := installmentFundingLoan(due, 50_000)
			loan.BankingRegion = "south"
			runner.stateCache.Loans = []gameapi.Loan{loan}
			runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			if donor >= protected["south"]+*loan.NextPaymentPence {
				t.Fatalf("fixture can fund stores without borrowing an installment: donor=%d floor=%d", donor, protected["south"])
			}
			ship := harnessShip(t, runner, recoveryShipBlastoise)
			if changed, err := runner.maintain(h.ctx, ship, nil, nil); err != nil || !changed {
				t.Fatalf("idle replenishment was not persisted: changed=%t error=%v", changed, err)
			}
			mission, data := h.mission(recoveryShipBlastoise)
			changed, err := runner.maintain(h.ctx, ship, mission, &data)
			if err != nil || changed == inside {
				t.Fatalf("inside=%t maintenance changed=%t error=%v holds=%+v", inside, changed, err, runner.holding)
			}
			_, retained := h.mission(recoveryShipBlastoise)
			if inside {
				heldForFunding := false
				for _, hold := range runner.holding {
					heldForFunding = heldForFunding || hold.ShipID == ship.Ship.ID && strings.Contains(hold.Reason, "store funding")
				}
				if retained.Action != nil || !heldForFunding {
					t.Fatalf("stores spent the next installment or failed for another reason: %+v holds=%+v", retained, runner.holding)
				}
			} else {
				var request struct {
					From  string `json:"from_region"`
					To    string `json:"to_region"`
					Gross int64  `json:"gross_amount_pence"`
				}
				if retained.Action == nil || retained.Action.Path != "/v1/treasury/remit" || json.Unmarshal(retained.Action.Body, &request) != nil || request.From != "south" || request.To != "north" || request.Gross <= 0 || donor-request.Gross < protected["south"] {
					t.Fatalf("outside-window safety stores did not stage reserve-safe funding: %+v request=%+v", retained, request)
				}
			}
			if h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
				t.Fatal("staging maintenance unexpectedly sent a game mutation")
			}
		})
	}
}

func installmentTreasuryFixture(t *testing.T, inside bool) (*recoveryHarness, *Runner, int64) {
	t.Helper()
	h, runner := incomeFundingRunner(t)
	now := time.Now().UTC()
	runner.stateCache.Clock.RealUTC = now
	runner.stateCache.Clock.Calendar.TimeScale = 60
	due := now.Add(4 * time.Hour)
	if inside {
		due = now.Add(gameDuration(3, 60) + 2*time.Minute)
	}
	loan := installmentFundingLoan(due, 500_000)
	runner.stateCache.Loans = []gameapi.Loan{loan}
	runner.snapshot["loans"] = encode(runner.stateCache.Loans)
	runner.snapshot["settings"] = encode(map[string]any{"auto_remit_for_bills": true, "auto_remit_min_reserve_pence": 10_000_000})
	runner.stateCache.Rates = []Rate{{FromRegion: "south", ToRegion: "america", FeeBPS: 335}}
	required := obligationReserves(runner.stateCache, runner.snapshot, runner.protectedReservePence())
	for index := range runner.stateCache.Vaults {
		vault := &runner.stateCache.Vaults[index]
		if vault.BankingRegion == "north" {
			vault.BalancePence = required["north"]
		} else if vault.BankingRegion == "south" {
			vault.BalancePence = 2_000_000
		}
	}
	runner.stateCache.Vaults = append(runner.stateCache.Vaults, Vault{AccountType: "vault", BankingRegion: "america"})
	return h, runner, *loan.NextPaymentPence
}

func TestTreasuryNoLoanRegionSweepBeforeReserveWindow(t *testing.T) {
	t.Parallel()
	h, runner, _ := installmentTreasuryFixture(t, false)
	if changed, err := runner.manageTreasury(h.ctx); err != nil || changed {
		t.Fatalf("treasury swept a not-yet-due installment: changed=%t error=%v holds=%+v", changed, err, runner.holding)
	}
	h.missingMission("__treasury")
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil || len(input.DeferredLoanReserves) != 1 || input.ReserveByRegion["america"] != 0 {
		t.Fatalf("pre-window treasury lost the deferral: input=%+v error=%v", input, err)
	}
}

func TestTreasurySweepsOneInstallmentInsideWindow(t *testing.T) {
	t.Parallel()
	h, runner, next := installmentTreasuryFixture(t, true)
	if changed, err := runner.manageTreasury(h.ctx); err != nil || !changed {
		t.Fatalf("due installment was not funded: changed=%t error=%v holds=%+v", changed, err, runner.holding)
	}
	_, data := h.mission("__treasury")
	var request struct {
		From  string `json:"from_region"`
		To    string `json:"to_region"`
		Gross int64  `json:"gross_amount_pence"`
	}
	want, err := GrossRemittance(next, 335)
	if err != nil || data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.From != "south" || request.To != "america" || request.Gross != want {
		t.Fatalf("treasury swept more or less than one installment: next=%d want=%d request=%+v data=%+v error=%v", next, want, request, data, err)
	}
	input, err := runner.committedPlanningInput(h.ctx, ShipState{})
	if err != nil || input.ReserveByRegion["america"] != next || len(input.DeferredLoanReserves) != 0 {
		t.Fatalf("inside-window planning did not protect exactly the next payment: input=%+v error=%v", input, err)
	}
}

func TestInstallmentFreeze20261104(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 4, 7, 0, 0, 0, time.UTC)
	template, state, snapshot := installmentIncomeFixture(t, now)
	due := time.Date(2026, 11, 4, 10, 58, 56, 0, time.UTC)
	state.Loans = []gameapi.Loan{installmentFundingLoan(due, 981_744)}
	input := PlanningInputFor(now, state.Ships[0], state, snapshot, PlanningSources{
		CompanyID: state.CompanyID, Metadata: template.Metadata, Generation: template.Generation,
	})
	// This is a cash replay, not an invented reconstruction of the eight live
	// hulls. Preserve the captured fleet bill and regional berth/bills floors.
	input.FleetDailyUpkeepPence = 67_081
	input.ReserveByRegion = map[string]int64{"america": 103_000, "baltic": 103_000, "north_sea": 103_012, "channel": 322_303}
	input.Balances = map[string]int64{"america": 1_824_827, "baltic": 102_750, "north_sea": 103_003, "channel": 0}
	input.Rates = []Rate{
		{FromRegion: "america", ToRegion: "baltic", FeeBPS: 335},
		{FromRegion: "america", ToRegion: "north_sea", FeeBPS: 335},
		{FromRegion: "america", ToRegion: "channel", FeeBPS: 335},
		{FromRegion: "channel", ToRegion: "america", FeeBPS: 335},
	}
	if state.HomePortID != "le_havre" || input.FleetDailyUpkeepPence != 67_081 || len(input.DeferredLoanReserves) != 1 || !input.DeferredLoanReserves[0].ReserveFrom.Equal(due.Add(-gameDuration(3, input.Clock.Calendar.TimeScale)-serverClockStaleness)) || input.DeferredLoanReserves[0].FullReserve != 1_963_488 {
		t.Fatalf("replay lost the captured home, upkeep or installment window: %+v", input)
	}
	required := clonePence(input.ReserveByRegion)
	var allInCosts int64
	for _, topup := range []struct {
		name   string
		port   string
		region string
		ask    int64
		cost   int64
	}{
		{"Charizard", "amsterdam", "north_sea", 24_230, 24_836},
		{"Charmander", "london", "channel", 25_217, 25_848},
		{"Ivysaur", "london", "channel", 25_217, 25_848},
		{"Lapras", "portsmouth", "channel", 23_839, 24_435},
		{"Charmeleon", "calais", "channel", 23_074, 23_651},
	} {
		// Equivalent 250-bps synthetic asks reproduce the observed tax-inclusive
		// one-ton costs; the unavailable live quay rows are not claimed here.
		if _, exists := findPort(input.Catalog, topup.port); !exists {
			input.Catalog.Ports = append(input.Catalog.Ports, Port{ID: topup.port, BankingRegion: topup.region, SeaZoneID: "english_channel", Phase: 1})
		}
		for index := range input.Catalog.Ports {
			if input.Catalog.Ports[index].ID == topup.port {
				input.Catalog.Ports[index].BankingRegion = topup.region
			}
		}
		input.Markets[topup.port] = Market{PortID: topup.port, Visibility: "quay", CustomsBPS: 250,
			Books: []CommodityBook{{CommodityID: "provisions", Depth: &struct {
				Bids []Level `json:"bids"`
				Asks []Level `json:"asks"`
			}{Asks: []Level{{OrderID: topup.name + "-stores", PricePence: topup.ask, RemainingQuantity: 100}}}}},
		}
		input.Metadata["books/"+topup.port] = sourcingFreshMeta(now)
		local := input
		local.Ship.Ship.ID, local.Ship.Ship.Name, local.Ship.Ship.CurrentPortID = topup.name, topup.name, topup.port
		local.Ship.Stores.ProvisionsTons = 1.05
		quote, err := quoteProvisions(local, 1.1)
		if err != nil || quote.quantity != 1 || quote.region != topup.region || topup.ask+ceilBPS(topup.ask, 250) != topup.cost || quote.cash != topup.cost+1 {
			t.Fatalf("%s did not quote its captured one-ton top-up: quote=%+v error=%v", topup.name, quote, err)
		}
		transfer, err := provisionFunding(local, quote)
		if err != nil || transfer.Rate.FromRegion != "america" {
			t.Fatalf("%s safety stores remained frozen despite deferred installment cash: transfer=%+v error=%v", topup.name, transfer, err)
		}
		required[quote.region] += quote.cash
		allInCosts += topup.cost
	}
	projected, _, transfers, funded := measuredFunding(input.Balances, required, input.Rates)
	if !funded || allInCosts != 124_618 || len(transfers) == 0 {
		t.Fatalf("all five named top-ups were not jointly fundable: funded=%t cost=%d transfers=%+v", funded, allInCosts, transfers)
	}
	for _, transfer := range transfers {
		if transfer.Rate.FromRegion != "america" || transfer.Rate.ToRegion == "america" {
			t.Fatalf("pre-window reserve repair swept cash into the installment region: %+v", transfers)
		}
	}
	// Pay every quoted top-up before admitting the next shipment, so the hop
	// cannot double-spend cash that was only individually fundable above.
	for region, amount := range required {
		projected[region] -= amount - input.ReserveByRegion[region]
	}
	input.Balances = projected
	input.Ship = state.Ships[0]
	for _, distance := range []int64{182, 1478} {
		input.Routes[routeKey("le_havre", "portsmouth", "hoy")] = sourcingRoute("le_havre", "portsmouth", distance)
		input.Routes[routeKey("portsmouth", "le_havre", "hoy")] = sourcingRoute("portsmouth", "le_havre", distance)
		seedZeroHazardQuotes(&input)
		pinned := Plan{Kind: "charter", ShipID: input.Ship.Ship.ID, Origin: "le_havre", Destination: "portsmouth", Commodity: "sawn_planks", ContractID: input.Contracts[0].ID, Quantity: 24}
		priced := priceIncomePlan(input, pinned, 0)
		if !priced.Accepted {
			t.Fatalf("replay route failed actual pricing rather than the installment cash gate: %+v", priced)
		}
		quote := fundVoyage(input, priced)
		if distance == 182 {
			if priced.Economics.GameDays < 1.74 || priced.Economics.GameDays > 1.76 || !quote.plan.Accepted || len(quote.transfers) == 0 || quote.transfers[0].Rate.FromRegion != "america" {
				t.Fatalf("approximately 1.75-game-day channel income remained frozen after stores: %+v", quote)
			}
		} else {
			if priced.Economics.GameDays < 12.99 || priced.Economics.GameDays > 13.02 || quote.plan.Accepted || !strings.Contains(quote.plan.Reason, "shipment cannot fund") {
				t.Fatalf("13-game-day capital crossed the installment window: %+v", quote)
			}
			if cargoFundingRequired(input, priced)["america"] != input.ReserveByRegion["america"]+1_963_488 {
				t.Fatal("long replay route did not restore the full legacy loan reserve")
			}
		}
	}
}
