package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// https://tradewinds.kibb.io/skill.md §3.12: 750 Crowns, in integer pence.
const expansionAgentHireFeePence int64 = 75_000

const (
	expansionAgentAdmissionReason = "expansion agent waits for an admissible build"
	expansionAgentQuoteReason     = "expansion agent needs a previously observed regional hull quote"
	expansionProbeReason          = "overseas probe: no gossip spread, hiring factors for quay evidence"
	expansionEstimateProbeReason  = "overseas probe: estimated build rejected, hiring factors for quay evidence"
)

type expansionAgent struct {
	PortID      string    `json:"port_id"`
	Partner     string    `json:"partner_port_id"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"created_at"`
	HiredAt     time.Time `json:"hired_at"`
	RetryAt     time.Time `json:"retry_at,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at,omitempty"`
	Probe       bool      `json:"probe,omitempty"`
}

type expansionAdmissionError struct {
	reason    string
	retryOnly bool
}

func (err *expansionAdmissionError) Error() string {
	return err.reason
}

type expansionPrice struct {
	Bid   int64 `json:"bid"`
	Ask   int64 `json:"ask"`
	bidAt time.Time
	askAt time.Time
}

type expansionOutcome struct {
	AttemptAt            time.Time `json:"attempt_at"`
	RetryAt              time.Time `json:"retry_at"`
	Losses               int       `json:"losses"`
	Reason               string    `json:"reason"`
	UnevaluatedAttempts  int       `json:"unevaluated_attempts,omitempty"`
	UnevaluatedAttemptAt time.Time `json:"unevaluated_attempt_at,omitempty"`
	UnevaluatedRetryAt   time.Time `json:"unevaluated_retry_at,omitempty"`
	CapacityRetryAt      time.Time `json:"capacity_retry_at,omitempty"`
	CapacityReason       string    `json:"capacity_reason,omitempty"`
}

// Regional and global exploration starts are separated by at least 24 real hours.
const expansionRegionRetry = 24 * time.Hour

const expansionObservationRetry = 5 * time.Minute

// A pair that expired before either factor was hired explored nothing, so it
// retries after its window plus this pause instead of the 24-hour start floor.
const expansionUnhiredRetry = time.Hour

// Historical gossip ranks paid observation only; timestamps do not turn it
// into executable depth or reset a losing region's economic backoff.
func expansionPrices(in PlanningInput) map[string]expansionPrice {
	prices := make(map[string]expansionPrice)
	_, upper, ok := in.serverBounds()
	if !ok || in.Clock.Calendar.TimeScale <= 0 {
		return prices
	}
	for boardID, board := range in.Ports {
		if !in.fresh("ports/" + boardID) {
			continue
		}
		for _, report := range board.CoffeeHouseGossip {
			if report.SnapshotAt.IsZero() || report.SnapshotAt.After(upper) {
				continue
			}
			for _, quote := range report.Commodities {
				if quote.SnapshotAt.IsZero() || quote.SnapshotAt.After(upper) {
					continue
				}
				at := report.SnapshotAt
				if quote.SnapshotAt.Before(at) {
					at = quote.SnapshotAt
				}
				age := upper.Sub(at).Seconds() * float64(in.Clock.Calendar.TimeScale) / 86_400
				if age < 0 || age > 30 {
					continue
				}
				key := report.SubjectPortID + "|" + quote.CommodityID
				price := prices[key]
				if bid := quote.BestBidPence; bid != nil && *bid > 0 &&
					(price.Bid == 0 || at.After(price.bidAt) || at.Equal(price.bidAt) && *bid < price.Bid) {
					price.Bid, price.bidAt = *bid, at
				}
				if ask := quote.BestAskPence; ask != nil && *ask > 0 &&
					(price.Ask == 0 || at.After(price.askAt) || at.Equal(price.askAt) && *ask > price.Ask) {
					price.Ask, price.askAt = *ask, at
				}
				prices[key] = price
			}
		}
	}
	return prices
}

type expansionPair struct {
	yard        Port
	partner     Port
	score       int64
	competition CompetitionEvidence
}

func expansionAgentYards(state AccountState, input PlanningInput) []expansionPair {
	prices := expansionPrices(input)
	present := expansionPresentRegions(state)
	var candidates []expansionPair
	for _, yard := range state.Catalog.Ports {
		if !yard.HasShipyard || yard.IsCove || yard.Phase < 2 || yard.Phase > state.Clock.UnlockedPhase || yard.BankingRegion == "" || present[yard.BankingRegion] ||
			!regionalWater(state.Catalog, yard) {
			continue
		}
		var score int64
		var partner Port
		for _, destination := range state.Catalog.Ports {
			if destination.ID == yard.ID || destination.IsCove || destination.Phase > state.Clock.UnlockedPhase ||
				destination.BankingRegion != yard.BankingRegion || !regionalWater(state.Catalog, destination) {
				continue
			}
			var outbound, inbound int64
			observedOutbound := false
			for _, commodity := range state.Catalog.Commodities {
				ask := prices[yard.ID+"|"+commodity.ID].Ask
				bid := prices[destination.ID+"|"+commodity.ID].Bid
				if ask > 0 && bid > 0 {
					spread := bid - ask
					if !observedOutbound || spread > outbound {
						outbound = spread
					}
					observedOutbound = true
				}
				ask = prices[destination.ID+"|"+commodity.ID].Ask
				bid = prices[yard.ID+"|"+commodity.ID].Bid
				if ask > 0 && bid > ask {
					inbound = max(inbound, bid-ask)
				}
			}
			if !observedOutbound {
				continue
			}
			pairScore := inbound + outbound
			if outbound >= 0 {
				pairScore = growthCashTotal(outbound, inbound)
			}
			if pairScore > score || pairScore == score && destination.ID < partner.ID {
				score, partner = pairScore, destination
			}
		}
		if score <= 0 {
			continue
		}
		candidates = append(candidates, expansionPair{yard, partner, score, competitionAt(input.Competition, yard.BankingRegion)})
	}
	sortExpansionPairs(candidates)
	return candidates
}

func sortExpansionPairs(candidates []expansionPair) {
	var economicBest int64
	for _, pair := range candidates {
		economicBest = max(economicBest, pair.score)
	}
	slices.SortFunc(candidates, func(a, b expansionPair) int {
		aComparable := economicallyComparable(float64(a.score), float64(economicBest))
		bComparable := economicallyComparable(float64(b.score), float64(economicBest))
		if aComparable != bComparable {
			if aComparable {
				return -1
			}
			return 1
		}
		if aComparable && a.competition.PreferenceShips != b.competition.PreferenceShips {
			if a.competition.PreferenceShips < b.competition.PreferenceShips {
				return -1
			}
			return 1
		}
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.yard.ID, b.yard.ID)
	})
}
func expansionPresentRegions(state AccountState) map[string]bool {
	present := make(map[string]bool)
	for _, ship := range state.Ships {
		for _, portID := range shipRegionPorts(ship) {
			if port, exists := findPort(state.Catalog, portID); exists {
				present[port.BankingRegion] = true
			}
		}
	}
	for _, portID := range state.Factors {
		if port, exists := findPort(state.Catalog, portID); exists {
			present[port.BankingRegion] = true
		}
	}
	return present
}

// Probe ranking deliberately does not infer profitability from missing prices.
// These pairs buy quay evidence; the ordinary capacity evaluator buys hulls.
func expansionProbeYards(state AccountState, input PlanningInput, hiring bool) []expansionPair {
	prices := expansionPrices(input)
	observations := make(map[string]int)
	for key, price := range prices {
		portID, _, _ := strings.Cut(key, "|")
		if price.Bid > 0 {
			observations[portID]++
		}
		if price.Ask > 0 {
			observations[portID]++
		}
	}
	lowerHazard := func(a, b Port) bool {
		aRisk, aKnown := expansionArrivalRisk(input, a)
		bRisk, bKnown := expansionArrivalRisk(input, b)
		if aKnown != bKnown {
			return aKnown
		}
		if aRisk != bRisk {
			return aRisk < bRisk
		}
		return a.ID < b.ID
	}
	var pairs []expansionPair
	for _, yard := range state.Catalog.Ports {
		if !yard.HasShipyard || yard.IsCove || yard.Phase < 2 || yard.Phase > state.Clock.UnlockedPhase ||
			yard.BankingRegion == "" || !regionalWater(state.Catalog, yard) {
			continue
		}
		var partner Port
		for _, port := range state.Catalog.Ports {
			if port.ID == yard.ID || port.IsCove || port.Phase > state.Clock.UnlockedPhase ||
				port.BankingRegion != yard.BankingRegion || !regionalWater(state.Catalog, port) ||
				hiring && slices.Contains(state.Factors, port.ID) {
				continue
			}
			if partner.ID == "" || observations[port.ID] > observations[partner.ID] ||
				observations[port.ID] == observations[partner.ID] && lowerHazard(port, partner) {
				partner = port
			}
		}
		if partner.ID != "" {
			pairs = append(pairs, expansionPair{
				yard: yard, partner: partner, competition: competitionAt(input.Competition, yard.BankingRegion),
			})
		}
	}
	slices.SortFunc(pairs, func(a, b expansionPair) int {
		if (a.yard.YardDiscount != nil) != (b.yard.YardDiscount != nil) {
			if a.yard.YardDiscount != nil {
				return -1
			}
			return 1
		}
		if lowerHazard(a.yard, b.yard) {
			return -1
		}
		if lowerHazard(b.yard, a.yard) {
			return 1
		}
		return 0
	})
	return pairs
}

func expansionArrivalRisk(input PlanningInput, port Port) (int64, bool) {
	_, _, highest, valid := hazardZoneRisk(input, Route{Legs: []RouteLeg{{ToNode: port.ID}}}, nil)
	return highest, valid
}

// Only measured hazard and catalog hull ratings can reject an unexplored
// region. Missing calibration or zone authority is a wait, not exhaustion.
func expansionPairSafety(input PlanningInput, yard, partner Port) (string, bool) {
	rank := 0
	for _, port := range []Port{yard, partner} {
		for _, zone := range input.Catalog.SeaZones {
			if zone.ID == port.SeaZoneID {
				rank = max(rank, classRank(zone.ZoneClass))
			}
		}
	}
	rated, uncalibrated := false, false
	for _, hull := range input.Catalog.ShipClasses {
		if !capacityHull(hull) || hull.CanSealDispatches || hull.Phase > input.Clock.UnlockedPhase ||
			rank <= 0 || classRank(hull.HullCategory) < rank {
			continue
		}
		rated = true
		if !input.fresh("zones") {
			return "expansion pair requires current successful zones authority", false
		}
		calibration := calibrateZoneHazard(input, hull, min(4, hull.MaxCannons))
		if calibration == nil {
			uncalibrated = true
			continue
		}
		safe := true
		for _, port := range []Port{yard, partner} {
			risk, valid := expansionArrivalRisk(input, port)
			if !valid {
				return "expansion pair requires current nonnegative arrival-zone risk at both ports", false
			}
			safe = safe && float64(risk) <= 1.5*float64(calibration.highestRisk)
		}
		if safe {
			return "", false
		}
	}
	if !rated {
		return "expansion pair has no unlocked capacity hull rated for both ports", true
	}
	if uncalibrated {
		return "expansion agent requires calibrated hazard and net worth no older than 6 real hours", false
	}
	return "expansion pair arrival-zone risk exceeds 1.5x calibrated maximum", true
}

func regionalWater(catalog Catalog, port Port) bool {
	for _, zone := range catalog.SeaZones {
		if zone.ID == port.SeaZoneID {
			return classRank(zone.ZoneClass) > 0 && classRank(zone.ZoneClass) <= classRank("regional")
		}
	}
	return false
}

func shipRegionPorts(ship ShipState) [2]string {
	if ship.Ship.Status == "sunk" {
		return [2]string{}
	}
	ports := [2]string{ship.Ship.CurrentPortID}
	if (ship.Voyage != nil || ship.Ship.Status == "sailing") && len(ship.RouteLegs) > 0 {
		ports[1] = ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
	}
	return ports
}

func (r *Runner) ownedShipInRegion(region string) bool {
	for _, ship := range r.stateCache.Ships {
		for _, portID := range shipRegionPorts(ship) {
			if port, exists := findPort(r.stateCache.Catalog, portID); exists && port.BankingRegion == region {
				return true
			}
		}
	}
	return false
}

func (r *Runner) expansionFactorUpkeep(port Port) int64 {
	if r.ownedShipInRegion(port.BankingRegion) {
		return 0
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		mate, paired := expansionMate(agents, agent)
		if agent.Region != port.BankingRegion || !paired || agent.HiredAt.IsZero() || mate.HiredAt.IsZero() ||
			!slices.Contains(r.stateCache.Factors, agent.PortID) || !slices.Contains(r.stateCache.Factors, mate.PortID) {
			continue
		}
		for _, factorID := range r.stateCache.Factors {
			if factorID == agent.PortID || factorID == mate.PortID {
				continue
			}
			if factor, exists := findPort(r.stateCache.Catalog, factorID); exists && factor.BankingRegion == port.BankingRegion {
				return 0
			}
		}
		factors := len(r.stateCache.Factors)
		return DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, factors) -
			DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, factors-2)
	}
	return 0
}

func (r *Runner) expansionObservations(ctx context.Context, yard, partner Port, hull Hull) error {
	routes := decode[map[string]json.RawMessage](r.snapshot, "routes")
	if routes == nil {
		routes = make(map[string]json.RawMessage)
	}
	ports := decode[map[string]json.RawMessage](r.snapshot, "ports")
	if ports == nil {
		ports = make(map[string]json.RawMessage)
	}
	now := time.Now().UTC()
	retryReady := func(key string) bool {
		meta := r.meta[key]
		until := meta.AttemptedAt.Add(expansionObservationRetry)
		if (meta.Stale || meta.Error != "") && !meta.AttemptedAt.IsZero() && now.Before(until) {
			r.wakeBy(until)
			return false
		}
		return true
	}
	var tasks []fetchTask
	for _, pair := range [][2]string{{yard.ID, partner.ID}, {partner.ID, yard.ID}} {
		key := routeKey(pair[0], pair[1], hull.ID)
		if route, exists := r.stateCache.Routes[key]; exists && r.fresh("routes/"+key) {
			routes[key] = encode(route)
		} else if retryReady("routes/" + key) {
			path := "/v1/world/route?from=" + url.QueryEscape(pair[0]) + "&to=" + url.QueryEscape(pair[1]) + "&ship_class=" + url.QueryEscape(hull.ID)
			tasks = append(tasks, fetchTask{key: "routes/" + key, path: path})
		}
		if detail, exists := r.stateCache.Ports[pair[0]]; exists && r.fresh("ports/"+pair[0]) {
			ports[pair[0]] = encode(detail)
		} else if retryReady("ports/" + pair[0]) {
			tasks = append(tasks, fetchTask{key: "ports/" + pair[0], path: "/v1/world/port?port_id=" + url.QueryEscape(pair[0])})
		}
	}
	if len(tasks) == 0 {
		return nil
	}
	if err := r.captureWave(ctx, tasks, map[string]map[string]json.RawMessage{"routes": routes, "ports": ports}); err != nil {
		return err
	}
	if r.stateCache.Routes == nil {
		r.stateCache.Routes = make(map[string]Route)
	}
	if r.stateCache.Ports == nil {
		r.stateCache.Ports = make(map[string]PortDetail)
	}
	for _, pair := range [][2]string{{yard.ID, partner.ID}, {partner.ID, yard.ID}} {
		key := routeKey(pair[0], pair[1], hull.ID)
		var route Route
		if json.Unmarshal(routes[key], &route) == nil {
			r.stateCache.Routes[key] = route
		}
		var detail PortDetail
		if json.Unmarshal(ports[pair[0]], &detail) == nil {
			r.stateCache.Ports[pair[0]] = detail
		}
	}
	r.snapshot["routes"], r.snapshot["ports"], r.snapshot["_meta"] = encode(routes), encode(ports), encode(r.meta)
	return r.saveSnapshot(ctx)
}

type expansionRegionalEarnings struct {
	rate      float64
	available bool
}

func (r *Runner) expansionRegionalRates(ctx context.Context, asOf time.Time) (map[string]expansionRegionalEarnings, error) {
	rates := make(map[string]expansionRegionalEarnings)
	scale := float64(r.stateCache.Clock.Calendar.TimeScale)
	if scale <= 0 || r.stateCache.Clock.Paused {
		return rates, nil
	}
	window := time.Duration(float64(capacityMeasuredGameDays) * float64(24*time.Hour) / scale)
	windowStart := asOf.Add(-window)
	settlements, err := r.db.SettlementsBetween(ctx, r.account.ID, windowStart, asOf)
	if err != nil {
		return nil, err
	}
	type regionalHistory struct {
		net     float64
		unknown bool
		ships   map[string]bool
	}
	histories := make(map[string]*regionalHistory)
	for _, settlement := range settlements {
		switch settlement.Kind {
		case "spot", "charter", "freight", "spot+freight", "charter+freight", "recovery":
		default:
			continue
		}
		origin, originFound := findPort(r.stateCache.Catalog, settlement.Origin)
		destination, destinationFound := findPort(r.stateCache.Catalog, settlement.Destination)
		if !originFound || !destinationFound || origin.BankingRegion == "" || origin.BankingRegion != destination.BankingRegion {
			continue
		}
		history := histories[origin.BankingRegion]
		if history == nil {
			history = &regionalHistory{ships: make(map[string]bool)}
			histories[origin.BankingRegion] = history
		}
		history.ships[settlement.ShipID] = true
		history.unknown = history.unknown || !settlement.CostsKnown || settlement.ProceedsUnknown || settlement.GameSeconds <= 0
		history.net += float64(settlement.ProceedsPence) - float64(settlement.AcquisitionPence) -
			float64(settlement.FeesPence) - float64(settlement.ProvisionsPence) - float64(settlement.ExpensesPence)
	}
	for region, history := range histories {
		var hullDays float64
		counted := make(map[string]bool)
		complete := !history.unknown
		for _, ship := range r.stateCache.Ships {
			if ship.Ship.Status == "sunk" {
				continue
			}
			regional := history.ships[ship.Ship.ID]
			for _, portID := range shipRegionPorts(ship) {
				if port, found := findPort(r.stateCache.Catalog, portID); found && port.BankingRegion == region {
					regional = true
				}
			}
			if !regional {
				continue
			}
			counted[ship.Ship.ID] = true
			created := ship.Ship.CreatedAt
			if created == nil || created.IsZero() || created.After(asOf) {
				complete = false
				continue
			}
			hullDays += asOf.Sub(maxTime(windowStart, *created)).Seconds() * scale / 86_400
		}
		for shipID := range history.ships {
			complete = complete && counted[shipID]
		}
		if complete && hullDays > 0 {
			rates[region] = expansionRegionalEarnings{rate: history.net / hullDays, available: true}
		}
	}
	return rates, nil
}

func (r *Runner) expansionBuildEstimate(ctx context.Context, yard, partner Port, capacity CapacityGrowthInput, asOf time.Time) (GrowthDecision, YardQuote, bool, error) {
	rates, err := r.expansionRegionalRates(ctx, asOf)
	if err != nil {
		return GrowthDecision{}, YardQuote{}, false, err
	}
	return r.expansionBuildEstimateForRegion(ctx, yard, partner, capacity, asOf, rates[yard.BankingRegion])
}

func (r *Runner) expansionBuildEstimateForRegion(ctx context.Context, yard, partner Port, capacity CapacityGrowthInput, asOf time.Time, regional expansionRegionalEarnings) (GrowthDecision, YardQuote, bool, error) {
	capacity.SurplusPence -= 2 * expansionAgentHireFeePence
	capacity.HasSpareWork = true
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	grown := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grown, r.stateCache.Ships)
	var classInputs map[string]CapacityGrowthInput
	var err error
	type hullEstimate struct {
		evidence  expansionRateEstimate
		reason    string
		rejection capacityRejection
	}
	estimates := make(map[string]hullEstimate)
	var best GrowthDecision
	var bestQuote YardQuote
	bestFresh := false
	needed := int64(math.MaxInt64)
	var yardRank int
	for _, zone := range r.stateCache.Catalog.SeaZones {
		if zone.ID == yard.SeaZoneID {
			yardRank = classRank(zone.ZoneClass)
			break
		}
	}
	ratedQuote := false
	for _, quote := range r.stateCache.Quotes {
		hull, exists := findHull(r.stateCache.Catalog, quote.ShipClass)
		_, sourceExists := findPort(r.stateCache.Catalog, quote.PortID)
		if exists && sourceExists && cheapCapacityHull(hull) && !hull.CanSealDispatches &&
			hull.Phase <= r.stateCache.Clock.UnlockedPhase && yardRank > 0 && classRank(hull.HullCategory) >= yardRank &&
			quote.BuildMode == "turnkey_cash" && quote.Available && quote.TotalCostPence != nil && *quote.TotalCostPence > 0 {
			ratedQuote = true
			break
		}
	}
	for _, quote := range r.stateCache.Quotes {
		hull, exists := findHull(r.stateCache.Catalog, quote.ShipClass)
		if !exists || !cheapCapacityHull(hull) || hull.CanSealDispatches || hull.Phase > r.stateCache.Clock.UnlockedPhase ||
			quote.BuildMode != "turnkey_cash" || !quote.Available || quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 {
			continue
		}
		if ratedQuote && (yardRank <= 0 || classRank(hull.HullCategory) < yardRank) {
			continue
		}
		source, exists := findPort(r.stateCache.Catalog, quote.PortID)
		if !exists {
			continue
		}
		candidate := capacity
		candidate.Hull, candidate.HullPricePence = hull, *quote.TotalCostPence
		grown[len(grown)-1] = growthCandidate(yard, hull)
		candidate.IncrementalDailyUpkeepPence = DailyUpkeep(grown, r.stateCache.Catalog, len(r.stateCache.Factors)+2) - oldUpkeep
		candidate.Mortgage = nil
		candidate.CandidateRatePencePerGameDay = regional.rate
		basis := "measured regional realized net per hull game day"
		var estimate *expansionRateEstimate
		estimateReason := ""
		estimateRejection := capacityRejectionNone
		if yardRank <= 0 || classRank(hull.HullCategory) < yardRank {
			estimateReason = "expansion agent hull rating cannot enter the yard sea zone"
			estimateRejection = capacityRejectionHullRating
		} else {
			cached, loaded := estimates[hull.ID]
			if !loaded {
				if err := r.expansionObservations(ctx, yard, partner, hull); err != nil {
					return GrowthDecision{}, YardQuote{}, false, err
				}
				input := r.planningInput(growthCandidate(yard, hull))
				input.hazardEstimate = calibrateZoneHazard(input, hull, min(int64(4), hull.MaxCannons))
				if input.hazardEstimate != nil && input.fresh("zones") {
					for _, port := range []Port{yard, partner} {
						route := Route{Legs: []RouteLeg{{ToNode: port.ID}}}
						_, _, highest, valid := hazardZoneRisk(input, route, nil)
						if valid && float64(highest) > 1.5*float64(input.hazardEstimate.highestRisk) {
							cached.reason = "expansion pair arrival-zone risk exceeds 1.5x calibrated maximum"
							cached.rejection = capacityRejectionHazard
							break
						}
					}
				}
				if cached.reason == "" {
					cached.evidence, cached.reason = expansionGossipEstimate(input, yard, partner, hull, expansionPrices(input))
				}
				estimates[hull.ID] = cached
			}
			estimateReason = cached.reason
			estimateRejection = cached.rejection
			candidate.InitialStoresPence = max(candidate.InitialStoresPence, cached.evidence.ProvisionsPence)
			if !regional.available {
				estimate = &cached.evidence
				basis = "gossip estimate"
				candidate.CandidateRatePencePerGameDay = estimate.RatePencePerGameDay
			}
			if cached.evidence.AskPence > 0 && cached.evidence.CargoTons <= math.MaxInt64/cached.evidence.AskPence {
				candidate.CandidateLotCashPence = max(candidate.LotCashPence,
					growthCashTotal(cached.evidence.AskPence*cached.evidence.CargoTons, cached.evidence.CustomsPence, cached.evidence.ProvisionsPence))
			} else if estimateReason == "" {
				estimateReason = "gossip estimate cargo working cash exceeds integer-pence bounds"
			}
		}
		decision := EvaluateCapacityGrowth(candidate)
		if estimateReason != "" {
			decision.Accepted, decision.EconomicallyViable = false, false
			decision.Reason = estimateReason
			decision.rejection = estimateRejection
		} else if !decision.Accepted && decision.CashShortfallPence > 0 &&
			r.mortgageAvailable() && validMortgageTerms(quote.Mortgage, candidate.HullPricePence) {
			measured, loaded := classInputs[hull.ID]
			if !loaded {
				measured, err = r.capacityClassInput(ctx, candidate, hull.ID, asOf)
				if err != nil {
					return GrowthDecision{}, YardQuote{}, false, err
				}
				if classInputs == nil {
					classInputs = make(map[string]CapacityGrowthInput)
				}
				classInputs[hull.ID] = measured
			}
			financed := candidate
			financed.Mortgage = quote.Mortgage
			financed.MeasuredNetAvailable = measured.MeasuredNetAvailable
			financed.MeasuredRatePencePerGameDay = measured.MeasuredRatePencePerGameDay
			financed.SampleHullGameDays = measured.SampleHullGameDays
			mortgage := EvaluateCapacityGrowth(financed)
			if mortgage.Accepted || mortgage.EconomicallyViable &&
				(!decision.EconomicallyViable || mortgage.CashShortfallPence < decision.CashShortfallPence) {
				decision = mortgage
			}
		}
		decision.EvidenceBasis, decision.ExpansionEstimate = basis, estimate
		_, quoteErr := r.growthQuote(source, hull)
		fresh := quoteErr == nil
		total := growthCashTotal(growthCapital(decision), decision.WorkingCapitalPence, 2*expansionAgentHireFeePence)
		better := bestQuote.PortID == "" || decision.Accepted && !best.Accepted ||
			decision.Accepted == best.Accepted && (decision.EconomicallyViable && !best.EconomicallyViable ||
				decision.EconomicallyViable == best.EconomicallyViable &&
					(fresh && !bestFresh || fresh == bestFresh &&
						(total < needed || total == needed && source.ID+"|"+hull.ID < bestQuote.PortID+"|"+bestQuote.ShipClass)))
		if better {
			best, bestQuote, bestFresh, needed = decision, quote, fresh, total
		}
	}
	if bestQuote.PortID == "" {
		best.Reason = expansionAgentQuoteReason
	}
	return best, bestQuote, bestFresh, nil
}

type expansionAdmission struct {
	yard        Port
	partner     Port
	region      string
	score       int64
	build       GrowthDecision
	quote       YardQuote
	fresh       bool
	input       PlanningInput
	required    map[string]int64
	needed      int64
	retryUntil  time.Time
	probe       bool
	probeReason string
}

func (r *Runner) expansionRetryUntil(region string) time.Time {
	outcome := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")[region]
	until := maxTime(maxTime(outcome.RetryAt, outcome.UnevaluatedRetryAt), outcome.CapacityRetryAt)
	at := decode[map[string]time.Time](r.snapshot, "_expansion_agent_retries")[region]
	// markExpansionRetry records the creation time only when neither factor
	// was hired, and considerExpansionAgent stamps that same instant as the
	// attempt. Hired or evaluated pairs keep the full start floor.
	if !at.IsZero() && at.Equal(outcome.AttemptAt) && outcome.Losses == 0 && outcome.UnevaluatedAttemptAt.IsZero() {
		until = maxTime(until, at.Add(2*time.Hour+expansionUnhiredRetry))
		at = time.Time{}
	}
	for _, agent := range decode[[]expansionAgent](r.snapshot, "_expansion_agents") {
		if agent.Region == region {
			at = maxTime(at, maxTime(agent.CreatedAt, agent.HiredAt))
		}
	}
	if !at.IsZero() {
		until = maxTime(until, at.Add(expansionRegionRetry))
	}
	return until
}

func (r *Runner) expansionAdmission(ctx context.Context) (expansionAdmission, string, error) {
	admission := expansionAdmission{region: "unavailable"}
	for _, dataset := range []string{"catalog", "clock", "ships", "factors", "contracts", "vaults", "rates", "profile"} {
		if !r.fresh(dataset) {
			return admission, "expansion agent requires current successful " + dataset + " authority", nil
		}
	}
	if len(r.stateCache.Factors)+2 > 12 {
		return admission, "expansion pair requires two free factor slots", nil
	}
	now := time.Now().UTC()
	if err := r.expireAwaitingExpansion(ctx, now); err != nil {
		return admission, "", err
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	lastStarted := decode[time.Time](r.snapshot, "_expansion_last_started_at")
	for _, agent := range agents {
		mate, paired := expansionMate(agents, agent)
		startedAt := agent.HiredAt
		if paired && !mate.HiredAt.IsZero() && (startedAt.IsZero() || mate.HiredAt.Before(startedAt)) {
			startedAt = mate.HiredAt
		}
		lastStarted = maxTime(lastStarted, startedAt)
		if paired && now.Before(expansionPairTime(agents, agent).Add(2*time.Hour)) {
			admission.region = agent.Region
			return admission, "expansion agent waits for the existing exploration to finish", nil
		}
	}
	if until := lastStarted.Add(expansionRegionRetry); !lastStarted.IsZero() && now.Before(until) {
		admission.retryUntil = until
		r.wakeBy(until)
		return admission, "expansion starts at most one paid pair per 24 real hours", nil
	}
	planning := r.planningInput(ShipState{})
	candidates := expansionAgentYards(r.stateCache, planning)
	withSpread := make(map[string]bool)
	for _, pair := range candidates {
		withSpread[pair.yard.BankingRegion] = true
	}
	yardPresent := make(map[string]bool)
	for _, port := range r.stateCache.Catalog.Ports {
		if port.HasShipyard && port.Phase >= 2 && port.Phase <= r.stateCache.Clock.UnlockedPhase && r.portPresence(port.ID) {
			yardPresent[port.BankingRegion] = true
		}
	}
	for _, pair := range expansionProbeYards(r.stateCache, planning, true) {
		if !yardPresent[pair.yard.BankingRegion] {
			candidates = append(candidates, pair)
		}
	}
	eligible := candidates[:0]
	for _, pair := range candidates {
		if until := r.expansionRetryUntil(pair.yard.BankingRegion); now.Before(until) {
			if admission.retryUntil.IsZero() || until.Before(admission.retryUntil) {
				admission.yard, admission.partner, admission.region, admission.retryUntil = pair.yard, pair.partner, pair.yard.BankingRegion, until
			}
			continue
		}
		eligible = append(eligible, pair)
	}
	if len(eligible) == 0 {
		if !admission.retryUntil.IsZero() {
			r.wakeBy(admission.retryUntil)
			return admission, "expansion region waits for its retry deadline", nil
		}
		return admission, "expansion agent has no positive historical pair spread in an unoccupied region", nil
	}
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return admission, "", err
	}
	if target.PortID != "" {
		return admission, capacitySavingsReason, nil
	}
	mission, _, err := r.loadMission(ctx, "__treasury")
	if err != nil {
		return admission, "", err
	}
	if mission != nil {
		return admission, "expansion agent waits for the current treasury intent", nil
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return admission, "", err
	}
	var pass expansionEvidence
	passLoaded, passReason := false, ""
	var firstRejected expansionAdmission
	firstReason := ""
	cashWaiting := make(map[string]bool)
	for _, pair := range eligible {
		candidate := expansionAdmission{
			yard: pair.yard, partner: pair.partner, region: pair.yard.BankingRegion,
			score: pair.score, input: input, probe: pair.score == 0,
		}
		if candidate.probe {
			if cashWaiting[candidate.region] {
				continue
			}
			candidate.probeReason = expansionProbeReason
			if withSpread[candidate.region] {
				candidate.probeReason = expansionEstimateProbeReason
			}
		}
		var reason string
		var err error
		if candidate.probe {
			reason, _, err = r.expansionProbeAdmission(ctx, &candidate)
		} else {
			if !passLoaded {
				pass, passReason, err = r.expansionPassEvidence(ctx, input)
				passLoaded = true
			}
			reason = passReason
			if err == nil && reason == "" {
				reason, _, err = r.expansionBuildAdmissionForPass(ctx, &candidate, &pass)
			}
		}
		if err != nil {
			return candidate, "", err
		}
		if reason == "" {
			return candidate, "", nil
		}
		if !candidate.probe && candidate.build.EconomicallyViable && candidate.build.CashShortfallPence > 0 {
			cashWaiting[candidate.region] = true
			reason = fmt.Sprintf("overseas build waits for %.2f Cr cash shortfall: %s", float64(candidate.build.CashShortfallPence)/100, reason)
			firstRejected, firstReason = candidate, reason
		}
		if !candidate.probe && !cashWaiting[candidate.region] {
			// A later probe must not erase the rejected hull estimate's evidence.
			if _, err := r.stageExpansionAgent(ctx, candidate, reason); err != nil {
				return candidate, "", err
			}
		}
		if firstReason == "" {
			firstRejected, firstReason = candidate, reason
		}
	}
	return firstRejected, firstReason, nil
}

func expansionFeeCommitments(state AccountState, snapshot map[string]json.RawMessage) map[string]int64 {
	agents := decode[[]expansionAgent](snapshot, "_expansion_agents")
	var fees map[string]int64
	for _, agent := range agents {
		if _, paired := expansionMate(agents, agent); !paired || !agent.HiredAt.IsZero() ||
			slices.Contains(state.Factors, agent.PortID) {
			continue
		}
		if fees == nil {
			fees = make(map[string]int64)
		}
		fees[agent.Region] = growthCashTotal(fees[agent.Region], expansionAgentHireFeePence)
	}
	return fees
}

func (r *Runner) expansionRequired(ctx context.Context, input PlanningInput, region string, fees int64) (map[string]int64, error) {
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return nil, err
	}
	return r.expansionFundingRequired(input, required, region, fees), nil
}

func (r *Runner) expansionFundingRequired(input PlanningInput, required map[string]int64, region string, fees int64) map[string]int64 {
	ownFees := expansionFeeCommitments(r.stateCache, r.snapshot)[region]
	required[region] = max(0, required[region]-ownFees)
	for candidate, protected := range input.ReserveByRegion {
		if candidate == region {
			protected = max(0, protected-ownFees)
		}
		required[candidate] = max(required[candidate], min(protected, input.Balances[candidate]))
	}
	required[region] = growthCashTotal(max(required[region], r.protectedReservePence()), fees)
	return required
}

type expansionEvidence struct {
	growth   growthEvidence
	capacity CapacityGrowthInput
	rates    map[string]expansionRegionalEarnings
}

func (r *Runner) expansionPassEvidence(ctx context.Context, input PlanningInput) (expansionEvidence, string, error) {
	pass := expansionEvidence{growth: growthEvidence{workingByRegion: make(map[string]int64)}}
	if reason, err := r.growthCashFunding(ctx, &pass.growth); err != nil || reason != "" {
		return pass, reason, err
	}
	pass.growth.available = 0
	for region, balance := range input.Balances {
		pass.growth.available = growthCashTotal(pass.growth.available, max(0, balance-input.ReserveByRegion[region]))
	}
	capacity, reason, err := r.capacityFleetInput(ctx, &pass.growth)
	if err != nil || reason != "" {
		return pass, reason, err
	}
	pass.capacity = capacity
	pass.rates, err = r.expansionRegionalRates(ctx, pass.growth.asOf)
	return pass, "", err
}

func (r *Runner) expansionBuildAdmission(ctx context.Context, admission *expansionAdmission) (string, bool, error) {
	pass, reason, err := r.expansionPassEvidence(ctx, admission.input)
	if err != nil || reason != "" {
		return reason, false, err
	}
	return r.expansionBuildAdmissionForPass(ctx, admission, &pass)
}
func (r *Runner) expansionProbeAdmission(ctx context.Context, admission *expansionAdmission) (string, bool, error) {
	if reason, definitive := expansionPairSafety(admission.input, admission.yard, admission.partner); reason != "" {
		return reason, definitive, nil
	}
	evidence := growthEvidence{}
	if reason, err := r.growthCashFunding(ctx, &evidence); err != nil || reason != "" {
		return reason, false, err
	}
	netWorth, err := r.growthNetWorth(ctx, &evidence)
	if err != nil {
		return "", false, err
	}
	if netWorth <= 0 {
		return "expansion agent requires calibrated hazard and net worth no older than 6 real hours", false, nil
	}
	admission.required = r.expansionFundingRequired(admission.input, evidence.required, admission.region, 2*expansionAgentHireFeePence)
	_, fees, _, funded := measuredFunding(admission.input.Balances, admission.required, admission.input.Rates)
	admission.needed = growthCashTotal(admission.required[admission.region], fees)
	if !funded {
		return "overseas probe waits for both hire fees, transfer fees and protected regional reserves", false, nil
	}
	return "", false, nil
}

func (r *Runner) expansionBuildAdmissionForPass(ctx context.Context, admission *expansionAdmission, pass *expansionEvidence) (string, bool, error) {
	input := &admission.input
	var err error
	admission.required = r.expansionFundingRequired(*input, maps.Clone(pass.growth.required), admission.region, 2*expansionAgentHireFeePence)
	projected, fees, _, funded := measuredFunding(input.Balances, admission.required, input.Rates)
	capacity := pass.capacity
	capacity.SurplusPence = 2 * expansionAgentHireFeePence
	for region, balance := range projected {
		capacity.SurplusPence = growthCashTotal(capacity.SurplusPence, max(0, balance-admission.required[region]))
	}
	admission.build, admission.quote, admission.fresh, err = r.expansionBuildEstimateForRegion(ctx,
		admission.yard, admission.partner, capacity, pass.growth.asOf, pass.rates[admission.region])
	if err != nil {
		return "", false, err
	}
	admission.needed = growthCashTotal(growthCapital(admission.build), admission.build.WorkingCapitalPence, 2*expansionAgentHireFeePence)
	if !admission.build.Accepted {
		definitive := admission.build.rejection != capacityRejectionNone
		return admission.build.Reason, definitive, nil
	}
	hull, _ := findHull(r.stateCache.Catalog, admission.quote.ShipClass)
	if err := r.capacityHazardEstimate(ctx, input, admission.yard, hull, &pass.growth); err != nil {
		return "", false, err
	}
	if input.hazardEstimate == nil {
		return "expansion agent requires calibrated hazard and net worth no older than 6 real hours", false, nil
	}
	if !r.fresh("zones") {
		return "expansion pair requires current successful zones authority", false, nil
	}
	for _, port := range []Port{admission.yard, admission.partner} {
		route := Route{Legs: []RouteLeg{{ToNode: port.ID}}}
		_, _, highest, valid := hazardZoneRisk(*input, route, nil)
		if !valid {
			return "expansion pair requires current nonnegative arrival-zone risk at both ports", false, nil
		}
		if float64(highest) > 1.5*float64(input.hazardEstimate.highestRisk) {
			return "expansion pair arrival-zone risk exceeds 1.5x calibrated maximum", true, nil
		}
	}
	if capped := capacityEstimatedExposure(admission.build, pass.growth.hazard.netWorth); !capped.EconomicallyViable {
		return capped.Reason, true, nil
	}
	admission.needed = growthCashTotal(admission.needed, fees)
	if !funded {
		return "expansion agent must preserve the hull capital, capacity working cash and funding fees", false, nil
	}
	return "", false, nil
}

func (r *Runner) markExpansionLoss(agent expansionAgent, reason string) {
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	outcome := outcomes[agent.Region]
	// Retained factors from an older pair cannot fail a newly selected pair.
	if !agent.HiredAt.IsZero() && outcome.AttemptAt.After(maxTime(agent.CreatedAt, agent.HiredAt)) {
		return
	}
	paid := !agent.HiredAt.IsZero()
	if !paid {
		for _, recorded := range decode[[]expansionAgent](r.snapshot, "_expansion_agents") {
			if recorded.Region == agent.Region && !recorded.HiredAt.IsZero() &&
				!outcome.AttemptAt.After(maxTime(recorded.CreatedAt, recorded.HiredAt)) {
				paid = true
				break
			}
		}
	}
	if !paid {
		return
	}
	if outcomes == nil {
		outcomes = make(map[string]expansionOutcome)
	}
	if outcome.Reason != "" {
		return
	}
	if outcome.AttemptAt.IsZero() {
		outcome.AttemptAt = maxTime(agent.CreatedAt, agent.HiredAt)
	}
	outcome.Losses++
	delay := expansionRegionRetry * time.Duration(1<<min(2, max(0, outcome.Losses-1)))
	outcome.RetryAt = time.Now().UTC().Add(delay)
	outcome.Reason = reason
	outcomes[agent.Region] = outcome
	r.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
}

func (r *Runner) markExpansionUnevaluated(agent expansionAgent) {
	if agent.HiredAt.IsZero() {
		return
	}
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	if outcomes == nil {
		outcomes = make(map[string]expansionOutcome)
	}
	outcome := outcomes[agent.Region]
	if outcome.AttemptAt.After(maxTime(agent.CreatedAt, agent.HiredAt)) {
		return
	}
	if outcome.AttemptAt.IsZero() {
		outcome.AttemptAt = agent.CreatedAt
		if outcome.AttemptAt.IsZero() {
			outcome.AttemptAt = agent.HiredAt
		}
	}
	// Both dismissals and an expired partner can finish the same paid attempt.
	if outcome.UnevaluatedAttemptAt.Equal(outcome.AttemptAt) {
		return
	}
	outcome.UnevaluatedAttempts++
	outcome.UnevaluatedAttemptAt = outcome.AttemptAt
	delay := expansionRegionRetry * time.Duration(1<<min(2, max(0, outcome.UnevaluatedAttempts-1)))
	outcome.UnevaluatedRetryAt = time.Now().UTC().Add(delay)
	outcomes[agent.Region] = outcome
	r.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
}

func (r *Runner) resetExpansionUnevaluated(region string) {
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	outcome := outcomes[region]
	if outcome.UnevaluatedAttempts == 0 && outcome.UnevaluatedAttemptAt.IsZero() && outcome.UnevaluatedRetryAt.IsZero() {
		return
	}
	outcome.UnevaluatedAttempts = 0
	outcome.UnevaluatedAttemptAt, outcome.UnevaluatedRetryAt = time.Time{}, time.Time{}
	outcomes[region] = outcome
	r.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
}

func (r *Runner) clearExpansionLossAfterBuild(ctx context.Context, portID string) error {
	port, found := findPort(r.stateCache.Catalog, portID)
	if !found {
		return nil
	}
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	if _, exists := outcomes[port.BankingRegion]; !exists {
		return nil
	}
	delete(outcomes, port.BankingRegion)
	r.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
	return r.saveSnapshot(ctx)
}

func (r *Runner) considerExpansionAgent(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	admission, reason, err := r.expansionAdmission(ctx)
	if err != nil {
		return false, err
	}
	return r.stageExpansionAgent(ctx, admission, reason)
}

func (r *Runner) stageExpansionAgent(ctx context.Context, admission expansionAdmission, reason string) (bool, error) {
	if reason != "" {
		err := r.decision(ctx, "", "expansion_agent:"+admission.region, reason, map[string]any{
			"region": admission.region, "reason": reason, "port_id": admission.yard.ID,
			"partner_port_id": admission.partner.ID, "hull_class": admission.quote.ShipClass,
			"estimated_quote_port": admission.quote.PortID, "estimated_hull_capital_stale": !admission.fresh,
			"estimated_hull_capital_pence": growthCapital(admission.build), "needed_cash_pence": admission.needed,
			"retry_until": admission.retryUntil,
			"build":       admission.build,
			"estimate":    admission.build.ExpansionEstimate,
		})
		return false, err
	}
	yard, partner := admission.yard, admission.partner
	reason = "an admissible estimated build supports the paid expansion pair"
	if admission.probe {
		reason = admission.probeReason
		if reason == "" {
			reason = expansionProbeReason
		}
	}
	if err := r.decision(ctx, "", "expansion_agent", reason, map[string]any{
		"port_id": yard.ID, "partner_port_id": partner.ID, "region": yard.BankingRegion, "score_pence": admission.score,
		"hull_class":                   admission.quote.ShipClass,
		"estimated_hull_capital_pence": growthCapital(admission.build), "estimated_quote_port": admission.quote.PortID,
		"estimated_hull_capital_stale": !admission.fresh, "competition": competitionAt(admission.input.Competition, yard.BankingRegion),
		"estimate": admission.build.ExpansionEstimate, "probe": admission.probe,
	}); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	// A still-hired factor keeps its record so manageExpansionAgents can
	// dismiss it later; dropping it would leave its daily upkeep unmanaged.
	agents = slices.DeleteFunc(agents, func(agent expansionAgent) bool {
		return agent.Region == yard.BankingRegion && !slices.Contains(r.stateCache.Factors, agent.PortID)
	})
	agents = append(agents,
		expansionAgent{PortID: yard.ID, Partner: partner.ID, Region: yard.BankingRegion, CreatedAt: now, Probe: admission.probe},
		expansionAgent{PortID: partner.ID, Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: now, Probe: admission.probe},
	)
	r.snapshot["_expansion_agents"] = encode(agents)
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	if outcomes == nil {
		outcomes = make(map[string]expansionOutcome)
	}
	outcome := outcomes[yard.BankingRegion]
	outcome.AttemptAt, outcome.Reason = now, ""
	outcomes[yard.BankingRegion] = outcome
	r.snapshot["_expansion_agent_outcomes"] = encode(outcomes)
	if err := r.saveSnapshot(ctx); err != nil {
		return false, err
	}
	if changed, err := r.fundTreasury(ctx, admission.input.Balances, admission.required); changed || err != nil {
		return changed, err
	}
	if admission.input.Balances[yard.BankingRegion] < admission.required[yard.BankingRegion] {
		return false, r.holdExpected(ctx, "", "expansion agent waits for completed regional fee and reserve funding")
	}
	mission := &store.Mission{
		AccountID: r.account.ID, ShipID: "__treasury", Phase: "hire-factor",
		Destination: yard.ID, Generation: uuid.NewString(),
	}
	data := missionData{Plan: Plan{Kind: "treasury"}, StartedAt: now, CostsKnown: true}
	return true, r.stage(ctx, mission, &data, ShipState{}, "hire-factor", "/v1/company/hire-factor", map[string]any{"port_id": yard.ID})
}

func (r *Runner) markExpansionRetry(agent expansionAgent) {
	retries := decode[map[string]time.Time](r.snapshot, "_expansion_agent_retries")
	if retries == nil {
		retries = make(map[string]time.Time)
	}
	retries[agent.Region] = maxTime(retries[agent.Region], maxTime(agent.CreatedAt, agent.HiredAt))
	r.snapshot["_expansion_agent_retries"] = encode(retries)
}

func (r *Runner) expireAwaitingExpansion(ctx context.Context, now time.Time) error {
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	expired := make(map[string]bool)
	for _, agent := range agents {
		if _, paired := expansionMate(agents, agent); paired && agent.HiredAt.IsZero() &&
			!now.Before(expansionPairTime(agents, agent).Add(2*time.Hour)) {
			expired[agent.PortID] = true
		}
	}
	if len(expired) == 0 {
		return nil
	}
	// An unresolved treasury action may still land. Its journal must reconcile
	// before the controller can release the corresponding exploration intent.
	mission, _, err := r.loadMission(ctx, "__treasury")
	if err != nil || mission != nil {
		return err
	}
	for _, agent := range agents {
		if expired[agent.PortID] || expired[agent.Partner] {
			r.markExpansionRetry(agent)
			r.markExpansionUnevaluated(agent)
		}
	}
	retained := slices.DeleteFunc(agents, func(agent expansionAgent) bool {
		return expired[agent.PortID]
	})
	for i := range retained {
		if expired[retained[i].Partner] {
			retained[i].Partner = ""
		}
	}
	r.snapshot["_expansion_agents"] = encode(retained)
	return r.saveSnapshot(ctx)
}

func expansionMate(agents []expansionAgent, agent expansionAgent) (expansionAgent, bool) {
	if agent.Partner != "" {
		for _, mate := range agents {
			if mate.PortID == agent.Partner && mate.Partner == agent.PortID {
				return mate, true
			}
		}
	}
	return expansionAgent{}, false
}

func expansionPairTime(agents []expansionAgent, agent expansionAgent) time.Time {
	at := agent.HiredAt
	if mate, found := expansionMate(agents, agent); found {
		at = maxTime(at, mate.HiredAt)
	}
	if at.IsZero() {
		at = agent.CreatedAt
	}
	return at
}

func (r *Runner) expansionPairLive(agents []expansionAgent, portID string, now time.Time) bool {
	for _, agent := range agents {
		if agent.PortID != portID {
			continue
		}
		mate, found := expansionMate(agents, agent)
		return found && (now.Before(expansionPairTime(agents, agent).Add(2*time.Hour)) ||
			!agent.HiredAt.IsZero() && !mate.HiredAt.IsZero() &&
				slices.Contains(r.stateCache.Factors, agent.PortID) && slices.Contains(r.stateCache.Factors, mate.PortID))
	}
	return false
}

func (r *Runner) expansionAwaitingEvaluation() bool {
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	now := time.Now().UTC()
	for _, agent := range agents {
		mate, found := expansionMate(agents, agent)
		if !found {
			continue
		}
		pairTime := expansionPairTime(agents, agent)
		until := pairTime.Add(2 * time.Hour)
		if !now.Before(until) {
			continue
		}
		if agent.HiredAt.IsZero() || mate.HiredAt.IsZero() {
			r.wakeBy(until)
			return true
		}
		yard, found := findPort(r.stateCache.Catalog, agent.PortID)
		if !found || !yard.HasShipyard {
			continue
		}
		if !maxTime(agent.EvaluatedAt, mate.EvaluatedAt).After(pairTime) || !r.fresh("books/"+agent.PortID) || !r.fresh("books/"+mate.PortID) ||
			r.stateCache.Markets[agent.PortID].Visibility != "quay" || r.stateCache.Markets[mate.PortID].Visibility != "quay" {
			r.wakeBy(until)
			return true
		}
	}
	return false
}

// Once an overseas yard is unlocked, Phase 1 construction and new financing
// stay closed even when overseas exploration is temporarily exhausted.
func (r *Runner) holdExpansionGrowth(ctx context.Context, canExplore bool, phaseOneBuild bool) (bool, bool, error) {
	if !r.opts.Execute {
		return false, false, nil
	}
	if r.expansionAwaitingEvaluation() {
		reason := "growth waits for the expansion pair's fresh capacity evaluation"
		if phaseOneBuild {
			if overseasReason := r.overseasGrowthHoldReason(reason); overseasReason != "" {
				reason = overseasReason
			}
		}
		return true, false, r.holdExpected(ctx, "", reason)
	}
	admissionReason := ""
	if canExplore {
		admission, reason, err := r.expansionAdmission(ctx)
		if err != nil {
			return false, false, err
		}
		changed, err := r.stageExpansionAgent(ctx, admission, reason)
		if changed || err != nil {
			return changed, changed, err
		}
		admissionReason = reason
	}
	if phaseOneBuild {
		if reason := r.overseasGrowthHoldReason(admissionReason); reason != "" {
			return true, false, r.holdExpected(ctx, "", reason)
		}
	}
	return false, false, nil
}

func (r *Runner) overseasGrowthHoldReason(wait string) string {
	if !r.overseasYardUnlocked() {
		return ""
	}
	_, detail, retry := r.overseasGrowthStatus()
	reason := "Phase 1 growth is closed: new hulls are built in Phase 2+ regions; " + detail
	if wait != "" {
		reason += "; " + wait
	}
	if !retry.IsZero() {
		reason += "; earliest overseas retry " + retry.UTC().Format(time.RFC3339)
		r.wakeBy(retry)
	}
	return reason
}

// Paid failed exploration, measured unbuildable capacity, hard safety gates and
// absent probe access exhaust a region. Mere scheduling or funding waits do not.
func (r *Runner) overseasGrowthStatus() (bool, string, time.Time) {
	if r.stateCache.Clock.UnlockedPhase < 2 {
		return true, "", time.Time{}
	}
	for _, dataset := range []string{"catalog", "clock", "ships", "factors"} {
		if !r.fresh(dataset) {
			return false, "overseas expansion requires current successful " + dataset + " authority", time.Time{}
		}
	}
	input := r.planningInput(ShipState{})
	pairs := expansionProbeYards(r.stateCache, input, false)
	regions := make(map[string][]expansionPair)
	for _, port := range r.stateCache.Catalog.Ports {
		if port.Phase >= 2 && port.Phase <= r.stateCache.Clock.UnlockedPhase && port.BankingRegion != "" {
			regions[port.BankingRegion] = nil
		}
	}
	for _, pair := range pairs {
		regions[pair.yard.BankingRegion] = append(regions[pair.yard.BankingRegion], pair)
	}
	names := make([]string, 0, len(regions))
	for region := range regions {
		names = append(names, region)
	}
	slices.Sort(names)
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	now := time.Now().UTC()
	globalRetry := decode[time.Time](r.snapshot, "_expansion_last_started_at")
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		globalRetry = maxTime(globalRetry, agent.HiredAt)
	}
	if !globalRetry.IsZero() {
		globalRetry = globalRetry.Add(expansionRegionRetry)
	}
	var details []string
	waiting := false
	var earliest time.Time
	for _, region := range names {
		retry := maxTime(globalRetry, r.expansionRetryUntil(region))
		if now.Before(retry) && (earliest.IsZero() || retry.Before(earliest)) {
			earliest = retry
		}
		outcome := outcomes[region]
		reason := ""
		switch {
		case outcome.Losses > 0 && now.Before(outcome.RetryAt):
			reason = "paid exploration recorded a realized loss: " + outcome.Reason
		case outcome.UnevaluatedAttempts > 0 && now.Before(outcome.UnevaluatedRetryAt):
			reason = "paid overseas pair ended without a capacity evaluation"
		case outcome.CapacityReason != "" && now.Before(outcome.CapacityRetryAt):
			reason = "measured overseas capacity is not buildable: " + outcome.CapacityReason
		case len(regions[region]) == 0:
			reason = "no probe-able overseas yard and partner"
		}
		if reason != "" {
			details = append(details, region+": "+reason)
			continue
		}
		exploring := slices.ContainsFunc(agents, func(agent expansionAgent) bool {
			_, paired := expansionMate(agents, agent)
			return agent.Region == region && paired && now.Before(expansionPairTime(agents, agent).Add(2*time.Hour))
		})
		if exploring {
			waiting = true
			details = append(details, region+": existing overseas exploration is still within its observation floor")
			continue
		}
		yardPresent := false
		for _, pair := range regions[region] {
			yardPresent = yardPresent || r.portPresence(pair.yard.ID)
		}
		if !yardPresent && len(r.stateCache.Factors)+2 > 12 {
			details = append(details, region+": no probe-able overseas yard with two free factor slots")
			continue
		}
		hardReason := ""
		for _, pair := range regions[region] {
			if yardPresent && !r.portPresence(pair.yard.ID) {
				continue
			}
			gate, hard := expansionPairSafety(input, pair.yard, pair.partner)
			if hard {
				hardReason = gate
				continue
			}
			reason = gate
			if reason == "" {
				reason = "probe-able overseas yard awaits quay evidence and profitable capacity"
			}
			break
		}
		if reason == "" {
			details = append(details, region+": "+hardReason)
			continue
		}
		if yardPresent {
			reason = "existing overseas yard presence awaits profitable capacity"
		} else if now.Before(retry) {
			reason = "overseas exploration waits for its retry deadline"
		}
		waiting = true
		details = append(details, region+": "+reason)
	}
	return !waiting, strings.Join(details, "; "), earliest
}

func (r *Runner) clearPhaseOneGrowthTargets(ctx context.Context) error {
	if !r.overseasYardUnlocked() {
		return nil
	}
	changed := false
	target := decode[capacitySavingsTarget](r.snapshot, "_capacity_savings_target")
	if port, _ := findPort(r.stateCache.Catalog, target.PortID); r.phaseOneYard(port) {
		delete(r.snapshot, "_capacity_savings_target")
		changed = true
	}
	if port, _ := findPort(r.stateCache.Catalog, decode[string](r.snapshot, "_capacity_growth_port")); r.phaseOneYard(port) {
		delete(r.snapshot, "_capacity_growth_port")
		changed = true
	}
	if !changed {
		return nil
	}
	return r.saveSnapshot(ctx)
}

// Retire obsolete local financing only before the first accepted loan and on
// current overseas authority. Possibly sent journal actions reconcile normally.
func (r *Runner) retirePhaseOneFinancing(ctx context.Context, mission *store.Mission, data *missionData) (bool, bool, error) {
	if !r.opts.Execute || data.BuiltShipID != "" || data.PlayerLoan == nil && (data.Capacity == nil || data.Capacity.Financing == nil) {
		return false, false, nil
	}
	if data.Capacity != nil && data.Capacity.Financing != nil && len(data.Capacity.Financing.Loans) > 0 {
		return false, false, nil
	}
	if data.PlayerLoan != nil {
		if request := r.recordedPlayerRequest(data.PlayerLoan); request != nil && request.Status == "funded" {
			return false, false, nil
		}
	}
	for _, dataset := range []string{"catalog", "clock", "ships", "factors", "zones"} {
		if !r.fresh(dataset) {
			return false, false, nil
		}
	}
	port, _ := findPort(r.stateCache.Catalog, data.BuildPort)
	if !r.phaseOneYard(port) {
		return false, false, nil
	}
	if data.Action != nil {
		if data.Action.Stage != "loan-request" && data.Action.Stage != "loan-accept" && data.Action.Stage != "build" {
			return false, false, nil
		}
		exists, err := r.db.HasAction(ctx, r.account.ID, data.Action.ID)
		if err != nil || exists {
			return false, false, err
		}
	}
	reason := r.overseasGrowthHoldReason("Phase 1 financing is retired before the first loan is accepted; open requests lapse unapproved")
	if err := r.holdExpected(ctx, mission.ShipID, reason); err != nil {
		return true, false, err
	}
	if data.PlayerLoan != nil {
		if err := r.recordPlayerLoanDecision(ctx, data.PlayerLoan, "overseas_first", reason); err != nil {
			return true, false, err
		}
	} else if err := r.recordCapacityFinanceFailure(ctx, data, "overseas_first", reason); err != nil {
		return true, false, err
	}
	data.Action = nil
	if err := r.clearPhaseOneGrowthTargets(ctx); err != nil {
		return true, false, err
	}
	return true, true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
}

// phaseOneYard reports a European yard closed to new construction.
func (r *Runner) phaseOneYard(port Port) bool {
	return port.ID != "" && port.Phase < 2 && r.overseasYardUnlocked()
}

// portPresence reports a factor or an idle ship lying at the port, the quay
// presence a yard candidate needs.
func (r *Runner) portPresence(portID string) bool {
	if slices.Contains(r.stateCache.Factors, portID) {
		return true
	}
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.CurrentPortID == portID && ship.Voyage == nil &&
			(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") {
			return true
		}
	}
	return false
}

func (r *Runner) overseasYardUnlocked() bool {
	for _, port := range r.stateCache.Catalog.Ports {
		if port.HasShipyard && port.Phase >= 2 && port.Phase <= r.stateCache.Clock.UnlockedPhase {
			return true
		}
	}
	return false
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
	now := time.Now().UTC()
	if err := r.expireAwaitingExpansion(ctx, now); err != nil {
		return false, err
	}
	agents = decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		if _, found := expansionMate(agents, agent); !found || !agent.HiredAt.IsZero() ||
			slices.Contains(r.stateCache.Factors, agent.PortID) || !now.Before(expansionPairTime(agents, agent).Add(2*time.Hour)) {
			continue
		}
		if now.Before(agent.RetryAt) {
			r.wakeBy(agent.RetryAt)
			continue
		}
		// The durable records are ordered yard first, then its partner.
		input, err := r.committedPlanningInput(ctx, ShipState{})
		if err != nil {
			return false, err
		}
		pending := int64(0)
		for _, member := range agents {
			if member.Region == agent.Region && member.HiredAt.IsZero() && !slices.Contains(r.stateCache.Factors, member.PortID) {
				pending++
			}
		}
		required, err := r.expansionRequired(ctx, input, agent.Region, pending*expansionAgentHireFeePence)
		if err != nil {
			return false, err
		}
		if changed, err := r.fundTreasury(ctx, input.Balances, required); changed || err != nil {
			return changed, err
		}
		if input.Balances[agent.Region] < required[agent.Region] {
			return false, r.holdExpected(ctx, "", "expansion partner waits for completed regional fee and reserve funding")
		}
		mission = &store.Mission{
			AccountID: r.account.ID, ShipID: "__treasury", Phase: "hire-factor",
			Destination: agent.PortID, Generation: uuid.NewString(),
		}
		data := missionData{Plan: Plan{Kind: "treasury"}, StartedAt: now, CostsKnown: true}
		return true, r.stage(ctx, mission, &data, ShipState{}, "hire-factor", "/v1/company/hire-factor", map[string]any{"port_id": agent.PortID})
	}
	savingsTarget, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return false, err
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	flagship := decode[americaFlagshipState](r.snapshot, "_america_flagship")
	for _, agent := range agents {
		if agent.PortID == "boston" && flagship.BuiltShipID == "" && (flagship.Pending || flagship.LoanID != "" || !flagship.RetryAt.IsZero()) {
			continue
		}
		if agent.HiredAt.IsZero() || !slices.Contains(r.stateCache.Factors, agent.PortID) {
			continue
		}
		dismissAt := expansionPairTime(agents, agent).Add(2 * time.Hour)
		if now.Before(dismissAt) {
			r.wakeBy(dismissAt)
			continue
		}
		references := []string{agent.PortID}
		if agent.Partner != "" {
			references = append(references, agent.Partner)
		}
		if slices.Contains(references, decode[string](r.snapshot, "_capacity_growth_port")) || slices.Contains(references, savingsTarget.PortID) {
			continue
		}
		needed := false
		for _, active := range missions {
			var intent missionData
			if err := json.Unmarshal(active.Data, &intent); err != nil {
				return false, err
			}
			shipID := active.ShipID
			if shipID == "__growth" {
				shipID = intent.BuiltShipID
			}
			ship, owned := r.ship(shipID)
			regionalShip := false
			if agent.Partner != "" && owned && ship.Ship.Status != "sunk" {
				for _, portID := range shipRegionPorts(ship) {
					location, found := findPort(r.stateCache.Catalog, portID)
					regionalShip = regionalShip || found && location.BankingRegion == agent.Region
				}
			}
			if agent.Partner == "" && owned && ship.Ship.Status != "sunk" && ship.Ship.CurrentPortID == agent.PortID {
				continue
			}
			activeMission := active.Phase != "done" && active.Phase != "settled" &&
				active.Phase != "maintenance_done" && active.Phase != "growth_rejected"
			for _, portID := range references {
				lastPlan := intent.Plan.Origin == portID || intent.Plan.Destination == portID
				nextPlan := intent.GrowthLane != nil && intent.GrowthLane.NextPlan != nil &&
					(intent.GrowthLane.NextPlan.Origin == portID || intent.GrowthLane.NextPlan.Destination == portID)
				if activeMission && (regionalShip && (lastPlan || nextPlan) ||
					lastPlan || active.Destination == portID || intent.BuildPort == portID ||
					intent.Capacity != nil && intent.Capacity.BuildPort == portID) {
					needed = true
					break
				}
			}
			if needed {
				break
			}
		}
		if needed {
			continue
		}
		evaluated := agent.EvaluatedAt
		if mate, paired := expansionMate(agents, agent); paired {
			evaluated = maxTime(evaluated, mate.EvaluatedAt)
		}
		unevaluated := !evaluated.After(expansionPairTime(agents, agent))
		if unevaluated || !r.ownedShipInRegion(agent.Region) {
			if unevaluated {
				r.markExpansionRetry(agent)
				r.markExpansionUnevaluated(agent)
			} else {
				r.markExpansionLoss(agent, "expansion pair has no active profitable plan or saved build")
			}
			if err := r.saveSnapshot(ctx); err != nil {
				return false, err
			}
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
	for i := range agents {
		if agents[i].PortID != port.ID {
			continue
		}
		if !agents[i].HiredAt.IsZero() {
			return nil
		}
		agents[i].HiredAt = time.Now().UTC()
		agents[i].RetryAt = time.Time{}
		if mate, paired := expansionMate(agents, agents[i]); !paired || mate.HiredAt.IsZero() {
			r.snapshot["_expansion_last_started_at"] = encode(maxTime(
				decode[time.Time](r.snapshot, "_expansion_last_started_at"), agents[i].HiredAt))
		}
		r.snapshot["_expansion_agents"] = encode(agents)
		return r.saveSnapshot(ctx)
	}
	agents = append(agents, expansionAgent{PortID: port.ID, Region: port.BankingRegion,
		CreatedAt: data.Action.CreatedAt, HiredAt: time.Now().UTC()})
	r.snapshot["_expansion_last_started_at"] = encode(maxTime(
		decode[time.Time](r.snapshot, "_expansion_last_started_at"), agents[len(agents)-1].HiredAt))
	r.snapshot["_expansion_agents"] = encode(agents)
	return r.saveSnapshot(ctx)
}

// A proven never-sent hire can wait without quarantining ships or charging
// another pair. The original two-hour exploration lifetime is unchanged.
func (r *Runner) deferExpansionHire(ctx context.Context, body []byte, now time.Time) (time.Time, error) {
	var request struct {
		PortID string `json:"port_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return time.Time{}, err
	}
	until := now.Add(15 * time.Minute / time.Duration(max(1, r.stateCache.Clock.Calendar.TimeScale)))
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	for _, agent := range agents {
		if agent.PortID != request.PortID {
			continue
		}
		if expires := expansionPairTime(agents, agent).Add(2 * time.Hour); expires.Before(until) {
			until = expires
		}
		for i := range agents {
			if agents[i].Region == agent.Region && agents[i].HiredAt.IsZero() {
				agents[i].RetryAt = maxTime(agents[i].RetryAt, until)
			}
		}
		r.snapshot["_expansion_agents"] = encode(agents)
		r.wakeBy(until)
		return until, r.saveSnapshot(ctx)
	}
	return until, nil
}

func (r *Runner) clearAwaitingExpansion(ctx context.Context, body []byte, recordLoss bool) error {
	var request struct {
		PortID string `json:"port_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	region := ""
	for _, agent := range agents {
		if agent.PortID == request.PortID {
			region = agent.Region
			break
		}
	}
	if region == "" {
		return nil
	}
	retries := decode[map[string]time.Time](r.snapshot, "_expansion_agent_retries")
	if retries == nil {
		retries = make(map[string]time.Time)
	}
	for _, agent := range agents {
		if agent.Region == region {
			at := agent.HiredAt
			if at.IsZero() {
				at = agent.CreatedAt
			}
			retries[region] = maxTime(retries[region], at)
			if !recordLoss {
				r.markExpansionUnevaluated(agent)
			}
		}
	}
	retained := slices.DeleteFunc(agents, func(agent expansionAgent) bool {
		return agent.Region == region && agent.HiredAt.IsZero()
	})
	if len(retained) == len(agents) {
		return nil
	}
	for i := range retained {
		if _, found := expansionMate(retained, retained[i]); !found {
			retained[i].Partner = ""
			retained[i].EvaluatedAt = time.Time{}
		}
	}
	if recordLoss {
		r.markExpansionLoss(expansionAgent{Region: region}, "expansion hire definitively rejected")
	}
	r.snapshot["_expansion_agents"] = encode(retained)
	r.snapshot["_expansion_agent_retries"] = encode(retries)
	return r.saveSnapshot(ctx)
}

func (r *Runner) expansionHireReady(ctx context.Context, body []byte) error {
	var request struct {
		PortID string `json:"port_id"`
	}
	if json.Unmarshal(body, &request) != nil || request.PortID == "" {
		return &expansionAdmissionError{reason: "expansion agent has an invalid immutable request"}
	}
	port, exists := findPort(r.stateCache.Catalog, request.PortID)
	if !exists || port.IsCove || port.Phase > r.stateCache.Clock.UnlockedPhase || !regionalWater(r.stateCache.Catalog, port) {
		return &expansionAdmissionError{reason: expansionAgentAdmissionReason}
	}
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	partnerHire := false
	for _, agent := range agents {
		if agent.PortID != port.ID || agent.Partner == "" || !agent.HiredAt.IsZero() {
			continue
		}
		if _, paired := expansionMate(agents, agent); paired &&
			!time.Now().Before(expansionPairTime(agents, agent).Add(2*time.Hour)) {
			return &expansionAdmissionError{
				reason: "expansion pair observation floor expired before its first send", retryOnly: true,
			}
		}
		for _, mate := range agents {
			if mate.PortID == agent.Partner && mate.Partner == agent.PortID &&
				!mate.HiredAt.IsZero() && slices.Contains(r.stateCache.Factors, mate.PortID) {
				partnerHire = true
			}
		}
	}
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return err
	}
	if target.PortID != "" && !partnerHire {
		return errors.New(capacitySavingsReason)
	}
	for _, dataset := range []string{"catalog", "ships", "factors", "vaults", "rates", "profile"} {
		if !r.fresh(dataset) {
			return fmt.Errorf("expansion agent requires current successful %s authority", dataset)
		}
	}
	if !port.HasShipyard && !partnerHire || slices.Contains(r.stateCache.Factors, port.ID) ||
		len(r.stateCache.Factors) >= 12 || !partnerHire && len(r.stateCache.Factors)+2 > 12 {
		return &expansionAdmissionError{reason: expansionAgentAdmissionReason}
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return err
	}
	required, err := r.expansionRequired(ctx, input, port.BankingRegion, expansionAgentHireFeePence)
	if err != nil {
		return err
	}
	if input.Balances[port.BankingRegion] < required[port.BankingRegion] {
		return errors.New("expansion hire requires its regional reserve plus fee")
	}
	if partnerHire {
		return nil
	}
	admission := expansionAdmission{yard: port, region: port.BankingRegion, input: input}
	for _, agent := range agents {
		if agent.PortID == port.ID {
			admission.partner, _ = findPort(r.stateCache.Catalog, agent.Partner)
			admission.probe = agent.Probe
			break
		}
	}
	var reason string
	var definitive bool
	if admission.probe {
		reason, definitive, err = r.expansionProbeAdmission(ctx, &admission)
	} else {
		reason, definitive, err = r.expansionBuildAdmission(ctx, &admission)
	}
	if err != nil || reason == "" {
		return err
	}
	if definitive {
		return &expansionAdmissionError{reason: reason}
	}
	return errors.New(reason)
}
