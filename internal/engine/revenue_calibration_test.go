package engine

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

func revenueCalibrationFixture(median, adverse int64) store.RevenueCalibrationModel {
	cell := store.RevenueCalibrationCell{Samples: 30, MedianBPS: median, AdverseBPS: adverse}
	return store.RevenueCalibrationModel{Cells: map[string]store.RevenueCalibrationCell{"global": cell, "contract": cell}}
}

func TestCalibratedAdmissionUsesOneBoundaryForEveryRevenueBasis(t *testing.T) {
	for _, basis := range []string{"depth", "retained", "gossip", "contract"} {
		for _, tc := range []struct {
			name     string
			net      int64
			stress   int64
			accepted bool
		}{
			{"zero stressed net", 200, 200, false},
			{"one stressed penny and two percent margin", 200, 199, true},
			{"one penny below two percent", 199, 0, false},
			{"two percent exactly", 200, 0, true},
		} {
			t.Run(basis+"/"+tc.name, func(t *testing.T) {
				plan := Plan{Kind: "spot", Economics: Economics{RevenueBasis: basis, PurchasePence: 9999, CustomsPence: 1, ExpectedNetPence: tc.net, PriceUncertaintyStressPence: tc.stress}}
				if got := admissionRejection(plan, tc.net) == ""; got != tc.accepted {
					t.Fatalf("admission=%t, want %t: %+v", got, tc.accepted, plan)
				}
				plan.Accepted = true
				plan.Economics.GameDays = 1
				if got := addFundingFees(plan, 0).Accepted; got != tc.accepted {
					t.Fatalf("funding changed %s admission: got %t, want %t", basis, got, tc.accepted)
				}
			})
		}
	}
}

func TestCalibrationPricesMedianAndAdverseReceiptsWithoutDoubleHaircut(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 6000, 10000, 1, 30)
	in.RevenueCalibration = revenueCalibrationFixture(8000, 7000)
	plan := moneySpot(t, in, "canvas_sails")
	if !plan.Accepted || plan.Economics.RevenuePence != 8000 || plan.Economics.ExpectedNetPence != 1999 ||
		plan.Economics.PriceUncertaintyStressPence != 1000 || !strings.Contains(plan.Economics.RevenueCalibrationReason, "3000 bps") {
		t.Fatalf("calibrated receipt distribution was clamped or charged twice: %+v", plan)
	}
	book := in.Markets["amsterdam"]
	book.Books[0].Depth.Asks[0].PricePence = 7000
	in.Markets["amsterdam"] = book
	if rejected := moneySpot(t, in, "canvas_sails"); rejected.Accepted {
		t.Fatalf("fresh depth bypassed fitted adverse receipts: %+v", rejected)
	}
}

func TestCalibrationContractRepriceReplacesPriorStress(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 6000, 10000, 1, 30)
	in.RevenueCalibration = revenueCalibrationFixture(9000, 8000)
	hull, _ := findHull(in.Catalog, "hoy")
	origin, _ := findPort(in.Catalog, "amsterdam")
	plan := evaluatePlan(in, hull, origin, Plan{
		Kind: "charter", ShipID: in.Ship.Ship.ID, Origin: origin.ID, Destination: "hamburg",
		Commodity: "canvas_sails", Quantity: 1, PriceSource: "live contract terms",
		Economics: Economics{PriceUncertaintyStressPence: 5000},
	}, 10000, 0)
	if !plan.Accepted || plan.Economics.ExpectedNetPence != 2999 || plan.Economics.PriceUncertaintyStressPence != 1000 {
		t.Fatalf("reprice compounded obsolete receipt uncertainty: %+v", plan)
	}
}

func TestRetainedDriftUsesCommodityGroupBeforeGlobalPool(t *testing.T) {
	var samples []BookDriftSample
	for range 30 {
		samples = append(samples, BookDriftSample{CommodityID: "raw_wool", AgeGameDays: 1, TopBidChangeBPS: -200})
		samples = append(samples, BookDriftSample{CommodityID: "coal", AgeGameDays: 1, TopBidChangeBPS: -3000})
	}
	if got := retainedCommodityDriftBPS(samples, "canvas_sails"); got != 200 {
		t.Fatalf("bulk volatility leaked into textile age add-on: %v", got)
	}
	if got := retainedCommodityDriftBPS(samples, "new_commodity"); got != 3000 {
		t.Fatalf("unseen group did not use the measured global pool: %v", got)
	}
}

func TestCalibrationPricesEveryBasisAtSameStressedBoundary(t *testing.T) {
	for _, basis := range []string{"depth", "retained", "gossip", "contract"} {
		stress := int64(1000)
		if basis == "gossip" {
			stress = 2000
		}
		for _, ask := range []int64{7998 - stress, 7999 - stress} {
			in := moneyInput(t, time.Now().UTC(), "canvas_sails", ask, 10000, 1, 30)
			in.RevenueCalibration = revenueCalibrationFixture(8000, 7000)
			if basis == "retained" || basis == "gossip" {
				delete(in.Markets, "hamburg")
				delete(in.Metadata, "books/hamburg")
			}
			if basis == "retained" {
				in.QuayBookMemory = map[string]RetainedQuayBook{"hamburg": {
					ObservedAt: in.Now,
					Bids:       map[string][]RetainedBid{"canvas_sails": {{PricePence: 10000, RemainingQuantity: 1}}},
				}}
			} else if basis == "gossip" {
				detail := in.Ports["amsterdam"]
				detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "hamburg", SnapshotAt: in.Now,
					Commodities: []GossipCommodity{{CommodityID: "canvas_sails", SnapshotAt: in.Now, BestBidPence: new(int64(10000))}}}}
				in.Ports["amsterdam"] = detail
			}
			var plan Plan
			if basis == "contract" {
				hull, _ := findHull(in.Catalog, "hoy")
				origin, _ := findPort(in.Catalog, "amsterdam")
				plan = evaluatePlan(in, hull, origin, Plan{Kind: "charter", ShipID: in.Ship.Ship.ID,
					Origin: origin.ID, Destination: "hamburg", Commodity: "canvas_sails", Quantity: 1}, 10000, 0)
			} else {
				plan = moneySpot(t, in, "canvas_sails")
			}
			if plan.Accepted != (ask == 7998-stress) || plan.Economics.RevenueBasis != basis ||
				plan.Economics.RevenuePence != 8000 || plan.Economics.PriceUncertaintyStressPence != stress {
				t.Fatalf("%s ask %d lost the maximum of fitted and executable fallback stress: %+v", basis, ask, plan)
			}
		}
	}
}

func TestPaidRetainedValuationKeepsCalibratedAgeAdjustment(t *testing.T) {
	in := retainedBookInput(t, 10, RetainedBid{PricePence: 1000, RemainingQuantity: 20})
	in.RevenueCalibration = revenueCalibrationFixture(10000, 10000)
	arrival := in.Now.Add(gameDuration(1, in.Clock.Calendar.TimeScale))
	raw, _, _, _, reason, _ := destinationRevenue(in, "portsmouth", "provisions", 20, arrival)
	paid, _, _, paidReason := paidDestinationRevenue(in, "portsmouth", "provisions", 20, arrival)
	if reason != "" || paidReason != "" || raw != 20000 || paid != 18200 || paid >= 19000 {
		t.Fatalf("paid retained cargo lost its age-adjusted comparison with 19000p local liquidation: raw=%d paid=%d reason=%q/%q", raw, paid, reason, paidReason)
	}
}

func TestPaidRetainedValuationDoesNotLiquidateOnUnmeasuredAdmissionPrior(t *testing.T) {
	for _, tc := range []struct {
		age  float64
		want int64
	}{{0, 20000}, {10, 18200}} {
		in := retainedBookInput(t, tc.age, RetainedBid{PricePence: 1000, RemainingQuantity: 20})
		in.RevenueCalibration = store.RevenueCalibrationModel{}
		arrival := in.Now.Add(gameDuration(1, in.Clock.Calendar.TimeScale))
		paid, _, _, reason := paidDestinationRevenue(in, "portsmouth", "provisions", 20, arrival)
		if reason != "" || paid != tc.want || in.RevenueCalibration.Factor("provisions", "retained", 1).MedianBPS != 10_000 {
			t.Fatalf("unmeasured calibration changed paid recovery: age=%v paid=%d reason=%q", tc.age, paid, reason)
		}
	}
}

func TestCalibratedOnwardSizingDoesNotMultiplyFixedCollateralHazard(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 400, 31, 30)
	in.singleLegOnly = true
	in.Contracts = []Contract{{ID: "freight", ContractType: "freight", RewardPence: 10000}}
	for key, quote := range in.HazardQuotes {
		quote.ExpectedLossBPS = 1000
		in.HazardQuotes[key] = quote
	}
	hull, _ := findHull(in.Catalog, "hoy")
	origin, _ := findPort(in.Catalog, "amsterdam")
	plan := sizePlan(in, hull, origin, Plan{Kind: "spot+freight", ShipID: in.Ship.Ship.ID,
		Origin: origin.ID, Destination: "hamburg", Commodity: "canvas_sails", FreightIDs: []string{"freight"},
		Economics: Economics{CollateralPence: 10000}}, 30, 0, 10000, 1)
	if plan.onwardPriority <= 0 || math.IsInf(plan.onwardPriority, 0) || plan.onwardSizing.maximum != 30 {
		t.Fatalf("fixed collateral hazard starved a profitable mixed lot: %+v", plan)
	}
	empty := sizePlan(in, hull, origin, plan, 0, 0, 0, 31)
	if empty.Accepted || empty.onwardSizing != (planSizing{}) || !math.IsInf(empty.onwardPriority, -1) {
		t.Fatalf("impossible mixed load inherited an earlier sizing commitment: %+v", empty)
	}
}

func TestCalibratedOnwardGossipProbeCannotProjectUnobservedQuantity(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 100, 400, 30, 30)
	in.singleLegOnly = true
	delete(in.Markets, "hamburg")
	delete(in.Metadata, "books/hamburg")
	detail := in.Ports["amsterdam"]
	detail.CoffeeHouseGossip = []Gossip{{SubjectPortID: "hamburg", SnapshotAt: in.Now,
		Commodities: []GossipCommodity{{CommodityID: "canvas_sails", SnapshotAt: in.Now, BestBidPence: new(int64(400))}}}}
	in.Ports["amsterdam"] = detail
	hull, _ := findHull(in.Catalog, "hoy")
	origin, _ := findPort(in.Catalog, "amsterdam")
	plan := sizePlan(in, hull, origin, Plan{Kind: "spot", ShipID: in.Ship.Ship.ID,
		Origin: origin.ID, Destination: "hamburg", Commodity: "canvas_sails"}, 30, 0, 0, 0)
	if !plan.Accepted || plan.onwardSizing.maximum != 1 {
		t.Fatalf("one-ton gossip evidence reserved a whole-hold shortlist slot: %+v", plan)
	}
}

func TestPaidFundingKeepsReceiptStressWithoutSecondMarginGate(t *testing.T) {
	for _, tc := range []struct {
		net, stress int64
		accepted    bool
	}{{0, 0, true}, {1, 0, true}, {100, 100, false}, {101, 100, true}} {
		plan := Plan{Accepted: true, EventPriceStress: true, sunkFundingFeesPence: 1000,
			Economics: Economics{ExpectedNetPence: tc.net - 1000, PriceUncertaintyStressPence: tc.stress,
				PurchasePence: 100000, GameDays: 1}}
		if got := addFundingFees(plan, 0).Accepted; got != tc.accepted {
			t.Fatalf("paid funding remaining=%d stress=%d accepted=%t, want %t", tc.net, tc.stress, got, tc.accepted)
		}
	}
}

func TestUnmeasuredCalibrationDoesNotBlockThinPositiveLot(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 9700, 10000, 1, 30)
	in.RevenueCalibration = store.RevenueCalibrationModel{}
	plan := moneySpot(t, in, "canvas_sails")
	if !plan.Accepted || plan.Economics.ExpectedNetPence != 299 || plan.Economics.RevenueCalibrationBPS != 10_000 {
		t.Fatalf("unmeasured prior trapped a profitable lot before it could create evidence: %+v", plan)
	}
}

func TestUnmeasuredRetainedAdmissionDoesNotStoreALiquidationHaircut(t *testing.T) {
	in := moneyInput(t, time.Now().UTC(), "canvas_sails", 6000, 10000, 1, 30)
	in.RevenueCalibration = store.RevenueCalibrationModel{}
	delete(in.Markets, "hamburg")
	delete(in.Metadata, "books/hamburg")
	in.QuayBookMemory = map[string]RetainedQuayBook{"hamburg": {
		ObservedAt: in.Now, Bids: map[string][]RetainedBid{"canvas_sails": {{PricePence: 10000, RemainingQuantity: 1}}},
	}}
	plan := moneySpot(t, in, "canvas_sails")
	delete(in.QuayBookMemory, "hamburg")
	revenue, known := paidSpotRevenue(in, plan, 1)
	if !plan.Accepted || plan.SellLimitPence != 10000 || !known || revenue != 10000 {
		t.Fatalf("an unmeasured admission haircut leaked into the paid-cargo floor: plan=%+v revenue=%d known=%t", plan, revenue, known)
	}
}

func TestPaidRetainedPreEventQuoteAgesUntilArrival(t *testing.T) {
	in := retainedBookInput(t, 10.0/24, RetainedBid{PricePence: 1000, RemainingQuantity: 20})
	in.RevenueCalibration = store.RevenueCalibrationModel{}
	in.MarketShocks = map[string]map[string][]MarketShock{"portsmouth": {"provisions": {{
		ID: "temporary-support", EventType: "demand_shortage", StartsAt: in.Now.Add(-time.Minute), ExpiresAt: in.Now.Add(time.Minute),
	}}}}
	arrival := in.Now.Add(gameDuration(2, in.Clock.Calendar.TimeScale))
	revenue, _, _, reason := paidDestinationRevenue(in, "portsmouth", "provisions", 20, arrival)
	if reason != "" || revenue != 19716 {
		t.Fatalf("pre-event recovery used departure age instead of the independent 142-bps arrival-age miss: revenue=%d reason=%q", revenue, reason)
	}
}

func TestRetainedSpecificCalibrationDoesNotChargeAgeTwice(t *testing.T) {
	in := retainedBookInput(t, 10, RetainedBid{PricePence: 1000, RemainingQuantity: 20})
	in.RevenueCalibration = store.RevenueCalibrationModel{Cells: map[string]store.RevenueCalibrationCell{
		"basis/retained/group/provisions/1-1.5": {Samples: 30, MedianBPS: 9000, AdverseBPS: 8000},
	}}
	arrival := in.Now.Add(gameDuration(1, in.Clock.Calendar.TimeScale))
	revenue, limit, _, reason := paidDestinationRevenue(in, "portsmouth", "provisions", 20, arrival)
	if reason != "" || revenue != 18000 || limit != 800 {
		t.Fatalf("retained-only receipts were aged a second time: revenue=%d limit=%d reason=%q", revenue, limit, reason)
	}
}
