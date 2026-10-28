package engine

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

// CompetitionEvidence counts observed name identities, not a census of hulls.
// Unknown coverage uses the midpoint of observed regional counts for ranking.
type CompetitionEvidence struct {
	Region           string  `json:"region"`
	Coverage         string  `json:"coverage"`
	ForeignCompanies *int    `json:"foreign_companies"`
	ForeignShips     *int    `json:"foreign_ships"`
	PreferenceShips  float64 `json:"preference_ships"`
}

func competitionAt(summary *store.CompetitionSummary, region string) CompetitionEvidence {
	result := CompetitionEvidence{Region: region, Coverage: "unobserved"}
	if summary == nil {
		result.Coverage = "unavailable"
		return result
	}
	var low, high int
	observed := false
	for _, item := range summary.Regions {
		if item.ID == region {
			result.Coverage = item.Coverage
			result.ForeignShips = item.ForeignShips
			result.ForeignCompanies = item.ForeignCompanies
		}
		if item.Coverage != "partial" || item.ForeignShips == nil {
			continue
		}
		count := *item.ForeignShips
		if !observed || count < low {
			low = count
		}
		if !observed || count > high {
			high = count
		}
		observed = true
		if item.ID == region {
			result.PreferenceShips = float64(count)
		}
	}
	if result.Coverage != "partial" || result.ForeignShips == nil {
		result.PreferenceShips = (float64(low) + float64(high)) / 2
	}
	return result
}

func (in PlanningInput) destinationCompetition(destination string) CompetitionEvidence {
	port, _ := findPort(in.Catalog, destination)
	return competitionAt(in.Competition, port.BankingRegion)
}

func economicallyComparable(value, best float64) bool {
	return best > 0 && value >= best*0.95
}

// rankPlans anchors the tolerance to the economic best, not a pairwise fuzzy
// comparison (which could create sorting cycles or drift toward worse returns).
func rankPlans(plans []Plan, input PlanningInput) {
	var best float64
	for i := range plans {
		plans[i].Competition = input.destinationCompetition(plans[i].Destination)
		plans[i].competitionComparable = false
		if plans[i].Accepted && plans[i].Economics.GameDays > 0 {
			best = max(best, plans[i].Economics.NetPencePerGameDay)
		}
	}
	for i := range plans {
		plans[i].competitionComparable = plans[i].Accepted && plans[i].Economics.GameDays > 0 &&
			economicallyComparable(plans[i].Economics.NetPencePerGameDay, best)
	}
	sort.SliceStable(plans, func(i, j int) bool { return betterPlan(plans[i], plans[j]) })
	for i := range plans {
		if !plans[i].competitionComparable {
			plans[i].Competition = CompetitionEvidence{}
		}
	}
}

type capacityCandidate struct {
	port     Port
	hull     Hull
	decision GrowthDecision
	required map[string]int64
}

func capacityReturn(decision GrowthDecision, depth bool) float64 {
	capital := growthCapital(decision)
	if capital <= 0 {
		return 0
	}
	net := decision.IncrementalNetPencePerGameDay
	if depth {
		net = decision.DepthNetPencePerGameDay
	}
	return net / float64(capital)
}

func markCapacityCompetition(candidates []capacityCandidate) {
	var bestCash, bestDepth float64
	for _, candidate := range candidates {
		if candidate.decision.Accepted {
			bestCash = max(bestCash, capacityReturn(candidate.decision, false))
		}
		if candidate.decision.EconomicallyViable && !candidate.decision.Accepted {
			bestDepth = max(bestDepth, capacityReturn(candidate.decision, true))
		}
	}
	for i := range candidates {
		decision := &candidates[i].decision
		decision.competitionComparable = decision.Accepted && economicallyComparable(capacityReturn(*decision, false), bestCash)
		decision.depthCompetitionComparable = !decision.Accepted && decision.EconomicallyViable &&
			economicallyComparable(capacityReturn(*decision, true), bestDepth)
	}
}

const competitionCacheTTL = 5 * time.Minute

type competitionCoverage struct {
	Since   time.Time `json:"since"`
	Through time.Time `json:"through"`
}

func (r *Runner) observeCompetitionCoverage() {
	if r.competitionCoverage == nil {
		r.competitionCoverage = decode[map[string]competitionCoverage](r.snapshot, "_competition_coverage")
		if r.competitionCoverage == nil {
			r.competitionCoverage = make(map[string]competitionCoverage)
		}
	}
	observed := make(map[string]time.Time)
	for _, port := range r.stateCache.Catalog.Ports {
		meta := r.meta["ports/"+port.ID]
		if r.stateCache.Ports[port.ID].Visibility != "quay" || !r.fresh("ports/"+port.ID) {
			continue
		}
		observed[port.BankingRegion] = maxTime(observed[port.BankingRegion], meta.UpdatedAt)
	}
	for region, through := range observed {
		coverage := r.competitionCoverage[region]
		if coverage.Since.IsZero() || through.Sub(coverage.Through) > 2*time.Minute || through.Before(coverage.Through) {
			coverage.Since = through
		}
		coverage.Through = through
		r.competitionCoverage[region] = coverage
	}
	for region := range r.competitionCoverage {
		if observed[region].IsZero() {
			delete(r.competitionCoverage, region)
		}
	}
	r.snapshot["_competition_coverage"] = encode(r.competitionCoverage)
}

func (r *Runner) refreshCompetition(ctx context.Context) {
	now := time.Now()
	r.observeCompetitionCoverage()
	if now.Before(r.competitionNextLoad) {
		return
	}
	r.competitionNextLoad = now.Add(competitionCacheTTL)
	r.competition = nil
	input, err := r.db.CompetitionActivityInput(ctx, r.account.ID, now, r.snapshot["profile"])
	if err == nil {
		input.Ports, err = store.CompetitionPorts(r.snapshot["catalog"], r.snapshot["ports"], encode(r.meta), now)
	}
	if err == nil && !input.OwnCompaniesComplete {
		err = errors.New("competition own-company names are incomplete")
	}
	if err != nil {
		r.log.Warn("competition signal unavailable; economic planning continues", zap.Error(err))
		return
	}
	summary := store.AggregateCompetition(input)
	for i := range summary.Regions {
		region := &summary.Regions[i]
		coverage := r.competitionCoverage[region.ID]
		// A brief arrival cannot be compared with a region watched for the
		// full identity window. Poll gaps restart this conservative coverage.
		if region.Coverage == "partial" && coverage.Through.Sub(coverage.Since) < store.CompetitionWindow {
			region.Coverage = "short_coverage"
		}
	}
	r.competition = &summary
}

func competitionFingerprintDetails(details any) any {
	switch value := details.(type) {
	case Plan:
		value.Competition = CompetitionEvidence{}
		if value.Kind == "relocation" {
			value.ExpectedArrival = time.Time{}
		}
		return value
	case GrowthDecision:
		value.Competition = CompetitionEvidence{}
		value.DepthCompetition = CompetitionEvidence{}
		return value
	case map[string]any:
		if _, hasCompetition := value["competition"]; !hasCompetition {
			if _, hasDepth := value["depth_competition"]; !hasDepth {
				return details
			}
		}
		result := make(map[string]any, len(value))
		for key, item := range value {
			if key != "competition" && key != "depth_competition" {
				result[key] = item
			}
		}
		return result
	default:
		return details
	}
}
