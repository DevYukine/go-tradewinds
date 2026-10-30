package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func offlineIncidentInput(t *testing.T, ask int64) (PlanningInput, Plan) {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Contracts = nil
	in.Ship.EffectiveHoldCapacityTons = 30
	in.Balances["channel"] = 10_000_000
	book := in.Markets["le_havre"]
	book.CustomsBPS = 0
	book.Books = append(book.Books, sourcingFixture[CommodityBook](t, `{"commodity_id":"coarse_cloth","depth":{"asks":[{"order_id":"cloth-ask","price_pence":1,"remaining_quantity":100}]}}`))
	book.Books[len(book.Books)-1].Depth.Asks[0].PricePence = ask
	in.Markets["le_havre"] = book
	in.Markets["portsmouth"] = sourcingFixture[Market](t, `{"port_id":"portsmouth","visibility":"quay","books":[{"commodity_id":"coarse_cloth","depth":{"bids":[{"order_id":"npc-1","price_pence":44726,"remaining_quantity":66,"is_npc":true},{"order_id":"npc-2","price_pence":33943,"remaining_quantity":101,"is_npc":true},{"order_id":"npc-3","price_pence":27505,"remaining_quantity":101,"is_npc":true}]}}]}`)
	return in, Plan{Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: "portsmouth", Commodity: "coarse_cloth", Quantity: 30}
}

func TestOfflineIncidentLadderFlagAndFundingBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ask      int64
		flag     bool
		accepted bool
		stress   int64
	}{
		{name: "live default retains fresh quay economics", ask: 40_000, accepted: true},
		{name: "next rung rejects thin margin", ask: 40_000, flag: true, stress: 323_490},
		{name: "next rung admits sufficient margin", ask: 20_000, flag: true, accepted: true, stress: 323_490},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, candidate := offlineIncidentInput(t, tc.ask)
			in.NextRungReceiptStress = tc.flag
			plan := pricePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate, 0, 0)
			if plan.Accepted != tc.accepted || plan.Economics.PriceUncertaintyStressPence != tc.stress {
				t.Fatalf("incident lot: accepted=%v stress=%d, want %v/%d: %+v", plan.Accepted, plan.Economics.PriceUncertaintyStressPence, tc.accepted, tc.stress, plan)
			}
			if plan.SellLimitPence != 44_726 {
				t.Fatalf("risk diagnostic replaced executable sell limit: %d", plan.SellLimitPence)
			}
			if tc.flag && tc.accepted {
				fee := plan.Economics.ExpectedNetPence - tc.stress
				if fee <= 0 || !addFundingFees(plan, fee-1).Accepted || addFundingFees(plan, fee).Accepted {
					t.Fatalf("funding fees did not recheck strict positive next-rung contribution: %+v", plan)
				}
			}
		})
	}
}

func TestOfflineIncidentSizeSelectsBestAcceptedWholeTon(t *testing.T) {
	in, candidate := offlineIncidentInput(t, 20_000)
	in.NextRungReceiptStress = true
	best := sizePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate, 30, 0, 0, 0)
	if !best.Accepted || best.Quantity != 30 || best.SellLimitPence != 44_726 || best.Economics.PriceUncertaintyStressPence != 323_490 {
		t.Fatalf("profitable incident ladder lot was undersized or repriced: %+v", best)
	}
	in, candidate = offlineIncidentInput(t, 40_000)
	in.NextRungReceiptStress = true
	if got := sizePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate, 30, 0, 0, 0); got.Accepted {
		t.Fatalf("sizing escaped the receipt stress floor: %+v", got)
	}
}

func TestOfflineReceiptStressLeavesContractAndGossipEconomicsUnchanged(t *testing.T) {
	for _, kind := range []string{"charter", "freight"} {
		t.Run(kind, func(t *testing.T) {
			in := incomeFundingInput(t, kind)
			before := PlanVoyages(in)
			in.NextRungReceiptStress = true
			if after := PlanVoyages(in); !reflect.DeepEqual(before, after) {
				t.Fatalf("receipt stress changed %s economics: before=%+v after=%+v", kind, before, after)
			}
		})
	}
	in, _ := offlineIncidentInput(t, 20_000)
	delete(in.Markets, "portsmouth")
	bid := int64(44_726)
	detail := in.Ports["le_havre"]
	detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "portsmouth", SnapshotAt: in.Now, Commodities: []GossipCommodity{{CommodityID: "coarse_cloth", SnapshotAt: in.Now, BestBidPence: &bid}}}}
	in.Ports["le_havre"] = detail
	before := PlanVoyages(in)
	in.NextRungReceiptStress = true
	if after := PlanVoyages(in); !reflect.DeepEqual(before, after) {
		t.Fatalf("one-ton gossip economics changed: before=%+v after=%+v", before, after)
	}
	in.NextRungReceiptStress = false
	playerBid, playerQuantity := int64(40_000), int64(30)
	detail.CoffeeHouseGossip[0].Commodities[0].BestPlayerBidPence = &playerBid
	detail.CoffeeHouseGossip[0].Commodities[0].BestPlayerBidQuantity = &playerQuantity
	in.Ports["le_havre"] = detail
	candidate := Plan{Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre", Destination: "portsmouth", Commodity: "coarse_cloth", Quantity: 20}
	quoted := evaluatePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate, 0, 0)
	if !quoted.Accepted || quoted.PriceSource != playerBidGossipSource {
		t.Fatalf("player gossip fixture has no executable player-bid opportunity: %+v", quoted)
	}
	in.NextRungReceiptStress = true
	if after := evaluatePlan(in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate, 0, 0); !reflect.DeepEqual(quoted, after) {
		t.Fatalf("fresh-quay flag altered existing player-bid stress economics: before=%+v after=%+v", quoted, after)
	}
}

func TestOfflineSourcingCashKnobUsesRollingBudget(t *testing.T) {
	for _, tc := range []struct {
		name string
		bps  *int64
		want int64
	}{
		{name: "nil live default", want: 18_500},
		{name: "explicit default", bps: new(int64(200)), want: 18_500},
		{name: "quarter share", bps: new(int64(50)), want: 4_625},
		{name: "double share", bps: new(int64(400)), want: 37_000},
		{name: "disabled", bps: new(int64(0))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sourcingInputFixture(t)
			in.SourcingCashBPS = tc.bps
			budget := int64(50_000)
			in.SourcingAllowancePence = &budget
			if got := sourcingAllowance(in); got != tc.want {
				t.Fatalf("rolling allowance=%d, want %d", got, tc.want)
			}
			zero := int64(0)
			in.SourcingAllowancePence = &zero
			in.SourcingIdleEligible = true
			if got := sourcingAllowance(in); got != 0 {
				t.Fatalf("idle reinspection replenished an exhausted rolling budget: %d", got)
			}
			if tc.want == 0 {
				if plans := planSourcingRelocation(in); len(acceptedRelocations(plans)) != 0 {
					t.Fatalf("zero share admitted a probe: %+v", plans)
				}
				const wantReason = "rolling exploration budget exhausted"
				if reason := sourcingBudgetReason(in, 1, 0); reason != wantReason {
					t.Fatalf("zero-share budget reason = %q, want %q", reason, wantReason)
				}
			}
			budget = 1
			in.SourcingAllowancePence = &budget
			if got := sourcingAllowance(in); got != min(tc.want, budget) {
				t.Fatalf("cash share bypassed rolling budget: got %d", got)
			}
		})
	}
}

func TestOfflinePlanningInputForMatchesRunner(t *testing.T) {
	h := newRecoveryHarness(t)
	runner := h.runner(primaryTestKey)
	if err := runner.refresh(h.ctx); err != nil {
		t.Fatal(err)
	}
	ship := runner.stateCache.Ships[0]
	live := runner.planningInput(ship)
	sources := PlanningSources{
		CompanyID: runner.account.CompanyID, ReservePence: runner.opts.ReservePence,
		Metadata: runner.meta, Generation: runner.generation,
		CharterProceedsPence: runner.charterProceedsPence, Competition: runner.competition,
	}
	state := StateFromSnapshot(live.Now, runner.snapshot, sources)
	if !reflect.DeepEqual(state, runner.stateCache) {
		t.Fatal("pure state decoder diverged from the Runner's refreshed state")
	}
	offline := PlanningInputFor(live.Now, ship, state, runner.snapshot, sources)
	if !reflect.DeepEqual(live, offline) {
		t.Fatalf("offline input lost Runner policy sources:\nlive=%+v\noffline=%+v", live, offline)
	}
	if live.NextRungReceiptStress || live.SourcingCashBPS != nil {
		t.Fatal("offline experiment knobs changed live defaults")
	}
}

func TestOfflineRelocationFundingMatchesLiveExpenseCap(t *testing.T) {
	in := probeHistoryLiveInput(t)
	in.FleetDailyUpkeepPence = 0
	for id, detail := range in.Ports {
		detail.BerthDuesPerDayPence = 0
		in.Ports[id] = detail
	}
	in.Balances = map[string]int64{"channel": 75_000, "donor": 1_000_000}
	in.ReserveByRegion = map[string]int64{"channel": 75_000, "donor": 75_000}
	in.Rates = []Rate{{FromRegion: "donor", ToRegion: "channel", FeeBPS: 1_000}}
	in.Metadata["rates"] = sourcingFreshMeta(in.Now)
	quoted := costRelocation(in, Plan{
		Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "le_havre",
		Destination: "amsterdam", Commodity: "sawn_planks", ContractID: in.Contracts[0].ID,
	})
	if !quoted.Accepted || quoted.Economics.FundingFeesPence != 0 || quoted.Economics.ExpectedNetPence != -8 {
		t.Fatalf("fixture must initially fund exactly 8p consumed stores without a remittance: %+v", quoted)
	}
	// The accepted quote predates the need to remit. Reprice through the same
	// live cost path, including its conservative gross and server fee rounding.
	in.Balances["channel"] = 0
	live := costRelocation(in, quoted)
	if !live.Accepted || live.Economics.FundingFeesPence != 8_333 || live.Economics.ExpectedNetPence != -8_341 {
		t.Fatalf("fresh live quote lost its exact 8p expense plus 8333p fee: %+v", live)
	}
	// Live creates the durable intent before funding with the origin's
	// replacement-store basis, exactly as intent creation does.
	basisFill, err := WalkDepth(bookCommodity(in.Markets[quoted.Origin], "provisions").Depth.Asks, max(1, quoted.StoresToBuy), true)
	if err != nil {
		t.Fatal(err)
	}
	basis := &relocationStoreBasis{
		InitialTons:            in.Ship.Stores.ProvisionsTons,
		ReplacementPencePerTon: basisFill.LimitPence + ceilBPS(basisFill.LimitPence, in.Markets[quoted.Origin].CustomsBPS),
	}
	runner := &Runner{
		stateCache: AccountState{Ships: []ShipState{in.Ship}, Clock: in.Clock},
		meta:       in.Metadata, generation: in.Generation,
	}
	for _, test := range []struct {
		name     string
		cap      int64
		accepted bool
	}{
		{name: "fee alone exceeds cap", cap: 8_332},
		{name: "fee fits but consumed expense does not", cap: 8_333},
		{name: "one penny below complete expense", cap: 8_340},
		{name: "complete expense fits exactly", cap: 8_341, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := in
			input.SourcingAllowancePence = &test.cap
			live := costRelocation(input, quoted)
			data := missionData{
				Plan: quoted, CostsKnown: true,
				Relocation: &relocationIntent{ExpenseCapPence: test.cap, StoreBasis: basis},
			}
			projected := runner.relocationProjectedBudget(&data, input.Ship, input, live, 0)
			// The remittance guard must also retain already charged expense:
			// consumed stores debit the basis at its replacement price, so
			// 0.075 consumed tons are exactly the modeled 8p expense.
			consumed := input.Ship
			consumed.Stores.ProvisionsTons -= 0.075
			spentRunner := &Runner{
				stateCache: AccountState{Ships: []ShipState{consumed}, Clock: in.Clock},
				meta:       in.Metadata, generation: in.Generation,
			}
			feeReady := spentRunner.relocationBudget(&data, live.Economics.FundingFeesPence)
			if projected != test.accepted || feeReady != test.accepted {
				t.Fatalf("live expense boundary changed: projected=%v remittance=%v cap=%d", projected, feeReady, test.cap)
			}
			plan, transfers := PlanFunding(input, quoted)
			if plan.Accepted != projected {
				t.Fatalf("offline funding differs from the live cap: accepted=%v live=%v plan=%+v", plan.Accepted, projected, plan)
			}
			if !projected {
				const reason = "fresh sourcing costs exceed the durable exploration cap or lack a known expense basis"
				if len(transfers) != 0 || plan.Reason != reason {
					t.Fatalf("over-cap funding retained transfers or lost the live rejection: transfers=%+v plan=%+v", transfers, plan)
				}
				return
			}
			wantTransfers := []FundingTransfer{{Rate: in.Rates[0], Gross: 83_334, Net: 75_001}}
			if !reflect.DeepEqual(plan, live) || !reflect.DeepEqual(transfers, wantTransfers) {
				t.Fatalf("offline funding changed the live quote or exact fee: plan=%+v live=%+v transfers=%+v", plan, live, transfers)
			}
			again, repeated := PlanFunding(input, plan)
			if !reflect.DeepEqual(again, plan) || !reflect.DeepEqual(repeated, transfers) {
				t.Fatalf("re-quoting charged the remittance twice: first=%+v repeated=%+v", plan, again)
			}
		})
	}

	// A stepped provisions depth makes the durable basis more expensive than
	// the modeled quote: the basis prices every consumed round-trip ton at the
	// marginal 513p replacement level, while the quote itself amortizes only
	// the first ton at 103p. The replacement-store term must then reject caps
	// whose plain modeled expense would still pass.
	in = probeHistoryLiveInput(t)
	in.FleetDailyUpkeepPence = 0
	for id, detail := range in.Ports {
		detail.BerthDuesPerDayPence = 0
		in.Ports[id] = detail
	}
	book := in.Markets["le_havre"]
	for i := range book.Books {
		if book.Books[i].CommodityID == "provisions" {
			book.Books[i].Depth.Asks = []Level{
				{OrderID: "lh-cheap", PricePence: 100, RemainingQuantity: 1},
				{OrderID: "lh-deep", PricePence: 500, RemainingQuantity: 500},
			}
		}
	}
	in.Markets["le_havre"] = book
	in.Ship.Stores.ProvisionsTons = 0
	in.Balances = map[string]int64{"channel": 75_000, "donor": 1_000_000}
	in.ReserveByRegion = map[string]int64{"channel": 75_000, "donor": 75_000}
	in.Rates = []Rate{{FromRegion: "donor", ToRegion: "channel", FeeBPS: 1_000}}
	in.Metadata["rates"] = sourcingFreshMeta(in.Now)
	quoted = costRelocation(in, Plan{
		Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: "le_havre",
		Destination: "amsterdam", Commodity: "sawn_planks", ContractID: in.Contracts[0].ID,
	})
	if !quoted.Accepted || quoted.Economics.FundingFeesPence != 114 || quoted.Economics.ExpectedNetPence != -122 ||
		quoted.StoresToBuy != 2 || quoted.Economics.StoreCashPence != 1_027 || quoted.Economics.ProvisionsPence != 8 {
		t.Fatalf("stepped fixture must buy two stepped tons and model exactly 8p consumed stores plus a 114p fee: %+v", quoted)
	}
	in.Balances["channel"] = 0
	live = costRelocation(in, quoted)
	if !live.Accepted || live.Economics.FundingFeesPence != 8_447 || live.Economics.ExpectedNetPence != -8_455 {
		t.Fatalf("fresh live quote lost its exact 8p expense plus 8447p fee: %+v", live)
	}
	basisFill, err = WalkDepth(bookCommodity(in.Markets[quoted.Origin], "provisions").Depth.Asks, max(1, quoted.StoresToBuy), true)
	if err != nil {
		t.Fatal(err)
	}
	basis = &relocationStoreBasis{
		InitialTons:            in.Ship.Stores.ProvisionsTons,
		ReplacementPencePerTon: basisFill.LimitPence + ceilBPS(basisFill.LimitPence, in.Markets[quoted.Origin].CustomsBPS),
	}
	if basis.InitialTons != 0 || basis.ReplacementPencePerTon != 513 {
		t.Fatalf("stepped basis must start from empty stores at the 513p marginal replacement level: %+v", basis)
	}
	runner = &Runner{
		stateCache: AccountState{Ships: []ShipState{in.Ship}, Clock: in.Clock},
		meta:       in.Metadata, generation: in.Generation,
	}
	for _, test := range []struct {
		name     string
		cap      int64
		accepted bool
	}{
		{name: "modeled fee alone does not fit", cap: 8_454},
		{name: "modeled expense fits but replacement stores do not", cap: 8_482},
		{name: "replacement stores complete the expense exactly", cap: 8_483, accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := in
			input.SourcingAllowancePence = &test.cap
			live := costRelocation(input, quoted)
			data := missionData{
				Plan: quoted, CostsKnown: true,
				Relocation: &relocationIntent{ExpenseCapPence: test.cap, StoreBasis: basis},
			}
			projected := runner.relocationProjectedBudget(&data, input.Ship, input, live, 0)
			if projected != test.accepted {
				t.Fatalf("live replacement-store boundary changed: projected=%v cap=%d", projected, test.cap)
			}
			if test.cap == 8_482 {
				expense := -live.Economics.ExpectedNetPence - live.Economics.FundingFeesPence
				if !relocationExpenseWithinCap(test.cap, expense, 0, live.Economics.FundingFeesPence) {
					t.Fatalf("cap %d no longer admits the plain modeled expense", test.cap)
				}
			}
			plan, transfers := PlanFunding(input, quoted)
			if plan.Accepted != projected {
				t.Fatalf("offline funding differs from the live cap: accepted=%v live=%v plan=%+v", plan.Accepted, projected, plan)
			}
			if !projected {
				const reason = "fresh sourcing costs exceed the durable exploration cap or lack a known expense basis"
				if len(transfers) != 0 || plan.Reason != reason {
					t.Fatalf("over-cap funding retained transfers or lost the live rejection: transfers=%+v plan=%+v", transfers, plan)
				}
				return
			}
			wantTransfers := []FundingTransfer{{Rate: in.Rates[0], Gross: 84_475, Net: 76_028}}
			if !reflect.DeepEqual(plan, live) || !reflect.DeepEqual(transfers, wantTransfers) {
				t.Fatalf("offline funding changed the live quote or exact fee: plan=%+v live=%+v transfers=%+v", plan, live, transfers)
			}
			again, repeated := PlanFunding(input, plan)
			if !reflect.DeepEqual(again, plan) || !reflect.DeepEqual(repeated, transfers) {
				t.Fatalf("re-quoting charged the remittance twice: first=%+v repeated=%+v", plan, again)
			}
		})
	}
}

func TestOfflineSourcingLedgerUsesLiveRollingConversions(t *testing.T) {
	in := probeHistoryLiveInput(t)
	rows := []store.Settlement{
		{ID: "expired-profit", Kind: "charter", Origin: "amsterdam", QuantityTons: 1,
			StartedAt: in.Now.Add(-49 * time.Hour), CompletedAt: in.Now.Add(-48 * time.Hour),
			ProceedsPence: 9_000_000, CostsKnown: true},
		{ID: "seed-probe", Kind: "relocation", Origin: "le_havre", Destination: "amsterdam",
			CompletedAt: in.Now.Add(-12 * time.Hour), CostsKnown: true, ExplorationExpensePence: new(int64(900))},
		{ID: "conversion", Kind: "charter", Origin: "amsterdam", QuantityTons: 1,
			StartedAt: in.Now.Add(-10 * time.Hour), CompletedAt: in.Now.Add(-9 * time.Hour),
			ProceedsPence: 60_000, AcquisitionPence: 20_000, CostsKnown: true},
		{ID: "window-probe", Kind: "relocation", Origin: "le_havre", Destination: "portsmouth",
			CompletedAt: in.Now.Add(-time.Hour), CostsKnown: true, ExplorationExpensePence: new(int64(500))},
		{ID: "future", Kind: "relocation", CompletedAt: in.Now.Add(time.Minute), CostsKnown: false},
	}
	input := in
	if err := NewSourcingLedger(rows, nil).Apply(&input, nil); err != nil {
		t.Fatal(err)
	}
	if input.SourcingAllowancePence == nil || *input.SourcingAllowancePence != 38_600 ||
		input.SourcingProbeExpensePence != 1_400 || input.SourcingProbeCostsUnknown {
		t.Fatalf("offline budget lost rolling conversion attribution: %+v", input)
	}
	var live sourcingProbeHistory
	if err := live.rollingBudget(rows, in.Now, in.Clock.Calendar.TimeScale); err != nil {
		t.Fatal(err)
	}
	if *input.SourcingAllowancePence != live.rollingAllowance() ||
		!reflect.DeepEqual(input.SourcingProbeHistory.Conversions, live.Conversions) {
		t.Fatalf("offline budget differs from live helper: %+v / %+v", input.SourcingProbeHistory, live)
	}
	rows = append(rows, store.Settlement{ID: "unknown", Kind: "relocation", CompletedAt: in.Now.Add(-time.Minute)})
	if err := NewSourcingLedger(rows, nil).Apply(&input, nil); err != nil {
		t.Fatal(err)
	}
	if !input.SourcingProbeCostsUnknown || *input.SourcingAllowancePence != 0 {
		t.Fatal("unknown rolling probe expense did not close the allowance")
	}
	rows[len(rows)-1] = store.Settlement{ID: "overflow", Kind: "relocation", CompletedAt: in.Now.Add(-time.Minute),
		CostsKnown: true, ExplorationExpensePence: new(int64(math.MaxInt64))}
	if err := NewSourcingLedger(rows, nil).Apply(&input, nil); err == nil {
		t.Fatal("overflowing rolling probe expense was accepted")
	}
}

// TestOfflineSourcingHistoryLanesMatchLiveShortlist pins the exported
// shortlist to the lanes the live loader actually queries.
func TestOfflineSourcingHistoryLanesMatchLiveShortlist(t *testing.T) {
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	completed := probeHistoryRow(in, "amsterdam", in.Now.Add(-2*time.Hour))
	completed.ID = "shortlist-completion"
	completed.Lane = "amsterdam|portsmouth|sawn_planks|charter"
	putProbeHistorySettlement(t, h, completed)
	offShortlist := probeHistoryRow(in, "amsterdam", in.Now.Add(-2*time.Hour))
	offShortlist.ID = "off-shortlist-completion"
	offShortlist.Lane = "nowhere|portsmouth|sawn_planks|charter"
	putProbeHistorySettlement(t, h, offShortlist)
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	live := in
	if err := runner.loadSourcingHistory(h.ctx, &live); err != nil {
		t.Fatal(err)
	}
	lanes := SourcingHistoryLanes(live, []ShipState{live.Ship})
	const shortlistLane = "amsterdam|portsmouth|sawn_planks|charter"
	if len(lanes) == 0 || !slices.Contains(lanes, shortlistLane) {
		t.Fatalf("exported shortlist lost the live open-charter lane: %+v", lanes)
	}
	if slices.Contains(lanes, offShortlist.Lane) {
		t.Fatalf("exported shortlist invented a lane outside live admission: %+v", lanes)
	}
	if len(live.SourcingProbeCompletions) != 1 {
		t.Fatalf("live query did not stay on the shortlist: %+v", live.SourcingProbeCompletions)
	}
	if observed, exists := live.SourcingProbeCompletions[shortlistLane]; !exists || observed.IsZero() {
		t.Fatalf("live query missed exactly the shortlisted completion: %+v", live.SourcingProbeCompletions)
	}
	// A zero rolling conversion budget does not disable main's independent
	// protected-free-cash scout allowance or the lanes it still needs.
	guarded := live
	zero := int64(0)
	guarded.SourcingAllowancePence = &zero
	guarded.SourcingIdleEligible = false
	if got := SourcingHistoryLanes(guarded, []ShipState{guarded.Ship}); len(got) == 0 {
		t.Fatal("scout allowance must retain its sourcing history lanes")
	}
	guarded.SourcingProbeCostsUnknown = true
	if got := SourcingHistoryLanes(guarded, []ShipState{guarded.Ship}); got != nil {
		t.Fatalf("unpriced expenses disable both sourcing and scouting lanes: %+v", got)
	}
}

func TestOfflineSourcingLedgerMatchesDurableHistory(t *testing.T) {
	for _, candidate := range []struct {
		name      string
		departure bool
		laden     bool
	}{
		{name: "failed source"},
		{name: "empty departure preserves failure", departure: true},
		{name: "laden departure converts probe", departure: true, laden: true},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			h := newRecoveryHarness(t)
			in := probeHistoryLiveInput(t)
			in.CompanyID = h.account.CompanyID
			detail := in.Ports["le_havre"]
			detail.CoffeeHouseGossip = nil
			in.Ports["le_havre"] = detail
			row := probeHistoryRow(in, "amsterdam", in.Now.Add(-time.Hour))
			row.ID = "offline-failed-source"
			row.ExpensesPence = 600
			row.ProvisionsPence = 200
			row.ExplorationExpensePence = new(int64(700))
			putProbeHistorySettlement(t, h, row)
			if candidate.departure {
				manifest := make(map[string]int64)
				if candidate.laden {
					manifest["sawn_planks"] = 1
				}
				payload := encode(map[string]any{
					"company_id": in.CompanyID,
					// Live admission must order the host receipt, not this
					// server timestamp preceding the completed inspection.
					"timestamp": in.Now.Add(-2 * time.Hour),
					"payload": map[string]any{
						"ship_id": in.Ship.Ship.ID, "origin_port_id": "amsterdam",
						"cargo_manifest": manifest,
					},
				})
				if err := h.db.RecordEvent(h.ctx, h.account.ID, "offline-source-departure", "ship_departed", payload); err != nil {
					t.Fatal(err)
				}
			}
			// Reload the encrypted fixtures so both sides use PostgreSQL's
			// receipt/completion precision and timestamp representation.
			rows, events, err := h.db.ProbeHistorySince(h.ctx, h.account.ID, store.ProbeHistoryCursor{})
			if err != nil || len(rows) != 1 {
				t.Fatalf("reload durable probe fixture: rows=%d err=%v", len(rows), err)
			}
			departures := make([]SourcingDeparture, 0, len(events))
			for _, event := range events {
				departures = append(departures, SourcingDeparture{
					ShipID: in.Ship.Ship.ID, Origin: "amsterdam",
					At: event.CreatedAt, Laden: candidate.laden,
				})
			}
			wantDepartures := 0
			if candidate.departure {
				wantDepartures = 1
			}
			if len(departures) != wantDepartures {
				t.Fatalf("durable departure fixture count = %d, want %d", len(departures), wantDepartures)
			}
			in.Now = time.Now().UTC()
			in.Clock.RealUTC = in.Now
			for key := range in.Metadata {
				in.Metadata[key] = sourcingFreshMeta(in.Now)
			}
			runner := h.runner(primaryTestKey)
			probeHistoryRunner(runner, in)
			live := in
			if err := runner.loadSourcingHistory(h.ctx, &live); err != nil {
				t.Fatal(err)
			}
			history, err := runner.readProbeOutcomes(h.ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			wantFailures := 1
			if candidate.laden {
				wantFailures = 0
				converted := history.Sources[probeSourceKey("amsterdam", "sawn_planks")].ConvertedAt
				if !converted.Equal(departures[0].At) {
					t.Fatalf("live durable ingestion did not convert at the host receipt: %v", converted)
				}
			}
			if got := history.Ships[in.Ship.Ship.ID].Failures; got != wantFailures {
				t.Fatalf("live durable departure failures = %d, want %d", got, wantFailures)
			}
			// Durable cursor IDs describe storage position, not admission economics.
			history.Cursor = store.ProbeHistoryCursor{}
			live.SourcingProbeHistory = history
			offline := in
			if err := NewSourcingLedger(rows, departures).Apply(&offline, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(live.SourcingProbeHistory, offline.SourcingProbeHistory) {
				t.Fatalf("database-free probe history differs from durable accounting: live=%+v offline=%+v", live.SourcingProbeHistory, offline.SourcingProbeHistory)
			}
			if live.SourcingProbeExpensePence != 700 || offline.SourcingProbeExpensePence != live.SourcingProbeExpensePence ||
				live.SourcingAllowancePence == nil || offline.SourcingAllowancePence == nil ||
				*offline.SourcingAllowancePence != *live.SourcingAllowancePence ||
				sourcingAllowance(offline) != sourcingAllowance(live) ||
				offline.SourcingProbeCostsUnknown != live.SourcingProbeCostsUnknown {
				t.Fatalf("offline rolling expense/allowance differs from live computation: live=%+v offline=%+v", live, offline)
			}
			for _, ask := range []int64{0, 900} {
				want := sourcingProbeRejection(live, "amsterdam", live.Contracts[0], ask)
				if got := sourcingProbeRejection(offline, "amsterdam", offline.Contracts[0], ask); got != want {
					t.Fatalf("cooldown/streak admission differs for ask %d: got %q want %q", ask, got, want)
				}
			}
		})
	}
}

func TestOfflineSourcingLedgerRetainsTwoProbeStreak(t *testing.T) {
	h := newRecoveryHarness(t)
	in := probeHistoryLiveInput(t)
	first := probeHistoryRow(in, "amsterdam", in.Now.Add(-2*time.Hour))
	first.ID = "offline-first-failure"
	second := probeHistoryRow(in, "portsmouth", in.Now.Add(-time.Hour))
	second.ID = "offline-second-failure"
	putProbeHistorySettlement(t, h, first)
	putProbeHistorySettlement(t, h, second)
	rows, _, err := h.db.ProbeHistorySince(h.ctx, h.account.ID, store.ProbeHistoryCursor{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("reload durable probe fixtures: rows=%d err=%v", len(rows), err)
	}
	first, second = rows[0], rows[1]
	runner := h.runner(primaryTestKey)
	probeHistoryRunner(runner, in)
	liveInput := in
	if err := runner.loadSourcingHistory(h.ctx, &liveInput); err != nil {
		t.Fatal(err)
	}
	live := liveInput.SourcingProbeHistory
	offline := in
	if err := NewSourcingLedger([]store.Settlement{second, first}, nil).Apply(&offline, nil); err != nil {
		t.Fatal(err)
	}
	live.Cursor = store.ProbeHistoryCursor{}
	if !reflect.DeepEqual(live, offline.SourcingProbeHistory) {
		t.Fatalf("unordered tape settlements lost durable failure streak: live=%+v offline=%+v", live, offline.SourcingProbeHistory)
	}
	if reason := sourcingProbeRejection(offline, "untried-source", offline.Contracts[0], 0); reason != failedProbeStreakReason {
		t.Fatalf("two permanent failures escaped ship-level admission gate: %q", reason)
	}
}

func TestOfflineStoredDecisionFrameReproducesRecordedPlan(t *testing.T) {
	h := newRecoveryHarness(t)
	h.api.factorPorts = []string{recoveryFarPort}
	h.api.setFleet([]map[string]any{destinationShipRow(recoveryShipBlastoise, "Blastoise", recoveryHomePort, "berthed", 3, nil, nil)})
	runner := replayRecorderRunner(t, h)
	runner.opts.Execute = false
	if _, err := h.tick(runner); err != nil {
		t.Fatal(err)
	}
	input := runner.planningInput(runner.stateCache.Ships[0])
	input.Now = input.Now.UTC().Truncate(time.Microsecond)
	plans := PlanVoyages(input)
	if len(plans) == 0 || !plans[0].Accepted {
		t.Fatalf("harness has no measured opportunity to reproduce: %+v", plans)
	}
	mission := &store.Mission{AccountID: h.account.ID, ShipID: input.Ship.Ship.ID, Phase: "provision", Generation: "offline-reproduction"}
	data := missionData{Plan: plans[0]}
	if err := runner.saveMission(h.ctx, mission, &data); err != nil {
		t.Fatal(err)
	}
	// This explicit decision is after the completed tick, outside its pass.
	// Give the standalone recording its own observation instant.
	runner.passStart = time.Time{}
	runner.flushReplay(h.ctx, input.Now)
	var frame store.ReplayFrame
	replayRecorderWait(t, func() bool {
		for _, recorded := range replayRecorderFrames(t, h) {
			if recorded.ObservedAt.Equal(input.Now) {
				frame = recorded
				return true
			}
		}
		return false
	})
	snapshot := make(map[string]json.RawMessage)
	metadata := make(map[string]DatasetMeta)
	grouped := map[string]map[string]json.RawMessage{"books": {}, "ports": {}, "routes": {}}
	for key, dataset := range frame.Datasets {
		metadata[key] = sourcingFixture[DatasetMeta](t, string(dataset.Meta))
		parts := strings.SplitN(key, "/", 2)
		if len(parts) == 2 && grouped[parts[0]] != nil {
			grouped[parts[0]][parts[1]] = json.RawMessage(dataset.Data)
		} else {
			snapshot[key] = json.RawMessage(dataset.Data)
		}
	}
	for key, values := range grouped {
		snapshot[key] = encode(values)
	}
	sources := PlanningSources{
		CompanyID: runner.account.CompanyID, ReservePence: runner.opts.ReservePence,
		Metadata: metadata, Generation: frame.Generation,
		CharterProceedsPence: decode[int64](snapshot, "_charter_proceeds"),
		Competition:          decode[*store.CompetitionSummary](snapshot, "_competition"),
	}
	recorded, err := h.db.ReplayPlans(h.ctx, h.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	var saved Plan
	found := false
	for _, row := range recorded {
		if row.MissionID == mission.Generation {
			if err := json.Unmarshal(row.Plan, &saved); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("saveMission did not retain its recorded plan")
	}
	state := StateFromSnapshot(frame.ObservedAt, snapshot, sources)
	var ship ShipState
	shipFound := false
	for _, candidate := range state.Ships {
		if candidate.Ship.ID == saved.ShipID {
			ship, shipFound = candidate, true
			break
		}
	}
	if !shipFound {
		t.Fatalf("stored decision frame lacks the recorded plan's ship %q", saved.ShipID)
	}
	replayed := PlanVoyages(PlanningInputFor(frame.ObservedAt, ship, state, snapshot, sources))
	// Compare the persisted/public Plan, not private planner scratch flags
	// such as competitionComparable that deliberately have no JSON field.
	if !found || len(replayed) == 0 || !bytes.Equal(encode(replayed[0]), encode(saved)) {
		t.Fatalf("persisted S2 frame did not reproduce saveMission plan: found=%v replayed=%s saved=%+v", found, fmt.Sprint(replayed), saved)
	}
}
