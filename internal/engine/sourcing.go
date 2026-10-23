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

func sourcingAllowance(in PlanningInput) int64 {
	var cash int64
	for _, balance := range in.Balances {
		cash += balance
	}
	for _, reserve := range in.ReserveByRegion {
		cash -= reserve
	}
	return min(int64(20_000), max(0, cash)/50)
}

func publicSourceEvidence(in PlanningInput, source Port, contract Contract) (bool, string) {
	production := slices.Contains(source.Produces, contract.CommodityID) || slices.Contains(source.Manufactures, contract.CommodityID)
	_, upper, ok := in.serverBounds()
	if !ok || in.Clock.Calendar.TimeScale <= 0 {
		return false, "source evidence requires current server clock authority"
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
			return false, "fresh source ask hint leaves no margin below the charter reward"
		}
		return true, sourcingAskEvidence
	}
	if production {
		return true, "catalog production/manufacture; executable price and quantity remain unknown"
	}
	return false, "source lacks production/manufacture or fresh six-game-hour ask gossip supply evidence"
}

// This public shortlist is also used during capture. Its ports never grant
// private book access; only actual ship or factor presence does that.
func sourcingPairs(in PlanningInput) []Plan {
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
			ok, reason := publicSourceEvidence(in, source, contract)
			plan.PriceSource = reason
			plan.Accepted = ok
			plan.Reason = reason
			if plan.Accepted {
				plan = relocationExposure(in, plan)
				if plan.Accepted {
					boundedInput := in
					boundedInput.ReserveByRegion = sourcingReserves(in, source)
					allowance := sourcingAllowance(boundedInput)
					if -plan.Economics.ExpectedNetPence > allowance {
						plan = reject(plan, fmt.Sprintf("sourcing modeled exposure %dp exceeds allowance/cap %dp", -plan.Economics.ExpectedNetPence, allowance))
					}
				}
			}
			pairs = append(pairs, plan)
		}
	}
	if len(pairs) == 0 {
		return []Plan{reject(Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID}, "no live charter/source opportunity for a bounded relocation")}
	}
	// Cost the empty outward/return contingency before fetching only the
	// shortlisted source-to-charter routes. No remote executable price is used.
	sort.SliceStable(pairs, func(i, j int) bool {
		a, b := pairs[i], pairs[j]
		if a.Accepted != b.Accepted {
			return a.Accepted
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
	count := 0
	for i := range pairs {
		if !pairs[i].Accepted {
			continue
		}
		count++
		if count > 3 {
			pairs[i] = reject(pairs[i], "outside the bounded three-pair public sourcing shortlist")
		}
	}
	return pairs
}

func planSourcingRelocation(in PlanningInput) []Plan {
	base := Plan{Kind: "relocation", ShipID: in.Ship.Ship.ID}
	ship := in.Ship
	if ship.Ship.ConvoyID != "" {
		return []Plan{reject(base, "sourcing cannot divert a convoy-bound ship")}
	}
	if ship.Voyage != nil || ship.Ship.CurrentPortID == "" || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
		return []Plan{reject(base, "sourcing requires a stationary independently navigable ship in port")}
	}
	if !emptyCargo(ship) {
		return []Plan{reject(base, "sourcing requires an empty cargo hold")}
	}
	for _, contract := range in.Contracts {
		if contract.ContractType == "freight" && contract.Status == "active" && contract.CarrierShipID == ship.Ship.ID {
			return []Plan{reject(base, "active sealed freight obligations prohibit sourcing relocation")}
		}
	}
	for _, dataset := range []string{"ships", "catalog", "clock", "zones", "contracts", "vaults"} {
		if !in.fresh(dataset) {
			return []Plan{reject(base, "sourcing requires fresh "+dataset)}
		}
	}
	if in.Clock.Paused || in.Clock.Calendar.TimeScale <= 0 {
		return []Plan{reject(base, "sourcing requires an unpaused authoritative game clock")}
	}
	ordinary := PlanVoyages(in)
	if len(ordinary) > 0 && ordinary[0].Accepted {
		return []Plan{reject(base, "an ordinary measured opportunity takes priority over sourcing")}
	}
	plans := sourcingPairs(in)
	for i := range plans {
		if plans[i].Accepted {
			plans[i] = validateRelocationTarget(in, plans[i])
		}
	}
	sort.SliceStable(plans, func(i, j int) bool {
		a, b := plans[i], plans[j]
		if a.Accepted != b.Accepted {
			return a.Accepted
		}
		if a.PriceSource != b.PriceSource {
			aGossip := a.PriceSource == sourcingAskEvidence
			bGossip := b.PriceSource == sourcingAskEvidence
			if aGossip != bGossip {
				return aGossip
			}
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
	return plans
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
	plan.TargetStoresTons = outward.SafeProvisionsTons + back.SafeProvisionsTons + float64(hull.CrewRequired)/200*portDays + 1
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
	fill, err := WalkDepth(provisions.Depth.Asks, max(1, int64(math.Ceil(consumed))), true)
	if err != nil {
		return reject(plan, "sourcing consumed-store replacement: "+err.Error())
	}
	storeExpense := max(purchaseCost, int64(math.Ceil(float64(fill.GrossPence+ceilBPS(fill.GrossPence, book.CustomsBPS))*consumed/float64(fill.Quantity))))
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
	plan.Economics = Economics{ProvisionsPence: storeExpense, BerthDuesPence: berth, UpkeepPence: upkeep, ExpectedNetPence: -exposure, StressNetPence: -exposure, GameDays: gameDays, NetPencePerGameDay: -float64(exposure) / gameDays}
	plan.Accepted = true
	plan.Reason = "bounded cost-only relocation; public supply evidence is not executable stock, price or revenue"
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
		if !candidate.Accepted && (candidate.Economics.GameDays <= 0 ||
			candidate.Reason != "local vault cannot fund trade, safe stores, round-trip bills and protected reserve" &&
				candidate.Reason != "destination region lacks its protected reserve and harbour bill") {
			if !best.Accepted && quantity == 1 {
				best.Reason = candidate.Reason
			}
			continue
		}
		required := cargoFundingRequired(in, candidate)
		balances, fundingFee, transfers, funded := measuredFunding(in.Balances, required, in.Rates)
		if !funded || len(transfers) > 0 && !in.fresh("rates") {
			if !best.Accepted {
				best.Reason = "targeted cargo deficit cannot be funded with observed fees and protected regional reserves"
			}
			continue
		}
		if !candidate.Accepted {
			fundedInput := in
			fundedInput.Balances = balances
			candidate = evaluatePlan(fundedInput, hull, origin, candidate, quantity**charter.RewardPerTonPence, 0)
			if !candidate.Accepted {
				if !best.Accepted {
					best.Reason = candidate.Reason
				}
				continue
			}
		}
		extraProvisions := max(0, in.PaidProvisionsPence-candidate.Economics.ProvisionsPence)
		candidate.Economics.ProvisionsPence += extraProvisions
		candidate.Economics.ExpectedNetPence -= spentCostPence + fundingFee + extraProvisions
		candidate.Economics.StressNetPence -= spentCostPence + fundingFee + extraProvisions
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

func cargoFundingRequired(in PlanningInput, plan Plan) map[string]int64 {
	required := clonePence(in.ReserveByRegion)
	origin, _ := findPort(in.Catalog, plan.Origin)
	destination, _ := findPort(in.Catalog, plan.Destination)
	purchase := plan.Quantity * plan.BuyLimitPence
	cost := purchase + ceilBPS(purchase, in.Markets[plan.Origin].CustomsBPS) + plan.Quantity
	stores := plan.StoresToBuy * plan.StoresLimitPence
	cost += stores + ceilBPS(stores, in.Markets[plan.Origin].CustomsBPS) + plan.StoresToBuy
	cost += plan.Economics.BerthDuesPence + plan.Economics.UpkeepPence + plan.Economics.CollateralPence + plan.Economics.TollPence
	required[origin.BankingRegion] += cost
	if destination.BankingRegion != origin.BankingRegion {
		required[destination.BankingRegion] = max(required[destination.BankingRegion], in.ReservePence+3*in.Ports[plan.Destination].BerthDuesPerDayPence)
	}
	return required
}

func (r *Runner) sourcingInput(ctx context.Context, ship ShipState) (PlanningInput, error) {
	input := r.planningInput(ship)
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return input, err
	}
	for region, reserve := range input.ReserveByRegion {
		required[region] = max(required[region], reserve)
	}
	for _, incumbent := range r.stateCache.Ships {
		mission, data, err := r.loadMission(ctx, incumbent.Ship.ID)
		if err != nil {
			return input, err
		}
		if mission == nil || incumbent.Ship.ID == ship.Ship.ID || data.Settlement != nil {
			continue
		}
		if data.Plan.Kind != "maintenance" && data.Plan.Kind != "recovery" {
			if origin, exists := findPort(input.Catalog, data.Plan.Origin); exists {
				required[origin.BankingRegion] += growthWorkingCapital(data.Plan)
			}
		}
	}
	input.ReserveByRegion = required
	return input, nil
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
	if len(plans) == 0 || !plans[0].Accepted || !r.opts.Execute {
		return false, nil
	}
	plan := plans[0]
	source, _ := findPort(input.Catalog, plan.Destination)
	input.ReserveByRegion = sourcingReserves(input, source)
	restart, err := r.db.LatestRelocationRestart(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	if restart.Latest != nil {
		latest := restart.Latest
		previousNet := latest.ProceedsPence - latest.AcquisitionPence - latest.FeesPence - latest.ProvisionsPence - latest.ExpensesPence
		if !latest.CostsKnown || restart.UnknownCostSince || !restart.PositiveDeliverySince || restart.NetSincePence+previousNet < -plan.Economics.ExpectedNetPence {
			return false, r.hold(ctx, ship.Ship.ID, "sourcing retry requires all known settled earnings since the last relocation, including its loss and later losses, plus a positive delivered shipment sufficient to fund this exposure")
		}
	}
	contract, exists := r.contract(plan.ContractID)
	if !exists {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing target contract disappeared before durable intent")
	}
	mission, data := r.newVoyage(ship, plan)
	mission.Phase = "sourcing_fund"
	data.Relocation = &relocationIntent{
		TargetContractID:    contract.ID,
		TargetCommodity:     contract.CommodityID,
		TargetDestination:   contract.OriginPortID,
		ExpenseCapPence:     sourcingAllowance(input),
		ModeledExpensePence: -plan.Economics.ExpectedNetPence,
	}
	data.ProvenLane = plan.Destination + "|" + contract.OriginPortID + "|" + contract.CommodityID + "|charter"
	return true, r.saveMission(ctx, mission, &data)
}

func relocationCashCost(data *missionData) int64 {
	return data.AcquisitionPence + data.FeesPence + data.ProvisionsPence + data.ExpensesPence
}

func (r *Runner) relocationBudget(data *missionData, extra int64) bool {
	if data.Relocation == nil || data.Relocation.ExpenseCapPence <= 0 || extra < 0 {
		return false
	}
	bills, _ := r.unclaimedBillsSince(data)
	return relocationCashCost(data)+bills+extra <= data.Relocation.ExpenseCapPence
}

func (r *Runner) stageSourcingFunding(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, input PlanningInput, required map[string]int64) (bool, error) {
	_, fees, transfers, ok := measuredFunding(input.Balances, required, input.Rates)
	if !ok {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing funds cannot be placed with observed fees while preserving every donor reserve and incumbent commitment")
	}
	if !r.relocationBudget(data, fees) {
		return false, r.hold(ctx, ship.Ship.ID, "sourcing remittance fees would exceed the durable exploration expense cap")
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
	bills, _ := r.unclaimedBillsSince(data)
	futureFees := max(0, -plan.Economics.ExpectedNetPence-plan.Economics.ProvisionsPence-plan.Economics.BerthDuesPence-plan.Economics.UpkeepPence)
	exposure := data.FeesPence + max(data.ProvisionsPence, plan.Economics.ProvisionsPence) +
		max(data.ExpensesPence+bills, plan.Economics.BerthDuesPence+plan.Economics.UpkeepPence) + futureFees
	if exposure > data.Relocation.ExpenseCapPence {
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
		required := clonePence(input.ReserveByRegion)
		required[source.BankingRegion] = max(required[source.BankingRegion], r.opts.ReservePence+3*input.Ports[source.ID].BerthDuesPerDayPence)
		required[origin.BankingRegion] += max(0, data.Relocation.ModeledExpensePence-relocationCashCost(data))
		if changed, err := r.stageSourcingFunding(ctx, ship, mission, data, input, required); changed || err != nil {
			return changed, err
		}
		if _, fees, transfers, funded := measuredFunding(input.Balances, required, input.Rates); !funded || len(transfers) > 0 || !r.relocationBudget(data, fees) {
			return r.abortRelocation(ctx, ship, mission, data, "fresh sourcing reserve funding or its expense allowance is no longer available")
		}
		data.Relocation.FundingRequiredPence = 0
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
		spent := max(data.Relocation.ModeledExpensePence, relocationCashCost(data))
		plan := sizeRelocationCargo(input, data.Plan, contract, spent)
		if plan.Accepted {
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
