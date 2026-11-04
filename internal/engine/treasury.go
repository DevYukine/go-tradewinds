package engine

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func (r *Runner) startMaintenance(ctx context.Context, ship ShipState, stage, path string, body any) (bool, error) {
	mission := &store.Mission{AccountID: r.account.ID, ShipID: ship.Ship.ID, Phase: "maintenance", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "maintenance", ShipID: ship.Ship.ID}, CostsKnown: true}
	return true, r.stageAction(ctx, mission, &data, ship, stage, path, body, stage == "dock")
}

// maintain runs even when there is no profitable voyage. Idle crews still eat;
// a hold and an escrow obligation do not turn stores into tradable cargo.
func (r *Runner) maintain(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	if ship.Ship.Status == "sailing" || ship.Ship.Status == "sunk" || ship.Ship.ConvoyID != "" {
		return false, nil
	}
	hull, exists := findHull(r.stateCache.Catalog, ship.Ship.ShipClass)
	if !exists {
		return false, r.hold(ctx, ship.Ship.ID, "idle maintenance lacks the ship's catalog crew requirements")
	}
	targetStores := 1 + float64(hull.CrewRequired)/200*2
	if ship.Stores.ProvisionsTons+1e-9 < targetStores {
		if mission == nil {
			mission = &store.Mission{AccountID: r.account.ID, ShipID: ship.Ship.ID, Phase: "maintenance", Generation: uuid.NewString()}
			data = &missionData{Plan: Plan{Kind: "maintenance", ShipID: ship.Ship.ID}, CostsKnown: true}
			return true, r.saveMission(ctx, mission, data)
		}
		if ship.Ship.Status != "berthed" {
			return r.dockForMission(ctx, ship, mission, data, true)
		}
		return r.provisionMission(ctx, ship, mission, data, targetStores)
	}
	if mission != nil && data.Plan.Kind == "maintenance" {
		mission.Phase = "maintenance_done"
		return true, r.saveMission(ctx, mission, data)
	}
	port, exists := findPort(r.stateCache.Catalog, ship.Ship.CurrentPortID)
	if !exists || !port.HasShipyard || ship.Ship.CannonsCount >= min(4, hull.MaxCannons) {
		return false, nil
	}
	target := min(int64(4), hull.MaxCannons)
	cost := (target - ship.Ship.CannonsCount) * 15_000
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	reserve := input.ReserveByRegion[port.BankingRegion]
	for _, deferred := range input.DeferredLoanReserves {
		if deferred.Region == port.BankingRegion {
			reserve = growthCashTotal(reserve, deferred.FullReserve)
		}
	}
	if input.Balances[port.BankingRegion]-cost < reserve {
		return false, r.hold(ctx, ship.Ship.ID, "armed maintenance deferred: measured gun cost would spend regional bills reserve")
	}
	if (target-ship.Ship.CannonsCount)*2 > ship.EffectiveHoldCapacityTons-ship.CargoUsedTons {
		return false, r.hold(ctx, ship.Ship.ID, "cannon displacement does not fit beside actual cargo and stores")
	}
	if ship.Ship.Status != "berthed" {
		if mission != nil {
			return r.dockForMission(ctx, ship, mission, data, true)
		}
		if ship.Ship.DockQueuedAt == nil {
			return r.startMaintenance(ctx, ship, "dock", "/v1/fleet/dock", map[string]any{"ship_id": ship.Ship.ID})
		}
		return false, nil
	}
	body := map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": target}
	if mission != nil {
		return true, r.stage(ctx, mission, data, ship, "refit", "/v1/shipyard/modify-ship", body)
	}
	return r.startMaintenance(ctx, ship, "refit", "/v1/shipyard/modify-ship", body)
}

// ProtectedReservePence is the regional operating floor. Fleet and factor
// upkeep is billed only at home and is added by obligationReserves.
func ProtectedReservePence(AccountState) int64 {
	return 100_000
}

func (r *Runner) protectedReservePence() int64 {
	return ProtectedReservePence(r.stateCache)
}

// obligationReserves protects observed obligations, not every vault or every
// hypothetical destination. Candidate-specific funding adds its chosen ports.
func obligationReserves(state AccountState, snapshot map[string]json.RawMessage, base int64, excludedLoans ...string) map[string]int64 {
	required, _ := obligationReservePlan(state, snapshot, base, excludedLoans...)
	return required
}

func obligationReservePlan(state AccountState, snapshot map[string]json.RawMessage, base int64, excludedLoans ...string) (map[string]int64, []loanReserveDeferral) {
	required := make(map[string]int64)
	var deferred []loanReserveDeferral
	if home, exists := findPort(state.Catalog, state.HomePortID); exists {
		upkeep := max(0, DailyUpkeep(state.Ships, state.Catalog, len(state.Factors)))
		required[home.BankingRegion] = growthCashTotal(base, upkeep, upkeep, upkeep)
	}
	ships := make(map[string]bool, len(state.Ships))
	for _, ship := range state.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		ships[ship.Ship.ID] = true
		portID := ship.Ship.CurrentPortID
		if ship.Ship.Status == "sailing" {
			// A sailing ship retains its departed origin in current_port_id.
			// Protect its observed arrival, not another berth at that origin.
			if len(ship.RouteLegs) == 0 {
				continue
			}
			portID = ship.RouteLegs[len(ship.RouteLegs)-1].ToNode
		} else if ship.Voyage != nil || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			continue
		}
		if port, exists := findPort(state.Catalog, portID); exists {
			required[port.BankingRegion] = max(required[port.BankingRegion], base)
			required[port.BankingRegion] += 3 * state.Ports[port.ID].BerthDuesPerDayPence
		}
	}
	for _, contract := range state.Contracts {
		if contract.ContractType != "freight" || contract.Status != "active" || !ships[contract.CarrierShipID] {
			continue
		}
		if destination, exists := findPort(state.Catalog, contract.DestPortID); exists {
			required[destination.BankingRegion] = max(required[destination.BankingRegion],
				base+3*state.Ports[destination.ID].BerthDuesPerDayPence)
		}
	}
	warehouses := decode[[]struct {
		PortID        string `json:"port_id"`
		LeasedTonnage int64  `json:"leased_tonnage"`
	}](snapshot, "warehouses")
	for _, warehouse := range warehouses {
		if warehouse.LeasedTonnage <= 0 {
			continue
		}
		if port, exists := findPort(state.Catalog, warehouse.PortID); exists {
			required[port.BankingRegion] = max(required[port.BankingRegion], base)
		}
	}
	pending := decode[underwritingPending](snapshot, "_underwriting_pending")
	if pending.CollateralPence > 0 && pending.CollateralPence <= maximumPence {
		for _, region := range pending.Regions {
			required[region] += pending.CollateralPence
		}
	}
	marque := decode[marquePending](snapshot, "_marque_pending")
	if marque.BankingRegion != "" && marque.CostPence > 0 && marque.CostPence <= maximumPence {
		required[marque.BankingRegion] += marque.CostPence
	}
	for _, loan := range state.Loans {
		if slices.Contains(excludedLoans, loan.ID) {
			continue
		}
		reserve := state.loanServiceReserve(loan, state.Clock.RealUTC.Add(serverClockStaleness))
		required[loan.BankingRegion] = growthCashTotal(required[loan.BankingRegion], reserve)
		if reserve == 0 && loanReserve(loan) > 0 {
			deferred = append(deferred, loanReserveDeferral{
				Region: loan.BankingRegion, FullReserve: loanReserve(loan),
				ReserveFrom: loan.NextPaymentDueAt.Add(-gameDuration(loanServiceLeadGameDays, state.Clock.Calendar.TimeScale) - serverClockStaleness),
			})
		}
	}
	for region, fees := range expansionFeeCommitments(state, snapshot) {
		required[region] = growthCashTotal(max(required[region], base), fees)
	}
	reserveAmericaFlagship(required, state, snapshot)
	reserveLoanRefinance(required, state, snapshot)
	return required, deferred
}

func (r *Runner) treasuryReserves(ctx context.Context) (map[string]int64, error) {
	return r.treasuryReservesExceptLoan(ctx, "")
}

func (r *Runner) treasuryReservesExceptLoan(ctx context.Context, excludedLoan string) (map[string]int64, error) {
	return r.treasuryReservesFor(ctx, r.snapshot, excludedLoan)
}

func (r *Runner) treasuryReservesFor(ctx context.Context, snapshot map[string]json.RawMessage, excludedLoans ...string) (map[string]int64, error) {
	state := r.stateCache
	_, _, validClock := r.serverBounds()
	state.loanClockUnknown = !validClock
	base := r.protectedReservePence()
	required := obligationReserves(state, snapshot, base, excludedLoans...)
	portRegions := make(map[string]string, len(state.Catalog.Ports))
	for _, port := range state.Catalog.Ports {
		portRegions[port.ID] = port.BankingRegion
	}
	futuresCash, err := r.db.FuturesCashReserveByRegion(ctx, r.account.ID, portRegions)
	if err != nil {
		return nil, err
	}
	for region, cash := range futuresCash {
		required[region] = growthCashTotal(required[region], cash)
	}
	for _, ship := range state.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return nil, err
		}
		conditionalIncome := ordinaryIncomePending(&data) &&
			(mission != nil && (mission.Phase == "provision" || mission.Phase == "buy" || mission.Phase == "freight"))
		if mission != nil && !conditionalIncome && !r.loadingReservationSuspended(mission, &data, ship) {
			if destination, exists := findPort(state.Catalog, mission.Destination); exists {
				required[destination.BankingRegion] = max(required[destination.BankingRegion],
					base+3*state.Ports[destination.ID].BerthDuesPerDayPence)
			}
		}
	}
	return required, nil
}

func (r *Runner) manageTreasury(ctx context.Context) (bool, error) {
	if !r.fresh("rates") || !r.fresh("settings") || !r.fresh("profile") {
		return false, r.hold(ctx, "", "treasury requires live regional fee matrix, settings and company home")
	}
	if err := r.expireAwaitingExpansion(ctx, time.Now().UTC()); err != nil {
		return false, err
	}
	input, err := r.committedPlanningInput(ctx, ShipState{})
	if err != nil {
		return false, err
	}
	balances := input.Balances
	required := input.ReserveByRegion
	baseRequired, err := r.treasuryReserves(ctx)
	if err != nil {
		return false, err
	}
	for region, protected := range required {
		// Ship controllers fund selected cargo/return commitments with their
		// own journal so transfer fees belong to that operating mission. The
		// company controller still protects those funds from other donors.
		required[region] = max(baseRequired[region], min(protected, balances[region]))
	}
	settings := decode[struct {
		AutoRemitForBills bool  `json:"auto_remit_for_bills"`
		MinReservePence   int64 `json:"auto_remit_min_reserve_pence"`
	}](r.snapshot, "settings")
	upkeep := max(0, DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors)))
	minimumReserve := growthCashTotal(r.protectedReservePence(), upkeep, upkeep, upkeep)
	if !settings.AutoRemitForBills || settings.MinReservePence < minimumReserve {
		mission := &store.Mission{
			AccountID:  r.account.ID,
			ShipID:     "__treasury",
			Phase:      "settings",
			Generation: uuid.NewString(),
		}
		data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
		return true, r.stage(ctx, mission, &data, ShipState{}, "settings", "/v1/company/settings", map[string]any{
			"auto_remit_for_bills":         true,
			"auto_remit_min_reserve_pence": minimumReserve,
		})
	}
	return r.fundTreasuryMission(ctx, balances, required, "__treasury", nil, nil, false)
}

func (r *Runner) fundTreasury(ctx context.Context, balances, required map[string]int64) (bool, error) {
	return r.fundTreasuryMission(ctx, balances, required, "__treasury", nil, nil, true)
}

func (r *Runner) fundTreasuryMission(ctx context.Context, balances, required map[string]int64, owner string, repayment *loanRepaymentIntent, funding *store.Mission, growthFunding bool) (bool, error) {
	current, data, err := r.loadMission(ctx, owner)
	if err != nil {
		return false, err
	}
	if current != nil {
		r.wakeBy(data.CooldownUntil)
		return false, r.holdExpected(ctx, owner, "regional funding waits for the existing treasury intent and its persisted retry")
	}
	regions := make([]string, 0, len(required))
	for region := range required {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	if repayment != nil {
		regions = []string{repayment.Before.BankingRegion}
	}
	for _, destination := range regions {
		shortfall := required[destination] - balances[destination]
		if shortfall <= 0 {
			continue
		}
		best, gross, net, ok := bestRemittance(balances, required, r.stateCache.Rates, destination)
		if !ok {
			if err := r.hold(ctx, "", "regional reserve shortfall in "+destination+"; no observed-fee remittance preserves source reserves"); err != nil {
				return false, err
			}
			continue
		}
		mission := &store.Mission{
			AccountID:   r.account.ID,
			ShipID:      owner,
			Phase:       "remit",
			Destination: destination,
			Quantity:    gross,
			Generation:  uuid.NewString(),
		}
		data := missionData{
			Plan:          Plan{Kind: "treasury"},
			StartedAt:     time.Now().UTC(),
			CostsKnown:    true,
			LoanRepayment: repayment,
			GrowthFunding: growthFunding,
		}
		if funding != nil {
			data.FundingMission, data.FundingGeneration = funding.ShipID, funding.Generation
		}
		if err := r.decision(
			ctx,
			"",
			"remittance",
			"fund regional cash from the lowest-fee donor that preserves its reserve, staging one partial remittance per fresh state",
			map[string]any{
				"from_region":       best.FromRegion,
				"to_region":         destination,
				"gross_pence":       gross,
				"fee_bps":           best.FeeBPS,
				"net_minimum_pence": net,
			},
		); err != nil {
			return false, err
		}
		return true, r.stage(ctx, mission, &data, ShipState{}, "remit", "/v1/treasury/remit", map[string]any{"from_region": best.FromRegion, "to_region": destination, "gross_amount_pence": gross})
	}
	return false, nil
}

func bestRemittance(balances, required map[string]int64, rates []Rate, destination string) (Rate, int64, int64, bool) {
	shortfall := required[destination] - balances[destination]
	var best Rate
	var gross, net int64
	found := false
	for _, rate := range rates {
		if rate.ToRegion != destination || rate.FromRegion == destination {
			continue
		}
		needed, err := GrossRemittance(shortfall, rate.FeeBPS)
		if err != nil {
			continue
		}
		if required[rate.FromRegion] < 0 || balances[rate.FromRegion] <= required[rate.FromRegion] {
			continue
		}
		surplus := balances[rate.FromRegion] - required[rate.FromRegion]
		amount := min(needed, surplus)
		if amount <= 0 || amount > maximumPence {
			continue
		}
		// This is the observed server's gross-fee rounding, not a hypothetical
		// post-transfer balance. Every simulated penny has an affordable donor.
		delivered := amount - amount*rate.FeeBPS/10_000
		if delivered <= 0 {
			continue
		}
		if !found || rate.FeeBPS < best.FeeBPS || rate.FeeBPS == best.FeeBPS && rate.FromRegion < best.FromRegion {
			best, gross, net, found = rate, amount, delivered, true
		}
	}
	return best, gross, net, found
}

type observedRemittance struct {
	Rate  Rate
	Gross int64
	Net   int64
}

func measuredFunding(balances, required map[string]int64, rates []Rate) (map[string]int64, int64, []observedRemittance, bool) {
	projected := clonePence(balances)
	regions := make([]string, 0, len(required))
	for region := range required {
		if required[region] < 0 || required[region] == math.MaxInt64 {
			return projected, 0, nil, false
		}
		regions = append(regions, region)
	}
	sort.Strings(regions)
	var fees int64
	var transfers []observedRemittance
	for _, destination := range regions {
		for projected[destination] < required[destination] {
			rate, gross, net, ok := bestRemittance(projected, required, rates, destination)
			if !ok {
				return projected, fees, transfers, false
			}
			projected[rate.FromRegion] -= gross
			if projected[destination] > math.MaxInt64-net || fees > math.MaxInt64-(gross-net) {
				return projected, fees, transfers, false
			}
			projected[destination] += net
			fees += gross - net
			transfers = append(transfers, observedRemittance{Rate: rate, Gross: gross, Net: net})
		}
	}
	return projected, fees, transfers, true
}
