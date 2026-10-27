package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const marqueQuoteInterval = 30 * time.Minute

type MarqueQuote struct {
	SeaZoneID             string  `json:"sea_zone_id"`
	AddedPiracyBPS        int64   `json:"added_piracy_bps"`
	DurationGameHours     int64   `json:"duration_game_hours"`
	CostPence             int64   `json:"cost_pence"`
	ExpectedNetPrizePence int64   `json:"expected_net_prize_pence"`
	LadenPassagesPerDay   float64 `json:"laden_passages_per_day"`
	MarqueBPSInForce      int64   `json:"marque_bps_in_force"`
	FloorApplied          bool    `json:"floor_applied"`
}

func (quote *MarqueQuote) UnmarshalJSON(data []byte) error {
	type wireQuote MarqueQuote
	var decoded wireQuote
	wire := struct {
		*wireQuote
		ExpectedPrize *int64   `json:"expected_net_prize_pence"`
		Passages      *float64 `json:"laden_passages_per_day"`
		InForceBPS    *int64   `json:"marque_bps_in_force"`
		Floor         *bool    `json:"floor_applied"`
	}{wireQuote: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.ExpectedPrize == nil || wire.Passages == nil || wire.InForceBPS == nil || wire.Floor == nil {
		return errors.New("marque quote omits required traffic, prize or pricing evidence")
	}
	decoded.ExpectedNetPrizePence, decoded.LadenPassagesPerDay = *wire.ExpectedPrize, *wire.Passages
	decoded.MarqueBPSInForce, decoded.FloorApplied = *wire.InForceBPS, *wire.Floor
	value := MarqueQuote(decoded)
	if !validMarqueQuote(value, value.SeaZoneID) {
		return errors.New("marque quote has invalid zone, term or monetary fields")
	}
	*quote = value
	return nil
}

type MarqueInput struct {
	Quote             MarqueQuote
	Exposed           bool
	InForceCostPence  int64
	LiquidPence       int64
	ExpiredCount      int
	ExpiredPrizePence int64
	ExpiredCostPence  int64
}

type MarqueDecision struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
}

func EvaluateMarque(in MarqueInput) MarqueDecision {
	switch {
	case in.Exposed:
		return MarqueDecision{Reason: "zone exposes our fleet or an underwritten voyage to our own privateers"}
	case in.Quote.FloorApplied:
		return MarqueDecision{Reason: "the price floor, not measured traffic, sets this quote"}
	case in.Quote.LadenPassagesPerDay < 1:
		return MarqueDecision{Reason: "fewer than one laden passage per game day was observed"}
	case in.Quote.MarqueBPSInForce+in.Quote.AddedPiracyBPS > 5_000:
		return MarqueDecision{Reason: "the new marque would exceed the zone's 5,000 bps cap"}
	case in.Quote.ExpectedNetPrizePence*10 < in.Quote.CostPence*14:
		return MarqueDecision{Reason: "expected prize after the Admiral's tenth is below 1.4 times cost"}
	case in.Quote.CostPence*20 > in.LiquidPence:
		return MarqueDecision{Reason: "one marque would spend more than five percent of liquid funds"}
	case (in.InForceCostPence+in.Quote.CostPence)*10 > in.LiquidPence:
		return MarqueDecision{Reason: "in-force marques would cost more than ten percent of liquid funds"}
	case in.ExpiredCount >= 3 && in.ExpiredPrizePence*10 < in.ExpiredCostPence*8:
		return MarqueDecision{Reason: "at least three expired marques returned less than eighty percent of cost"}
	default:
		return MarqueDecision{Accepted: true, Reason: "unexposed measured traffic supports the expected prize within liquidity and realized-return limits"}
	}
}

type letterOfMarque struct {
	ID               string    `json:"id"`
	SeaZoneID        string    `json:"sea_zone_id"`
	SponsorCompanyID string    `json:"sponsor_company_id"`
	BankingRegion    string    `json:"banking_region"`
	AddedPiracyBPS   int64     `json:"added_piracy_bps"`
	CostPence        int64     `json:"cost_pence"`
	PrizeEarnedPence int64     `json:"prize_earned_pence"`
	ExpiresAt        time.Time `json:"expires_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (marque *letterOfMarque) UnmarshalJSON(data []byte) error {
	type wireMarque letterOfMarque
	var decoded wireMarque
	wire := struct {
		*wireMarque
		Cost  *int64 `json:"cost_pence"`
		Prize *int64 `json:"prize_earned_pence"`
	}{wireMarque: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.Cost == nil || wire.Prize == nil {
		return errors.New("marque omits required cost or prize evidence")
	}
	decoded.CostPence, decoded.PrizeEarnedPence = *wire.Cost, *wire.Prize
	value := letterOfMarque(decoded)
	if !validMarque(value) {
		return errors.New("marque has invalid identity, money or term fields")
	}
	*marque = value
	return nil
}

type marqueOutcome struct {
	ID         string `json:"id"`
	CostPence  int64  `json:"cost_pence"`
	PrizePence int64  `json:"prize_pence"`
}

type marqueIntent struct {
	Quote         MarqueQuote `json:"quote"`
	BankingRegion string      `json:"banking_region"`
	BeforeIDs     []string    `json:"before_ids"`
	NotBefore     time.Time   `json:"not_before"`
	IssuedID      string      `json:"issued_id,omitempty"`
	ExpiresAt     time.Time   `json:"expires_at,omitempty"`
}

type marquePending struct {
	SeaZoneID     string    `json:"sea_zone_id"`
	BankingRegion string    `json:"banking_region"`
	CostPence     int64     `json:"cost_pence"`
	IssuedID      string    `json:"issued_id,omitempty"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
}

func validMarqueQuote(quote MarqueQuote, zone string) bool {
	return quote.SeaZoneID == zone && zone != "" && quote.AddedPiracyBPS == 500 && quote.DurationGameHours == 60 &&
		quote.CostPence > 0 && quote.CostPence <= maximumPence && quote.ExpectedNetPrizePence >= 0 && quote.ExpectedNetPrizePence <= maximumPence &&
		quote.LadenPassagesPerDay >= 0 && !math.IsInf(quote.LadenPassagesPerDay, 0) && !math.IsNaN(quote.LadenPassagesPerDay) &&
		quote.MarqueBPSInForce >= 0 && quote.MarqueBPSInForce <= 5_000
}

func validMarque(marque letterOfMarque) bool {
	return uuid.Validate(marque.ID) == nil && marque.SeaZoneID != "" && marque.AddedPiracyBPS >= 50 && marque.AddedPiracyBPS <= 1_500 &&
		marque.CostPence >= 0 && marque.CostPence <= maximumPence && marque.PrizeEarnedPence >= 0 && marque.PrizeEarnedPence <= maximumPence &&
		!marque.CreatedAt.IsZero() && marque.ExpiresAt.After(marque.CreatedAt) && !marque.UpdatedAt.Before(marque.CreatedAt)
}

func marqueQuoteFresh(meta DatasetMeta, now time.Time) bool {
	// These quotes deliberately span capture generations and have a real-time TTL.
	return meta.Complete && !meta.Stale && meta.Error == "" && !meta.UpdatedAt.IsZero() &&
		!meta.UpdatedAt.After(now.Add(time.Second)) && now.Sub(meta.UpdatedAt) <= marqueQuoteInterval
}

func (r *Runner) refreshMarqueQuotes(ctx context.Context, catalog Catalog) error {
	quotes := decode[map[string]json.RawMessage](r.snapshot, "marque_quotes")
	if quotes == nil {
		quotes = make(map[string]json.RawMessage)
	}
	now := time.Now()
	var tasks []fetchTask
	zones := make(map[string]bool, len(catalog.SeaZones))
	for _, zone := range catalog.SeaZones {
		zones[zone.ID] = true
		meta := r.meta["marque_quotes/"+zone.ID]
		if now.Sub(maxTime(meta.AttemptedAt, meta.UpdatedAt)) < marqueQuoteInterval {
			continue
		}
		tasks = append(tasks, fetchTask{key: "marque_quotes/" + zone.ID,
			path: "/v1/fleet/marque-quote?sea_zone_id=" + url.QueryEscape(zone.ID) + "&added_piracy_bps=500&duration_game_hours=60"})
	}
	if err := r.captureWave(ctx, tasks, map[string]map[string]json.RawMessage{"marque_quotes": quotes}); err != nil {
		return err
	}
	meta := DatasetMeta{UpdatedAt: now, AttemptedAt: now, Generation: r.generation, Complete: true}
	for zone := range quotes {
		if !zones[zone] {
			delete(quotes, zone)
		}
	}
	for key := range r.meta {
		if zone, child := strings.CutPrefix(key, "marque_quotes/"); child && !zones[zone] {
			delete(r.meta, key)
		}
	}
	for zone := range zones {
		if !marqueQuoteFresh(r.meta["marque_quotes/"+zone], time.Now()) || len(quotes[zone]) == 0 {
			meta.Complete = false
			meta.Stale = true
			meta.Error = "one or more marque quotes are unavailable; see per-zone metadata"
		}
	}
	r.snapshot["marque_quotes"] = encode(quotes)
	r.meta["marque_quotes"] = meta
	return nil
}

func marqueMissionPlans(state accountState, ship ShipState, mission *store.Mission, data *missionData) []Plan {
	current := ship.Ship.CurrentPortID
	atSea := ship.Ship.Status == "sailing" || ship.Voyage != nil
	if atSea {
		current = ""
	}
	if current == "" && len(ship.RouteLegs) > 0 {
		arrival := ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
		if _, exists := findPort(state.Catalog, arrival); exists {
			current = arrival
		}
	}
	routes := []Plan{{Origin: current, Destination: mission.Destination}}
	if !atSea {
		routes = append(routes, data.Plan)
	}
	if !data.ReturnJourney && !data.FinishAtDestination && data.Plan.Kind != "recovery" {
		routes = append(routes, Plan{Origin: data.Plan.Destination, Destination: data.Plan.Origin})
	}
	from := current
	if mission.Destination != "" {
		from = mission.Destination
	}
	for _, freight := range freightStops(data.Freight) {
		routes = append(routes, Plan{Origin: from, Destination: freight.Destination})
		from = freight.Destination
	}
	if data.Relocation != nil {
		routes = append(routes, Plan{Origin: data.Plan.Destination, Destination: data.Relocation.TargetDestination})
	}
	if data.GrowthLane != nil && data.GrowthLane.NextPlan != nil {
		routes = append(routes, *data.GrowthLane.NextPlan)
	}
	return routes
}

// Exposure uses actual legs and persisted destinations, never candidate plans.
// The policy board omits our permanent slice ownership, so use frozen positions.
func marqueExposure(state accountState, plans map[string][]Plan, positions []store.UnderwritingPosition) (map[string]bool, error) {
	exposed := make(map[string]bool)
	addNode := func(node string) {
		for _, zone := range state.Catalog.SeaZones {
			if node == zone.ID {
				exposed[zone.ID] = true
			}
		}
		if port, exists := findPort(state.Catalog, node); exists && port.SeaZoneID != "" {
			exposed[port.SeaZoneID] = true
		}
	}
	addLegs := func(legs []RouteLeg) {
		for _, leg := range legs {
			addNode(leg.FromNode)
			addNode(leg.ToNode)
		}
	}
	addNode(state.HomePortID)
	for _, ship := range state.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		addNode(ship.Ship.CurrentPortID)
		addLegs(ship.RouteLegs)
		atSea := ship.Ship.Status == "sailing" || ship.Voyage != nil
		if atSea && len(ship.RouteLegs) == 0 {
			return nil, fmt.Errorf("marque exposure lacks the current voyage legs for ship %s", ship.Ship.ID)
		}
		for _, plan := range plans[ship.Ship.ID] {
			from := plan.Origin
			if from == "" {
				if !atSea {
					from = ship.Ship.CurrentPortID
				}
				if from == "" && len(ship.RouteLegs) > 0 {
					arrival := ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
					if _, exists := findPort(state.Catalog, arrival); exists {
						from = arrival
					}
				}
			}
			if plan.Destination == "" || from == plan.Destination {
				continue
			}
			if from == "" {
				return nil, fmt.Errorf("marque exposure cannot identify the arrival port for ship %s's committed route to %s", ship.Ship.ID, plan.Destination)
			}
			addNode(from)
			addNode(plan.Destination)
			route, known := state.Routes[routeKey(from, plan.Destination, ship.Ship.ShipClass)]
			if !known || len(route.Legs) == 0 {
				return nil, fmt.Errorf("marque exposure lacks the committed route from %s to %s for ship %s", from, plan.Destination, ship.Ship.ID)
			}
			addLegs(route.Legs)
		}
	}
	for _, position := range positions {
		if position.ClosedAt != nil {
			continue
		}
		var policy InsurancePolicy
		if json.Unmarshal(position.FrozenPolicy, &policy) != nil || policy.OriginPortID == "" || policy.DestPortID == "" {
			return nil, errors.New("marque exposure has invalid permanent underwriting route terms")
		}
		addNode(policy.OriginPortID)
		addNode(policy.DestPortID)
		known := false
		for _, route := range state.Routes {
			if route.FromPort == policy.OriginPortID && route.ToPort == policy.DestPortID && len(route.Legs) > 0 {
				addLegs(route.Legs)
				known = true
			}
		}
		if !known {
			return nil, errors.New("marque exposure lacks an underwritten voyage's origin-destination route")
		}
	}
	return exposed, nil
}

func mergeMarqueOutcomes(outcomes map[string]marqueOutcome, marques []letterOfMarque, now time.Time) {
	for _, marque := range marques {
		if !now.Before(marque.ExpiresAt) {
			outcomes[marque.ID] = marqueOutcome{ID: marque.ID, CostPence: marque.CostPence, PrizePence: marque.PrizeEarnedPence}
		}
	}
}

func (r *Runner) recordMarques(ctx context.Context) error {
	outcomes := make(map[string]marqueOutcome)
	if raw := r.snapshot["_marque_outcomes"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &outcomes); err != nil || outcomes == nil {
			return errors.New("permanent marque outcomes are invalid; preserve history before adding risk")
		}
	}
	now, _, validClock := r.serverBounds()
	marques := decode[[]letterOfMarque](r.snapshot, "marques")
	if validClock && r.fresh("marques") {
		mergeMarqueOutcomes(outcomes, marques, now)
	}
	r.snapshot["_marque_outcomes"] = encode(outcomes)
	pending := decode[marquePending](r.snapshot, "_marque_pending")
	mission, data, err := r.loadMission(ctx, "__marque")
	if err != nil {
		return err
	}
	if mission != nil && data.Marque != nil && mission.Phase == "done" {
		// An issue changes prize sharing and the zone cap. Do not reuse the
		// pre-request quote; its next scheduled refresh still waits thirty minutes.
		key := "marque_quotes/" + data.Marque.Quote.SeaZoneID
		meta := r.meta[key]
		meta.Complete, meta.Stale = false, true
		meta.Error = "quote needs refresh after a completed marque request"
		r.meta[key] = meta
	}
	if mission != nil && data.Marque != nil {
		intent := data.Marque
		if data.Action != nil || data.LastActionID == "" {
			pending = marquePending{SeaZoneID: intent.Quote.SeaZoneID, BankingRegion: intent.BankingRegion, CostPence: intent.Quote.CostPence}
		} else if intent.IssuedID != "" {
			// The receipt proves the charge; retain only the zone guard until
			// a later authoritative list contains this exact issued marque.
			pending = marquePending{SeaZoneID: intent.Quote.SeaZoneID, IssuedID: intent.IssuedID, ExpiresAt: intent.ExpiresAt}
		} else {
			pending = marquePending{}
		}
	} else if pending.IssuedID == "" {
		pending = marquePending{}
	}
	if pending.IssuedID != "" {
		if validClock && !pending.ExpiresAt.IsZero() && !now.Before(pending.ExpiresAt) {
			pending = marquePending{}
		} else if r.fresh("marques") {
			for _, marque := range marques {
				if marque.ID == pending.IssuedID {
					pending = marquePending{}
					break
				}
			}
		}
	}
	if pending.SeaZoneID == "" {
		delete(r.snapshot, "_marque_pending")
	} else {
		r.snapshot["_marque_pending"] = encode(pending)
	}
	return nil
}

func (r *Runner) marqueEvidence(ctx context.Context) (MarqueInput, map[string]bool, error) {
	var input MarqueInput
	for _, key := range []string{"ships", "catalog", "profile", "clock", "vaults", "marques", "underwriting_positions"} {
		if !r.fresh(key) {
			return input, nil, fmt.Errorf("marque evaluation requires complete current %s", key)
		}
	}
	plans := make(map[string][]Plan)
	for _, ship := range r.stateCache.Ships {
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return input, nil, err
		}
		if mission == nil {
			continue
		}
		plans[ship.Ship.ID] = marqueMissionPlans(r.stateCache, ship, mission, &data)
	}
	exposed, err := marqueExposure(r.stateCache, plans, decode[[]store.UnderwritingPosition](r.snapshot, "underwriting_positions"))
	if err != nil {
		return input, nil, err
	}
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" {
			if vault.BalancePence < 0 || vault.BalancePence > maximumPence-input.LiquidPence {
				return input, nil, errors.New("marque liquidity is negative or exceeds the supported money range")
			}
			input.LiquidPence += vault.BalancePence
		}
	}
	now, _, valid := r.serverBounds()
	if !valid {
		return input, nil, errors.New("marque evaluation requires authoritative server time")
	}
	for _, marque := range decode[[]letterOfMarque](r.snapshot, "marques") {
		if now.Before(marque.ExpiresAt) {
			input.InForceCostPence = growthCashTotal(input.InForceCostPence, marque.CostPence)
		}
	}
	for id, outcome := range decode[map[string]marqueOutcome](r.snapshot, "_marque_outcomes") {
		if outcome.ID != id || outcome.CostPence < 0 || outcome.PrizePence < 0 {
			return input, nil, errors.New("permanent marque outcomes contain invalid cost or prize evidence")
		}
		input.ExpiredCount++
		input.ExpiredCostPence = growthCashTotal(input.ExpiredCostPence, outcome.CostPence)
		input.ExpiredPrizePence = growthCashTotal(input.ExpiredPrizePence, outcome.PrizePence)
	}
	if input.InForceCostPence > maximumPence || input.ExpiredCostPence > maximumPence || input.ExpiredPrizePence > maximumPence {
		return input, nil, errors.New("marque history exceeds the supported money range")
	}
	return input, exposed, nil
}

func (r *Runner) marqueHomeFunds(ctx context.Context, self bool) (string, int64, error) {
	home, exists := findPort(r.stateCache.Catalog, r.stateCache.HomePortID)
	if !exists || home.BankingRegion == "" {
		return "", 0, errors.New("marque spending requires the observed home banking region")
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return "", 0, err
	}
	if self {
		pending := decode[marquePending](r.snapshot, "_marque_pending")
		if pending.BankingRegion == home.BankingRegion && pending.CostPence > 0 && pending.CostPence <= maximumPence {
			input.ReserveByRegion[home.BankingRegion] -= pending.CostPence
		}
	}
	for _, balance := range input.Balances {
		if balance < 0 || balance > maximumPence {
			return "", 0, errors.New("marque spending requires nonnegative bounded regional vault balances")
		}
	}
	for _, reserve := range input.ReserveByRegion {
		if reserve < 0 || reserve > maximumPence {
			return "", 0, errors.New("marque protected regional commitments exceed the supported money range")
		}
	}
	protected, _, _, funded := measuredFunding(input.Balances, input.ReserveByRegion, input.Rates)
	if !funded {
		// This optional investment cannot preempt maintenance or other fleet
		// work when commitments lack cash or an observed remittance route.
		return home.BankingRegion, 0, nil
	}
	region := home.BankingRegion
	available := max(int64(0), min(input.Balances[region], protected[region])-input.ReserveByRegion[region])
	return region, available, nil
}

func (r *Runner) considerMarque(ctx context.Context) (bool, error) {
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return false, err
	}
	if target.PortID != "" {
		return false, r.hold(ctx, "__marque", capacitySavingsReason)
	}
	growth, _, err := r.loadMission(ctx, "__growth")
	if err != nil || growth != nil {
		return false, err
	}
	if pending := decode[marquePending](r.snapshot, "_marque_pending"); pending.IssuedID != "" {
		return false, r.hold(ctx, "__marque", "the last issued marque awaits its authoritative list record before adding more risk")
	}
	input, exposed, err := r.marqueEvidence(ctx)
	if err != nil {
		return false, r.hold(ctx, "__marque", err.Error())
	}
	region, available, err := r.marqueHomeFunds(ctx, false)
	if err != nil {
		return false, err
	}
	quotes := decode[map[string]MarqueQuote](r.snapshot, "marque_quotes")
	var best *marqueIntent
	backoffs := decode[map[string]time.Time](r.snapshot, "_marque_backoffs")
	for _, zone := range r.stateCache.Catalog.SeaZones {
		if until := backoffs[zone.ID]; time.Now().Before(until) {
			r.wakeBy(until)
			continue
		}
		quote := quotes[zone.ID]
		if !validMarqueQuote(quote, zone.ID) || !marqueQuoteFresh(r.meta["marque_quotes/"+zone.ID], time.Now()) {
			continue
		}
		input.Quote, input.Exposed = quote, exposed[zone.ID]
		decision := EvaluateMarque(input)
		if !decision.Accepted {
			if err := r.decision(ctx, "__marque", "marque_rejected:"+zone.ID, decision.Reason, quote); err != nil {
				return false, err
			}
			continue
		}
		if quote.CostPence > available {
			continue
		}
		if best == nil || quote.ExpectedNetPrizePence-quote.CostPence > best.Quote.ExpectedNetPrizePence-best.Quote.CostPence ||
			quote.ExpectedNetPrizePence-quote.CostPence == best.Quote.ExpectedNetPrizePence-best.Quote.CostPence && zone.ID < best.Quote.SeaZoneID {
			best = &marqueIntent{Quote: quote, BankingRegion: region}
		}
	}
	if best == nil {
		return false, r.decision(ctx, "__marque", "marque_wait", "no unexposed traffic-priced marque meets expected return, local reserves and realized-return limits", nil)
	}
	if err := r.decision(ctx, "__marque", "marque_selected", "highest expected prize less cost among admitted sea zones", best); err != nil {
		return false, err
	}
	mission := &store.Mission{AccountID: r.account.ID, ShipID: "__marque", Phase: "marque", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "marque"}, Marque: best, CostsKnown: true}
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) driveMarque(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute || data.Action != nil {
		return false, nil
	}
	if mission.Phase == "done" || data.Marque == nil || data.LastActionID != "" {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if err := r.marquePreflight(ctx, data, false); err != nil {
		if holdErr := r.hold(ctx, mission.ShipID, err.Error()); holdErr != nil {
			return false, holdErr
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	intent := data.Marque
	intent.BeforeIDs = nil
	for _, marque := range decode[[]letterOfMarque](r.snapshot, "marques") {
		intent.BeforeIDs = append(intent.BeforeIDs, marque.ID)
	}
	intent.NotBefore, _, _ = r.serverBounds()
	return true, r.stage(ctx, mission, data, ShipState{}, "marque", "/v1/fleet/issue-marque", map[string]any{
		"sea_zone_id": intent.Quote.SeaZoneID, "added_piracy_bps": int64(500), "duration_game_hours": int64(60),
	})
}

func (r *Runner) marquePreflight(ctx context.Context, data *missionData, requestExists bool) error {
	target, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return err
	}
	if target.PortID != "" {
		return errors.New(capacitySavingsReason)
	}
	// A persisted intent may predate the switch being turned off; never spend on it.
	if !r.opts.MarquesEnabled {
		return errors.New("marque issuance is disabled (MARQUES_ENABLED=false)")
	}
	if data.Marque == nil {
		return errors.New("marque has no frozen admitted quote")
	}
	growth, _, err := r.loadMission(ctx, "__growth")
	if err != nil {
		return err
	}
	if growth != nil {
		return errors.New("fleet growth takes priority over new marque spending")
	}
	intent := data.Marque
	if requestExists {
		var request struct {
			SeaZoneID         string `json:"sea_zone_id"`
			AddedPiracyBPS    int64  `json:"added_piracy_bps"`
			DurationGameHours int64  `json:"duration_game_hours"`
		}
		if data.Action == nil || json.Unmarshal(data.Action.Body, &request) != nil || request.SeaZoneID != intent.Quote.SeaZoneID || request.AddedPiracyBPS != 500 || request.DurationGameHours != 60 {
			return errors.New("marque immutable request differs from its admitted zone, 500 bps or 60 game hours")
		}
	}
	input, exposed, err := r.marqueEvidence(ctx)
	if err != nil {
		return err
	}
	quote := decode[map[string]MarqueQuote](r.snapshot, "marque_quotes")[intent.Quote.SeaZoneID]
	if requestExists {
		// issue-marque has no maximum-price field. Requote immediately before
		// the first send; this narrows, but cannot eliminate, the upstream race.
		path := "/v1/fleet/marque-quote?sea_zone_id=" + url.QueryEscape(intent.Quote.SeaZoneID) + "&added_piracy_bps=500&duration_game_hours=60"
		if err := r.api.Get(ctx, path, &quote); err != nil {
			return fmt.Errorf("refresh marque price before first send: %w", err)
		}
	} else if !marqueQuoteFresh(r.meta["marque_quotes/"+quote.SeaZoneID], time.Now()) {
		return errors.New("marque quote is unavailable or older than thirty real minutes")
	}
	if !validMarqueQuote(quote, intent.Quote.SeaZoneID) || !validMarqueQuote(intent.Quote, intent.Quote.SeaZoneID) {
		return errors.New("marque quote has invalid pricing terms")
	}
	if quote.CostPence*10 > intent.Quote.CostPence*11 {
		return errors.New("marque cost increased more than ten percent above its admitted quote")
	}
	if until := decode[map[string]time.Time](r.snapshot, "_marque_backoffs")[quote.SeaZoneID]; time.Now().Before(until) {
		return errors.New("marque zone remains in its six-real-hour refusal backoff")
	}
	input.Quote, input.Exposed = quote, exposed[quote.SeaZoneID]
	if decision := EvaluateMarque(input); !decision.Accepted {
		return fmt.Errorf("marque admission changed: %s", decision.Reason)
	}
	region, available, err := r.marqueHomeFunds(ctx, true)
	if err != nil {
		return err
	}
	if quote.CostPence > available {
		return errors.New("marque cost would spend the home vault's protected regional commitments")
	}
	if region != intent.BankingRegion {
		return errors.New("marque home banking region changed before first send")
	}
	return nil
}

func (r *Runner) recordMarqueRefusal(ctx context.Context, data *missionData) error {
	if data.Marque == nil {
		return errors.New("marque refusal has no persisted sea zone")
	}
	backoffs := decode[map[string]time.Time](r.snapshot, "_marque_backoffs")
	if backoffs == nil {
		backoffs = make(map[string]time.Time)
	}
	now := time.Now().UTC()
	for zone, until := range backoffs {
		if !now.Before(until) {
			delete(backoffs, zone)
		}
	}
	zone := data.Marque.Quote.SeaZoneID
	backoffs[zone] = maxTime(backoffs[zone], now.Add(6*time.Hour))
	r.snapshot["_marque_backoffs"] = encode(backoffs)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}

func (r *Runner) reconcileMarque(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	if data.Marque == nil || !r.fresh("marques") || data.Marque.NotBefore.IsZero() {
		return false, nil
	}
	intent := data.Marque
	var match *letterOfMarque
	for _, marque := range decode[[]letterOfMarque](r.snapshot, "marques") {
		if marque.SeaZoneID != intent.Quote.SeaZoneID || marque.AddedPiracyBPS != 500 ||
			slices.Contains(intent.BeforeIDs, marque.ID) || marque.CreatedAt.Before(intent.NotBefore) ||
			marque.SponsorCompanyID != "" && marque.SponsorCompanyID != r.account.CompanyID {
			continue
		}
		if !validMarque(marque) || match != nil {
			return false, r.holdUnresolvedAction(ctx, mission.ShipID, data.Action, "marque issue has ambiguous new REST records; retain its immutable request")
		}
		copy := marque
		match = &copy
	}
	if match == nil {
		return false, nil
	}
	if action.State != store.ActionComplete {
		evidence := encode(map[string]any{"kind": "authoritative_marque", "marque": match, "observed_at": r.meta["marques"].UpdatedAt})
		if err := r.db.ReconcileAction(ctx, action.ID, evidence); err != nil {
			return false, err
		}
	}
	intent.IssuedID, intent.ExpiresAt = match.ID, match.ExpiresAt
	return r.applyReconciliation(ctx, mission, data)
}
