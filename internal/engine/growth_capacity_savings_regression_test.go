package engine

import (
	"net/http"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func TestCapacitySavingsFreshYardObservationReleasesTarget(t *testing.T) {
	t.Parallel()
	for _, observation := range []string{"unavailable quote", "build backoff", "no longer winning"} {
		t.Run(observation, func(t *testing.T) {
			h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID == "" {
				t.Fatal("positive control did not establish a savings target")
			}
			key := recoveryHomePort + "|savings-large-hull"
			switch observation {
			case "unavailable quote":
				quote := runner.stateCache.Quotes[key]
				quote.Available = false
				runner.stateCache.Quotes[key] = quote
			case "build backoff":
				runner.snapshot["_growth_build_backoffs"] = encode(map[string]time.Time{key: time.Now().Add(time.Hour)})
			case "no longer winning":
				runner.stateCache.Catalog.ShipClasses[1].HoldCapacityTons = runner.stateCache.Catalog.ShipClasses[0].HoldCapacityTons
			}
			if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
				t.Fatalf("fresh negative observation did not release the affordable build: changed=%t err=%v", changed, err)
			}
			_, data := h.mission("__growth")
			if data.BuildClass != recoveryHull || data.Action == nil || data.Action.Path != "/v1/shipyard/build-ship" {
				t.Fatalf("fresh negative observation did not select the small hull: %+v", data)
			}
			if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
				t.Fatalf("fresh negative observation retained the target: %+v", target)
			}
		})
	}
}

func TestCapacitySavingsBuildsTargetWhenAffordable(t *testing.T) {
	t.Parallel()
	h, runner, _, save := capacitySavingsRunnerFixture(t, true)
	if _, err := runner.considerGrowth(h.ctx); err != nil {
		t.Fatal(err)
	}
	port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
	for i := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[i].BankingRegion == port.BankingRegion {
			runner.stateCache.Vaults[i].BalancePence += save.CashShortfallPence
		}
	}
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("newly affordable target was not admitted: changed=%t err=%v", changed, err)
	}
	_, data := h.mission("__growth")
	if data.BuildClass != "savings-large-hull" || data.Action == nil || data.Action.Path != "/v1/shipyard/build-ship" {
		t.Fatalf("saving paid its wait but then selected a smaller hull: %+v", data)
	}
	if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
		t.Fatalf("affordable fresh target retained a savings marker: %+v", target)
	}
}

func TestCapacitySavingsAffordableAbsentTargetNeedsDestination(t *testing.T) {
	t.Parallel()
	for _, destination := range []string{"voyage", "committed mission", "neither"} {
		t.Run(destination, func(t *testing.T) {
			h, runner, _, save := capacitySavingsRunnerFixture(t, true)
			quote := runner.stateCache.Quotes[recoveryHomePort+"|savings-large-hull"]
			quote.Available = false
			runner.stateCache.Quotes[recoveryHomePort+"|savings-large-hull"] = quote
			runner.stateCache.Factors = []string{recoveryHomePort}
			if destination != "voyage" {
				ship := &runner.stateCache.Ships[0]
				ship.RouteLegs[len(ship.RouteLegs)-1].ToNode = recoveryHomePort
			}
			if destination == "committed mission" {
				h.putMission(recoveryShipBlastoise, "savings-committed-arrival", "sailing", missionData{
					Plan: Plan{Kind: "spot", ShipID: recoveryShipBlastoise, Origin: recoveryHomePort, Destination: recoveryFarPort},
				})
			}
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("absent-yard funding: %s, %v", reason, err)
			}
			runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
				PortID: recoveryFarPort, HullID: "savings-large-hull", RequiredPence: evidence.available,
				CapitalPence:            growthCashTotal(save.QuotedHullPence, save.SlippageReservePence, save.CannonsPence, save.InitialStoresPence),
				DepthNetPencePerGameDay: save.DepthNetPencePerGameDay, ExpiresAt: time.Now().Add(time.Hour),
			})
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			if destination != "neither" {
				h.missingMission("__growth")
				if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != recoveryFarPort {
					t.Fatalf("committed arrival did not retain the affordable target: %+v", target)
				}
				return
			}
			_, data := h.mission("__growth")
			if data.BuildClass != recoveryHull || data.Action == nil {
				t.Fatalf("unreachable affordable target did not release the small build: %+v", data)
			}
			if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
				t.Fatalf("unreachable affordable target remained live: %+v", target)
			}
		})
	}
}

func TestCapacitySavingsExpansionAgentOnlyCannotSave(t *testing.T) {
	t.Parallel()
	h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{{PortID: recoveryHomePort, HiredAt: time.Now()}})
	if changed, err := runner.considerGrowth(h.ctx); err != nil || !changed {
		t.Fatalf("agent-only candidate blocked the affordable hull: changed=%t err=%v", changed, err)
	}
	_, data := h.mission("__growth")
	if data.BuildClass != recoveryHull || data.Action == nil {
		t.Fatalf("agent-only yard became a savings target: %+v", data)
	}
	if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.PortID != "" {
		t.Fatalf("agent-only presence retained a savings target: %+v", target)
	}
}

func TestCapacitySavingsFinancingRefusalKeepsSaving(t *testing.T) {
	t.Parallel()
	h, runner, buy, save := capacitySavingsRunnerFixture(t, true)
	// Reach the loan minimum without moving the savings comparison near its boundary.
	// Keep this income inside the savings window but outside the lane-proof window.
	persistGrowthWindowSettlement(t, h, store.Settlement{
		ID: "savings-financing-income", Kind: "charter", CostsKnown: true,
		QuantityTons: 1, GameSeconds: 3_600, ProceedsPence: 600_000_000,
		CompletedAt: time.Now().Add(-13 * time.Hour),
	})
	key := recoveryHomePort + "|savings-large-hull"
	quote := runner.stateCache.Quotes[key]
	price := *quote.TotalCostPence + 9_000
	quote.TotalCostPence = &price
	runner.stateCache.Quotes[key] = quote
	oldCapital := growthCashTotal(save.QuotedHullPence, save.SlippageReservePence)
	oldBuffer := max(runner.opts.ReservePence, ceilBPS(save.QuotedHullPence, 1_500))
	save.QuotedHullPence, save.SlippageReservePence = price, ceilBPS(price, 1_000)
	save.CashShortfallPence += growthCashTotal(price, save.SlippageReservePence) - oldCapital +
		max(runner.opts.ReservePence, ceilBPS(price, 1_500)) - oldBuffer
	rate, err := runner.capacitySavingsRate(h.ctx, time.Now())
	if _, margin, wins := capacitySavingsMargin(buy, save, rate, 0); err != nil || !wins {
		t.Fatalf("financing fixture must still prefer saving: margin=%v err=%v", margin, err)
	}
	runner.snapshot["loans"] = encode([]any{})
	runner.meta["loans"] = runner.meta["ships"]
	downstream := h.api.server.Config.Handler
	h.api.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/loan/quote" {
			h.api.mu.Lock()
			h.api.record(request, nil, http.StatusBadRequest)
			h.api.mu.Unlock()
			h.api.writeProblem(writer, http.StatusBadRequest, "principal_cap", "the exact lien cannot fund this package")
			return
		}
		downstream.ServeHTTP(writer, request)
	})
	for range 2 {
		if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
			t.Fatalf("quoted financing refusal spent the savings: changed=%t err=%v", changed, err)
		}
		h.missingMission("__growth")
		if target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); target.HullID != "savings-large-hull" {
			t.Fatalf("quoted financing refusal erased the winning target: %+v", target)
		}
	}
	if h.api.mutationCount("/v1/loan/quote") != 1 ||
		!time.Now().Before(decode[map[string]time.Time](runner.snapshot, "_capacity_financing_backoffs")[key]) ||
		time.Now().Before(decode[map[string]time.Time](runner.snapshot, "_growth_build_backoffs")[key]) {
		t.Fatal("financing refusal did not back off independently of cash-funded growth")
	}
}

func TestCapacitySavingsFinancingInterestComparison(t *testing.T) {
	t.Parallel()
	buy := GrowthDecision{IncrementalNetPencePerGameDay: 10_000}
	save := GrowthDecision{CashShortfallPence: 20_000, DepthNetPencePerGameDay: 20_000}
	principal, installment := estimatedCapacityPrincipal(save.CashShortfallPence, 1)
	interest := 36*float64(installment) - float64(principal)
	if interest <= 0 {
		t.Fatalf("fixture requires positive total interest: principal=%d installment=%d", principal, installment)
	}
	boundaryRate := (buy.IncrementalNetPencePerGameDay + save.DepthNetPencePerGameDay) *
		float64(save.CashShortfallPence) / interest
	for _, candidate := range []struct {
		name string
		rate float64
		want bool
	}{
		{"avoided wait exceeds interest", boundaryRate * 0.99, true},
		{"interest exceeds avoided wait", boundaryRate * 1.01, false},
		{"nonpositive savings rate", 0, false},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			if got := capacitySavingsFinancingWorthwhile(buy, save, candidate.rate, 1); got != candidate.want {
				t.Fatalf("worth financing = %t, want %t", got, candidate.want)
			}
		})
	}
}

func TestCapacitySavingsLanePackageMustBeatLiveTarget(t *testing.T) {
	t.Parallel()
	for _, saving := range []bool{false, true} {
		t.Run(map[bool]string{false: "lane positive control", true: "live target holds lane"}[saving], func(t *testing.T) {
			h := newRecoveryHarness(t)
			row := sailingShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, recoveryFarPort)
			row["ship"].(map[string]any)["created_at"] = time.Now().Add(-30 * time.Hour)
			h.api.setFleet([]map[string]any{row})
			h.seedLaneProofs()
			runner := growthWindowRunner(t, h)
			runner.stateCache.Factors = []string{recoveryHomePort}
			persistGrowthWindowSettlement(t, h, store.Settlement{
				ID: "savings-lane-charter-income", Kind: "charter", CostsKnown: true,
				QuantityTons: 1, GameSeconds: 3_600, ProceedsPence: 60_000_000,
				CompletedAt: time.Now().Add(-time.Hour),
			})
			if saving {
				evidence := growthEvidence{workingByRegion: make(map[string]int64)}
				if reason, err := runner.growthCashFunding(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("lane funding: %s, %v", reason, err)
				}
				runner.snapshot["_capacity_savings_target"] = encode(capacitySavingsTarget{
					PortID: recoveryFarPort, HullID: "savings-offsite-hull", RequiredPence: evidence.available + 1_000,
					CapitalPence: 200_000, DepthNetPencePerGameDay: 100_000_000, ExpiresAt: time.Now().Add(time.Hour),
				})
			}
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			if saving {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				return
			}
			_, data := h.mission("__growth")
			if data.Capacity != nil || data.GrowthLane == nil || data.Action == nil {
				t.Fatalf("positive control did not reach the lane-proof build: %+v", data)
			}
		})
	}
}

func TestCapacitySavingsExpiredIdentityWaitsForBuildOrHistoryWindow(t *testing.T) {
	t.Parallel()
	for _, elapsed := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry still excludes saving", true: "history window permits saving again"}[elapsed], func(t *testing.T) {
			h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
			if target.HullID != "savings-large-hull" {
				t.Fatalf("positive control did not establish a target: %+v", target)
			}
			target.ExpiresAt = time.Now().UTC().Add(-time.Second)
			runner.snapshot["_capacity_savings_target"] = encode(target)
			smallKey := recoveryHomePort + "|" + recoveryHull
			small := runner.stateCache.Quotes[smallKey]
			small.Available = false
			runner.stateCache.Quotes[smallKey] = small
			runner.opts.Execute = false
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			snapshot, err := h.db.Snapshot(h.ctx, h.account.ID)
			if err != nil {
				t.Fatal(err)
			}
			key := target.PortID + "|" + target.HullID
			expired := decode[map[string]time.Time](snapshot, "_capacity_savings_expired")
			if got := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target"); got.PortID != "" ||
				!expired[key].Equal(target.ExpiresAt.Add(capacityHistoryWindow)) {
				t.Fatalf("expiry was not durably excluded without affordable A: target %+v expired %+v", got, expired)
			}
			runner.snapshot = snapshot
			if elapsed {
				expired[key] = time.Now().UTC().Add(-time.Second)
				runner.snapshot["_capacity_savings_expired"] = encode(expired)
			}
			small.Available = true
			runner.stateCache.Quotes[smallKey] = small
			runner.opts.Execute = true
			if _, err := runner.considerGrowth(h.ctx); err != nil {
				t.Fatal(err)
			}
			got := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
			if elapsed {
				h.missingMission("__growth")
				if got.HullID != target.HullID {
					t.Fatalf("elapsed history window still excluded fresh saving: %+v", got)
				}
				return
			}
			_, data := h.mission("__growth")
			if got.PortID != "" || data.BuildClass != recoveryHull || data.Action == nil {
				t.Fatalf("later affordable A recreated the expired savings hold: target %+v build %+v", got, data.Action)
			}
		})
	}
}
