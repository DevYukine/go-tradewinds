package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

const capacityHistoryWindow = 24 * time.Hour

// capacityFleetFillShare bounds one cash-funded build to 1/N of surplus so idle
// cash scales the fleet gradually instead of draining into hulls at once.
const capacityFleetFillShare = 3

var (
	capacityEvidenceBasis  = fmt.Sprintf("recorded %.0f-real-hour fleet net per active ship game day", capacityHistoryWindow.Hours())
	capacityShipLossReason = fmt.Sprintf("capacity growth waits %.0f real hours after a ship loss", capacityHistoryWindow.Hours())
	capacityFleetFillBasis = "owner policy: below SMALL_FLEET_TARGET, idle cash funds the cheapest capable hull with a profitable first voyage, at most a third of surplus"
)

type CapacityGrowthInput struct {
	Hull                        Hull
	HullPricePence              int64
	InitialStoresPence          int64
	SurplusPence                int64
	AllTimeNetPence             int64
	WindowNetPence              int64
	ActiveShipGameDays          float64
	FleetSize                   int
	SmallFleetTarget            int
	FleetAverageHoldTons        float64
	FleetFillRatio              float64
	LotCashPence                int64
	IncrementalDailyUpkeepPence int64
	ReservePence                int64
	MaxPaybackDays              float64
	HasSpareWork                bool
	NewestShipProven            bool
}

// Below SmallFleetTarget the owner wants idle cash turned into hulls that have
// work: a build needs a profitable first voyage (gossip of other companies'
// prices counts) but no earnings record, its capital may use at most a third
// of the surplus, and reserves plus every ship's working cash stay untouched.
// From the target onward, recorded earnings must repay the capital within
// MaxPaybackDays.
func EvaluateCapacityGrowth(in CapacityGrowthInput) GrowthDecision {
	decision := GrowthDecision{}
	guns := min(int64(4), in.Hull.MaxCannons)
	if in.Hull.CanSealDispatches || in.HullPricePence <= 0 || guns < 1 || in.Hull.HoldCapacityTons <= 2*guns {
		decision.Reason = "capacity growth requires a priced trading hull with room for cannons and cargo"
		return decision
	}
	fill := in.FleetSize < in.SmallFleetTarget
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
		EvidenceBasis:          capacityFleetFillBasis,
	}
	if !in.HasSpareWork {
		decision.Reason = "capacity growth needs an executable profitable voyage after incumbent commitments"
		return decision
	}
	if !fill {
		if reason := capacityFleetBlockReason(in); reason != "" {
			decision.Reason = reason
			return decision
		}
		rate := float64(in.WindowNetPence) / in.ActiveShipGameDays
		if in.FleetFillRatio >= 0.8 && in.FleetAverageHoldTons > 0 {
			rate *= min(2, max(0.5, float64(in.Hull.HoldCapacityTons)/in.FleetAverageHoldTons))
		}
		net := rate - float64(in.IncrementalDailyUpkeepPence)
		if net <= 0 || math.IsNaN(net) || math.IsInf(net, 0) {
			decision.Reason = "capacity growth marginal fleet upkeep consumes the observed per-ship earning rate"
			return decision
		}
		decision.IncrementalNetPencePerGameDay = net
		decision.PaybackGameDays = float64(capital) / net
		decision.EvidenceBasis = capacityEvidenceBasis
		if in.MaxPaybackDays <= 0 || math.IsNaN(in.MaxPaybackDays) || math.IsInf(in.MaxPaybackDays, 0) || decision.PaybackGameDays > in.MaxPaybackDays {
			decision.Reason = "capacity growth hull, slippage, cannon and store capital exceeds maximum game-day payback"
			return decision
		}
	}
	if in.FleetSize < 0 || in.LotCashPence < 0 ||
		in.LotCashPence > 0 && int64(in.FleetSize) >= math.MaxInt64/in.LotCashPence {
		decision.Reason = "capacity growth working cash exceeds integer-pence bounds"
		return decision
	}
	buffer := growthCashTotal(max(in.ReservePence, ceilBPS(in.HullPricePence, 1_500)), (int64(in.FleetSize)+1)*in.LotCashPence)
	decision.WorkingCapitalPence = buffer
	required := growthCashTotal(capital, buffer)
	if required == math.MaxInt64 || in.SurplusPence < required {
		decision.Reason = "capacity growth must preserve quote reserves and a cash lot for every ship after the build"
		return decision
	}
	if fill && capital > in.SurplusPence/capacityFleetFillShare {
		decision.Reason = "capacity growth below the fleet target spends at most a third of surplus cash per hull"
		return decision
	}
	decision.Accepted = true
	decision.Reason = "recorded fleet earnings cover capacity payback, marginal upkeep and every ship's working cash"
	if fill {
		decision.Reason = "fleet below SMALL_FLEET_TARGET: the hull costs at most a third of surplus and keeps every ship's working cash"
	}
	return decision
}

func capacityFleetBlockReason(in CapacityGrowthInput) string {
	if !in.NewestShipProven {
		return "capacity growth waits for the newest ship to complete a positive delivery"
	}
	if in.AllTimeNetPence <= 0 {
		return "capacity growth requires positive all-time recorded realized net"
	}
	if in.ActiveShipGameDays < 1 || math.IsNaN(in.ActiveShipGameDays) || math.IsInf(in.ActiveShipGameDays, 0) || in.WindowNetPence <= 0 {
		return fmt.Sprintf("capacity growth needs positive %.0f-real-hour recorded net and at least one active ship game day", capacityHistoryWindow.Hours())
	}
	return ""
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

type capacityIntent struct {
	BuildPort       string    `json:"build_port"`
	BuildClass      string    `json:"build_class"`
	QuoteLimitPence int64     `json:"quote_limit_pence"`
	AdmittedAt      time.Time `json:"admitted_at"`
	RefitAttempts   int       `json:"refit_attempts,omitempty"`
	RefitRefused    bool      `json:"refit_refused,omitempty"`
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

// fleetBelowTarget reports whether owner fleet-fill policy applies: a relocating
// ship then no longer pauses growth, because every ship's working cash is
// still reserved before a build.
func (r *Runner) fleetBelowTarget() bool {
	active := 0
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status != "sunk" {
			active++
		}
	}
	return active < r.opts.SmallFleetTarget
}

func (r *Runner) capacityFleetInput(ctx context.Context, evidence *growthEvidence) (CapacityGrowthInput, string, error) {
	input := CapacityGrowthInput{
		SurplusPence:     evidence.available,
		SmallFleetTarget: r.opts.SmallFleetTarget,
		ReservePence:     r.opts.ReservePence,
		MaxPaybackDays:   r.opts.CapacityPaybackDays,
	}
	windowStart := evidence.asOf.Add(-capacityHistoryWindow)
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
	var newest ShipState
	var newestCreatedAt time.Time
	var totalHold float64
	allHoldsKnown, allCreationTimesKnown := true, true
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		input.FleetSize++
		activeSince := windowStart
		if ship.Ship.CreatedAt == nil || ship.Ship.CreatedAt.IsZero() {
			allCreationTimesKnown = false
		} else {
			if ship.Ship.CreatedAt.After(activeSince) {
				activeSince = *ship.Ship.CreatedAt
			}
			if ship.Ship.CreatedAt.After(newestCreatedAt) {
				newestCreatedAt = *ship.Ship.CreatedAt
				newest = ship
			}
		}
		input.ActiveShipGameDays += max(0, evidence.asOf.Sub(activeSince).Seconds()) * scale / 86_400
		hold := ship.HoldCapacityTons
		if hold <= 0 {
			if hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass); exists {
				hold = hull.HoldCapacityTons
			}
		}
		if hold <= 0 {
			allHoldsKnown = false
		} else {
			totalHold += float64(hold)
		}
	}
	if !newestCreatedAt.IsZero() && evidence.asOf.Before(newestCreatedAt.Add(90*time.Minute)) {
		r.wakeBy(newestCreatedAt.Add(90 * time.Minute))
		return input, "capacity growth waits 90 real minutes after the newest ship was created", nil
	}
	if input.FleetSize > 0 && allHoldsKnown {
		input.FleetAverageHoldTons = totalHold / float64(input.FleetSize)
	}
	lifetime, err := r.db.EconomicHistory(ctx, r.account.ID, "")
	if err != nil {
		return input, "", err
	}
	if lifetime.UnknownLossPence < 0 && lifetime.NetPence < math.MinInt64-lifetime.UnknownLossPence {
		return input, "", errors.New("capacity growth all-time net exceeds integer-pence bounds")
	}
	input.AllTimeNetPence = lifetime.NetPence + lifetime.UnknownLossPence
	window, err := r.db.EconomicHistoryWindow(ctx, r.account.ID, "", evidence.asOf, capacityHistoryWindow)
	if err != nil {
		return input, "", err
	}
	if window.FutureCompletion {
		return input, "capacity growth rejects future-dated settlement evidence", nil
	}
	// Settlements with unknown costs (routine for freight settled outside our
	// delivery) are charged at their recorded loss instead of pausing growth:
	// they can only lower the earnings estimate, never raise it.
	if window.UnknownLossPence < 0 && window.NetPence < math.MinInt64-window.UnknownLossPence {
		return input, "", errors.New("capacity growth window net exceeds integer-pence bounds")
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
	input.WindowNetPence = window.NetPence + window.UnknownLossPence
	input.NewestShipProven = input.FleetSize == 1
	proofShipID := ""
	if input.FleetSize > 1 && allCreationTimesKnown {
		proofShipID = newest.Ship.ID
	}
	deliveries, err := r.db.CapacityDeliveryHistory(ctx, r.account.ID, windowStart, evidence.asOf, proofShipID, newestCreatedAt)
	if err != nil {
		return input, "", err
	}
	lots := make([]int64, 0, min(10, len(deliveries)))
	var deliveredTons, effectiveHold float64
	fillKnown := true
	for _, delivery := range deliveries {
		if delivery.CostsKnown && len(lots) < 10 {
			lots = append(lots, growthCashTotal(delivery.AcquisitionPence, delivery.FeesPence, delivery.ProvisionsPence))
		}
		cost := growthCashTotal(delivery.AcquisitionPence, delivery.FeesPence, delivery.ProvisionsPence, delivery.ExpensesPence)
		if proofShipID != "" && !delivery.CompletedAt.Before(newestCreatedAt) && delivery.ShipID == proofShipID && delivery.CostsKnown && delivery.ProceedsPence > cost {
			input.NewestShipProven = true
		}
		if delivery.CompletedAt.Before(windowStart) {
			continue
		}
		ship, exists := r.ship(delivery.ShipID)
		if !exists || ship.EffectiveHoldCapacityTons <= 0 {
			fillKnown = false
		} else {
			effectiveHold += float64(ship.EffectiveHoldCapacityTons)
		}
		deliveredTons += float64(delivery.QuantityTons)
	}
	if fillKnown && effectiveHold > 0 {
		input.FleetFillRatio = min(1, max(0, deliveredTons/effectiveHold))
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
	if input.FleetSize < input.SmallFleetTarget {
		return input, "", nil
	}
	return input, capacityFleetBlockReason(input), nil
}

func (r *Runner) capacityAtYard(ctx context.Context, port Port, hull Hull, quote YardQuote, input CapacityGrowthInput, evidence *growthEvidence) (GrowthDecision, map[string]int64, error) {
	planning, err := r.committedPlanningInputExcept(ctx, growthCandidate(port, hull), "__growth")
	if err != nil {
		return GrowthDecision{}, nil, err
	}
	// Fleet net already prices incumbents; admission separately charges the
	// candidate's nonlinear marginal upkeep, as the lane-growth path does.
	planning.FleetDailyUpkeepPence = 0
	plans := PlanVoyages(planning)
	input.Hull = hull
	input.HullPricePence = *quote.TotalCostPence
	fill := input.FleetSize < input.SmallFleetTarget
	for _, plan := range plans {
		basis := plan.Economics.RevenueBasis
		// Below the fleet target, recent gossip of other companies' bids is
		// enough evidence that the new hull has a profitable first trade.
		if plan.Accepted && (basis == "depth" || basis == "contract" || fill && basis == "gossip") {
			input.HasSpareWork = true
			input.InitialStoresPence = max(plan.Economics.StoreCashPence, plan.Economics.ProvisionsPence)
			break
		}
	}
	decision := EvaluateCapacityGrowth(input)
	if !decision.Accepted {
		return decision, nil, nil
	}
	required := clonePence(evidence.required)
	capital := growthCashTotal(decision.QuotedHullPence, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
	required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], capital)
	_, fees, _, funded := measuredFunding(evidence.balances, required, planning.Rates)
	if !funded {
		decision.Accepted = false
		decision.Reason = "capacity growth cannot fund the yard while preserving incumbent commitments and regional reserves"
	} else if fees > 0 {
		input.SurplusPence -= fees
		decision = EvaluateCapacityGrowth(input)
		decision.FundingFeesPence = fees
	}
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
	found, evaluated := false, false
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	for _, port := range r.stateCache.Catalog.Ports {
		if !port.HasShipyard || port.Phase > r.stateCache.Clock.UnlockedPhase {
			continue
		}
		for _, hull := range r.stateCache.Catalog.ShipClasses {
			if hull.CanSealDispatches || hull.Phase > r.stateCache.Clock.UnlockedPhase {
				continue
			}
			quote, err := r.growthQuote(port, hull)
			if err != nil {
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
			if decision.Accepted && (!found || betterCapacityCandidate(decision, bestDecision)) ||
				!found && (!evaluated || betterCapacityCandidate(decision, bestDecision)) {
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
	if !found {
		return false, r.hold(ctx, "", bestDecision.Reason)
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
	return r.startCapacityGrowth(ctx, bestPort, bestHull, bestDecision)
}

func (r *Runner) startCapacityGrowth(ctx context.Context, port Port, hull Hull, decision GrowthDecision) (bool, error) {
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
		},
		BuildName: name, BuildClass: hull.ID, BuildPort: port.ID,
		TargetCannons: min(4, hull.MaxCannons), CostsKnown: true,
		StartedAt: now, StartedGameHours: r.stateCache.Clock.Calendar.TotalGameHours,
		InitialLedgerIDs: r.ledgerIDs(),
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
	// Turnkey builds have no atomic price cap. Requote after all planning so
	// only the request round trip separates this check from the first send.
	var live YardQuote
	path := "/v1/shipyard/quote?port_id=" + url.QueryEscape(port.ID) + "&ship_class=" + url.QueryEscape(hull.ID) + "&build_mode=turnkey_cash"
	if err := r.api.Get(ctx, path, &live); err != nil {
		return fmt.Errorf("refresh capacity build price before first send: %w", err)
	}
	// Admission used the cached quote; a live refusal would otherwise readmit
	// the same yard and hull every pass, starving ordinary ship missions.
	refuse := func(reason string) error {
		if err := r.recordGrowthBuildRefusal(ctx, data); err != nil {
			return err
		}
		return errors.New(reason)
	}
	if live.PortID != port.ID || live.ShipClass != hull.ID || live.BuildMode != "turnkey_cash" || !live.Available ||
		live.TotalCostPence == nil || *live.TotalCostPence <= 0 || *live.TotalCostPence > intent.QuoteLimitPence {
		return refuse("capacity build live turnkey quote is unavailable, mismatched or above the admitted ceiling")
	}
	if live.MaterialsPence < 0 || live.NPCMaterialsPence < 0 || live.NPCMaterialsPence > live.MaterialsPence ||
		live.MaterialsPence-live.NPCMaterialsPence > intent.QuoteLimitPence-*live.TotalCostPence {
		return refuse("capacity build live player-sourced materials exceed the remaining price headroom")
	}
	yardRequired := growthCashTotal(required[port.BankingRegion], max(0, *live.TotalCostPence-*quote.TotalCostPence-decision.SlippageReservePence))
	if yardRequired == math.MaxInt64 || evidence.balances[port.BankingRegion] < yardRequired {
		return refuse("capacity build live quote would spend protected regional yard funding")
	}
	return nil
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
