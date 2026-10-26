package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// https://tradewinds.kibb.io/skill.md §3.12: 750 Crowns, in integer pence.
const expansionAgentHireFeePence int64 = 75_000

type expansionAgent struct {
	PortID  string    `json:"port_id"`
	Region  string    `json:"region"`
	HiredAt time.Time `json:"hired_at"`
}

func expansionAgentYard(state accountState, agents []expansionAgent) (Port, int64) {
	present := make(map[string]bool)
	for _, ship := range state.Ships {
		if ship.Ship.Status != "sunk" {
			if port, exists := findPort(state.Catalog, ship.Ship.CurrentPortID); exists {
				present[port.BankingRegion] = true
			}
		}
	}
	for _, portID := range state.Factors {
		if port, exists := findPort(state.Catalog, portID); exists {
			present[port.BankingRegion] = true
		}
	}
	for _, agent := range agents {
		present[agent.Region] = true
	}
	var best Port
	var bestScore int64
	for _, yard := range state.Catalog.Ports {
		if !yard.HasShipyard || yard.IsCove || yard.Phase > state.Clock.UnlockedPhase || yard.BankingRegion == "" || present[yard.BankingRegion] {
			continue
		}
		var score int64
		for _, contract := range state.Contracts {
			if contract.ContractType != "charter" || contract.Status != "open" || contract.Stale || contract.IssuerCompanyID != "" || contract.RemainingQuantity <= 0 || contract.OriginPortID == yard.ID {
				continue
			}
			origin, exists := findPort(state.Catalog, contract.OriginPortID)
			if !exists || origin.BankingRegion != yard.BankingRegion || !slices.Contains(yard.Produces, contract.CommodityID) && !slices.Contains(yard.Manufactures, contract.CommodityID) {
				continue
			}
			for _, commodity := range state.Catalog.Commodities {
				if commodity.ID != contract.CommodityID || commodity.BasePricePence < 0 {
					continue
				}
				premium := max(0, nominalCharterReward(contract)-commodity.BasePricePence)
				if premium > (math.MaxInt64-score)/contract.RemainingQuantity {
					score = math.MaxInt64
				} else {
					score += premium * contract.RemainingQuantity
				}
				break
			}
		}
		if score > bestScore || score > 0 && score == bestScore && yard.ID < best.ID {
			best, bestScore = yard, score
		}
	}
	return best, bestScore
}

func (r *Runner) considerExpansionAgent(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	for _, dataset := range []string{"catalog", "clock", "ships", "factors", "contracts", "vaults", "rates", "profile"} {
		if !r.fresh(dataset) {
			return false, r.hold(ctx, "", "expansion agent requires current successful "+dataset+" authority")
		}
	}
	mission, _, err := r.loadMission(ctx, "__treasury")
	if err != nil || mission != nil {
		return false, err
	}
	if len(r.stateCache.Factors) >= 12 {
		return false, nil
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		if slices.Contains(r.stateCache.Factors, agent.PortID) {
			return false, nil
		}
	}
	if until := decode[time.Time](r.snapshot, "_expansion_agent_retry_at"); time.Now().Before(until) {
		r.wakeBy(until)
		return false, nil
	}
	yard, score := expansionAgentYard(r.stateCache, agents)
	if score <= 0 {
		return false, nil
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return false, err
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := r.growthCashFunding(ctx, &evidence); err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return false, r.hold(ctx, "", reason)
	}
	evidence.available = 0
	for region, balance := range input.Balances {
		evidence.available = growthCashTotal(evidence.available, max(0, balance-input.ReserveByRegion[region]))
	}
	capacity, reason, err := r.capacityFleetInput(ctx, &evidence)
	if err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return false, r.hold(ctx, "", reason)
	}
	cheapest := int64(math.MaxInt64)
	var estimatedCapital int64
	bestFresh := false
	var observedAt time.Time
	for _, observed := range r.stateCache.Quotes {
		hull, exists := findHull(r.stateCache.Catalog, observed.ShipClass)
		if !exists || hull.HullCategory != "regional" && hull.HullCategory != "oceanic" ||
			hull.CanSealDispatches || hull.Phase > r.stateCache.Clock.UnlockedPhase || hull.MaxCannons < 1 || hull.HoldCapacityTons <= 2*min(4, hull.MaxCannons) {
			continue
		}
		port, exists := findPort(r.stateCache.Catalog, observed.PortID)
		if !exists {
			continue
		}
		if observed.BuildMode != "turnkey_cash" || !observed.Available || observed.TotalCostPence == nil || *observed.TotalCostPence <= 0 {
			continue
		}
		_, quoteErr := r.growthQuote(port, hull)
		fresh := quoteErr == nil
		updatedAt := r.meta["yard_quotes/"+port.ID+"|"+hull.ID].UpdatedAt
		price := *observed.TotalCostPence
		capital := growthCashTotal(price, ceilBPS(price, 1_000), min(4, hull.MaxCannons)*15_000)
		if capacity.LotCashPence > 0 && int64(capacity.FleetSize) >= math.MaxInt64/capacity.LotCashPence {
			continue
		}
		buffer := growthCashTotal(max(capacity.ReservePence, ceilBPS(price, 1_500)), (int64(capacity.FleetSize)+1)*capacity.LotCashPence)
		needed := growthCashTotal(capital, buffer, expansionAgentHireFeePence)
		if fresh && (!bestFresh || needed < cheapest) ||
			!fresh && !bestFresh && (cheapest == math.MaxInt64 || updatedAt.After(observedAt) || updatedAt.Equal(observedAt) && needed < cheapest) {
			cheapest, estimatedCapital, bestFresh, observedAt = needed, capital, fresh, updatedAt
		}
	}
	if cheapest == math.MaxInt64 {
		return false, r.hold(ctx, "", "expansion agent needs a previously observed regional hull quote to estimate overseas capital")
	}
	if err := r.decision(ctx, "", "expansion_agent", "estimate overseas capital from observed quotes before exploring the largest unoccupied Crown charter premium pool", map[string]any{
		"port_id": yard.ID, "region": yard.BankingRegion, "score_pence": score, "estimated_hull_capital_pence": estimatedCapital,
		"estimated_hull_capital_stale": !bestFresh, "estimated_hull_quote_observed_at": observedAt,
	}); err != nil {
		return false, err
	}
	if evidence.available < cheapest {
		return false, r.hold(ctx, "", "expansion agent requires surplus for estimated regional hull capital, capacity cash buffer and hire fee")
	}
	required := input.ReserveByRegion
	baseRequired, err := r.treasuryReserves(ctx)
	if err != nil {
		return false, err
	}
	for region, protected := range required {
		required[region] = max(baseRequired[region], min(protected, input.Balances[region]))
	}
	required[yard.BankingRegion] = growthCashTotal(max(required[yard.BankingRegion], r.opts.ReservePence), expansionAgentHireFeePence)
	_, fees, _, funded := measuredFunding(input.Balances, required, input.Rates)
	if !funded || evidence.available < growthCashTotal(cheapest, fees) ||
		estimatedCapital > (evidence.available-growthCashTotal(expansionAgentHireFeePence, fees))/capacityFleetFillShare {
		return false, r.hold(ctx, "", "expansion agent must preserve the hull's half-surplus capital limit, capacity working cash and funding fees")
	}
	if changed, err := r.fundTreasury(ctx, input.Balances, required); changed || err != nil {
		return changed, err
	}
	for region, amount := range required {
		if input.Balances[region] < amount {
			return false, r.hold(ctx, "", "expansion agent waits for completed regional fee and reserve funding")
		}
	}
	mission = &store.Mission{
		AccountID: r.account.ID, ShipID: "__treasury", Phase: "hire-factor",
		Destination: yard.ID, Generation: uuid.NewString(),
	}
	data := missionData{Plan: Plan{Kind: "treasury"}, StartedAt: time.Now().UTC(), CostsKnown: true}
	return true, r.stage(ctx, mission, &data, ShipState{}, "hire-factor", "/v1/company/hire-factor", map[string]any{"port_id": yard.ID})
}

func (r *Runner) manageExpansionAgents(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	if len(agents) == 0 {
		return false, nil
	}
	if !r.fresh("factors") {
		return false, r.hold(ctx, "", "expansion agent dismissal requires current factors")
	}
	mission, _, err := r.loadMission(ctx, "__treasury")
	if err != nil || mission != nil {
		return false, err
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, agent := range agents {
		if !slices.Contains(r.stateCache.Factors, agent.PortID) {
			continue
		}
		dismissAt := agent.HiredAt.Add(2 * time.Hour)
		if now.Before(dismissAt) {
			r.wakeBy(dismissAt)
			continue
		}
		if decode[string](r.snapshot, "_capacity_growth_port") == agent.PortID {
			continue
		}
		needed := false
		for _, active := range missions {
			switch active.Phase {
			case "done", "settled", "maintenance_done", "growth_rejected":
				continue
			}
			var intent missionData
			if err := json.Unmarshal(active.Data, &intent); err != nil {
				return false, err
			}
			shipID := active.ShipID
			if shipID == "__growth" {
				shipID = intent.BuiltShipID
			}
			if ship, exists := r.ship(shipID); exists && ship.Voyage == nil && ship.Ship.CurrentPortID == agent.PortID &&
				(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") {
				continue
			}
			if intent.Plan.Origin == agent.PortID || intent.Plan.Destination == agent.PortID || active.Destination == agent.PortID ||
				intent.BuildPort == agent.PortID || intent.Capacity != nil && intent.Capacity.BuildPort == agent.PortID {
				needed = true
				break
			}
		}
		if needed {
			continue
		}
		mission = &store.Mission{
			AccountID: r.account.ID, ShipID: "__treasury", Phase: "dismiss-factor",
			Destination: agent.PortID, Generation: uuid.NewString(),
		}
		control := missionData{Plan: Plan{Kind: "treasury"}, StartedAt: now, CostsKnown: true}
		return true, r.stage(ctx, mission, &control, ShipState{}, "dismiss-factor", "/v1/company/dismiss-factor", map[string]any{"port_id": agent.PortID})
	}
	return false, nil
}

func (r *Runner) recordExpansionAgent(ctx context.Context, data *missionData) error {
	var request struct {
		PortID string `json:"port_id"`
	}
	if err := json.Unmarshal(data.Action.Body, &request); err != nil {
		return err
	}
	port, exists := findPort(r.stateCache.Catalog, request.PortID)
	if !exists || port.BankingRegion == "" {
		return fmt.Errorf("expansion agent receipt lacks its catalog banking region for %s", request.PortID)
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		if agent.Region == port.BankingRegion {
			return nil
		}
	}
	agents = append(agents, expansionAgent{PortID: port.ID, Region: port.BankingRegion, HiredAt: time.Now().UTC()})
	r.snapshot["_expansion_agents"] = encode(agents)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}
