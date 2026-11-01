package engine

import (
	"math"
	"strings"
)

// Gossip is an estimate, not executable depth, so a prospective overseas build
// is admitted on only half of the projected spread proceeds.
const expansionGossipHaircutBPS int64 = 5_000

// expansionRateEstimate reports the best conservative round trip between an
// expansion yard and its partner port using fresh coffee-house gossip alone.
// AskPence is the per-ton source ask the winning estimate assumes, so callers
// can fund the source purchase without re-identifying the quote; SpreadPence
// is the whole-lot bid-minus-ask proceeds. CustomsPence is the cargo purchase
// tax at the origin, ProvisionsPence prices the whole bought store tons' gross
// purchase including their tax and per-ton fee, BerthDuesPence prices both
// harbours' stays, and the hazard
// fields carry the calibrated zone estimate charged on the purchase. The
// parent admission still applies its valuation gate, then hull and pair
// upkeep, after this rate.
type expansionRateEstimate struct {
	Basis               string  `json:"basis"`
	Commodity           string  `json:"commodity"`
	Origin              string  `json:"origin"`
	Destination         string  `json:"destination"`
	CargoTons           int64   `json:"cargo_tons"`
	AskPence            int64   `json:"ask_pence"`
	SpreadPence         int64   `json:"spread_pence"`
	HaircutBPS          int64   `json:"haircut_bps"`
	RoundTripGameDays   float64 `json:"round_trip_game_days"`
	CustomsPence        int64   `json:"customs_pence"`
	ProvisionsPence     int64   `json:"provisions_pence"`
	BerthDuesPence      int64   `json:"berth_dues_pence"`
	HazardBPS           int64   `json:"hazard_bps"`
	HazardPence         int64   `json:"hazard_pence"`
	HazardSource        string  `json:"hazard_source"`
	RatePencePerGameDay float64 `json:"rate_pence_per_game_day"`
}

// expansionGossipEstimate ranks both pair directions on their resulting net
// rate and returns the best gossip round trip, or a nonempty reason when
// neither direction yields a usable estimate. It is pure: no I/O, and prices
// must already carry the 30-game-day freshness filter of expansionPrices.
func expansionGossipEstimate(input PlanningInput, yard, partner Port, hull Hull, prices map[string]expansionPrice) (expansionRateEstimate, string) {
	if yard.ID == partner.ID {
		return expansionRateEstimate{}, "gossip estimate needs two different pair ports"
	}
	provisionsPrice, reason := expansionGossipProvisionsPrice(yard, partner, input.Catalog, prices)
	if reason != "" {
		return expansionRateEstimate{}, reason
	}
	var failures []string
	var best expansionRateEstimate
	for _, direction := range [][2]Port{{yard, partner}, {partner, yard}} {
		estimate, failure := expansionGossipDirection(input, direction[0], direction[1], hull, prices, provisionsPrice)
		if failure != "" {
			failures = append(failures, failure)
			continue
		}
		if best.Basis == "" || expansionGossipRateBetter(estimate, best) {
			best = estimate
		}
	}
	if best.Basis == "" {
		return expansionRateEstimate{}, "no usable gossip pair estimate: " + strings.Join(failures, "; ")
	}
	return best, ""
}

// expansionGossipProvisionsPrice resolves the conservative per-ton provisions
// price for the pair: the catalog base price or the fresher gossip ask at
// either harbour, whichever is higher. Provisions are mandatory stores, so a
// missing price fails the estimate instead of assuming free stores.
func expansionGossipProvisionsPrice(yard, partner Port, catalog Catalog, prices map[string]expansionPrice) (int64, string) {
	price := int64(0)
	for _, commodity := range catalog.Commodities {
		if commodity.ID == "provisions" && commodity.BasePricePence > price {
			price = commodity.BasePricePence
		}
	}
	for _, port := range []Port{yard, partner} {
		if ask := prices[port.ID+"|provisions"].Ask; ask > price {
			price = ask
		}
	}
	if price <= 0 {
		return 0, "gossip estimate requires provisions pricing, but neither the catalog base price nor a fresh ask at either harbour exists"
	}
	return price, ""
}

// expansionGossipDirection estimates one direction: cargo bought at the source
// gossip ask, sold at the buyer gossip bid, then a ballast leg home. It reuses
// the planner's observed-route, delivery, berth, purchase-tax and hazard
// formulas; the routes, harbours and zones must already be fresh observations.
func expansionGossipDirection(input PlanningInput, origin, buyer Port, hull Hull, prices map[string]expansionPrice, provisionsPrice int64) (expansionRateEstimate, string) {
	prefix := origin.ID + " -> " + buyer.ID + ": "
	outward, err := observedPlanningRoute(input, origin.ID, buyer.ID, hull)
	if err != nil {
		return expansionRateEstimate{}, prefix + err.Error()
	}
	back, err := observedPlanningRoute(input, buyer.ID, origin.ID, hull)
	if err != nil {
		return expansionRateEstimate{}, prefix + err.Error()
	}
	// Fresh successful metadata at both harbours, exactly like
	// ballastRepeatStoreTarget: a retained port body from a failed refresh
	// must not pass. A route carries no toll amount and ValidateRoute already
	// refuses Sound Toll edges, so the estimate stays fail-closed on tolls.
	if !input.fresh("ports/"+origin.ID) || !input.fresh("ports/"+buyer.ID) {
		return expansionRateEstimate{}, prefix + "harbour metadata is stale or missing"
	}
	originDetail, buyerDetail := input.Ports[origin.ID], input.Ports[buyer.ID]
	if originDetail.EffectiveBerths <= 0 || buyerDetail.EffectiveBerths <= 0 ||
		originDetail.QueuedShips < 0 || buyerDetail.QueuedShips < 0 ||
		originDetail.BerthDuesPerDayPence < 0 || buyerDetail.BerthDuesPerDayPence < 0 {
		return expansionRateEstimate{}, prefix + "observed berth capacity, queue or dues are unavailable"
	}
	// The buyer tax is charged on the purchase at the origin as in pricePlan:
	// the fresh book's rate wins, otherwise the origin catalog rate. It is
	// never the destination's sale-side rate.
	rate := origin.CustomsBPS
	if input.fresh("books/" + origin.ID) {
		rate = input.Markets[origin.ID].CustomsBPS
	}
	if rate < 0 || rate > 10_000 {
		return expansionRateEstimate{}, prefix + "purchase customs rate is outside 0..10000 basis points"
	}
	// The hazard charge needs fresh piracy observations and the calibrated zone
	// estimate the parent derives for the candidate; there is no zero fallback.
	if !input.fresh("zones") {
		return expansionRateEstimate{}, prefix + "piracy zone freshness is unavailable"
	}
	hazardBPS, hazardSource, hazardOK := estimatedZoneHazard(input, outward)
	if !hazardOK {
		if input.hazardEstimate == nil {
			return expansionRateEstimate{}, prefix + "zone hazard calibration is missing"
		}
		if _, _, highest, valid := hazardZoneRisk(input, outward, nil); valid &&
			float64(highest) > 1.5*float64(input.hazardEstimate.highestRisk) {
			return expansionRateEstimate{}, prefix + "outward arrival risk exceeds the calibrated 1.5x hazard cap"
		}
		return expansionRateEstimate{}, prefix + "calibrated zone hazard estimate is unavailable"
	}
	guns := min(int64(4), hull.MaxCannons)
	usableTons := hull.HoldCapacityTons - 2*guns
	if guns < 1 || usableTons <= 0 {
		return expansionRateEstimate{}, prefix + "hull leaves no cargo hold after its planned cannons"
	}
	// Safe round-trip stores follow ballastRepeatStoreTarget: both legs' route
	// safety stock, crew port consumption and one spare ton. Handling adds four
	// minutes per cargo ton to the outbound port stay, so solve the reservation
	// for the largest whole cargo that still fits the hold.
	handlingPerTon := float64(hull.CrewRequired) / 200 * 4.0 / 1_440
	outQueue := float64(buyerDetail.QueuedShips+1) / float64(buyerDetail.EffectiveBerths) * 0.25
	backQueue := float64(originDetail.QueuedShips+1) / float64(originDetail.EffectiveBerths) * 0.25
	storeBase := outward.SafeProvisionsTons + back.SafeProvisionsTons +
		float64(hull.CrewRequired)/200*(1.0/24+outQueue+30.0/1_440+backQueue) + 1
	if storeBase < 0 || math.IsNaN(storeBase) || math.IsInf(storeBase, 0) {
		return expansionRateEstimate{}, prefix + "safe round-trip stores are unavailable"
	}
	cargoTons := int64(math.Floor((float64(usableTons) - storeBase) / (1 + handlingPerTon)))
	if cargoTons <= 0 {
		return expansionRateEstimate{}, prefix + "safe round-trip stores consume the whole cargo hold"
	}
	reservationTons := storeBase + handlingPerTon*float64(cargoTons)
	sailingOut, portOut := deliveryGameDays(outward, hull, buyerDetail, cargoTons)
	// The ballast return keeps ballastLegEconomics' duration form: the same
	// laden speed bound for sailing and the short 30-minute turnaround.
	sailingBack := float64(back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	portBack := 30.0/1_440 + backQueue
	roundTrip := sailingOut + portOut + sailingBack + portBack
	if roundTrip <= 0 || math.IsNaN(roundTrip) || math.IsInf(roundTrip, 0) {
		return expansionRateEstimate{}, prefix + "round-trip duration is not finite positive game days"
	}
	// Berth dues follow pricePlan and ballastLegEconomics: per-port ceilings on
	// the split delivery stay with a quarter-day minimum, plus the ballast
	// turnaround at the origin.
	berthFloats := [...]float64{
		float64(originDetail.BerthDuesPerDayPence) * max(0.25, portOut/2),
		float64(buyerDetail.BerthDuesPerDayPence) * max(0.25, portOut/2),
		float64(originDetail.BerthDuesPerDayPence) * max(0.25, portBack),
	}
	var berth int64
	for _, berthFloat := range berthFloats {
		// float64(math.MaxInt64) rounds up to 2^63 and no longer fits int64,
		// so the boundary itself is rejected like every float conversion.
		if math.IsNaN(berthFloat) || math.IsInf(berthFloat, 0) || berthFloat >= float64(math.MaxInt64) {
			return expansionRateEstimate{}, prefix + "berth dues overflow pence"
		}
		berth = growthCashTotal(berth, int64(math.Ceil(berthFloat)))
	}
	if berth == math.MaxInt64 {
		return expansionRateEstimate{}, prefix + "berth dues overflow pence"
	}
	// The planner funds integer store tons: StoresToBuy = ceil(target-current)
	// against a new hull's empty stores, so the reservation rounds up to whole
	// bought tons and prices their full purchase.
	boughtTons := int64(math.Ceil(reservationTons))
	if boughtTons <= 0 || provisionsPrice > math.MaxInt64/boughtTons {
		return expansionRateEstimate{}, prefix + "round-trip provisions cost overflows pence"
	}
	provisionsCost := boughtTons * provisionsPrice
	var best expansionRateEstimate
	var overflow string
	for _, commodity := range input.Catalog.Commodities {
		ask := prices[origin.ID+"|"+commodity.ID].Ask
		bid := prices[buyer.ID+"|"+commodity.ID].Bid
		if ask <= 0 || bid <= ask {
			continue
		}
		spread := bid - ask
		if cargoTons > math.MaxInt64/spread || bid > math.MaxInt64/cargoTons {
			overflow = prefix + "gossip expenses overflow pence"
			continue
		}
		proceeds := spread * cargoTons
		purchase := ask * cargoTons
		// Tax follows pricePlan: one origin rate on the combined cargo and
		// stores purchase plus one pence per cargo ton. CustomsPence keeps the
		// cargo share; gross ProvisionsPence carries the stores' tax share.
		combinedGross := growthCashTotal(purchase, provisionsCost)
		totalTax := ceilBPS(combinedGross, rate)
		cargoTax := ceilBPS(purchase, rate)
		customs := growthCashTotal(cargoTax, cargoTons)
		storesTax := totalTax - cargoTax
		provisionsPence := growthCashTotal(provisionsCost, storesTax, boughtTons)
		if combinedGross == math.MaxInt64 || customs == math.MaxInt64 || provisionsPence == math.MaxInt64 {
			overflow = prefix + "gossip expenses overflow pence"
			continue
		}
		// The hazard base matches pricePlan's risk: the purchase plus the whole
		// charged customs, stores tax and per-ton fees included.
		risk := growthCashTotal(purchase, customs, storesTax, boughtTons)
		if risk == math.MaxInt64 {
			overflow = prefix + "gossip expenses overflow pence"
			continue
		}
		hazardPence, hazardValid := hazardCharge(risk, hazardBPS)
		if !hazardValid {
			overflow = prefix + "gossip hazard charge overflows pence"
			continue
		}
		expenses := growthCashTotal(ceilBPS(proceeds, expansionGossipHaircutBPS), customs, provisionsPence, berth, hazardPence)
		if expenses == math.MaxInt64 {
			overflow = prefix + "gossip expenses overflow pence"
			continue
		}
		net := proceeds - expenses
		candidate := expansionRateEstimate{
			Basis:               "gossip estimate",
			Commodity:           commodity.ID,
			Origin:              origin.ID,
			Destination:         buyer.ID,
			CargoTons:           cargoTons,
			AskPence:            ask,
			SpreadPence:         proceeds,
			HaircutBPS:          expansionGossipHaircutBPS,
			RoundTripGameDays:   roundTrip,
			CustomsPence:        customs,
			ProvisionsPence:     provisionsPence,
			BerthDuesPence:      berth,
			HazardBPS:           hazardBPS,
			HazardPence:         hazardPence,
			HazardSource:        hazardSource,
			RatePencePerGameDay: float64(net) / roundTrip,
		}
		if best.Basis == "" || expansionGossipRateBetter(candidate, best) {
			best = candidate
		}
	}
	if best.Basis == "" {
		if overflow != "" {
			return expansionRateEstimate{}, overflow
		}
		return expansionRateEstimate{}, prefix + "no commodity has both a fresh source ask and a higher fresh buyer bid"
	}
	return best, ""
}

// expansionGossipRateBetter ranks by net rate, then lexically by commodity and
// origin port so equal-rate candidates stay deterministic.
func expansionGossipRateBetter(candidate, incumbent expansionRateEstimate) bool {
	if candidate.RatePencePerGameDay != incumbent.RatePencePerGameDay {
		return candidate.RatePencePerGameDay > incumbent.RatePencePerGameDay
	}
	if candidate.Commodity != incumbent.Commodity {
		return candidate.Commodity < incumbent.Commodity
	}
	return candidate.Origin < incumbent.Origin
}
