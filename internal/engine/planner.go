package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"
)

const maximumPence int64 = 1_000_000_000_000

// DepthFill is executable at LimitPence or better, before buyer-only customs.
type DepthFill struct {
	Quantity   int64 `json:"quantity"`
	GrossPence int64 `json:"gross_pence"`
	LimitPence int64 `json:"limit_pence"`
}

// WalkDepth never substitutes a top-of-book price for missing quantity.
func WalkDepth(levels []Level, quantity int64, buy bool) (DepthFill, error) {
	if quantity <= 0 {
		return DepthFill{}, errors.New("quantity must be positive")
	}
	compare := func(left, right Level) int {
		order := cmp.Compare(left.PricePence, right.PricePence)
		if !buy {
			return -order
		}
		return order
	}
	ordered := levels
	if !slices.IsSortedFunc(levels, compare) {
		ordered = append([]Level(nil), levels...)
		slices.SortStableFunc(ordered, compare)
	}
	var result DepthFill
	for _, level := range ordered {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		take := min(quantity-result.Quantity, level.RemainingQuantity)
		if take > (math.MaxInt64-result.GrossPence)/level.PricePence {
			return DepthFill{}, errors.New("depth value overflows pence")
		}
		result.GrossPence += take * level.PricePence
		result.Quantity += take
		result.LimitPence = level.PricePence
		if result.Quantity == quantity {
			return result, nil
		}
	}
	return result, errors.New("insufficient observed depth")
}

func ceilBPS(pence, bps int64) int64 {
	if pence <= 0 || bps <= 0 {
		return 0
	}
	return (pence/10_000)*bps + ((pence%10_000)*bps+9_999)/10_000
}

func classRank(class string) int {
	switch class {
	case "coastal":
		return 1
	case "regional":
		return 2
	case "oceanic":
		return 3
	default:
		return 0
	}
}

// ValidateRoute rejects every under-rated edge and zone, not an average rating.
// Sound routes remain unavailable until an authoritative toll-cost source exists.
func ValidateRoute(route Route, hull Hull, zones []Zone) error {
	if classRank(hull.HullCategory) == 0 || len(route.Legs) == 0 || route.TotalDistanceNM <= 0 {
		return errors.New("route or hull classification is incomplete")
	}
	zoneByID := make(map[string]Zone, len(zones))
	for _, zone := range zones {
		zoneByID[zone.ID] = zone
	}
	for i, leg := range route.Legs {
		if classRank(leg.EdgeClass) == 0 || classRank(hull.HullCategory) < classRank(leg.EdgeClass) {
			return fmt.Errorf("hull %s is below route edge %s -> %s (%s)", hull.HullCategory, leg.FromNode, leg.ToNode, leg.EdgeClass)
		}
		for _, node := range []string{leg.FromNode, leg.ToNode} {
			if zone, exists := zoneByID[node]; exists {
				if classRank(zone.ZoneClass) == 0 || classRank(hull.HullCategory) < classRank(zone.ZoneClass) {
					return fmt.Errorf("hull %s is below zone %s (%s)", hull.HullCategory, node, zone.ZoneClass)
				}
			}
		}
		if i < len(route.Legs)-1 {
			if _, exists := zoneByID[leg.ToNode]; !exists {
				return fmt.Errorf("route zone %s is absent from catalog", leg.ToNode)
			}
		}
		if leg.IsSoundToll {
			return errors.New("Sound toll has no observed route cost")
		}
	}
	return nil
}

// ExpectedNetPence includes observable cash and replacement costs. Stress fields
// are separate diagnostics, never expected losses or profitability gates.
// ProbabilityEstimate remains nil because no loss probability formula is public.
type Economics struct {
	RevenuePence                int64    `json:"revenue_pence"`
	PurchasePence               int64    `json:"purchase_pence"`
	CustomsPence                int64    `json:"customs_pence"`
	ProvisionsPence             int64    `json:"provisions_pence"`
	BerthDuesPence              int64    `json:"berth_dues_pence"`
	UpkeepPence                 int64    `json:"upkeep_pence"`
	TollPence                   int64    `json:"toll_pence"`
	CollateralPence             int64    `json:"collateral_pence"`
	ExpectedNetPence            int64    `json:"expected_net_pence"`
	StressHaircutBPS            int64    `json:"stress_haircut_bps"`
	StressHaircutPence          int64    `json:"stress_haircut_pence"`
	PriceUncertaintyStressPence int64    `json:"price_uncertainty_stress_pence"`
	StressNetPence              int64    `json:"stress_net_pence"`
	GameDays                    float64  `json:"game_days"`
	NetPencePerGameDay          float64  `json:"net_pence_per_game_day"`
	ProbabilityEstimate         *float64 `json:"probability_estimate"`
}

type Plan struct {
	Kind             string     `json:"kind"`
	ShipID           string     `json:"ship_id"`
	Origin           string     `json:"origin"`
	Destination      string     `json:"destination"`
	Commodity        string     `json:"commodity"`
	ContractID       string     `json:"contract_id,omitempty"`
	FreightIDs       []string   `json:"freight_ids"`
	Quantity         int64      `json:"quantity"`
	BuyLimitPence    int64      `json:"buy_limit_pence"`
	SellLimitPence   int64      `json:"sell_limit_pence"`
	StoresToBuy      int64      `json:"stores_to_buy"`
	StoresLimitPence int64      `json:"stores_limit_pence"`
	TargetStoresTons float64    `json:"target_stores_tons"`
	Deadline         *time.Time `json:"deadline,omitempty"`
	Economics        Economics  `json:"economics"`
	Accepted         bool       `json:"accepted"`
	Reason           string     `json:"reason"`
	PriceSource      string     `json:"price_source"`
	ExpectedArrival  time.Time  `json:"expected_arrival"`
}

type PlanningInput struct {
	Now                   time.Time
	CompanyID             string
	Ship                  ShipState
	Catalog               Catalog
	Clock                 Clock
	Zones                 []ZoneObservation
	Markets               map[string]Market
	Ports                 map[string]PortDetail
	Routes                map[string]Route
	Contracts             []Contract
	Balances              map[string]int64
	ReserveByRegion       map[string]int64
	ReservePence          int64
	PaidProvisionsPence   int64
	Metadata              map[string]DatasetMeta
	Generation            string
	OwnedOrderIDs         map[string]bool
	FleetDailyUpkeepPence int64
	Rates                 []Rate
}

func (in PlanningInput) fresh(dataset string) bool {
	meta, exists := in.Metadata[dataset]
	if !exists || !meta.Complete || meta.Stale || meta.Error != "" || meta.UpdatedAt.IsZero() ||
		(in.Generation != "" && meta.Generation != in.Generation) ||
		meta.UpdatedAt.After(in.Now.Add(time.Second)) || in.Now.Sub(meta.UpdatedAt) > 2*time.Minute {
		return false
	}
	if dataset == "clock" {
		_, _, ok := in.serverBounds()
		return ok
	}
	if meta.UpstreamAsOf != nil {
		_, upper, ok := in.serverBounds()
		return ok && !meta.UpstreamAsOf.After(upper.Add(time.Second)) && upper.Sub(*meta.UpstreamAsOf) <= 2*time.Minute
	}
	return true
}

func reject(plan Plan, reason string) Plan {
	plan.Accepted = false
	plan.Reason = reason
	return plan
}

func betterPlan(candidate, other Plan) bool {
	if candidate.Accepted != other.Accepted {
		return candidate.Accepted
	}
	candidateCosted := candidate.Economics.GameDays > 0
	otherCosted := other.Economics.GameDays > 0
	if candidateCosted != otherCosted {
		return candidateCosted
	}
	return candidate.Economics.NetPencePerGameDay > other.Economics.NetPencePerGameDay
}

// PlanVoyages returns accepted and rejected alternatives, accepted first and
// ranked by expected net pence per round-trip game day. No side effects.
func PlanVoyages(in PlanningInput) []Plan {
	in.Markets = withoutOwnOrders(in.Markets, in.OwnedOrderIDs)
	ship := in.Ship
	if (ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead") || ship.Ship.ConvoyID != "" || ship.Voyage != nil {
		return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "ship must be in port and independently navigable")}
	}
	for _, dataset := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships"} {
		if !in.fresh(dataset) {
			return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "missing, stale or failed dataset: "+dataset)}
		}
	}
	if in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 {
		return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "world is paused or its time scale is unavailable")}
	}
	if in.Clock.Calendar.Month < 1 || in.Clock.Calendar.Month > 12 || in.Clock.Calendar.Day < 1 || in.Clock.Calendar.Day > 30 {
		return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "authoritative game calendar is invalid")}
	}
	hull, exists := findHull(in.Catalog, ship.Ship.ShipClass)
	if !exists || hull.BaseSpeedCentiknots <= 0 {
		return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "ship class or speed is missing from catalog")}
	}
	origin, exists := findPort(in.Catalog, ship.Ship.CurrentPortID)
	if !exists {
		return []Plan{reject(Plan{ShipID: ship.Ship.ID, Kind: "hold"}, "current port is absent from catalog")}
	}
	var plans []Plan
	for _, contract := range in.Contracts {
		if contract.ContractType == "charter" && contract.OriginPortID != origin.ID {
			plan := Plan{Kind: "charter", ShipID: ship.Ship.ID, Origin: origin.ID, Destination: contract.OriginPortID, Commodity: contract.CommodityID, ContractID: contract.ID, Deadline: contract.ExpiresAt}
			if contract.Status != "open" || contract.Stale || contract.RewardPerTonPence == nil || contract.RemainingQuantity <= 0 {
				plans = append(plans, reject(plan, "charter is closed, gossiped, lacks typed reward or is already filled"))
				continue
			}
			plans = append(plans, sizePlan(in, hull, origin, plan, contract.RemainingQuantity, *contract.RewardPerTonPence, 0))
		}
		if contract.ContractType == "freight" && contract.OriginPortID == origin.ID {
			plan := Plan{Kind: "freight", ShipID: ship.Ship.ID, Origin: origin.ID, Destination: contract.DestPortID, Commodity: contract.CommodityID, ContractID: contract.ID, FreightIDs: []string{contract.ID}}
			if contract.Status != "open" || contract.Stale || contract.IssuerCompanyID == in.CompanyID || contract.QuantityRequired <= 0 {
				plans = append(plans, reject(plan, "freight is unavailable, gossiped or self-issued"))
				continue
			}
			plan.Quantity = contract.QuantityRequired
			plan.Economics.CollateralPence = contract.CollateralPence
			plans = append(plans, evaluatePlan(in, hull, origin, plan, contract.RewardPence, 0))
		}
	}
	book := in.Markets[origin.ID]
	for _, destination := range in.Catalog.Ports {
		if destination.ID == origin.ID || destination.Phase > in.Clock.UnlockedPhase {
			continue
		}
		for _, commodity := range book.Books {
			if commodity.Depth == nil || len(commodity.Depth.Asks) == 0 || !portTrades(destination, commodity.CommodityID) {
				continue
			}
			plan := Plan{Kind: "spot", ShipID: ship.Ship.ID, Origin: origin.ID, Destination: destination.ID, Commodity: commodity.CommodityID}
			plans = append(plans, sizePlan(in, hull, origin, plan, ship.EffectiveHoldCapacityTons-ship.CargoUsedTons, 0, 0))
		}
	}
	// Compatible sealed freight competes as a whole voyage and as incremental
	// income in spare hold; never combine destinations or reduce a fixed lot.
	basePlans := append([]Plan(nil), plans...)
	for _, plan := range basePlans {
		if !plan.Accepted || plan.Kind == "freight" {
			continue
		}
		for _, freight := range in.Contracts {
			if freight.ContractType != "freight" ||
				freight.Status != "open" ||
				freight.Stale ||
				freight.OriginPortID != origin.ID ||
				freight.DestPortID != plan.Destination ||
				freight.IssuerCompanyID == in.CompanyID ||
				freight.QuantityRequired <= 0 {
				continue
			}
			bestMixed := reject(plan, "compatible freight has no profitable mixed-hold size")
			for quantity := int64(1); quantity <= plan.Quantity; quantity++ {
				mixed := plan
				mixed.Kind = plan.Kind + "+freight"
				mixed.Quantity = quantity
				mixed.FreightIDs = []string{freight.ID}
				mixed.Economics.CollateralPence = freight.CollateralPence
				var cargoRevenue int64
				if plan.Kind == "charter" {
					contractReward := int64(0)
					for _, charter := range in.Contracts {
						if charter.ID == plan.ContractID && charter.RewardPerTonPence != nil {
							contractReward = *charter.RewardPerTonPence
						}
					}
					cargoRevenue = quantity * contractReward
				} else {
					var reason string
					cargoRevenue, mixed.SellLimitPence, mixed.Economics.PriceUncertaintyStressPence, mixed.PriceSource, reason = destinationRevenue(in, mixed.Destination, mixed.Commodity, quantity)
					if reason != "" {
						continue
					}
				}
				mixed = evaluatePlan(in, hull, origin, mixed, cargoRevenue+freight.RewardPence, freight.QuantityRequired)
				if quantity == 1 || betterPlan(mixed, bestMixed) {
					bestMixed = mixed
				}
			}
			plans = append(plans, bestMixed)
		}
	}
	if len(plans) == 0 {
		plans = append(plans, reject(Plan{Kind: "hold", ShipID: ship.Ship.ID}, "no observed profitable charter, executable destination book, recent gossip or local freight"))
	}
	sort.SliceStable(plans, func(i, j int) bool {
		return betterPlan(plans[i], plans[j])
	})
	return plans
}

func portTrades(port Port, commodity string) bool {
	if commodity == "provisions" {
		return true
	}
	for _, list := range [][]string{port.Produces, port.Manufactures, port.Imports} {
		for _, id := range list {
			if id == commodity {
				return true
			}
		}
	}
	return false
}

func sizePlan(in PlanningInput, hull Hull, origin Port, plan Plan, maximum, rewardPerTon, extraTons int64) Plan {
	best := reject(plan, "no whole-ton size has positive expected cash net and adequate reserves")
	maximum = min(maximum, in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons)
	for quantity := int64(1); quantity <= maximum; quantity++ {
		candidate := plan
		candidate.Quantity = quantity
		candidate = evaluatePlan(in, hull, origin, candidate, quantity*rewardPerTon, extraTons)
		if quantity == 1 || betterPlan(candidate, best) {
			best = candidate
		}
	}
	return best
}

func evaluatePlan(in PlanningInput, hull Hull, origin Port, plan Plan, revenue, extraTons int64) Plan {
	outwardKey := routeKey(origin.ID, plan.Destination, hull.ID)
	returnKey := routeKey(plan.Destination, origin.ID, hull.ID)
	outward, exists := in.Routes[outwardKey]
	if !exists || !in.fresh("routes/"+outwardKey) || !in.fresh("routes/"+returnKey) {
		return reject(plan, "round-trip route is missing, failed or stale")
	}
	back := in.Routes[returnKey]
	for _, route := range []Route{outward, back} {
		if err := ValidateRoute(route, hull, in.Catalog.SeaZones); err != nil {
			return reject(plan, err.Error())
		}
		for _, leg := range route.Legs {
			for _, zone := range in.Catalog.SeaZones {
				if zone.ID != leg.FromNode && zone.ID != leg.ToNode {
					continue
				}
				observed := false
				for _, live := range in.Zones {
					if live.SeaZoneID == zone.ID && live.NetPiracyBPS >= 0 {
						observed = true
					}
				}
				if !observed {
					return reject(plan, "live piracy observation is missing for route zone "+zone.ID)
				}
			}
		}
	}
	if !in.fresh("books/"+origin.ID) || !in.fresh("ports/"+origin.ID) || !in.fresh("ports/"+plan.Destination) {
		return reject(plan, "local depth or round-trip harbour dues are not fresh")
	}
	originBook := in.Markets[origin.ID]
	if originBook.Visibility != "quay" {
		return reject(plan, "local full depth requires quay visibility")
	}
	// Fully laden speed is a conservative upper bound on voyage duration. Both
	// legs and an explicit queue/turnaround allowance are charged to this trip.
	sailingDays := float64(outward.TotalDistanceNM+back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	// Two arrivals each lock the berth for 30 game minutes; handling is
	// conservatively charged at two minutes per ton for both operations.
	portDays := 1.0/24 + float64((plan.Quantity+extraTons)*4)/1_440
	destinationDetail := in.Ports[plan.Destination]
	if destinationDetail.EffectiveBerths <= 0 {
		return reject(plan, "destination berth capacity is unavailable")
	}
	portDays += float64(destinationDetail.QueuedShips+1) / float64(destinationDetail.EffectiveBerths) * 0.25
	gameDays := sailingDays + portDays
	outwardDays := float64(outward.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	_, upper, ok := in.serverBounds()
	if !ok {
		return reject(plan, "current server clock authority is unavailable")
	}
	arrival := upper.Add(gameDuration(outwardDays+portDays, in.Clock.Calendar.TimeScale))
	plan.ExpectedArrival = arrival
	if plan.Deadline != nil && !arrival.Add(gameDuration(1, in.Clock.Calendar.TimeScale)).Before(*plan.Deadline) {
		return reject(plan, "charter deadline lacks one game day of delivery slack")
	}
	voyageStores := outward.SafeProvisionsTons + back.SafeProvisionsTons + float64(hull.CrewRequired)/200*portDays + 1
	storesNeeded := math.Max(voyageStores, 1+float64(hull.CrewRequired)/200*2)
	plan.TargetStoresTons = storesNeeded
	plan.StoresToBuy = max(0, int64(math.Ceil(storesNeeded-in.Ship.Stores.ProvisionsTons)))
	storeIncrease := int64(math.Ceil(in.Ship.Stores.ProvisionsTons+float64(plan.StoresToBuy))) - int64(math.Ceil(in.Ship.Stores.ProvisionsTons))
	if plan.Quantity+extraTons+storeIncrease > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons {
		return reject(plan, "cargo, sealed freight and safe stores exceed effective hold")
	}
	var storeCost int64
	if plan.StoresToBuy > 0 {
		provisions := bookCommodity(originBook, "provisions")
		if provisions == nil || provisions.Depth == nil {
			return reject(plan, "safe stores lack executable local provisions depth")
		}
		fill, err := WalkDepth(provisions.Depth.Asks, plan.StoresToBuy, true)
		if err != nil {
			return reject(plan, "safe stores: "+err.Error())
		}
		plan.StoresLimitPence = fill.LimitPence
		storeCost = fill.GrossPence + ceilBPS(fill.GrossPence, originBook.CustomsBPS)
		// Per-fill integer customs can round more than once.
		storeCost += plan.StoresToBuy
	}
	var purchase, customs int64
	priceStress := plan.Economics.PriceUncertaintyStressPence
	if plan.Kind != "freight" {
		commodity := bookCommodity(originBook, plan.Commodity)
		if commodity == nil || commodity.Depth == nil {
			return reject(plan, fmt.Sprintf("purchase of %dt %s at %s has no observable local ask depth", plan.Quantity, plan.Commodity, origin.ID))
		}
		// Stores and hold provisions consume the same asks; walk their combined
		// quantity so two individually affordable orders cannot double-count depth.
		quantity := plan.Quantity
		if plan.Commodity == "provisions" {
			quantity += plan.StoresToBuy
		}
		fill, err := WalkDepth(commodity.Depth.Asks, quantity, true)
		if err != nil {
			return reject(plan, fmt.Sprintf("purchase of %dt %s at %s: %s", quantity, plan.Commodity, origin.ID, err.Error()))
		}
		purchase = fill.GrossPence
		if plan.Commodity == "provisions" {
			storeFill, _ := WalkDepth(commodity.Depth.Asks, plan.StoresToBuy, true)
			purchase -= storeFill.GrossPence
		}
		customs = ceilBPS(purchase, originBook.CustomsBPS)
		customs += plan.Quantity
		plan.BuyLimitPence = fill.LimitPence
	}
	if plan.Kind == "spot" {
		var reason string
		revenue, plan.SellLimitPence, priceStress, plan.PriceSource, reason = destinationRevenue(in, plan.Destination, plan.Commodity, plan.Quantity)
		if reason != "" {
			return reject(plan, reason)
		}
	} else if plan.PriceSource == "" {
		plan.PriceSource = "live contract terms"
	}
	// Store consumption has an opportunity cost even when no whole ton must be
	// bought this time. Price that usage against the observed local ask ladder.
	provisions := bookCommodity(originBook, "provisions")
	if provisions == nil || provisions.Depth == nil {
		return reject(plan, "consumed provisions cannot be valued without local depth")
	}
	consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
	consumptionFill, err := WalkDepth(provisions.Depth.Asks, max(1, int64(math.Ceil(consumed))), true)
	if err != nil {
		return reject(plan, "provision consumption has insufficient observed replacement depth")
	}
	provisionExpense := max(storeCost, int64(math.Ceil(float64(consumptionFill.GrossPence+ceilBPS(consumptionFill.GrossPence, originBook.CustomsBPS))*consumed/float64(consumptionFill.Quantity))))
	originBerthDays := max(0.25, portDays/2)
	if in.Ship.Ship.BerthedAt != nil && !in.Ship.Ship.BerthedAt.After(upper) {
		accruedDays := upper.Sub(*in.Ship.Ship.BerthedAt).Seconds() * float64(in.Clock.Calendar.TimeScale) / 86_400
		originBerthDays = max(originBerthDays, accruedDays+portDays/2)
	}
	berthCost := int64(math.Ceil(float64(in.Ports[origin.ID].BerthDuesPerDayPence)*originBerthDays +
		float64(destinationDetail.BerthDuesPerDayPence)*max(0.25, portDays/2)))
	upkeep := int64(math.Ceil(float64(in.FleetDailyUpkeepPence) * gameDays))
	stressBPS := diagnosticStressBPS(in, outward, hull)
	if in.Clock.Calendar.Month >= 3 && in.Clock.Calendar.Month <= 10 {
		daysUntilWinter := float64((10-in.Clock.Calendar.Month)*30 + 31 - in.Clock.Calendar.Day)
		if gameDays >= daysUntilWinter {
			stressBPS = min(6_000, stressBPS+1_500)
		}
	}
	stressHaircut := ceilBPS(revenue+plan.Economics.CollateralPence, stressBPS)
	expectedNet := revenue - purchase - customs - provisionExpense - berthCost - upkeep
	collateral := plan.Economics.CollateralPence
	plan.Economics = Economics{
		RevenuePence:                revenue,
		PurchasePence:               purchase,
		CustomsPence:                customs,
		ProvisionsPence:             provisionExpense,
		BerthDuesPence:              berthCost,
		UpkeepPence:                 upkeep,
		CollateralPence:             collateral,
		ExpectedNetPence:            expectedNet,
		StressHaircutBPS:            stressBPS,
		StressHaircutPence:          stressHaircut,
		PriceUncertaintyStressPence: priceStress,
		StressNetPence:              expectedNet - stressHaircut - priceStress,
		GameDays:                    gameDays,
		NetPencePerGameDay:          float64(expectedNet) / gameDays,
	}
	worstPurchase := purchase
	if plan.Quantity > 0 && plan.BuyLimitPence > 0 {
		if plan.BuyLimitPence > math.MaxInt64/plan.Quantity {
			return reject(plan, "IOC limit-price funding bound exceeds integer pence")
		}
		worstPurchase = max(purchase, plan.Quantity*plan.BuyLimitPence)
	}
	worstCustoms := customs
	if worstPurchase > purchase {
		worstCustoms = ceilBPS(worstPurchase, in.Markets[origin.ID].CustomsBPS) + plan.Quantity
	}
	cashRequired := worstPurchase + worstCustoms + storeCost + berthCost + upkeep + collateral
	if in.Balances[origin.BankingRegion]-cashRequired < in.ReserveByRegion[origin.BankingRegion] {
		return reject(plan, "local vault cannot fund trade, safe stores, round-trip bills and protected reserve")
	}
	destination, exists := findPort(in.Catalog, plan.Destination)
	if !exists {
		return reject(plan, "destination is absent from catalog")
	}
	if destination.BankingRegion != origin.BankingRegion && in.Balances[destination.BankingRegion]-destinationDetail.BerthDuesPerDayPence < in.ReserveByRegion[destination.BankingRegion] {
		return reject(plan, "destination region lacks its protected reserve and harbour bill")
	}
	if expectedNet <= 0 {
		return reject(plan, "non-positive expected cash net after measured depth, buyer customs, round-trip provisions, dues and upkeep")
	}
	plan.Accepted = true
	plan.Reason = "positive expected round-trip cash net; unpriced hazard and price stress are diagnostic, not estimated losses or acceptance gates"
	return plan
}

func destinationRevenue(in PlanningInput, destination, commodity string, quantity int64) (int64, int64, int64, string, string) {
	_, upper, ok := in.serverBounds()
	if !ok {
		return 0, 0, 0, "", "current server clock authority is unavailable"
	}
	if in.fresh("books/" + destination) {
		book := in.Markets[destination]
		entry := bookCommodity(book, commodity)
		if book.Visibility == "quay" && entry != nil && entry.Depth != nil {
			fill, err := WalkDepth(entry.Depth.Bids, quantity, false)
			if err == nil {
				return fill.GrossPence, fill.LimitPence, 0, "observed destination full depth", ""
			}
		}
	}
	// Gossip gives no bid quantity. Limit an exploratory lot to one ton and
	// disclose both price age and a separate uncertainty haircut.
	if quantity == 1 {
		for portID, detail := range in.Ports {
			if !in.fresh("ports/" + portID) {
				continue
			}
			for _, report := range detail.CoffeeHouseGossip {
				if report.SubjectPortID != destination || report.SnapshotAt.IsZero() {
					continue
				}
				age := upper.Sub(report.SnapshotAt)
				if age < 0 || age.Seconds()*float64(in.Clock.Calendar.TimeScale) > 6*3_600 {
					continue
				}
				for _, price := range report.Commodities {
					priceAge := upper.Sub(price.SnapshotAt)
					if price.SnapshotAt.IsZero() || priceAge < 0 || priceAge.Seconds()*float64(in.Clock.Calendar.TimeScale) > 6*3_600 {
						continue
					}
					if price.CommodityID == commodity && price.BestBidPence != nil && *price.BestBidPence > 0 {
						return *price.BestBidPence, max(1, *price.BestBidPence*3/4), ceilBPS(*price.BestBidPence, 2_500), "recent observed gossip; one-ton probe; unverified bid size", ""
					}
				}
			}
		}
	}
	return 0, 0, 0, "", "destination has neither executable full depth nor recent observed one-ton gossip"
}

func diagnosticStressBPS(in PlanningInput, route Route, hull Hull) int64 {
	// This illustrative capital-loss scenario becomes harsher with public
	// exposure drivers. It is not calibrated odds, expected cost or a cash bill,
	// and never changes acceptance, ranking, funding or realized settlements.
	haircut := int64(1_000)
	for _, leg := range route.Legs {
		for _, zone := range in.Catalog.SeaZones {
			if zone.ID != leg.ToNode && zone.ID != leg.FromNode {
				continue
			}
			piracy := zone.BasePiracyBPS
			for _, current := range in.Zones {
				if current.SeaZoneID == zone.ID {
					piracy = current.NetPiracyBPS
				}
			}
			haircut = max(haircut, 1_000+max(0, piracy)/2+max(0, zone.BaseStormBPS)/2)
		}
	}
	if in.Clock.Calendar.Month <= 2 || in.Clock.Calendar.Month >= 11 {
		haircut += 1_500
	}
	if in.Ship.Ship.CannonsCount < min(4, hull.MaxCannons) {
		haircut += 1_000
	}
	return min(6_000, haircut)
}

// DailyUpkeep uses the guide's published billing formula, rounded upward to
// whole pence. Oceanic tons do not enter the coastal/regional sprawl term.
func DailyUpkeep(fleet []ShipState, catalog Catalog, factors int) int64 {
	var sprawlTons, oceanicTons int64
	for _, ship := range fleet {
		if ship.Ship.Status == "sunk" {
			continue
		}
		hull, exists := findHull(catalog, ship.Ship.ShipClass)
		if !exists {
			continue
		}
		if hull.HullCategory == "oceanic" {
			oceanicTons += hull.HoldCapacityTons
		} else {
			sprawlTons += hull.HoldCapacityTons
		}
	}
	sprawl := 4_500 * math.Pow(float64(max(0, sprawlTons-40))/100, 1.45)
	factorCost := 12_000 * math.Pow(float64(max(0, factors)), 1.3)
	return int64(math.Ceil(sprawl+factorCost)) + oceanicTons*100
}

// GrossRemittance rounds conservatively so the observed bps fee cannot leave
// the destination one penny short, regardless of server fee rounding.
func GrossRemittance(net, feeBPS int64) (int64, error) {
	if net <= 0 || net > maximumPence || feeBPS < 0 || feeBPS >= 10_000 {
		return 0, errors.New("invalid remittance amount or observed fee")
	}
	denominator := int64(10_000) - feeBPS
	gross := (net/denominator)*10_000 + ((net%denominator)*10_000+denominator-1)/denominator
	if gross > maximumPence {
		return 0, errors.New("gross remittance exceeds API monetary cap")
	}
	return gross, nil
}
