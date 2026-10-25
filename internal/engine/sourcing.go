package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

const sourcingAskEvidence = "recent public ask gossip; executable quantity and revenue remain unknown"

const sourcingPairLimit = 3

// This basis values physically consumed stores for the exploration risk cap.
// It is a replacement-cost estimate, never realized cash profit; purchase cash
// remains in missionData.ProvisionsPence and the ordinary settlement fields.
type relocationStoreBasis struct {
	InitialTons              float64 `json:"initial_tons"`
	ReplacementPencePerTon   int64   `json:"replacement_pence_per_ton"`
	PurchasedTons            int64   `json:"purchased_tons"`
	PurchasedPence           int64   `json:"purchased_pence"`
	PurchaseUnitCeilingPence int64   `json:"purchase_unit_ceiling_pence"`
}

func sourcingFreeCash(in PlanningInput) int64 {
	var cash, reserves int64
	for _, balance := range in.Balances {
		if balance < 0 || balance > math.MaxInt64-cash {
			return 0
		}
		cash += balance
	}
	for _, reserve := range in.ReserveByRegion {
		if reserve < 0 || reserve > math.MaxInt64-reserves {
			return 0
		}
		reserves += reserve
	}
	return max(0, cash-reserves)
}

func sourcingNormalAllowance(in PlanningInput) int64 {
	allowance := min(int64(20_000), sourcingFreeCash(in)/50)
	if in.SourcingAllowancePence != nil {
		allowance = min(allowance, max(0, *in.SourcingAllowancePence))
	}
	return allowance
}

func sourcingAllowance(in PlanningInput) int64 {
	normal := sourcingNormalAllowance(in)
	if !in.SourcingIdleEligible || in.SourcingProbeCostsUnknown || in.SourcingProbeExpensePence < 0 {
		return normal
	}
	free := sourcingFreeCash(in)
	remaining := max(0, 20_000+free/20-in.SourcingProbeExpensePence)
	pilot := min(int64(20_000), free/50, remaining)
	// The liquidity-backed pilot is risk capital, not measured profit and not
	// a second seed to add to the ordinary permanent allowance.
	return max(normal, pilot)
}

func sourcingBudgetReason(in PlanningInput, exposure, allowance int64) string {
	if in.SourcingProbeCostsUnknown {
		return "unpriced permanent probe expense blocks new exploration risk capital"
	}
	if allowance == 0 {
		if sourcingFreeCash(in) == 0 {
			return "protected current cash leaves no exploration risk capital"
		}
		if !in.SourcingIdleEligible && in.SourcingAllowancePence != nil && *in.SourcingAllowancePence == 0 {
			return "ordinary exploration risk capital exhausted; waiting for six observed idle game hours on this ship before a cash-backed inspection"
		}
		return "idle exploration risk capital exhausted after all permanent probe expenses and current protected cash"
	}
	return fmt.Sprintf("sourcing modeled exposure %dp exceeds current exploration risk capital cap %dp; no measured profit is assumed", exposure, allowance)
}

func sourcingLifetimeAllowance(history store.EconomicHistory) int64 {
	if history.UnknownProbeCosts {
		return 0
	}
	// Unknown trading returns cannot replenish the fixed seed. They do not
	// erase its unspent balance: current protected cash still caps every probe.
	commercialProfit := int64(0)
	if !history.UnknownCosts {
		commercialProfit = max(0, history.CommercialNetPence)
	}
	return max(0, 20_000+commercialProfit/50-history.ProbeExpensePence)
}

func publicSourceEvidence(in PlanningInput, source Port, contract Contract) (bool, string, int64) {
	production := slices.Contains(source.Produces, contract.CommodityID) || slices.Contains(source.Manufactures, contract.CommodityID)
	_, upper, ok := in.serverBounds()
	if !ok || in.Clock.Calendar.TimeScale <= 0 {
		return false, "source evidence requires current server clock authority", 0
	}
	var bestAsk int64
	var askAt time.Time
	for portID, detail := range in.Ports {
		if !in.fresh("ports/" + portID) {
			continue
		}
		for _, report := range detail.CoffeeHouseGossip {
			age := upper.Sub(report.SnapshotAt)
			if report.SubjectPortID != source.ID || report.SnapshotAt.IsZero() || age < 0 || age.Seconds()*float64(in.Clock.Calendar.TimeScale) > 6*3_600 {
				continue
			}
			for _, price := range report.Commodities {
				priceAge := upper.Sub(price.SnapshotAt)
				if price.CommodityID != contract.CommodityID || price.SnapshotAt.IsZero() || priceAge < 0 || priceAge.Seconds()*float64(in.Clock.Calendar.TimeScale) > 6*3_600 || price.BestAskPence == nil || *price.BestAskPence <= 0 {
					continue
				}
				if bestAsk == 0 || price.SnapshotAt.After(askAt) || price.SnapshotAt.Equal(askAt) && *price.BestAskPence > bestAsk {
					bestAsk = *price.BestAskPence
					askAt = price.SnapshotAt
				}
			}
		}
	}
	if bestAsk > 0 {
		if contract.RewardPerTonPence != nil && bestAsk >= *contract.RewardPerTonPence {
			return false, "fresh source ask hint leaves no margin below the charter reward", bestAsk
		}
		return true, sourcingAskEvidence, bestAsk
	}
	if production {
		return true, "catalog production/manufacture; executable price and quantity remain unknown", 0
	}
	return false, "source lacks production/manufacture or fresh six-game-hour ask gossip supply evidence", 0
}

// Candidates are ranked before the route-inspection request budget is spent.
// A candidate source never grants private book access.
func sourcingCandidates(in PlanningInput) []Plan {
	var pairs []Plan
	origin, _ := findPort(in.Catalog, in.Ship.Ship.CurrentPortID)
	for _, contract := range in.Contracts {
		if contract.ContractType != "charter" {
			continue
		}
		base := Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID, Origin: origin.ID, Commodity: contract.CommodityID, ContractID: contract.ID, Deadline: contract.ExpiresAt}
		if contract.Status != "open" || contract.Stale || contract.RemainingQuantity <= 0 || contract.RewardPerTonPence == nil || *contract.RewardPerTonPence <= 0 {
			pairs = append(pairs, reject(base, "sourcing requires an open nonstale charter with typed positive reward and remaining demand"))
			continue
		}
		for _, source := range in.Catalog.Ports {
			if source.ID == origin.ID || source.ID == contract.OriginPortID {
				continue
			}
			plan := base
			plan.Destination = source.ID
			if source.Phase > in.Clock.UnlockedPhase {
				pairs = append(pairs, reject(plan, "source port phase is locked"))
				continue
			}
			ok, reason, ask := publicSourceEvidence(in, source, contract)
			plan.PriceSource = reason
			plan.Accepted = ok
			plan.Reason = reason
			if plan.Accepted {
				plan = relocationExposure(in, plan)
				if plan.Accepted {
					if reason := sourcingProbeRejection(in, source.ID, contract, ask); reason != "" {
						plan = reject(plan, reason)
					}
				}
				if plan.Accepted {
					boundedInput := in
					boundedInput.ReserveByRegion = sourcingReserves(in, source)
					allowance := sourcingAllowance(boundedInput)
					if -plan.Economics.ExpectedNetPence > allowance {
						plan = reject(plan, sourcingBudgetReason(boundedInput, -plan.Economics.ExpectedNetPence, allowance))
					}
				}
			}
			pairs = append(pairs, plan)
		}
	}
	if len(pairs) == 0 {
		return []Plan{reject(Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID}, "no live charter/source opportunity for a bounded relocation")}
	}
	sortSourcingPlans(pairs)
	return pairs
}

func sourcingEvidenceKey(source string, charter Contract, ask int64) string {
	var reward int64
	if charter.RewardPerTonPence != nil {
		reward = *charter.RewardPerTonPence
	}
	// Contract IDs, refresh times and gossip timestamps do not make unchanged
	// supply and reward terms new evidence or grant another trip to the source.
	return fmt.Sprintf("%s|%s|%s|probe:%d:%d", source, charter.OriginPortID, charter.CommodityID, ask, reward)
}

func sourcingProbeRejection(in PlanningInput, source string, charter Contract, ask int64) string {
	var latest time.Time
	inspected := false
	for _, lane := range []string{
		sourcingEvidenceKey(source, charter, ask),
		source + "|" + charter.OriginPortID + "|" + charter.CommodityID + "|charter",
	} {
		completed, exists := in.SourcingProbeCompletions[lane]
		if !in.SourcingProbeLanes[lane] && !exists {
			continue
		}
		inspected = true
		if !exists || completed.IsZero() {
			return "unchanged source evidence was already inspected; its recorded completion is unavailable"
		}
		if completed.After(in.Now) {
			return "unchanged source evidence has a future recorded completion; idle reinspection is blocked"
		}
		if completed.After(latest) {
			latest = completed
		}
	}
	if !inspected {
		return ""
	}
	if !in.SourcingIdleEligible {
		return "unchanged source evidence was already inspected; reinspection waits for six observed idle game hours on this ship"
	}
	if in.Now.Before(latest.Add(sourcingReinspectionCooldown)) {
		return "unchanged source evidence is cooling down for 24 real minutes after its latest matching recorded inspection"
	}
	return ""
}

func sourcingUsesPilot(in PlanningInput, source string, charter Contract, ask int64) bool {
	if !in.SourcingIdleEligible {
		return false
	}
	if sourcingAllowance(in) > sourcingNormalAllowance(in) {
		return true
	}
	for _, lane := range []string{
		sourcingEvidenceKey(source, charter, ask),
		source + "|" + charter.OriginPortID + "|" + charter.CommodityID + "|charter",
	} {
		if in.SourcingProbeLanes[lane] {
			return true
		}
		if _, exists := in.SourcingProbeCompletions[lane]; exists {
			return true
		}
	}
	return false
}

func sortSourcingPlans(plans []Plan) {
	sort.SliceStable(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		if a.Accepted != b.Accepted {
			return a.Accepted
		}
		if !a.Accepted && (a.Economics.GameDays > 0) != (b.Economics.GameDays > 0) {
			return a.Economics.GameDays > 0
		}
		aGossip := a.PriceSource == sourcingAskEvidence
		bGossip := b.PriceSource == sourcingAskEvidence
		if aGossip != bGossip {
			return aGossip
		}
		if a.Economics.ExpectedNetPence != b.Economics.ExpectedNetPence {
			return a.Economics.ExpectedNetPence > b.Economics.ExpectedNetPence
		}
		if a.Economics.GameDays != b.Economics.GameDays {
			return a.Economics.GameDays < b.Economics.GameDays
		}
		if a.Destination != b.Destination {
			return a.Destination < b.Destination
		}
		return a.ContractID < b.ContractID
	})
}

func sourcingPairs(in PlanningInput) []Plan {
	pairs := sourcingCandidates(in)
	for i := range pairs {
		if pairs[i].Accepted {
			pairs[i] = validateRelocationTarget(in, pairs[i])
		}
	}
	sortSourcingPlans(pairs)
	count := 0
	for i := range pairs {
		if !pairs[i].Accepted {
			continue
		}
		count++
		if count > sourcingPairLimit {
			pairs[i] = reject(pairs[i], "outside the bounded three-pair public sourcing shortlist")
		}
	}
	return pairs
}

func sourcingAdmissionRejection(in PlanningInput) string {
	ship := in.Ship
	if ship.Ship.ConvoyID != "" {
		return "sourcing cannot divert a convoy-bound ship"
	}
	if ship.Voyage != nil || ship.Ship.CurrentPortID == "" || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
		return "sourcing requires a stationary independently navigable ship in port"
	}
	if !emptyCargo(ship) {
		return "sourcing requires an empty cargo hold"
	}
	for _, contract := range in.Contracts {
		if contract.ContractType == "freight" && contract.Status == "active" && contract.CarrierShipID == ship.Ship.ID {
			return "active sealed freight obligations prohibit sourcing relocation"
		}
	}
	for _, dataset := range []string{"ships", "catalog", "clock", "zones", "contracts", "vaults"} {
		if !in.fresh(dataset) {
			return "sourcing requires fresh " + dataset
		}
	}
	if in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 {
		return "sourcing requires an unpaused authoritative game clock"
	}
	if !in.SourcingLoadingBlocked {
		ordinary := PlanVoyages(in)
		if len(ordinary) > 0 && ordinary[0].Accepted {
			return "an ordinary measured opportunity takes priority over sourcing"
		}
	}
	return ""
}

func planSourcingRelocation(in PlanningInput) []Plan {
	if reason := sourcingAdmissionRejection(in); reason != "" {
		return []Plan{reject(Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID}, reason)}
	}
	return sourcingPairs(in)
}

func sourcingRouteReady(in PlanningInput, from, to string, hull Hull) (Route, error) {
	key := routeKey(from, to, hull.ID)
	route, exists := in.Routes[key]
	if !exists || !in.fresh("routes/"+key) {
		return Route{}, fmt.Errorf("sourcing needs fresh public route %s", key)
	}
	if err := ValidateRoute(route, hull, in.Catalog.SeaZones); err != nil {
		return Route{}, err
	}
	for _, leg := range route.Legs {
		for _, zone := range in.Catalog.SeaZones {
			if zone.ID != leg.FromNode && zone.ID != leg.ToNode {
				continue
			}
			if !slices.ContainsFunc(in.Zones, func(live ZoneObservation) bool { return live.SeaZoneID == zone.ID && live.NetPiracyBPS >= 0 }) {
				return Route{}, fmt.Errorf("sourcing lacks live hazards for %s", zone.ID)
			}
		}
	}
	return route, nil
}

func sourcingReserves(in PlanningInput, source Port) map[string]int64 {
	required := clonePence(in.ReserveByRegion)
	required[source.BankingRegion] = max(required[source.BankingRegion], in.ReservePence+3*in.Ports[source.ID].BerthDuesPerDayPence)
	return required
}

func relocationExposure(in PlanningInput, plan Plan) Plan {
	hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	origin, originExists := findPort(in.Catalog, plan.Origin)
	source, sourceExists := findPort(in.Catalog, plan.Destination)
	if !exists || !originExists || !sourceExists || hull.BaseSpeedCentiknots <= 0 {
		return reject(plan, "sourcing hull or ports are missing from catalog")
	}
	var charter Contract
	for _, contract := range in.Contracts {
		if contract.ID == plan.ContractID {
			charter = contract
			break
		}
	}
	if charter.ContractType != "charter" || charter.Status != "open" || charter.Stale || charter.RemainingQuantity <= 0 ||
		charter.RewardPerTonPence == nil || *charter.RewardPerTonPence <= 0 {
		return reject(plan, "sourcing target charter is no longer live with a typed positive reward")
	}
	destination, exists := findPort(in.Catalog, charter.OriginPortID)
	if !exists || destination.Phase > in.Clock.UnlockedPhase {
		return reject(plan, "charter destination is missing or phase locked")
	}
	outward, err := sourcingRouteReady(in, origin.ID, source.ID, hull)
	if err != nil {
		return reject(plan, err.Error())
	}
	back, err := sourcingRouteReady(in, source.ID, origin.ID, hull)
	if err != nil {
		return reject(plan, err.Error())
	}
	for _, port := range []Port{origin, source, destination} {
		if !in.fresh("ports/"+port.ID) || in.Ports[port.ID].EffectiveBerths <= 0 {
			return reject(plan, "sourcing requires fresh harbour costs and berth capacity at "+port.ID)
		}
	}
	if !in.fresh("books/"+origin.ID) || in.Markets[origin.ID].Visibility != "quay" {
		return reject(plan, "sourcing provisions need fresh local quay depth")
	}
	speed := float64(hull.BaseSpeedCentiknots) / 100 * 0.8
	sailingDays := float64(outward.TotalDistanceNM+back.TotalDistanceNM) / speed / 24
	portDays := 6.0/24 + 1.0/24 + float64(in.Ports[source.ID].QueuedShips+1)/float64(in.Ports[source.ID].EffectiveBerths)*0.25
	gameDays := sailingDays + portDays
	_, upper, ok := in.serverBounds()
	if !ok {
		return reject(plan, "sourcing requires current server clock authority")
	}
	plan.ExpectedArrival = upper.Add(gameDuration(float64(outward.TotalDistanceNM)/speed/24, in.Clock.Calendar.TimeScale))
	plan.TargetStoresTons = max(plan.TargetStoresTons, outward.SafeProvisionsTons+back.SafeProvisionsTons+float64(hull.CrewRequired)/200*portDays+1)
	plan.TargetStoresTons = max(plan.TargetStoresTons, 1+float64(hull.CrewRequired)/200*2)
	plan.StoresToBuy = max(0, int64(math.Ceil(plan.TargetStoresTons-in.Ship.Stores.ProvisionsTons)))
	if plan.StoresToBuy > in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons {
		return reject(plan, "both-way safe sourcing stores do not fit the empty hold")
	}
	book := in.Markets[origin.ID]
	provisions := bookCommodity(book, "provisions")
	if provisions == nil || provisions.Depth == nil {
		return reject(plan, "sourcing consumption replacement has no measured local provisions depth")
	}
	var purchaseCost int64
	if plan.StoresToBuy > 0 {
		fill, err := WalkDepth(provisions.Depth.Asks, plan.StoresToBuy, true)
		if err != nil {
			return reject(plan, "sourcing safe stores: "+err.Error())
		}
		plan.StoresLimitPence = fill.LimitPence
		purchaseCost = plan.StoresToBuy*fill.LimitPence + ceilBPS(plan.StoresToBuy*fill.LimitPence, book.CustomsBPS) + plan.StoresToBuy
	}
	consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
	storeExpense, err := provisionsExpense(book, consumed)
	if err != nil {
		return reject(plan, "sourcing consumed-store replacement: "+err.Error())
	}
	originDays := 0.25
	if in.Ship.Ship.BerthedAt != nil && upper.After(*in.Ship.Ship.BerthedAt) {
		originDays += upper.Sub(*in.Ship.Ship.BerthedAt).Seconds() * float64(in.Clock.Calendar.TimeScale) / 86_400
	}
	berth := int64(math.Ceil(float64(in.Ports[origin.ID].BerthDuesPerDayPence)*originDays + float64(in.Ports[source.ID].BerthDuesPerDayPence)*portDays))
	upkeep := int64(math.Ceil(float64(in.FleetDailyUpkeepPence) * gameDays))
	required := sourcingReserves(in, source)
	required[origin.BankingRegion] += purchaseCost + berth + upkeep
	_, fees, transfers, funded := measuredFunding(in.Balances, required, in.Rates)
	if !funded || len(transfers) > 0 && !in.fresh("rates") {
		return reject(plan, "modeled sourcing costs and protected reserves cannot be funded with observed remittance fees")
	}
	exposure := storeExpense + berth + upkeep + fees
	if exposure <= 0 {
		return reject(plan, "sourcing relocation lacks a positive measured cost exposure")
	}
	plan.Economics = Economics{
		Basis:              "destination",
		ProvisionsPence:    storeExpense,
		StoreCashPence:     purchaseCost,
		FundingFeesPence:   fees,
		BerthDuesPence:     berth,
		UpkeepPence:        upkeep,
		ExpectedNetPence:   -exposure,
		StressNetPence:     -exposure,
		GameDays:           gameDays,
		NetPencePerGameDay: -float64(exposure) / gameDays,
	}
	plan.Accepted = true
	plan.Reason = "bounded cost-only relocation uses exploration risk capital; public supply is not executable stock, price, revenue or measured profit"
	return plan
}

func costRelocation(in PlanningInput, plan Plan) Plan {
	return validateRelocationTarget(in, relocationExposure(in, plan))
}

func validateRelocationTarget(in PlanningInput, plan Plan) Plan {
	if !plan.Accepted {
		return plan
	}
	hull, _ := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	var charter Contract
	for _, contract := range in.Contracts {
		if contract.ID == plan.ContractID {
			charter = contract
			break
		}
	}
	laden, err := sourcingRouteReady(in, plan.Destination, charter.OriginPortID, hull)
	if err != nil {
		return reject(plan, err.Error())
	}
	if _, err := sourcingRouteReady(in, charter.OriginPortID, plan.Destination, hull); err != nil {
		return reject(plan, err.Error())
	}
	outward := in.Routes[routeKey(plan.Origin, plan.Destination, hull.ID)]
	speed := float64(hull.BaseSpeedCentiknots) / 100 * 0.8
	inspectionDays := 6.0/24 + 1.0/24 + float64(in.Ports[plan.Destination].QueuedShips+1)/float64(in.Ports[plan.Destination].EffectiveBerths)*0.25
	// The inspection must retain stores for either the safe empty retreat or
	// the actual charter continuation, rather than substituting the shorter
	// retreat for an arbitrarily expensive source-to-charter leg.
	continuationStores := outward.SafeProvisionsTons + laden.SafeProvisionsTons + float64(hull.CrewRequired)/200*inspectionDays + 1
	if continuationStores > plan.TargetStoresTons {
		plan.TargetStoresTons = continuationStores
		plan = relocationExposure(in, plan)
		if !plan.Accepted {
			return plan
		}
		source, _ := findPort(in.Catalog, plan.Destination)
		boundedInput := in
		boundedInput.ReserveByRegion = sourcingReserves(in, source)
		allowance := sourcingAllowance(boundedInput)
		if -plan.Economics.ExpectedNetPence > allowance {
			return reject(plan, sourcingBudgetReason(boundedInput, -plan.Economics.ExpectedNetPence, allowance))
		}
	}
	arrivalDays := float64(outward.TotalDistanceNM+laden.TotalDistanceNM)/speed/24 + inspectionDays + float64(in.Ship.EffectiveHoldCapacityTons*4)/1_440 + 1
	_, upper, ok := in.serverBounds()
	if !ok || charter.ExpiresAt != nil && !upper.Add(gameDuration(arrivalDays, in.Clock.Calendar.TimeScale)).Before(*charter.ExpiresAt) {
		return reject(plan, "sourcing charter deadline cannot cover relocation, inspection, handling and delivery slack")
	}
	return plan
}

func clonePence(values map[string]int64) map[string]int64 {
	copy := make(map[string]int64, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}

func sizeRelocationCargo(in PlanningInput, probe Plan, charter Contract, spentCostPence int64) Plan {
	base := Plan{Kind: "charter", ShipID: in.Ship.Ship.ID, Origin: in.Ship.Ship.CurrentPortID, Destination: charter.OriginPortID, Commodity: charter.CommodityID, ContractID: charter.ID, Deadline: charter.ExpiresAt}
	if charter.ContractType != "charter" || charter.Status != "open" || charter.Stale || charter.RemainingQuantity <= 0 || charter.RewardPerTonPence == nil || *charter.RewardPerTonPence <= 0 {
		return reject(base, "targeted charter is no longer live/open with typed reward")
	}
	for _, key := range []string{"catalog", "ships", "clock", "zones", "contracts", "vaults"} {
		if !in.fresh(key) {
			return reject(base, "arrival evidence is stale or not fresh: "+key)
		}
	}
	if in.Ship.Voyage != nil || in.Ship.Ship.Status != "berthed" && in.Ship.Ship.Status != "anchored_roadstead" || !emptyCargo(in.Ship) {
		return reject(base, "arrival procurement requires a stationary empty unsealed ship")
	}
	for _, contract := range in.Contracts {
		if contract.ContractType == "freight" && contract.Status == "active" && contract.CarrierShipID == in.Ship.Ship.ID {
			return reject(base, "active sealed freight prohibits sourcing cargo procurement")
		}
	}
	if in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 {
		return reject(base, "arrival procurement requires an unpaused server clock")
	}
	if !in.fresh("books/" + base.Origin) {
		return reject(base, "source depth is stale or not fresh")
	}
	in.Markets = withoutOwnOrders(in.Markets, in.OwnedOrderIDs)
	if in.Markets[base.Origin].Visibility != "quay" {
		return reject(base, "fresh source depth requires quay visibility")
	}
	hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	origin, originExists := findPort(in.Catalog, base.Origin)
	if !exists || !originExists {
		return reject(base, "source or hull is absent from catalog")
	}
	best := reject(base, "no whole-ton size has positive combined cash net after relocation expense")
	maximum := min(charter.RemainingQuantity, in.Ship.EffectiveHoldCapacityTons-in.Ship.CargoUsedTons)
	for quantity := int64(1); quantity <= maximum; quantity++ {
		candidate := base
		candidate.Quantity = quantity
		candidate = evaluatePlan(in, hull, origin, candidate, quantity**charter.RewardPerTonPence, 0)
		if !candidate.Accepted {
			if !best.Accepted && quantity == 1 {
				best.Reason = candidate.Reason
			}
			continue
		}
		candidate.Economics.ExpectedNetPence -= spentCostPence
		candidate.Economics.StressNetPence -= spentCostPence
		if candidate.Economics.BallastRepeatGameDays > 0 {
			candidate.Economics.BallastRepeatNetPence -= spentCostPence
		}
		candidate.Economics.GameDays += max(0, probe.Economics.GameDays)
		candidate.Economics.NetPencePerGameDay = float64(candidate.Economics.ExpectedNetPence) / candidate.Economics.GameDays
		if candidate.Economics.ExpectedNetPence <= 0 {
			if !best.Accepted {
				best.Reason = "non-positive combined net after measured cargo, stores, bills, relocation and funding fees"
			}
			continue
		}
		if !best.Accepted || candidate.Economics.NetPencePerGameDay > best.Economics.NetPencePerGameDay || candidate.Economics.NetPencePerGameDay == best.Economics.NetPencePerGameDay && candidate.Economics.ExpectedNetPence > best.Economics.ExpectedNetPence {
			best = candidate
		}
	}
	return best
}

func (r *Runner) sourcingInput(ctx context.Context, ship ShipState) (PlanningInput, error) {
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return input, err
	}
	input.SourcingLoadingBlocked = r.insuranceActionReady(ship, "buy", "") != nil
	if err := r.loadSourcingHistory(ctx, &input); err != nil {
		return input, err
	}
	return input, nil
}

func (r *Runner) committedPlanningInput(ctx context.Context, ship ShipState) (PlanningInput, error) {
	return r.committedPlanningInputExcept(ctx, ship, "")
}

func (r *Runner) committedPlanningInputExcept(ctx context.Context, ship ShipState, excludedMission string) (PlanningInput, error) {
	input := r.planningInput(ship)
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return input, err
	}
	for region, reserve := range input.ReserveByRegion {
		required[region] = max(required[region], reserve)
	}
	reservations := growthEvidence{}
	reservedFreight := make(map[string]bool)
	ids := make([]string, 0, len(r.stateCache.Ships)+1)
	for _, incumbent := range r.stateCache.Ships {
		ids = append(ids, incumbent.Ship.ID)
		if incumbent.Ship.ID == ship.Ship.ID && len(ids) > 1 {
			ids[0], ids[len(ids)-1] = ids[len(ids)-1], ids[0]
		}
	}
	ids = append(ids, "__growth")
	maintenance := false
	for _, id := range ids {
		if excludedMission != "" && id == excludedMission {
			continue
		}
		mission, data, err := r.loadMission(ctx, id)
		if err != nil {
			return input, err
		}
		if mission == nil {
			continue
		}
		if ship.Ship.ID != "" && id == ship.Ship.ID {
			maintenance = mission.Phase == "maintenance" && data.Plan.Kind == "maintenance"
			continue
		}
		if ship.Ship.ID != "" && id == "__growth" && data.BuiltShipID == ship.Ship.ID {
			continue
		}
		owner, _ := r.ship(id)
		if id == "__growth" {
			owner, _ = r.ship(data.BuiltShipID)
		}
		if data.Settlement == nil {
			for _, contractID := range data.Plan.FreightIDs {
				reservedFreight[contractID] = true
			}
		}
		if ordinaryIncomePending(&data) && (mission.Phase == "provision" || mission.Phase == "buy" || mission.Phase == "freight") &&
			!r.loadingReservationSuspended(mission, &data, owner) {
			origin, originExists := findPort(input.Catalog, data.Plan.Origin)
			destination, destinationExists := findPort(input.Catalog, data.Plan.Destination)
			if originExists && destinationExists && origin.BankingRegion != destination.BankingRegion {
				required[destination.BankingRegion] = max(required[destination.BankingRegion], input.ReservePence)
			}
		}
		r.reserveMissionCash(required, mission, &data, owner)
		// Unsubmitted idle quotes must not mutually block scarce stores.
		// Work serializes the winner's staged action before another consumer;
		// attached requests and every mission's cash remain protected.
		if maintenance && mission.Phase == "maintenance" && data.Plan.Kind == "maintenance" && data.Action == nil {
			continue
		}
		r.reserveGrowthMission(&reservations, mission, &data)
	}
	input.ReserveByRegion = required
	orders := r.ownedOrders()
	input.Markets = withoutOwnOrders(input.Markets, orders)
	if len(reservations.incumbents) > 0 {
		if len(orders) == 0 {
			markets := make(map[string]Market, len(input.Markets))
			for port, book := range input.Markets {
				markets[port] = book
			}
			input.Markets = markets
		}
		contractsOwned := false
		for _, incumbent := range reservations.incumbents {
			plan := incumbent.plan
			input.Markets[plan.Origin] = reserveGrowthBook(input.Markets[plan.Origin], plan.Commodity, incumbent.unbought, 0, plan.StoresToBuy)
			if plan.ContractID == "" {
				input.Markets[plan.Destination] = reserveGrowthBook(input.Markets[plan.Destination], plan.Commodity, 0, incumbent.outstanding, 0)
			} else if incumbent.outstanding > 0 {
				if !contractsOwned {
					input.Contracts = append([]Contract(nil), input.Contracts...)
					contractsOwned = true
				}
				for i := range input.Contracts {
					if input.Contracts[i].ID == plan.ContractID {
						input.Contracts[i].RemainingQuantity = max(0, input.Contracts[i].RemainingQuantity-incumbent.outstanding)
					}
				}
			}
		}
	}
	if len(reservedFreight) > 0 {
		contracts := make([]Contract, 0, len(input.Contracts))
		for _, contract := range input.Contracts {
			if !reservedFreight[contract.ID] {
				contracts = append(contracts, contract)
			}
		}
		input.Contracts = contracts
	}
	return input, nil
}

func (r *Runner) loadSourcingHistory(ctx context.Context, input *PlanningInput) error {
	history, err := r.db.EconomicHistory(ctx, r.account.ID, "")
	if err != nil {
		return err
	}
	allowance := sourcingLifetimeAllowance(history)
	input.SourcingAllowancePence = &allowance
	input.SourcingIdleEligible = r.shipIdleEligible(*input)
	input.SourcingProbeExpensePence = history.ProbeExpensePence
	input.SourcingProbeCostsUnknown = history.UnknownProbeCosts
	input.SourcingProbeLanes = make(map[string]bool)
	input.SourcingProbeCompletions = make(map[string]time.Time)
	if sourcingAllowance(*input) == 0 {
		return nil
	}
	lanes := make([]string, 0)
	seen := make(map[string]bool)
	for _, charter := range input.Contracts {
		if charter.ContractType != "charter" || charter.Status != "open" || charter.Stale ||
			charter.RemainingQuantity <= 0 || charter.RewardPerTonPence == nil || *charter.RewardPerTonPence <= 0 {
			continue
		}
		for _, source := range input.Catalog.Ports {
			if source.Phase > input.Clock.UnlockedPhase || source.ID == charter.OriginPortID {
				continue
			}
			ok, _, ask := publicSourceEvidence(*input, source, charter)
			if !ok {
				continue
			}
			for _, lane := range []string{
				sourcingEvidenceKey(source.ID, charter, ask),
				source.ID + "|" + charter.OriginPortID + "|" + charter.CommodityID + "|charter",
			} {
				if !seen[lane] {
					seen[lane] = true
					lanes = append(lanes, lane)
				}
			}
		}
	}
	for len(lanes) > 0 {
		size := min(len(lanes), 1024)
		completions, err := r.db.RelocationProbeCompletions(ctx, r.account.ID, lanes[:size])
		if err != nil {
			return err
		}
		for lane, completed := range completions {
			input.SourcingProbeLanes[lane] = true
			input.SourcingProbeCompletions[lane] = completed
		}
		lanes = lanes[size:]
	}
	return nil
}

func (r *Runner) considerSourcing(ctx context.Context, ship ShipState) (bool, error) {
	for _, id := range []string{"__treasury", "__growth"} {
		mission, err := r.db.GetMission(ctx, r.account.ID, id)
		if err != nil {
			return false, err
		}
		if mission != nil {
			return false, nil
		}
	}
	for _, other := range r.stateCache.Ships {
		mission, data, err := r.loadMission(ctx, other.Ship.ID)
		if err != nil {
			return false, err
		}
		if mission != nil && (data.Relocation != nil || data.SourcingHandoff != nil) {
			return false, nil
		}
	}
	input, err := r.sourcingInput(ctx, ship)
	if err != nil {
		return false, err
	}
	plans := planSourcingRelocation(input)
	accepted := 0
	for _, plan := range plans {
		if plan.Accepted {
			accepted++
		}
	}
	shown := min(3, len(plans))
	summary := map[string]int{"candidates": len(plans), "accepted": accepted, "shown": shown}
	if err := r.decision(ctx, ship.Ship.ID, "sourcing", "ranked public sourcing candidates; remote executable inventory and proceeds remain unknown", summary); err != nil {
		return false, err
	}
	for _, plan := range plans[:shown] {
		if err := r.decision(ctx, ship.Ship.ID, "sourcing:"+plan.Destination+":"+plan.ContractID, plan.Reason, plan); err != nil {
			return false, err
		}
	}
	if len(plans) == 0 {
		return false, nil
	}
	if !plans[0].Accepted {
		return false, r.hold(ctx, ship.Ship.ID, plans[0].Reason)
	}
	if !r.opts.Execute {
		return false, r.hold(ctx, ship.Ship.ID, "observe mode: funded sourcing inspection is not executed")
	}
	planIndex := 0
	insuranceErr := r.insuranceActionReady(ship, "depart", plans[0].Destination)
	if insuranceErr != nil {
		for i := 1; i < len(plans); i++ {
			if !plans[i].Accepted {
				continue
			}
			if r.insuranceActionReady(ship, "depart", plans[i].Destination) == nil {
				planIndex = i
				insuranceErr = nil
				break
			}
		}
	}
	if insuranceErr != nil {
		return false, r.hold(ctx, ship.Ship.ID, insuranceErr.Error())
	}
	plan := plans[planIndex]
	source, _ := findPort(input.Catalog, plan.Destination)
	input.ReserveByRegion = sourcingReserves(input, source)
	provisions := bookCommodity(input.Markets[plan.Origin], "provisions")
	if provisions == nil || provisions.Depth == nil {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing store-risk basis lacks current local replacement depth")
	}
	fill, err := WalkDepth(provisions.Depth.Asks, max(1, plan.StoresToBuy), true)
	if err != nil {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing store-risk basis: "+err.Error())
	}
	contract, exists := r.contract(plan.ContractID)
	if !exists {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing target contract disappeared before durable intent")
	}
	mission, data := r.newVoyage(ship, plan)
	mission.Phase = "sourcing_fund"
	_, _, ask := publicSourceEvidence(input, source, contract)
	data.Relocation = &relocationIntent{
		TargetContractID:    contract.ID,
		TargetCommodity:     contract.CommodityID,
		TargetDestination:   contract.OriginPortID,
		ExpenseCapPence:     sourcingAllowance(input),
		ModeledExpensePence: -plan.Economics.ExpectedNetPence,
		IdlePilot:           sourcingUsesPilot(input, source.ID, contract, ask),
		StoreBasis: &relocationStoreBasis{
			InitialTons:            ship.Stores.ProvisionsTons,
			ReplacementPencePerTon: fill.LimitPence + ceilBPS(fill.LimitPence, input.Markets[plan.Origin].CustomsBPS),
		},
	}
	data.ProvenLane = sourcingEvidenceKey(source.ID, contract, ask)
	return true, r.saveMission(ctx, mission, &data)
}

func relocationCashCost(data *missionData) int64 {
	return data.AcquisitionPence + data.FeesPence + data.ProvisionsPence + data.ExpensesPence
}

func recordRelocationStores(data *missionData, result tradeResult) {
	if data.Relocation == nil || data.Relocation.StoreBasis == nil {
		return
	}
	basis := data.Relocation.StoreBasis
	if !validTrade(result) || *result.TotalGrossPence > math.MaxInt64-*result.CustomsDutyPence {
		data.CostsKnown = false
		return
	}
	cash := *result.TotalGrossPence + *result.CustomsDutyPence
	quantity := *result.FilledQuantity
	if quantity == 0 {
		if cash != 0 {
			data.CostsKnown = false
		}
		return
	}
	if basis.PurchasedTons > math.MaxInt64-quantity || basis.PurchasedPence > math.MaxInt64-cash {
		data.CostsKnown = false
		return
	}
	basis.PurchasedTons += quantity
	basis.PurchasedPence += cash
	unit := cash / quantity
	if cash%quantity != 0 {
		unit++
	}
	basis.PurchaseUnitCeilingPence = max(basis.PurchaseUnitCeilingPence, unit)
}

func relocationSpentExpense(data *missionData, ship ShipState) (int64, bool) {
	if data.Relocation == nil || !data.CostsKnown {
		return 0, false
	}
	basis := data.Relocation.StoreBasis
	if basis == nil {
		// Older missions did not retain a physical store basis. Never erase
		// their paid cash merely because the new estimate is unavailable.
		return relocationCashCost(data), true
	}
	if math.IsNaN(basis.InitialTons) || math.IsInf(basis.InitialTons, 0) || basis.InitialTons < 0 ||
		math.IsNaN(ship.Stores.ProvisionsTons) || math.IsInf(ship.Stores.ProvisionsTons, 0) || ship.Stores.ProvisionsTons < 0 ||
		basis.PurchasedTons < 0 || basis.PurchasedPence != data.ProvisionsPence ||
		basis.ReplacementPencePerTon <= 0 || basis.PurchasedTons > 0 && basis.PurchaseUnitCeilingPence <= 0 {
		return 0, false
	}
	totalTons := basis.InitialTons + float64(basis.PurchasedTons)
	if ship.Stores.ProvisionsTons > totalTons+1e-9 {
		return 0, false
	}
	consumed := max(0, totalTons-ship.Stores.ProvisionsTons)
	// The unconsumed physical stores remain an asset, not another risk debit.
	// The price is an explicit conservative replacement estimate; all actual
	// purchase cash is still recorded separately and remains in cash P&L.
	unit := max(basis.ReplacementPencePerTon, basis.PurchaseUnitCeilingPence)
	storeExpense := math.Ceil(consumed * float64(unit))
	if math.IsInf(storeExpense, 0) || storeExpense >= float64(math.MaxInt64) {
		return 0, false
	}
	expense := int64(storeExpense)
	for _, cost := range []int64{data.AcquisitionPence, data.FeesPence, data.ExpensesPence} {
		if cost < 0 || cost > math.MaxInt64-expense {
			return 0, false
		}
		expense += cost
	}
	return expense, true
}

// Called after actual overhead has been claimed into data.ExpensesPence.
func (r *Runner) relocationExplorationExpense(data *missionData, ship ShipState) (int64, bool) {
	if !r.fresh("ships") {
		return 0, false
	}
	return relocationSpentExpense(data, ship)
}

func (r *Runner) relocationBudget(data *missionData, extra int64) bool {
	if data.Relocation == nil || data.Relocation.ExpenseCapPence <= 0 || extra < 0 {
		return false
	}
	ship, exists := r.ship(data.Plan.ShipID)
	if !exists || !r.fresh("ships") {
		return false
	}
	expense, known := relocationSpentExpense(data, ship)
	bills, _ := r.unclaimedBillsSince(data)
	cap := data.Relocation.ExpenseCapPence
	return known && expense >= 0 && bills >= 0 && expense <= cap && bills <= cap-expense && extra <= cap-expense-bills
}

// The durable cap is an immutable upper bound, not continuing permission to use
// cash that another mission or a newly recorded loss has since committed.
func (r *Runner) relocationSpendingReady(ctx context.Context, ship ShipState, data *missionData, extra int64) error {
	return r.relocationSpendingReadyAtPrice(ctx, ship, data, extra, 0)
}

func (r *Runner) relocationProvisionReady(ctx context.Context, ship ShipState, data *missionData, quantity, limit int64) error {
	if data.Relocation == nil || !data.Relocation.IdlePilot {
		return nil
	}
	if quantity <= 0 || limit <= 0 || limit > math.MaxInt64/quantity {
		return errors.New("idle exploration provision order exceeds integer-pence funding bounds")
	}
	return r.relocationSpendingReadyAtPrice(ctx, ship, data, 0, limit)
}

func (r *Runner) relocationSpendingReadyAtPrice(ctx context.Context, ship ShipState, data *missionData, extra, storeLimit int64) error {
	if data.Relocation == nil || !data.Relocation.IdlePilot {
		return nil
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return err
	}
	for _, key := range []string{"ships", "contracts", "clock", "vaults", "ledger"} {
		if !input.fresh(key) {
			return fmt.Errorf("idle exploration risk capital requires fresh %s", key)
		}
	}
	if input.Clock.Paused || input.Clock.Calendar.TimeScale <= 0 || input.Clock.Calendar.TotalGameHours < data.StartedGameHours {
		return errors.New("idle exploration risk capital requires an unpaused nonregressed game clock")
	}
	atOrigin := ship.Ship.CurrentPortID == data.Plan.Origin && ship.Voyage == nil
	if atOrigin {
		if !r.shipIdleEligible(input) {
			return errors.New("unstarted idle inspection waits for a coherent six-observed-game-hour idle baseline")
		}
		input.SourcingLoadingBlocked = r.insuranceActionReady(ship, "buy", "") != nil
		if reason := sourcingAdmissionRejection(input); reason != "" {
			return fmt.Errorf("unstarted idle inspection is no longer admissible: %s", reason)
		}
	}
	history, err := r.db.EconomicHistory(ctx, r.account.ID, "")
	if err != nil {
		return err
	}
	if history.UnknownProbeCosts {
		return errors.New("unpriced permanent probe expense blocks further idle exploration spending")
	}
	source, exists := findPort(input.Catalog, data.Plan.Destination)
	if !exists {
		return errors.New("idle exploration source is absent from the current catalog")
	}
	input.ReserveByRegion = sourcingReserves(input, source)
	normal := sourcingLifetimeAllowance(history)
	input.SourcingAllowancePence = &normal
	input.SourcingProbeExpensePence = history.ProbeExpensePence
	// Committed source continuation does not re-earn idle after the physical
	// voyage reset. Origin sends above still require current idle eligibility.
	// Neither path acquires a fresh seed or a larger durable cap.
	input.SourcingIdleEligible = true
	bounded := *data
	intent := *data.Relocation
	intent.ExpenseCapPence = min(intent.ExpenseCapPence, sourcingAllowance(input))
	bounded.Relocation = &intent
	if !r.relocationBudget(&bounded, extra) {
		return errors.New("current idle exploration risk capital cannot cover active expense, unclaimed bills and new spending within the immutable cap")
	}
	if atOrigin {
		plan := costRelocation(input, data.Plan)
		if !plan.Accepted {
			return fmt.Errorf("fresh idle inspection is no longer admissible: %s", plan.Reason)
		}
		var storeUnitCeiling int64
		if storeLimit > 0 {
			// Reuse the immutable IOC buyer-cost ceiling, including customs
			// rounding slack, in the very same reduced-cap calculation.
			storeUnitCeiling = iocCash(1, storeLimit, input.Markets[ship.Ship.CurrentPortID].CustomsBPS)
			if storeUnitCeiling == math.MaxInt64 {
				return errors.New("idle exploration provision unit ceiling exceeds integer-pence funding bounds")
			}
		}
		if !r.relocationProjectedBudget(&bounded, ship, input, plan, storeUnitCeiling) {
			return errors.New("fresh projected inspection exposure exceeds current idle exploration risk capital or its immutable cap")
		}
	}
	return nil
}

func (r *Runner) relocationProjectedBudget(data *missionData, ship ShipState, in PlanningInput, plan Plan, storeUnitCeiling int64) bool {
	if data.Relocation == nil || !plan.Accepted || !r.relocationBudget(data, 0) {
		return false
	}
	spent, known := relocationSpentExpense(data, ship)
	if !known {
		return false
	}
	futureStores := plan.Economics.ProvisionsPence
	if basis := data.Relocation.StoreBasis; basis != nil {
		unit := max(storeUnitCeiling, basis.ReplacementPencePerTon, basis.PurchaseUnitCeilingPence)
		hull, exists := findHull(in.Catalog, ship.Ship.ShipClass)
		if !exists || hull.BaseSpeedCentiknots <= 0 {
			return false
		}
		outward := in.Routes[routeKey(plan.Origin, plan.Destination, hull.ID)]
		back := in.Routes[routeKey(plan.Destination, plan.Origin, hull.ID)]
		sailingDays := float64(outward.TotalDistanceNM+back.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8) / 24
		portDays := max(0, plan.Economics.GameDays-sailingDays)
		consumed := float64(hull.CrewRequired)/100*sailingDays + float64(hull.CrewRequired)/200*portDays
		estimate := math.Ceil(consumed * float64(unit))
		if math.IsInf(estimate, 0) || estimate >= float64(math.MaxInt64) {
			return false
		}
		futureStores = max(futureStores, int64(estimate))
	}
	bills, _ := r.unclaimedBillsSince(data)
	futureFees := max(0, -plan.Economics.ExpectedNetPence-plan.Economics.ProvisionsPence-plan.Economics.BerthDuesPence-plan.Economics.UpkeepPence)
	remaining := data.Relocation.ExpenseCapPence - spent
	for _, cost := range []int64{bills, futureStores, plan.Economics.BerthDuesPence, plan.Economics.UpkeepPence, futureFees} {
		if cost < 0 || cost > remaining {
			return false
		}
		remaining -= cost
	}
	return true
}

func (r *Runner) relocationProvisionBudget(data *missionData, ship ShipState, in PlanningInput, quantity, limit int64) bool {
	if quantity <= 0 || limit <= 0 || limit > math.MaxInt64/quantity {
		return false
	}
	unit := limit + ceilBPS(limit, in.Markets[ship.Ship.CurrentPortID].CustomsBPS)
	plan := costRelocation(in, data.Plan)
	return r.relocationProjectedBudget(data, ship, in, plan, unit)
}

func (r *Runner) stageSourcingFunding(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, input PlanningInput, required map[string]int64) (bool, error) {
	_, fees, transfers, ok := measuredFunding(input.Balances, required, input.Rates)
	if !ok {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing funds cannot be placed with observed fees while preserving every donor reserve and incumbent commitment")
	}
	if !r.relocationBudget(data, fees) {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing remittance fees would exceed the durable exploration expense cap")
	}
	if err := r.relocationSpendingReady(ctx, ship, data, fees); err != nil {
		return false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	if len(transfers) == 0 {
		return false, nil
	}
	transfer := transfers[0]
	data.Relocation.FundingRequiredPence = required[transfer.Rate.ToRegion] - input.Balances[transfer.Rate.ToRegion]
	return true, r.stage(ctx, mission, data, ship, "sourcing-remit", "/v1/treasury/remit", map[string]any{
		"from_region":        transfer.Rate.FromRegion,
		"to_region":          transfer.Rate.ToRegion,
		"gross_amount_pence": transfer.Gross,
	})
}

func (r *Runner) relocationDepartureReady(ctx context.Context, ship ShipState, data *missionData) error {
	if data.Relocation == nil || !emptyCargo(ship) || len(freightStops(data.Freight)) != 0 || len(r.activeFreight(ship.Ship.ID)) != 0 {
		return errors.New("relocation departure requires empty cargo and no sealed obligations")
	}
	if ship.Stores.ProvisionsTons+1e-9 < data.Plan.TargetStoresTons {
		return errors.New("relocation departure requires both-way safe stores and existing safety margins")
	}
	if !r.relocationBudget(data, 0) {
		return errors.New("relocation expense cap has been consumed; no further discretionary departure")
	}
	if err := r.relocationSpendingReady(ctx, ship, data, 0); err != nil {
		return err
	}
	input, err := r.sourcingInput(ctx, ship)
	if err != nil {
		return err
	}
	// Re-evaluate every legal leg and target deadline, but do not treat already
	// paid provision purchases as a second prospective purchase.
	plan := costRelocation(input, data.Plan)
	if !plan.Accepted {
		return fmt.Errorf("fresh relocation admission failed: %s", plan.Reason)
	}
	if ship.Stores.ProvisionsTons+1e-9 < plan.TargetStoresTons {
		return errors.New("relocation departure requires the freshly calculated both-way safe stores and safety margins")
	}
	if !r.relocationProjectedBudget(data, ship, input, plan, 0) {
		return errors.New("fresh relocation exposure exceeds its immutable expense cap")
	}
	for region, reserve := range input.ReserveByRegion {
		if input.Balances[region] < reserve {
			return fmt.Errorf("relocation region %s lacks its funded reserve", region)
		}
	}
	return nil
}

func (r *Runner) driveRelocation(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if !emptyCargo(ship) || len(freightStops(data.Freight)) > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
		return false, r.hold(ctx, ship.Ship.ID, "relocation cannot proceed or settle with residual cargo or sealed obligations")
	}
	switch mission.Phase {
	case "sourcing_fund":
		if err := r.insuranceActionReady(ship, "depart", mission.Destination); err != nil {
			return false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
		input, err := r.sourcingInput(ctx, ship)
		if err != nil {
			return false, err
		}
		if !r.fresh("rates") {
			return false, r.hold(ctx, ship.Ship.ID, "sourcing funding requires current observed regional rates")
		}
		source, exists := findPort(input.Catalog, data.Plan.Destination)
		origin, originExists := findPort(input.Catalog, data.Plan.Origin)
		if !exists || !originExists {
			return false, r.hold(ctx, ship.Ship.ID, "durable sourcing ports are absent from catalog")
		}
		plan := costRelocation(input, data.Plan)
		if !r.relocationProjectedBudget(data, ship, input, plan, 0) {
			return r.abortRelocation(ctx, ship, mission, data, "fresh sourcing costs exceed the durable exploration cap or lack a known expense basis")
		}
		required := sourcingReserves(input, source)
		required[origin.BankingRegion] += plan.Economics.StoreCashPence + plan.Economics.BerthDuesPence + plan.Economics.UpkeepPence
		if changed, err := r.stageSourcingFunding(ctx, ship, mission, data, input, required); changed || err != nil {
			return changed, err
		}
		if _, fees, transfers, funded := measuredFunding(input.Balances, required, input.Rates); !funded || len(transfers) > 0 || !r.relocationBudget(data, fees) {
			return r.abortRelocation(ctx, ship, mission, data, "fresh sourcing reserve funding or its expense allowance is no longer available")
		}
		data.Relocation.FundingRequiredPence = 0
		data.Plan = plan
		mission.Phase = "provision"
		return true, r.saveMission(ctx, mission, data)
	case "provision":
		if ship.Stores.ProvisionsTons+1e-9 < data.Plan.TargetStoresTons {
			return r.provisionMission(ctx, ship, mission, data, data.Plan.TargetStoresTons)
		}
		mission.Phase = "depart"
		return true, r.saveMission(ctx, mission, data)
	case "depart":
		if err := r.relocationDepartureReady(ctx, ship, data); err != nil {
			return r.abortRelocation(ctx, ship, mission, data, err.Error())
		}
		return r.prepareDeparture(ctx, ship, mission, data)
	case "sailing":
		if ship.Ship.Status == "sailing" {
			return false, r.hold(ctx, ship.Ship.ID, "sourcing relocation waits for authoritative source arrival")
		}
		if ship.Voyage != nil || ship.Ship.CurrentPortID != data.Plan.Destination {
			return false, r.hold(ctx, ship.Ship.ID, "sourcing arrival disagrees with the durable source port")
		}
		data.Relocation.InspectUntilGameHour = r.stateCache.Clock.Calendar.TotalGameHours + 6
		mission.Phase = "inspect"
		return true, r.saveMission(ctx, mission, data)
	case "arrival", "inspect":
		if ship.Voyage != nil || ship.Ship.CurrentPortID != data.Plan.Destination || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			return false, r.hold(ctx, ship.Ship.ID, "sourcing inspection requires authoritative stationary source presence")
		}
		if data.Relocation.InspectUntilGameHour == 0 {
			data.Relocation.InspectUntilGameHour = r.stateCache.Clock.Calendar.TotalGameHours + 6
			mission.Phase = "inspect"
			return true, r.saveMission(ctx, mission, data)
		}
		contract, exists := r.contract(data.Relocation.TargetContractID)
		if r.stateCache.Clock.Calendar.TotalGameHours >= data.Relocation.InspectUntilGameHour {
			return r.closeAtDestination(ctx, ship, mission, data)
		}
		if !exists || contract.Status != "open" || contract.Stale || !r.deadlineOpen(contract.ExpiresAt) {
			return r.closeAtDestination(ctx, ship, mission, data)
		}
		input, err := r.sourcingInput(ctx, ship)
		if err != nil {
			return false, err
		}
		bills, _ := r.unclaimedBillsSince(data)
		spent := relocationCashCost(data) + bills
		plan := sizeRelocationCargo(input, data.Plan, contract, spent)
		if plan.Accepted {
			if err := r.insuranceActionReady(ship, "buy", plan.Destination); err != nil {
				return false, r.hold(ctx, ship.Ship.ID, err.Error())
			}
			required := cargoFundingRequired(input, plan)
			if changed, err := r.stageSourcingFunding(ctx, ship, mission, data, input, required); changed || err != nil {
				return changed, err
			}
			_, fees, transfers, funded := measuredFunding(input.Balances, required, input.Rates)
			if !funded || len(transfers) != 0 || !r.relocationBudget(data, fees) {
				return r.closeAtDestination(ctx, ship, mission, data)
			}
			handoff := *data.Relocation
			data.SourcingHandoff = &handoff
			data.SourcingSpentPence = spent
			return r.closeAtDestination(ctx, ship, mission, data)
		}
		if r.stateCache.Clock.Calendar.TotalGameHours >= data.Relocation.InspectUntilGameHour || !r.relocationBudget(data, 0) ||
			input.fresh("books/"+ship.Ship.CurrentPortID) && input.Markets[ship.Ship.CurrentPortID].Visibility == "quay" && input.fresh("routes/"+routeKey(ship.Ship.CurrentPortID, contract.OriginPortID, ship.Ship.ShipClass)) {
			return r.closeAtDestination(ctx, ship, mission, data)
		}
		return false, r.hold(ctx, ship.Ship.ID, "sourcing inspection waits at most six observed game hours for usable fresh evidence: "+plan.Reason)
	default:
		return false, r.hold(ctx, ship.Ship.ID, "unknown durable relocation phase: "+mission.Phase)
	}
}

func (r *Runner) continueSourcing(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	input, err := r.sourcingInput(ctx, ship)
	if err != nil {
		return false, err
	}
	target, exists := r.contract(data.SourcingHandoff.TargetContractID)
	if !exists {
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	probe := data.Plan
	probe.Economics.GameDays = float64(data.Settlement.GameSeconds) / 86_400
	spent := max(data.SourcingSpentPence, relocationCashCost(data))
	plan := sizeRelocationCargo(input, probe, target, spent)
	if !plan.Accepted {
		if err := r.hold(ctx, ship.Ship.ID, "settled sourcing probe found no fresh positive purchase: "+plan.Reason); err != nil {
			return false, err
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	required := cargoFundingRequired(input, plan)
	for region, reserve := range required {
		if input.Balances[region] < reserve {
			return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
		}
	}
	next, nextData := r.newVoyage(ship, plan)
	nextData.SourcingHandoff = data.SourcingHandoff
	nextData.SourcingSpentPence = spent
	nextData.SourcingGameDays = probe.Economics.GameDays
	return true, r.saveMission(ctx, next, &nextData)
}

func (r *Runner) abortRelocation(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, reason string) (bool, error) {
	if err := r.hold(ctx, ship.Ship.ID, "sourcing relocation cancelled before further spending: "+reason); err != nil {
		return false, err
	}
	if ship.Voyage != nil || ship.Ship.Status == "sailing" {
		return false, nil
	}
	data.SourcingHandoff = nil
	data.Plan.Destination = ship.Ship.CurrentPortID
	mission.Destination = ship.Ship.CurrentPortID
	return r.closeAtDestination(ctx, ship, mission, data)
}
