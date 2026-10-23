package engine

import (
	"context"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

func (r *Runner) startMaintenance(ctx context.Context, ship ShipState, stage, path string, body any) (bool, error) {
	mission := &store.Mission{AccountID: r.account.ID, ShipID: ship.Ship.ID, Phase: "maintenance", Generation: uuid.NewString()}
	data := missionData{Plan: Plan{Kind: "maintenance", ShipID: ship.Ship.ID}, CostsKnown: true}
	return true, r.stage(ctx, mission, &data, ship, stage, path, body)
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
		}
		return r.provisionMission(ctx, ship, mission, data, targetStores)
	}
	port, exists := findPort(r.stateCache.Catalog, ship.Ship.CurrentPortID)
	if !exists || !port.HasShipyard || ship.Ship.CannonsCount >= min(4, hull.MaxCannons) {
		return false, nil
	}
	if ship.Ship.Status != "berthed" {
		if mission != nil {
			return r.dockForMission(ctx, ship, mission, data)
		}
		if ship.Ship.DockQueuedAt == nil {
			return r.startMaintenance(ctx, ship, "dock", "/v1/fleet/dock", map[string]any{"ship_id": ship.Ship.ID})
		}
		return false, nil
	}
	target := min(int64(4), hull.MaxCannons)
	cost := (target - ship.Ship.CannonsCount) * 15_000
	input := r.planningInput(ship)
	if input.Balances[port.BankingRegion]-cost < input.ReserveByRegion[port.BankingRegion] {
		return false, r.hold(ctx, ship.Ship.ID, "armed maintenance deferred: measured gun cost would spend regional bills reserve")
	}
	if (target-ship.Ship.CannonsCount)*2 > ship.EffectiveHoldCapacityTons-ship.CargoUsedTons {
		return false, r.hold(ctx, ship.Ship.ID, "cannon displacement does not fit beside actual cargo and stores")
	}
	body := map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": target}
	if mission != nil {
		return true, r.stage(ctx, mission, data, ship, "refit", "/v1/shipyard/modify-ship", body)
	}
	return r.startMaintenance(ctx, ship, "refit", "/v1/shipyard/modify-ship", body)
}

func (r *Runner) treasuryReserves(ctx context.Context) (map[string]int64, error) {
	state := r.stateCache
	required := make(map[string]int64)
	upkeep := DailyUpkeep(state.Ships, state.Catalog, len(state.Factors))
	if home, exists := findPort(state.Catalog, state.HomePortID); exists {
		required[home.BankingRegion] = r.opts.ReservePence + upkeep*3
	}
	for _, ship := range state.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		if port, exists := findPort(state.Catalog, ship.Ship.CurrentPortID); exists {
			required[port.BankingRegion] = max(required[port.BankingRegion], r.opts.ReservePence)
			if detail, exists := state.Ports[port.ID]; exists {
				required[port.BankingRegion] += 3 * detail.BerthDuesPerDayPence
			}
		}
		mission, err := r.db.GetMission(ctx, r.account.ID, ship.Ship.ID)
		if err != nil {
			return nil, err
		}
		if mission != nil {
			if destination, exists := findPort(state.Catalog, mission.Destination); exists {
				required[destination.BankingRegion] = max(required[destination.BankingRegion], r.opts.ReservePence+3*state.Ports[destination.ID].BerthDuesPerDayPence)
			}
		}
		for _, obligation := range r.activeFreight(ship.Ship.ID) {
			if destination, exists := findPort(state.Catalog, obligation.DestPortID); exists {
				required[destination.BankingRegion] = max(required[destination.BankingRegion], r.opts.ReservePence+3*state.Ports[destination.ID].BerthDuesPerDayPence)
			}
		}
		// Fund a measured destination before the ordinary execution reserve gate.
		input := r.planningInput(ship)
		for _, port := range state.Catalog.Ports {
			input.Balances[port.BankingRegion] = max(input.Balances[port.BankingRegion], input.ReserveByRegion[port.BankingRegion]+100_000)
		}
		plans := PlanVoyages(input)
		if len(plans) > 0 && plans[0].Accepted {
			if destination, exists := findPort(state.Catalog, plans[0].Destination); exists {
				required[destination.BankingRegion] = max(required[destination.BankingRegion], r.opts.ReservePence+3*state.Ports[destination.ID].BerthDuesPerDayPence)
			}
		}
	}
	warehouses := decode[[]struct {
		PortID        string `json:"port_id"`
		LeasedTonnage int64  `json:"leased_tonnage"`
	}](r.snapshot, "warehouses")
	for _, warehouse := range warehouses {
		if warehouse.LeasedTonnage <= 0 {
			continue
		}
		if port, exists := findPort(state.Catalog, warehouse.PortID); exists {
			required[port.BankingRegion] = max(required[port.BankingRegion], r.opts.ReservePence)
		}
	}
	return required, nil
}

func (r *Runner) manageTreasury(ctx context.Context) (bool, error) {
	if !r.fresh("rates") || !r.fresh("settings") || !r.fresh("profile") {
		return false, r.hold(ctx, "", "treasury requires live regional fee matrix, settings and company home")
	}
	state := r.stateCache
	balances := r.planningInput(ShipState{}).Balances
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return false, err
	}
	upkeep := DailyUpkeep(state.Ships, state.Catalog, len(state.Factors))
	settings := decode[struct {
		AutoRemitForBills bool  `json:"auto_remit_for_bills"`
		MinReservePence   int64 `json:"auto_remit_min_reserve_pence"`
	}](r.snapshot, "settings")
	if !settings.AutoRemitForBills || settings.MinReservePence < r.opts.ReservePence+3*upkeep {
		mission := &store.Mission{
			AccountID:  r.account.ID,
			ShipID:     "__treasury",
			Phase:      "settings",
			Generation: uuid.NewString(),
		}
		data := missionData{Plan: Plan{Kind: "treasury"}, CostsKnown: true}
		return true, r.stage(ctx, mission, &data, ShipState{}, "settings", "/v1/company/settings", map[string]any{
			"auto_remit_for_bills":         true,
			"auto_remit_min_reserve_pence": r.opts.ReservePence + 3*upkeep,
		})
	}
	return r.fundTreasury(ctx, balances, required)
}

func (r *Runner) fundTreasury(ctx context.Context, balances, required map[string]int64) (bool, error) {
	state := r.stateCache
	regions := make([]string, 0, len(required))
	for region := range required {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	for _, destination := range regions {
		shortfall := required[destination] - balances[destination]
		if shortfall <= 0 {
			continue
		}
		var best *Rate
		var gross, net int64
		for i := range state.Rates {
			rate := &state.Rates[i]
			if rate.ToRegion != destination || rate.FromRegion == destination {
				continue
			}
			needed, err := GrossRemittance(shortfall, rate.FeeBPS)
			if err != nil {
				continue
			}
			// A donor never remits past its own protected reserve. When no
			// single donor covers the whole shortfall, stage one partial
			// remittance now; the next fresh-state pass combines further
			// donors while aggregate surplus suffices.
			surplus := balances[rate.FromRegion] - required[rate.FromRegion]
			if surplus <= 0 {
				continue
			}
			amount := min(needed, surplus)
			// Observed server rounding charges gross times bps over 10,000,
			// truncated; the diagnostic must promise only the surviving net.
			delivered := amount - amount*rate.FeeBPS/10_000
			if delivered <= 0 {
				continue
			}
			if best == nil || rate.FeeBPS < best.FeeBPS {
				best = rate
				gross = amount
				net = delivered
			}
		}
		if best == nil {
			if err := r.hold(ctx, "", "regional reserve shortfall in "+destination+"; no observed-fee remittance preserves source reserves"); err != nil {
				return false, err
			}
			continue
		}
		mission := &store.Mission{
			AccountID:   r.account.ID,
			ShipID:      "__treasury",
			Phase:       "remit",
			Destination: destination,
			Quantity:    gross,
			Generation:  uuid.NewString(),
		}
		data := missionData{
			Plan:       Plan{Kind: "treasury"},
			StartedAt:  time.Now().UTC(),
			CostsKnown: true,
		}
		if err := r.decision(
			ctx,
			"",
			"remittance",
			"fund regional bills from the lowest-fee donor that preserves its reserve, staging one partial remittance per fresh state until combined surpluses cover the shortfall",
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
