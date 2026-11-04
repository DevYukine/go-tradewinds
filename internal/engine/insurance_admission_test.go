package engine

// Acceptance test for planning-time departure cover (planner.md D4): an
// oceanic voyage the departure insurance rule would insure must price that
// premium before the cargo is bought, so a lot whose expected net cannot carry
// the premium is rejected at planning, while coastal-only and self-insured
// voyages keep their economics unchanged. Written before the implementation;
// the failing assertion names the observable that must change.

import (
	"reflect"
	"testing"
)

// oceanicPremiumFixture builds a marginal four-ton cloth lot to portsmouth
// whose receipts come from the retained memory, over an oceanic or coastal
// route. The lane's recorded Lloyd's terms carry 350/400 bps premium on a
// 2,000,000p hull that Lloyd's writes, so the departure rule's premium basis
// points are the laden 400 on hull plus cargo, and the fixture balances decide
// whether that insured value stays above the free cash left after funding this
// lot.
func oceanicPremiumFixture(t *testing.T, edgeClass string, balances int64) (PlanningInput, Hull, Port, Plan) {
	t.Helper()
	in := sourcingInputFixture(t)
	in.Catalog.ShipClasses[0].HullCategory = "oceanic"
	key := routeKey("le_havre", "portsmouth", "hoy")
	route := in.Routes[key]
	route.Legs[0].EdgeClass = edgeClass
	in.Routes[key] = route
	delete(in.Markets, "portsmouth")
	delete(in.Metadata, "books/portsmouth")
	in.QuayBookMemory = map[string]RetainedQuayBook{"portsmouth": {
		ObservedAt: in.Now.Add(-gameDuration(0.1, in.Clock.Calendar.TimeScale)),
		Bids:       map[string][]RetainedBid{"coarse_cloth": {{PricePence: 44_726, RemainingQuantity: 66}}},
	}}
	book := in.Markets["le_havre"]
	book.CustomsBPS = 0
	book.Books = append(book.Books, sourcingFixture[CommodityBook](t,
		`{"commodity_id":"coarse_cloth","depth":{"asks":[{"order_id":"cloth-ask","price_pence":42000,"remaining_quantity":100}]}}`))
	in.Markets["le_havre"] = book
	in.Balances["channel"] = balances
	for id, quote := range in.HazardQuotes {
		if quote.Destination != "portsmouth" {
			continue
		}
		quote.ExpectedLossBPS = 350
		quote.LloydsPremiumBPS = new(int64(350))
		quote.HullValuePence = 2_000_000
		quote.LloydsWrites = true
		in.HazardQuotes[id] = quote
		laden := quote
		laden.Laden = true
		laden.ExpectedLossBPS = 400
		laden.LloydsPremiumBPS = new(int64(400))
		ladenKey := hazardLadenKey("le_havre", "portsmouth", "hoy", in.Ship.Ship.CannonsCount, in.Clock.Calendar.Month)
		in.HazardQuotes[ladenKey] = laden
	}
	candidate := Plan{Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre",
		Destination: "portsmouth", Commodity: "coarse_cloth", Quantity: 4}
	return in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate
}

func TestOceanVoyageMarginalLotIsRejectedWithoutPricedCover(t *testing.T) {
	insured, hull, origin, candidate := oceanicPremiumFixture(t, "oceanic", 1_000_000)
	coastal, coastalHull, coastalOrigin, coastalCandidate := oceanicPremiumFixture(t, "coastal", 1_000_000)
	selfInsured, selfHull, selfOrigin, selfCandidate := oceanicPremiumFixture(t, "oceanic", 5_000_000)
	quoted := evaluatePlan(insured, hull, origin, candidate, 0, 0)
	coastalQuoted := evaluatePlan(coastal, coastalHull, coastalOrigin, coastalCandidate, 0, 0)
	selfQuoted := evaluatePlan(selfInsured, selfHull, selfOrigin, selfCandidate, 0, 0)
	if !coastalQuoted.Accepted || !selfQuoted.Accepted {
		t.Fatalf("coastal and self-insured fixture lots must stay executable: coastal accepted=%v, self-insured accepted=%v",
			coastalQuoted.Accepted, selfQuoted.Accepted)
	}
	if !reflect.DeepEqual(coastalQuoted.Economics, selfQuoted.Economics) {
		t.Fatalf("self-insured ocean economics diverged from the coastal baseline: coastal=%+v self-insured=%+v",
			coastalQuoted.Economics, selfQuoted.Economics)
	}
	insuredValue := int64(2_000_000)
	freeAfterLot := sourcingFreeCash(insured) - quoted.Economics.PurchasePence - quoted.Economics.CustomsPence - quoted.Economics.StoreCashPence
	if quoted.Accepted && freeAfterLot >= insuredValue {
		t.Fatalf("insured fixture leaves %d free cash after the lot, above the %d insurable value", freeAfterLot, insuredValue)
	}
	if selfQuoted.Accepted {
		selfFree := sourcingFreeCash(selfInsured) - selfQuoted.Economics.PurchasePence - selfQuoted.Economics.CustomsPence - selfQuoted.Economics.StoreCashPence
		if selfFree < insuredValue {
			t.Fatalf("self-insured fixture leaves only %d free cash after the lot, below the %d insurable value", selfFree, insuredValue)
		}
	}
	if quoted.Accepted {
		t.Fatalf("oceanic lot with %d expected net below its 400 bps departure premium on %d insurable value was admitted without pricing hull+cargo cover (hazard %d bps charged %d): %+v",
			quoted.Economics.ExpectedNetPence, insuredValue, quoted.Economics.HazardBPS, quoted.Economics.HazardPence, quoted)
	}
	// The priced premium must live in its own fixed economics field, absent
	// from the coastal and self-insured lots whose economics stay unchanged.
	insuredEconomics, coastalEconomics := reflect.ValueOf(quoted.Economics), reflect.ValueOf(coastalQuoted.Economics)
	selfEconomics := reflect.ValueOf(selfQuoted.Economics)
	premiumFields := 0
	for i := range insuredEconomics.NumField() {
		field := insuredEconomics.Field(i)
		if field.Kind() != reflect.Int64 || !field.CanInterface() {
			continue
		}
		if field.Int() > 0 && coastalEconomics.Field(i).Int() == 0 && selfEconomics.Field(i).Int() == 0 {
			premiumFields++
		}
	}
	if premiumFields == 0 {
		t.Fatalf("departure premium was not recorded as its own economics field for the insured oceanic lot: %+v", quoted.Economics)
	}
}
