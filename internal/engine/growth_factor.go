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
)

type expansionAgent struct {
	PortID      string    `json:"port_id"`
	Partner     string    `json:"partner_port_id"`
	Region      string    `json:"region"`
	CreatedAt   time.Time `json:"created_at"`
	HiredAt     time.Time `json:"hired_at"`
	EvaluatedAt time.Time `json:"evaluated_at,omitempty"`
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
}

// Regional and global exploration starts are separated by at least 24 real hours.
const expansionRegionRetry = 24 * time.Hour

const expansionObservationRetry = 5 * time.Minute

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
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
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
	yard       Port
	partner    Port
	region     string
	score      int64
	build      GrowthDecision
	quote      YardQuote
	fresh      bool
	input      PlanningInput
	required   map[string]int64
	needed     int64
	retryUntil time.Time
}

func (r *Runner) expansionRetryUntil(region string) time.Time {
	outcome := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")[region]
	until := maxTime(outcome.RetryAt, outcome.UnevaluatedRetryAt)
	at := decode[map[string]time.Time](r.snapshot, "_expansion_agent_retries")[region]
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
	candidates := expansionAgentYards(r.stateCache, r.planningInput(ShipState{}))
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
	sortExpansionPairs(eligible)
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
	pass, reason, err := r.expansionPassEvidence(ctx, input)
	if err != nil || reason != "" {
		return admission, reason, err
	}
	var firstRejected expansionAdmission
	firstReason := ""
	for _, pair := range eligible {
		candidate := expansionAdmission{
			yard: pair.yard, partner: pair.partner, region: pair.yard.BankingRegion,
			score: pair.score, input: input,
		}
		reason, _, err := r.expansionBuildAdmissionForPass(ctx, &candidate, &pass)
		if err != nil {
			return candidate, "", err
		}
		if reason == "" {
			return candidate, "", nil
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
	ownFees := expansionFeeCommitments(r.stateCache, r.snapshot)[region]
	required[region] = max(0, required[region]-ownFees)
	for candidate, protected := range input.ReserveByRegion {
		if candidate == region {
			protected = max(0, protected-ownFees)
		}
		required[candidate] = max(required[candidate], min(protected, input.Balances[candidate]))
	}
	required[region] = growthCashTotal(max(required[region], r.opts.ReservePence), fees)
	return required, nil
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
	ownFees := expansionFeeCommitments(r.stateCache, r.snapshot)[admission.region]
	if ownFees > 0 {
		admission.input.ReserveByRegion = maps.Clone(admission.input.ReserveByRegion)
		admission.input.ReserveByRegion[admission.region] = max(0, admission.input.ReserveByRegion[admission.region]-ownFees)
	}
	pass, reason, err := r.expansionPassEvidence(ctx, admission.input)
	if err != nil || reason != "" {
		return reason, false, err
	}
	pass.growth.required[admission.region] = max(0, pass.growth.required[admission.region]-ownFees)
	return r.expansionBuildAdmissionForPass(ctx, admission, &pass)
}

func (r *Runner) expansionBuildAdmissionForPass(ctx context.Context, admission *expansionAdmission, pass *expansionEvidence) (string, bool, error) {
	input := &admission.input
	var err error
	admission.build, admission.quote, admission.fresh, err = r.expansionBuildEstimateForRegion(ctx,
		admission.yard, admission.partner, pass.capacity, pass.growth.asOf, pass.rates[admission.region])
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
	admission.required = maps.Clone(pass.growth.required)
	for region, protected := range input.ReserveByRegion {
		admission.required[region] = max(admission.required[region], min(protected, input.Balances[region]))
	}
	admission.required[admission.region] = growthCashTotal(
		max(admission.required[admission.region], r.opts.ReservePence), 2*expansionAgentHireFeePence)
	_, fees, _, funded := measuredFunding(input.Balances, admission.required, input.Rates)
	admission.needed = growthCashTotal(admission.needed, fees)
	if !funded || pass.growth.available < admission.needed {
		return "expansion agent must preserve the hull capital, capacity working cash and funding fees", false, nil
	}
	return "", false, nil
}

func (r *Runner) markExpansionLoss(agent expansionAgent, reason string) {
	outcomes := decode[map[string]expansionOutcome](r.snapshot, "_expansion_agent_outcomes")
	if outcomes == nil {
		outcomes = make(map[string]expansionOutcome)
	}
	outcome := outcomes[agent.Region]
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
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) considerExpansionAgent(ctx context.Context) (bool, error) {
	if !r.opts.Execute {
		return false, nil
	}
	admission, reason, err := r.expansionAdmission(ctx)
	if err != nil {
		return false, err
	}
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
	if err := r.decision(ctx, "", "expansion_agent", "an admissible estimated build supports the paid expansion pair", map[string]any{
		"port_id": yard.ID, "partner_port_id": partner.ID, "region": yard.BankingRegion, "score_pence": admission.score,
		"hull_class":                   admission.quote.ShipClass,
		"estimated_hull_capital_pence": growthCapital(admission.build), "estimated_quote_port": admission.quote.PortID,
		"estimated_hull_capital_stale": !admission.fresh, "competition": competitionAt(admission.input.Competition, yard.BankingRegion),
		"estimate": admission.build.ExpansionEstimate,
	}); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	agents := decode[[]expansionAgent](r.snapshot, "_expansion_agents")
	agents = slices.DeleteFunc(agents, func(agent expansionAgent) bool {
		return agent.Region == yard.BankingRegion
	})
	agents = append(agents,
		expansionAgent{PortID: yard.ID, Partner: partner.ID, Region: yard.BankingRegion, CreatedAt: now},
		expansionAgent{PortID: partner.ID, Partner: yard.ID, Region: yard.BankingRegion, CreatedAt: now},
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
	if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
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
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
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

func (r *Runner) holdExpansionGrowth(ctx context.Context, canExplore bool) (bool, bool, error) {
	if !r.opts.Execute {
		return false, false, nil
	}
	if r.expansionAwaitingEvaluation() {
		return true, false, r.holdExpected(ctx, "", "growth waits for the expansion pair's fresh capacity evaluation")
	}
	if !canExplore {
		return false, false, nil
	}
	changed, err := r.considerExpansionAgent(ctx)
	return changed, changed, err
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
	for _, agent := range agents {
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
			if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
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
		if mate, paired := expansionMate(agents, agents[i]); !paired || mate.HiredAt.IsZero() {
			r.snapshot["_expansion_last_started_at"] = encode(maxTime(
				decode[time.Time](r.snapshot, "_expansion_last_started_at"), agents[i].HiredAt))
		}
		r.snapshot["_expansion_agents"] = encode(agents)
		return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
	}
	agents = append(agents, expansionAgent{PortID: port.ID, Region: port.BankingRegion,
		CreatedAt: data.Action.CreatedAt, HiredAt: time.Now().UTC()})
	r.snapshot["_expansion_last_started_at"] = encode(maxTime(
		decode[time.Time](r.snapshot, "_expansion_last_started_at"), agents[len(agents)-1].HiredAt))
	r.snapshot["_expansion_agents"] = encode(agents)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
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
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
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
			break
		}
	}
	reason, definitive, err := r.expansionBuildAdmission(ctx, &admission)
	if err != nil || reason == "" {
		return err
	}
	if definitive {
		return &expansionAdmissionError{reason: reason}
	}
	return errors.New(reason)
}
