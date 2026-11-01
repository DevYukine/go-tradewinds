package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
)

func TestCapacityFinancedBuildRevalidatesEstimateRouteAgainstLivePiracy(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"unchanged control", "piracy beyond calibration limit", "negative piracy observation", "missing piracy observation"} {
		t.Run(change, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID)
			quote, netWorth, _ := overseasCapacityBoundary(t, h, runner, yard, hull, quote)
			at := time.Now().UTC()
			overseasMetric(t, h, netWorth, at)
			evidence, input := overseasCapacityEvidence(t, h, runner, at)
			admitted, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil || !admitted.Accepted || !strings.HasPrefix(admitted.HazardSource, "zone estimate") {
				t.Fatalf("financed admission control: %+v %v", admitted, err)
			}
			// The estimate must retain its target lane so the financed
			// preflight revalidates exactly that route, not the calibration.
			if admitted.HazardDestination != "new_york" {
				t.Fatalf("admitted estimate lost its target-route pin: %+v", admitted)
			}
			if change != "unchanged control" {
				// Calibration samples record zero piracy in zone-north, so
				// the sampled arrival risk is that zone's storm rating.
				sampleRisk := int64(0)
				for _, zone := range runner.stateCache.Catalog.SeaZones {
					if zone.ID == "zone-north" {
						sampleRisk = zone.BaseStormBPS
						break
					}
				}
				limitPiracy := sampleRisk + sampleRisk/2
				for _, zone := range runner.stateCache.Catalog.SeaZones {
					if zone.ID == yard.SeaZoneID {
						limitPiracy += -zone.BaseStormBPS
						break
					}
				}
				limitPiracy++
				zones := decode[[]ZoneObservation](runner.snapshot, "zones")
				observed := make([]ZoneObservation, 0, len(zones))
				touched := false
				for _, zone := range zones {
					if zone.SeaZoneID != yard.SeaZoneID {
						observed = append(observed, zone)
						continue
					}
					touched = true
					switch change {
					case "piracy beyond calibration limit":
						zone.NetPiracyBPS = limitPiracy
						observed = append(observed, zone)
					case "negative piracy observation":
						zone.NetPiracyBPS = -1
						observed = append(observed, zone)
					case "missing piracy observation":
						// the API stopped observing the arrival zone
					}
				}
				if !touched {
					t.Fatalf("arrival zone %s is not observed in the fixture", yard.SeaZoneID)
				}
				runner.snapshot["zones"] = encode(observed)
			}
			loan := snapshotTestLoan()
			loan.ID, loan.BorrowerCompanyID, loan.BankingRegion = "test-financed-build", runner.account.CompanyID, "north"
			loan.IsMortgage, loan.Status = false, "active"
			loan.PrincipalPence, loan.OutstandingPrincipalPence, loan.OwedPence = 10_000, 10_000, 10_000
			payment := int64(100)
			loan.InstallmentPence, loan.NextPaymentPence = &payment, &payment
			runner.stateCache.Loans = append(runner.stateCache.Loans, loan)
			runner.snapshot["loans"] = encode(runner.stateCache.Loans)
			runner.meta["loans"] = runner.meta["ships"]
			data := missionData{
				BuildPort: yard.ID, BuildClass: hull.ID, TargetCannons: 4,
				Capacity: &capacityIntent{
					BuildPort: yard.ID, BuildClass: hull.ID, QuoteLimitPence: 1_000_000, AdmittedAt: at,
					Financing: &capacityFinancing{
						Decision: admitted,
						Loans:    []gameapi.Loan{loan},
						Quotes:   []gameapi.LoanQuote{{PrincipalPence: loan.PrincipalPence, AnnualRateBPS: 300, InstallmentPence: &payment, ExpiresAt: at.Add(time.Hour)}},
					},
				},
			}
			err = runner.capacityBuildReady(h.ctx, &data)
			wantErr := change != "unchanged control"
			if wantErr != (err != nil) {
				t.Fatalf("financed preflight with %s: %v", change, err)
			}
			if !wantErr {
				return
			}
			if err.Error() != hazardUnavailable || h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatalf("changed target-route piracy did not fail closed as %q: %v", hazardUnavailable, err)
			}
			row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
			if !strings.Contains(string(row.Details), hazardUnavailable) {
				t.Fatalf("financed revalidation lost its hazard verdict: %s", row.Details)
			}
		})
	}
}

func TestCapacityEstimateDisablesWhenShipSailsIntoTargetRegion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		destination string
		estimate    bool
	}{
		{name: "final leg arrives in the target region", destination: "new_york"},
		{name: "final leg stays outside the region", destination: recoveryFarPort, estimate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			sailor := runner.stateCache.Ships[0]
			sailor.Ship.ID = "test-atlantic-sailor"
			sailor.Ship.Status = "sailing"
			sailor.Ship.CurrentPortID = "" // the API nulls the port while a ship is at sea
			sailor.RouteLegs = []RouteLeg{{FromNode: recoveryHomePort, ToNode: tc.destination, DistanceNM: 24, EdgeClass: "coastal"}}
			runner.stateCache.Ships = append(runner.stateCache.Ships, sailor)
			at := time.Now().UTC()
			overseasMetric(t, h, 10_000_000, at)
			evidence, input := overseasCapacityEvidence(t, h, runner, at)
			decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil {
				t.Fatal(err)
			}
			row := overseasCapacityDecision(t, h, yard.ID, hull.ID)
			if !tc.estimate {
				if decision.Accepted || decision.EconomicallyViable || decision.Reason == capacitySolvencyReason ||
					!strings.Contains(string(row.Details), hazardUnavailable) {
					t.Fatalf("a ship sailing into %s left the zone estimate on: %+v %s", tc.destination, decision, row.Details)
				}
				return
			}
			if !decision.Accepted {
				t.Fatalf("sailing past the region must keep the calibrated estimate: %+v %s", decision, row.Details)
			}
			overseasRequireEstimatePayload(t, row)
		})
	}
}

func TestCapacityCandidateChargesRetainedPairUpkeepAsSoleRegionalPresence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		resident bool // an owned ship berthed in the candidate's region
		stranger bool // a permanent factor in the region beyond the pair
	}{
		{name: "pair is the only regional presence"},
		{name: "owned ship berthed in the region", resident: true},
		{name: "permanent factor beyond the pair", stranger: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, runner, yard, hull, quote := overseasCapacityFixture(t)
			if tc.stranger {
				runner.stateCache.Catalog.Ports = append(runner.stateCache.Catalog.Ports,
					Port{ID: "test_baltimore", BankingRegion: yard.BankingRegion, SeaZoneID: yard.SeaZoneID, Phase: 2})
			}
			runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
			if tc.stranger {
				runner.stateCache.Factors = append(runner.stateCache.Factors, "test_baltimore")
			}
			hiredAt := time.Now().UTC().Add(-3 * time.Hour)
			runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
				{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hiredAt},
				{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hiredAt},
			})
			if tc.resident {
				resident := runner.stateCache.Ships[0]
				resident.Ship.ID = "test-regional-resident"
				resident.Ship.Status = "berthed"
				resident.Ship.CurrentPortID = "new_york"
				resident.Voyage = nil
				resident.RouteLegs = nil
				runner.stateCache.Ships = append(runner.stateCache.Ships, resident)
			}
			at := time.Now().UTC()
			overseasMetric(t, h, 10_000_000, at)
			evidence, input := overseasCapacityEvidence(t, h, runner, at)
			grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(yard, hull))
			factors := len(runner.stateCache.Factors)
			base := DailyUpkeep(grown, runner.stateCache.Catalog, factors) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, factors)
			withPair := DailyUpkeep(grown, runner.stateCache.Catalog, factors) - DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, factors-2)
			input.IncrementalDailyUpkeepPence = base
			decision, _, err := runner.capacityAtYard(h.ctx, yard, hull, quote, input, &evidence)
			if err != nil {
				t.Fatal(err)
			}
			if withPair <= base {
				t.Fatalf("fixture lost the retained pair's marginal factor cost: %d <= %d", withPair, base)
			}
			if !tc.resident && !tc.stranger {
				if !decision.EconomicallyViable {
					t.Fatalf("sole-presence pair candidate is not an economic comparison: %+v", decision)
				}
				if decision.IncrementalUpkeepPence != withPair {
					t.Fatalf("sole regional pair upkeep was not charged: upkeep=%d want=%d", decision.IncrementalUpkeepPence, withPair)
				}
				return
			}
			if decision.IncrementalUpkeepPence != base {
				t.Fatalf("%s must not charge the retained pair: upkeep=%d want=%d", tc.name, decision.IncrementalUpkeepPence, base)
			}
		})
	}
}

func TestCapacityPairedSavingsExpiryBlocksImmediateRearm(t *testing.T) {
	t.Parallel()
	h, runner, _, _ := capacitySavingsRunnerFixture(t, true)
	yard, small, _ := overseasAddCapacityWorld(t, h, runner)
	overseasMetric(t, h, 10_000_000, time.Now().UTC())
	runner.stateCache.Factors = append(runner.stateCache.Factors, yard.ID, "new_york")
	for i := range runner.stateCache.Vaults {
		if runner.stateCache.Vaults[i].BankingRegion == yard.BankingRegion {
			runner.stateCache.Vaults[i].BalancePence = 0
		}
	}
	hiredAt := time.Now().UTC().Add(-3 * time.Hour)
	runner.snapshot["_expansion_agents"] = encode([]expansionAgent{
		{PortID: yard.ID, Partner: "new_york", Region: yard.BankingRegion, HiredAt: hiredAt},
		{PortID: "new_york", Partner: yard.ID, Region: yard.BankingRegion, HiredAt: hiredAt},
	})
	large, exists := findHull(runner.stateCache.Catalog, "brigantine")
	if !exists {
		t.Fatal("brigantine is absent from the catalog")
	}
	quote := runner.stateCache.Quotes[recoveryHomePort+"|"+large.ID]
	delete(runner.stateCache.Quotes, recoveryHomePort+"|"+large.ID)
	quote.PortID = yard.ID
	runner.stateCache.Quotes[yard.ID+"|"+large.ID] = quote
	runner.meta["yard_quotes/"+yard.ID+"|"+large.ID] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+large.ID]
	for _, pair := range [][2]string{{yard.ID, "new_york"}, {"new_york", yard.ID}} {
		key := routeKey(pair[0], pair[1], large.ID)
		sourceKey := routeKey(pair[0], pair[1], small.ID)
		route := runner.stateCache.Routes[sourceKey]
		route.ShipClass = large.ID
		runner.stateCache.Routes[key] = route
		runner.meta["routes/"+key] = runner.meta["routes/"+sourceKey]
	}
	bookCommodity(runner.stateCache.Markets["new_york"], recoveryGrain).Depth.Bids[0].PricePence = 50_000
	// This savings-only fixture has already exhausted player credit here.
	runner.snapshot["_player_loan_fallback"] = encode(map[string]time.Time{
		yard.ID + "|" + large.ID: time.Now().UTC().Add(6 * time.Hour),
	})
	evidence, input := overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	candidate, _, err := runner.capacityAtYard(h.ctx, yard, large, quote, input, &evidence)
	if err != nil || candidate.Accepted || !candidate.EconomicallyViable || candidate.CashShortfallPence <= 0 || candidate.DepthNetPencePerGameDay <= 0 {
		t.Fatalf("paired overseas winner must stay cash-short and economic: %+v %v", candidate, err)
	}
	if _, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil {
		t.Fatal(err)
	}
	target := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target")
	if target.PortID != yard.ID || target.HullID != large.ID {
		t.Fatalf("paired cash-short winner did not become the savings target: %+v", target)
	}
	target.ExpiresAt = time.Now().UTC().Add(-time.Second)
	runner.snapshot["_capacity_savings_target"] = encode(target)
	evidence, input = overseasCapacityEvidence(t, h, runner, time.Now().UTC())
	if _, err := runner.considerCapacityGrowth(h.ctx, &evidence); err != nil {
		t.Fatal(err)
	}
	if re := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); re.PortID != "" || re.HullID != "" {
		t.Fatalf("expired savings target re-armed inside its 24h exclusion: %+v", re)
	}
	expired := decode[map[string]time.Time](runner.snapshot, "_capacity_savings_expired")
	until, held := expired[yard.ID+"|"+large.ID]
	if !held || !time.Now().UTC().Before(until) {
		t.Fatalf("expiry lost the documented 24h exclusion: %v", expired)
	}
	h.missingMission("__growth")
}
