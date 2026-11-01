package engine

// Required production surface:
//   type expansionRateEstimate with snake_case JSON fields including
//   ask_pence, spread_pence, haircut_bps, round_trip_game_days, customs_pence,
//   provisions_pence, berth_dues_pence, hazard_bps, hazard_pence,
//   hazard_source and rate_pence_per_game_day; Basis "gossip estimate"
//   func expansionGossipEstimate(input PlanningInput, yard, partner Port, hull Hull,
//       prices map[string]expansionPrice) (expansionRateEstimate, string)

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// overseasGossipFixture reuses the planner hazard fixture: a coastal hoy with
// fresh symmetric origin/destination routes (two 15 NM legs each way, 0.1 safe
// tons per leg), four effective berths, empty queues and no dues at both
// harbours. The hoy holds 40 tons, plans four cannons (8 tons) and needs ten
// crew. Both route legs arrive in the channel zone (storm 9999 + piracy 200),
// and the zone hazard needs the growth-candidate identity plus a calibration
// whose bounds cover the fixture routes.
func overseasGossipFixture() (PlanningInput, Port, Port, Hull) {
	in := hazardInputFixture()
	in.Catalog.Commodities = []Commodity{
		{ID: "grain", BasePricePence: 100},
		{ID: "provisions", BasePricePence: 50},
	}
	for i := range in.Catalog.Ports {
		in.Catalog.Ports[i].SeaZoneID = "channel"
	}
	in.Ship.Ship.ID = "growth-candidate"
	in.hazardEstimate = &hazardCalibration{k: 0.1, n: 12, longestLeg: 30, highestRisk: 10_199}
	return in, in.Catalog.Ports[0], in.Catalog.Ports[1], in.Catalog.ShipClasses[0]
}

// overseasGossipPrices quotes grain in one direction only, so the returned
// estimate is always the forward origin -> destination lot.
func overseasGossipPrices() map[string]expansionPrice {
	return map[string]expansionPrice{
		"origin|grain":      {Ask: 100},
		"destination|grain": {Bid: 300},
	}
}

func TestExpansionGossipEstimateExactHaircutLabelAndJSON(t *testing.T) {
	in, yard, partner, hull := overseasGossipFixture()
	estimate, reason := expansionGossipEstimate(in, yard, partner, hull, overseasGossipPrices())
	if reason != "" {
		t.Fatalf("two-sided gossip produced no estimate: %q", reason)
	}
	if estimate.Basis != "gossip estimate" || estimate.Commodity != "grain" ||
		estimate.Origin != "origin" || estimate.Destination != "destination" {
		t.Fatalf("estimate label or direction is wrong: %+v", estimate)
	}
	// Usable hold 40 - 2*4 = 32 tons minus round-trip stores of about 1.21
	// tons leaves a 30-ton lot; 200 pence spread times 30 tons is 6,000 pence.
	if estimate.CargoTons != 30 || estimate.AskPence != 100 || estimate.SpreadPence != 6_000 || estimate.HaircutBPS != 5_000 {
		t.Fatalf("tons/ask/proceeds/haircut = %d/%d/%d/%d; want 30/100/6000/5000",
			estimate.CargoTons, estimate.AskPence, estimate.SpreadPence, estimate.HaircutBPS)
	}
	// The fresh origin book charges no tax, so customs is the one pence per
	// cargo ton; the 1.21-ton reservation rounds up to two bought store tons
	// at 50 pence plus their 2 pence store fee; both harbours charge no dues.
	if estimate.CustomsPence != 30 || estimate.ProvisionsPence != 102 || estimate.BerthDuesPence != 0 {
		t.Fatalf("customs/provisions/dues = %d/%d/%d; want 30/102/0",
			estimate.CustomsPence, estimate.ProvisionsPence, estimate.BerthDuesPence)
	}
	// Both legs arrive in the channel zone: risk 2*(9999+200) = 20398 at
	// k=0.1 gives 2040 bps on 3000p cargo plus 32p purchase customs and fees.
	if estimate.HazardBPS != 2_040 || estimate.HazardPence != 619 ||
		estimate.HazardSource != "zone estimate (k=0.1, n=12, Z=20398)" {
		t.Fatalf("hazard estimate = %d bps %dp %q; want 2040 bps 619p", estimate.HazardBPS, estimate.HazardPence, estimate.HazardSource)
	}
	sailing := 30.0 / (600.0 / 100 * 0.8) / 24
	wantDays := sailing + (1.0/24 + 30*4.0/1_440 + 0.0625) + sailing + (30.0/1_440 + 0.0625)
	if math.Abs(estimate.RoundTripGameDays-wantDays) > 1e-9 {
		t.Fatalf("round trip %v; want %v", estimate.RoundTripGameDays, wantDays)
	}
	// Exactly half the spread proceeds survive the haircut, then costs.
	net := estimate.SpreadPence - estimate.SpreadPence*estimate.HaircutBPS/10_000 -
		estimate.CustomsPence - estimate.ProvisionsPence - estimate.BerthDuesPence - estimate.HazardPence
	if net != 2_249 || estimate.RatePencePerGameDay != float64(net)/estimate.RoundTripGameDays {
		t.Fatalf("rate %v does not equal halved proceeds %d over %v game days",
			estimate.RatePencePerGameDay, net, estimate.RoundTripGameDays)
	}
	encoded, err := json.Marshal(estimate)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"basis", "commodity", "origin", "destination", "cargo_tons", "ask_pence",
		"spread_pence", "haircut_bps", "round_trip_game_days", "customs_pence", "provisions_pence",
		"berth_dues_pence", "hazard_bps", "hazard_pence", "hazard_source", "rate_pence_per_game_day"} {
		if _, exists := decoded[key]; !exists {
			t.Fatalf("estimate JSON lacks %q: %s", key, encoded)
		}
	}
	if len(decoded) != 16 || decoded["basis"] != "gossip estimate" {
		t.Fatalf("unexpected estimate JSON keys or basis: %s", encoded)
	}
}

func TestExpansionGossipEstimateNeedsBothGossipSides(t *testing.T) {
	in, yard, partner, hull := overseasGossipFixture()
	for name, prices := range map[string]map[string]expansionPrice{
		"ask without buyer bid":      {"origin|grain": {Ask: 100}},
		"bid without source ask":     {"destination|grain": {Bid: 300}},
		"buyer bid below source ask": {"origin|grain": {Ask: 300}, "destination|grain": {Bid: 100}},
	} {
		t.Run(name, func(t *testing.T) {
			estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
			if reason == "" || estimate != (expansionRateEstimate{}) {
				t.Fatalf("one-sided or crossed gossip produced %+v (reason %q); want a refusal", estimate, reason)
			}
		})
	}
}

func TestExpansionGossipEstimateRanksDirections(t *testing.T) {
	in, yard, partner, hull := overseasGossipFixture()
	prices := map[string]expansionPrice{
		"origin|grain":      {Ask: 100, Bid: 600},
		"destination|grain": {Ask: 50, Bid: 300},
	}
	estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
	// The reverse direction buys at the destination ask 50 and sells at the
	// origin bid 600; its purchase tax comes from the destination catalog rate.
	if reason != "" || estimate.Origin != "destination" || estimate.Destination != "origin" ||
		estimate.CargoTons != 30 || estimate.AskPence != 50 || estimate.SpreadPence != 16_500 ||
		estimate.CustomsPence != 30 || estimate.ProvisionsPence != 102 || estimate.HazardPence != 313 {
		t.Fatalf("better reverse direction lost: %+v %q", estimate, reason)
	}
	net := estimate.SpreadPence - estimate.SpreadPence*estimate.HaircutBPS/10_000 -
		estimate.CustomsPence - estimate.ProvisionsPence - estimate.BerthDuesPence - estimate.HazardPence
	if net != 7_805 || estimate.RatePencePerGameDay != float64(net)/estimate.RoundTripGameDays {
		t.Fatalf("reverse rate arithmetic is wrong: %+v", estimate)
	}
	// Equal rates tie lexically by commodity, then origin port, so the
	// destination-origin direction wins the symmetric fixture deterministically.
	tied := map[string]expansionPrice{
		"origin|grain":      {Ask: 100, Bid: 300},
		"destination|grain": {Ask: 100, Bid: 300},
	}
	estimate, reason = expansionGossipEstimate(in, yard, partner, hull, tied)
	if reason != "" || estimate.Origin != "destination" || estimate.Destination != "origin" {
		t.Fatalf("lexical tie broke determinism: %+v %q", estimate, reason)
	}
}

func TestExpansionGossipEstimateChargesOriginPurchaseTax(t *testing.T) {
	in, yard, partner, hull := overseasGossipFixture()
	prices := overseasGossipPrices()
	base, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
	if reason != "" || base.CustomsPence != 30 || base.ProvisionsPence != 102 {
		t.Fatalf("untaxed control is wrong: %+v %q", base, reason)
	}
	t.Run("fresh origin book rate", func(t *testing.T) {
		in.Markets["origin"] = Market{PortID: "origin", Visibility: "quay", CustomsBPS: 500}
		estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
		// 5% of the combined 3000+100 pence purchase is 155, split 150 cargo
		// plus 5 stores tax; customs adds the per-ton cargo fee and the two
		// bought store tons carry their own fee: 100+5+2 gross provisions.
		if reason != "" || estimate.CustomsPence != 180 || estimate.ProvisionsPence != 107 || estimate.HazardPence != 651 {
			t.Fatalf("book rate tax is wrong: %+v %q", estimate, reason)
		}
		net := estimate.SpreadPence - estimate.SpreadPence*estimate.HaircutBPS/10_000 -
			estimate.CustomsPence - estimate.ProvisionsPence - estimate.BerthDuesPence - estimate.HazardPence
		if net != 2_062 || estimate.RatePencePerGameDay != float64(net)/estimate.RoundTripGameDays {
			t.Fatalf("taxed rate arithmetic is wrong: %+v", estimate)
		}
	})
	t.Run("destination sale rate ignored", func(t *testing.T) {
		in.Markets["origin"] = Market{PortID: "origin", Visibility: "quay", CustomsBPS: 500}
		partner.CustomsBPS = 10_000
		estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
		if reason != "" || estimate.CustomsPence != 180 || estimate.ProvisionsPence != 107 {
			t.Fatalf("destination rate leaked into the purchase tax: %+v %q", estimate, reason)
		}
	})
	t.Run("stale book falls back to origin catalog rate", func(t *testing.T) {
		meta := in.Metadata["books/origin"]
		meta.Stale = true
		in.Metadata["books/origin"] = meta
		in.Catalog.Ports[0].CustomsBPS = 200
		yard = in.Catalog.Ports[0]
		estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
		// 2% of the combined purchase is 62, split 60 cargo plus 2 stores tax.
		if reason != "" || estimate.CustomsPence != 90 || estimate.ProvisionsPence != 104 || estimate.HazardPence != 632 {
			t.Fatalf("catalog fallback tax is wrong: %+v %q", estimate, reason)
		}
	})
	for name, rate := range map[string]int64{"above range": 20_000, "negative": -5} {
		t.Run(name, func(t *testing.T) {
			bookMeta := in.Metadata["books/origin"]
			bookMeta.Stale = false
			in.Metadata["books/origin"] = bookMeta
			in.Markets["origin"] = Market{PortID: "origin", Visibility: "quay", CustomsBPS: rate}
			if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
				estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "customs rate") {
				t.Fatalf("origin book rate %d was accepted: %+v %q", rate, estimate, reason)
			}
			bookMeta.Stale = true
			in.Metadata["books/origin"] = bookMeta
			in.Catalog.Ports[0].CustomsBPS = rate
			yard = in.Catalog.Ports[0]
			if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
				estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "customs rate") {
				t.Fatalf("origin catalog rate %d was accepted: %+v %q", rate, estimate, reason)
			}
		})
	}
}

func TestExpansionGossipEstimateChargesBerthDues(t *testing.T) {
	in, yard, partner, hull := overseasGossipFixture()
	prices := overseasGossipPrices()
	base, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
	if reason != "" || base.BerthDuesPence != 0 {
		t.Fatalf("dues-free control is wrong: %+v %q", base, reason)
	}
	// The delivery stay splits evenly (0.1875/2 days, floored to the 0.25-day
	// minimum) and the ballast turnaround bills the origin at its 0.25-day
	// minimum: 480*0.25 + 960*0.25 + 480*0.25 = 480 pence.
	in.Ports["origin"] = PortDetail{ID: "origin", EffectiveBerths: 4, BerthDuesPerDayPence: 480}
	in.Ports["destination"] = PortDetail{ID: "destination", EffectiveBerths: 4, BerthDuesPerDayPence: 960}
	estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
	if reason != "" || estimate.BerthDuesPence != 480 || estimate.RatePencePerGameDay >= base.RatePencePerGameDay {
		t.Fatalf("harbour dues did not reduce the rate: base %+v due %+v %q", base, estimate, reason)
	}
	in.Ports["destination"] = PortDetail{ID: "destination", EffectiveBerths: 4, BerthDuesPerDayPence: -1}
	if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
		estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "dues") {
		t.Fatalf("negative berth dues were accepted: %+v %q", estimate, reason)
	}
}

func TestExpansionGossipEstimateRequiresCalibratedHazard(t *testing.T) {
	prices := overseasGossipPrices()
	{
		in, yard, partner, hull := overseasGossipFixture()
		in.hazardEstimate = nil
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "calibration is missing") {
			t.Fatalf("missing calibration fell back to zero hazard: %+v %q", estimate, reason)
		}
	}
	{
		in, yard, partner, hull := overseasGossipFixture()
		in.hazardEstimate = &hazardCalibration{k: 0.1, n: 12, longestLeg: 30, highestRisk: 5_099}
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "1.5x") {
			t.Fatalf("excess arrival risk was priced anyway: %+v %q", estimate, reason)
		}
	}
	{
		in, yard, partner, hull := overseasGossipFixture()
		in.hazardEstimate = &hazardCalibration{k: 0.1, n: 12, longestLeg: 7, highestRisk: 10_199}
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "calibrated zone hazard estimate is unavailable") {
			t.Fatalf("over-long calibration legs were priced anyway: %+v %q", estimate, reason)
		}
	}
	in, yard, partner, hull := overseasGossipFixture()
	meta := in.Metadata["zones"]
	meta.Stale = true
	in.Metadata["zones"] = meta
	if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
		estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "zone freshness") {
		t.Fatalf("stale piracy zones were priced anyway: %+v %q", estimate, reason)
	}
}

func TestExpansionGossipEstimateRejectsExpenseOverflow(t *testing.T) {
	t.Run("saturating combined purchase", func(t *testing.T) {
		in, yard, partner, hull := overseasGossipFixture()
		in.Markets["origin"] = Market{PortID: "origin", Visibility: "quay", CustomsBPS: 10_000}
		// The ask one pence under the largest legal buyer bid keeps the lot
		// tradable (spread 1) while the 30-ton purchase at 100% customs
		// saturates the combined purchase.
		prices := map[string]expansionPrice{
			"origin|grain":      {Ask: math.MaxInt64/30 - 1},
			"destination|grain": {Bid: math.MaxInt64 / 30},
		}
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "overflow") {
			t.Fatalf("saturating purchase escaped the expense bound: %+v %q", estimate, reason)
		}
	})
	t.Run("saturating hazard risk base", func(t *testing.T) {
		in, yard, partner, hull := overseasGossipFixture()
		in.Markets["origin"] = Market{PortID: "origin", Visibility: "quay", CustomsBPS: 10_000}
		// One notch smaller the purchase and its full tax fit, but purchase
		// plus customs saturates the hazard risk base.
		prices := map[string]expansionPrice{
			"origin|grain":      {Ask: math.MaxInt64/60 - 1},
			"destination|grain": {Bid: math.MaxInt64 / 60},
		}
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "overflow") {
			t.Fatalf("saturating risk base escaped the expense bound: %+v %q", estimate, reason)
		}
	})
	t.Run("saturating provisions purchase", func(t *testing.T) {
		in, yard, partner, hull := overseasGossipFixture()
		prices := map[string]expansionPrice{
			"origin|grain":      {Ask: 100},
			"destination|grain": {Bid: 300},
			"origin|provisions": {Ask: 9_000_000_000_000_000_000},
		}
		// Two whole bought tons at 9e18 pence exceed int64, refused by the
		// checked purchase multiplication.
		if estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices); reason == "" ||
			estimate != (expansionRateEstimate{}) || !strings.Contains(reason, "overflow") {
			t.Fatalf("int64 boundary provisions pricing was accepted: %+v %q", estimate, reason)
		}
	})
	in, yard, partner, hull := overseasGossipFixture()
	prices := map[string]expansionPrice{
		"origin|grain":      {Ask: 100},
		"destination|grain": {Bid: 300},
		"origin|provisions": {Ask: 4_000_000_000_000_000_000},
	}
	estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
	// Below the boundary two bought tons at 4e18 pence price exactly
	// 8000000000000000002 with the per-ton store fee and no tax.
	if reason != "" || estimate == (expansionRateEstimate{}) || estimate.ProvisionsPence != 8_000_000_000_000_000_002 {
		t.Fatalf("below-boundary provisions pricing was refused: %+v %q", estimate, reason)
	}
}

func TestExpansionGossipEstimateFailsClosedOnMissingEvidence(t *testing.T) {
	prices := overseasGossipPrices()
	for name, corrupt := range map[string]func(in *PlanningInput, yard, partner Port, hull Hull){
		"missing return route": func(in *PlanningInput, _ Port, _ Port, hull Hull) {
			delete(in.Routes, routeKey("destination", "origin", hull.ID))
		},
		"hull below route edge": func(in *PlanningInput, _ Port, _ Port, hull Hull) {
			route := in.Routes[routeKey("origin", "destination", hull.ID)]
			route.Legs[0].EdgeClass = "oceanic"
			in.Routes[routeKey("origin", "destination", hull.ID)] = route
		},
		"stale harbour metadata": func(in *PlanningInput, _, partner Port, _ Hull) {
			meta := in.Metadata["ports/"+partner.ID]
			meta.Stale = true
			in.Metadata["ports/"+partner.ID] = meta
		},
		"destination berths unavailable": func(in *PlanningInput, _, partner Port, _ Hull) {
			in.Ports[partner.ID] = PortDetail{ID: partner.ID, EffectiveBerths: 0}
		},
		"origin queue unavailable": func(in *PlanningInput, yard Port, _ Port, _ Hull) {
			in.Ports[yard.ID] = PortDetail{ID: yard.ID, EffectiveBerths: 4, QueuedShips: -1}
		},
		"provisions pricing missing": func(in *PlanningInput, _, _ Port, _ Hull) {
			in.Catalog.Commodities = []Commodity{{ID: "grain", BasePricePence: 100}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in, yard, partner, hull := overseasGossipFixture()
			corrupt(&in, yard, partner, hull)
			estimate, reason := expansionGossipEstimate(in, yard, partner, hull, prices)
			if reason == "" || estimate != (expansionRateEstimate{}) {
				t.Fatalf("corrupt evidence produced %+v (reason %q); want a refusal", estimate, reason)
			}
		})
	}
	if _, reason := expansionGossipEstimate(PlanningInput{}, Port{ID: "same"}, Port{ID: "same"}, Hull{}, nil); !strings.Contains(reason, "two different pair ports") {
		t.Fatalf("identical yard and partner were not refused: %q", reason)
	}
}
