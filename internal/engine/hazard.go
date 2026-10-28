package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

const (
	hazardQuoteLimit         = 30
	hazardHullGameHours      = 6.0
	hazardLadenGameHours     = 30.0 * 24
	hazardPublishedGameHours = 3.0 * 30 * 24
	hazardUnavailable        = "Lloyd's hazard quote unavailable; hazard is not assumed zero"
	hazardQuoteSnapshotKey   = "_hazard_quotes"
)

// HazardQuote retains Lloyd's published per-passage expected-claim upper bound,
// not a probability inferred from a zone's storm rating. Month is deliberately
// part of the season identity: no unpublished seasonal formula is assumed.
type HazardQuote struct {
	ShipID            string           `json:"ship_id"`
	Origin            string           `json:"origin"`
	Destination       string           `json:"destination"`
	ShipClass         string           `json:"ship_class"`
	CannonsCount      int64            `json:"cannons_count"`
	Month             int              `json:"month"`
	Laden             bool             `json:"laden"`
	ExpectedLossBPS   int64            `json:"expected_loss_bps"`
	MaxInsurablePence int64            `json:"max_insurable_pence"`
	ObservedAt        time.Time        `json:"observed_at"`
	AttemptedAt       time.Time        `json:"attempted_at"`
	NetPiracyBPS      map[string]int64 `json:"net_piracy_bps"`
	Illegal           bool             `json:"illegal,omitempty"`
	Error             string           `json:"error,omitempty"`
	Deferred          bool             `json:"deferred,omitempty"`
}

func hazardHullKey(shipID, origin, destination string) string {
	return "hull|" + shipID + "|" + origin + "|" + destination
}

func hazardLadenKey(origin, destination, class string, cannons int64, month int) string {
	return "laden|" + routeKey(origin, destination, class) + "|" + strconv.FormatInt(cannons, 10) + "|" + strconv.Itoa(month)
}

func hazardAgeGameHours(now, observedAt time.Time, scale int64) (float64, bool) {
	if observedAt.IsZero() || now.Before(observedAt) || scale <= 0 {
		return 0, false
	}
	return now.Sub(observedAt).Hours() * float64(scale), true
}

func hazardCharge(pence, bps int64) (int64, bool) {
	if pence < 0 || bps < 0 {
		return 0, false
	}
	high, low := bits.Mul64(uint64(pence), uint64(bps))
	if high >= 10_000 {
		return 0, false
	}
	charge, remainder := bits.Div64(high, low, 10_000)
	if charge > math.MaxInt64 || charge == math.MaxInt64 && remainder != 0 {
		return 0, false
	}
	if remainder != 0 {
		charge++
	}
	return int64(charge), true
}

func hazardRoutePiracy(in PlanningInput, route Route) (map[string]int64, bool) {
	zones := make(map[string]bool)
	for _, zone := range in.Catalog.SeaZones {
		zones[zone.ID] = true
	}
	piracy := make(map[string]int64)
	for _, leg := range route.Legs {
		for _, node := range []string{leg.FromNode, leg.ToNode} {
			if !zones[node] {
				continue
			}
			found := false
			for _, zone := range in.Zones {
				if zone.SeaZoneID == node {
					piracy[node] = zone.NetPiracyBPS
					found = true
					break
				}
			}
			if !found {
				return nil, false
			}
		}
	}
	return piracy, true
}

func hazardQuoteCurrent(in PlanningInput, quote HazardQuote, route Route, laden bool) bool {
	age, validAge := hazardAgeGameHours(in.Now, quote.ObservedAt, in.Clock.Calendar.TimeScale)
	ttl := hazardHullGameHours
	if laden {
		ttl = hazardLadenGameHours
	}
	if !validAge || age > ttl || quote.Month != in.Clock.Calendar.Month || quote.Laden != laden ||
		quote.Origin != route.FromPort || quote.Destination != route.ToPort || quote.ShipClass != in.Ship.Ship.ShipClass ||
		quote.CannonsCount != in.Ship.Ship.CannonsCount {
		return false
	}
	return hazardPiracyCovers(in, quote, route, true)
}

func hazardPiracyCovers(in PlanningInput, quote HazardQuote, route Route, exact bool) bool {
	// Validate the retained zone fingerprint without allocating on every
	// whole-ton candidate evaluated by the planner.
	for _, leg := range route.Legs {
		for _, node := range []string{leg.FromNode, leg.ToNode} {
			isZone := false
			for _, zone := range in.Catalog.SeaZones {
				isZone = isZone || zone.ID == node
			}
			if !isZone {
				continue
			}
			expected, exists := quote.NetPiracyBPS[node]
			matched := false
			for _, zone := range in.Zones {
				if zone.SeaZoneID == node && exists &&
					(expected == zone.NetPiracyBPS || !exact && expected > zone.NetPiracyBPS) {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func planHazard(in PlanningInput, route Route) (int64, string, bool) {
	if !in.fresh("zones") {
		return 0, "", false
	}
	if in.Ship.Ship.ID != "growth-candidate" {
		if quote, source, exists := quotedLaneHazard(in, route); exists {
			return describeHazardQuote(in, quote, source, "Lloyd's expected-claim upper bound")
		}
		if quote := in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, route.FromPort, route.ToPort)]; quote.Illegal &&
			hazardQuoteCurrent(in, quote, route, false) {
			return 0, "", false
		}
	}
	var quote HazardQuote
	var source string
	exists := false
	for _, ship := range in.Fleet {
		if ship.Ship.Status == "sunk" || ship.Ship.ConvoyID != "" {
			continue
		}
		owner := in
		owner.Ship = ship
		sourceRoute, found := in.Routes[routeKey(route.FromPort, route.ToPort, ship.Ship.ShipClass)]
		if !found {
			continue
		}
		candidate, candidateSource, available := quotedLaneHazard(owner, sourceRoute)
		if !available || !hazardProxyEligible(in, candidate, route) {
			continue
		}
		if !exists || candidate.ExpectedLossBPS > quote.ExpectedLossBPS ||
			candidate.ExpectedLossBPS == quote.ExpectedLossBPS && candidate.ObservedAt.After(quote.ObservedAt) {
			quote, source, exists = candidate, candidateSource, true
		}
	}
	if exists {
		label := "Lloyd's expected-claim lane proxy (highest owned-hull quote)"
		if in.Ship.Ship.ID == "growth-candidate" && quote.ShipClass == in.Ship.Ship.ShipClass &&
			quote.CannonsCount == in.Ship.Ship.CannonsCount {
			label = "Lloyd's expected-claim upper bound"
		}
		return describeHazardQuote(in, quote, source, label)
	}
	// A failed refresh does not erase the last published bound. For at most
	// three game months, retain only quotes from the same winter/non-winter
	// class and piracy no lower than the current lane, explicitly as proxies.
	failed := false
	for _, candidate := range in.HazardQuotes {
		if candidate.Origin == route.FromPort && candidate.Destination == route.ToPort &&
			candidate.Error != "" && !candidate.Illegal && hazardQuoteOwned(in, candidate.ShipID) {
			failed = true
			break
		}
	}
	if !failed {
		return 0, "", false
	}
	for _, candidate := range in.HazardQuotes {
		age, valid := hazardAgeGameHours(in.Now, candidate.ObservedAt, in.Clock.Calendar.TimeScale)
		if candidate.Origin != route.FromPort || candidate.Destination != route.ToPort || candidate.Illegal ||
			candidate.ExpectedLossBPS < 0 || !valid || age > hazardPublishedGameHours ||
			!hazardSameStormSeason(candidate.Month, in.Clock.Calendar.Month) || !hazardProxyEligible(in, candidate, route) {
			continue
		}
		if !exists || candidate.ExpectedLossBPS > quote.ExpectedLossBPS ||
			candidate.ExpectedLossBPS == quote.ExpectedLossBPS && candidate.ObservedAt.After(quote.ObservedAt) {
			quote, exists = candidate, true
		}
	}
	if !exists {
		return 0, "", false
	}
	source = "hull-only"
	if quote.Laden {
		source = "laden"
	}
	return describeHazardQuote(in, quote, source, "Lloyd's stale published quote lane proxy (highest owned-hull quote)")
}

func hazardSameStormSeason(quotedMonth, passageMonth int) bool {
	if quotedMonth < 1 || quotedMonth > 12 || passageMonth < 1 || passageMonth > 12 {
		return false
	}
	// Published storm season is November through February, inclusive.
	return (quotedMonth >= 11 || quotedMonth <= 2) == (passageMonth >= 11 || passageMonth <= 2)
}

func hazardProxyEligible(in PlanningInput, quote HazardQuote, route Route) bool {
	if !hazardQuoteOwned(in, quote.ShipID) || quote.CannonsCount < 0 ||
		in.Ship.Ship.CannonsCount < quote.CannonsCount {
		return false
	}
	sourceHull, sourceOK := findHull(in.Catalog, quote.ShipClass)
	targetHull, targetOK := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	sourceRank, targetRank := classRank(sourceHull.HullCategory), classRank(targetHull.HullCategory)
	// Fewer guns and a lower-rated hull are conservative on the same water.
	// Do not use a better-protected source to price a weaker target.
	return sourceOK && targetOK && sourceRank > 0 && targetRank > 0 && sourceRank <= targetRank &&
		hazardPiracyCovers(in, quote, route, false)
}

func hazardQuoteOwned(in PlanningInput, shipID string) bool {
	if in.Ship.Ship.ID == shipID && in.Ship.Ship.Status != "sunk" && in.Ship.Ship.ConvoyID == "" {
		return true
	}
	for _, ship := range in.Fleet {
		if ship.Ship.ID == shipID && ship.Ship.Status != "sunk" && ship.Ship.ConvoyID == "" {
			return true
		}
	}
	return false
}

func describeHazardQuote(in PlanningInput, quote HazardQuote, source, label string) (int64, string, bool) {
	age, _ := hazardAgeGameHours(in.Now, quote.ObservedAt, in.Clock.Calendar.TimeScale)
	return quote.ExpectedLossBPS, fmt.Sprintf("%s, %s, %.2f gh old", label, source, age), true
}

func quotedLaneHazard(in PlanningInput, route Route) (HazardQuote, string, bool) {
	quote, exists := in.HazardQuotes[hazardHullKey(in.Ship.Ship.ID, route.FromPort, route.ToPort)]
	if !exists || quote.Error != "" || quote.Illegal || quote.ExpectedLossBPS < 0 || !hazardQuoteCurrent(in, quote, route, false) {
		return HazardQuote{}, "", false
	}
	source := "hull-only"
	laden := in.HazardQuotes[hazardLadenKey(route.FromPort, route.ToPort, in.Ship.Ship.ShipClass, in.Ship.Ship.CannonsCount, in.Clock.Calendar.Month)]
	if laden.Error == "" && !laden.Illegal && hazardQuoteCurrent(in, laden, route, true) && laden.ExpectedLossBPS > quote.ExpectedLossBPS {
		quote, source = laden, "laden"
	}
	return quote, source, true
}

func hazardStationary(ship ShipState) bool {
	return (ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") &&
		ship.Voyage == nil && ship.Ship.ConvoyID == "" && ship.Ship.CurrentPortID != ""
}

type hazardQuoteRequest struct {
	key     string
	ship    ShipState
	route   Route
	laden   bool
	insured int64
	plan    *Plan
}

func (r *Runner) refreshHazardQuotes(ctx context.Context, state AccountState) error {
	in := r.planningInputFrom(ShipState{}, state)
	quotes := decode[map[string]HazardQuote](r.snapshot, hazardQuoteSnapshotKey)
	if quotes == nil {
		quotes = make(map[string]HazardQuote)
	}
	if !in.fresh("ships") || !in.fresh("clock") || !in.fresh("zones") || !in.fresh("catalog") {
		return nil
	}
	queued := make(map[string]bool)
	var pending []hazardQuoteRequest
	priorities := make(map[string]*Plan)
	orderDirty := true
	queue := func(request hazardQuoteRequest) {
		if queued[request.key] {
			return
		}
		request.plan = priorities[hazardHullKey(request.ship.Ship.ID, request.route.FromPort, request.route.ToPort)]
		queued[request.key] = true
		pending = append(pending, request)
		orderDirty = true
	}
	queueLaden := func(ship ShipState, route Route, hullQuote HazardQuote) {
		if emptyCargo(ship) || hullQuote.MaxInsurablePence < 10_000 {
			return
		}
		key := hazardLadenKey(route.FromPort, route.ToPort, ship.Ship.ShipClass, ship.Ship.CannonsCount, in.Clock.Calendar.Month)
		local := in
		local.Ship = ship
		if quote := quotes[key]; quote.Error == "" && hazardQuoteCurrent(local, quote, route, true) {
			return
		}
		queue(hazardQuoteRequest{key: key, ship: ship, route: route, laden: true, insured: hullQuote.MaxInsurablePence})
	}
	for _, ship := range state.Ships {
		if !hazardStationary(ship) {
			continue
		}
		local := in
		local.Ship = ship
		start := len(pending)
		hull, exists := findHull(state.Catalog, ship.Ship.ShipClass)
		if !exists {
			continue
		}
		for _, port := range state.Catalog.Ports {
			if port.ID == ship.Ship.CurrentPortID || port.Phase > state.Clock.UnlockedPhase {
				continue
			}
			route, err := observedPlanningRoute(local, ship.Ship.CurrentPortID, port.ID, hull)
			if err != nil {
				continue
			}
			key := hazardHullKey(ship.Ship.ID, route.FromPort, route.ToPort)
			quote := quotes[key]
			if hazardQuoteCurrent(local, quote, route, false) && (quote.Error == "" || quote.Illegal) {
				if !quote.Illegal {
					queueLaden(ship, route, quote)
				}
				continue
			}
			queue(hazardQuoteRequest{key: key, ship: ship, route: route, insured: 10_000})
		}
		if len(pending) > start {
			// Only compute optimistic candidate upper bounds when quotes are
			// needed. They schedule requests, never authorize or persist trades.
			local.Contracts = taperedContracts(state.Contracts, state.Catalog, r.charterProceedsPence, ship.EffectiveHoldCapacityTons)
			local.pinnedHazard = &Economics{}
			plans := PlanVoyages(local)
			for index := range plans {
				plan := &plans[index]
				key := hazardHullKey(ship.Ship.ID, plan.Origin, plan.Destination)
				if plan.Accepted && priorities[key] == nil {
					priorities[key] = plan
				}
			}
			for index := start; index < len(pending); index++ {
				request := &pending[index]
				request.plan = priorities[hazardHullKey(ship.Ship.ID, request.route.FromPort, request.route.ToPort)]
			}
		}
	}
	for key, quote := range quotes {
		observedAt := quote.ObservedAt
		if observedAt.IsZero() {
			observedAt = quote.AttemptedAt
		}
		age, valid := hazardAgeGameHours(in.Now, observedAt, in.Clock.Calendar.TimeScale)
		// Retain published observations for the labelled one-season outage
		// proxy; normal direct pricing still enforces its shorter freshness.
		if !queued[key] && (!valid || age > hazardPublishedGameHours) {
			delete(quotes, key)
		}
	}
	for requests := 0; len(pending) > 0 && requests < hazardQuoteLimit; requests++ {
		if orderDirty {
			sort.Slice(pending, func(i, j int) bool {
				left, right := pending[i], pending[j]
				leftQuote, rightQuote := quotes[left.key], quotes[right.key]
				// A capped pair moves ahead next pass, even if better pairs fail.
				if leftQuote.Deferred != rightQuote.Deferred {
					return leftQuote.Deferred
				}
				if (left.plan != nil) != (right.plan != nil) {
					return left.plan != nil
				}
				// Price each candidate's primary bound before spending the cap on
				// optional laden refinements, including on the deferred pass.
				if left.laden != right.laden {
					return !left.laden
				}
				if left.plan != nil {
					a, b := &left.plan.Economics, &right.plan.Economics
					if a.NetPencePerGameDay != b.NetPencePerGameDay {
						return a.NetPencePerGameDay > b.NetPencePerGameDay
					}
					if a.ExpectedNetPence != b.ExpectedNetPence {
						return a.ExpectedNetPence > b.ExpectedNetPence
					}
				}
				if !leftQuote.AttemptedAt.Equal(rightQuote.AttemptedAt) {
					return leftQuote.AttemptedAt.Before(rightQuote.AttemptedAt)
				}
				return left.key < right.key
			})
			orderDirty = false
		}
		request := pending[0]
		pending = pending[1:]
		local := in
		local.Ship = request.ship
		piracy, complete := hazardRoutePiracy(local, request.route)
		if !complete {
			continue
		}
		quote := HazardQuote{
			ShipID: request.ship.Ship.ID, Origin: request.route.FromPort, Destination: request.route.ToPort,
			ShipClass: request.ship.Ship.ShipClass, CannonsCount: request.ship.Ship.CannonsCount,
			Month: in.Clock.Calendar.Month, Laden: request.laden, NetPiracyBPS: piracy, AttemptedAt: time.Now(),
		}
		var response struct {
			ExpectedLossBPS   *int64   `json:"lloyds_expected_loss_bps"`
			MaxInsurablePence int64    `json:"max_insurable_pence"`
			PricedConsorts    []string `json:"priced_consort_ship_ids"`
		}
		query := url.Values{
			"ship_id": {request.ship.Ship.ID}, "dest_port_id": {request.route.ToPort},
			"insured_value_pence": {strconv.FormatInt(request.insured, 10)},
		}
		err := r.api.GetOnce(ctx, "/v1/insurance/quote?"+query.Encode(), &response)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil && (response.ExpectedLossBPS == nil || *response.ExpectedLossBPS < 0 ||
			response.MaxInsurablePence < request.insured || len(response.PricedConsorts) != 0) {
			err = errors.New("Lloyd's hazard response lacks an independent ship's valid published bound")
		}
		if err != nil {
			attempt := quote
			previous := quotes[request.key]
			if !previous.ObservedAt.IsZero() {
				attemptedAt := quote.AttemptedAt
				quote = previous
				quote.AttemptedAt, quote.Deferred = attemptedAt, false
			}
			quote.Error = err.Error()
			quote.Illegal = false
			var upstream *gameapi.APIError
			if errors.As(err, &upstream) {
				quote.Illegal = upstream.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(upstream.Detail), "no open route")
				if quote.Illegal {
					quote = attempt
					quote.Illegal, quote.Error, quote.ObservedAt = true, err.Error(), time.Now()
				}
				if upstream.Status == http.StatusTooManyRequests {
					quotes[request.key] = quote
					break
				}
			}
		} else {
			quote.ObservedAt = time.Now()
			quote.ExpectedLossBPS = *response.ExpectedLossBPS
			quote.MaxInsurablePence = response.MaxInsurablePence
			if request.laden {
				hull := quotes[hazardHullKey(request.ship.Ship.ID, request.route.FromPort, request.route.ToPort)]
				if entry := r.log.Check(zap.InfoLevel, "Lloyd's voyage hazard bounds"); entry != nil {
					entry.Write(r.scrubOutcomeFields(ctx, []zap.Field{
						zap.Int64("hull_only_bps", hull.ExpectedLossBPS), zap.Int64("laden_bps", quote.ExpectedLossBPS),
					}, []outcomeText{
						{field: "ship_id", value: quote.ShipID}, {field: "origin", value: quote.Origin},
						{field: "destination", value: quote.Destination},
					})...)
				}
			} else {
				queueLaden(request.ship, request.route, quote)
			}
		}
		quotes[request.key] = quote
	}
	for _, request := range pending {
		quote := quotes[request.key]
		quote.Deferred = true
		quotes[request.key] = quote
	}
	r.snapshot[hazardQuoteSnapshotKey] = encode(quotes)
	return nil
}
