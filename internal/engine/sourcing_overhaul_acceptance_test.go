package engine

// Failure-first acceptance contracts for the sourcing overhaul (plan E1),
// written against the current planningInput/sourcing seams before the engine
// changes land:
//
//   - An unused zero-balance vault region that no ship, mission, warehouse or
//     plan can ever reach is not a funding obligation when the runner planning
//     input is constructed, so bounded source inspections stay plannable
//     instead of being deferred forever by unreachable bookkeeping reserves.
//   - A legal source inspection separates the full safe-store purchase cash
//     (which must be funded from observed balances) from the consumed store
//     expense (which is what the capped exploration exposure counts), while
//     the hull-fit, observed-depth and charter-deadline constraints still bind.
//
// Every fixture supplies the reserves and demand the probe needs so the only
// possible failure is the targeted behavior.

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPlanningInputSkipsUnusedZeroBalanceRegionFunding(t *testing.T) {
	t.Parallel()
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	ship := sourcingFixture[ShipState](t, `{"ship":{"id":"`+recoveryShipBlastoise+`","name":"Blastoise","ship_class":"`+recoveryHull+`","status":"sailing","current_port_id":"`+recoveryHomePort+`"}}`)
	state := accountState{
		Catalog: Catalog{
			Ports: []Port{
				{ID: recoveryHomePort, BankingRegion: "north"},
				{ID: recoveryFarPort, BankingRegion: "south"},
			},
			ShipClasses: []Hull{{ID: recoveryHull, HullCategory: "coastal", HoldCapacityTons: 60}},
		},
		Ships: []ShipState{ship},
		Vaults: []Vault{
			{AccountID: "vault-north", BankingRegion: "north", AccountType: "vault", BalancePence: 1_000_000},
			{AccountID: "vault-far", BankingRegion: "far", AccountType: "vault", BalancePence: 0},
		},
		HomePortID: recoveryHomePort,
	}
	input := runner.planningInputFrom(ship, state)
	if input.ReserveByRegion["far"] != 0 {
		t.Fatalf("unused zero-balance unrelated region far requires %dp of funding in the planning input although no ship, mission, warehouse or plan can ever reach it: %v", input.ReserveByRegion["far"], input.ReserveByRegion)
	}
	if want := runner.opts.ReservePence + 3*DailyUpkeep(state.Ships, state.Catalog, 0); input.ReserveByRegion["north"] != want {
		t.Fatalf("home region reserve changed: got %d, want %d", input.ReserveByRegion["north"], want)
	}
	if input.Balances["north"] != 1_000_000 || input.Balances["far"] != 0 {
		t.Fatalf("planning input lost observed balances: %v", input.Balances)
	}

	// Consumer consequence: the same unused zero-balance vault must not deaden
	// bounded sourcing probes with an unreachable regional reserve.
	h.api.setFleet([]map[string]any{sourcingReviewShipRow()})
	h.api.setVaultBalance("far", 0)
	prober := h.runner(primaryTestKey)
	prober.generation = uuid.NewString()
	if err := prober.refresh(h.ctx); err != nil {
		t.Fatalf("refresh sourcing probe world with an unused zero-balance vault: %v", err)
	}
	sourcingReviewInject(t, prober, sourcingReviewCharters())
	idle, exists := prober.ship(recoveryShipBlastoise)
	if !exists {
		t.Fatal("idle probe ship missing from the refreshed fleet")
	}
	probeInput, err := prober.sourcingInput(h.ctx, idle)
	if err != nil {
		t.Fatalf("build sourcing input: %v", err)
	}
	accepted := acceptedRelocations(planSourcingRelocation(probeInput))
	if len(accepted) == 0 {
		t.Fatalf("an unused zero-balance unrelated vault region made every bounded sourcing probe unfundable: %+v", planSourcingRelocation(probeInput))
	}
	if accepted[0].Destination != "amsterdam" {
		t.Fatalf("sourcing probe drifted from the evidenced source: %+v", accepted[0])
	}
}

// sourcingStorePurchaseFixture forces the probe to buy its whole both-way safe
// store load at the origin quay at 12000p/ton: the full purchase alone (24602p
// with customs and handling) exceeds the fixture's 18500p exploration
// allowance, while the consumed expense on the modeled route is a small
// fraction of one ton.
func sourcingStorePurchaseFixture(t *testing.T) PlanningInput {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Ship.Stores.ProvisionsTons = 0
	market := in.Markets["le_havre"]
	market.Books[0].Depth.Asks[0].PricePence = 12_000
	in.Markets["le_havre"] = market
	return in
}

func TestSourcingInspectCapsConsumedExpenseNotStorePurchaseCash(t *testing.T) {
	t.Parallel()
	t.Run("full purchase cash funded, consumed expense capped", func(t *testing.T) {
		in := sourcingStorePurchaseFixture(t)
		accepted := acceptedRelocations(planSourcingRelocation(in))
		if len(accepted) == 0 {
			t.Fatalf("a legal funded source inspection was rejected because the full store purchase sat inside the capped exploration exposure: %+v", planSourcingRelocation(in))
		}
		probe := accepted[0]
		if probe.Destination != "amsterdam" || probe.Economics.RevenuePence != 0 {
			t.Fatalf("probe is not a zero-revenue inspection of the evidenced source: %+v", probe)
		}
		if exposure := sourcingExposure(probe); exposure >= 9_000 {
			t.Fatalf("capped exploration exposure %dp still carries the full store purchase cash (the both-way purchase alone costs 24602p at the observed 12000p ask); it must carry only the consumed expense plus bills", exposure)
		}
	})
	t.Run("unfunded purchase cash still rejects", func(t *testing.T) {
		in := sourcingStorePurchaseFixture(t)
		// Above the protected reserve plus the consumed-expense exposure, but
		// below the full purchase funding the relocation requires.
		in.Balances["channel"] = 95_000
		if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
			t.Fatalf("source inspection accepted although observed balances cannot fund the full safe-store purchase: %+v", accepted)
		}
	})
	t.Run("stores that cannot fit the hull still reject", func(t *testing.T) {
		in := sourcingStorePurchaseFixture(t)
		for _, key := range []string{routeKey("le_havre", "amsterdam", "hoy"), routeKey("amsterdam", "portsmouth", "hoy")} {
			route := in.Routes[key]
			route.SafeProvisionsTons = 20
			in.Routes[key] = route
		}
		if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
			t.Fatalf("both-way safe stores beyond the empty hold were accepted for purchase: %+v", accepted)
		}
	})
	t.Run("store depth thinner than the safe load still rejects", func(t *testing.T) {
		in := sourcingStorePurchaseFixture(t)
		market := in.Markets["le_havre"]
		market.Books[0].Depth.Asks[0].RemainingQuantity = 1
		in.Markets["le_havre"] = market
		if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
			t.Fatalf("missing observed provisions depth was replaced with a top-of-book guess: %+v", accepted)
		}
	})
	t.Run("impossible charter deadline still rejects", func(t *testing.T) {
		in := sourcingStorePurchaseFixture(t)
		expiry := in.Now.Add(time.Second)
		in.Contracts[0].ExpiresAt = &expiry
		if accepted := acceptedRelocations(planSourcingRelocation(in)); len(accepted) != 0 {
			t.Fatalf("source inspection accepted against an impossible charter deadline: %+v", accepted)
		}
	})
}
