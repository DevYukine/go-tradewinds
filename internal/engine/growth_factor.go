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

const (
	expansionAgentAdmissionReason = "expansion agent waits for an admissible build"
	expansionAgentQuoteReason     = "expansion agent needs a previously observed regional hull quote"
)

type expansionAgent struct {
	PortID  string    `json:"port_id"`
	Region  string    `json:"region"`
	HiredAt time.Time `json:"hired_at"`
}

// expansionRegionRetry bounds how often one region can cost a hire fee, while
// still letting a region be explored again once a build there is affordable.
const expansionRegionRetry = 24 * time.Hour

func expansionAgentYard(state accountState, input PlanningInput, agents []expansionAgent, now time.Time) (Port, int64) {
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
		if now.Before(agent.HiredAt.Add(expansionRegionRetry)) {
			present[agent.Region] = true
		}
	}
	var best Port
	var bestScore int64
	type yardCandidate struct {
		port        Port
		score       int64
		competition CompetitionEvidence
	}
	var candidates []yardCandidate
	var economicBest int64
	for _, yard := range state.Catalog.Ports {
		if !yard.HasShipyard || yard.IsCove || yard.Phase > state.Clock.UnlockedPhase || yard.BankingRegion == "" || present[yard.BankingRegion] ||
			!regionalWater(state.Catalog, yard) {
			continue
		}
		var score int64
		for _, commodity := range state.Catalog.Commodities {
			// Exploration needs observed spot supply, not finite charter income
			// or catalog production with an unknown executable purchase price.
			_, _, ask := publicSourceEvidence(input, yard, Contract{CommodityID: commodity.ID})
			if ask <= 0 {
				continue
			}
			for _, destination := range state.Catalog.Ports {
				if destination.ID == yard.ID || destination.IsCove || destination.Phase > state.Clock.UnlockedPhase ||
					destination.BankingRegion != yard.BankingRegion || !regionalWater(state.Catalog, destination) {
					continue
				}
				revenue, _, stress, _, reason := destinationRevenue(input, destination.ID, commodity.ID, 1)
				if reason == "" {
					// A conservative one-ton spread ranks exploration only; the
					// measured capacity planner still admits the eventual hull.
					score = max(score, revenue-stress-ask)
				}
			}
		}
		if score <= 0 {
			continue
		}
		economicBest = max(economicBest, score)
		candidates = append(candidates, yardCandidate{yard, score, competitionAt(input.Competition, yard.BankingRegion)})
	}
	var bestCompetition CompetitionEvidence
	for _, candidate := range candidates {
		comparable := economicallyComparable(float64(candidate.score), float64(economicBest))
		bestComparable := economicallyComparable(float64(bestScore), float64(economicBest))
		better := candidate.score > bestScore || candidate.score == bestScore && candidate.port.ID < best.ID
		if comparable && bestComparable && candidate.competition.PreferenceShips != bestCompetition.PreferenceShips {
			better = candidate.competition.PreferenceShips < bestCompetition.PreferenceShips
		}
		if best.ID == "" || better {
			best, bestScore, bestCompetition = candidate.port, candidate.score, candidate.competition
		}
	}
	return best, bestScore
}

func regionalWater(catalog Catalog, port Port) bool {
	for _, zone := range catalog.SeaZones {
		if zone.ID == port.SeaZoneID {
			return classRank(zone.ZoneClass) > 0 && classRank(zone.ZoneClass) <= classRank("regional")
		}
	}
	return false
}

func (r *Runner) expansionBuildEstimate(yard Port, capacity CapacityGrowthInput) (GrowthDecision, YardQuote, bool) {
	capacity.SurplusPence -= expansionAgentHireFeePence
	capacity.CandidateRatePencePerGameDay = capacity.MeasuredRatePencePerGameDay
	capacity.HasSpareWork = true
	var oldUpkeep int64
	var grown []ShipState
	if capacity.MeasuredNetAvailable {
		oldUpkeep = DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
		grown = make([]ShipState, len(r.stateCache.Ships)+1)
		copy(grown, r.stateCache.Ships)
	}
	var best GrowthDecision
	var bestQuote YardQuote
	bestFresh := false
	var observedAt time.Time
	needed := int64(math.MaxInt64)
	hasObservedQuote := false
	for _, quote := range r.stateCache.Quotes {
		hull, exists := findHull(r.stateCache.Catalog, quote.ShipClass)
		if !exists || hull.HullCategory != "regional" && hull.HullCategory != "oceanic" ||
			hull.CanSealDispatches || hull.Phase > r.stateCache.Clock.UnlockedPhase ||
			quote.BuildMode != "turnkey_cash" || !quote.Available || quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 {
			continue
		}
		source, exists := findPort(r.stateCache.Catalog, quote.PortID)
		if !exists {
			continue
		}
		hasObservedQuote = true
		if !capacity.MeasuredNetAvailable {
			continue
		}
		grown[len(grown)-1] = growthCandidate(yard, hull)
		capacity.Hull, capacity.HullPricePence = hull, *quote.TotalCostPence
		capacity.IncrementalDailyUpkeepPence = DailyUpkeep(grown, r.stateCache.Catalog, len(r.stateCache.Factors)+1) - oldUpkeep
		decision := EvaluateCapacityGrowth(capacity)
		if !decision.Accepted {
			continue
		}
		_, quoteErr := r.growthQuote(source, hull)
		fresh := quoteErr == nil
		updatedAt := r.meta["yard_quotes/"+source.ID+"|"+hull.ID].UpdatedAt
		capital := growthCapital(decision)
		total := growthCashTotal(capital, decision.WorkingCapitalPence, expansionAgentHireFeePence)
		if bestQuote.PortID == "" || fresh && (!bestFresh || total < needed) ||
			!fresh && !bestFresh && (updatedAt.After(observedAt) || updatedAt.Equal(observedAt) && total < needed) {
			best, bestQuote, bestFresh, observedAt, needed = decision, quote, fresh, updatedAt, total
		}
	}
	if bestQuote.PortID == "" {
		best.Reason = expansionAgentAdmissionReason
		if !hasObservedQuote {
			best.Reason = expansionAgentQuoteReason
		}
	}
	return best, bestQuote, bestFresh
}

func (r *Runner) considerExpansionAgent(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return false, err
	}
	if target.PortID != "" {
		return false, r.holdExpected(ctx, "", capacitySavingsReason)
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
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return false, err
	}
	yard, score := expansionAgentYard(r.stateCache, input, agents, time.Now())
	if score <= 0 {
		return false, nil
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	if reason, err := r.growthCashFunding(ctx, &evidence); err != nil || reason != "" {
		if err != nil {
			return false, err
		}
		return false, r.recordHold(ctx, "", reason, expectedGrowthAdmission(reason))
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
		return false, r.recordHold(ctx, "", reason, expectedGrowthAdmission(reason))
	}
	build, observed, bestFresh := r.expansionBuildEstimate(yard, capacity)
	if !build.Accepted {
		return false, r.holdExpected(ctx, "", build.Reason)
	}
	estimatedCapital := growthCapital(build)
	cheapest := growthCashTotal(estimatedCapital, build.WorkingCapitalPence, expansionAgentHireFeePence)
	if err := r.decision(ctx, "", "capacity_candidate:"+yard.ID+"|"+observed.ShipClass,
		"measured earnings admit the estimated regional build before hiring its expansion agent", map[string]any{
			"accepted": true, "estimated_quote_port": observed.PortID, "estimated_hull_capital_stale": !bestFresh,
			"measured_rate_pence_per_game_day": capacity.MeasuredRatePencePerGameDay,
			"used_rate_pence_per_game_day":     capacity.MeasuredRatePencePerGameDay,
			"sample_hull_game_days":            capacity.SampleHullGameDays, "decision": build,
		}); err != nil {
		return false, err
	}
	if err := r.decision(ctx, "", "expansion_agent", "an admissible measured build supports the paid expansion agent", map[string]any{
		"port_id": yard.ID, "region": yard.BankingRegion, "score_pence": score, "estimated_hull_capital_pence": estimatedCapital,
		"estimated_hull_capital_stale": !bestFresh, "competition": competitionAt(input.Competition, yard.BankingRegion),
	}); err != nil {
		return false, err
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
	if !funded || evidence.available < growthCashTotal(cheapest, fees) {
		return false, r.holdExpected(ctx, "", "expansion agent must preserve the hull capital, capacity working cash and funding fees")
	}
	if changed, err := r.fundTreasury(ctx, input.Balances, required); changed || err != nil {
		return changed, err
	}
	for region, amount := range required {
		if input.Balances[region] < amount {
			return false, r.holdExpected(ctx, "", "expansion agent waits for completed regional fee and reserve funding")
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
	savingsTarget, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
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
		if decode[string](r.snapshot, "_capacity_growth_port") == agent.PortID || savingsTarget.PortID == agent.PortID {
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
