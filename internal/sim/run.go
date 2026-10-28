package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type RunOptions struct {
	From         time.Time
	To           time.Time
	FleetAt      time.Time
	Regen        float64
	ReservePence int64
	CompanyID    string
	SpeedFactors map[string]SpeedFactor
	Variants     []Variant
}

type RegionalBalance struct {
	Region       string `json:"region"`
	BalancePence int64  `json:"balance_pence"`
}

type VariantResult struct {
	Variant                string             `json:"variant"`
	Regen                  float64            `json:"regen"`
	InitialCashPence       int64              `json:"initial_cash_pence"`
	FinalCashPence         int64              `json:"final_cash_pence"`
	InitialNetWorthPence   int64              `json:"initial_net_worth_pence"`
	FinalNetWorthPence     int64              `json:"final_net_worth_pence"`
	DeltaVsBaselinePence   int64              `json:"delta_vs_baseline_pence"`
	RealizedNetPence       int64              `json:"realized_net_pence"`
	IdleGameHours          float64            `json:"idle_game_hours"`
	TotalShipGameHours     float64            `json:"total_ship_game_hours"`
	IdlePercent            float64            `json:"idle_percent"`
	Probes                 int                `json:"probes"`
	Conversions            int                `json:"conversions"`
	FailedProbes           int                `json:"failed_probes"`
	ProbeExpensePence      int64              `json:"probe_expense_pence"`
	ObservedTons           int64              `json:"observed_tons"`
	CarriedTons            int64              `json:"carried_tons"`
	DarkDecisions          int64              `json:"dark_decisions"`
	SettlementCount        int64              `json:"settlement_count"`
	LowCoverage            bool               `json:"low_coverage"`
	HazardAvailable        bool               `json:"hazard_available"`
	HazardUnavailablePlans int                `json:"hazard_unavailable_plans"`
	Settlements            []store.Settlement `json:"settlements"`
	Ships                  []engine.ShipState `json:"ships"`
	Vaults                 []RegionalBalance  `json:"vaults"`
	Reconciliation         CashReconciliation `json:"reconciliation"`
	Omissions              []string           `json:"omissions"`
}

type simulatedMission struct {
	id              string
	plan            engine.Plan
	stage           string
	nextAt          time.Time
	startedHour     float64
	coverage        Coverage
	initialStores   float64
	purchasedStores float64
	storeUnitPence  float64
	exploration     int64
	conversion      bool
}

type variantWorld struct {
	variant             Variant
	options             RunOptions
	tape                *Tape
	market              *MarketModel
	fleet               *Fleet
	accounting          *Accounting
	ledger              *engine.SourcingLedger
	seedSettlements     []store.Settlement
	seedSettlementIDs   map[string]bool
	sourcingScale       int64
	missions            map[string]*simulatedMission
	initialProbeHistory json.RawMessage
	idleHours           map[string]float64
	recordedDeliveries  []ContractDelivery
	deliveries          []ContractDelivery
	sellFills           []engine.PlayerBidSellFill
	sequence            int64
	hour                float64
	result              VariantResult
}

// Run advances each world concurrently, then waits at every recorded frame and
// exact simulated event. Only the immutable FrameView is shared across worlds.
func Run(ctx context.Context, tape *Tape, options RunOptions) ([]VariantResult, error) {
	if tape == nil || !options.To.After(options.From) || math.IsNaN(options.Regen) || math.IsInf(options.Regen, 0) || options.Regen < 0 || options.Regen > 1 {
		return nil, errors.New("invalid simulation tape, window or regeneration")
	}
	if options.ReservePence <= 0 {
		options.ReservePence = 75_000
	}
	if len(options.Variants) == 0 {
		options.Variants = DefaultVariants()
	}
	seen := make(map[string]bool)
	for _, variant := range options.Variants {
		if variant.Name == "" || seen[variant.Name] || variant.SourcingCashBPS != nil && *variant.SourcingCashBPS < 0 {
			return nil, errors.New("simulation variants require unique names and nonnegative cash shares")
		}
		seen[variant.Name] = true
	}
	reader := tape.Reader()
	initial, err := reader.Advance(options.From)
	if err != nil {
		return nil, err
	}
	if len(initial.State.Ships) == 0 || len(initial.State.Catalog.Ports) == 0 || initial.State.Clock.Calendar.TimeScale <= 0 {
		return nil, errors.New("simulation start lacks a recorded fleet, catalog or clock")
	}
	if !options.FleetAt.IsZero() {
		if options.FleetAt.After(options.From) {
			return nil, errors.New("fixed fleet selection must not be after simulation start")
		}
		fleetView, err := tape.Reader().Advance(options.FleetAt)
		if err != nil {
			return nil, err
		}
		ids := make(map[string]bool)
		for _, ship := range fleetView.State.Ships {
			if ship.Ship.Status != "sunk" {
				ids[ship.Ship.ID] = true
			}
		}
		ships := make([]engine.ShipState, 0, len(ids))
		for _, ship := range initial.State.Ships {
			if ids[ship.Ship.ID] {
				ships = append(ships, ship)
			}
		}
		if len(ships) != len(ids) || len(ids) == 0 {
			return nil, errors.New("holdout state cannot reconstruct the fixed starting fleet")
		}
		initial.State.Ships = ships
	}
	if options.CompanyID == "" {
		var profile struct {
			Company struct {
				ID string `json:"id"`
			} `json:"company"`
		}
		_ = json.Unmarshal(initial.Snapshot["profile"], &profile)
		options.CompanyID = profile.Company.ID
	}
	worlds := make([]*variantWorld, len(options.Variants))
	for i, variant := range options.Variants {
		worlds[i], err = newVariantWorld(tape, options, variant, initial)
		if err != nil {
			return nil, err
		}
	}
	at := options.From
	view := initial
	for {
		view, err = postCutoverFills(view, options.From)
		if err != nil {
			return nil, err
		}
		if at.Before(options.To) && !recordedHazardAvailable(view) {
			for _, world := range worlds {
				if world.result.HazardAvailable {
					world.result.HazardAvailable = false
					world.result.Omissions = append(world.result.Omissions, "UNTRUSTED: no recorded Lloyd's quotes")
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var barrier sync.WaitGroup
		errorsByWorld := make([]error, len(worlds))
		barrier.Add(len(worlds))
		for i, world := range worlds {
			go func(index int, world *variantWorld) {
				defer barrier.Done()
				errorsByWorld[index] = world.advance(view, at.Before(options.To))
			}(i, world)
		}
		barrier.Wait()
		for i, err := range errorsByWorld {
			if err != nil {
				return nil, fmt.Errorf("variant %s at %s: %w", worlds[i].variant.Name, at.Format(time.RFC3339Nano), err)
			}
		}
		if !at.Before(options.To) {
			break
		}
		next := options.To
		observedAt, err := reader.NextTime()
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if err == nil && observedAt.Before(next) {
			next = observedAt
		}
		for _, world := range worlds {
			if event := world.nextEvent(view); event.After(at) && event.Before(next) {
				next = event
			}
		}
		at = next
		view, err = reader.Advance(at)
		if err != nil {
			return nil, err
		}
	}
	results := make([]VariantResult, len(worlds))
	var baseline int64
	for i, world := range worlds {
		results[i], err = world.finish(view)
		if err != nil {
			return nil, fmt.Errorf("variant %s: %w", world.variant.Name, err)
		}
		if results[i].Variant == "baseline" {
			baseline = results[i].FinalNetWorthPence
		}
	}
	if seen["baseline"] {
		for i := range results {
			results[i].DeltaVsBaselinePence = results[i].FinalNetWorthPence - baseline
		}
	}
	return results, nil
}

func postCutoverFills(view FrameView, from time.Time) (FrameView, error) {
	var fills []RecordedFill
	if raw := view.Snapshot["fills"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &fills); err != nil {
			return FrameView{}, fmt.Errorf("decode recorded market fills: %w", err)
		}
	}
	current := fills[:0]
	for _, fill := range fills {
		if fill.CreatedAt.After(from) && !fill.CreatedAt.After(view.At) {
			current = append(current, fill)
		}
	}
	view.Snapshot = cloneMap(view.Snapshot)
	var err error
	view.Snapshot["fills"], err = json.Marshal(current)
	return view, err
}

func newVariantWorld(tape *Tape, options RunOptions, variant Variant, view FrameView) (*variantWorld, error) {
	seed := make([]store.Settlement, 0)
	for _, row := range tape.Settlements {
		if !row.CompletedAt.After(options.From) {
			seed = append(seed, row)
		}
	}
	prefix, err := tape.sourcingSettlementsBefore(view.At, view.State.Clock.Calendar.TimeScale)
	if err != nil {
		return nil, fmt.Errorf("load rolling sourcing prefix: %w", err)
	}
	seed = append(seed, prefix...)
	world := &variantWorld{
		variant: variant, options: options, tape: tape, market: NewMarketModel(options.Regen),
		fleet:      NewFleet(view.State, view.At, view.GameHour, options.SpeedFactors),
		accounting: NewAccounting(view.State.Vaults), ledger: engine.NewSourcingLedger(seed, nil),
		seedSettlements: seed, sourcingScale: view.State.Clock.Calendar.TimeScale,
		missions: make(map[string]*simulatedMission), idleHours: make(map[string]float64),
		initialProbeHistory: view.Snapshot["_sourcing_probe_history"],
		recordedDeliveries:  charterDeliveryReceipts(tape.Actions, options.From),
		hour:                view.GameHour,
		result: VariantResult{Variant: variant.Name, Regen: options.Regen, HazardAvailable: true, Omissions: []string{
			"Simulated estimates, not observed profit; recorded Lloyd's quotes price plans, but random sinkings and cargo loss are not simulated.",
			"No berth queues, competitor or NPC price response, or partial-fill probability model; these omissions are optimistic.",
			"Fixed starting fleet; no freight, hull purchases, growth, refits, loans, underwriting, marques, warehouses, factor hiring or convoys.",
			"Net worth values cargo at catalog base prices; identical hulls and stores are excluded.",
		}},
	}
	balances := world.accounting.Balances()
	for _, region := range sortedKeys(balances) {
		world.result.InitialCashPence += balances[region]
	}
	world.result.InitialNetWorthPence = NetWorth(balances, world.fleet.Ships(), view.State.Catalog)
	var priorIdle map[string]idleObservation
	if raw := view.Snapshot["_ship_idle"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &priorIdle); err != nil {
			return nil, fmt.Errorf("decode starting idle state: %w", err)
		}
	}
	for _, ship := range world.fleet.Ships() {
		observed := priorIdle[ship.Ship.ID]
		if ship.Voyage == nil && cargoTons(ship) == 0 && observed.Validated &&
			observed.PortID == ship.Ship.CurrentPortID && !observed.LastClockUTC.After(view.At) {
			world.idleHours[ship.Ship.ID] = float64(max(0, observed.EarnedGameHours))
		}
	}
	var fills []RecordedFill
	if raw := view.Snapshot["fills"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &fills); err != nil {
			return nil, fmt.Errorf("decode starting fill history: %w", err)
		}
	}
	for _, fill := range fills {
		if !fill.CreatedAt.After(options.From) && (fill.Side == "ask" || fill.Side == "sell") {
			world.sellFills = append(world.sellFills, engine.PlayerBidSellFill{
				PortID: fill.PortID, CommodityID: fill.CommodityID, Side: "ask", Quantity: fill.Quantity, CreatedAt: fill.CreatedAt,
			})
		}
	}
	for _, ship := range world.fleet.Ships() {
		if ship.CargoUsedTons == 0 && len(ship.CargoManifest) == 0 {
			continue
		}
		world.sequence++
		id := fmt.Sprintf("sim-%s-%06d", variant.Name, world.sequence)
		origin := ship.Ship.CurrentPortID
		if len(ship.RouteLegs) > 0 {
			origin = ship.RouteLegs[0].FromNode
		}
		plan := engine.Plan{Kind: "recovery", ShipID: ship.Ship.ID, Origin: origin, Destination: ship.Ship.CurrentPortID}
		mission := &simulatedMission{id: id, plan: plan, startedHour: view.GameHour, stage: "deliver"}
		if ship.Voyage != nil {
			mission.stage, mission.nextAt = "sailing", ship.Voyage.EstimatedArrivalAt
		} else {
			port := catalogPort(view.State.Catalog, ship.Ship.CurrentPortID)
			if err := world.fleet.Handle(ship.Ship.ID, cargoTons(ship), port.IsHub, view.At, view.State.Clock.Calendar.TimeScale); err != nil {
				return nil, err
			}
			mission.nextAt = *world.fleet.Ship(ship.Ship.ID).Ship.TurnaroundUnlocksAt
		}
		world.missions[ship.Ship.ID] = mission
		world.accounting.Start(store.Settlement{ID: id, MissionID: id, ActionID: id, ShipID: ship.Ship.ID,
			Kind: "recovery", Origin: origin, StartedAt: view.At, CostsKnown: false})
	}
	return world, nil
}

func (world *variantWorld) nextEvent(view FrameView) time.Time {
	next := world.options.To
	for _, ship := range world.fleet.Ships() {
		if ship.Voyage != nil && ship.Voyage.EstimatedArrivalAt.After(view.At) && ship.Voyage.EstimatedArrivalAt.Before(next) {
			next = ship.Voyage.EstimatedArrivalAt
		}
		if mission := world.missions[ship.Ship.ID]; mission != nil {
			if mission.nextAt.After(view.At) && mission.nextAt.Before(next) {
				next = mission.nextAt
			}
		} else if ship.Voyage == nil && ship.CargoUsedTons == 0 && world.idleHours[ship.Ship.ID] < 6 && !view.State.Clock.Paused {
			remaining := 6 - world.idleHours[ship.Ship.ID]
			idleAt := view.At.Add(time.Duration(remaining / float64(view.State.Clock.Calendar.TimeScale) * float64(time.Hour)))
			if idleAt.After(view.At) && idleAt.Before(next) {
				next = idleAt
			}
		}
	}
	return next
}

func (world *variantWorld) advance(view FrameView, mayStart bool) error {
	world.fleet.UpdateContext(view.State.Catalog, view.State.Ports)
	hours := max(0, view.GameHour-world.hour)
	for _, ship := range world.fleet.Ships() {
		world.result.TotalShipGameHours += hours
		if world.missions[ship.Ship.ID] == nil && ship.Voyage == nil && ship.CargoUsedTons == 0 && ship.Ship.CurrentPortID != "" {
			world.idleHours[ship.Ship.ID] += hours
			world.result.IdleGameHours += hours
		} else {
			world.idleHours[ship.Ship.ID] = 0
		}
	}
	arrivals := world.fleet.Advance(view.At, view.GameHour)
	world.hour = view.GameHour
	if err := world.chargeFleetBills(); err != nil {
		return err
	}
	for _, arrival := range arrivals {
		if mission := world.missions[arrival.ShipID]; mission != nil {
			mission.plan.Destination = arrival.PortID
			ship := world.fleet.Ship(arrival.ShipID)
			if mission.plan.Kind == "relocation" {
				mission.stage = "inspect"
			} else {
				mission.stage = "deliver"
				port := catalogPort(view.State.Catalog, arrival.PortID)
				if err := world.fleet.Handle(arrival.ShipID, cargoTons(*ship), port.IsHub, arrival.At, view.State.Clock.Calendar.TimeScale); err != nil {
					return err
				}
			}
			mission.nextAt = *ship.Ship.TurnaroundUnlocksAt
		}
	}
	world.market.Observe(view)
	for _, ship := range world.fleet.Ships() {
		mission := world.missions[ship.Ship.ID]
		if mission != nil {
			if mission.nextAt.After(view.At) || ship.Voyage != nil {
				continue
			}
			if err := world.advanceMission(view, mission, mayStart); err != nil {
				return err
			}
			if world.missions[ship.Ship.ID] != nil {
				continue
			}
			ship = *world.fleet.Ship(ship.Ship.ID)
		}
		if !mayStart || ship.Voyage != nil || ship.Ship.CurrentPortID == "" || cargoTons(ship) != 0 || ship.Ship.ConvoyID != "" {
			continue
		}
		if ship.Ship.TurnaroundUnlocksAt != nil && ship.Ship.TurnaroundUnlocksAt.After(view.At) {
			continue
		}
		input, err := world.planningInput(view, ship)
		if err != nil {
			return err
		}
		if plan, accepted := world.bestOrdinary(input); accepted {
			if err := world.startMission(view, input, plan, false); err != nil {
				return err
			}
			continue
		}
		if !world.probeActive() {
			if plan, accepted := world.bestProbe(input); accepted {
				if err := world.startMission(view, input, plan, false); err != nil {
					return err
				}
				continue
			}
		}
		if _, coverage := world.market.Book(ship.Ship.CurrentPortID); coverage == Dark {
			world.result.DarkDecisions++
		}
	}
	return nil
}

func (world *variantWorld) probeActive() bool {
	for _, mission := range world.missions {
		if mission.plan.Kind == "relocation" {
			return true
		}
	}
	return false
}

func (world *variantWorld) startMission(view FrameView, input engine.PlanningInput, plan engine.Plan, conversion bool) error {
	plan, transfers := engine.PlanFunding(input, plan)
	if !plan.Accepted {
		return nil
	}
	world.sequence++
	id := fmt.Sprintf("sim-%s-%06d", world.variant.Name, world.sequence)
	ship := world.fleet.Ship(plan.ShipID)
	mission := &simulatedMission{id: id, plan: plan, stage: "load", startedHour: view.GameHour,
		initialStores: ship.Stores.ProvisionsTons, conversion: conversion}
	_, mission.coverage = world.market.Book(plan.Origin)
	mission.storeUnitPence = provisionUnit(input.Markets[plan.Origin])
	lane := engine.LaneKey(plan)
	row := store.Settlement{ID: id, MissionID: id, ActionID: id, ShipID: plan.ShipID, Kind: plan.Kind,
		Origin: plan.Origin, Destination: plan.Destination, Commodity: plan.Commodity, Lane: lane, StartedAt: view.At, CostsKnown: true}
	if plan.Kind == "relocation" {
		row.Lane = engine.SourcingLaneKey(input, plan)
		row.ExplorationExpensePence = &mission.exploration
		world.result.Probes++
	}
	world.accounting.Start(row)
	world.missions[plan.ShipID] = mission
	for _, transfer := range transfers {
		if err := world.accounting.Remit(id, transfer.Rate.FromRegion, transfer.Rate.ToRegion, transfer.Gross, transfer.Gross-transfer.Net); err != nil {
			return err
		}
	}
	if plan.StoresToBuy > 0 {
		fill, err := world.market.Execute(plan.Origin, "provisions", "buy", plan.StoresToBuy, plan.StoresLimitPence)
		if err != nil {
			return err
		}
		if err := world.tradeReceipt(mission, plan.Origin, "provision", fill); err != nil {
			return err
		}
		ship.Stores.ProvisionsTons += float64(fill.Quantity)
		mission.purchasedStores += float64(fill.Quantity)
		if fill.Quantity != plan.StoresToBuy {
			return errors.New("accepted plan could not buy its complete safe stores")
		}
	}
	if plan.Quantity > 0 {
		fill, err := world.market.Execute(plan.Origin, plan.Commodity, "buy", plan.Quantity, plan.BuyLimitPence)
		if err != nil {
			return err
		}
		if err := world.tradeReceipt(mission, plan.Origin, "buy", fill); err != nil {
			return err
		}
		if ship.CargoManifest == nil {
			ship.CargoManifest = make(map[string]int64)
		}
		ship.CargoManifest[plan.Commodity] += fill.Quantity
		ship.CargoUsedTons += fill.Quantity
		if fill.Quantity != plan.Quantity {
			return errors.New("accepted plan exceeded its residual source depth")
		}
	}
	port := catalogPort(view.State.Catalog, plan.Origin)
	if err := world.fleet.Handle(plan.ShipID, plan.Quantity+plan.StoresToBuy, port.IsHub, view.At, view.State.Clock.Calendar.TimeScale); err != nil {
		return err
	}
	mission.nextAt = *ship.Ship.TurnaroundUnlocksAt
	world.idleHours[plan.ShipID] = 0
	return nil
}

func (world *variantWorld) advanceMission(view FrameView, mission *simulatedMission, mayStart bool) error {
	ship := world.fleet.Ship(mission.plan.ShipID)
	switch mission.stage {
	case "load":
		key := mission.plan.Origin + "|" + mission.plan.Destination + "|" + ship.Ship.ShipClass
		route, exists := view.State.Routes[key]
		if !exists || view.State.Clock.Paused {
			return nil
		}
		arrival, err := world.fleet.Depart(ship.Ship.ID, route, view.At, view.State.Clock.Calendar.TimeScale)
		if err != nil {
			return err
		}
		world.ledger.RecordDeparture(engine.SourcingDeparture{ShipID: ship.Ship.ID, Origin: mission.plan.Origin, At: view.At, Laden: cargoTons(*ship) > 0})
		if mission.conversion {
			world.result.Conversions++
			mission.conversion = false
		}
		mission.stage, mission.nextAt = "sailing", arrival
		return world.chargeFleetBills()
	case "inspect":
		return world.inspect(view, mission, mayStart)
	case "deliver":
		return world.deliver(view, mission)
	case "finish":
		_, err := world.settleMission(view, mission)
		return err
	}
	return nil
}

func (world *variantWorld) inspect(view FrameView, mission *simulatedMission, mayStart bool) error {
	row, err := world.settleMission(view, mission)
	if err != nil {
		return err
	}
	input, err := world.planningInput(view, *world.fleet.Ship(mission.plan.ShipID))
	if err != nil {
		return err
	}
	var charter engine.Contract
	for _, candidate := range input.Contracts {
		if candidate.ID == mission.plan.ContractID {
			charter = candidate
			break
		}
	}
	spent := max(0, -settlementMoney(row).Net())
	candidate := engine.SizeRelocationCargo(input, mission.plan, charter, spent)
	row.ProbeOutcome = store.ProbeOutcomeFailed
	if candidate.Accepted {
		row.ProbeOutcome = store.ProbeOutcomeContinuation
	} else if world.recordHazardGap(candidate) {
		row.ProbeOutcome = ""
	} else {
		world.result.FailedProbes++
		if _, coverage := world.market.Book(row.Destination); coverage == Dark {
			world.result.DarkDecisions++
		}
	}
	// Settle already owns this row; its outcome is known only after inspection.
	world.accounting.settled[len(world.accounting.settled)-1].ProbeOutcome = row.ProbeOutcome
	world.ledger.RecordSettlement(row)
	if !candidate.Accepted || !mayStart {
		return nil
	}
	// The admission comparison includes the sunk probe. Cash funding from this
	// point prices only the continuation's remaining voyage, not the probe twice.
	candidate.Economics.GameDays -= max(0, mission.plan.Economics.GameDays)
	candidate.Economics.ExpectedNetPence += spent
	candidate.Economics.StressNetPence += spent
	if candidate.Economics.BallastRepeatGameDays > 0 {
		candidate.Economics.BallastRepeatNetPence += spent
	}
	candidate.Economics.NetPencePerGameDay = float64(candidate.Economics.ExpectedNetPence) / candidate.Economics.GameDays
	return world.startMission(view, input, candidate, true)
}

// charterProceedsPence matches the live settled-charter history window. Receipts
// on open missions, or missions settling as another kind, do not enter the taper.
func (world *variantWorld) charterProceedsPence(view FrameView) (int64, error) {
	cutoff := view.At.Add(-engine.CharterProceedsWindow(view.State.Clock.Calendar.TimeScale))
	proceeds, recorded, err := world.tape.charterProceedsBefore(cutoff, world.options.From)
	if err != nil {
		return 0, fmt.Errorf("load historical charter proceeds: %w", err)
	}
	if !recorded {
		for _, row := range world.seedSettlements {
			if row.Kind == "charter" && !row.CompletedAt.Before(cutoff) {
				proceeds += row.ProceedsPence
			}
		}
	}
	for _, row := range world.accounting.settled {
		if row.Kind == "charter" && !row.CompletedAt.Before(cutoff) {
			proceeds += row.ProceedsPence
		}
	}
	return proceeds, nil
}

func (world *variantWorld) deliver(view FrameView, mission *simulatedMission) error {
	ship := world.fleet.Ship(mission.plan.ShipID)
	port := ship.Ship.CurrentPortID
	input, err := world.planningInput(view, *ship)
	if err != nil {
		return err
	}
	commodities := make([]string, 0, len(ship.CargoManifest))
	for commodity, quantity := range ship.CargoManifest {
		if quantity > 0 {
			commodities = append(commodities, commodity)
		}
	}
	sort.Strings(commodities)
	for _, commodity := range commodities {
		quantity := ship.CargoManifest[commodity]
		var charter engine.Contract
		for _, candidate := range input.Contracts {
			if candidate.ContractType != "charter" || candidate.Status != "open" || candidate.Stale || candidate.OriginPortID != port ||
				candidate.CommodityID != commodity || candidate.RemainingQuantity <= 0 || candidate.RewardPerTonPence == nil ||
				(candidate.ExpiresAt != nil && !view.At.Before(*candidate.ExpiresAt)) || mission.plan.Origin == port {
				continue
			}
			// PlanningInputFor pre-tapers rewards by hold capacity; ranking and
			// payment restore the posted reward and taper the actual delivery,
			// and the planned contract wins no preferential force.
			if candidate.NominalRewardPerTonPence != nil {
				candidate.RewardPerTonPence = candidate.NominalRewardPerTonPence
			}
			if charter.RewardPerTonPence == nil || *candidate.RewardPerTonPence > *charter.RewardPerTonPence {
				charter = candidate
			}
		}
		book, coverage := world.market.Book(port)
		if charter.RewardPerTonPence != nil {
			tons := min(quantity, charter.RemainingQuantity)
			proceeds, err := world.charterProceedsPence(view)
			if err != nil {
				return err
			}
			perTon := engine.TaperedCharterReward(charter, view.State.Catalog, proceeds, tons)
			charterPay := tons * perTon
			marketPay := int64(0)
			for _, entry := range book.Books {
				if entry.CommodityID == commodity && entry.Depth != nil {
					if fill, err := engine.WalkDepth(entry.Depth.Bids, tons, false); err == nil {
						marketPay = fill.GrossPence
					}
				}
			}
			if charterPay >= marketPay {
				response, err := json.Marshal(map[string]int64{"delivered_quantity": tons, "reward_paid_pence": charterPay})
				if err != nil {
					return err
				}
				if err := world.accounting.Receipt(mission.id, catalogPort(view.State.Catalog, port).BankingRegion, "deliver-charter", response); err != nil {
					return err
				}
				ship.CargoManifest[commodity] -= tons
				ship.CargoUsedTons -= tons
				world.accounting.missions[mission.id].settlement.QuantityTons += tons
				world.deliveries = append(world.deliveries, ContractDelivery{ContractID: charter.ID, Quantity: tons, At: view.At})
				world.countCoverage(mission.coverage, tons)
				// Live returns to arrival after one charter receipt, so another
				// simulated pass must reconsider any remaining carried-in cargo.
				break
			}
		}
		if quantity > 0 && coverage != Dark {
			fill, err := world.market.Execute(port, commodity, "sell", quantity, 0)
			if err != nil {
				return err
			}
			if err := world.tradeReceipt(mission, port, "sell", fill); err != nil {
				return err
			}
			ship.CargoManifest[commodity] -= fill.Quantity
			ship.CargoUsedTons -= fill.Quantity
			world.accounting.missions[mission.id].settlement.QuantityTons += fill.Quantity
			world.countCoverage(fill.Coverage, fill.Quantity)
			world.sellFills = append(world.sellFills, engine.PlayerBidSellFill{PortID: port, CommodityID: commodity, Side: "ask", Quantity: fill.Quantity, CreatedAt: view.At})
		} else if quantity > 0 {
			world.result.DarkDecisions++
		}
	}
	if cargoTons(*ship) != 0 {
		mission.nextAt = time.Time{}
		return nil
	}
	mission.stage = "finish"
	_, err = world.settleMission(view, mission)
	return err
}

func (world *variantWorld) settleMission(view FrameView, mission *simulatedMission) (store.Settlement, error) {
	if err := world.fleet.Offshore(mission.plan.ShipID, view.At); err != nil {
		return store.Settlement{}, err
	}
	if err := world.chargeFleetBills(); err != nil {
		return store.Settlement{}, err
	}
	account := world.accounting.missions[mission.id]
	account.settlement.GameSeconds = int64(math.Round(max(0, view.GameHour-mission.startedHour) * 3600))
	account.settlement.Destination = world.fleet.Ship(mission.plan.ShipID).Ship.CurrentPortID
	row, err := world.accounting.Settle(mission.id, view.At)
	if err != nil {
		return row, err
	}
	if mission.plan.Kind == "relocation" {
		ship := world.fleet.Ship(mission.plan.ShipID)
		consumed := max(0, mission.initialStores+mission.purchasedStores-ship.Stores.ProvisionsTons)
		mission.exploration = int64(math.Ceil(consumed*mission.storeUnitPence)) + row.FeesPence + row.ExpensesPence
	} else {
		world.ledger.RecordSettlement(row)
	}
	delete(world.missions, mission.plan.ShipID)
	return row, nil
}

func (world *variantWorld) chargeFleetBills() error {
	for _, bill := range world.fleet.PendingBills() {
		if err := world.accounting.Charge(bill); err != nil {
			return err
		}
	}
	return nil
}

func (world *variantWorld) tradeReceipt(mission *simulatedMission, port, stage string, fill TradeResult) error {
	response, err := json.Marshal(map[string]int64{"filled_quantity": fill.Quantity, "total_gross_pence": fill.GrossPence, "customs_duty_pence": fill.CustomsPence})
	if err != nil {
		return err
	}
	region := ""
	for _, entry := range world.fleet.catalog.Ports {
		if entry.ID == port {
			region = entry.BankingRegion
			break
		}
	}
	return world.accounting.Receipt(mission.id, region, stage, response)
}

func (world *variantWorld) countCoverage(coverage Coverage, tons int64) {
	if coverage == Observed {
		world.result.ObservedTons += tons
	} else if coverage == Carried {
		world.result.CarriedTons += tons
	}
}

func (world *variantWorld) finish(view FrameView) (VariantResult, error) {
	world.result.Settlements = world.accounting.Settlements()
	world.result.SettlementCount = int64(len(world.result.Settlements))
	for _, row := range world.result.Settlements {
		world.result.RealizedNetPence += settlementMoney(row).Net()
		if row.Kind == "relocation" && row.ExplorationExpensePence != nil {
			world.result.ProbeExpensePence += *row.ExplorationExpensePence
		}
	}
	world.result.Ships = world.fleet.Ships()
	balances := world.accounting.Balances()
	for _, vault := range vaultsFor(balances) {
		world.result.FinalCashPence += vault.BalancePence
		world.result.Vaults = append(world.result.Vaults, RegionalBalance{Region: vault.BankingRegion, BalancePence: vault.BalancePence})
	}
	world.result.FinalNetWorthPence = NetWorth(balances, world.result.Ships, view.State.Catalog)
	if world.result.TotalShipGameHours > 0 {
		world.result.IdlePercent = 100 * world.result.IdleGameHours / world.result.TotalShipGameHours
	}
	total := world.result.ObservedTons + world.result.CarriedTons
	world.result.LowCoverage = total == 0 || float64(world.result.ObservedTons)/float64(total) < 0.6
	var err error
	world.result.Reconciliation, err = world.accounting.Reconcile()
	return world.result, err
}

func catalogPort(catalog engine.Catalog, id string) engine.Port {
	for _, port := range catalog.Ports {
		if port.ID == id {
			return port
		}
	}
	return engine.Port{}
}

func cargoTons(ship engine.ShipState) int64 {
	var tons int64
	for _, quantity := range ship.CargoManifest {
		tons += max(0, quantity)
	}
	return max(tons, ship.CargoUsedTons)
}

func provisionUnit(book engine.Market) float64 {
	for _, entry := range book.Books {
		if entry.CommodityID != "provisions" || entry.Depth == nil {
			continue
		}
		fill, err := engine.WalkDepth(entry.Depth.Asks, 1, true)
		if err == nil {
			return float64(fill.GrossPence + ceilBPS(fill.GrossPence, book.CustomsBPS) + 1)
		}
	}
	return 0
}
