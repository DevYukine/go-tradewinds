package engine

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const maximumPence int64 = 1_000_000_000_000

// RouteGeometryTTL is the retained lifetime for live and replay route geometry.
const RouteGeometryTTL = 24 * time.Hour

// DepthFill is executable at LimitPence or better, before buyer-only customs.
type DepthFill struct {
	Quantity   int64 `json:"quantity"`
	GrossPence int64 `json:"gross_pence"`
	LimitPence int64 `json:"limit_pence"`
}

// WalkDepth never substitutes a top-of-book price for missing quantity.
func WalkDepth(levels []Level, quantity int64, buy bool) (DepthFill, error) {
	fill, _, err := walkDepthCosts(levels, quantity, buy, 0)
	return fill, err
}

func walkDepthCosts(levels []Level, quantity int64, buy bool, customsBPS int64) (DepthFill, int64, error) {
	if quantity <= 0 || customsBPS < 0 {
		return DepthFill{}, 0, errors.New("quantity must be positive and customs non-negative")
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
	var customs int64
	for _, level := range ordered {
		if level.PricePence <= 0 || level.PricePence > maximumPence || level.RemainingQuantity <= 0 {
			continue
		}
		take := min(quantity-result.Quantity, level.RemainingQuantity)
		if take > (math.MaxInt64-result.GrossPence)/level.PricePence {
			return DepthFill{}, 0, errors.New("depth value overflows pence")
		}
		gross := take * level.PricePence
		if customsBPS > 0 {
			whole := gross / 10_000
			if whole > math.MaxInt64/customsBPS {
				return DepthFill{}, 0, errors.New("depth customs overflow pence")
			}
			whole *= customsBPS
			// Split both operands so even a large valid rate cannot overflow
			// the intermediate numerator before division by 10,000.
			fraction := (customsBPS/10_000)*(gross%10_000) + ceilBPS(gross%10_000, customsBPS%10_000)
			if fraction > math.MaxInt64-whole || whole+fraction > math.MaxInt64-customs {
				return DepthFill{}, 0, errors.New("depth customs overflow pence")
			}
			customs += whole + fraction
		}
		result.GrossPence += gross
		result.Quantity += take
		result.LimitPence = level.PricePence
		if result.Quantity == quantity {
			return result, customs, nil
		}
	}
	return result, customs, errors.New("insufficient observed depth")
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

// Destination valuations separate operating contribution from cash funding.
// Lloyd's expected-claim bound prices hazard; zone-rating stress remains diagnostic.
// ProbabilityEstimate remains nil because no loss probability formula is public.
type Economics struct {
	Basis                       string   `json:"basis,omitempty"`
	RevenueBasis                string   `json:"revenue_basis,omitempty"`
	RevenueAgeGameHours         *float64 `json:"revenue_age_game_hours,omitempty"`
	RevenueCalibrationBPS       int64    `json:"revenue_calibration_bps"`
	RevenueCalibrationReason    string   `json:"revenue_calibration_reason,omitempty"`
	RevenueAgeAddonBPS          int64    `json:"revenue_age_addon_bps,omitempty"`
	RevenueHorizonGameDays      float64  `json:"revenue_horizon_game_days,omitempty"`
	RevenuePence                int64    `json:"revenue_pence"`
	UncalibratedRevenuePence    *int64   `json:"uncalibrated_revenue_pence,omitempty"`
	RevenueQuantityTons         int64    `json:"revenue_quantity_tons,omitempty"`
	PurchasePence               int64    `json:"purchase_pence"`
	CustomsPence                int64    `json:"customs_pence"`
	ProvisionsPence             int64    `json:"provisions_pence"`
	StoreCashPence              int64    `json:"store_cash_pence"`
	BerthDuesPence              int64    `json:"berth_dues_pence"`
	UpkeepPence                 int64    `json:"upkeep_pence"`
	TollPence                   int64    `json:"toll_pence"`
	FundingFeesPence            int64    `json:"funding_fees_pence,omitempty"`
	CollateralPence             int64    `json:"collateral_pence"`
	HazardBPS                   int64    `json:"hazard_bps"`
	HazardPence                 int64    `json:"hazard_pence"`
	HazardSource                string   `json:"hazard_source,omitempty"`
	VoyagePremiumPence          int64    `json:"voyage_premium_pence,omitempty"`
	ExpectedNetPence            int64    `json:"expected_net_pence"`
	StressHaircutBPS            int64    `json:"stress_haircut_bps"`
	StressHaircutPence          int64    `json:"stress_haircut_pence"`
	PriceUncertaintyStressPence int64    `json:"price_uncertainty_stress_pence"`
	StressNetPence              int64    `json:"stress_net_pence"`
	GameDays                    float64  `json:"game_days"`
	NetPencePerGameDay          float64  `json:"net_pence_per_game_day"`
	RankScorePencePerGameDay    float64  `json:"rank_score_pence_per_game_day"`
	BallastRepeatGameDays       float64  `json:"ballast_repeat_game_days,omitempty"`
	BallastRepeatNetPence       int64    `json:"ballast_repeat_net_pence"`
	ProbabilityEstimate         *float64 `json:"probability_estimate"`
	RankScoreSet                bool     `json:"rank_score_set,omitempty"`
}

type Continuation struct {
	Destination string  `json:"destination"`
	NetPence    int64   `json:"net_pence"`
	GameDays    float64 `json:"game_days"`
}

type Plan struct {
	Kind                  string              `json:"kind"`
	ShipID                string              `json:"ship_id"`
	Origin                string              `json:"origin"`
	Destination           string              `json:"destination"`
	Commodity             string              `json:"commodity"`
	ContractID            string              `json:"contract_id,omitempty"`
	FreightIDs            []string            `json:"freight_ids"`
	Quantity              int64               `json:"quantity"`
	BuyLimitPence         int64               `json:"buy_limit_pence"`
	SellLimitPence        int64               `json:"sell_limit_pence"`
	StoresToBuy           int64               `json:"stores_to_buy"`
	StoresLimitPence      int64               `json:"stores_limit_pence"`
	TargetStoresTons      float64             `json:"target_stores_tons"`
	Deadline              *time.Time          `json:"deadline,omitempty"`
	Economics             Economics           `json:"economics"`
	Accepted              bool                `json:"accepted"`
	Reason                string              `json:"reason"`
	PriceSource           string              `json:"price_source"`
	ExpectedArrival       time.Time           `json:"expected_arrival"`
	Competition           CompetitionEvidence `json:"competition"`
	Continuation          *Continuation       `json:"continuation,omitempty"`
	competitionComparable bool
	sunkFundingFeesPence  int64
	onwardSizing          planSizing
	onwardPriority        float64
	EventPriceStress      bool `json:"event_price_stress,omitempty"`
}

type planSizing struct {
	maximum       int64
	rewardPerTon  int64
	freightReward int64
	extraTons     int64
}

type PlayerBidSellFill struct {
	PortID      string    `json:"port_id"`
	CommodityID string    `json:"commodity_id"`
	Side        string    `json:"side"`
	Quantity    int64     `json:"quantity"`
	CreatedAt   time.Time `json:"created_at"`
}

type planningHazardBound struct {
	bps       int64
	source    string
	available bool
	cover     HazardQuote
}

type PlanningInput struct {
	Now                    time.Time
	CompanyID              string
	Ship                   ShipState
	Catalog                Catalog
	Fleet                  []ShipState
	HazardQuotes           map[string]HazardQuote
	growthHullValuePence   int64
	Clock                  Clock
	Zones                  []ZoneObservation
	MarqueZones            map[string]bool
	Markets                map[string]Market
	Ports                  map[string]PortDetail
	Routes                 map[string]Route
	Contracts              []Contract
	Balances               map[string]int64
	ReserveByRegion        map[string]int64
	DeferredLoanReserves   []loanReserveDeferral
	ReservePence           int64
	SourcingAllowancePence *int64
	// Other active relocations retain their modeled or already incurred risk.
	SourcingCommittedPence int64
	ScoutCommittedPence    int64
	RelocationTargets      map[string]string
	// Nil retains the live 200-bps per-probe cash cap.
	SourcingCashBPS           *int64
	SourcingProbeLanes        map[string]bool
	SourcingLoadingBlocked    bool
	SourcingIdleEligible      bool
	SourcingProbeExpensePence int64
	SourcingProbeCompletions  map[string]time.Time
	SourcingProbeCostsUnknown bool
	SourcingProbeHistory      sourcingProbeHistory
	BookObservedAt            map[string]time.Time
	QuayBookMemory            map[string]RetainedQuayBook
	BookDrift                 []BookDriftSample
	RevenueCalibration        store.RevenueCalibrationModel
	retainedDriftByGroup      map[string]float64
	MarketShocks              map[string]map[string][]MarketShock
	marketEventsInvalid       bool
	SourcingHistoryError      string
	Metadata                  map[string]DatasetMeta
	Generation                string
	OwnedOrderIDs             map[string]bool
	ReservedPlayerBidTons     map[string]map[string]int64
	ReservedRetainedBidTons   map[string]map[string]int64
	PlayerBidSellFills        []PlayerBidSellFill
	FleetDailyUpkeepPence     int64
	Rates                     []Rate
	Competition               *store.CompetitionSummary
	ignorePlayerBids          bool
	pinnedHazard              *Economics
	singleLegOnly             bool
	onwardDelay               time.Duration
	hazardBounds              map[string]planningHazardBound
	hazardEstimate            *hazardCalibration
}

func (in PlanningInput) fresh(dataset string) bool {
	meta, exists := in.Metadata[dataset]
	if !exists || !meta.Complete || meta.Stale || meta.Error != "" || meta.UpdatedAt.IsZero() ||
		meta.UpdatedAt.After(in.Now.Add(time.Second)) {
		return false
	}
	// Public geometry and hull-derived provisions do not change with each
	// observation. Refresh invalidates them when the unlocked phase changes.
	if strings.HasPrefix(dataset, "routes/") {
		return in.Now.Sub(meta.UpdatedAt) <= RouteGeometryTTL
	}
	if (in.Generation != "" && meta.Generation != in.Generation) || in.Now.Sub(meta.UpdatedAt) > 2*time.Minute {
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

func (in PlanningInput) deadlineOpen(deadline *time.Time) bool {
	_, upper, ok := in.serverBounds()
	return ok && (deadline == nil || upper.Add(in.onwardDelay).Before(*deadline))
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
	if candidate.competitionComparable && other.competitionComparable &&
		candidate.Competition.PreferenceShips != other.Competition.PreferenceShips {
		return candidate.Competition.PreferenceShips < other.Competition.PreferenceShips
	}
	if candidateRate, otherRate := planRankScore(candidate.Economics), planRankScore(other.Economics); candidateRate != otherRate {
		return candidateRate > otherRate
	}
	if candidate.Economics.ExpectedNetPence != other.Economics.ExpectedNetPence {
		return candidate.Economics.ExpectedNetPence > other.Economics.ExpectedNetPence
	}
	if candidate.Economics.GameDays != other.Economics.GameDays {
		return candidate.Economics.GameDays < other.Economics.GameDays
	}
	if candidate.Destination != other.Destination {
		return candidate.Destination < other.Destination
	}
	if candidate.Commodity != other.Commodity {
		return candidate.Commodity < other.Commodity
	}
	if candidate.Kind != other.Kind {
		return candidate.Kind < other.Kind
	}
	if candidate.ContractID != other.ContractID {
		return candidate.ContractID < other.ContractID
	}
	if order := slices.Compare(candidate.FreightIDs, other.FreightIDs); order != 0 {
		return order < 0
	}
	return candidate.Quantity > other.Quantity
}

// PlanVoyages returns accepted and rejected alternatives, accepted first and
// ranked by two-leg contribution per game day. Admission remains single-leg.
func PlanVoyages(in PlanningInput) []Plan {
	plans := planVoyages(in)
	rankPlans(plans, in)
	return plans
}

func hasAcceptedVoyage(in PlanningInput) bool {
	for _, plan := range planVoyages(in) {
		if plan.Accepted {
			return true
		}
	}
	return false
}

func planVoyages(in PlanningInput) []Plan {
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
	in.hazardBounds = make(map[string]planningHazardBound, len(in.Catalog.Ports))
	in.retainedDriftByGroup = make(map[string]float64, 5)
	var plans []Plan
	for _, contract := range in.Contracts {
		if contract.ContractType == "charter" && contract.OriginPortID != origin.ID {
			plan := Plan{Kind: "charter", ShipID: ship.Ship.ID, Origin: origin.ID, Destination: contract.OriginPortID, Commodity: contract.CommodityID, ContractID: contract.ID, Deadline: contract.ExpiresAt}
			if contract.Status != "open" || contract.Stale || contract.RewardPerTonPence == nil || contract.RemainingQuantity <= 0 {
				plans = append(plans, reject(plan, "charter is closed, gossiped, lacks typed reward or is already filled"))
				continue
			}
			plans = append(plans, sizePlan(in, hull, origin, plan, contract.RemainingQuantity, *contract.RewardPerTonPence, 0, 0))
		}
		if contract.ContractType == "freight" && contract.OriginPortID == origin.ID {
			plan := Plan{Kind: "freight", ShipID: ship.Ship.ID, Origin: origin.ID, Destination: contract.DestPortID, Commodity: contract.CommodityID, ContractID: contract.ID, FreightIDs: []string{contract.ID}}
			if contract.Status != "open" || contract.Stale || contract.IssuerCompanyID == in.CompanyID ||
				contract.QuantityRequired <= 0 || !in.deadlineOpen(contract.ExpiresAt) {
				plans = append(plans, reject(plan, "freight is unavailable, expired, gossiped or self-issued"))
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
			plans = append(plans, sizePlan(in, hull, origin, plan, ship.EffectiveHoldCapacityTons-ship.CargoUsedTons, 0, 0, 0))
		}
	}
	// Share voyage costs before the profitability gate. A rejected standalone
	// load can still contribute to a profitable manifest; its chosen quantity
	// is not a bound on the jointly sized cargo. Freight lots stay indivisible.
	baseCount := len(plans)
	for i := 0; i < baseCount; i++ {
		plan := plans[i]
		if plan.Kind != "charter" && plan.Kind != "spot" {
			continue
		}
		maximum := ship.EffectiveHoldCapacityTons - ship.CargoUsedTons
		var rewardPerTon int64
		if plan.Kind == "charter" {
			var available bool
			for _, charter := range in.Contracts {
				if charter.ID == plan.ContractID && charter.ContractType == "charter" &&
					charter.Status == "open" && !charter.Stale && charter.RemainingQuantity > 0 &&
					charter.RewardPerTonPence != nil && *charter.RewardPerTonPence > 0 {
					available = true
					maximum = min(maximum, charter.RemainingQuantity)
					rewardPerTon = *charter.RewardPerTonPence
					break
				}
			}
			if !available {
				continue
			}
		}
		for _, freight := range in.Contracts {
			if freight.ContractType != "freight" ||
				freight.Status != "open" ||
				freight.Stale ||
				!in.deadlineOpen(freight.ExpiresAt) ||
				freight.OriginPortID != origin.ID ||
				freight.DestPortID != plan.Destination ||
				freight.IssuerCompanyID == in.CompanyID ||
				freight.QuantityRequired <= 0 {
				continue
			}
			mixedBase := plan
			mixedBase.Kind = plan.Kind + "+freight"
			mixedBase.FreightIDs = []string{freight.ID}
			mixedBase.Economics = Economics{CollateralPence: freight.CollateralPence}
			mixedBase.TargetStoresTons = 0
			mixedMaximum := min(maximum, ship.EffectiveHoldCapacityTons-ship.CargoUsedTons-freight.QuantityRequired)
			plans = append(plans, sizePlan(in, hull, origin, mixedBase, mixedMaximum, rewardPerTon, freight.RewardPence, freight.QuantityRequired))
		}
	}
	if len(plans) == 0 {
		plans = append(plans, reject(Plan{Kind: "hold", ShipID: ship.Ship.ID}, "no observed charter, executable destination book or local freight"))
	}
	if in.singleLegOnly {
		// One-ton quotes use full destination costs. Only cost-dominated
		// rejections may compete for sizing into a profitable larger lot.
		for i := range plans {
			if plans[i].onwardSizing.maximum == 0 {
				plans[i].onwardPriority = math.Inf(-1)
				if plans[i].Accepted {
					plans[i].onwardPriority = plans[i].Economics.NetPencePerGameDay
				}
			}
		}
		slices.SortStableFunc(plans, func(left, right Plan) int {
			return cmp.Compare(right.onwardPriority, left.onwardPriority)
		})
		plans = plans[:min(3, len(plans))]
		full := in
		full.singleLegOnly = false
		for i, plan := range plans {
			if sizing := plan.onwardSizing; sizing.maximum > 0 {
				plans[i] = sizePlan(full, hull, origin, plan, sizing.maximum, sizing.rewardPerTon, sizing.freightReward, sizing.extraTons)
			}
		}
	}
	slices.SortStableFunc(plans, func(left, right Plan) int {
		if betterPlan(left, right) {
			return -1
		}
		if betterPlan(right, left) {
			return 1
		}
		return 0
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

func sizePlan(in PlanningInput, hull Hull, origin Port, plan Plan, maximum, rewardPerTon, freightReward, extraTons int64) Plan {
	best := reject(plan, "no whole-ton size has positive destination contribution and adequate reserves")
	best.onwardSizing = planSizing{}
	best.onwardPriority = math.Inf(-1)
	maximum = min(maximum, in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons-extraTons)
	if in.singleLegOnly && maximum > 0 {
		plan.Quantity = 1
		plan = evaluatePlan(in, hull, origin, plan, rewardPerTon+freightReward, extraTons)
		if plan.PriceSource == "recent observed gossip; one-ton probe; unverified bid size" {
			maximum = 1
		}
		plan.onwardSizing = planSizing{maximum, rewardPerTon, freightReward, extraTons}
		plan.onwardPriority = math.Inf(-1)
		if plan.Economics.GameDays > 0 && (plan.Accepted ||
			plan.Reason == "non-positive stressed destination contribution after fitted receipt uncertainty" ||
			plan.Reason == "below 2% minimum margin on purchase plus customs") {
			fixedHazard := int64(0)
			if in.pinnedHazard == nil {
				fixedHazard, _ = hazardCharge(plan.Economics.CollateralPence, plan.Economics.HazardBPS)
			}
			unitContribution := float64(plan.Economics.RevenuePence) - float64(freightReward) -
				float64(plan.Economics.PurchasePence) - float64(plan.Economics.CustomsPence) - float64(plan.Economics.HazardPence-fixedHazard)
			projectedNet := float64(plan.Economics.ExpectedNetPence) + unitContribution*float64(maximum-1)
			if in.pinnedHazard == nil && hull.HullCategory == "oceanic" {
				route := in.Routes[routeKey(plan.Origin, plan.Destination, hull.ID)]
				if oceanicVoyage(route, in.Catalog.SeaZones) {
					bound, cached := in.hazardBounds[plan.Destination]
					if !cached {
						bound = resolvePlanningHazard(in, route)
						bound.cover = planningVoyageCover(in, hull, route, bound)
					}
					cargo := checkedPenceProduct(maximum, growthCashTotal(plan.Economics.PurchasePence, plan.Economics.CustomsPence))
					premium, valid := plannedVoyagePremium(in, hull, route, bound.cover, cargo,
						voyagePlanningFreeCash(in, cargo, plan.Economics.StoreCashPence))
					if !valid {
						return plan
					}
					if premium > 0 || plan.Economics.VoyagePremiumPence > 0 {
						projectedHazard := int64(0)
						if premium == 0 {
							projectedHazard, _ = hazardCharge(cargo, bound.bps)
						}
						// Re-price the projected cargo and any cover switch-on;
						// the hull premium is a single fixed passage charge.
						projectedNet = float64(plan.Economics.ExpectedNetPence) +
							(unitContribution+float64(plan.Economics.HazardPence-fixedHazard))*float64(maximum-1) +
							float64(plan.Economics.HazardPence-fixedHazard+plan.Economics.VoyagePremiumPence) -
							float64(projectedHazard+premium)
					}
				}
			}
			projectedStress := float64(plan.Economics.PriceUncertaintyStressPence) * float64(maximum)
			if projectedNet-projectedStress > 0 {
				plan.onwardPriority = projectedNet / plan.Economics.GameDays
			}
		}
		return plan
	}
	plan.onwardSizing = planSizing{}
	for quantity := int64(1); quantity <= maximum; quantity++ {
		candidate := plan
		candidate.Quantity = quantity
		candidate = evaluatePlan(in, hull, origin, candidate, quantity*rewardPerTon+freightReward, extraTons)
		if quantity == 1 || betterPlan(candidate, best) {
			best = candidate
		}
	}
	return best
}

func pricePlan(in PlanningInput, hull Hull, origin Port, plan Plan, revenue, extraTons int64) Plan {
	outward, err := observedPlanningRoute(in, origin.ID, plan.Destination, hull)
	if err != nil {
		return reject(plan, err.Error())
	}
	if !in.fresh("books/"+origin.ID) || !in.fresh("ports/"+origin.ID) || !in.fresh("ports/"+plan.Destination) {
		return reject(plan, "local depth or delivery harbour dues are not fresh")
	}
	originBook := in.Markets[origin.ID]
	if originBook.Visibility != "quay" {
		return reject(plan, "local full depth requires quay visibility")
	}
	// Delivery includes the laden sailing bound, handling and current queues.
	destinationDetail := in.Ports[plan.Destination]
	if destinationDetail.EffectiveBerths <= 0 {
		return reject(plan, "destination berth capacity is unavailable")
	}
	sailingDays, portDays := deliveryGameDays(outward, hull, destinationDetail, plan.Quantity+extraTons)
	gameDays := sailingDays + portDays
	outwardDays := sailingDays
	_, upper, ok := in.serverBounds()
	if !ok {
		return reject(plan, "current server clock authority is unavailable")
	}
	arrival := upper.Add(in.onwardDelay).Add(gameDuration(outwardDays+portDays, in.Clock.Calendar.TimeScale))
	plan.ExpectedArrival = arrival
	if plan.Deadline != nil && !arrival.Add(gameDuration(1, in.Clock.Calendar.TimeScale)).Before(*plan.Deadline) {
		return reject(plan, "charter deadline lacks one game day of delivery slack")
	}
	voyageStores := outward.SafeProvisionsTons + float64(hull.CrewRequired)/200*portDays + 1
	// A capital candidate may explicitly fund safe stores for a repeat. Normal
	// delivery candidates have no additional target and never require a return.
	storesNeeded := max(voyageStores, 1+float64(hull.CrewRequired)/200*2, plan.TargetStoresTons)
	plan.TargetStoresTons = storesNeeded
	plan.StoresToBuy = max(0, int64(math.Ceil(storesNeeded-in.Ship.Stores.ProvisionsTons)))
	storeIncrease := int64(math.Ceil(in.Ship.Stores.ProvisionsTons+float64(plan.StoresToBuy))) - int64(math.Ceil(in.Ship.Stores.ProvisionsTons))
	if plan.Quantity+extraTons+storeIncrease > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons {
		return reject(plan, "cargo, sealed freight and safe stores exceed effective hold")
	}
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
	}
	var purchase, customs int64
	priceStress := int64(0)
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
	fixedFreightRevenue := int64(0)
	freightTermsKnown := true
	for _, id := range plan.FreightIDs {
		found := false
		for _, contract := range in.Contracts {
			if contract.ID == id && contract.ContractType == "freight" {
				fixedFreightRevenue = growthCashTotal(fixedFreightRevenue, contract.RewardPence)
				found = true
			}
		}
		freightTermsKnown = freightTermsKnown && found
	}
	cargoRevenue := revenue - fixedFreightRevenue
	if strings.HasPrefix(plan.Kind, "spot") {
		var observedCargoRevenue int64
		var reason string
		observedCargoRevenue, plan.SellLimitPence, priceStress, plan.PriceSource, reason, plan.EventPriceStress = destinationRevenue(in, plan.Destination, plan.Commodity, plan.Quantity, arrival)
		if reason != "" {
			return reject(plan, reason)
		}
		cargoRevenue = observedCargoRevenue
		if plan.Kind == "spot" {
			revenue = cargoRevenue
		} else {
			revenue += cargoRevenue
		}
	} else if plan.PriceSource == "" {
		plan.PriceSource = "live contract terms"
	}
	uncalibratedRevenue := revenue
	if !freightTermsKnown || fixedFreightRevenue == math.MaxInt64 {
		return reject(plan, "fixed freight terms are unavailable for receipt calibration")
	}
	revenueBasis := ""
	switch {
	case plan.PriceSource == "observed destination full depth":
		revenueBasis = "depth"
	case plan.PriceSource == "live contract terms":
		revenueBasis = "contract"
	case plan.PriceSource == playerBidGossipSource || plan.PriceSource == "recent observed gossip; one-ton probe; unverified bid size":
		revenueBasis = "gossip"
	case strings.HasPrefix(plan.PriceSource, retainedBookSource):
		revenueBasis = "retained"
	}
	valuationAt := upper.Add(in.onwardDelay)
	if revenueBasis == "retained" {
		valuationAt = arrival
	}
	revenueHorizon := gameDays + in.onwardDelay.Seconds()*float64(in.Clock.Calendar.TimeScale)/86_400
	calibration, ageAddon := revenueCalibrationFor(in, plan.Destination, plan.Commodity, revenueBasis, revenueHorizon, valuationAt)
	quoteStress := priceStress
	calibrationBPS := calibration.MedianBPS
	calibratedCargoRevenue := cargoRevenue - ceilBPS(cargoRevenue, 10_000-calibrationBPS)
	adverseCargoRevenue := cargoRevenue - ceilBPS(cargoRevenue, 10_000-calibration.AdverseBPS)
	// Gossip bids can vanish before arrival. Keep their executable fallback
	// floor as well as measured receipt uncertainty. Fresh depth is not
	// stressed to its next rung: live on 2026-11-04 that tripled depth stress
	// (455 to 1,514 bps of revenue) and rejected every positive depth lot.
	priceStress = max(0, calibratedCargoRevenue-adverseCargoRevenue)
	if revenueBasis == "gossip" {
		priceStress = max(priceStress, ceilBPS(quoteStress, calibrationBPS))
	}
	revenue = revenue - cargoRevenue + calibratedCargoRevenue
	cargoRevenue = calibratedCargoRevenue
	if revenueBasis == "retained" {
		plan.SellLimitPence = plan.SellLimitPence * calibration.AdverseBPS / 10_000
	}
	calibrationReason := fmt.Sprintf("%s; n=%d; horizon %s game days; adverse haircut %d bps",
		calibration.Source, calibration.Samples, store.RevenueHorizonBucket(revenueHorizon), 10_000-calibration.AdverseBPS)
	priceStress = eventPriceStress(cargoRevenue, priceStress, plan.EventPriceStress)
	// Whole-ton purchases fund safety stock, but only the fraction consumed is
	// expense. Existing stores have the same measured replacement cost.
	consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
	provisionExpense, err := provisionsExpense(originBook, consumed)
	if err != nil {
		return reject(plan, err.Error())
	}
	originBerthDays := max(0.25, portDays/2)
	if in.Ship.Ship.BerthedAt != nil && !in.Ship.Ship.BerthedAt.After(upper) {
		accruedDays := upper.Sub(*in.Ship.Ship.BerthedAt).Seconds() * float64(in.Clock.Calendar.TimeScale) / 86_400
		originBerthDays = max(originBerthDays, accruedDays+portDays/2)
	}
	berthCost := int64(math.Ceil(float64(in.Ports[origin.ID].BerthDuesPerDayPence)*originBerthDays)) +
		int64(math.Ceil(float64(destinationDetail.BerthDuesPerDayPence)*max(0.25, portDays/2)))
	stressBPS := diagnosticStressBPS(in, outward, hull)
	if in.Clock.Calendar.Month >= 3 && in.Clock.Calendar.Month <= 10 {
		daysUntilWinter := float64((10-in.Clock.Calendar.Month)*30 + 31 - in.Clock.Calendar.Day)
		if gameDays >= daysUntilWinter {
			stressBPS = min(6_000, stressBPS+1_500)
		}
	}
	stressHaircut := ceilBPS(revenue, stressBPS)
	storeCash := int64(0)
	if plan.StoresToBuy > 0 {
		storeCash = growthCargoCash(Plan{Quantity: plan.StoresToBuy, BuyLimitPence: plan.StoresLimitPence}, originBook.CustomsBPS)
	}
	expectedNet := revenue - purchase - customs - provisionExpense - berthCost
	var hazardBPS, hazardPence, voyagePremium int64
	var hazardSource string
	if in.pinnedHazard != nil {
		hazardBPS = in.pinnedHazard.HazardBPS
		hazardPence = in.pinnedHazard.HazardPence
		hazardSource = in.pinnedHazard.HazardSource
		voyagePremium = in.pinnedHazard.VoyagePremiumPence
	} else {
		bound, cached := in.hazardBounds[plan.Destination]
		if !cached {
			bound = resolvePlanningHazard(in, outward)
			bound.cover = planningVoyageCover(in, hull, outward, bound)
			if in.hazardBounds != nil {
				in.hazardBounds[plan.Destination] = bound
			}
		}
		hazardBPS, hazardSource = bound.bps, bound.source
		if !bound.available {
			return reject(plan, hazardUnavailable)
		}
		risk := growthCashTotal(purchase, customs, plan.Economics.CollateralPence)
		var validCharge bool
		hazardPence, validCharge = hazardCharge(risk, hazardBPS)
		if risk == math.MaxInt64 || !validCharge {
			return reject(plan, "Lloyd's hazard charge exceeds checked integer-pence bounds")
		}
		cargo := growthCashTotal(purchase, customs)
		freeAfterLot := voyagePlanningFreeCash(in, cargo, storeCash)
		var validPremium bool
		voyagePremium, validPremium = plannedVoyagePremium(in, hull, outward, bound.cover, cargo, freeAfterLot)
		if !validPremium {
			return reject(plan, "Lloyd's voyage premium has no checked hull value or exceeds integer-pence bounds")
		}
		if voyagePremium > 0 {
			// Hull and declared cargo are covered; freight collateral remains
			// a separate liability rather than a second cargo-loss charge.
			hazardPence, _ = hazardCharge(plan.Economics.CollateralPence, hazardBPS)
		}
	}
	expectedNet -= hazardPence + voyagePremium
	collateral := plan.Economics.CollateralPence
	var revenueObservedAt time.Time
	switch revenueBasis {
	case "depth":
		_, revenueObservedAt, _ = in.marketObservationBounds("books/" + plan.Destination)
	case "retained":
		revenueObservedAt = in.QuayBookMemory[plan.Destination].observedAt(plan.Commodity)
	}
	var revenueAge *float64
	if age, valid := quayBookAgeGameDays(upper, revenueObservedAt, in.Clock.Calendar.TimeScale); valid {
		hours := age * 24
		revenueAge = &hours
	}
	plan.Economics = Economics{
		Basis:                       "destination",
		RevenueBasis:                revenueBasis,
		RevenueAgeGameHours:         revenueAge,
		RevenueCalibrationBPS:       calibrationBPS,
		RevenueCalibrationReason:    calibrationReason,
		RevenueAgeAddonBPS:          ageAddon,
		RevenueHorizonGameDays:      revenueHorizon,
		RevenuePence:                revenue,
		UncalibratedRevenuePence:    new(uncalibratedRevenue),
		RevenueQuantityTons:         plan.Quantity,
		PurchasePence:               purchase,
		CustomsPence:                customs,
		ProvisionsPence:             provisionExpense,
		StoreCashPence:              storeCash,
		BerthDuesPence:              berthCost,
		CollateralPence:             collateral,
		HazardBPS:                   hazardBPS,
		HazardPence:                 hazardPence,
		HazardSource:                hazardSource,
		VoyagePremiumPence:          voyagePremium,
		ExpectedNetPence:            expectedNet,
		StressHaircutBPS:            stressBPS,
		StressHaircutPence:          stressHaircut,
		PriceUncertaintyStressPence: priceStress,
		StressNetPence:              expectedNet - stressHaircut - priceStress,
		GameDays:                    gameDays,
		NetPencePerGameDay:          float64(expectedNet) / gameDays,
	}
	plan.Economics.BallastRepeatGameDays, plan.Economics.BallastRepeatNetPence = ballastRepeatEconomics(in, hull, origin, plan, portDays)
	if plan.PriceSource == playerBidGossipSource && expectedNet-priceStress <= 0 && plan.Quantity == 1 {
		playerRevenue, _, _, _, _, _ := destinationRevenue(in, plan.Destination, plan.Commodity, 1, arrival)
		in.ignorePlayerBids = true
		_, limit, stress, source, reason, _ := destinationRevenue(in, plan.Destination, plan.Commodity, 1, arrival)
		if reason == "" {
			plan.PriceSource = source
			plan.SellLimitPence = limit
			plan.Economics.PriceUncertaintyStressPence = stress
			return pricePlan(in, hull, origin, plan, uncalibratedRevenue-playerRevenue, extraTons)
		}
	}
	if reason := admissionRejection(plan, expectedNet); reason != "" {
		return reject(plan, reason)
	}
	plan.Accepted = true
	plan.Reason = "positive stressed destination contribution and at least 2% margin after fitted receipt uncertainty"
	return plan
}

func admissionRejection(plan Plan, remainingNet int64) string {
	if plan.sunkFundingFeesPence > 0 {
		// Funding is already paid. Zero remaining contribution still avoids
		// wasting that fee; this is not fresh admission.
		if remainingNet < 0 {
			return "negative remaining destination contribution after funding"
		}
		if plan.Economics.PriceUncertaintyStressPence > 0 && remainingNet <= plan.Economics.PriceUncertaintyStressPence {
			return "non-positive stressed remaining destination contribution after funding"
		}
		return ""
	}
	if remainingNet-plan.Economics.PriceUncertaintyStressPence <= 0 {
		return "non-positive stressed destination contribution after fitted receipt uncertainty"
	}
	if remainingNet < ceilBPS(plan.Economics.PurchasePence+plan.Economics.CustomsPence, 200) {
		return "below 2% minimum margin on purchase plus customs"
	}
	return ""
}

// observedPlanningRoute shares the delivery and optional repeat safety checks.
func observedPlanningRoute(in PlanningInput, from, to string, hull Hull) (Route, error) {
	key := routeKey(from, to, hull.ID)
	route, exists := in.Routes[key]
	if !exists || !in.fresh("routes/"+key) {
		return Route{}, fmt.Errorf("route is missing, failed or stale: %s", key)
	}
	if err := ValidateRoute(route, hull, in.Catalog.SeaZones); err != nil {
		return Route{}, err
	}
	for _, leg := range route.Legs {
		for _, node := range []string{leg.FromNode, leg.ToNode} {
			zoneID := node
			if port, exists := findPort(in.Catalog, node); exists {
				zoneID = port.SeaZoneID
			}
			if in.MarqueZones[zoneID] {
				return Route{}, fmt.Errorf("route crosses %s, where our own letter of marque raises piracy", zoneID)
			}
		}
	}
	if hull.BaseSpeedCentiknots <= 0 || route.SafeProvisionsTons < 0 ||
		math.IsNaN(route.SafeProvisionsTons) || math.IsInf(route.SafeProvisionsTons, 0) {
		return Route{}, errors.New("route safe provisions or hull speed is unavailable")
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
					break
				}
			}
			if !observed {
				return Route{}, fmt.Errorf("live piracy observation is missing for route zone %s", zone.ID)
			}
		}
	}
	return route, nil
}

// provisionsExpense prices fractional consumption, not the integer safety-stock
// purchase. It is also used for the separately costed empty-return forecast.
func provisionsExpense(book Market, consumed float64) (int64, error) {
	provisions := bookCommodity(book, "provisions")
	if provisions == nil || provisions.Depth == nil {
		return 0, errors.New("consumed provisions cannot be valued without local depth")
	}
	fill, customs, err := walkDepthCosts(provisions.Depth.Asks, max(1, int64(math.Ceil(consumed))), true, book.CustomsBPS)
	if err != nil {
		return 0, errors.New("provision consumption has insufficient observed replacement depth")
	}
	if customs > math.MaxInt64-fill.GrossPence {
		return 0, errors.New("provision replacement cost overflows pence")
	}
	replacementCash := fill.GrossPence + customs
	return int64(math.Ceil(float64(replacementCash) * consumed / float64(fill.Quantity))), nil
}

// ballastRepeatStoreTarget lets capital candidates explicitly fund a safe empty
// return. Pass the target to evaluatePlan, which prices the full purchase and
// enforces shared hold, depth and cash before this forecast can underwrite growth.
func ballastRepeatStoreTarget(in PlanningInput, hull Hull, plan Plan) (float64, error) {
	back, err := observedPlanningRoute(in, plan.Destination, plan.Origin, hull)
	if err != nil {
		return 0, err
	}
	outwardKey := routeKey(plan.Origin, plan.Destination, hull.ID)
	outward, exists := in.Routes[outwardKey]
	originDetail, destinationDetail := in.Ports[plan.Origin], in.Ports[plan.Destination]
	if !exists || !in.fresh("routes/"+outwardKey) ||
		!in.fresh("ports/"+plan.Origin) || !in.fresh("ports/"+plan.Destination) ||
		originDetail.EffectiveBerths <= 0 || destinationDetail.EffectiveBerths <= 0 {
		return 0, errors.New("repeat safe stores need fresh delivery route and both harbours")
	}
	portDays := 1.0/24 + float64(plan.Quantity*4)/1_440 +
		float64(destinationDetail.QueuedShips+1)/float64(destinationDetail.EffectiveBerths)*0.25
	returnPortDays := 30.0/1_440 + float64(originDetail.QueuedShips+1)/float64(originDetail.EffectiveBerths)*0.25
	target := outward.SafeProvisionsTons + back.SafeProvisionsTons +
		float64(hull.CrewRequired)/200*(portDays+returnPortDays) + 1
	return target, nil
}

func ballastLegEconomics(in PlanningInput, hull Hull, origin string, back Route) (float64, int64, int64, bool) {
	detail := in.Ports[origin]
	if detail.EffectiveBerths <= 0 || hull.BaseSpeedCentiknots <= 0 {
		return 0, 0, 0, false
	}
	sailingDays := float64(back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	portDays := 30.0/1_440 + float64(detail.QueuedShips+1)/float64(detail.EffectiveBerths)*0.25
	consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
	provisions, err := provisionsExpense(in.Markets[origin], consumed)
	if err != nil {
		return 0, 0, 0, false
	}
	berth := int64(math.Ceil(float64(detail.BerthDuesPerDayPence) * max(0.25, portDays)))
	return sailingDays + portDays, provisions, berth, true
}

func ballastRepeatEconomics(in PlanningInput, hull Hull, origin Port, plan Plan, portDays float64) (float64, int64) {
	// Fixed freight cannot be cloned into repeat revenue, and gossip has no
	// measured demand. Neither is a repeatable capital-underwriting basis.
	if len(plan.FreightIDs) > 0 ||
		plan.PriceSource != "live contract terms" && plan.PriceSource != "observed destination full depth" {
		return 0, 0
	}
	safeStores, err := ballastRepeatStoreTarget(in, hull, plan)
	if err != nil || in.Ship.Stores.ProvisionsTons+float64(plan.StoresToBuy)+1e-9 < safeStores {
		return 0, 0
	}
	back := in.Routes[routeKey(plan.Destination, origin.ID, hull.ID)]
	returnDays, returnProvisions, returnBerth, ok := ballastLegEconomics(in, hull, origin.ID, back)
	if !ok {
		return 0, 0
	}
	returnInput := in
	returnInput.Ship.Ship.CurrentPortID = plan.Destination
	returnCash := growthCashTotal(sourcingFreeCash(in), plan.Economics.RevenuePence)
	returnSpent := growthCashTotal(in.growthHullValuePence, plan.Economics.PurchasePence, plan.Economics.CustomsPence,
		plan.Economics.StoreCashPence, plan.Economics.BerthDuesPence, plan.Economics.VoyagePremiumPence)
	returnPremium, valid := ballastVoyagePremium(returnInput, hull, back, max(0, returnCash-returnSpent))
	if returnCash == math.MaxInt64 || returnSpent == math.MaxInt64 || !valid {
		return 0, 0
	}
	repeatProvisions := plan.Economics.ProvisionsPence + returnProvisions
	repeatNet := plan.Economics.ExpectedNetPence - returnProvisions - returnBerth - returnPremium
	destination, exists := findPort(in.Catalog, plan.Destination)
	if !exists || origin.BankingRegion == "" || destination.BankingRegion == "" {
		return 0, 0
	}
	if destination.BankingRegion != origin.BankingRegion {
		if !in.fresh("rates") {
			return 0, 0
		}
		feeBPS := int64(-1)
		for _, rate := range in.Rates {
			if rate.FromRegion == destination.BankingRegion && rate.ToRegion == origin.BankingRegion &&
				rate.FeeBPS >= 0 && rate.FeeBPS < 10_000 && (feeBPS < 0 || rate.FeeBPS < feeBPS) {
				feeBPS = rate.FeeBPS
			}
		}
		if feeBPS < 0 {
			return 0, 0
		}
		destinationBerth := int64(math.Ceil(float64(in.Ports[plan.Destination].BerthDuesPerDayPence) * max(0.25, portDays/2)))
		originDues := plan.Economics.BerthDuesPence - destinationBerth + returnBerth
		// Restore only recurring origin cash. Initial safety-stock inventory
		// remains funded once; its consumed replacement is the repeat expense.
		replacementCash := plan.Economics.PurchasePence + plan.Economics.CustomsPence + repeatProvisions + originDues + plan.Economics.VoyagePremiumPence
		gross, err := GrossRemittance(replacementCash, feeBPS)
		if err != nil || plan.Economics.RevenuePence-destinationBerth-returnPremium < gross {
			return 0, 0
		}
		repeatNet -= gross - replacementCash
	}
	return plan.Economics.GameDays + returnDays, repeatNet
}

// deliveryGameDays uses fully laden speed, two berth locks, four minutes per ton
// for loading/unloading, and the observed destination queue. Durations are game days.
func deliveryGameDays(route Route, hull Hull, destination PortDetail, tons int64) (sailingDays, portDays float64) {
	sailingDays = float64(route.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
	portDays = 1.0/24 + float64(tons*4)/1_440
	portDays += float64(destination.QueuedShips+1) / float64(destination.EffectiveBerths) * 0.25
	return sailingDays, portDays
}

func destinationRevenue(in PlanningInput, destination, commodity string, quantity int64, arrival time.Time) (int64, int64, int64, string, string, bool) {
	_, upper, ok := in.serverBounds()
	if !ok {
		return 0, 0, 0, "", "current server clock authority is unavailable", false
	}
	var eventReason string
	if in.fresh("books/" + destination) {
		book := in.Markets[destination]
		if book.Visibility == "quay" {
			from, to, valid := in.marketObservationBounds("books/" + destination)
			if !valid {
				return 0, 0, 0, "", "destination book has no bounded server observation time", false
			}
			required, reason := in.marketObservationPolicy(destination, commodity, from, to, arrival)
			if reason != "" {
				revenue, limit, stress, source, retainedReason, retainedStress := retainedDestinationRevenue(in, destination, commodity, quantity, 0, arrival)
				if retainedReason == "" {
					return revenue, limit, stress, source, "", retainedStress
				}
				return 0, 0, 0, "", reason + "; no usable pre-event retained bids", false
			}
			entry := bookCommodity(book, commodity)
			if entry == nil || entry.Depth == nil {
				return 0, 0, 0, "", "fresh destination quay book lacks commodity bid depth", false
			}
			levels := entry.Depth.Bids
			if reserved := in.ReservedRetainedBidTons[destination][commodity]; reserved > 0 {
				levels = slices.Clone(levels)
				reserveBidLevels(levels, reserved)
			}
			fill, err := WalkDepth(levels, quantity, false)
			if err != nil {
				return 0, 0, 0, "", "fresh destination quay bids cannot fill the whole lot", false
			}
			stress := int64(0)
			var nextBid int64
			for _, level := range levels {
				if level.IsNPC && level.RemainingQuantity > 0 && level.PricePence < fill.LimitPence {
					nextBid = max(nextBid, level.PricePence)
				}
			}
			if nextBid > 0 {
				stress = max(0, fill.GrossPence-quantity*nextBid)
			}
			return fill.GrossPence, fill.LimitPence, eventPriceStress(fill.GrossPence, stress, required), "observed destination full depth", "", required
		}
	}
	var bid, playerPrice, npcBid, fresherBid int64
	var bidStress, playerStress bool
	reserved := in.ReservedPlayerBidTons[destination][commodity]
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
				if price.CommodityID != commodity || price.SnapshotAt.IsZero() || priceAge < 0 ||
					priceAge.Seconds()*float64(in.Clock.Calendar.TimeScale) > 6*3_600 ||
					price.BestBidPence == nil || *price.BestBidPence <= 0 {
					continue
				}
				required, reason := in.marketObservationPolicy(destination, commodity, price.SnapshotAt, price.SnapshotAt, arrival)
				if reason != "" {
					eventReason = reason
					continue
				}
				if bid == 0 || *price.BestBidPence < bid {
					bid = *price.BestBidPence
					bidStress = required
				} else if *price.BestBidPence == bid {
					bidStress = bidStress || required
				}
				if price.SnapshotAt.After(in.QuayBookMemory[destination].observedAt(commodity)) &&
					(fresherBid == 0 || *price.BestBidPence < fresherBid) {
					fresherBid = *price.BestBidPence
				}
				isNPCBid := price.BestPlayerBidPence != nil && *price.BestBidPence > *price.BestPlayerBidPence
				if (isNPCBid || price.BestPlayerBidPence == nil) && (npcBid == 0 || *price.BestBidPence < npcBid) {
					npcBid = *price.BestBidPence
				}
				reportReserved := reserved
				for _, fill := range in.PlayerBidSellFills {
					if fill.Side == "ask" && fill.PortID == destination && fill.CommodityID == commodity &&
						fill.Quantity > 0 && fill.CreatedAt.After(price.SnapshotAt) {
						reportReserved = growthCashTotal(reportReserved, fill.Quantity)
					}
				}
				playerBid, tons := playerBidCapacity(price, reportReserved)
				if !in.ignorePlayerBids && isNPCBid && quantity > 0 && tons >= quantity {
					if playerPrice == 0 || playerBid < playerPrice {
						playerPrice, playerStress = playerBid, required
					} else if playerBid == playerPrice {
						playerStress = playerStress || required
					}
				}
			}
		}
	}
	if playerPrice > 0 {
		npcStressPrice := max(1, npcBid-ceilBPS(npcBid, 2_500))
		revenue := checkedPenceProduct(quantity, playerPrice)
		stressedRevenue := checkedPenceProduct(quantity, npcStressPrice)
		if revenue == math.MaxInt64 || stressedRevenue == math.MaxInt64 {
			return 0, 0, 0, "", "gossip player bid value exceeds integer-pence bounds", false
		}
		stress := eventPriceStress(revenue, max(0, revenue-stressedRevenue), playerStress)
		return revenue, npcStressPrice, stress, playerBidGossipSource, "", playerStress
	}
	revenue, limit, stress, source, reason, required := retainedDestinationRevenue(in, destination, commodity, quantity, fresherBid, arrival)
	if reason == "" {
		return revenue, limit, stress, source, "", required
	}
	if strings.Contains(reason, "market event") {
		eventReason = reason
	}
	// Without sufficient player quantity, gossip remains a one-ton probe.
	if quantity == 1 && bid > 0 {
		return bid, max(1, bid*3/4), ceilBPS(bid, 2_500), "recent observed gossip; one-ton probe; unverified bid size", "", bidStress
	}
	if eventReason != "" {
		return 0, 0, 0, "", eventReason + "; no usable pre-event retained bids", false
	}
	return 0, 0, 0, "", "destination has neither executable full depth, unreserved retained quay bids nor recent observed gossip with enough unreserved player bid quantity", false
}

const playerBidGossipSource = "recent observed gossip; player bid quantity; stressed NPC bid floor"

func playerBidCapacity(report GossipCommodity, reservedTons int64) (pricePence, tons int64) {
	if report.BestPlayerBidPence == nil || report.BestPlayerBidQuantity == nil ||
		*report.BestPlayerBidPence <= 0 || *report.BestPlayerBidQuantity <= 0 ||
		max(0, reservedTons) >= *report.BestPlayerBidQuantity {
		return 0, 0
	}
	return *report.BestPlayerBidPence, *report.BestPlayerBidQuantity - max(0, reservedTons)
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
// whole pence. Half the oceanic tonnage, rounded down, enters the sprawl term
// on top of the flat 1 Cr per oceanic ton.
func DailyUpkeep(fleet []ShipState, catalog Catalog, factors int) int64 {
	var largestHold int64
	var sprawlTons, oceanicTons, unknownTons int64
	for _, ship := range fleet {
		if ship.Ship.Status == "sunk" {
			continue
		}
		hull, exists := findHull(catalog, ship.Ship.ShipClass)
		if !exists {
			for _, excluded := range catalog.UpkeepShipClasses {
				if excluded.ID == ship.Ship.ShipClass {
					hull = excluded
					hull.HoldCapacityTons = 0
					break
				}
			}
		}
		if hull.HoldCapacityTons <= 0 || hull.HoldCapacityTons > 1_000_000 {
			hull.HoldCapacityTons = ship.HoldCapacityTons
			if hull.HoldCapacityTons <= 0 || hull.HoldCapacityTons > 1_000_000 {
				if largestHold == 0 {
					for _, candidate := range catalog.ShipClasses {
						if candidate.HoldCapacityTons > 0 && candidate.HoldCapacityTons <= 1_000_000 {
							largestHold = max(largestHold, candidate.HoldCapacityTons)
						}
					}
					if largestHold == 0 {
						// Catalog validation bounds tonnage even when no
						// usable class or owned ship reports a hold.
						largestHold = 1_000_000
					}
				}
				hull.HoldCapacityTons = largestHold
			}
		}
		switch hull.HullCategory {
		case "oceanic":
			oceanicTons += hull.HoldCapacityTons
		case "coastal", "regional":
			sprawlTons += hull.HoldCapacityTons
		default:
			unknownTons += hull.HoldCapacityTons
		}
	}
	factorCost := 12_000 * math.Pow(float64(max(0, factors)), 1.25)
	bill := func(sprawl, oceanic int64) int64 {
		sprawlCost := 4_500 * math.Pow(float64(max(0, sprawl+oceanic/2-40))/100, 1.45)
		return int64(math.Ceil(sprawlCost+factorCost)) + oceanic*100
	}
	if unknownTons == 0 {
		return bill(sprawlTons, oceanicTons)
	}
	// On each oceanic-tonnage parity the sprawl formula is convex, so the
	// most expensive category allocation is at one of these four endpoints.
	return max(
		bill(sprawlTons+unknownTons, oceanicTons),
		bill(sprawlTons+unknownTons-1, oceanicTons+1),
		bill(sprawlTons+1, oceanicTons+unknownTons-1),
		bill(sprawlTons, oceanicTons+unknownTons),
	)
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
