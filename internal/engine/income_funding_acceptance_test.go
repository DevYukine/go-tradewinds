package engine

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func incomeFundingInput(t *testing.T, kind string) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Catalog.Ports = []Port{in.Catalog.Ports[0], in.Catalog.Ports[2]}
	book := in.Markets["le_havre"]
	book.Books = append(book.Books, sourcingFixture[CommodityBook](t, `{"commodity_id":"sawn_planks","depth":{"asks":[{"order_id":"income-planks","price_pence":1000,"remaining_quantity":100}],"bids":[]}}`))
	in.Markets["le_havre"] = book
	in.Balances = map[string]int64{"channel": 75_000, "donor": 10_000_000}
	in.ReserveByRegion = map[string]int64{"channel": 75_000, "donor": 75_000}
	in.ReservePence = 75_000
	in.Rates = []Rate{{FromRegion: "donor", ToRegion: "channel", FeeBPS: 100}}
	in.Metadata["rates"] = sourcingFreshMeta(in.Now)
	if kind == "freight" {
		in.Contracts = []Contract{{
			ID: "income-freight", ContractType: "freight", Status: "open",
			OriginPortID: "le_havre", DestPortID: "portsmouth", CommodityID: "sawn_planks",
			IssuerCompanyID: "other-company", QuantityRequired: 24, RemainingQuantity: 24,
			RewardPence: 5_000, CollateralPence: 40_000,
		}}
	}
	return in
}

func incomeFundingBest(t *testing.T, in PlanningInput) Plan {
	t.Helper()
	plans := PlanVoyages(in)
	if len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("observed donor surplus stranded ordinary income: %+v", plans)
	}
	return plans[0]
}

func incomeFundingFees(t *testing.T, economics Economics) int64 {
	t.Helper()
	payload, err := json.Marshal(economics)
	if err != nil {
		t.Fatal(err)
	}
	var costs struct {
		FundingFeesPence int64 `json:"funding_fees_pence"`
	}
	if err := json.Unmarshal(payload, &costs); err != nil {
		t.Fatal(err)
	}
	return costs.FundingFeesPence
}

func TestIncomeFundingOrdinaryCharterAndFreightUseDonorSurplus(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"charter", "freight"} {
		t.Run(kind, func(t *testing.T) {
			in := incomeFundingInput(t, kind)
			before, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			plan := incomeFundingBest(t, in)
			if plan.Kind != kind || plan.Quantity != 24 || plan.Economics.ExpectedNetPence <= 0 {
				t.Fatalf("best funded ordinary shipment was not admitted: %+v", plan)
			}
			fees := incomeFundingFees(t, plan.Economics)
			if fees <= 0 {
				t.Fatalf("funded shipment did not disclose its modeled operating fee: %+v", plan.Economics)
			}
			e := plan.Economics
			wantNet := e.RevenuePence - e.PurchasePence - e.CustomsPence - e.ProvisionsPence - e.BerthDuesPence - e.UpkeepPence - e.TollPence - fees
			if e.ExpectedNetPence != wantNet || e.StressNetPence != wantNet-e.StressHaircutPence-e.PriceUncertaintyStressPence ||
				math.Abs(e.NetPencePerGameDay-float64(wantNet)/e.GameDays) > 1e-8 {
				t.Fatalf("funding principal or fee was omitted or charged twice: %+v fees=%d", e, fees)
			}
			if kind == "freight" && (e.PurchasePence != 0 || e.CustomsPence != 0 || plan.BuyLimitPence != 0 || e.CollateralPence != 40_000) {
				t.Fatalf("sealed freight invented cargo procurement or expensed refundable collateral: %+v", plan)
			}
			after, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("candidate funding spent or rewrote authoritative caller observations")
			}
		})
	}
}

func TestIncomeFundingFeesCanEraseOtherwisePositiveIncome(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"charter", "freight"} {
		t.Run(kind, func(t *testing.T) {
			in := incomeFundingInput(t, kind)
			if kind == "charter" {
				reward := int64(1_100)
				in.Contracts[0].RewardPerTonPence = &reward
			}
			lowFee := incomeFundingBest(t, in)
			if lowFee.Economics.ExpectedNetPence <= 0 {
				t.Fatalf("low-fee control has no measured margin: %+v", lowFee)
			}
			in.Rates[0].FeeBPS = 5_000
			for _, plan := range PlanVoyages(in) {
				if plan.Accepted {
					t.Fatalf("transfer fees erased this shipment's margin but it remained spendable: %+v", plan)
				}
			}
		})
	}
}

func TestIncomeFundingRanksFreightAfterCollateralFundingFees(t *testing.T) {
	t.Parallel()
	in := incomeFundingInput(t, "freight")
	in.Rates[0].FeeBPS = 1_000
	highCollateral := in.Contracts[0]
	highCollateral.ID = "higher-gross-reward"
	highCollateral.RewardPence = 10_000
	highCollateral.CollateralPence = 50_000
	lowCollateral := highCollateral
	lowCollateral.ID = "better-fee-net"
	lowCollateral.RewardPence = 7_000
	lowCollateral.CollateralPence = 1_000
	in.Contracts = []Contract{highCollateral, lowCollateral}
	plans := PlanVoyages(in)
	if len(plans) < 2 || !plans[0].Accepted || !plans[1].Accepted || plans[0].ContractID != lowCollateral.ID ||
		plans[0].Economics.ExpectedNetPence <= plans[1].Economics.ExpectedNetPence {
		t.Fatalf("gross reward displaced the better fee-net ordinary freight: %+v", plans)
	}
}

func TestIncomeFundingCombinesDonorsWithPerTransferRounding(t *testing.T) {
	t.Parallel()
	in := incomeFundingInput(t, "freight")
	in.Contracts[0].RewardPence = 10_000
	in.Balances = map[string]int64{"channel": 75_000, "donor-a": 100_000, "donor-b": 100_000}
	in.ReserveByRegion = map[string]int64{"channel": 75_000, "donor-a": 75_000, "donor-b": 75_000}
	in.Rates = []Rate{
		{FromRegion: "donor-a", ToRegion: "channel", FeeBPS: 1_000},
		{FromRegion: "donor-b", ToRegion: "channel", FeeBPS: 1_000},
	}
	plan := incomeFundingBest(t, in)
	e := plan.Economics
	needed := e.CollateralPence + e.StoreCashPence + e.BerthDuesPence +
		int64(math.Ceil(float64(in.FleetDailyUpkeepPence)*e.GameDays))
	// The first donor spends its 25000p surplus and delivers 22500p.
	// The second gross rounds up, but its charged server fee rounds down.
	if needed <= 22_500 || needed > 45_000 {
		t.Fatalf("fixture no longer requires two reserve-safe donors: need=%d plan=%+v", needed, plan)
	}
	secondGross := ((needed-22_500)*10_000 + 8_999) / 9_000
	wantFees := int64(2_500) + secondGross*1_000/10_000
	if fees := incomeFundingFees(t, e); fees != wantFees {
		t.Fatalf("funding did not charge each observed transfer once: got=%d want=%d plan=%+v", fees, wantFees, plan)
	}
}

func TestIncomeFundingProtectsRegionalCashAndRateAuthority(t *testing.T) {
	t.Parallel()
	for _, changed := range []string{"donor reserve", "origin reserve", "destination reserve", "stale rates", "failed rates", "missing rates"} {
		t.Run(changed, func(t *testing.T) {
			in := incomeFundingInput(t, "freight")
			incomeFundingBest(t, in)
			switch changed {
			case "donor reserve":
				in.ReserveByRegion["donor"] = in.Balances["donor"]
			case "origin reserve":
				in.ReserveByRegion["channel"] = in.Balances["donor"] + in.Balances["channel"]
			case "destination reserve":
				in.Catalog.Ports[1].BankingRegion = "destination"
				in.Balances["destination"] = 75_000
				in.ReserveByRegion["destination"] = 75_000
				// The only observed transfer route reaches the origin, not
				// this destination's harbour bill above its protected reserve.
			case "stale rates":
				meta := in.Metadata["rates"]
				meta.UpdatedAt = in.Now.Add(-3 * time.Minute)
				in.Metadata["rates"] = meta
			case "failed rates":
				meta := in.Metadata["rates"]
				meta.Error = "fee observation unavailable"
				in.Metadata["rates"] = meta
			case "missing rates":
				delete(in.Metadata, "rates")
			}
			for _, plan := range PlanVoyages(in) {
				if plan.Accepted {
					t.Fatalf("%s was spent to admit ordinary income: %+v", changed, plan)
				}
			}
		})
	}
}

func incomeFundingRunner(t *testing.T) (*recoveryHarness, *Runner) {
	t.Helper()
	h := newRecoveryHarness(t)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["status"] = "berthed"
	row["ship"].(map[string]any)["cannons_count"] = 4
	row["effective_hold_capacity_tons"] = 52
	row["stores"] = map[string]any{"provisions_tons": 4, "sea_days_remaining": 5}
	row["route_legs"] = []any{}
	row["voyage"] = nil
	h.api.setFleet([]map[string]any{row})
	h.api.setFactorPorts(recoveryFarPort)
	h.api.setVaultBalance("north", 1_000_000)
	h.api.setVaultBalance("south", 1_000_000)
	runner := h.runner(primaryTestKey)
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	reserves, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Company bills are already funded. Only the selected shipment needs
	// remittance; a company-level reserve repair is not evidence of I1.
	h.api.setVaultBalance("north", reserves["north"])
	return h, runner
}

func TestIncomeFundingIncumbentMissionCashCannotBecomeDonorSurplus(t *testing.T) {
	t.Parallel()
	h, runner := incomeFundingRunner(t)
	incumbent := sailingShipRow(recoveryShipCharizard, "Charizard", recoveryFarPort, recoveryHomePort)
	incumbent["ship"].(map[string]any)["status"] = "berthed"
	incumbent["route_legs"] = []any{}
	incumbent["voyage"] = nil
	h.api.mu.Lock()
	h.api.fleet = append(h.api.fleet, incumbent)
	h.api.mu.Unlock()
	h.putMission(recoveryShipCharizard, "reserved-incumbent-cargo", "buy", missionData{
		Plan: Plan{Kind: "charter", ShipID: recoveryShipCharizard, Origin: recoveryFarPort,
			Destination: recoveryHomePort, Commodity: recoveryGrain, Quantity: 1_000, BuyLimitPence: 1_000},
		CostsKnown: true,
	})
	runner.generation = uuid.NewString()
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	ship, ok := runner.ship(recoveryShipBlastoise)
	if !ok {
		t.Fatal("income ship missing from authoritative fleet")
	}
	input, err := runner.committedPlanningInput(h.ctx, ship)
	if err != nil {
		t.Fatal(err)
	}
	if input.ReserveByRegion["south"] < 1_000_000 {
		t.Fatalf("incumbent's unpaid immutable-limit cargo cash disappeared: %+v", input.ReserveByRegion)
	}
	for _, plan := range PlanVoyages(input) {
		if plan.Accepted {
			t.Fatalf("ordinary income borrowed incumbent procurement cash: %+v", plan)
		}
	}
}

func TestIncomeFundingShipOwnsDurableRemittanceAndRejectsChangedOpportunity(t *testing.T) {
	t.Parallel()
	h, runner := incomeFundingRunner(t)
	var funded bool
	for range 12 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
		mission, err := h.db.GetMission(h.ctx, h.account.ID, recoveryShipBlastoise)
		if err != nil {
			t.Fatal(err)
		}
		if mission == nil {
			continue
		}
		_, data := h.mission(recoveryShipBlastoise)
		if data.Action == nil || data.Action.Stage != "remit" {
			continue
		}
		if data.Plan.Kind != "spot" || data.Action.Path != "/v1/treasury/remit" || len(h.pending()) != 0 ||
			h.api.mutationCount("/v1/treasury/remit") != 0 || h.api.mutationCount("/v1/market/place-order") != 0 {
			t.Fatalf("ordinary funding was not a never-sent ship-owned durable request: %+v", data)
		}
		h.missingMission("__treasury")
		// Restart may re-quote an unsent intent, but must send owned funding only once.
		runner = h.runner(primaryTestKey)
		for range 8 {
			if _, err := h.tick(runner); err != nil {
				t.Fatal(err)
			}
			calls := h.api.mutationsFor("/v1/treasury/remit")
			if len(calls) == 0 {
				continue
			}
			if len(calls) != 1 || calls[0].Status != http.StatusOK {
				t.Fatalf("restart did not send exactly one successful owned remittance: %+v", calls)
			}
			funded = true
			break
		}
		break
	}
	if !funded {
		t.Fatal("cash-poor ordinary income never executed its ship-owned remittance")
	}
	// A new observation erases the purchase margin after paying the fee but
	// before buying any cargo. No stale IOC or fee-only funding loop is legal.
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/v1/market/book" && request.URL.Query().Get("port_id") == recoveryHomePort {
			document := h.api.bookDocument(recoveryHomePort)
			for _, raw := range document["books"].([]any) {
				entry := raw.(map[string]any)
				if entry["commodity_id"] == recoveryGrain {
					entry["depth"].(map[string]any)["asks"].([]any)[0].(map[string]any)["price_pence"] = int64(10_000)
				}
			}
			h.api.writeJSON(writer, http.StatusOK, document)
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/v1/fleet/offshore" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read harbour request: %v", err)
				return
			}
			var payload struct {
				ShipID string `json:"ship_id"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Errorf("decode harbour request: %v", err)
				return
			}
			h.api.mu.Lock()
			defer h.api.mu.Unlock()
			for _, row := range h.api.fleet {
				ship := row["ship"].(map[string]any)
				if ship["id"] == payload.ShipID && ship["status"] == "berthed" && ship["current_port_id"] == recoveryHomePort {
					ship["status"] = "anchored_roadstead"
					h.api.vaultBalance["north"] -= 25
					h.api.record(request, body, http.StatusOK)
					h.api.writeJSON(writer, http.StatusOK, map[string]any{
						"ship_id": payload.ShipID, "status": "anchored_roadstead",
						"berth_dues_pence": 25, "berth_dues_debt_pence": 0,
					})
					return
				}
			}
			h.api.record(request, body, http.StatusConflict)
			h.api.writeProblem(writer, http.StatusConflict, "invalid_harbour_state", "ship is not berthed at the observed source")
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	runner = h.runner(primaryTestKey)
	for range 16 {
		if _, err := h.tick(runner); err != nil {
			t.Fatal(err)
		}
	}
	if h.api.mutationCount("/v1/treasury/remit") != 1 || h.api.mutationCount("/v1/market/place-order") != 0 {
		t.Fatalf("changed opportunity caused stale procurement or repeated fee-only funding: remit=%d orders=%d",
			h.api.mutationCount("/v1/treasury/remit"), h.api.mutationCount("/v1/market/place-order"))
	}
	rows, err := h.db.SettlementHistory(h.ctx, h.account.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var remit struct {
		Gross int64 `json:"gross_amount_pence"`
	}
	if err := json.Unmarshal(h.api.mutationsFor("/v1/treasury/remit")[0].Body, &remit); err != nil {
		t.Fatal(err)
	}
	actualFee := remit.Gross * 100 / 10_000
	if len(rows) != 1 || rows[0].ShipID != recoveryShipBlastoise || rows[0].QuantityTons != 0 ||
		rows[0].AcquisitionPence != 0 || rows[0].ProceedsPence != 0 || actualFee <= 0 ||
		rows[0].FeesPence != actualFee || !rows[0].CostsKnown {
		t.Fatalf("paid pre-buy funding fee must close exactly once as an actual zero-cargo loss: %+v fee=%d", rows, actualFee)
	}
}
