package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const capacityHistoryWindow = 24 * time.Hour

const capacityNoSpareWorkReason = "capacity growth needs an executable profitable voyage after incumbent commitments"

var capacityShipLossReason = fmt.Sprintf("capacity growth waits %.0f real hours after a ship loss", capacityHistoryWindow.Hours())

type CapacityGrowthInput struct {
	Hull                         Hull
	HullPricePence               int64
	InitialStoresPence           int64
	SurplusPence                 int64
	FleetSize                    int
	CandidateRatePencePerGameDay float64
	LotCashPence                 int64
	IncrementalDailyUpkeepPence  int64
	ReservePence                 int64
	MaxPaybackDays               float64
	HasSpareWork                 bool
}

// Every hull must repay its capital from an accepted plan while preserving
// quote reserves and a cash lot for every ship.
func EvaluateCapacityGrowth(in CapacityGrowthInput) GrowthDecision {
	decision := GrowthDecision{}
	guns := min(int64(4), in.Hull.MaxCannons)
	if in.Hull.CanSealDispatches || in.HullPricePence <= 0 || guns < 1 || in.Hull.HoldCapacityTons <= 2*guns {
		decision.Reason = "capacity growth requires a priced trading hull with room for cannons and cargo"
		return decision
	}
	slippage := ceilBPS(in.HullPricePence, 1_000)
	cannons := guns * 15_000
	capital := growthCashTotal(in.HullPricePence, slippage, cannons, in.InitialStoresPence)
	if capital == math.MaxInt64 {
		decision.Reason = "capacity growth capital exceeds integer-pence bounds"
		return decision
	}
	decision = GrowthDecision{
		QuotedHullPence:        in.HullPricePence,
		SlippageReservePence:   slippage,
		CannonsPence:           cannons,
		InitialStoresPence:     in.InitialStoresPence,
		IncrementalUpkeepPence: in.IncrementalDailyUpkeepPence,
		EvidenceBasis:          "best accepted non-charter voyage repeat net per game day, or half its expected outbound rate",
	}
	if !in.HasSpareWork {
		decision.Reason = capacityNoSpareWorkReason
		return decision
	}
	net := in.CandidateRatePencePerGameDay - float64(in.IncrementalDailyUpkeepPence)
	if net <= 0 || math.IsNaN(net) || math.IsInf(net, 0) {
		decision.Reason = "capacity growth marginal fleet upkeep consumes the candidate voyage earning rate"
		return decision
	}
	decision.IncrementalNetPencePerGameDay = net
	decision.PaybackGameDays = float64(capital) / net
	if in.MaxPaybackDays <= 0 || math.IsNaN(in.MaxPaybackDays) || math.IsInf(in.MaxPaybackDays, 0) || decision.PaybackGameDays > in.MaxPaybackDays {
		decision.Reason = "capacity growth hull, slippage, cannon and store capital exceeds maximum game-day payback"
		return decision
	}
	if in.FleetSize < 0 || in.LotCashPence < 0 ||
		in.LotCashPence > 0 && int64(in.FleetSize) >= math.MaxInt64/in.LotCashPence {
		decision.Reason = "capacity growth working cash exceeds integer-pence bounds"
		return decision
	}
	buffer := growthCashTotal(max(in.ReservePence, ceilBPS(in.HullPricePence, 1_500)), (int64(in.FleetSize)+1)*in.LotCashPence)
	decision.WorkingCapitalPence = buffer
	required := growthCashTotal(capital, buffer)
	if required == math.MaxInt64 {
		decision.Reason = "capacity growth working cash exceeds integer-pence bounds"
		return decision
	}
	decision.EconomicallyViable = true
	if in.SurplusPence < required {
		decision.CashShortfallPence = required - in.SurplusPence
		decision.Reason = "capacity growth must preserve quote reserves and a cash lot for every ship after the build"
		return decision
	}
	decision.Accepted = true
	decision.Reason = "candidate voyage earnings cover payback and marginal upkeep while preserving every ship's working cash"
	return decision
}

func betterCapacityCandidate(candidate, other GrowthDecision) bool {
	capital := growthCashTotal(candidate.QuotedHullPence, candidate.SlippageReservePence, candidate.CannonsPence, candidate.InitialStoresPence)
	otherCapital := growthCashTotal(other.QuotedHullPence, other.SlippageReservePence, other.CannonsPence, other.InitialStoresPence)
	if capital <= 0 || capital == math.MaxInt64 {
		return false
	}
	if otherCapital <= 0 || otherCapital == math.MaxInt64 {
		return true
	}
	returnRate := candidate.IncrementalNetPencePerGameDay / float64(capital)
	otherReturnRate := other.IncrementalNetPencePerGameDay / float64(otherCapital)
	if returnRate != otherReturnRate {
		return returnRate > otherReturnRate
	}
	return capital < otherCapital
}

func betterCapacityFinancingCandidate(candidate, other GrowthDecision) bool {
	candidate.IncrementalNetPencePerGameDay = candidate.DepthNetPencePerGameDay
	other.IncrementalNetPencePerGameDay = other.DepthNetPencePerGameDay
	return betterCapacityCandidate(candidate, other)
}

type capacityIntent struct {
	BuildPort       string             `json:"build_port"`
	BuildClass      string             `json:"build_class"`
	QuoteLimitPence int64              `json:"quote_limit_pence"`
	AdmittedAt      time.Time          `json:"admitted_at"`
	RefitAttempts   int                `json:"refit_attempts,omitempty"`
	RefitRefused    bool               `json:"refit_refused,omitempty"`
	Financing       *capacityFinancing `json:"financing,omitempty"`
}

func (r *Runner) holdCapacityGrowth(ctx context.Context, reason string) (bool, error) {
	decision := GrowthDecision{Reason: reason}
	if err := r.decision(ctx, "", "capacity_growth", reason, decision); err != nil {
		return false, err
	}
	return false, r.hold(ctx, "", reason)
}

func (r *Runner) capacityLosses(ctx context.Context, asOf time.Time) (string, error) {
	if !r.fresh("ships") || !r.meta["ships"].Complete {
		return "capacity growth requires a fresh complete fleet list before excluding ship losses", nil
	}
	losses := decode[map[string]time.Time](r.snapshot, "_ship_losses")
	changed := false
	recordLoss := func(shipID string, lostAt time.Time) {
		if prior, exists := losses[shipID]; exists && !lostAt.Before(prior) {
			return
		}
		if losses == nil {
			losses = make(map[string]time.Time)
		}
		losses[shipID] = lostAt
		changed = true
	}
	undatedLoss, futureLoss := false, false
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status != "sunk" {
			continue
		}
		if ship.Ship.UpdatedAt == nil || ship.Ship.UpdatedAt.IsZero() {
			recordLoss(ship.Ship.ID, asOf)
			undatedLoss = true
			r.wakeBy(asOf.Add(capacityHistoryWindow))
			continue
		}
		recordLoss(ship.Ship.ID, *ship.Ship.UpdatedAt)
		if ship.Ship.UpdatedAt.After(asOf) {
			futureLoss = true
			r.wakeBy(ship.Ship.UpdatedAt.Add(capacityHistoryWindow))
		}
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return "", err
	}
	for _, mission := range missions {
		shipID := mission.ShipID
		if strings.HasPrefix(shipID, "__") {
			continue
		}
		if shipID == "" {
			continue
		}
		if _, exists := r.ship(shipID); !exists {
			recordLoss(shipID, r.meta["ships"].UpdatedAt)
		}
	}
	if changed {
		r.snapshot["_ship_losses"] = encode(losses)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return "", err
		}
	}
	windowStart := asOf.Add(-capacityHistoryWindow)
	recentLoss := false
	for _, lostAt := range losses {
		if !lostAt.Before(windowStart) {
			recentLoss = true
			r.wakeBy(lostAt.Add(capacityHistoryWindow))
		}
	}
	if undatedLoss || futureLoss || recentLoss {
		return capacityShipLossReason, nil
	}
	return "", nil
}

func (r *Runner) capacityFleetInput(ctx context.Context, evidence *growthEvidence) (CapacityGrowthInput, string, error) {
	input := CapacityGrowthInput{
		SurplusPence:   evidence.available,
		ReservePence:   r.opts.ReservePence,
		MaxPaybackDays: r.opts.CapacityPaybackDays,
	}
	if until := decode[time.Time](r.snapshot, "_capacity_growth_paused_until"); evidence.asOf.Before(until) {
		r.wakeBy(until)
		return input, fmt.Sprintf("capacity growth pauses for %.0f real hours after a build price overrun", capacityHistoryWindow.Hours()), nil
	}
	if reason, err := r.capacityLosses(ctx, evidence.asOf); err != nil || reason != "" {
		return input, reason, err
	}
	scale := float64(r.stateCache.Clock.Calendar.TimeScale)
	if scale <= 0 || r.stateCache.Clock.Paused {
		return input, "capacity growth requires an unpaused authoritative game time scale", nil
	}
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		input.FleetSize++
		if ship.Voyage == nil && ship.Ship.Status != "sailing" {
			mission, data, err := r.loadMission(ctx, ship.Ship.ID)
			if err != nil {
				return input, "", err
			}
			if mission != nil && (data.Plan.Destination == "" || data.Plan.Kind == "maintenance" ||
				data.Relocation == nil && !r.reserveGrowthMission(&growthEvidence{}, mission, &data)) {
				return input, "capacity growth waits for every stationary incumbent to have a voyage commitment", nil
			}
		}
	}
	window, err := r.db.EconomicHistoryWindow(ctx, r.account.ID, "", evidence.asOf, capacityHistoryWindow)
	if err != nil {
		return input, "", err
	}
	if window.FutureCompletion {
		return input, "capacity growth rejects future-dated settlement evidence", nil
	}
	var controlIDs []string
	if err := json.Unmarshal(window.ControlCloseoutIDs, &controlIDs); err != nil {
		return input, "", fmt.Errorf("decode capacity control closeout identities: %w", err)
	}
	controls, err := r.db.SettlementsByID(ctx, r.account.ID, controlIDs)
	if err != nil {
		return input, "", err
	}
	for _, control := range controls {
		if control.ShipID == "" {
			continue
		}
		ship, exists := r.ship(control.ShipID)
		if !exists || ship.Ship.Status == "sunk" {
			return input, "capacity growth requires a surviving hull for an unknown-cost shipyard closeout", nil
		}
	}
	deliveries, err := r.db.CapacityDeliveryHistory(ctx, r.account.ID, evidence.asOf)
	if err != nil {
		return input, "", err
	}
	lots := make([]int64, 0, min(10, len(deliveries)))
	for _, delivery := range deliveries {
		if delivery.CostsKnown && len(lots) < 10 {
			lots = append(lots, growthCashTotal(delivery.AcquisitionPence, delivery.FeesPence, delivery.ProvisionsPence))
		}
	}
	if len(lots) > 0 {
		sort.Slice(lots, func(i, j int) bool { return lots[i] < lots[j] })
		middle := len(lots) / 2
		input.LotCashPence = lots[middle]
		if len(lots)%2 == 0 {
			// Round a half-penny median upward without overflowing the sum.
			input.LotCashPence = lots[middle-1] + (lots[middle]-lots[middle-1])/2 + (lots[middle]-lots[middle-1])%2
		}
	}
	return input, "", nil
}

// capacityVoyageRate prices a hull from the planner's expected net. The stress
// net is an illustrative capital-loss scenario, not an expected cost, so it
// must not decide whether a hull earns its keep.
func capacityVoyageRate(economics Economics) float64 {
	if economics.BallastRepeatGameDays > 0 {
		return float64(economics.BallastRepeatNetPence) / economics.BallastRepeatGameDays
	}
	if economics.GameDays > 0 {
		// Without a computed repeat, assume an empty return of equal length.
		return float64(economics.ExpectedNetPence) / (2 * economics.GameDays)
	}
	return 0
}

func (r *Runner) capacityAtYard(ctx context.Context, port Port, hull Hull, quote YardQuote, input CapacityGrowthInput, evidence *growthEvidence) (GrowthDecision, map[string]int64, error) {
	planning, err := r.committedPlanningInputExcept(ctx, growthCandidate(port, hull), "__growth")
	if err != nil {
		return GrowthDecision{}, nil, err
	}
	// Charge the candidate's nonlinear marginal upkeep separately from its
	// voyage rate rather than assigning the incumbent fleet's upkeep to it.
	planning.FleetDailyUpkeepPence = 0
	plans := PlanVoyages(planning)
	input.Hull = hull
	input.HullPricePence = *quote.TotalCostPence
	var depthRate float64
	for _, plan := range plans {
		basis := plan.Economics.RevenueBasis
		if !plan.Accepted || len(plan.FreightIDs) > 0 {
			continue
		}
		// A finite charter can be the first load, but never a perpetual rate.
		input.LotCashPence = max(input.LotCashPence, growthCashTotal(plan.Economics.PurchasePence, plan.Economics.CustomsPence, plan.Economics.StoreCashPence))
		if plan.ContractID != "" || basis != "depth" && basis != "gossip" {
			continue
		}
		input.HasSpareWork = true
		rate := capacityVoyageRate(plan.Economics)
		if basis == "depth" {
			depthRate = max(depthRate, rate)
		}
		if rate > input.CandidateRatePencePerGameDay {
			input.CandidateRatePencePerGameDay = rate
			input.InitialStoresPence = max(plan.Economics.StoreCashPence, plan.Economics.ProvisionsPence)
		}
	}
	decision := EvaluateCapacityGrowth(input)
	// Only the best candidate is recorded by the caller; record every yard and
	// hull too so an operator can see why a present yard does not build. The
	// details are coarse on purpose: decisions deduplicate on their details,
	// and raw economics move with every book capture.
	rejections := make(map[string]int)
	var acceptedRoutes []string
	for _, plan := range plans {
		route := plan.Kind + " " + plan.Origin + ">" + plan.Destination + " " + plan.Commodity
		if !plan.Accepted && len(rejections) < 6 {
			rejections[route+": "+plan.Reason]++
		}
		if plan.Accepted && len(acceptedRoutes) < 3 {
			acceptedRoutes = append(acceptedRoutes, route)
		}
	}
	if err := r.decision(ctx, "", "capacity_candidate:"+port.ID+"|"+hull.ID, decision.Reason, map[string]any{
		"accepted":            decision.Accepted,
		"rate_crowns_per_day": math.Round(input.CandidateRatePencePerGameDay/1_000) * 10,
		"payback_game_days":   math.Round(decision.PaybackGameDays),
		"capital_crowns":      growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence) / 1_000 * 10,
		"accepted_voyages":    acceptedRoutes,
		"plan_rejections":     rejections,
	}); err != nil {
		return GrowthDecision{}, nil, err
	}
	if !decision.EconomicallyViable {
		return decision, nil, nil
	}
	required := clonePence(evidence.required)
	capital := growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
	required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], capital)
	_, fees, _, funded := measuredFunding(evidence.balances, required, planning.Rates)
	decision.FundingFeesPence = fees
	if !funded {
		decision.Accepted = false
		decision.Reason = "capacity growth cannot fund the yard while preserving incumbent commitments and regional reserves"
	} else {
		input.SurplusPence -= fees
		decision = EvaluateCapacityGrowth(input)
		decision.FundingFeesPence = fees
	}
	decision.DepthNetPencePerGameDay = max(0, depthRate-float64(input.IncrementalDailyUpkeepPence))
	return decision, required, nil
}

func (r *Runner) considerCapacityGrowth(ctx context.Context, evidence *growthEvidence) (bool, error) {
	for _, dataset := range []string{"vaults", "ships", "catalog", "clock", "zones", "rates", "factors", "profile", "warehouses", "contracts", "orders", "fills"} {
		if !r.fresh(dataset) {
			return r.holdCapacityGrowth(ctx, "capacity growth requires current successful "+dataset+" authority")
		}
	}
	input, reason, err := r.capacityFleetInput(ctx, evidence)
	if err != nil {
		return false, err
	}
	if reason != "" {
		return r.holdCapacityGrowth(ctx, reason)
	}
	bestDecision := GrowthDecision{Reason: "capacity growth has no phase-unlocked trading hull with a fresh executable quay and turnkey quote"}
	var bestPort Port
	var bestHull Hull
	var bestRequired map[string]int64
	found, evaluated, canExpand := false, false, true
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	buildBackoffs := decode[map[string]time.Time](r.snapshot, "_growth_build_backoffs")
	for _, port := range r.stateCache.Catalog.Ports {
		if !port.HasShipyard || port.Phase > r.stateCache.Clock.UnlockedPhase {
			continue
		}
		present := slices.Contains(r.stateCache.Factors, port.ID)
		for _, ship := range r.stateCache.Ships {
			present = present || ship.Ship.CurrentPortID == port.ID && ship.Voyage == nil &&
				(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead")
		}
		if !present {
			continue
		}
		for _, hull := range r.stateCache.Catalog.ShipClasses {
			if hull.CanSealDispatches || hull.Phase > r.stateCache.Clock.UnlockedPhase {
				continue
			}
			quote, err := r.growthQuote(port, hull)
			if err != nil {
				key := port.ID + "|" + hull.ID
				observed, exists := r.stateCache.Quotes[key]
				if !exists || !r.fresh("yard_quotes/"+key) || !r.fresh("books/"+port.ID) ||
					r.stateCache.Markets[port.ID].Visibility != "quay" || time.Now().Before(buildBackoffs[key]) ||
					observed.PortID != port.ID || observed.ShipClass != hull.ID {
					// An observed unavailable hull is not missing authority.
					canExpand = false
				}
				if !evaluated {
					bestDecision.Reason = err.Error()
				}
				continue
			}
			grownFleet[len(grownFleet)-1] = growthCandidate(port, hull)
			input.IncrementalDailyUpkeepPence = DailyUpkeep(grownFleet, r.stateCache.Catalog, len(r.stateCache.Factors)) - oldUpkeep
			decision, required, err := r.capacityAtYard(ctx, port, hull, quote, input, evidence)
			if err != nil {
				return false, err
			}
			if decision.Accepted || !decision.EconomicallyViable {
				noLoan := capacityLoanDecision{ShortfallPence: decision.CashShortfallPence, Reason: "no borrowing: " + decision.Reason}
				if decision.Accepted {
					noLoan.Reason = "own cash funds this candidate; no borrowing is needed"
				}
				if err := r.recordCapacityLoan(ctx, port, hull, noLoan); err != nil {
					return false, err
				}
			}
			betterUnfunded := betterCapacityCandidate(decision, bestDecision)
			if decision.EconomicallyViable && bestDecision.EconomicallyViable {
				betterUnfunded = betterCapacityFinancingCandidate(decision, bestDecision)
			}
			if decision.Accepted && (!found || betterCapacityCandidate(decision, bestDecision)) ||
				!found && (!evaluated || decision.EconomicallyViable && !bestDecision.EconomicallyViable ||
					decision.EconomicallyViable == bestDecision.EconomicallyViable && betterUnfunded) {
				bestPort, bestHull, bestDecision, bestRequired = port, hull, decision, required
			}
			found = found || decision.Accepted
			evaluated = true
		}
	}
	key := "capacity_growth"
	if evaluated {
		key += ":" + bestPort.ID + "|" + bestHull.ID
	}
	if err := r.decision(ctx, "", key, bestDecision.Reason, bestDecision); err != nil {
		return false, err
	}
	target := ""
	if found {
		target = bestPort.ID
	}
	if decode[string](r.snapshot, "_capacity_growth_port") != target {
		r.snapshot["_capacity_growth_port"] = encode(target)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return false, err
		}
	}
	if !found && evaluated && bestDecision.EconomicallyViable {
		changed, err := r.considerCapacityFinancing(ctx, bestPort, bestHull, bestDecision, bestRequired, evidence)
		if changed || err != nil {
			return changed, err
		}
	}
	if !found {
		if err := r.hold(ctx, "", bestDecision.Reason); err != nil {
			return false, err
		}
		if r.opts.Execute && canExpand {
			return r.considerExpansionAgent(ctx)
		}
		return false, nil
	}
	if !r.opts.Execute {
		return false, r.hold(ctx, "", "observe mode: admitted capacity-growth hull is not built")
	}
	if changed, err := r.fundTreasury(ctx, evidence.balances, bestRequired); changed || err != nil {
		return changed, err
	}
	for region, required := range bestRequired {
		if evidence.balances[region] < required {
			return false, r.hold(ctx, "", "capacity growth waits for completed regional yard funding")
		}
	}
	return r.startCapacityGrowth(ctx, bestPort, bestHull, bestDecision, nil)
}

func (r *Runner) startCapacityGrowth(ctx context.Context, port Port, hull Hull, decision GrowthDecision, financing *capacityFinancing) (bool, error) {
	used := decode[[]string](r.snapshot, "_used_ship_names")
	for _, ship := range r.stateCache.Ships {
		used = append(used, ship.Ship.Name)
	}
	name := names.Next(used)
	now := time.Now().UTC()
	mission := &store.Mission{
		AccountID: r.account.ID, ShipID: "__growth", Phase: "growth_build",
		Generation: uuid.NewString(), Destination: port.ID,
	}
	data := missionData{
		Capacity: &capacityIntent{
			BuildPort: port.ID, BuildClass: hull.ID,
			QuoteLimitPence: growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence),
			AdmittedAt:      now,
			Financing:       financing,
		},
		BuildName: name, BuildClass: hull.ID, BuildPort: port.ID,
		TargetCannons: min(4, hull.MaxCannons), CostsKnown: true,
		StartedAt: now, StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
		InitialLedgerIDs: r.ledgerIDs(),
	}
	if financing != nil {
		mission.Phase = "growth_finance"
		return true, r.saveMission(ctx, mission, &data)
	}
	return true, r.stage(ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{
		"port_id": port.ID, "name": name, "ship_class": hull.ID, "build_mode": "turnkey_cash",
	})
}

func (r *Runner) capacityBuildReady(ctx context.Context, data *missionData) error {
	intent := data.Capacity
	if intent == nil || intent.BuildPort != data.BuildPort || intent.BuildClass != data.BuildClass ||
		intent.QuoteLimitPence <= 0 || intent.AdmittedAt.IsZero() {
		return errors.New("capacity build requires its matching durable yard, hull and quote ceiling")
	}
	if intent.Financing != nil {
		if intent.Financing.Cancel || len(intent.Financing.Loans) != len(intent.Financing.Quotes) {
			return errors.New("financed build requires its complete uncanceled debt package")
		}
		_, required, err := r.capacityFinanceReady(ctx, data)
		if err != nil {
			return err
		}
		input := r.planningInput(ShipState{})
		for region, amount := range required {
			if input.Balances[region] < amount {
				return errors.New("financed build waits for its frozen package and protected debt cash")
			}
		}
		_, err = r.capacityYardReady(ctx, data)
		return err
	}
	for _, dataset := range []string{"vaults", "ships", "catalog", "clock", "zones", "rates", "factors", "profile", "warehouses", "contracts", "orders", "fills"} {
		if !r.fresh(dataset) {
			return fmt.Errorf("capacity build requires current successful %s authority", dataset)
		}
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return err
	}
	for _, action := range pending {
		if data.Action == nil || action.ID != data.Action.ID {
			return errors.New("capacity build waits for every other unresolved account action")
		}
	}
	for _, id := range []string{"__treasury", "__underwriting", "__marque"} {
		mission, current, err := r.loadMission(ctx, id)
		if err != nil {
			return err
		}
		if mission != nil && current.Action != nil {
			return errors.New("capacity build waits for every other unresolved control action")
		}
	}
	port, exists := findPort(r.stateCache.Catalog, intent.BuildPort)
	if !exists {
		return errors.New("capacity build yard is absent from the current catalog")
	}
	hull, exists := findHull(r.stateCache.Catalog, intent.BuildClass)
	if !exists {
		return errors.New("capacity build hull is absent from the current catalog")
	}
	if data.TargetCannons != min(int64(4), hull.MaxCannons) {
		return errors.New("capacity build cannon target no longer matches the admitted hull")
	}
	quote, err := r.growthQuote(port, hull)
	if err != nil {
		return err
	}
	if *quote.TotalCostPence > intent.QuoteLimitPence {
		return errors.New("capacity build fresh turnkey quote exceeds the admitted quote and slippage ceiling")
	}
	evidence := growthEvidence{workingByRegion: make(map[string]int64)}
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		mission, current, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return err
		}
		if mission != nil {
			if current.Action != nil {
				return errors.New("capacity build waits for every other unresolved mission action")
			}
			r.reserveMissionCash(evidence.workingByRegion, mission, &current, ship)
			continue
		}
		if ship.Ship.Status != "sailing" && ship.Ship.ConvoyID == "" && ship.CargoUsedTons == 0 && len(r.activeFreight(ship.Ship.ID)) == 0 {
			planning, err := r.committedPlanningInputExcept(ctx, ship, "__growth")
			if err != nil {
				return err
			}
			plans := PlanVoyages(planning)
			if len(plans) > 0 && plans[0].Accepted {
				return errors.New("capacity build must reuse an idle ship's currently executable profitable lane first")
			}
		}
	}
	if reason, err := r.growthCashFunding(ctx, &evidence); err != nil {
		return err
	} else if reason != "" {
		return errors.New(reason)
	}
	input, reason, err := r.capacityFleetInput(ctx, &evidence)
	if err != nil {
		return err
	}
	if reason != "" {
		if err := r.decision(ctx, "", "capacity_growth:"+port.ID+"|"+hull.ID, reason, GrowthDecision{Reason: reason}); err != nil {
			return err
		}
		return errors.New(reason)
	}
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	grownFleet[len(grownFleet)-1] = growthCandidate(port, hull)
	input.IncrementalDailyUpkeepPence = DailyUpkeep(grownFleet, r.stateCache.Catalog, len(r.stateCache.Factors)) -
		DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	decision, required, err := r.capacityAtYard(ctx, port, hull, quote, input, &evidence)
	if err != nil {
		return err
	}
	if err := r.decision(ctx, "", "capacity_growth:"+port.ID+"|"+hull.ID, decision.Reason, decision); err != nil {
		return err
	}
	if !decision.Accepted {
		return errors.New(decision.Reason)
	}
	for region, amount := range required {
		if evidence.balances[region] < amount {
			return errors.New("capacity build waits for completed reserve-safe regional yard funding")
		}
	}
	live, err := r.capacityYardReady(ctx, data)
	if err != nil {
		return err
	}
	extra := max(0, *live.TotalCostPence-growthCashTotal(*quote.TotalCostPence, decision.SlippageReservePence))
	if evidence.balances[port.BankingRegion]-extra < required[port.BankingRegion] {
		if err := r.recordGrowthBuildRefusal(ctx, data); err != nil {
			return err
		}
		return errors.New("capacity live turnkey quote exceeds protected regional yard funding")
	}
	return nil
}

// Both acceptance and construction check the same no-price-cap endpoint.
func (r *Runner) capacityYardReady(ctx context.Context, data *missionData) (YardQuote, error) {
	var live YardQuote
	path := "/v1/shipyard/quote?port_id=" + url.QueryEscape(data.BuildPort) + "&ship_class=" + url.QueryEscape(data.BuildClass) + "&build_mode=turnkey_cash"
	if err := r.api.Get(ctx, path, &live); err != nil {
		var problem *gameapi.APIError
		if data.Capacity.Financing != nil && errors.As(err, &problem) &&
			(problem.Status == http.StatusForbidden || problem.Status == http.StatusNotFound || problem.Code == "no_port_presence") {
			data.Capacity.Financing.Cancel = true
			if saveErr := r.recordGrowthBuildRefusal(ctx, data); saveErr != nil {
				return live, saveErr
			}
		}
		return live, fmt.Errorf("refresh capacity turnkey price before first send: %w", err)
	}
	intent := data.Capacity
	if live.PortID == data.BuildPort && live.ShipClass == data.BuildClass && live.BuildMode == "turnkey_cash" && live.Available &&
		live.TotalCostPence != nil && *live.TotalCostPence > 0 && *live.TotalCostPence <= intent.QuoteLimitPence &&
		live.MaterialsPence >= 0 && live.NPCMaterialsPence >= 0 && live.NPCMaterialsPence <= live.MaterialsPence &&
		live.MaterialsPence-live.NPCMaterialsPence <= intent.QuoteLimitPence-*live.TotalCostPence {
		return live, nil
	}
	if intent.Financing != nil {
		intent.Financing.Cancel = true
	}
	if err := r.recordGrowthBuildRefusal(ctx, data); err != nil {
		return live, err
	}
	return live, errors.New("capacity live turnkey quote is unavailable, mismatched or above the admitted ceiling")
}

func (r *Runner) driveCapacityGrowth(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if ship.Ship.CurrentPortID != data.BuildPort || ship.Voyage != nil || ship.Ship.CannonsCount >= data.TargetCannons ||
		data.Capacity.RefitRefused || data.Capacity.RefitAttempts >= 3 {
		return r.finishGrowthControl(ctx, mission, data)
	}
	port, err := r.spendingPort(ship)
	if err != nil {
		return false, err
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	cost := (data.TargetCannons - ship.Ship.CannonsCount) * 15_000
	// Guns were funded at admission; hold rather than let an unarmed hull trade.
	if input.Balances[port.BankingRegion]-cost < input.ReserveByRegion[port.BankingRegion] {
		return false, r.hold(ctx, ship.Ship.ID, "capacity cannons must preserve incumbent commitments and regional reserves")
	}
	// An evicted hull must berth again before the yard can arm it; nothing else
	// docks it while this control owns the hull.
	if ship.Ship.Status != "berthed" {
		return r.dockForMission(ctx, ship, mission, data, false)
	}
	return true, r.stage(ctx, mission, data, ship, "refit", "/v1/shipyard/modify-ship", map[string]any{
		"ship_id": ship.Ship.ID, "target_cannons_count": data.TargetCannons,
	})
}

func (r *Runner) recordGrowthBuildRefusal(ctx context.Context, data *missionData) error {
	backoffs := decode[map[string]time.Time](r.snapshot, "_growth_build_backoffs")
	if backoffs == nil {
		backoffs = make(map[string]time.Time)
	}
	now := time.Now().UTC()
	for key, until := range backoffs {
		if !now.Before(until) {
			delete(backoffs, key)
		}
	}
	key := data.BuildPort + "|" + data.BuildClass
	backoffs[key] = maxTime(backoffs[key], now.Add(6*time.Hour))
	r.snapshot["_growth_build_backoffs"] = encode(backoffs)
	return r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot)
}
