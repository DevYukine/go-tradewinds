package engine

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacitySavingsUnevaluatedTargetCannotFinanceAnotherHull(t *testing.T) {
	t.Parallel()
	for _, saving := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmarked candidate finances", true: "unevaluated B holds C"}[saving], func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			runner.stateCache.Clock.Calendar.TimeScale = 60
			runner.stateCache.Contracts = nil
			bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
			// Keep the executable books across the cash-only refresh below.
			loanCapacityCashShortfall(t, h, runner, 100_000)
			runner.stateCache.Clock.Calendar.TimeScale = 60
			runner.stateCache.Contracts = nil
			bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
			port, hull, candidate, _, evidence := loanCapacityDecision(t, h, runner)
			if candidate.Accepted || !candidate.EconomicallyViable || candidate.CashShortfallPence <= 0 || !runner.loanAuthority() {
				t.Fatalf("C must be cash-blocked with real borrower authority: %+v", candidate)
			}
			rate, err := runner.capacitySavingsRate(h.ctx, evidence.asOf)
			if err != nil || rate <= 0 || !capacitySavingsFinancingWorthwhile(GrowthDecision{}, candidate, rate, len(runner.stateCache.Ships)) {
				t.Fatalf("C must pass the old savings financing gate: rate=%v candidate=%+v err=%v", rate, candidate, err)
			}
			target := capacitySavingsTarget{
				PortID: recoveryFarPort, HullID: "collier",
				RequiredPence: evidence.available + 1_000, CapitalPence: 200_000,
				DepthNetPencePerGameDay: 2 * candidate.DepthNetPencePerGameDay,
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}
			// B remains in the catalog for its marginal-upkeep measurement,
			// but no current yard quote makes it an evaluated candidate.
			unevaluated := hull
			unevaluated.ID = target.HullID
			runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, unevaluated)
			if saving {
				if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
					t.Fatal(err)
				}
			}
			before := h.api.mutationCount("/v1/loan/quote")
			changed, err := runner.considerCapacityGrowth(h.ctx, evidence)
			if err != nil {
				t.Fatal(err)
			}
			quotes := h.api.mutationCount("/v1/loan/quote") - before
			if !saving {
				_, data := h.mission("__growth")
				if !changed || quotes == 0 || data.BuildPort != port.ID || data.BuildClass != hull.ID || data.Capacity == nil || data.Capacity.Financing == nil {
					t.Fatalf("positive control did not quote and stage financing for C: changed=%t quotes=%d intent=%+v", changed, quotes, data)
				}
				return
			}
			if changed || quotes != 0 {
				t.Fatalf("unevaluated B financed another candidate: changed=%t quote GETs=%d", changed, quotes)
			}
			h.missingMission("__growth")
			h.missingMission("__treasury")
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("live B savings were spent on another candidate")
			}
			got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
			if got.PortID != target.PortID || got.HullID != target.HullID || got.RequiredPence != target.RequiredPence || !got.ExpiresAt.Equal(target.ExpiresAt) {
				t.Fatalf("financing changed the unevaluated B marker: got %+v want %+v", got, target)
			}
		})
	}
}

func TestCapacitySavingsTotalInterestRefusalRecordsTargetDecision(t *testing.T) {
	t.Parallel()
	h, runner := loanCapacityFixture(t, 750_000, false)
	runner.stateCache.Clock.Calendar.TimeScale = 60
	runner.stateCache.Contracts = nil
	bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	seedCapacitySavingsWindows(t, h, 0)
	port, small, _, _, _ := loanCapacityDecision(t, h, runner)
	large := small
	large.ID, large.HoldCapacityTons = "brigantine", 240
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, large)
	for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
		oldKey := routeKey(pair[0], pair[1], small.ID)
		key := routeKey(pair[0], pair[1], large.ID)
		route := runner.stateCache.Routes[oldKey]
		route.ShipClass = large.ID
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
	}
	quote, err := runner.growthQuote(port, small)
	if err != nil {
		t.Fatal(err)
	}
	price := int64(129_000)
	quote.ShipClass, quote.TotalCostPence = large.ID, &price
	key := port.ID + "|" + large.ID
	runner.stateCache.Quotes[key] = quote
	runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+port.ID+"|"+small.ID]
	evaluate := func(hull Hull) GrowthDecision {
		t.Helper()
		evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
		if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
			t.Fatalf("capacity funding: %s, %v", reason, err)
		}
		input, reason, err := runner.capacityFleetInput(h.ctx, evidence)
		if err != nil || reason != "" {
			t.Fatalf("capacity economics: %s, %v", reason, err)
		}
		grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
		input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
		quote, err := runner.growthQuote(port, hull)
		if err != nil {
			t.Fatal(err)
		}
		decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, evidence)
		if err != nil {
			t.Fatal(err)
		}
		return decision
	}
	save := evaluate(large)
	required := growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence, save.WorkingCapitalPence)
	protected, err := runner.treasuryReserves(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range runner.stateCache.Vaults {
		region := runner.stateCache.Vaults[i].BankingRegion
		runner.stateCache.Vaults[i].BalancePence = protected[region]
		if region == port.BankingRegion {
			runner.stateCache.Vaults[i].BalancePence += required - 10_000
		}
	}
	buy, save := evaluate(small), evaluate(large)
	if !buy.Accepted || save.Accepted || !save.EconomicallyViable || save.CashShortfallPence != 10_000 || !runner.loanAuthority() {
		t.Fatalf("fixture needs affordable A and cash-only blocked B with loan authority: A=%+v B=%+v", buy, save)
	}
	principal, installment := estimatedCapacityPrincipal(save.CashShortfallPence, int64(len(runner.stateCache.Ships)))
	depth := save
	depth.IncrementalNetPencePerGameDay = save.DepthNetPencePerGameDay
	if loan := evaluateCapacityLoan(depth, save.CashShortfallPence, goldsmithsMaximumPence, principal, installment, 3_000); !loan.Borrow {
		t.Fatalf("B must be financing-eligible before the total-interest comparison: %+v", loan)
	}
	interest := 36*float64(installment) - float64(principal)
	if interest <= 0 {
		t.Fatalf("fixture needs positive total interest: principal=%d installment=%d", principal, installment)
	}
	// Make the wait cheaper than total interest, without approaching the P1
	// comparison boundary or relying on an undersized principal to block debt.
	wantRate := 4 * (buy.IncrementalNetPencePerGameDay + save.DepthNetPencePerGameDay) * float64(save.CashShortfallPence) / interest
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "savings-interest-income", Kind: "charter", CostsKnown: true,
		QuantityTons: 1, GameSeconds: 3_600, ProceedsPence: int64(math.Ceil(wantRate * 60)),
		CompletedAt: time.Now().UTC().Add(-13 * time.Hour),
	})
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now().UTC())
	if err != nil || rate <= 0 {
		t.Fatalf("fixture needs positive realized savings: rate=%v err=%v", rate, err)
	}
	if _, margin, wins := capacitySavingsMargin(buy, save, rate, 0); !wins || margin <= 0 {
		t.Fatalf("B must win the savings comparison: rate=%v margin=%v", rate, margin)
	}
	if capacitySavingsFinancingWorthwhile(buy, save, rate, len(runner.stateCache.Ships)) {
		t.Fatal("fixture must refuse financing because estimated total interest exceeds avoided-wait earnings")
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
		t.Fatalf("total-interest refusal must save rather than build: changed=%t err=%v", changed, err)
	}
	h.missingMission("__growth")
	h.missingMission("__treasury")
	if h.api.mutationCount("/v1/loan/quote") != 0 {
		t.Fatal("failed total-interest comparison created a loan quote GET")
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.PortID != port.ID || target.HullID != large.ID {
		t.Fatalf("total-interest refusal lost B savings: %+v", target)
	}
	history, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
	if err != nil {
		t.Fatal(err)
	}
	decisions, ok := history.([]store.Decision)
	if !ok {
		t.Fatalf("unexpected decision history %T", history)
	}
	for _, decision := range decisions {
		if decision.Kind != "capacity_financing:"+key || !strings.Contains(strings.ToLower(decision.Reason), "total interest") {
			continue
		}
		var loan capacityLoanDecision
		if err := json.Unmarshal(decision.Details, &loan); err != nil {
			t.Fatal(err)
		}
		if loan.Borrow || loan.ShortfallPence != save.CashShortfallPence {
			t.Fatalf("total-interest decision does not record B's no-borrow shortfall: %+v", loan)
		}
		return
	}
	t.Fatalf("missing stored capacity_financing:%s no-borrow decision explaining total interest", key)
}
