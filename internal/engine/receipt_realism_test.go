package engine

// Acceptance tests for spot receipt realism. Retained
// destination bids must be haircut for their age on arrival (not departure)
// when the fit is not retained-specific. Retained calibration cells already
// include age decay and must not double count it.

import (
	"strings"
	"testing"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// retainedClothFixture deletes the portsmouth quay book so the destination
// receipts come from the retained memory: one 66t bid observed half a game day
// before departure, bought from fresh le_havre cloth asks.
func retainedClothFixture(t *testing.T, ask int64) (PlanningInput, Hull, Port, Plan) {
	t.Helper()
	in := sourcingInputFixture(t)
	delete(in.Markets, "portsmouth")
	delete(in.Metadata, "books/portsmouth")
	in.QuayBookMemory = map[string]RetainedQuayBook{"portsmouth": {
		ObservedAt: in.Now.Add(-gameDuration(0.5, in.Clock.Calendar.TimeScale)),
		Bids:       map[string][]RetainedBid{"coarse_cloth": {{PricePence: 44_726, RemainingQuantity: 66}}},
	}}
	book := in.Markets["le_havre"]
	book.CustomsBPS = 0
	book.Books = append(book.Books, sourcingFixture[CommodityBook](t,
		`{"commodity_id":"coarse_cloth","depth":{"asks":[{"order_id":"cloth-ask","price_pence":40000,"remaining_quantity":100}]}}`))
	book.Books[len(book.Books)-1].Depth.Asks[0].PricePence = ask
	in.Markets["le_havre"] = book
	candidate := Plan{Kind: "spot", ShipID: in.Ship.Ship.ID, Origin: "le_havre",
		Destination: "portsmouth", Commodity: "coarse_cloth", Quantity: 4}
	return in, in.Catalog.ShipClasses[0], in.Catalog.Ports[0], candidate
}

func TestRetainedReceiptsAreHaircutAtArrival(t *testing.T) {
	in, hull, origin, candidate := retainedClothFixture(t, 40_000)
	departure := pricePlan(in, hull, origin, candidate, 0, 0)
	if !departure.Accepted || !strings.HasPrefix(departure.PriceSource, retainedBookSource) {
		t.Fatalf("departure-arrival retained fixture is not executable: %+v", departure)
	}
	// A nine-hundred-mile leg reaches portsmouth about eight game days later.
	key := routeKey("le_havre", "portsmouth", "hoy")
	in.Routes[key] = sourcingRoute("le_havre", "portsmouth", 900)
	later := pricePlan(in, hull, origin, candidate, 0, 0)
	if !strings.HasPrefix(later.PriceSource, retainedBookSource) {
		t.Fatalf("late-arrival fixture lost the retained basis: %+v", later)
	}
	if later.Economics.RevenueCalibrationBPS >= departure.Economics.RevenueCalibrationBPS ||
		later.Economics.RevenuePence >= departure.Economics.RevenuePence {
		t.Fatalf("retained receipts ignored their age on arrival (%.2f game days out): departure %dp at %d bps, later arrival %dp at %d bps",
			later.Economics.GameDays, departure.Economics.RevenuePence, departure.Economics.RevenueCalibrationBPS,
			later.Economics.RevenuePence, later.Economics.RevenueCalibrationBPS)
	}
}

func TestRetainedPoolCellsDoNotDoubleCountArrivalAge(t *testing.T) {
	pool := store.RevenueCalibrationModel{Version: store.RevenueCalibrationVersion, Cells: map[string]store.RevenueCalibrationCell{
		"basis/retained/<1":   {Samples: 30, MedianBPS: 8200, AdverseBPS: 8200},
		"basis/retained/2.5+": {Samples: 30, MedianBPS: 8200, AdverseBPS: 8200},
		"global":              {Samples: 30, MedianBPS: 9500, AdverseBPS: 9500},
	}}
	in, hull, origin, candidate := retainedClothFixture(t, 40_000)
	in.RevenueCalibration = pool
	departure := pricePlan(in, hull, origin, candidate, 0, 0)
	if departure.Economics.RevenueCalibrationBPS != 8200 || departure.Economics.RevenueAgeAddonBPS != 0 {
		t.Fatalf("pooled retained cell was skipped at the departure horizon in favour of %q: %+v",
			departure.Economics.RevenueCalibrationReason, departure.Economics)
	}
	key := routeKey("le_havre", "portsmouth", "hoy")
	in.Routes[key] = sourcingRoute("le_havre", "portsmouth", 900)
	later := pricePlan(in, hull, origin, candidate, 0, 0)
	if later.Economics.RevenueCalibrationBPS != 8200 || later.Economics.RevenueAgeAddonBPS != 0 ||
		later.Economics.RevenuePence != departure.Economics.RevenuePence {
		t.Fatalf("pooled retained receipts counted arrival-age decay twice: departure %+v, later %+v",
			departure.Economics, later.Economics)
	}
	delete(pool.Cells, "basis/retained/<1")
	delete(pool.Cells, "basis/retained/2.5+")
	pool.Cells["basis/retained"] = store.RevenueCalibrationCell{Samples: 30, MedianBPS: 8200, AdverseBPS: 8200}
	basisOnly := pricePlan(in, hull, origin, candidate, 0, 0)
	if basisOnly.Economics.RevenueCalibrationBPS != 8200 || basisOnly.Economics.RevenueAgeAddonBPS != 0 {
		t.Fatalf("basis-only retained pool counted arrival-age decay twice: %+v", basisOnly.Economics)
	}
	// Measured commodity retained cells already embed their book age and stay
	// exempt from the generic drift.
	commodity := pool
	commodity.Cells["basis/retained/commodity/coarse_cloth/<1"] = store.RevenueCalibrationCell{Samples: 30, MedianBPS: 9000, AdverseBPS: 9000}
	commodity.Cells["basis/retained/commodity/coarse_cloth/2.5+"] = store.RevenueCalibrationCell{Samples: 30, MedianBPS: 9000, AdverseBPS: 9000}
	in.RevenueCalibration = commodity
	in.Routes[key] = sourcingRoute("le_havre", "portsmouth", 30)
	commodityDeparture := pricePlan(in, hull, origin, candidate, 0, 0)
	in.Routes[key] = sourcingRoute("le_havre", "portsmouth", 900)
	commodityLater := pricePlan(in, hull, origin, candidate, 0, 0)
	if commodityDeparture.Economics.RevenueCalibrationBPS != 9000 || commodityLater.Economics.RevenueCalibrationBPS != 9000 {
		t.Fatalf("measured commodity retained cell lost its exemption from the age drift addon: departure %d bps, later arrival %d bps",
			commodityDeparture.Economics.RevenueCalibrationBPS, commodityLater.Economics.RevenueCalibrationBPS)
	}
}
