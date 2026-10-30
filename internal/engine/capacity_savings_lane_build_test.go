package engine

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestCapacitySavingsLaneBuildHonorsExactAffordableTarget(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"lane without marker", "capacity without marker", "saved present yard", "saved arriving yard", "lane before B remittance", "saved B remittance"} {
		t.Run(scenario, func(t *testing.T) {
			h, runner, _, save := capacitySavingsRunnerFixture(t, true)
			h.seedLaneProofs()
			port, _ := findPort(runner.stateCache.Catalog, recoveryHomePort)
			for i := range runner.stateCache.Vaults {
				runner.stateCache.Vaults[i].BalancePence += 2_000_000
			}

			// C has B's useful hold for half its hull price. Both ordinary lane
			// selection and unrestricted capacity selection should prefer C;
			// merely falling back to unrestricted capacity cannot satisfy B.
			const savedHull = "brigantine"
			const alternativeHull = "cutter"
			alternative, exists := findHull(runner.stateCache.Catalog, savedHull)
			if !exists {
				t.Fatal("fixture has no saved hull")
			}
			alternative.ID = alternativeHull
			runner.stateCache.Catalog.ShipClasses = append(runner.stateCache.Catalog.ShipClasses, alternative)
			for _, pair := range [][2]string{{recoveryHomePort, recoveryFarPort}, {recoveryFarPort, recoveryHomePort}} {
				oldKey := routeKey(pair[0], pair[1], savedHull)
				key := routeKey(pair[0], pair[1], alternativeHull)
				route := runner.stateCache.Routes[oldKey]
				route.ShipClass = alternativeHull
				runner.stateCache.Routes[key] = route
				runner.meta["routes/"+key] = runner.meta["routes/"+oldKey]
				if !runner.fresh("routes/"+key) || route.FromPort != pair[0] || route.ToPort != pair[1] {
					t.Fatalf("alternative lacks fresh matching route authority: %+v", route)
				}
			}
			quote := runner.stateCache.Quotes[recoveryHomePort+"|"+recoveryHull]
			quote.ShipClass = alternativeHull
			alternativeKey := recoveryHomePort + "|" + alternativeHull
			runner.stateCache.Quotes[alternativeKey] = quote
			runner.meta["yard_quotes/"+alternativeKey] = runner.meta["yard_quotes/"+recoveryHomePort+"|"+recoveryHull]
			if _, err := runner.growthQuote(port, alternative); err != nil {
				t.Fatalf("alternative lacks executable fresh quote authority: %v", err)
			}

			target := capacitySavingsTarget{
				PortID: recoveryHomePort, HullID: savedHull,
				RequiredPence: growthCashTotal(save.QuotedHullPence, save.SlippageReservePence,
					save.CannonsPence, save.InitialStoresPence, save.WorkingCapitalPence),
				CapitalPence: growthCashTotal(save.QuotedHullPence, save.SlippageReservePence,
					save.CannonsPence, save.InitialStoresPence),
				DepthNetPencePerGameDay: save.DepthNetPencePerGameDay,
				DepthPreferenceShips:    save.DepthCompetition.PreferenceShips,
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}
			if scenario == "saved arriving yard" {
				// B is quoted at a yard without present authority, but the live
				// fleet is already sailing there. C remains executable at home.
				target.PortID = recoveryFarPort
				runner.stateCache.Factors = []string{recoveryHomePort}
				for i := range runner.stateCache.Catalog.Ports {
					if runner.stateCache.Catalog.Ports[i].ID == recoveryFarPort {
						runner.stateCache.Catalog.Ports[i].HasShipyard = true
					}
				}
				savedKey := recoveryHomePort + "|" + savedHull
				absentKey := recoveryFarPort + "|" + savedHull
				absentQuote := runner.stateCache.Quotes[savedKey]
				absentQuote.PortID = recoveryFarPort
				runner.stateCache.Quotes[absentKey] = absentQuote
				runner.meta["yard_quotes/"+absentKey] = runner.meta["yard_quotes/"+savedKey]
				delete(runner.stateCache.Quotes, savedKey)
				delete(runner.meta, "yard_quotes/"+savedKey)
				ship := runner.stateCache.Ships[0]
				if ship.Ship.Status != "sailing" || len(ship.RouteLegs) == 0 || ship.RouteLegs[len(ship.RouteLegs)-1].ToNode != target.PortID {
					t.Fatalf("absent savings yard has no arriving ship: %+v", ship)
				}
			}
			evidence := growthEvidence{workingByRegion: make(map[string]int64)}
			if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
				t.Fatalf("lane proof and funding must be valid: %s, %v", reason, err)
			}
			if evidence.available < target.RequiredPence {
				t.Fatalf("saved B must already be affordable: available=%d required=%d", evidence.available, target.RequiredPence)
			}
			observeVaults := func() {
				t.Helper()
				// Refresh only real treasury authority; a catalog refresh would
				// discard the synthetic B and C hulls and their executable routes.
				if err := runner.captureWave(h.ctx, []fetchTask{{key: "vaults", path: "/v1/treasury/vaults", kind: fetchJSON}}, nil); err != nil {
					t.Fatal(err)
				}
				observed := decode[struct {
					Vaults []Vault `json:"vaults"`
				}](runner.snapshot, "vaults")
				if !runner.fresh("vaults") || len(observed.Vaults) == 0 {
					t.Fatal("treasury did not return fresh regional balances")
				}
				runner.stateCache.Vaults = observed.Vaults
			}
			if scenario == "lane before B remittance" || scenario == "saved B remittance" {
				// Make B's yard capital exceed C's entire local lane package,
				// while the company's donor region can still fund either hull.
				savedKey := recoveryHomePort + "|" + savedHull
				savedQuote := runner.stateCache.Quotes[savedKey]
				oldPrice := *savedQuote.TotalCostPence
				newPrice := oldPrice + 100_000
				savedQuote.TotalCostPence = &newPrice
				runner.stateCache.Quotes[savedKey] = savedQuote
				capitalIncrease := growthCashTotal(newPrice, ceilBPS(newPrice, 1_000)) -
					growthCashTotal(oldPrice, ceilBPS(oldPrice, 1_000))
				target.CapitalPence += capitalIncrease
				target.RequiredPence += capitalIncrease + max(runner.opts.ReservePence, ceilBPS(newPrice, 1_500)) -
					max(runner.opts.ReservePence, ceilBPS(oldPrice, 1_500))
				grown := append(append([]ShipState(nil), runner.stateCache.Ships...), growthCandidate(port, alternative))
				deltaUpkeep := DailyUpkeep(grown, runner.stateCache.Catalog, len(runner.stateCache.Factors)) -
					DailyUpkeep(runner.stateCache.Ships, runner.stateCache.Catalog, len(runner.stateCache.Factors))
				pack, feasible, err := runner.growthAtYard(h.ctx, port, alternative, quote, deltaUpkeep, &evidence, nil)
				if err != nil || !feasible || !pack.decision.Accepted || !pack.plan.Accepted || pack.plan.Quantity <= 0 {
					t.Fatalf("C must have a genuinely feasible cheaper lane package: %+v, feasible=%t err=%v", pack, feasible, err)
				}
				// C's whole lane package is funded locally. Only B needs the
				// donor's cash: company affordability must not erase its marker.
				yardCash := pack.required[port.BankingRegion]
				if yardCash >= evidence.required[port.BankingRegion]+target.CapitalPence {
					t.Fatalf("C's funded lane must leave B's yard capital short: lane=%d B=%d", yardCash, evidence.required[port.BankingRegion]+target.CapitalPence)
				}
				h.api.setVaultBalance(port.BankingRegion, yardCash)
				h.api.setVaultBalance("south", evidence.required["south"]+2_000_000)
				observeVaults()
				if reason, err := runner.growthFunding(h.ctx, &evidence); err != nil || reason != "" {
					t.Fatalf("regional funding fixture lost lane proof: %s, %v", reason, err)
				}
				pack, feasible, err = runner.growthAtYard(h.ctx, port, alternative, quote, deltaUpkeep, &evidence, nil)
				if err != nil || !feasible || pack.decision.FundingFeesPence != 0 {
					t.Fatalf("C must remain executable without B's remittance: %+v, feasible=%t err=%v", pack, feasible, err)
				}
				if evidence.available < target.RequiredPence {
					t.Fatalf("B must be globally affordable before remittance: available=%d required=%d", evidence.available, target.RequiredPence)
				}
				if wait, err := runner.capacitySavingsWait(h.ctx, target, pack.decision, evidence.available, 1, 0); err != nil || !wait {
					t.Fatalf("feasible C lane must lose to live B: wait=%t err=%v lane=%+v target=%+v", wait, err, pack.decision, target)
				}
			}
			if scenario == "saved present yard" || scenario == "saved arriving yard" || scenario == "saved B remittance" {
				if err := runner.setCapacitySavingsTarget(h.ctx, target); err != nil {
					t.Fatal(err)
				}
			}

			var changed bool
			var err error
			if scenario == "capacity without marker" {
				changed, err = runner.considerCapacityGrowth(h.ctx, &evidence)
			} else {
				changed, err = runner.considerGrowth(h.ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "saved B remittance" {
				assertPendingSavings := func() {
					t.Helper()
					persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, snapshot := range []map[string]json.RawMessage{runner.snapshot, persisted} {
						if retained := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target"); retained != target {
							t.Fatalf("B lost its durable marker before build staging: got %+v want %+v", retained, target)
						}
						if got := decode[string](snapshot, "_capacity_growth_port"); got != "" {
							t.Fatalf("unfinished B funding claimed a staged growth port: %q", got)
						}
					}
					h.missingMission("__growth")
					if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
						t.Fatal("B funding built a lane or alternative hull early")
					}
				}
				if !changed {
					t.Fatalf("globally affordable B did not stage regional funding: holds=%v", runner.holding)
				}
				assertPendingSavings()
				treasury, transfer := h.mission("__treasury")
				if transfer.Action == nil || transfer.Action.Stage != "remit" || transfer.Action.Path != "/v1/treasury/remit" {
					t.Fatalf("B did not stage a genuine treasury action: %+v", transfer)
				}
				var request struct {
					From  string `json:"from_region"`
					To    string `json:"to_region"`
					Gross int64  `json:"gross_amount_pence"`
				}
				if err := json.Unmarshal(transfer.Action.Body, &request); err != nil {
					t.Fatal(err)
				}
				if request.From != "south" || request.To != port.BankingRegion || request.Gross <= 0 ||
					request.Gross > evidence.balances["south"]-evidence.required["south"] {
					t.Fatalf("B funding did not preserve its real donor reserve: %+v", request)
				}
				if changed, err := runner.considerGrowth(h.ctx); err != nil || changed {
					t.Fatalf("pending B remittance admitted another growth action: changed=%t err=%v", changed, err)
				}
				assertPendingSavings()
				if changed, err := runner.dispatch(h.ctx, treasury, &transfer); err != nil || !changed {
					t.Fatalf("dispatch real B remittance: changed=%t err=%v holds=%v", changed, err, runner.holding)
				}
				calls := h.api.mutationsFor("/v1/treasury/remit")
				if len(calls) != 1 || calls[0].Status != http.StatusOK {
					t.Fatalf("B needs exactly one successful real remittance: %+v", calls)
				}
				treasury, transfer = h.mission("__treasury")
				if treasury.Phase != "done" || transfer.Action != nil || transfer.FeesPence <= 0 {
					t.Fatalf("B funding did not consume its measured receipt: %+v %+v", treasury, transfer)
				}
				assertPendingSavings()
				if err := h.db.DeleteMission(h.ctx, h.account.ID, "__treasury"); err != nil {
					t.Fatal(err)
				}
				observeVaults()
				funded := runner.planningInput(ShipState{}).Balances
				fee := request.Gross * 100 / 10_000
				if funded[port.BankingRegion] != evidence.balances[port.BankingRegion]+request.Gross-fee ||
					funded["south"] != evidence.balances["south"]-request.Gross ||
					funded[port.BankingRegion] < evidence.required[port.BankingRegion]+target.CapitalPence {
					t.Fatalf("real remittance failed to fund B's yard: before=%v after=%v request=%+v", evidence.balances, funded, request)
				}
				assertPendingSavings()
				changed, err = runner.considerGrowth(h.ctx)
				if err != nil {
					t.Fatal(err)
				}
				persisted, err := h.db.Snapshot(h.ctx, h.account.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, snapshot := range []map[string]json.RawMessage{runner.snapshot, persisted} {
					if retained := decode[capacitySavingsTarget](snapshot, "_capacity_savings_target"); retained.PortID != "" {
						t.Fatalf("successfully staged B retained its savings marker: %+v", retained)
					}
					if got := decode[string](snapshot, "_capacity_growth_port"); got != target.PortID {
						t.Fatalf("staged B did not publish its growth port: got %q want %q", got, target.PortID)
					}
				}
			}
			if scenario == "saved arriving yard" {
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if retained := decode[capacitySavingsTarget](runner.snapshot, "_capacity_savings_target"); retained.PortID != target.PortID || retained.HullID != target.HullID {
					t.Fatalf("affordable B awaiting arrival lost its exact marker: %+v", retained)
				}
				if _, err := runner.considerGrowth(h.ctx); err != nil {
					t.Fatal(err)
				}
				h.missingMission("__growth")
				h.missingMission("__treasury")
				if h.api.mutationCount("/v1/shipyard/build-ship") != 0 || h.api.mutationCount("/v1/company/hire-factor") != 0 {
					t.Fatal("waiting for B spent savings on another hull or factor")
				}
				return
			}
			if !changed {
				t.Fatalf("funded growth did not stage a build: holds=%v", runner.holding)
			}
			mission, data := h.mission("__growth")
			wantHull := alternativeHull
			if scenario == "saved present yard" || scenario == "saved B remittance" {
				wantHull = savedHull
			}
			if data.BuildPort != recoveryHomePort || data.BuildClass != wantHull || data.Action == nil || data.Action.Stage != "build" || data.Action.Path != "/v1/shipyard/build-ship" {
				t.Fatalf("growth staged the wrong yard or hull: %+v", data)
			}
			if scenario == "lane without marker" || scenario == "lane before B remittance" {
				if data.GrowthLane == nil || data.Capacity != nil || !data.Plan.Accepted || data.Plan.Quantity <= 0 {
					t.Fatalf("positive control did not genuinely admit a lane package: %+v", data)
				}
			} else if data.Capacity == nil || data.GrowthLane != nil {
				t.Fatalf("saved or capacity-control hull bypassed capacity admission: %+v", data)
			}
			if h.api.mutationCount("/v1/shipyard/build-ship") != 0 {
				t.Fatal("admission sent a build before durable action dispatch")
			}
			if changed, err := runner.dispatch(h.ctx, mission, &data); err != nil || !changed {
				t.Fatalf("dispatch admitted build: changed=%t err=%v holds=%v", changed, err, runner.holding)
			}
			calls := h.api.mutationsFor("/v1/shipyard/build-ship")
			if len(calls) != 1 || calls[0].Status != http.StatusOK {
				t.Fatalf("expected exactly one successful build and no alternate: %+v", calls)
			}
			var built struct {
				PortID    string `json:"port_id"`
				ShipClass string `json:"ship_class"`
			}
			if err := json.Unmarshal(calls[0].Body, &built); err != nil {
				t.Fatal(err)
			}
			if built.PortID != recoveryHomePort || built.ShipClass != wantHull {
				t.Fatalf("wire build chose a different yard or hull: %+v", built)
			}
			if scenario == "lane before B remittance" && h.api.mutationCount("/v1/treasury/remit") != 0 {
				t.Fatal("unmarked C lane needed the remittance reserved for B")
			}
			h.missingMission("__treasury")
			if h.api.mutationCount("/v1/company/hire-factor") != 0 {
				t.Fatal("admitted build also spent cash on an expansion factor")
			}
		})
	}
}
