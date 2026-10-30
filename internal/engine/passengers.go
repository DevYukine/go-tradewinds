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
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

// Passage promises and ladder state live in the same durable mission as their
// immutable actions. An offer is not a promise; a boarded group is.
type passageIntent struct {
	FloorPence       int64                              `json:"floor_pence"`
	FarePence        int64                              `json:"fare_pence"`
	MaxBerths        int64                              `json:"max_berths"`
	OfferID          string                             `json:"offer_id"`
	StepHour         int64                              `json:"step_hour"`
	StepOpenedAt     time.Time                          `json:"step_opened_at"`
	StepGroups       []gameapi.PassengerWaitingGroup    `json:"step_groups"`
	OfferBaselineID  string                             `json:"offer_baseline_id,omitempty"`
	BoardBy          time.Time                          `json:"board_by"`
	Reoffer          bool                               `json:"reoffer"`
	Abandon          bool                               `json:"abandon"`
	Groups           []gameapi.PassengerAboardGroup     `json:"groups"`
	FirstBoardedAt   time.Time                          `json:"first_boarded_at"`
	WaitUntil        time.Time                          `json:"wait_until"`
	StressHours      float64                            `json:"stress_hours"`
	Measurements     map[string]store.PassengerEvidence `json:"measurements,omitempty"`
	Receipts         []*store.Settlement                `json:"receipts,omitempty"`
	ReceiptsPrepared bool                               `json:"receipts_prepared"`
	RetryOffer       bool                               `json:"-"`
	QuarantineReason string                             `json:"quarantine_reason,omitempty"`
	RecoveryOnly     bool                               `json:"recovery_only,omitempty"`
	RecoveryReason   string                             `json:"recovery_reason,omitempty"`
}

type passengerOfferRequest struct {
	ShipID      string `json:"ship_id"`
	Destination string `json:"dest_port_id"`
	Fare        int64  `json:"fare_per_head_pence"`
	MaxBerths   int64  `json:"max_berths"`
}

type passengerEventPayload struct {
	ShipID            string     `json:"ship_id"`
	GroupID           string     `json:"group_id"`
	Kind              string     `json:"kind"`
	Origin            string     `json:"origin_port_id"`
	Destination       string     `json:"dest_port_id"`
	Heads             int64      `json:"heads"`
	FarePerHeadPence  int64      `json:"fare_per_head_pence"`
	FullFarePence     int64      `json:"full_fare_pence"`
	FarePaidPence     *int64     `json:"fare_paid_pence"`
	GameDaysLate      *float64   `json:"game_days_late"`
	BoardedAt         *time.Time `json:"boarded_at"`
	PromisedArrivalAt *time.Time `json:"promised_arrival_at"`
	Timestamp         time.Time  `json:"timestamp"`
	Cause             string     `json:"cause"`
}

func decodePassengerEvent(raw json.RawMessage, out *passengerEventPayload) bool {
	var envelope struct {
		Payload   json.RawMessage `json:"payload"`
		Timestamp time.Time       `json:"timestamp"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	if len(envelope.Payload) != 0 {
		raw = envelope.Payload
	}
	if json.Unmarshal(raw, out) != nil {
		return false
	}
	if out.Timestamp.IsZero() {
		out.Timestamp = envelope.Timestamp
	}
	return true
}

// Recording time is not source time: a replay after an SSE gap must not pay
// the ship's newer voyage. Untagged terminal events require their source time.
func passengerTerminalAt(payload passengerEventPayload, event store.Event) (time.Time, bool) {
	if !payload.Timestamp.IsZero() {
		return payload.Timestamp, true
	}
	if payload.GroupID == "" {
		return time.Time{}, false
	}
	return event.CreatedAt, !event.CreatedAt.IsZero()
}

func (r *Runner) quarantinePassage(ctx context.Context, mission *store.Mission, data *missionData, cause error) (bool, error) {
	if data.Passage == nil {
		data.Passage = &passageIntent{}
	}
	if data.Passage.QuarantineReason != "" {
		return false, r.hold(ctx, mission.ShipID, "passenger passage remains quarantined: "+cause.Error())
	}
	data.Passage.QuarantineReason = cause.Error()
	data.Passage.RecoveryOnly = false
	if err := r.saveMission(ctx, mission, data); err != nil {
		return false, r.hold(ctx, mission.ShipID, "passenger integrity quarantine could not be persisted: "+err.Error())
	}
	if err := r.ensurePassengerBoardingHint(ctx, mission.ShipID); err != nil {
		return false, r.hold(ctx, mission.ShipID, "passenger quarantine boarding hint could not be persisted: "+err.Error())
	}
	if err := r.decision(ctx, mission.ShipID, "passage_quarantined", cause.Error(), map[string]any{"mission_id": mission.Generation}); err != nil {
		return false, err
	}
	if data.Action == nil {
		if err := r.archiveQuarantinedPassage(ctx, mission, data); err != nil {
			return false, r.hold(ctx, mission.ShipID, "passenger quarantine accounting marker deferred: "+err.Error())
		}
	}
	return false, nil
}

func passengerIntegrityFailure(err error) bool {
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	return errors.Is(err, store.ErrActionConflict) || errors.Is(err, store.ErrInvalidInput) ||
		errors.Is(err, store.ErrNotFound) || errors.As(err, &syntax) || errors.As(err, &shape)
}

func (r *Runner) quarantinePassageError(ctx context.Context, mission *store.Mission, data *missionData, changed *bool, err *error) {
	if *err == nil {
		return
	}
	cause := *err
	if data.Passage != nil && data.Passage.QuarantineReason != "" {
		*changed, *err = false, r.hold(ctx, mission.ShipID, "quarantined passenger recovery deferred: "+cause.Error())
	} else if passengerIntegrityFailure(cause) {
		*changed, *err = r.quarantinePassage(ctx, mission, data, cause)
	} else {
		*changed, *err = false, r.hold(ctx, mission.ShipID, "passenger work deferred; original intent retained for retry: "+cause.Error())
	}
}

func (r *Runner) passengerAdmissionError(ctx context.Context, shipID string, changed *bool, err *error) {
	if *err == nil {
		return
	}
	cause := *err
	if !passengerIntegrityFailure(cause) {
		*changed, *err = false, r.hold(ctx, shipID, "passenger admission deferred: "+cause.Error())
		return
	}
	if mission, data, loadErr := r.loadMission(ctx, shipID); loadErr == nil && mission != nil && data.Passage != nil {
		*changed, *err = r.quarantinePassage(ctx, mission, &data, cause)
	} else {
		*changed, *err = false, r.decision(ctx, shipID, "passage_quarantined", cause.Error(), nil)
	}
}

func passengerStage(stage string) bool {
	return stage == "passenger-offer" || stage == "passenger-cancel"
}

func passengerFloor(cost, heads, hazardBPS int64) (int64, error) {
	if cost < 0 || cost > maximumPence || heads <= 0 || hazardBPS < 0 || hazardBPS >= 10_000 {
		return 0, errors.New("passage cost, whole-group size or published hazard bound is unavailable")
	}
	// Solve fare * heads - ceil(fare * heads * bound) >= cost. The
	// published Lloyd's expected-claim upper bound is conservative risk pricing,
	// not an invented piracy encounter probability.
	total := ceilRatio(cost, 10_000, 10_000-hazardBPS)
	if total < 0 || total == math.MaxInt64 {
		return 0, errors.New("passage fare overflows integer pence")
	}
	return max(1, ceilRatio(total, 1, heads)), nil
}

// Cover the full three-game-hour ladder at the published in-port crew rate.
func passengerLadderConsumption(hull Hull) float64 {
	return float64(hull.CrewRequired) / 200 * (3.0 / 24)
}

func passengerFareAllowed(fare, floor int64) bool {
	return floor > 0 && fare >= floor && fare <= maximumPence
}

func passengerNextFare(current, floor int64, hoursRemaining float64) int64 {
	if current <= floor {
		return floor
	}
	if hoursRemaining < 3 {
		return floor
	}
	next := current/100*85 + current%100*85/100
	// Do not create a price from which the floor cannot be reached by a legal
	// <= 0.85 step. Go directly to the floor instead.
	if next < floor || next < ceilRatio(floor, 100, 85) {
		return floor
	}
	return next
}

func passengerCapacity(ship ShipState, hull Hull, storesToBuy int64, groups []gameapi.PassengerWaitingGroup, destination string, now time.Time) (smallest, largest int64, boardBy time.Time) {
	if hull.PassengersPerTon <= 0 {
		return 0, 0, time.Time{}
	}
	freeTons := max(0, ship.EffectiveHoldCapacityTons-ship.CargoUsedTons-storesToBuy)
	capacity := min(int64(10_000), checkedPenceProduct(freeTons, hull.PassengersPerTon))
	for _, group := range groups {
		if group.Size <= 0 || group.Size > capacity || group.DestPortID != destination || group.OriginPortID != ship.Ship.CurrentPortID || !now.Before(group.BoardBy) {
			continue
		}
		if smallest == 0 || group.Size < smallest {
			smallest = group.Size
		}
		largest = max(largest, group.Size)
		if boardBy.IsZero() || group.BoardBy.Before(boardBy) {
			boardBy = group.BoardBy
		}
	}
	return
}

func passageRouteHours(route Route, hull Hull) (float64, bool) {
	if hull.BaseSpeedCentiknots <= 0 || route.TotalDistanceNM <= 0 {
		return 0, false
	}
	// The existing cargo planner's fully laden 0.8-speed bound is also the
	// passenger stress passage. Passenger unloading requires no berth queue.
	hours := float64(route.TotalDistanceNM) / (float64(hull.BaseSpeedCentiknots) / 100 * 0.8)
	return hours, hours > 0 && !math.IsInf(hours, 0) && !math.IsNaN(hours)
}

func passengerRouteSafe(in PlanningInput, route Route, marques []letterOfMarque) error {
	piracy, complete := hazardRoutePiracy(in, route)
	if !complete || !in.fresh("zones") || !in.fresh("marques") {
		return errors.New("passage needs authoritative piracy and marque observations")
	}
	_, upper, ok := in.serverBounds()
	if !ok {
		return errors.New("passage needs authoritative clock")
	}
	for zone, bps := range piracy {
		if bps < 0 || bps >= 1_000 {
			return fmt.Errorf("passage piracy in %s is at least 1000 bps or unknown", zone)
		}
		var ownBPS, ownCount int64
		for _, marque := range marques {
			if marque.SeaZoneID != zone || !marque.ExpiresAt.After(upper) {
				continue
			}
			if marque.SponsorCompanyID != in.CompanyID {
				return errors.New("passage crosses an active foreign marque")
			}
			ownBPS += marque.AddedPiracyBPS
			ownCount++
		}
		var observed ZoneObservation
		exists := false
		for _, candidate := range in.Zones {
			if candidate.SeaZoneID == zone {
				observed, exists = candidate, true
				break
			}
		}
		if !exists || observed.MarquePiracyBPS == nil || observed.ActiveMarques == nil ||
			*observed.MarquePiracyBPS < 0 || *observed.MarquePiracyBPS > 5_000 || *observed.ActiveMarques < 0 {
			return errors.New("passage needs public marque totals")
		}
		if *observed.ActiveMarques != ownCount || *observed.MarquePiracyBPS != min(int64(5_000), ownBPS) {
			return errors.New("passage crosses a foreign marque or inconsistent public marque totals")
		}
	}
	return nil
}

type passengerQuote struct {
	Plan                   Plan
	Floor, Ceiling, Berths int64
	BoardBy                time.Time
	StressHours            float64
}

func quotePassage(in PlanningInput, destination string, groups []gameapi.PassengerWaitingGroup, fares []gameapi.PassengerFare, marques []letterOfMarque) (passengerQuote, error) {
	var quote passengerQuote
	hull, exists := findHull(in.Catalog, in.Ship.Ship.ShipClass)
	if !exists || hull.PassengersPerTon <= 0 {
		return quote, errors.New("passenger hull density is not published")
	}
	route, err := observedPlanningRoute(in, in.Ship.Ship.CurrentPortID, destination, hull)
	if err != nil {
		return quote, err
	}
	if err := passengerRouteSafe(in, route, marques); err != nil {
		return quote, err
	}
	hours, ok := passageRouteHours(route, hull)
	if !ok {
		return quote, errors.New("passage stress hours are unavailable")
	}
	for _, key := range []string{"books/" + route.FromPort, "ports/" + route.FromPort, "ports/" + destination} {
		if !in.fresh(key) {
			return quote, errors.New("passage needs current provisions depth and harbour dues")
		}
	}
	_, upper, clockOK := in.serverBounds()
	if !clockOK || in.Clock.Calendar.TimeScale <= 0 {
		return quote, errors.New("passage needs current clock")
	}
	// Reposition to the cheapest observed legal base (origin or a catalogued
	// hub). A destination hub needs no ballast repositioning at all.
	returnCost := int64(math.MaxInt64)
	returnStores := 0.0
	for _, base := range in.Catalog.Ports {
		if base.ID != route.FromPort && !base.IsHub {
			continue
		}
		if base.ID == destination {
			returnCost, returnStores = 0, 0
			break
		}
		back, err := observedPlanningRoute(in, destination, base.ID, hull)
		if err != nil || !in.fresh("ports/"+base.ID) {
			continue
		}
		backHours, valid := passageRouteHours(back, hull)
		if !valid {
			continue
		}
		consumed := float64(hull.CrewRequired) / 100 * backHours / 24
		cost, err := provisionsExpense(in.Markets[route.FromPort], consumed)
		if err != nil {
			continue
		}
		cost = growthCashTotal(cost, int64(math.Ceil(float64(in.Ports[base.ID].BerthDuesPerDayPence)*0.25)))
		if cost < returnCost {
			returnCost, returnStores = cost, back.SafeProvisionsTons
		}
	}
	if returnCost == math.MaxInt64 {
		return quote, errors.New("passage repositioning back to a base is not priced")
	}
	target := route.SafeProvisionsTons + returnStores + 1 + passengerLadderConsumption(hull)
	stores := max(0, int64(math.Ceil(target-in.Ship.Stores.ProvisionsTons)))
	// Groups leaving before a full price hour cannot participate in this ladder;
	// they must not poison an otherwise viable destination's deadline.
	eligible := make([]gameapi.PassengerWaitingGroup, 0, len(groups))
	firstHour := upper.Add(gameDuration(1.0/24, in.Clock.Calendar.TimeScale))
	for _, group := range groups {
		if !group.BoardBy.Before(firstHour) {
			eligible = append(eligible, group)
		}
	}
	smallest, largest, boardBy := passengerCapacity(in.Ship, hull, stores, eligible, destination, upper)
	if smallest == 0 {
		return quote, errors.New("no whole waiting group fits after safe stores")
	}
	if upper.Add(gameDuration(1.0/24, in.Clock.Calendar.TimeScale)).After(boardBy) {
		return quote, errors.New("waiting group leaves before one full ladder hour")
	}
	consumption := float64(hull.CrewRequired)/100*hours/24 + passengerLadderConsumption(hull)
	provisions, err := provisionsExpense(in.Markets[route.FromPort], consumption)
	if err != nil {
		return quote, err
	}
	berth := int64(math.Ceil(float64(in.Ports[destination].BerthDuesPerDayPence) * 0.25))
	cost := growthCashTotal(provisions, berth, returnCost)
	bps, source, priced := planHazard(in, route)
	if !priced {
		return quote, errors.New(hazardUnavailable)
	}
	floor, err := passengerFloor(cost, smallest, bps)
	if err != nil {
		return quote, err
	}
	ceiling := floor
	for _, fare := range fares {
		if fare.OriginPortID == route.FromPort && fare.DestPortID == destination && fare.FarePerHeadPence > 0 {
			ceiling = max(ceiling, growthCashTotal(fare.FarePerHeadPence, ceilRatio(fare.FarePerHeadPence, 1, 2)))
		}
	}
	// A live freight award supplies the analog; never invent a Cr/ton/nm rate.
	for _, contract := range in.Contracts {
		if contract.ContractType == "freight" && !contract.Stale && contract.OriginPortID == route.FromPort && contract.DestPortID == destination && contract.QuantityRequired > 0 && contract.RewardPence > 0 {
			ceiling = max(ceiling, checkedPenceProduct(ceilRatio(contract.RewardPence, 1, contract.QuantityRequired), 4))
		}
	}
	if ceiling > maximumPence {
		return quote, errors.New("passage ceiling exceeds checked integer-pence bounds")
	}
	if ceiling < ceilRatio(floor, 100, 85) || !upper.Add(gameDuration(3.0/24, in.Clock.Calendar.TimeScale)).Before(boardBy) {
		ceiling = floor
	}
	plan := Plan{Kind: "passage", ShipID: in.Ship.Ship.ID, Origin: route.FromPort, Destination: destination, TargetStoresTons: target, StoresToBuy: stores, Accepted: true,
		Reason: "idle-ship passage; whole-group cost floor with priced ballast reposition and Lloyd's bound", PriceSource: "passenger public fares and observed freight analog", Economics: Economics{Basis: "passage", RevenueBasis: "passenger_offer", ProvisionsPence: provisions, BerthDuesPence: berth, HazardBPS: bps, HazardSource: source, GameDays: hours / 24}}
	if stores > 0 {
		order, err := quoteProvisions(in, target)
		if err != nil {
			return quote, err
		}
		origin, _ := findPort(in.Catalog, route.FromPort)
		if in.Balances[origin.BankingRegion]-order.cash < in.ReserveByRegion[origin.BankingRegion] {
			return quote, errors.New("passage safe stores would spend protected regional reserves")
		}
		plan.StoresLimitPence, plan.Economics.StoreCashPence = order.limit, order.cash
	}
	quote = passengerQuote{Plan: plan, Floor: floor, Ceiling: ceiling, Berths: largest, BoardBy: boardBy, StressHours: hours}
	return quote, nil
}

func (r *Runner) passengerGroups(port string) []gameapi.PassengerWaitingGroup {
	values := decode[map[string]json.RawMessage](r.snapshot, "passenger_waiting")
	var groups []gameapi.PassengerWaitingGroup
	_ = json.Unmarshal(values[port], &groups)
	return groups
}

func (r *Runner) passengerAboard(ship string) []gameapi.PassengerAboardGroup {
	values := decode[map[string]json.RawMessage](r.snapshot, "passenger_aboard")
	var groups []gameapi.PassengerAboardGroup
	_ = json.Unmarshal(values[ship], &groups)
	return groups
}

func (r *Runner) passengerFares(origin, destination string) []gameapi.PassengerFare {
	values := decode[map[string]json.RawMessage](r.snapshot, "passenger_fares")
	var fares []gameapi.PassengerFare
	_ = json.Unmarshal(values[origin+"|"+destination], &fares)
	return fares
}

func (r *Runner) considerPassage(ctx context.Context, ship ShipState, input PlanningInput, missions map[string]*store.Mission) (changed bool, err error) {
	defer r.passengerAdmissionError(ctx, ship.Ship.ID, &changed, &err)
	if !hazardStationary(ship) || !emptyCargo(ship) || !r.fresh("passenger_waiting/"+ship.Ship.CurrentPortID) || !r.fresh("passenger_offers") {
		return false, nil
	}
	// A retained offer from a crash is recovered, never blindly posted twice.
	for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
		if offer.ShipID == ship.Ship.ID && offer.Status == "open" {
			return r.recoverPassageOffer(ctx, ship, offer)
		}
	}
	destinations := make(map[string]bool)
	for _, group := range r.passengerGroups(ship.Ship.CurrentPortID) {
		destinations[group.DestPortID] = true
	}
	ordered := make([]string, 0, len(destinations))
	for destination := range destinations {
		ordered = append(ordered, destination)
	}
	sort.Strings(ordered)
	var selected passengerQuote
	for _, destination := range ordered {
		refusals := decode[map[string][]string](r.snapshot, "_passenger_refusals")
		if refused, exists := refusals[ship.Ship.ID+"|"+ship.Ship.CurrentPortID+"|"+destination]; exists &&
			slices.Equal(refused, r.passengerLaneGroupIDs(ship.Ship.CurrentPortID, destination)) {
			continue
		}
		competing := false
		for otherID, mission := range missions {
			if otherID == ship.Ship.ID {
				continue
			}
			var data missionData
			if json.Unmarshal(mission.Data, &data) == nil && data.Passage != nil && data.Plan.Origin == ship.Ship.CurrentPortID && data.Plan.Destination == destination {
				competing = true
			}
		}
		for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
			competing = competing || offer.ShipID != ship.Ship.ID && offer.Status == "open" && offer.PortID == ship.Ship.CurrentPortID && offer.DestPortID == destination
		}
		if competing {
			continue
		}
		evidence, err := r.db.PassengerEvidenceHistory(ctx, r.account.ID, ship.Ship.CurrentPortID+"|"+destination, 100)
		if err != nil {
			return false, err
		}
		unsafePromise := false
		for _, measured := range evidence {
			if measured.LeewayGameHours != nil {
				unsafePromise = *measured.LeewayGameHours < 0
				break
			}
		}
		if unsafePromise {
			continue
		}
		quote, err := quotePassage(input, destination, r.passengerGroups(ship.Ship.CurrentPortID), r.passengerFares(ship.Ship.CurrentPortID, destination), decode[[]letterOfMarque](r.snapshot, "marques"))
		if err != nil {
			continue
		}
		if selected.Floor == 0 || quote.Plan.Economics.GameDays < selected.Plan.Economics.GameDays {
			selected = quote
		}
	}
	if selected.Floor == 0 {
		return false, nil
	}
	if err := r.insuranceActionReady(ship, "depart", selected.Plan.Destination); err != nil {
		return false, nil
	}
	mission, data := r.newVoyage(ship, selected.Plan)
	mission.Phase = "passenger_loading"
	data.Passage = &passageIntent{FloorPence: selected.Floor, FarePence: selected.Ceiling, MaxBerths: selected.Berths, BoardBy: selected.BoardBy, StressHours: selected.StressHours}
	if err := r.decision(ctx, ship.Ship.ID, "passage_floor", data.Plan.Reason, map[string]any{"floor_per_head_pence": selected.Floor, "ceiling_per_head_pence": selected.Ceiling, "max_berths": selected.Berths, "hazard_source": selected.Plan.Economics.HazardSource}); err != nil {
		return false, err
	}
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) recoverPassageOffer(ctx context.Context, ship ShipState, offer gameapi.PassengerOffer) (changed bool, err error) {
	defer r.passengerAdmissionError(ctx, ship.Ship.ID, &changed, &err)
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: offer.PortID, Destination: offer.DestPortID})
	mission.Phase = "passenger_loading"
	// Unknown historical floor cannot authorize a fresh price. Cancel the
	// orphaned offer, retaining its ship for authoritative aboard reconciliation.
	data.Passage = &passageIntent{OfferID: offer.ID, Abandon: true}
	data.CostsKnown = false
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) recoverPassage(ctx context.Context, ship ShipState) (changed bool, err error) {
	defer r.passengerAdmissionError(ctx, ship.Ship.ID, &changed, &err)
	if !r.fresh("passenger_aboard/" + ship.Ship.ID) {
		return false, r.hold(ctx, ship.Ship.ID, "passengers aboard require authoritative group promises before recovery")
	}
	groups := r.passengerAboard(ship.Ship.ID)
	if len(groups) == 0 {
		return false, r.hold(ctx, ship.Ship.ID, "passenger hold is occupied but no promises are available")
	}
	destination := groups[0].DestPortID
	for _, group := range groups {
		if group.DestPortID != destination {
			return false, r.hold(ctx, ship.Ship.ID, "passenger recovery refuses mixed destinations")
		}
	}
	mission, data := r.newVoyage(ship, Plan{Kind: "passage", ShipID: ship.Ship.ID, Origin: groups[0].OriginPortID, Destination: destination})
	mission.Phase = "passenger_loading"
	if ship.Ship.Status == "sailing" {
		mission.Phase = "sailing"
	}
	data.Passage = &passageIntent{Groups: groups, Abandon: true}
	for _, contract := range r.activeFreight(ship.Ship.ID) {
		data.Freight = append(data.Freight, obligationFromContract(contract))
	}
	data.CostsKnown = false
	return true, r.saveMission(ctx, mission, &data)
}

func (r *Runner) drivePassage(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (changed bool, err error) {
	defer r.quarantinePassageError(ctx, mission, data, &changed, &err)
	if data.Passage.QuarantineReason != "" {
		return r.driveQuarantinedPassage(ctx, ship, mission, data)
	}
	if data.Passage.RecoveryOnly {
		return r.drivePassengerRecovery(ctx, ship, mission, data)
	}
	passage := data.Passage
	if data.Settlement != nil && data.Settlement.ProceedsUnknown {
		return r.reconcilePassengerCompletion(ctx, ship, mission, data)
	}
	if changed, err := r.syncPassage(ctx, ship, mission, data); changed || err != nil {
		return changed, err
	}
	if passage.QuarantineReason != "" {
		return false, nil
	}
	if passage.ReceiptsPrepared {
		for _, receipt := range passage.Receipts {
			if err := r.db.PutSettlement(ctx, receipt); err != nil {
				return false, err
			}
		}
		hints := decode[map[string]bool](r.snapshot, "_passenger_boarding_ships")
		delete(hints, ship.Ship.ID)
		r.snapshot["_passenger_boarding_ships"] = encode(hints)
		for _, ledgerID := range data.SettlementExpenseIDs {
			if owner := r.expenseOwners[ledgerID]; owner != "" && owner != mission.Generation {
				return false, fmt.Errorf("passage expense has conflicting durable mission ownership: %w", store.ErrActionConflict)
			}
			r.expenseOwners[ledgerID] = mission.Generation
		}
		r.snapshot["_expense_allocations"] = encode(r.expenseOwners)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return false, err
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if time.Now().Before(data.CooldownUntil) {
		r.wakeBy(data.CooldownUntil)
		return false, r.hold(ctx, ship.Ship.ID, "passage waits for persisted retry cooldown")
	}
	if ship.Ship.Status == "sailing" {
		return false, r.holdVoyageArrival(ctx, ship, mission, data, "passenger passage in flight; waiting for fare receipts")
	}
	if len(passage.Groups) > 0 && ship.Ship.CurrentPortID == data.Plan.Destination {
		return r.reconcilePassengerCompletion(ctx, ship, mission, data)
	}
	if !r.fresh("passenger_offers") {
		return false, r.hold(ctx, ship.Ship.ID, "passage waits for authoritative offers")
	}
	offers := decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers")
	var open *gameapi.PassengerOffer
	for i := range offers {
		offer := &offers[i]
		if offer.ShipID != ship.Ship.ID || offer.Status != "open" {
			continue
		}
		// Cancel any other destination before honoring a boarded promise.
		if offer.DestPortID != data.Plan.Destination {
			return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": offer.ID})
		}
		if open != nil {
			return false, r.hold(ctx, ship.Ship.ID, "duplicate open passenger offers require reconciliation")
		}
		open = offer
	}
	if open != nil && passage.OfferID != open.ID {
		passage.OfferID = open.ID
		passage.Abandon = true // An unjournaled offer cannot inherit this ladder's floor.
		passage.Reoffer = false
		passage.FarePence, passage.MaxBerths = open.FarePerHeadPence, open.MaxBerths
		return true, r.saveMission(ctx, mission, data)
	}
	if open != nil && passage.Reoffer && len(passage.Groups) == 0 {
		return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": open.ID})
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	if len(passage.Groups) == 0 {
		// Open fare ladders do not reserve a ship against a newly admitted cargo
		// or charter opportunity. Cancel first; the normal planner wins next pass.
		plans := PlanVoyages(input)
		if len(plans) > 0 && plans[0].Accepted {
			passage.Abandon = true
		}
	}
	_, upper, clockOK := r.serverBounds()
	if !clockOK {
		return false, r.hold(ctx, ship.Ship.ID, "passage waits for clock authority")
	}
	if len(passage.Groups) > 0 {
		if open != nil {
			hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
			freeHeads := min(open.MaxBerths-open.BerthsTaken, checkedPenceProduct(max(0, ship.EffectiveHoldCapacityTons-ship.CargoUsedTons), hull.PassengersPerTon))
			fitting := false
			for _, group := range r.passengerGroups(ship.Ship.CurrentPortID) {
				fitting = fitting || r.fresh("passenger_waiting/"+ship.Ship.CurrentPortID) &&
					group.OriginPortID == ship.Ship.CurrentPortID && group.DestPortID == data.Plan.Destination &&
					group.Size > 0 && group.Size <= freeHeads && upper.Before(group.BoardBy)
			}
			wait := !passage.WaitUntil.IsZero() && upper.Before(passage.WaitUntil) && fitting
			arrival := upper.Add(gameDuration(passage.StressHours/24, r.stateCache.Clock.Calendar.TimeScale))
			for _, group := range passage.Groups {
				wait = wait && group.PromisedArrivalAt != nil && arrival.Before(*group.PromisedArrivalAt)
			}
			if wait {
				r.wakeAtServer(passage.WaitUntil, false)
				return false, r.hold(ctx, ship.Ship.ID, "passenger promise permits a bounded wait for one whole group")
			}
			return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": open.ID})
		}
		if err := r.passageDepartureReady(ship, data.Plan.Destination, data); err != nil {
			// An already-boarded promise is not improved by repeatedly refusing
			// to sail. Remaining holds here concern actual route/ship safety.
			return false, r.hold(ctx, ship.Ship.ID, err.Error())
		}
		hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
		route := r.stateCache.Routes[routeKey(ship.Ship.CurrentPortID, data.Plan.Destination, hull.ID)]
		if ship.Stores.ProvisionsTons+1e-9 < route.SafeProvisionsTons {
			return r.provisionMission(ctx, ship, mission, data, route.SafeProvisionsTons)
		}
		return true, r.stage(ctx, mission, data, ship, "depart", "/v1/fleet/depart", map[string]string{"ship_id": ship.Ship.ID, "dest_port_id": data.Plan.Destination})
	}
	if passage.Abandon || !upper.Before(passage.BoardBy) {
		if ship.PassengerHoldTons > 0 || open != nil && open.BerthsTaken > 0 {
			return false, r.hold(ctx, ship.Ship.ID, "passenger closure waits for authoritative boarded promises")
		}
		if open != nil {
			return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": open.ID})
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
	}
	if open == nil {
		if ship.PassengerHoldTons > 0 {
			return false, r.hold(ctx, ship.Ship.ID, "passenger hold waits for authoritative boarded promises")
		}
		if passage.OfferID != "" && !passage.Reoffer {
			return true, r.db.DeleteMission(ctx, r.account.ID, ship.Ship.ID)
		}
		if !r.shipUnlocked(ship) {
			return false, r.hold(ctx, ship.Ship.ID, "passenger loading waits for turnaround unlock")
		}
		// Price-ladder re-offers stay offshore: consuming a little hotel stock
		// must not re-dock the ship at every lower fare. Before the first offer,
		// load the outbound requirement, operating margin and ladder consumption.
		if passage.StepOpenedAt.IsZero() && !passage.Reoffer {
			hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
			route := r.stateCache.Routes[routeKey(ship.Ship.CurrentPortID, data.Plan.Destination, hull.ID)]
			target := route.SafeProvisionsTons + 1 + passengerLadderConsumption(hull)
			if ship.Stores.ProvisionsTons+1e-9 < target {
				return r.provisionMission(ctx, ship, mission, data, target)
			}
		}
		if ship.Ship.Status == "berthed" {
			return true, r.stage(ctx, mission, data, ship, "offshore", "/v1/fleet/offshore", map[string]string{"ship_id": ship.Ship.ID})
		}
		quote, err := quotePassage(input, data.Plan.Destination, r.passengerGroups(ship.Ship.CurrentPortID), r.passengerFares(ship.Ship.CurrentPortID, data.Plan.Destination), decode[[]letterOfMarque](r.snapshot, "marques"))
		if err != nil || passage.FarePence < quote.Floor || passage.MaxBerths > quote.Berths {
			passage.Abandon = true
			return true, r.saveMission(ctx, mission, data)
		}
		var newest []gameapi.PassengerOffer
		if err := r.api.Get(ctx, "/v1/passengers/offers?open_only=false&limit=1&ship_id="+url.QueryEscape(ship.Ship.ID), &newest); err != nil {
			return false, r.hold(ctx, ship.Ship.ID, "passenger offer awaits pre-send newest-offer fence")
		}
		if newest == nil || len(newest) > 1 || len(newest) == 1 && (newest[0].ID == "" || newest[0].ShipID != ship.Ship.ID) {
			return false, r.hold(ctx, ship.Ship.ID, "invalid passenger offer identity fence; awaiting authoritative newest offer")
		}
		passage.OfferBaselineID = ""
		if len(newest) != 0 {
			passage.OfferBaselineID = newest[0].ID
		}
		return true, r.stage(ctx, mission, data, ship, "passenger-offer", "/v1/passengers/offer", passengerOfferRequest{ShipID: ship.Ship.ID, Destination: data.Plan.Destination, Fare: passage.FarePence, MaxBerths: passage.MaxBerths})
	}
	elapsed := r.stateCache.Clock.Calendar.TotalGameHours - passage.StepHour
	if elapsed < 1 || upper.Before(passage.StepOpenedAt.Add(gameDuration(1.0/24, r.stateCache.Clock.Calendar.TimeScale))) {
		r.wakeAtServer(passage.StepOpenedAt.Add(gameDuration(1.0/24, r.stateCache.Clock.Calendar.TimeScale)), false)
		return false, r.hold(ctx, ship.Ship.ID, "passenger ladder retains each price for a full game hour")
	}
	if err := r.recordPassengerRefusals(ctx, data); err != nil {
		return false, err
	}
	if passage.FarePence <= passage.FloorPence {
		return false, r.hold(ctx, ship.Ship.ID, "passenger fare remains at its protected floor")
	}
	hoursRemaining := passage.BoardBy.Sub(upper).Hours() * float64(r.stateCache.Clock.Calendar.TimeScale)
	passage.FarePence = passengerNextFare(passage.FarePence, passage.FloorPence, hoursRemaining)
	passage.Reoffer = true
	return true, r.stage(ctx, mission, data, ship, "passenger-cancel", "/v1/passengers/cancel-offer", map[string]string{"offer_id": open.ID})
}

func (r *Runner) passageDepartureReady(ship ShipState, destination string, data *missionData) error {
	if data.Passage == nil || len(data.Passage.Groups) == 0 {
		return errors.New("new departure refused: passengers aboard have no matching persisted passage record")
	}
	if destination != data.Plan.Destination {
		return errors.New("passenger departure refuses the wrong destination")
	}
	if !r.fresh("passenger_aboard/" + ship.Ship.ID) {
		return errors.New("passenger departure requires fresh aboard promises")
	}
	current := r.passengerAboard(ship.Ship.ID)
	if len(current) == 0 {
		return errors.New("passenger departure requires authoritative aboard groups")
	}
	hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	if !exists {
		return errors.New("passenger departure hull is unknown")
	}
	if err := r.validateFreightDeparture(ship, destination, data, hull); err != nil {
		return err
	}
	input := r.planningInput(ship)
	route, err := observedPlanningRoute(input, ship.Ship.CurrentPortID, destination, hull)
	if err != nil {
		return err
	}
	if err := passengerRouteSafe(input, route, decode[[]letterOfMarque](r.snapshot, "marques")); err != nil {
		return err
	}
	// Promises gate offering and bounded loading waits, not recovery sailing.
	// Late passengers still pay a floored fare; delaying here only worsens it.
	var hold int64
	for _, group := range current {
		matched := false
		for _, persisted := range data.Passage.Groups {
			promiseMatches := data.Passage.RecoveryOnly || group.PromisedArrivalAt != nil && persisted.PromisedArrivalAt != nil && group.PromisedArrivalAt.Equal(*persisted.PromisedArrivalAt)
			if group.ID == persisted.ID && group.Size == persisted.Size && group.DestPortID == persisted.DestPortID && promiseMatches {
				matched = true
				break
			}
		}
		if !matched || group.DestPortID != destination {
			return errors.New("passenger departure has unmatched groups or a wrong destination")
		}
		hold += group.HoldTons
	}
	if hold != ship.PassengerHoldTons {
		return errors.New("passenger departure promises do not explain occupied hold")
	}
	return nil
}

func (r *Runner) passengerActionReady(ctx context.Context, mission *store.Mission, data *missionData) error {
	if data.Passage == nil || !r.fresh("passenger_offers") {
		return errors.New("passenger action requires its durable passage and current offers")
	}
	var request passengerOfferRequest
	if data.Action.Stage == "passenger-cancel" {
		var cancel struct {
			OfferID string `json:"offer_id"`
		}
		if json.Unmarshal(data.Action.Body, &cancel) != nil || cancel.OfferID == "" {
			return errors.New("passenger cancel lacks its immutable offer")
		}
		for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
			if offer.ID == cancel.OfferID && offer.ShipID == mission.ShipID {
				return nil
			}
		}
		return errors.New("passenger cancel offer does not belong to its ship")
	}
	if json.Unmarshal(data.Action.Body, &request) != nil || request.ShipID != mission.ShipID || request.Destination != data.Plan.Destination || !passengerFareAllowed(request.Fare, data.Passage.FloorPence) || request.Fare != data.Passage.FarePence || request.MaxBerths != data.Passage.MaxBerths {
		return errors.New("passenger offer disagrees with its protected fare floor and immutable intent")
	}
	ship, exists := r.ship(request.ShipID)
	if !exists || !r.fresh("ships") || !hazardStationary(ship) || ship.Ship.Status != "anchored_roadstead" || ship.Ship.DockQueuedAt != nil || ship.PassengerHoldTons > 0 || !emptyCargo(ship) {
		return errors.New("new passenger offer requires its idle unqueued roadstead ship")
	}
	for _, offer := range decode[[]gameapi.PassengerOffer](r.snapshot, "passenger_offers") {
		if offer.ShipID == ship.Ship.ID && offer.Status == "open" {
			return errors.New("passenger offer already exists; GET reconciliation precedes another POST")
		}
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return err
	}
	plans := PlanVoyages(input)
	if len(plans) > 0 && plans[0].Accepted {
		return errors.New("profitable cargo or charter plan takes priority over passenger offers")
	}
	quote, err := quotePassage(input, request.Destination, r.passengerGroups(ship.Ship.CurrentPortID), r.passengerFares(ship.Ship.CurrentPortID, request.Destination), decode[[]letterOfMarque](r.snapshot, "marques"))
	if err != nil {
		return err
	}
	if !r.fresh("passenger_waiting/"+ship.Ship.CurrentPortID) || request.Fare < quote.Floor || request.MaxBerths > quote.Berths {
		return errors.New("passenger offer no longer covers its current floor or whole fitting groups")
	}
	return r.insuranceActionReady(ship, "depart", request.Destination)
}

func (r *Runner) finishPassengerAction(ctx context.Context, mission *store.Mission, data *missionData, offer gameapi.PassengerOffer) (bool, error) {
	action := data.Action
	if action.Stage == "passenger-offer" {
		var request passengerOfferRequest
		if json.Unmarshal(action.Body, &request) != nil || offer.ID == "" || offer.ShipID != request.ShipID || offer.DestPortID != request.Destination || offer.FarePerHeadPence != request.Fare || offer.MaxBerths != request.MaxBerths {
			return false, r.hold(ctx, mission.ShipID, "passenger offer response does not match immutable body")
		}
		data.Passage.OfferID = offer.ID
		data.Passage.Reoffer = false
		data.Passage.StepHour = r.stateCache.Clock.Calendar.TotalGameHours
		_, upper, _ := r.serverBounds()
		data.Passage.StepOpenedAt = upper
		data.Passage.StepGroups = r.passengerGroups(data.Plan.Origin)
	} else {
		var request struct {
			OfferID string `json:"offer_id"`
		}
		if json.Unmarshal(action.Body, &request) != nil || offer.ID != request.OfferID || offer.ShipID != mission.ShipID || offer.Status == "open" {
			return false, r.hold(ctx, mission.ShipID, "passenger cancel needs its exact closed offer")
		}
		if data.Passage.OfferID == offer.ID {
			data.Passage.OfferID = ""
		}
	}
	data.LastActionID = action.ID
	data.Sequence++
	data.Action = nil
	return true, r.saveMission(ctx, mission, data)
}

func (r *Runner) consumePassengerResult(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if status >= 200 && status < 300 {
		var offer gameapi.PassengerOffer
		if json.Unmarshal(response, &offer) == nil && offer.ID != "" {
			var request passengerOfferRequest
			exactOffer := data.Action.Stage == "passenger-offer" && json.Unmarshal(data.Action.Body, &request) == nil &&
				offer.ShipID == request.ShipID && offer.DestPortID == request.Destination &&
				offer.FarePerHeadPence == request.Fare && offer.MaxBerths == request.MaxBerths
			var cancel struct {
				OfferID string `json:"offer_id"`
			}
			exactCancel := data.Action.Stage == "passenger-cancel" && json.Unmarshal(data.Action.Body, &cancel) == nil &&
				offer.ID == cancel.OfferID && offer.ShipID == mission.ShipID && offer.Status != "open"
			if exactOffer || exactCancel {
				return r.finishPassengerAction(ctx, mission, data, offer)
			}
		}
	}
	if status == http.StatusTooManyRequests {
		return r.consumeRejection(ctx, mission, data, status, response)
	}
	if status == http.StatusConflict || status >= 200 && status < 300 {
		action := &store.Action{ID: data.Action.ID, AccountID: r.account.ID, State: store.ActionComplete, Status: status, Response: response}
		return r.reconcilePassengerAction(ctx, mission, data, action)
	}
	if status >= 400 && status < 500 {
		return r.rejectPassengerAction(ctx, mission, data, status, response)
	}
	return false, r.hold(ctx, mission.ShipID, "passenger action retained until authoritative offers reconcile it")
}

func (r *Runner) passengerLaneGroupIDs(origin, destination string) []string {
	ids := make([]string, 0)
	for _, group := range r.passengerGroups(origin) {
		if group.DestPortID == destination {
			ids = append(ids, group.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func (r *Runner) rejectPassengerAction(ctx context.Context, mission *store.Mission, data *missionData, status int, response json.RawMessage) (bool, error) {
	if data.Action.Stage == "passenger-offer" {
		// A definitive refusal belongs to these observed waiting groups, not
		// an endlessly recreated mission for the same unchanged opportunity.
		refusals := decode[map[string][]string](r.snapshot, "_passenger_refusals")
		if refusals == nil {
			refusals = make(map[string][]string)
		}
		refusals[mission.ShipID+"|"+data.Plan.Origin+"|"+data.Plan.Destination] = r.passengerLaneGroupIDs(data.Plan.Origin, data.Plan.Destination)
		r.snapshot["_passenger_refusals"] = encode(refusals)
		if err := r.db.SaveSnapshot(ctx, r.account.ID, r.snapshot); err != nil {
			return false, err
		}
		data.Passage.Abandon = true
	}
	return r.consumeRejection(ctx, mission, data, status, response)
}

// Offers are newest first. A durable pre-send fence excludes every older offer
// without re-reading a hull's lifetime of closed price-ladder steps.
const passengerOfferMaxPages = 5
const passengerOfferMaxRows = 1_000
const passengerOfferMaxBytes = 1 << 20
const passengerOfferReadTimeout = 15 * time.Second

var errPassengerOfferReadBound = errors.New("passenger offer reconciliation exceeds its bounded observation budget")

func (r *Runner) passengerOfferHistory(ctx context.Context, ship, fence string, includeFence bool, openOnly bool) ([]gameapi.PassengerOffer, error) {
	ctx, cancel := context.WithTimeout(ctx, passengerOfferReadTimeout)
	defer cancel()
	base := "/v1/passengers/offers?open_only=" + fmt.Sprint(openOnly) + "&limit=200&ship_id=" + url.QueryEscape(ship)
	path := base
	offers := make([]gameapi.PassengerOffer, 0)
	seen := make(map[string]bool)
	bytesRead, rowsRead := 0, 0
	for pages := 0; pages < passengerOfferMaxPages; pages++ {
		var raw json.RawMessage
		if err := r.api.Get(ctx, path, &raw); err != nil {
			return nil, err
		}
		bytesRead += len(raw)
		if bytesRead > passengerOfferMaxBytes {
			return nil, errPassengerOfferReadBound
		}
		var page []gameapi.PassengerOffer
		if json.Unmarshal(raw, &page) != nil || page == nil || len(page) > 200 {
			return nil, errors.New("invalid passenger offers page")
		}
		rowsRead += len(page)
		if rowsRead > passengerOfferMaxRows {
			return nil, errPassengerOfferReadBound
		}
		for _, offer := range page {
			if offer.ID == "" || offer.ShipID != ship || seen[offer.ID] {
				return nil, errors.New("passenger offers page has invalid or repeated identity")
			}
			seen[offer.ID] = true
			if fence != "" && offer.ID == fence {
				if includeFence {
					offers = append(offers, offer)
				}
				return offers, nil
			}
			offers = append(offers, offer)
		}
		if len(page) < 200 {
			return offers, nil
		}
		path = base + "&cursor=" + url.QueryEscape(page[len(page)-1].ID)
	}
	return nil, errPassengerOfferReadBound
}

func (r *Runner) reconcilePassengerAction(ctx context.Context, mission *store.Mission, data *missionData, action *store.Action) (bool, error) {
	data.Passage.RetryOffer = false
	var offers []gameapi.PassengerOffer
	var err error
	if action.Status == http.StatusConflict && data.Action.Stage == "passenger-offer" {
		offers, err = r.passengerOfferHistory(ctx, mission.ShipID, "", false, true)
	} else {
		fence, includeFence := data.Passage.OfferBaselineID, false
		if data.Action.Stage == "passenger-cancel" {
			var request struct {
				OfferID string `json:"offer_id"`
			}
			if json.Unmarshal(data.Action.Body, &request) != nil || request.OfferID == "" {
				return false, errors.New("passenger cancellation lacks immutable identity")
			}
			fence, includeFence = request.OfferID, true
		}
		offers, err = r.passengerOfferHistory(ctx, mission.ShipID, fence, includeFence, false)
	}
	if err != nil {
		return false, r.hold(ctx, mission.ShipID, "passenger GET offers reconciliation failed; no second offer POST")
	}
	var matched *gameapi.PassengerOffer
	for i := range offers {
		offer := &offers[i]
		if offer.ShipID != mission.ShipID {
			continue
		}
		if data.Action.Stage == "passenger-cancel" {
			var request struct {
				OfferID string `json:"offer_id"`
			}
			_ = json.Unmarshal(data.Action.Body, &request)
			if offer.ID == request.OfferID {
				if offer.Status == "open" {
					// The same offer cannot reopen after cancellation. GET proves
					// its current state, so a new idempotent cancellation is safe
					// even if the old transport's result was lost.
					if action.State == store.ActionPending || action.State == store.ActionReconciled {
						if action.State == store.ActionPending {
							if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_passenger_offer_open", "offer": offer})); err != nil {
								return false, err
							}
						}
						data.LastActionID = data.Action.ID
						data.Action = nil
						data.Sequence++
						return true, r.saveMission(ctx, mission, data)
					}
					return r.consumeRejection(ctx, mission, data, action.Status, action.Response)
				}
				matched = offer
				break
			}
		} else {
			var request passengerOfferRequest
			_ = json.Unmarshal(data.Action.Body, &request)
			// Every returned non-conflict offer is newer than the durable fence.
			// Do not compare server and host clocks to prove request identity.
			exactNew := action.Status != http.StatusConflict && offer.FarePerHeadPence == request.Fare && offer.MaxBerths == request.MaxBerths
			existingConflict := action.Status == http.StatusConflict && offer.Status == "open"
			if offer.DestPortID == request.Destination && offer.PortID == data.Plan.Origin && (exactNew || existingConflict) {
				if matched != nil {
					return false, r.hold(ctx, mission.ShipID, "passenger offer reconciliation is ambiguous")
				}
				matched = offer
			}
		}
	}
	if matched == nil {
		if action.Status == http.StatusConflict && data.Action.Stage == "passenger-offer" {
			return r.rejectPassengerAction(ctx, mission, data, action.Status, action.Response)
		}
		if data.Action.Stage == "passenger-offer" && (action.State == store.ActionPending || action.State == store.ActionReconciled) && !action.CreatedAt.IsZero() {
			if time.Now().Before(action.CreatedAt.Add(store.ActionReplayWindow)) {
				// A lost transport may never have reached the server. Reuse only
				// this immutable action/key, bounded by the replay window.
				data.Passage.RetryOffer = true
			} else {
				if action.State == store.ActionPending {
					if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_passenger_offer_absent", "newest_before_attempt": data.Passage.OfferBaselineID})); err != nil {
						return false, err
					}
				}
				data.Passage.Abandon = true
				data.LastActionID = data.Action.ID
				data.Action = nil
				data.Sequence++
				return true, r.saveMission(ctx, mission, data)
			}
		}
		return false, r.hold(ctx, mission.ShipID, "passenger action has no matching GET offer; retain its immutable replay/reconciliation state")
	}
	if action.Status == http.StatusConflict && data.Action.Stage == "passenger-offer" {
		passage := data.Passage
		passage.OfferID = matched.ID
		passage.Reoffer = false
		passage.Abandon = !passengerFareAllowed(matched.FarePerHeadPence, passage.FloorPence) || matched.MaxBerths > passage.MaxBerths
		passage.FarePence, passage.MaxBerths = matched.FarePerHeadPence, matched.MaxBerths
		passage.StepHour = r.stateCache.Clock.Calendar.TotalGameHours
		_, passage.StepOpenedAt, _ = r.serverBounds()
		passage.StepGroups = r.passengerGroups(data.Plan.Origin)
		data.LastActionID = data.Action.ID
		data.Action = nil
		data.Sequence++
		return true, r.saveMission(ctx, mission, data)
	}
	if action.State == store.ActionPending {
		if err := r.db.ReconcileAction(ctx, action.ID, encode(map[string]any{"kind": "authoritative_passenger_offer", "offer": matched})); err != nil {
			return false, err
		}
	}
	return r.finishPassengerAction(ctx, mission, data, *matched)
}

func (r *Runner) recordPassengerRefusals(ctx context.Context, data *missionData) error {
	passage := data.Passage
	if !r.fresh("passenger_waiting/" + data.Plan.Origin) {
		return nil
	}
	// Public fares list accepted business, not rival offers. If any acceptance
	// occurred in this hour the observation cannot isolate a refusal.
	meta := r.meta["passenger_fares/"+data.Plan.Origin+"|"+data.Plan.Destination]
	if meta.Stale || !meta.Complete || meta.Error != "" || meta.UpdatedAt.Before(passage.StepOpenedAt) {
		return nil
	}
	for _, fare := range r.passengerFares(data.Plan.Origin, data.Plan.Destination) {
		if !fare.AcceptedAt.Before(passage.StepOpenedAt) {
			return nil
		}
	}
	current := r.passengerGroups(data.Plan.Origin)
	for _, before := range passage.StepGroups {
		if before.DestPortID != data.Plan.Destination || before.Size > passage.MaxBerths || before.Size <= 0 ||
			before.Kind != "travellers" && before.Kind != "emigrants" {
			continue
		}
		for _, after := range current {
			if before.ID == after.ID && before.Size == after.Size && after.BoardBy.After(passage.StepOpenedAt.Add(gameDuration(1.0/24, r.stateCache.Clock.Calendar.TimeScale))) {
				evidence := store.PassengerEvidence{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("passenger-refusal:%s:%s:%s:%d", r.account.ID, passage.OfferID, before.ID, passage.StepHour))).String(), AccountID: r.account.ID, Lane: data.Plan.Origin + "|" + data.Plan.Destination, Kind: before.Kind, FarePerHeadPence: passage.FarePence, Outcome: "refused", Heads: before.Size, GameHour: float64(passage.StepHour + 1)}
				if err := r.db.PutPassengerEvidence(ctx, &evidence); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (r *Runner) syncPassage(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (changed bool, err error) {
	passage := data.Passage
	if passage.QuarantineReason != "" {
		return false, nil
	}
	seen := make(map[string]bool, len(passage.Groups))
	for _, group := range passage.Groups {
		if group.ID == "" || seen[group.ID] || group.BoardedAt == nil {
			return r.quarantinePassage(ctx, mission, data, errors.New("passenger batch has missing boarding time or duplicate group identity"))
		}
		seen[group.ID] = true
	}
	events, err := r.db.PassengerEventsSince(ctx, r.account.ID, data.StartedAt)
	if err != nil {
		return false, err
	}
	hull, _ := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	// Boarding and arrival may both occur between REST captures. Recover the
	// promise from the retained event instead of silently losing its receipt.
	for _, event := range events {
		if event.EventType != "passengers_boarded" || event.CreatedAt.Before(data.StartedAt) {
			continue
		}
		var boarded passengerEventPayload
		if !decodePassengerEvent(event.Payload, &boarded) || boarded.ShipID != ship.Ship.ID || boarded.Heads <= 0 || boarded.PromisedArrivalAt == nil || hull.PassengersPerTon <= 0 {
			continue
		}
		if boarded.GroupID == "" {
			var matching *gameapi.PassengerWaitingGroup
			ambiguous := false
			for i := range passage.StepGroups {
				group := &passage.StepGroups[i]
				if group.Size != boarded.Heads || group.DestPortID != data.Plan.Destination ||
					boarded.Destination != "" && boarded.Destination != group.DestPortID ||
					boarded.Origin != "" && boarded.Origin != group.OriginPortID ||
					boarded.Kind != "" && boarded.Kind != group.Kind {
					continue
				}
				if matching != nil {
					ambiguous = true
					break
				}
				matching = group
			}
			if matching == nil || ambiguous {
				continue // Do not invent which same-sized whole group boarded.
			}
			boarded.GroupID, boarded.Kind = matching.ID, matching.Kind
			boarded.Origin, boarded.Destination = matching.OriginPortID, matching.DestPortID
		}
		for _, waiting := range passage.StepGroups {
			if waiting.ID == boarded.GroupID {
				if boarded.Kind == "" {
					boarded.Kind = waiting.Kind
				}
				if boarded.Origin == "" {
					boarded.Origin = waiting.OriginPortID
				}
				if boarded.Destination == "" {
					boarded.Destination = waiting.DestPortID
				}
				break
			}
		}
		if boarded.FullFarePence == 0 && boarded.FarePerHeadPence > 0 {
			boarded.FullFarePence = checkedPenceProduct(boarded.FarePerHeadPence, boarded.Heads)
		}
		known := false
		for _, group := range passage.Groups {
			if group.ID == boarded.GroupID {
				known = true
				break
			}
		}
		if known {
			continue
		}
		at := boarded.BoardedAt
		if at == nil && !boarded.Timestamp.IsZero() {
			at = new(boarded.Timestamp)
		}
		if at == nil || at.Before(data.StartedAt) {
			continue
		}
		if boarded.Destination != data.Plan.Destination {
			return false, r.hold(ctx, ship.Ship.ID, "boarded passenger event disagrees with passage destination")
		}
		passage.Groups = append(passage.Groups, gameapi.PassengerAboardGroup{ID: boarded.GroupID, Kind: boarded.Kind, Size: boarded.Heads, OriginPortID: boarded.Origin, DestPortID: boarded.Destination, FarePerHeadPence: boarded.FarePerHeadPence, FullFarePence: boarded.FullFarePence, HoldTons: ceilRatio(boarded.Heads, 1, hull.PassengersPerTon), BoardedAt: at, PromisedArrivalAt: boarded.PromisedArrivalAt})
		changed = true
	}
	if r.fresh("passenger_aboard/" + ship.Ship.ID) {
		for _, group := range r.passengerAboard(ship.Ship.ID) {
			if group.ID == "" || group.Size <= 0 || group.BoardedAt == nil || group.PromisedArrivalAt == nil || group.DestPortID != data.Plan.Destination {
				return false, r.hold(ctx, ship.Ship.ID, "passenger boarding lacks a matching complete promise")
			}
			known := false
			for i, previous := range passage.Groups {
				if group.ID != previous.ID {
					continue
				}
				known = true
				// REST is canonical when an earlier SSE projection omitted fields
				// or represented the promise with less precision.
				if previous.Size != group.Size || previous.Kind != group.Kind || previous.FarePerHeadPence != group.FarePerHeadPence ||
					previous.FullFarePence != group.FullFarePence || previous.HoldTons != group.HoldTons ||
					previous.BoardedAt == nil || !previous.BoardedAt.Equal(*group.BoardedAt) ||
					previous.PromisedArrivalAt == nil || !previous.PromisedArrivalAt.Equal(*group.PromisedArrivalAt) {
					passage.Groups[i] = group
					changed = true
				}
				break
			}
			if !known {
				passage.Groups = append(passage.Groups, group)
				changed = true
			}
		}
	}
	if len(passage.Groups) > 0 {
		hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
		route := r.stateCache.Routes[routeKey(data.Plan.Origin, data.Plan.Destination, ship.Ship.ShipClass)]
		hours, valid := passageRouteHours(route, hull)
		if exists && valid {
			passage.StressHours = hours
			for _, group := range passage.Groups {
				if group.BoardedAt == nil || group.PromisedArrivalAt == nil {
					continue
				}
				if passage.FirstBoardedAt.IsZero() || group.BoardedAt.Before(passage.FirstBoardedAt) {
					passage.FirstBoardedAt = *group.BoardedAt
					leeway := group.PromisedArrivalAt.Sub(*group.BoardedAt).Hours()*float64(r.stateCache.Clock.Calendar.TimeScale) - hours
					passage.WaitUntil = group.BoardedAt.Add(gameDuration(max(0, min(leeway/2, 2))/24, r.stateCache.Clock.Calendar.TimeScale))
					changed = true
				}
				leeway := group.PromisedArrivalAt.Sub(*group.BoardedAt).Hours()*float64(r.stateCache.Clock.Calendar.TimeScale) - hours
				waitUntil := passage.FirstBoardedAt.Add(gameDuration(max(0, min(leeway/2, 2))/24, r.stateCache.Clock.Calendar.TimeScale))
				if passage.WaitUntil.IsZero() || waitUntil.Before(passage.WaitUntil) {
					passage.WaitUntil = waitUntil
					changed = true
				}
				if _, measured := passage.Measurements[group.ID]; !measured {
					if (group.Kind != "travellers" && group.Kind != "emigrants") || group.FarePerHeadPence <= 0 {
						continue // Incomplete event projections cannot become measured commercial evidence.
					}
					if passage.Measurements == nil {
						passage.Measurements = make(map[string]store.PassengerEvidence)
					}
					hour := float64(data.StartedGameHours) + max(0, group.BoardedAt.Sub(data.StartedAt).Hours()*float64(r.stateCache.Clock.Calendar.TimeScale))
					passage.Measurements[group.ID] = store.PassengerEvidence{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("passenger-boarded:"+r.account.ID+":"+group.ID)).String(), AccountID: r.account.ID, Lane: data.Plan.Origin + "|" + data.Plan.Destination, Kind: group.Kind, FarePerHeadPence: group.FarePerHeadPence, Outcome: "boarded", Heads: group.Size, GameHour: hour, LeewayGameHours: new(leeway)}
					changed = true
				}
			}
		}
	}
	if changed {
		return true, r.saveMission(ctx, mission, data)
	}
	for _, evidence := range passage.Measurements {
		if err := r.db.PutPassengerEvidence(ctx, &evidence); err != nil {
			return false, err
		}
	}
	if len(passage.Groups) == 0 || passage.ReceiptsPrepared {
		return false, nil
	}
	type receiptEvent struct {
		event   store.Event
		payload passengerEventPayload
		group   gameapi.PassengerAboardGroup
	}
	terminal := make(map[string]receiptEvent)
	for _, event := range events {
		if event.EventType != "passengers_paid" && event.EventType != "passengers_lost" {
			continue
		}
		var payload passengerEventPayload
		if !decodePassengerEvent(event.Payload, &payload) || payload.ShipID != ship.Ship.ID {
			continue
		}
		at, valid := passengerTerminalAt(payload, event)
		if !valid {
			continue
		}
		matches := make([]gameapi.PassengerAboardGroup, 0, 1)
		for _, group := range passage.Groups {
			if payload.GroupID != "" && payload.GroupID != group.ID {
				continue
			}
			if payload.Destination != "" && payload.Destination != group.DestPortID {
				continue
			}
			if payload.Heads > 0 && payload.Heads != group.Size {
				continue
			}
			if group.BoardedAt == nil || at.Before(*group.BoardedAt) || payload.GroupID == "" && !at.After(*group.BoardedAt) {
				continue
			}
			matches = append(matches, group)
		}
		if len(matches) != 1 {
			continue
		}
		group := matches[0]
		if previous, exists := terminal[group.ID]; exists && previous.event.EventID != event.EventID {
			return r.quarantinePassage(ctx, mission, data, errors.New("conflicting terminal passenger events require reconciliation"))
		}
		if event.EventType == "passengers_paid" && (payload.FarePaidPence == nil || *payload.FarePaidPence < 0) {
			continue
		}
		terminal[group.ID] = receiptEvent{event: event, payload: payload, group: group}
	}
	if len(terminal) != len(passage.Groups) {
		return r.preparePassengerBatchReceipts(ctx, ship, mission, data, events)
	}
	bills, expenseIDs := r.claimBillsSince(data, mission.Generation)
	data.ExpensesPence = growthCashTotal(data.ExpensesPence, bills)
	data.SettlementExpenseIDs = expenseIDs
	totalHeads := int64(0)
	for _, group := range passage.Groups {
		totalHeads += group.Size
	}
	remainingFees, remainingProvisions, remainingExpenses := data.FeesPence, data.ProvisionsPence, data.ExpensesPence
	for i, group := range passage.Groups {
		receipt := terminal[group.ID]
		fees, provisions, expenses := remainingFees, remainingProvisions, remainingExpenses
		if i < len(passage.Groups)-1 {
			fees = ceilRatio(data.FeesPence, group.Size, totalHeads)
			provisions = ceilRatio(data.ProvisionsPence, group.Size, totalHeads)
			expenses = ceilRatio(data.ExpensesPence, group.Size, totalHeads)
			fees = min(fees, remainingFees)
			provisions = min(provisions, remainingProvisions)
			expenses = min(expenses, remainingExpenses)
		}
		remainingFees -= fees
		remainingProvisions -= provisions
		remainingExpenses -= expenses
		completed := receipt.payload.Timestamp
		if completed.IsZero() {
			completed = receipt.event.CreatedAt
		}
		started := *group.BoardedAt
		seconds := max(int64(0), int64(completed.Sub(started).Seconds()*float64(r.stateCache.Clock.Calendar.TimeScale)))
		proceeds := int64(0)
		if receipt.event.EventType == "passengers_paid" {
			proceeds = *receipt.payload.FarePaidPence
		}
		settlement := &store.Settlement{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("passage-settlement:"+r.account.ID+":"+receipt.event.EventID)).String(), AccountID: r.account.ID, ShipID: ship.Ship.ID, MissionID: mission.Generation, EventID: receipt.event.EventID, Kind: "passage", Lane: data.Plan.Origin + "|" + data.Plan.Destination, Origin: data.Plan.Origin, Destination: data.Plan.Destination, QuantityTons: group.HoldTons, GameSeconds: seconds, CostsKnown: data.CostsKnown, ProceedsPence: proceeds, FeesPence: fees, ProvisionsPence: provisions, ExpensesPence: expenses, StartedAt: started, CompletedAt: completed}
		passage.Receipts = append(passage.Receipts, settlement)
		if receipt.payload.GameDaysLate != nil && *receipt.payload.GameDaysLate > 0 && group.FullFarePence >= proceeds &&
			(group.Kind == "travellers" || group.Kind == "emigrants") && group.FarePerHeadPence > 0 {
			cut := int64(math.Ceil(float64(group.FullFarePence-proceeds) / *receipt.payload.GameDaysLate))
			evidence := store.PassengerEvidence{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte("passenger-latecut:"+r.account.ID+":"+receipt.event.EventID)).String(), AccountID: r.account.ID, Lane: data.Plan.Origin + "|" + data.Plan.Destination, Kind: group.Kind, FarePerHeadPence: group.FarePerHeadPence, Outcome: "boarded", Heads: group.Size, GameHour: float64(data.StartedGameHours), LateCutPerGameDayPence: new(cut)}
			if err := r.db.PutPassengerEvidence(ctx, &evidence); err != nil {
				return false, err
			}
		}
	}
	passage.ReceiptsPrepared = true
	return true, r.saveMission(ctx, mission, data)
}

// Some SSE payloads identify only the ship and paid fare, not the individual
// group. Never manufacture that attribution. Once REST proves the hold empty,
// account for the whole completed batch: each event keeps its exact proceeds,
// while costs and hold receive explicit equal batch allocations. Duration uses
// the measured first boarding, conservatively including the whole loading wait.
func (r *Runner) preparePassengerBatchReceipts(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData, events []store.Event) (bool, error) {
	passage := data.Passage
	var aboard []gameapi.PassengerAboardGroup
	aboardRaw := decode[map[string]json.RawMessage](r.snapshot, "passenger_aboard")
	aboardOK := json.Unmarshal(aboardRaw[ship.Ship.ID], &aboard) == nil && aboard != nil
	if ship.Ship.Status != "sunk" && ship.PassengerHoldTons == 0 && r.fresh("passenger_aboard/"+ship.Ship.ID) && !aboardOK {
		return r.quarantinePassage(ctx, mission, data, errors.New("passenger batch completion has invalid authoritative aboard capture"))
	}
	empty := ship.PassengerHoldTons == 0 && r.fresh("passenger_aboard/"+ship.Ship.ID) && aboardOK && len(aboard) == 0
	if !empty && ship.Ship.Status != "sunk" {
		return false, nil
	}
	started := time.Time{}
	var hold int64
	groupIDs := make(map[string]bool, len(passage.Groups))
	for _, group := range passage.Groups {
		if group.BoardedAt == nil {
			return false, nil
		}
		if started.IsZero() || group.BoardedAt.Before(started) {
			started = *group.BoardedAt
		}
		hold += group.HoldTons
		groupIDs[group.ID] = true
	}
	if started.IsZero() {
		return false, nil
	}
	type batchReceipt struct {
		event     store.Event
		payload   passengerEventPayload
		completed time.Time
	}
	var receipts []batchReceipt
	allLost, wholeShipLoss := true, ship.Ship.Status == "sunk"
	for _, event := range events {
		if event.EventType != "passengers_paid" && event.EventType != "passengers_lost" {
			continue
		}
		var payload passengerEventPayload
		if !decodePassengerEvent(event.Payload, &payload) || payload.ShipID != ship.Ship.ID ||
			payload.GroupID != "" && !groupIDs[payload.GroupID] ||
			payload.Destination != "" && payload.Destination != data.Plan.Destination {
			continue
		}
		completed, valid := passengerTerminalAt(payload, event)
		if !valid || completed.Before(started) || payload.GroupID == "" && !completed.After(started) {
			continue
		}
		if event.EventType == "passengers_paid" {
			if payload.FarePaidPence == nil || *payload.FarePaidPence < 0 {
				continue
			}
			allLost = false
		}
		if event.EventType == "passengers_lost" && (payload.Cause == "sunk" || payload.Cause == "captured" || payload.Cause == "seized" || payload.Cause == "ship_changed_hands") {
			wholeShipLoss = true
		}
		receipts = append(receipts, batchReceipt{event, payload, completed})
	}
	// Payment is per group. A whole-ship loss may instead report one batch.
	// An empty REST hold alone must not turn a partially delivered SSE stream
	// into an incomplete paid settlement.
	if len(receipts) > len(passage.Groups) {
		return r.quarantinePassage(ctx, mission, data, errors.New("passenger batch has more terminal events than boarded groups"))
	}
	if len(receipts) == 0 || !(allLost && wholeShipLoss) && len(receipts) < len(passage.Groups) {
		return false, nil
	}
	sort.SliceStable(receipts, func(i, j int) bool { return receipts[i].completed.Before(receipts[j].completed) })
	bills, expenseIDs := r.claimBillsSince(data, mission.Generation)
	data.ExpensesPence = growthCashTotal(data.ExpensesPence, bills)
	data.SettlementExpenseIDs = expenseIDs
	remainingFees, remainingProvisions, remainingExpenses, remainingHold := data.FeesPence, data.ProvisionsPence, data.ExpensesPence, hold
	for i, receipt := range receipts {
		proceeds := int64(0)
		if receipt.event.EventType == "passengers_paid" {
			proceeds = *receipt.payload.FarePaidPence
		}
		settlement := &store.Settlement{
			ID:        uuid.NewSHA1(uuid.NameSpaceURL, []byte("passage-settlement:"+r.account.ID+":"+receipt.event.EventID)).String(),
			AccountID: r.account.ID, ShipID: ship.Ship.ID, MissionID: mission.Generation, EventID: receipt.event.EventID,
			Kind: "passage", Lane: data.Plan.Origin + "|" + data.Plan.Destination, Origin: data.Plan.Origin, Destination: data.Plan.Destination,
			CostsKnown: data.CostsKnown, ProceedsPence: proceeds, StartedAt: started, CompletedAt: receipt.completed,
			RevenueBasis: new("passenger_batch_allocation"),
			GameSeconds:  max(int64(0), int64(receipt.completed.Sub(started).Seconds()*float64(r.stateCache.Clock.Calendar.TimeScale))),
		}
		count := int64(len(receipts) - i)
		settlement.QuantityTons = remainingHold / count
		settlement.FeesPence = remainingFees / count
		settlement.ProvisionsPence = remainingProvisions / count
		settlement.ExpensesPence = remainingExpenses / count
		remainingHold -= settlement.QuantityTons
		remainingFees -= settlement.FeesPence
		remainingProvisions -= settlement.ProvisionsPence
		remainingExpenses -= settlement.ExpensesPence
		passage.Receipts = append(passage.Receipts, settlement)
	}
	passage.ReceiptsPrepared = true
	return true, r.saveMission(ctx, mission, data)
}

// A captured, seized or transferred ship can disappear from the owned fleet.
// Its durable passage still owns its costs and retained loss event.
func (r *Runner) reconcileMissingPassages(ctx context.Context) (bool, error) {
	if !r.fresh("ships") {
		return false, nil
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	for i := range missions {
		mission := &missions[i]
		if _, present := r.ship(mission.ShipID); present {
			continue
		}
		var data missionData
		if err := json.Unmarshal(mission.Data, &data); err != nil {
			_ = r.decision(ctx, mission.ShipID, "passage_quarantined", err.Error(), nil)
			continue
		}
		if data.Passage == nil || data.Action != nil || data.Passage.QuarantineReason != "" {
			continue
		}
		var absent ShipState
		absent.Ship.ID, absent.Ship.Status = mission.ShipID, "sunk"
		if data.Passage.ReceiptsPrepared {
			if changed, err := r.drivePassage(ctx, absent, mission, &data); changed || err != nil {
				return changed, err
			}
		} else if changed, err := r.syncPassage(ctx, absent, mission, &data); err != nil {
			if passengerIntegrityFailure(err) {
				_, _ = r.quarantinePassage(ctx, mission, &data, err)
			} else {
				_ = r.hold(ctx, mission.ShipID, "missing passenger passage reconciliation deferred: "+err.Error())
			}
		} else if changed {
			return true, nil
		}
	}
	return false, nil
}
