package engine

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacitySavingsEqualNetAffordableTargetLosesToBetterCandidate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		income bool
	}{
		{name: "positive savings rate", income: true},
		{name: "zero savings rate", income: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, _, _ := capacitySavingsRunnerFixture(t, test.income)
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			small, _ := findHull(runner.stateCache.Catalog, recoveryHull)
			const savedHull = "brigantine"
			// Identical operating characteristics force equal voyage earnings
			// and marginal upkeep; B retains its more expensive real yard quote.
			for i := range runner.stateCache.Catalog.ShipClasses {
				if runner.stateCache.Catalog.ShipClasses[i].ID == savedHull {
					runner.stateCache.Catalog.ShipClasses[i] = small
					runner.stateCache.Catalog.ShipClasses[i].ID = savedHull
				}
			}
			for i := range runner.stateCache.Vaults {
				runner.stateCache.Vaults[i].BalancePence += 2_000_000
			}
			evidence := &growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, evidence); err != nil || reason != "" {
				t.Fatalf("capacity funding: %s, %v", reason, err)
			}
			input, reason, err := runner.capacityFleetInput(h.ctx, evidence)
			if err != nil || reason != "" {
				t.Fatalf("capacity fleet: %s, %v", reason, err)
			}
			evaluate := func(hullID string) GrowthDecision {
				t.Helper()
				hull, found := findHull(runner.stateCache.Catalog, hullID)
				if !found {
					t.Fatalf("missing fixture hull %q", hullID)
				}
				grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
				candidateInput := input
				candidateInput.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
				quote, err := runner.growthQuote(port, hull)
				if err != nil {
					t.Fatal(err)
				}
				decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, candidateInput, evidence)
				if err != nil {
					t.Fatal(err)
				}
				return decision
			}
			buy, save := evaluate(small.ID), evaluate(savedHull)
			if !buy.Accepted || !save.Accepted || buy.CashShortfallPence != 0 || save.CashShortfallPence != 0 {
				t.Fatalf("A and B must both be affordable: A=%+v B=%+v", buy, save)
			}
			if buy.IncrementalNetPencePerGameDay != save.IncrementalNetPencePerGameDay || buy.IncrementalNetPencePerGameDay <= 0 {
				t.Fatalf("fixture must force exact positive equal net: A.net=%v B.net=%v", buy.IncrementalNetPencePerGameDay, save.IncrementalNetPencePerGameDay)
			}
			if !betterCapacityCandidate(buy, save) || betterCapacityCandidate(save, buy) {
				t.Fatalf("A must strictly win the existing capacity ranking: A=%+v B=%+v", buy, save)
			}
			rate, err := runner.capacitySavingsRate(h.ctx, evidence.asOf)
			if err != nil || test.income && rate <= 0 || !test.income && rate != 0 {
				t.Fatalf("unexpected savings rate: income=%t rate=%v err=%v", test.income, rate, err)
			}
			capital := growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence)
			target := capacitySavingsTarget{
				PortID: port.ID, HullID: savedHull,
				RequiredPence: growthCashTotal(capital, save.WorkingCapitalPence), CapitalPence: capital,
				DepthNetPencePerGameDay: save.DepthNetPencePerGameDay,
				DepthPreferenceShips:    save.DepthCompetition.PreferenceShips,
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}
			if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
				t.Fatal(err)
			}
			if live, err := runner.liveCapacitySavingsTarget(h.ctx); err != nil || live.PortID != port.ID || live.HullID != savedHull {
				t.Fatalf("B must have a live exact savings marker: target=%+v err=%v", live, err)
			}
			if changed, err := runner.considerCapacityGrowth(h.ctx, evidence); err != nil || !changed {
				t.Fatalf("equal-net A was not admitted: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			_, data := h.mission("__growth")
			if data.BuildPort != port.ID || data.BuildClass != small.ID || data.Capacity == nil || data.Capacity.Financing != nil || data.Action == nil || data.Action.Stage != "build" {
				t.Fatalf("live B displaced the better equal-net A: %+v", data)
			}
		})
	}
}

func TestCapacitySavingsRestrictedHandoffCannotFinanceCashBlockedTarget(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"normal capacity finances B", "restricted fresh B holds", "restricted unevaluated B holds"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			runner.stateCache.Clock.Calendar.TimeScale = 60
			runner.stateCache.Contracts = nil
			bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
			loanCapacityCashShortfall(t, h, runner, 100_000)
			// The cash refresh restores the API books; apply the same executable
			// depth economics to both the loan control and restricted handoff.
			runner.stateCache.Clock.Calendar.TimeScale = 60
			runner.stateCache.Contracts = nil
			bookCommodity(runner.stateCache.Markets[recoveryFarPort], recoveryGrain).Depth.Bids[0].PricePence = 50_000
			port, hull, candidate, _, evidence := loanCapacityDecision(t, h, runner)
			if candidate.Accepted || !candidate.EconomicallyViable || candidate.CashShortfallPence <= 0 || !runner.loanAuthority() {
				t.Fatalf("fresh B must be viable and cash-blocked with real loan authority: %+v", candidate)
			}
			rate, err := runner.capacitySavingsRate(h.ctx, evidence.asOf)
			maxWait := capacityHistoryWindow.Seconds() * float64(runner.stateCache.Clock.Calendar.TimeScale) / 86_400
			if err != nil || rate <= 0 || float64(candidate.CashShortfallPence)/rate > maxWait || !capacitySavingsFinancingWorthwhile(GrowthDecision{}, candidate, rate, len(runner.stateCache.Ships)) {
				t.Fatalf("B must pass the savings wait and financing gates: rate=%v candidate=%+v err=%v", rate, candidate, err)
			}
			capital := growthCashTotal(candidate.QuotedHullPence, candidate.SlippageReservePence, candidate.CannonsPence, candidate.InitialStoresPence)
			target := capacitySavingsTarget{
				PortID: port.ID, HullID: hull.ID,
				RequiredPence: growthCashTotal(evidence.available, candidate.CashShortfallPence), CapitalPence: capital,
				DepthNetPencePerGameDay: candidate.DepthNetPencePerGameDay,
				DepthPreferenceShips:    candidate.DepthCompetition.PreferenceShips,
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}
			if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
				t.Fatal(err)
			}
			if scenario == "restricted unevaluated B holds" {
				delete(runner.stateCache.Quotes, port.ID+"|"+hull.ID)
				delete(runner.meta, "yard_quotes/"+port.ID+"|"+hull.ID)
				if _, err := runner.growthQuote(port, hull); err == nil {
					t.Fatal("unevaluated restricted B must lack executable quote authority")
				}
			}
			before := h.api.mutationCount("/v1/loan/quote")
			var changed bool
			if scenario == "normal capacity finances B" {
				changed, err = runner.considerCapacityGrowth(h.ctx, evidence)
			} else {
				changed, err = runner.considerCapacityGrowthAt(h.ctx, evidence, port.ID, hull.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			quotes := h.api.mutationCount("/v1/loan/quote") - before
			if scenario == "normal capacity finances B" {
				_, data := h.mission("__growth")
				if !changed || quotes == 0 || data.BuildPort != port.ID || data.BuildClass != hull.ID || data.Capacity == nil || data.Capacity.Financing == nil || len(data.Capacity.Financing.Quotes) == 0 || data.Action != nil {
					t.Fatalf("normal capacity must quote and persist real B financing: changed=%t quotes=%d intent=%+v", changed, quotes, data)
				}
				return
			}
			if changed || quotes != 0 {
				t.Fatalf("restricted handoff started debt instead of holding: changed=%t quotes=%d", changed, quotes)
			}
			h.missingMission("__growth")
			h.missingMission("__treasury")
			if h.api.mutationCount("/v1/loan/accept-quote") != 0 || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("restricted handoff sent a loan acceptance or build")
			}
			retained := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
			if retained.PortID != target.PortID || retained.HullID != target.HullID {
				t.Fatalf("restricted handoff lost B's savings marker: %+v", retained)
			}
			if committed := decode[string](runner.snapshot, "_capacity_growth_port"); committed != "" {
				t.Fatalf("restricted cash-blocked B committed growth port %q", committed)
			}
			history, err := h.db.History(h.ctx, h.account.ID, "decisions", 300)
			if err != nil {
				t.Fatal(err)
			}
			decisions, ok := history.([]store.Decision)
			if !ok {
				t.Fatalf("unexpected decision history %T", history)
			}
			foundHold := false
			for _, decision := range decisions {
				foundHold = foundHold || decision.Kind == "hold" && decision.Reason == capacitySavingsReason
				if scenario == "restricted unevaluated B holds" && decision.Kind == "capacity_growth" {
					t.Fatalf("unevaluated restricted pass recorded a generic capacity decision: %+v", decision)
				}
			}
			if !foundHold {
				t.Fatal("restricted handoff must record its savings hold for the next lane pass")
			}
		})
	}
}

func TestCapacitySavingsInvalidatedStagePreservesAffordableTarget(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		invalidate func(*Runner)
		holdReason string
	}{
		{
			name:       "financial stream invalidation",
			invalidate: func(runner *Runner) { runner.publishedFinancial.Add(1) },
			holdReason: "financial event invalidated the captured funds; authoritative resynchronization precedes staging",
		},
		{
			name:       "control stream invalidation",
			invalidate: func(runner *Runner) { runner.publishedControl.Add(1) },
			holdReason: "stream control invalidated the captured state; authoritative resynchronization precedes staging",
		},
		{name: "uninvalidated durable build"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
			h.seedLaneProofs()
			for i := range runner.stateCache.Vaults {
				runner.stateCache.Vaults[i].BalancePence += 2_000_000
			}
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			hull, _ := findHull(runner.stateCache.Catalog, "brigantine")
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("lane proof and funding must be valid: %s, %v", reason, err)
			}
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" {
				t.Fatalf("capacity fleet must be valid: %s, %v", reason, err)
			}
			grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, hull))
			input.IncrementalDailyUpkeepPence = DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) -
				DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
			quote, err := runner.growthQuote(port, hull)
			if err != nil {
				t.Fatal(err)
			}
			save, required, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
			if err != nil || !save.Accepted || save.CashShortfallPence != 0 || save.FundingFeesPence != 0 {
				t.Fatalf("saved B must be executable with local cash and no remittance: %+v err=%v", save, err)
			}
			for region, amount := range required {
				if evidence.balances[region] < amount {
					t.Fatalf("B lacks regional cash in %s: have=%d need=%d", region, evidence.balances[region], amount)
				}
			}
			pack, feasible, err := runner.growthAtYard(h.ctx, port, hull, quote, input.IncrementalDailyUpkeepPence, &evidence, nil)
			if err != nil || !feasible || !pack.plan.Accepted {
				t.Fatalf("next lane evaluation must have a genuinely executable alternative: %+v feasible=%t err=%v", pack, feasible, err)
			}
			capital := growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence)
			target := capacitySavingsTarget{
				PortID: port.ID, HullID: hull.ID,
				RequiredPence: growthCashTotal(capital, save.WorkingCapitalPence), CapitalPence: capital,
				DepthNetPencePerGameDay: save.DepthNetPencePerGameDay,
				DepthPreferenceShips:    save.DepthCompetition.PreferenceShips,
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}
			if evidence.available < target.RequiredPence {
				t.Fatalf("saved B lacks global cash: have=%d need=%d", evidence.available, target.RequiredPence)
			}
			if runner.capturedControl != runner.publishedControl.Load() || runner.capturedFinancial != runner.publishedFinancial.Load() {
				t.Fatal("fixture must begin with coherent captured stream epochs")
			}
			if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
				t.Fatal(err)
			}
			assertSavingsState := func(staged bool) {
				t.Helper()
				persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, snapshot := range []map[string]json.RawMessage{runner.snapshot, persisted} {
					retained := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target")
					committed := decode[string](snapshot, "_capacity_growth_port")
					if staged {
						if _, exists := snapshot["_capacity_savings_target"]; exists || committed != target.PortID {
							t.Fatalf("durable B build must clear its marker and publish its port: target=%+v port=%q", retained, committed)
						}
					} else if retained != target || committed != "" {
						t.Fatalf("unstaged B must retain its exact durable marker without a committed port: target=%+v want=%+v port=%q", retained, target, committed)
					}
				}
			}
			assertSavingsState(false)
			if test.invalidate != nil {
				test.invalidate(runner)
			}
			changed, err := runner.considerCapacityGrowth(h.ctx, &evidence)
			if test.invalidate == nil {
				if err != nil || !changed {
					t.Fatalf("coherent saved B did not stage: changed=%t err=%v holds=%v", changed, err, runner.holding)
				}
				mission, data := h.mission("__growth")
				if mission.Phase != "growth_build" || data.BuildPort != target.PortID || data.BuildClass != target.HullID ||
					data.Capacity == nil || data.Capacity.Financing != nil || data.Action == nil ||
					data.Action.Stage != "build" || data.Action.Path != "/v1/shipyard/build-ship" {
					t.Fatalf("positive control must persist a real cash B build action: mission=%+v data=%+v", mission, data)
				}
				assertSavingsState(true)
				if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
					t.Fatal("staging must not send the build before dispatch")
				}
				return
			}
			for evaluation := range 2 {
				if err != nil || changed {
					t.Fatalf("invalidated evaluation %d reported a staged build: changed=%t err=%v holds=%v", evaluation, changed, err, runner.holding)
				}
				reachedStageFence := false
				for _, hold := range runner.holding {
					reachedStageFence = reachedStageFence || hold.subjectID == "__growth" && hold.Reason == test.holdReason
				}
				if !reachedStageFence {
					t.Fatalf("evaluation %d must reach the stage invalidation fence, not an earlier authority hold: %v", evaluation, runner.holding)
				}
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
					t.Fatal("invalidated admission sent a build")
				}
				assertSavingsState(false)
				if evaluation == 0 {
					runner.holding = nil
					changed, err = runner.considerGrowth(h.ctx)
				}
			}
		})
	}
}
