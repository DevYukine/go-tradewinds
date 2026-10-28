package engine

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacitySavingsRateRecordedWindow(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := growthWindowRunner(t, h)
	asOf := time.Now().UTC()
	runner.stateCache.Clock.Calendar.TimeScale = 60
	for _, row := range []store.Settlement{
		{ID: "savings-spot", Kind: "spot", CostsKnown: true, QuantityTons: 10, AcquisitionPence: 60_000, ProceedsPence: 660_000, GameSeconds: 3_600, CompletedAt: asOf.Add(-23 * time.Hour)},
		{ID: "savings-charter", Kind: "charter", CostsKnown: true, QuantityTons: 10, AcquisitionPence: 100_000, ProceedsPence: 400_000, GameSeconds: 7_200, CompletedAt: asOf.Add(-time.Hour)},
		{ID: "savings-unknown-loss", Kind: "freight", CostsKnown: false, FeesPence: 60_000, CompletedAt: asOf.Add(-time.Minute)},
		{ID: "savings-too-old", Kind: "spot", CostsKnown: true, QuantityTons: 10, ProceedsPence: 60_000_000, GameSeconds: 3_600, CompletedAt: asOf.Add(-25 * time.Hour)},
	} {
		persistGrowthWindowSettlement(t, h, row)
	}
	active := snapshotTestLoan()
	installment := int64(24_205)
	active.InstallmentPence = &installment
	closed, lender, bullet := active, active, active
	closed.ID, closed.Status = "savings-closed", "repaid"
	lender.ID, lender.BorrowerCompanyID, lender.LenderCompanyID = "savings-lender", "another-company", recoveryCompanyID
	bullet.ID, bullet.Repayment = "savings-bullet", "bullet"
	runner.stateCache.Loans = []gameapi.Loan{active, closed, lender, bullet}
	rate, err := runner.capacitySavingsRate(h.ctx, asOf)
	want := float64(600_000+300_000-60_000)/60 - float64(installment)/10
	if err != nil || math.IsNaN(rate) || math.Abs(rate-want) > 1e-9 {
		t.Fatalf("savings rate = %v, %v; want %v including charter, unknown loss, upkeep and active borrower installment", rate, err, want)
	}
}

// A recent known closeout supports measured per-hull earnings without adding a
// delivered lane cycle. Its older offset independently fixes 24-hour savings.
func seedCapacitySavingsWindows(t *testing.T, h *recoveryHarness, savingsNet int64) {
	t.Helper()
	const recentNet = int64(60_000_000_000)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "savings-measured-closeout", Kind: "aborted", CostsKnown: true,
		ProceedsPence: recentNet, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-time.Hour),
	})
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "savings-prior-loss", Kind: "aborted", CostsKnown: true,
		AcquisitionPence: recentNet - savingsNet, GameSeconds: 3_600,
		CompletedAt: time.Now().UTC().Add(-13 * time.Hour),
	})
}

// Keep the executable capacity world with a larger local hull. Recent known
// income supports its measured rate; an older loss preserves the savings rate.
func capacitySavingsRunnerFixture(t *testing.T, income bool) (*recoveryHarness, *Runner, GrowthDecision, GrowthDecision) {
	t.Helper()
	h := newRecoveryHarness(t)
	row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
	row["ship"].(map[string]any)["created_at"] = time.Now().UTC().Add(-30 * 24 * time.Hour)
	h.api.setFleet([]map[string]any{row})
	newExpansionFactorAPI(h)
	runner := growthWindowRunner(t, h)
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.stateCache.Contracts = nil
	savingsNet := int64(0)
	if income {
		savingsNet = 60_000_000
	}
	seedCapacitySavingsWindows(t, h, savingsNet)
	small := runner.stateCache.Catalog.ShipClasses[0]
	large := small
	large.ID, large.HoldCapacityTons = "savings-large-hull", 240
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, large)
	for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
		oldKey := routeKey(pair[0], pair[1], small.ID)
		key := routeKey(pair[0], pair[1], large.ID)
		route := runner.stateCache.Routes[oldKey]
		route.ShipClass = large.ID
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	quote, err := runner.growthQuote(port, small)
	if err != nil {
		t.Fatal(err)
	}
	price := int64(120_000)
	quote.ShipClass, quote.TotalCostPence = large.ID, &price
	key := recoveryHomePort + "|" + large.ID
	runner.stateCache.Quotes[key] = quote
	runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+small.ID]
	// A deep bid makes the larger hold useful, independently of finite charters.
	bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	evaluate := func(hull Hull) GrowthDecision {
		t.Helper()
		evidence := growthEvidence{workingByRegion: make(map[string]int64)}
		if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
			t.Fatalf("capacity cash: %s, %v", reason, err)
		}
		input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
		if err != nil || reason != "" {
			t.Fatalf("capacity fleet: %s, %v", reason, err)
		}
		grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
		input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
		quoted, err := runner.growthQuote(port, hull)
		if err != nil {
			t.Fatal(err)
		}
		decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quoted, input, &evidence)
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	largeDecision := evaluate(large)
	required := growthCashTotal(largeDecision.QuotedHullPence, largeDecision.SlippageReservePence, largeDecision.CannonsPence, largeDecision.InitialStoresPence, largeDecision.WorkingCapitalPence)
	protected, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runner.stateCache.Vaults {
		region := runner.stateCache.Vaults[i].BankingRegion
		runner.stateCache.Vaults[i].BalancePence = protected[region]
		if region == port.BankingRegion {
			runner.stateCache.Vaults[i].BalancePence += required - 1_000
		}
	}
	buy, save := evaluate(small), evaluate(large)
	depth := save
	depth.IncrementalNetPencePerGameDay = save.DepthNetPencePerGameDay
	if !buy.Accepted || save.Accepted || !save.EconomicallyViable || save.CashShortfallPence <= 0 || !betterCapacityCandidate(depth, buy) {
		t.Fatalf("fixture must offer an affordable small hull and cash-blocked better-return large hull: buy %+v save %+v", buy, save)
	}
	return h, runner, buy, save
}

func TestCapacitySavingsHoldsAffordableBuildAndAgentHire(t *testing.T) {
	t.Parallel()
	h, runner, _, save := capacitySavingsRunnerFixture(t, true)
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now().UTC())
	if err != nil || rate <= 0 || float64(save.CashShortfallPence)/rate > 60 {
		t.Fatalf("fixture saving wait exceeds the measured window: rate %v, shortfall %d, error %v", rate, save.CashShortfallPence, err)
	}
	before := time.Now().UTC()
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	h.missingMission("__growth")
	h.missingMission("__treasury")
	if h.api.mutationCount("/v1/shipyard/build-ship") != 0 || h.api.mutationCount("/v1/company/hire-factor") != 0 {
		t.Fatal("saving spent cash on an affordable small hull or an expansion agent")
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	wantCapital := growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence)
	if target.PortID != recoveryHomePort || target.HullID != "savings-large-hull" || target.CapitalPence != wantCapital ||
		target.RequiredPence != growthCashTotal(wantCapital, save.WorkingCapitalPence) || target.DepthNetPencePerGameDay != save.DepthNetPencePerGameDay || !target.ExpiresAt.After(before) {
		t.Fatalf("missing or incorrect durable savings target: %+v", target)
	}
	wait := time.Duration(min(
		capacityHistoryWindow.Seconds()*float64(runner.stateCache.Clock.Calendar.TimeScale)/86_400,
		1.25*float64(save.CashShortfallPence)/rate,
	) * 86_400 / float64(runner.stateCache.Clock.Calendar.TimeScale) * float64(time.Second))
	if target.ExpiresAt.Before(before.Add(wait-time.Second)) || target.ExpiresAt.After(time.Now().UTC().Add(wait+time.Second)) {
		t.Fatalf("target expires %s, want now plus savings wait %s", target.ExpiresAt, wait)
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := history.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history %T", history)
	}
	foundSavings, foundHold := false, false
	for _, decision := range decisions {
		foundSavings = foundSavings || strings.HasPrefix(decision.Kind, "capacity_savings")
		foundHold = foundHold || decision.Kind == "hold" && decision.Reason == "capacity growth saves for a better hull"
	}
	if !foundSavings || !foundHold {
		t.Fatalf("missing savings decision or exact savings hold: savings=%t hold=%t", foundSavings, foundHold)
	}
	if port := decode[string](runner.snapshot, "_capacity_growth_port"); port != "" {
		t.Fatalf("saving retained an expansion-agent yard commitment %q", port)
	}
}

func TestCapacitySavingsNonpositiveRateBuildsSmallHull(t *testing.T) {
	t.Parallel()
	h, runner, _, _ := capacitySavingsRunnerFixture(t, false)
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now().UTC())
	if err != nil || rate > 0 {
		t.Fatalf("empty settlement window rate = %v, %v; want nonpositive", rate, err)
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("ordinary affordable build did not proceed: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
	mission, data := h.mission("__growth")
	if data.Capacity == nil || data.BuildClass != recoveryHull || data.Action == nil || data.Action.Path != "/v1/shipyard/build-ship" {
		t.Fatalf("nonpositive savings rate did not stage the ordinary small build: %+v", data)
	}
	if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
		t.Fatalf("ordinary small build did not dispatch exactly once: changed=%t err=%v holds=%v", changed, err, runner.holding)
	}
}

func TestCapacitySavingsAbsentYardMarkerLifecycle(t *testing.T) {
	t.Parallel()
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "live holds", true: "expired builds"}[expired], func(t *testing.T) {
			h, runner, _, save := capacitySavingsRunnerFixture(t, true)
			// Only the small hull is sold at the present yard. The stored target
			// was observed elsewhere, where neither a factor nor a ship is now present.
			quote := runner.stateCache.Quotes[recoveryHomePort+"|savings-large-hull"]
			quote.Available = false
			runner.stateCache.Quotes[recoveryHomePort+"|savings-large-hull"] = quote
			runner.stateCache.Factors = []string{recoveryHomePort}
			for i := range runner.stateCache.Catalog.Ports {
				if runner.stateCache.Catalog.Ports[i].ID == recoveryFarPort {
					runner.stateCache.Catalog.Ports[i].HasShipyard = true
				}
			}
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("absent-yard cash: %s, %v", reason, err)
			}
			expiry := time.Now().UTC().Add(time.Hour)
			if expired {
				expiry = time.Now().UTC().Add(-time.Second)
			}
			runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
				PortID: recoveryFarPort, HullID: "savings-large-hull",
				RequiredPence:           evidence.available + save.CashShortfallPence,
				CapitalPence:            growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence),
				DepthNetPencePerGameDay: save.DepthNetPencePerGameDay, ExpiresAt: expiry,
			})
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			if !expired {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if h.api.mutationCount("/v1/shipyard/build-ship") != 0 || h.api.mutationCount("/v1/company/hire-factor") != 0 {
					t.Fatal("live absent-yard marker spent savings")
				}
				return
			}
			mission, data := h.mission("__growth")
			if data.BuildClass != recoveryHull || data.Action == nil || data.Action.Path != "/v1/shipyard/build-ship" {
				t.Fatalf("expired savings marker did not release the small build: %+v", data)
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed || h.api.mutationCount("/v1/shipyard/build-ship") != 1 {
				t.Fatalf("expired marker did not dispatch the small build exactly once: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
		})
	}
}
