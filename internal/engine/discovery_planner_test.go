package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func staleRefreshMemory(in PlanningInput) map[string]RetainedQuayBook {
	memory := make(map[string]RetainedQuayBook)
	for _, port := range in.Catalog.Ports {
		memory[port.ID] = RetainedQuayBook{
			ObservedAt: in.Clock.RealUTC.Add(-gameDuration(2, in.Clock.Calendar.TimeScale)),
			Bids: map[string][]RetainedBid{
				"copper_sheathing": {{PricePence: 100, RemainingQuantity: 30}},
				"salted_meat":      {{PricePence: 100, RemainingQuantity: 30}},
				"provisions":       {{PricePence: 100, RemainingQuantity: 30}},
			},
		}
	}
	return memory
}

func bookRefreshInput(t *testing.T) PlanningInput {
	t.Helper()
	in := scoutAcceptanceInput(t)
	in.QuayBookMemory = staleRefreshMemory(in)
	return in
}

func TestBookRefreshNeedsThreeIndividuallyStaleCommodities(t *testing.T) {
	for _, test := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"boundary", 36 * time.Minute, false},
		{"older", 36*time.Minute + time.Second, true},
		{"future", -time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := bookRefreshInput(t)
			book := in.QuayBookMemory["amsterdam"]
			book.ObservedAt = in.Clock.RealUTC.Add(-test.age)
			in.QuayBookMemory["amsterdam"] = book
			if got := bookRefreshNeeded(in, "amsterdam"); got != test.want {
				t.Fatalf("age %s: refresh=%v want=%v", test.age, got, test.want)
			}
			if !test.want {
				return
			}
			book.CommodityObservedAt = map[string]time.Time{"provisions": in.Clock.RealUTC}
			in.QuayBookMemory["amsterdam"] = book
			if bookRefreshNeeded(in, "amsterdam") {
				t.Fatal("two old commodities and a fresh third triggered a refresh")
			}
			book.CommodityObservedAt["provisions"] = time.Time{}
			if bookRefreshNeeded(in, "amsterdam") {
				t.Fatal("an unknown commodity observation counted as stale evidence")
			}
		})
	}
}

func TestBookRefreshDailySpendBoundary(t *testing.T) {
	in := bookRefreshInput(t)
	baseline := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam")
	if !baseline.Accepted {
		t.Fatalf("baseline refresh rejected: %+v", baseline)
	}
	cost := -baseline.Economics.ExpectedNetPence
	for _, remaining := range []int64{cost - 1, cost, 0} {
		in.SourcingProbeHistory.ScoutExpensePence = scoutDailySeedPence - remaining
		plan := scoutAcceptancePlan(t, bookRefreshCandidates(in, nil), "amsterdam")
		if plan.Accepted != (remaining >= cost) {
			t.Fatalf("remaining=%dp cost=%dp accepted=%v", remaining, cost, plan.Accepted)
		}
	}
	in.SourcingProbeHistory.ScoutExpensePence = 0
	in.SourcingProbeHistory.ScoutCostsUnknown = true
	if scoutAllowance(in) != 0 {
		t.Fatal("unpriced daily scout expense left a spending allowance")
	}
}

func TestScoutDailyBudgetUsesRealDayAndMeasuredExpense(t *testing.T) {
	in := bookRefreshInput(t)
	for _, scale := range []int64{30, 60, 120} {
		history := probeHistoryFixture()
		rows := []store.Settlement{
			{Kind: "relocation", Lane: "a|b|c|scout", CompletedAt: in.Now.Add(-24*time.Hour + time.Second), CostsKnown: true,
				ExplorationExpensePence: new(int64(800)), ExpensesPence: 9000},
			{Kind: "relocation", Lane: "a|b|c|scout", CompletedAt: in.Now.Add(-time.Hour), CostsKnown: true,
				AcquisitionPence: 100, FeesPence: 200, ProvisionsPence: 300, ExpensesPence: 400},
			{Kind: "relocation", Lane: "a|b|c|scout", CompletedAt: in.Now.Add(-24 * time.Hour), CostsKnown: false},
			{Kind: "relocation", Lane: "a|b|c|charter", CompletedAt: in.Now.Add(-time.Hour), CostsKnown: true,
				ExplorationExpensePence: new(int64(500))},
		}
		if err := history.rollingBudget(rows, in.Now, scale); err != nil {
			t.Fatal(err)
		}
		if history.ScoutExpensePence != 1800 || history.ScoutCostsUnknown || history.ProbeExpensePence != 500 {
			t.Fatalf("scale %d mixed budgets or counted expired costs: %+v", scale, history)
		}
		rows[2].CompletedAt = in.Now.Add(-24*time.Hour + time.Nanosecond)
		if err := history.rollingBudget(rows, in.Now, scale); err != nil || !history.ScoutCostsUnknown {
			t.Fatalf("scale %d lost unpriced expense inside the daily window: history=%+v err=%v", scale, history, err)
		}
	}
}

func TestScoutHistoryCutoverRebuildsPerPortConversion(t *testing.T) {
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	in.CompanyID = h.account.CompanyID
	row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
	row.ID, row.Lane = "cutover-scout", scoutLaneKey("amsterdam", "le_havre", row.Commodity)
	putProbeHistorySettlement(t, h, row)
	departure := encode(map[string]any{"company_id": in.CompanyID, "payload": map[string]any{
		"ship_id": in.Ship.Ship.ID, "origin_port_id": "amsterdam", "cargo_manifest": map[string]int64{row.Commodity: 1},
	}})
	if err := h.db.RecordEvent(h.ctx, h.account.ID, "cutover-laden-departure", "ship_departed", departure); err != nil {
		t.Fatal(err)
	}
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	runner.snapshot["_sourcing_probe_history"] = encode(sourcingProbeHistory{
		Version: 3, Cursor: store.ProbeHistoryCursor{SettlementAt: in.Now.Add(time.Hour), DepartureID: math.MaxInt64},
	})
	runner.snapshot["_book_refresh"] = encode(map[string]int64{"started_game_hour": 1})
	in.Now = time.Now().UTC()
	history, err := runner.readProbeOutcomes(h.ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	port := history.ScoutPorts["amsterdam"]
	if history.Version != sourcingProbeHistoryVersion || port.CompletedAt.Sub(row.CompletedAt).Abs() > time.Microsecond || !port.ConvertedAt.After(port.CompletedAt) {
		t.Fatalf("old cursor hid an authoritative port conversion on cutover: %+v", history)
	}
	if _, retained := runner.snapshot["_book_refresh"]; retained {
		t.Fatal("removed fleet refresh latch survived the history cutover")
	}
}

func TestBookRefreshTreatsMissingAndPrunedBooksAsStale(t *testing.T) {
	for _, missing := range []string{"unseen", "pruned"} {
		t.Run(missing, func(t *testing.T) {
			in := bookRefreshInput(t)
			if missing == "unseen" {
				delete(in.QuayBookMemory, "portsmouth")
			} else {
				in.QuayBookMemory["portsmouth"] = RetainedQuayBook{}
			}
			in.BookObservedAt = map[string]time.Time{"portsmouth": in.Now.Add(-time.Hour)}
			if !bookRefreshNeeded(in, "portsmouth") {
				t.Fatal("missing book was not refreshable")
			}
			plans := bookRefreshCandidates(in, nil)
			if len(plans) == 0 || !plans[0].Accepted || plans[0].Destination != "portsmouth" {
				t.Fatalf("missing book did not rank before retained books: %+v", plans)
			}
		})
	}
}

func TestScoutDailyCapScalesOnlyWithAttributedMeasuredNet(t *testing.T) {
	for _, test := range []struct{ net, spent, want int64 }{
		{0, 0, 100_000},
		{-100_000, 0, 100_000},
		{20_000, 0, 110_000},
		{200_000, 0, 200_000},
		{math.MaxInt64, 0, 200_000},
		{20_000, 109_999, 1},
		{20_000, 110_000, 0},
	} {
		in := bookRefreshInput(t)
		in.Balances["north"] = 100_000_000
		in.SourcingProbeHistory.ScoutConversionNetPence = test.net
		in.SourcingProbeHistory.ScoutExpensePence = test.spent
		in.SourcingProbeHistory.ConversionNetPence = math.MaxInt64
		if got := scoutAllowance(in); got != test.want {
			t.Fatalf("scout net %dp spent %dp: allowance=%dp want=%dp", test.net, test.spent, got, test.want)
		}
	}
}

func TestScoutConversionNetRequiresSameShipSettledIncomeWithinRealDay(t *testing.T) {
	now := time.Now().UTC()
	for _, scale := range []int64{30, 60, 120} {
		for _, scenario := range []string{"scout", "other ship", "charter probe", "unknown costs", "expired", "loss"} {
			t.Run(fmt.Sprintf("%d/%s", scale, scenario), func(t *testing.T) {
				probe := store.Settlement{Kind: "relocation", ShipID: recoveryShipBlastoise,
					Origin: "hamburg", Destination: "amsterdam", Lane: scoutLaneKey("amsterdam", "hamburg", "provisions"),
					CompletedAt: now.Add(-26 * time.Hour), CostsKnown: true}
				income := store.Settlement{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: "amsterdam", Destination: "hamburg",
					QuantityTons: 1, CostsKnown: true, StartedAt: now.Add(-25 * time.Hour), CompletedAt: now.Add(-23 * time.Hour),
					ProceedsPence: 50_000, AcquisitionPence: 10_000, FeesPence: 1000, ProvisionsPence: 2000, ExpensesPence: 3000}
				want := int64(34_000)
				switch scenario {
				case "other ship":
					income.ShipID, want = recoveryShipLapras, 0
				case "charter probe":
					probe.Lane, want = "amsterdam|hamburg|provisions|charter", 0
				case "unknown costs":
					income.CostsKnown, want = false, 0
				case "expired":
					income.CompletedAt, want = now.Add(-24*time.Hour), 0
				case "loss":
					income.ProceedsPence, want = 10_000, -6000
				}
				history := probeHistoryFixture()
				if err := history.rollingBudget([]store.Settlement{probe, income}, now, scale); err != nil {
					t.Fatal(err)
				}
				if history.ScoutConversionNetPence != want {
					t.Fatalf("attributed scout net=%dp want=%dp", history.ScoutConversionNetPence, want)
				}
			})
		}
	}
}

func TestScoutSettledDailyExpenseBlocksFreshRunnerAdmissionAndFirstSend(t *testing.T) {
	for _, firstSend := range []bool{false, true} {
		t.Run(fmt.Sprint(firstSend), func(t *testing.T) {
			h, runner := bookRefreshRunner(t, 6)
			if firstSend {
				scoutAcceptanceAdmit(t, h, runner)
			}
			putProbeHistorySettlement(t, h, store.Settlement{
				ID: "spent-before-restart", Kind: "relocation", ShipID: recoveryShipLapras,
				Origin: "flushing", Destination: "hamburg", Commodity: "provisions",
				Lane: scoutLaneKey("hamburg", "flushing", "provisions"), CostsKnown: true,
				ExplorationExpensePence: new(scoutDailyBudgetPence), CompletedAt: time.Now().UTC().Add(-time.Hour),
			})
			runner.snapshot["_meta"] = encode(runner.meta)
			if err := h.db.SaveSnapshot(h.ctx, h.account.ID, runner.snapshot); err != nil {
				t.Fatal(err)
			}
			restarted := h.runner(primaryTestKey)
			restarted.stateCache, restarted.generation = runner.stateCache, runner.generation
			restarted.refreshSourcingHistory(h.ctx, restarted.planningInput(ShipState{}))
			restarted.refreshSourcingBudget(h.ctx, restarted.planningInput(ShipState{}))
			ship := harnessShip(t, restarted, recoveryShipBlastoise)
			if restarted.sourcingHistory.Outcomes.ScoutExpensePence != scoutDailyBudgetPence {
				t.Fatal("fresh runner failed to recover authoritative settled scout expense")
			}
			if !firstSend {
				changed, reason, err := restarted.considerSourcing(h.ctx, ship)
				if err != nil || changed || !strings.Contains(reason, "scout allowance") {
					t.Fatalf("spent daily budget admitted a fresh refresh: changed=%v reason=%q err=%v", changed, reason, err)
				}
				h.missingMission(ship.Ship.ID)
				return
			}
			mission, data := h.mission(ship.Ship.ID)
			if err := restarted.stage(h.ctx, mission, &data, ship, "depart", "/v1/fleet/depart",
				map[string]any{"ship_id": ship.Ship.ID, "dest_port_id": data.Plan.Destination}); err != nil {
				t.Fatal(err)
			}
			if err := restarted.relocationSpendingReady(h.ctx, ship, &data, 0); err == nil || !strings.Contains(err.Error(), "scout allowance") {
				t.Fatalf("settled expense failed to close first-send allowance: %v", err)
			}
			if _, err := restarted.dispatch(h.ctx, mission, &data); err != nil {
				t.Fatal(err)
			}
			if h.api.mutationCount("/v1/fleet/depart") != 0 {
				t.Fatal("first-send preflight spent beyond the recovered daily budget")
			}
			persisted, current := h.mission(ship.Ship.ID)
			if current.Action != nil || current.CooldownUntil.IsZero() || persisted.Phase == "sailing" {
				t.Fatalf("first-send veto was not durably retired: %+v %+v", persisted, current)
			}
		})
	}
}

func TestBookRefreshActiveMissionAllowsDistinctTarget(t *testing.T) {
	h, runner := bookRefreshRunner(t, 6)
	_, first := scoutAcceptanceAdmit(t, h, runner)
	changed, reason, err := runner.considerSourcing(h.ctx, harnessShip(t, runner, recoveryShipCharizard))
	if err != nil || !changed || scoutAcceptanceMissionCount(t, h, runner) != 2 {
		t.Fatalf("active refresh blocked a distinct relocation: changed=%v reason=%q err=%v", changed, reason, err)
	}
	_, second := h.mission(recoveryShipCharizard)
	if second.Relocation == nil || !isBookRefresh(second.Plan) || first.Plan.Destination == second.Plan.Destination {
		t.Fatalf("concurrent refresh did not retain a distinct target: first=%+v second=%+v", first.Plan, second.Plan)
	}
}

func TestDiscoveryTriesPassengerIncomeBeforePaidEmptyRefresh(t *testing.T) {
	h, runner := bookRefreshRunner(t, 6)
	runner.stateCache.Ships = runner.stateCache.Ships[2:]
	for i := range runner.stateCache.Catalog.ShipClasses {
		runner.stateCache.Catalog.ShipClasses[i].PassengersPerTon = 1
	}
	runner.meta["marques"] = runner.meta["ships"]
	runner.meta["passenger_offers"] = runner.meta["ships"]
	runner.meta["passenger_waiting/hamburg"] = runner.meta["ships"]
	runner.meta["passenger_aboard/"+recoveryShipBlastoise] = runner.meta["ships"]
	runner.snapshot["marques"] = encode([]letterOfMarque{})
	runner.snapshot["passenger_offers"] = encode([]gameapi.PassengerOffer{})
	runner.snapshot["passenger_waiting"] = encode(map[string]any{"hamburg": []gameapi.PassengerWaitingGroup{{
		ID: "discovery-passengers", Kind: "travellers", Size: 10, OriginPortID: "hamburg",
		DestPortID: "amsterdam", BoardBy: runner.stateCache.Clock.RealUTC.Add(time.Hour),
	}}})
	runner.snapshot["passenger_aboard"] = encode(map[string]any{recoveryShipBlastoise: []gameapi.PassengerAboardGroup{}})
	if changed, err := runner.work(h.ctx); err != nil || !changed {
		t.Fatalf("passenger-first work pass: changed=%v err=%v holds=%+v", changed, err, runner.holding)
	}
	_, data := h.mission(recoveryShipBlastoise)
	if data.Passage == nil || data.Relocation != nil {
		t.Fatalf("paid empty discovery preempted passenger income: %+v", data)
	}
}
