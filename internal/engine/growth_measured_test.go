package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const capacityMeasuredTestHull = "measured-brig"

func capacityMeasuredFixture(t *testing.T, scale int64, ages ...time.Duration) (*recoveryHarness, *Runner, time.Time) {
	t.Helper()
	h := newRecoveryHarness(t)
	asOf := time.Now().UTC().Truncate(time.Second)
	rows := make([]map[string]any, 0, len(ages))
	for i, age := range ages {
		row := sailingShipRow(fmt.Sprintf("measured-hull-%d", i), fmt.Sprintf("Hull %d", i), recoveryHomePort, recoveryFarPort)
		row["ship"].(map[string]any)["created_at"] = asOf.Add(-age)
		rows = append(rows, row)
	}
	h.api.setFleet(rows)
	runner := growthWindowRunner(t, h)
	runner.stateCache.Clock.Calendar.TimeScale = scale
	hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
	hull.ID = capacityMeasuredTestHull
	runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
	for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
		oldKey := routeKey(pair[0], pair[1], recoveryHull)
		key := routeKey(pair[0], pair[1], hull.ID)
		route := runner.stateCache.Routes[oldKey]
		route.ShipClass = hull.ID
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
	}
	for _, portID := range []string{recoveryHomePort, recoveryFarPort} {
		oldKey := portID + "|" + recoveryHull
		if quote, exists := runner.stateCache.Quotes[oldKey]; exists {
			quote.ShipClass = hull.ID
			key := portID + "|" + hull.ID
			runner.stateCache.Quotes[key] = quote
			runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+oldKey]
		}
	}
	return h, runner, asOf
}

func capacityMeasuredEvidence(t *testing.T, h *recoveryHarness, runner *Runner, asOf time.Time) growthEvidence {
	t.Helper()
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
		t.Fatalf("measured growth cash: %s, %v", reason, err)
	}
	evidence.asOf = asOf
	return evidence
}

func TestCapacityMeasuredWindowUsesAuthoritativeTimeScale(t *testing.T) {
	t.Parallel()
	for _, scale := range []int64{30, 60, 120} {
		t.Run(fmt.Sprintf("scale_%d", scale), func(t *testing.T) {
			h, runner, asOf := capacityMeasuredFixture(t, scale, 48*time.Hour)
			window := 30 * 24 * time.Hour / time.Duration(scale)
			for _, row := range []store.Settlement{
				{ID: "window-start", Kind: "spot", CostsKnown: true, QuantityTons: 1, ProceedsPence: 150_000, GameSeconds: 1, CompletedAt: asOf.Add(-window)},
				{ID: "window-end", Kind: "spot", CostsKnown: true, QuantityTons: 1, ProceedsPence: 180_000, FeesPence: 10_000, GameSeconds: 1, CompletedAt: asOf},
				{ID: "window-loss", Kind: "aborted", CostsKnown: true, AcquisitionPence: 20_000, GameSeconds: 1, CompletedAt: asOf.Add(-window / 2)},
				{ID: "old-profit", Kind: "spot", CostsKnown: true, QuantityTons: 1, ProceedsPence: 90_000_000, GameSeconds: 1, CompletedAt: asOf.Add(-window - time.Second)},
				{ID: "old-unknown", Kind: "aborted", AcquisitionPence: 90_000_000, GameSeconds: 1, CompletedAt: asOf.Add(-window - time.Second)},
			} {
				persistGrowthWindowSettlement(t, h, row)
			}
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" || input.SampleHullGameDays != 30 || input.MeasuredRatePencePerGameDay != 10_000 {
				t.Fatalf("30-game-day measurement at %dx: input %+v reason %q error %v", scale, input, reason, err)
			}
		})
	}
}

func TestCapacityMeasuredHullDaysClipCreationAndExcludeSunk(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		ages       []time.Duration
		sunk       bool
		wantSample float64
		wantFleet  int
		wantHold   bool
	}{
		{name: "six real hours is fifteen game days", ages: []time.Duration{6 * time.Hour}, wantSample: 15, wantFleet: 1, wantHold: true},
		{name: "old hull clips to window", ages: []time.Duration{48 * time.Hour}, wantSample: 30, wantFleet: 1},
		{name: "new hull and sunk hull do not add days", ages: []time.Duration{48 * time.Hour, 6 * time.Hour, 0, 48 * time.Hour}, sunk: true, wantSample: 45, wantFleet: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, asOf := capacityMeasuredFixture(t, 60, test.ages...)
			if test.sunk {
				lost := &runner.stateCache.Ships[len(runner.stateCache.Ships)-1]
				lost.Ship.Status = "sunk"
				updated := asOf.Add(-25 * time.Hour)
				lost.Ship.UpdatedAt = &updated
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "measured-income", Kind: "spot", CostsKnown: true, QuantityTons: 1,
				ProceedsPence: int64(test.wantSample * 10_000), GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
			})
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" || input.FleetSize != test.wantFleet || input.SampleHullGameDays != test.wantSample ||
				input.MeasuredRatePencePerGameDay != 10_000 || input.MeasuredNetAvailable == test.wantHold {
				t.Fatalf("live hull exposure: %+v reason %q error %v; want fleet %d sample %v measured available %t", input, reason, err, test.wantFleet, test.wantSample, !test.wantHold)
			}
		})
	}
}

func TestCapacityMeasuredSampleFloor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		age  time.Duration
		hold bool
	}{
		{name: "23.9 hull-game-days holds", age: 24*24*time.Hour/60 - 144*time.Second, hold: true},
		{name: "24 hull-game-days proceeds", age: 24 * 24 * time.Hour / 60},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, runner, asOf := capacityMeasuredFixture(t, 60, test.age)
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "floor-income", Kind: "spot", CostsKnown: true, QuantityTons: 1,
				ProceedsPence: 24_000_000, GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
			})
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			wantSample := 24.0
			if test.hold {
				wantSample = 23.9
			}
			if err != nil || reason != "" || math.Abs(input.SampleHullGameDays-wantSample) > 1e-9 || input.MeasuredNetAvailable == test.hold {
				t.Fatalf("sample boundary: %+v reason %q error %v", input, reason, err)
			}
			changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, capacityMeasuredTestHull)
			if err != nil || changed == test.hold {
				t.Fatalf("sample boundary admission: changed %t error %v holds %v", changed, err, runner.holding)
			}
			if test.hold {
				h.missingMission("__growth")
				requireReviewHold(t, runner, capacityMeasuredNetReason, true)
			} else {
				_, data := h.mission("__growth")
				if data.Capacity == nil || data.Action == nil || data.Action.Stage != "build" {
					t.Fatalf("measured sample did not admit a real capacity intent: %+v", data)
				}
			}
		})
	}
}

func TestCapacityMeasuredUncertaintyHolds(t *testing.T) {
	t.Parallel()
	for _, future := range []bool{false, true} {
		t.Run(fmt.Sprintf("future_completion_%t", future), func(t *testing.T) {
			h, runner, asOf := capacityMeasuredFixture(t, 60, 24*time.Hour)
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "known-income", Kind: "spot", CostsKnown: true, QuantityTons: 1,
				ProceedsPence: 30_000_000, GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
			})
			completed := asOf.Add(-time.Minute)
			if future {
				completed = asOf.Add(time.Minute)
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "uncertain-row", Kind: "aborted", CostsKnown: future,
				GameSeconds: 1, CompletedAt: completed,
			})
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, capacityMeasuredTestHull); err != nil || changed {
				t.Fatalf("uncertain measurement admitted growth: changed %t error %v", changed, err)
			}
			h.missingMission("__growth")
			reason := capacityMeasuredNetReason
			if future {
				reason = "capacity growth rejects future-dated settlement evidence"
			}
			requireReviewHold(t, runner, reason, !future)
		})
	}
}

func TestCapacityMeasuredRateCapsPlannerAndDepthRates(t *testing.T) {
	t.Parallel()
	for _, measured := range []int64{5_000, 10_000_000, -5_000} {
		t.Run(fmt.Sprintf("measured_%d", measured), func(t *testing.T) {
			h, runner, asOf := capacityMeasuredFixture(t, 60, 24*time.Hour)
			runner.stateCache.Contracts = nil
			row := store.Settlement{
				ID: "rate-income", Kind: "spot", CostsKnown: true, QuantityTons: 1,
				GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
			}
			if measured >= 0 {
				row.ProceedsPence = measured * 30
			} else {
				row.AcquisitionPence = -measured * 30
			}
			persistGrowthWindowSettlement(t, h, row)
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" {
				t.Fatalf("measured input: %q %v", reason, err)
			}
			input.IncrementalDailyUpkeepPence = 100
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			hull, _ := findHull(runner.stateCache.Catalog, capacityMeasuredTestHull)
			quote, err := runner.growthQuote(port, hull)
			if err != nil {
				t.Fatal(err)
			}
			planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(port, hull), "__growth")
			if err != nil {
				t.Fatal(err)
			}
			planning.FleetDailyUpkeepPence = 0
			var predicted float64
			for _, plan := range PlanVoyages(planning) {
				if plan.Accepted && plan.ContractID == "" && len(plan.FreightIDs) == 0 && plan.Economics.RevenueBasis == "depth" {
					predicted = max(predicted, capacityVoyageRate(plan.Economics))
				}
			}
			if predicted <= 5_000 || predicted >= 10_000_000 {
				t.Fatalf("fixture must straddle measured rates, predicted %v", predicted)
			}
			decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
			used := min(predicted, float64(measured))
			if err != nil || (decision.Accepted != (measured > 0)) {
				t.Fatalf("measured rate admission: %+v, %v", decision, err)
			}
			if measured > 0 && (decision.IncrementalNetPencePerGameDay != used-100 || decision.DepthNetPencePerGameDay != used-100) {
				t.Fatalf("candidate or debt-backed depth escaped measured cap %v: %+v", used, decision)
			}
			rows, err := h.db.History(h.ctx, h.account.ID, "decisions", 100)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, recorded := range rows.([]store.Decision) {
				if recorded.Kind != "capacity_candidate:"+port.ID+"|"+hull.ID {
					continue
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(recorded.Details, &raw); err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]float64{
					"predicted_rate_pence_per_game_day": predicted,
					"measured_rate_pence_per_game_day":  float64(measured),
					"used_rate_pence_per_game_day":      used,
					"sample_hull_game_days":             30,
				} {
					var value float64
					if err := json.Unmarshal(raw[key], &value); err != nil {
						t.Fatalf("decode %s: %v", key, err)
					}
					if value != want {
						t.Errorf("decision %s = %v, want %v", key, value, want)
					}
				}
				found = true
			}
			if !found {
				t.Fatal("measured candidate decision was not persisted")
			}
		})
	}
}

func TestCapacityMeasuredRetainedWorkDoesNotBecomeDepth(t *testing.T) {
	t.Parallel()
	h, runner, asOf := capacityMeasuredFixture(t, 60, 24*time.Hour)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "retained-work-income", Kind: "spot", CostsKnown: true, QuantityTons: 1,
		ProceedsPence: 30_000_000, GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
	})
	runner.stateCache.Contracts = nil
	delete(runner.stateCache.Markets, recoveryFarPort)
	delete(runner.meta, "books/"+recoveryFarPort)
	runner.snapshot["_quay_book_memory"] = encode(map[string]RetainedQuayBook{
		recoveryFarPort: {
			ObservedAt: runner.stateCache.Clock.RealUTC.Add(-time.Hour),
			Bids: map[string][]RetainedBid{
				recoveryGrain: {{PricePence: 50_000, RemainingQuantity: 100}},
			},
		},
	})
	evidence := capacityMeasuredEvidence(t, h, runner, asOf)
	input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
	if err != nil || reason != "" {
		t.Fatalf("measured fleet input: %q %v", reason, err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	hull, _ := findHull(runner.stateCache.Catalog, capacityMeasuredTestHull)
	quote, err := runner.growthQuote(port, hull)
	if err != nil {
		t.Fatal(err)
	}
	planning, err := runner.committedPlanningInputExcept(h.ctx, growthCandidate(port, hull), "__growth")
	if err != nil {
		t.Fatal(err)
	}
	planning.FleetDailyUpkeepPence = 0
	retainedWork := false
	for _, plan := range PlanVoyages(planning) {
		if !plan.Accepted || plan.ContractID != "" || len(plan.FreightIDs) > 0 {
			continue
		}
		if plan.Economics.RevenueBasis != "retained" {
			t.Fatalf("fixture must supply only retained-basis spare work: %+v", plan)
		}
		retainedWork = true
	}
	if !retainedWork {
		t.Fatal("fixture did not yield executable retained-basis work")
	}
	decision, _, err := runner.capacityAtYard(h.ctx, port, hull, quote, input, &evidence)
	if err != nil || !decision.Accepted || decision.DepthNetPencePerGameDay != 0 {
		t.Fatalf("retained spare work was ignored or counted as debt-backed depth: %+v, %v", decision, err)
	}
}

func TestCapacityMeasuredHamburgHoyPaybackBoundary(t *testing.T) {
	t.Parallel()
	for _, rate := range []float64{8_940, 8_941} {
		input := CapacityGrowthInput{
			Hull:           Hull{ID: "hoy", HoldCapacityTons: 40, MaxCannons: 4},
			HullPricePence: 2_264_500, InitialStoresPence: 70_000,
			SurplusPence: 10_000_000, FleetSize: 1, LotCashPence: 100_000,
			IncrementalDailyUpkeepPence: 2_700, MaxPaybackDays: 420,
			CandidateRatePencePerGameDay: rate, HasSpareWork: true,
		}
		decision := EvaluateCapacityGrowth(input)
		if decision.Accepted != (rate == 8_941) {
			t.Fatalf("Hamburg hoy at %.0fp/hull-gd: %+v", rate, decision)
		}
		if decision.SlippageReservePence != 226_450 || decision.CannonsPence != 60_000 || decision.InitialStoresPence != 70_000 ||
			decision.QuotedHullPence+decision.SlippageReservePence+decision.CannonsPence+decision.InitialStoresPence != 2_620_950 ||
			decision.IncrementalUpkeepPence != 2_700 {
			t.Fatalf("Hamburg hoy capital or upkeep differs from quote fixture: %+v", decision)
		}
	}
}

func TestCapacityMeasuredCheapHullReinvestmentNeverWaitsForEarnings(t *testing.T) {
	t.Parallel()
	for _, hullID := range []string{"hoy", "cutter", "brigantine", "collier"} {
		for _, measurement := range []string{"missing", "zero", "low", "negative"} {
			t.Run(hullID+"/"+measurement, func(t *testing.T) {
				age := 24 * time.Hour
				if measurement == "missing" {
					age = time.Hour
				}
				h, runner, asOf := capacityMeasuredFixture(t, 60, age)
				if measurement != "missing" {
					row := store.Settlement{
						ID: "cheap-hull-earnings", Kind: "spot", CostsKnown: true, QuantityTons: 1,
						GameSeconds: 1, CompletedAt: asOf.Add(-30 * time.Minute),
					}
					switch measurement {
					case "low":
						row.ProceedsPence = 30
					case "negative":
						row.AcquisitionPence = 300_000
					}
					persistGrowthWindowSettlement(t, h, row)
				}
				if hullID != recoveryHull {
					hull, _ := findHull(runner.stateCache.Catalog, recoveryHull)
					hull.ID = hullID
					runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, hull)
					for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
						oldKey := routeKey(pair[0], pair[1], recoveryHull)
						key := routeKey(pair[0], pair[1], hull.ID)
						route := runner.stateCache.Routes[oldKey]
						route.ShipClass = hull.ID
						runner.stateCache.Routes[key] = route
						runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
					}
					oldKey := recoveryHomePort + "|" + recoveryHull
					quote := runner.stateCache.Quotes[oldKey]
					quote.ShipClass = hull.ID
					key := recoveryHomePort + "|" + hull.ID
					runner.stateCache.Quotes[key] = quote
					runner.meta["yard_quotes/"+key] = runner.meta["yard_quotes/"+oldKey]
				}
				evidence := capacityMeasuredEvidence(t, h, runner, asOf)
				if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, hullID); err != nil || !changed {
					t.Fatalf("cheap reinvestment was blocked by %s measured earnings: changed %t error %v holds %v", measurement, changed, err, runner.holding)
				}
				mission, data := h.mission("__growth")
				if data.Capacity == nil || data.BuildClass != hullID || data.Action == nil || data.Action.Stage != "build" {
					t.Fatalf("cheap reinvestment did not stage the selected hull: %+v", data)
				}
				if err := runner.newActionPreflight(h.ctx, mission, &data); err != nil {
					t.Fatalf("cheap %s preflight reintroduced an earnings gate: %v", hullID, err)
				}
			})
		}
	}
}

func TestCapacityMeasuredCheapHullFinancingDoesNotWaitForEarnings(t *testing.T) {
	t.Parallel()
	for _, measurement := range []string{"missing", "zero", "negative"} {
		t.Run(measurement, func(t *testing.T) {
			h, runner := loanCapacityFixture(t, 750_000, false)
			loanCapacityCashShortfall(t, h, runner, 100_000)
			asOf := time.Now().UTC()
			window, err := runner.db.EconomicHistoryWindow(h.ctx, runner.account.ID, "", asOf, 30*24*time.Hour)
			if err != nil || window.NetPence <= 0 {
				t.Fatalf("financing fixture lacks positive measured net: %+v, %v", window, err)
			}
			loss := window.NetPence
			if measurement == "negative" {
				loss += 300_000
			}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "cheap-finance-loss", Kind: "aborted", CostsKnown: true,
				AcquisitionPence: loss, GameSeconds: 1, CompletedAt: asOf.Add(-time.Minute),
			})
			if measurement == "missing" {
				runner.stateCache.Ships[0].Ship.CreatedAt = nil
			}
			evidence := capacityMeasuredEvidence(t, h, runner, asOf)
			input, reason, err := runner.capacityFleetInput(h.ctx, &evidence)
			if err != nil || reason != "" || input.MeasuredRatePencePerGameDay > 0 ||
				measurement == "missing" && input.MeasuredNetAvailable {
				t.Fatalf("expected %s measured evidence: %+v reason %q error %v", measurement, input, reason, err)
			}
			if changed, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil || !changed {
				t.Fatalf("cheap financing was blocked by %s measured earnings: changed %t error %v holds %v", measurement, changed, err, runner.holding)
			}
			_, data := h.mission("__growth")
			if data.Capacity == nil || data.Capacity.Financing == nil || data.BuildClass != recoveryHull {
				t.Fatalf("cheap reinvestment did not stage recourse financing: %+v", data)
			}
		})
	}
}

func TestCapacityMeasuredFutureCompletionVetoesCheapHull(t *testing.T) {
	t.Parallel()
	h, runner, asOf := capacityMeasuredFixture(t, 60, 24*time.Hour)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "future-cheap-earnings", Kind: "spot", CostsKnown: true, QuantityTons: 1,
		ProceedsPence: 3_000_000, GameSeconds: 1, CompletedAt: asOf.Add(time.Minute),
	})
	evidence := capacityMeasuredEvidence(t, h, runner, asOf)
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, recoveryHull); err != nil || changed {
		t.Fatalf("future-dated settlement did not veto cheap growth: changed %t error %v", changed, err)
	}
	h.missingMission("__growth")
	requireReviewHold(t, runner, "capacity growth rejects future-dated settlement evidence", false)
}

func TestCapacityMeasuredLowNetReleasesUnsupportedSavingsForCheapHull(t *testing.T) {
	t.Parallel()
	h, runner, asOf := capacityMeasuredFixture(t, 60, 24*time.Hour)
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "low-measured-savings", Kind: "spot", CostsKnown: true, QuantityTons: 1,
		ProceedsPence: 150_000, GameSeconds: 1, CompletedAt: asOf.Add(-time.Hour),
	})
	evidence := capacityMeasuredEvidence(t, h, runner, asOf)
	if err := runner.setCapacitySavingsTarget(h.ctx, capacitySavingsTarget{
		PortID: recoveryFarPort, HullID: capacityMeasuredTestHull,
		CapitalPence: 2_620_950, RequiredPence: evidence.available - 1,
		DepthNetPencePerGameDay: 100_000, ExpiresAt: asOf.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := runner.considerCapacityGrowthAt(h.ctx, &evidence, recoveryHomePort, recoveryHull); err != nil || !changed {
		t.Fatalf("unsupported saved target blocked cheap reinvestment: changed %t error %v holds %v", changed, err, runner.holding)
	}
	_, data := h.mission("__growth")
	if data.Capacity == nil || data.BuildClass != recoveryHull {
		t.Fatalf("unsupported target displaced the cheap cash build: %+v", data)
	}
	if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
		t.Fatalf("low measured net retained an inadmissible save-up target: %+v", target)
	}
}
