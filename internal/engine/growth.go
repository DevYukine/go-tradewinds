package engine

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/names"
	"github.com/DevYukine/go-tradewinds/internal/store"
	"github.com/google/uuid"
)

type LaneProof struct {
	QuantityTons int64
	GameSeconds  int64
	NetPence     int64
	CostsKnown   bool
}

type GrowthInput struct {
	Plan                        Plan
	Quote                       YardQuote
	Hull                        Hull
	Proofs                      []LaneProof
	BalancePence                int64
	ReservePence                int64
	ExistingWorkingCapitalPence int64
	// Cycle tons are per-candidate-window repeat demand: exact-lane demand
	// shares all three pools; other lanes reserve only overlaps.
	ExistingCycleTons         int64
	OtherSourceCycleTons      int64
	OtherDestinationCycleTons int64
	OtherContractCycleTons    int64
	// Whole outstanding lots land once, so they reserve shared destination
	// bids or charter quantity once, never multiplied per cycle.
	OutstandingDestinationTons int64
	OutstandingContractTons    int64
	// Not-yet-purchased committed lots still consume shared source asks once;
	// lots already bought only repeat at their cycle rate.
	UnboughtSourceTons          int64
	ObservedAskCapacityTons     int64
	ObservedBidCapacityTons     int64
	ContractRemainingTons       int64
	ContractGameDaysRemaining   *float64
	IncrementalDailyUpkeepPence int64
	InitialStoresPence          int64
	MaxPaybackDays              float64
}

type GrowthDecision struct {
	Accepted                      bool    `json:"accepted"`
	Reason                        string  `json:"reason"`
	QuotedHullPence               int64   `json:"quoted_hull_pence"`
	SlippageReservePence          int64   `json:"slippage_reserve_pence"`
	CannonsPence                  int64   `json:"cannons_pence"`
	InitialStoresPence            int64   `json:"initial_stores_pence"`
	WorkingCapitalPence           int64   `json:"working_capital_pence"`
	IncrementalUpkeepPence        int64   `json:"incremental_upkeep_pence"`
	IncrementalNetPencePerGameDay float64 `json:"incremental_net_pence_per_game_day"`
	PaybackGameDays               float64 `json:"payback_game_days"`
}

func growthWorkingCapital(plan Plan) int64 {
	return 2 * (plan.Economics.PurchasePence + plan.Economics.CustomsPence +
		plan.Economics.CollateralPence + plan.Economics.BerthDuesPence +
		plan.Economics.ProvisionsPence + plan.Economics.TollPence)
}

// EvaluateGrowth separates sunk capital from refundable working capital and
// requires spare observed throughput after existing ships' cycle reservations.
func EvaluateGrowth(in GrowthInput) GrowthDecision {
	decision := GrowthDecision{Reason: "growth requires a fresh available turnkey quote and an accepted executable lane"}
	if !in.Plan.Accepted || !in.Quote.Available || in.Quote.BuildMode != "turnkey_cash" || in.Quote.TotalCostPence == nil || *in.Quote.TotalCostPence <= 0 {
		return decision
	}
	var provenQuantity, provenSeconds, provenNet int64
	var count int
	for _, proof := range in.Proofs {
		if !proof.CostsKnown || proof.GameSeconds <= 0 {
			decision.Reason = "lane history contains unresolved costs or capital loss; cannot establish positive realized net"
			return decision
		}
		// Failed procurement and loss-making voyages remain in the denominator
		// and integer net; selecting only winners would manufacture evidence.
		provenSeconds += proof.GameSeconds
		provenNet += proof.NetPence
		if proof.NetPence > 0 && proof.QuantityTons > 0 {
			count++
			provenQuantity += proof.QuantityTons
		}
	}
	if count < 2 {
		decision.Reason = "lane needs two durable positive realized settlements with known integer cost basis"
		return decision
	}
	if provenNet <= 0 {
		decision.Reason = "actual lane net including failed procurement and loss-making voyages is not positive"
		return decision
	}
	if in.Plan.Quantity <= 0 || in.ObservedAskCapacityTons < 2*(in.ExistingCycleTons+in.OtherSourceCycleTons+in.Plan.Quantity)+in.UnboughtSourceTons {
		decision.Reason = "measured source depth cannot fund two cycles after competing source demand"
		return decision
	}
	// Do not extrapolate a large hull's productivity beyond demonstrated
	// throughput. New capacity must be filled by an observed profitable lot.
	averageLot := float64(provenQuantity) / float64(count)
	if float64(in.Plan.Quantity) < averageLot*0.8 {
		decision.Reason = "marginal lane lot is below 80% of proven throughput; existing capacity is not saturated"
		return decision
	}
	provenRate := float64(provenNet) * 86_400 / float64(provenSeconds)
	scaledRate := provenRate * min(1, float64(in.Plan.Quantity)/averageLot)
	netRate := min(scaledRate, in.Plan.Economics.NetPencePerGameDay) - float64(in.IncrementalDailyUpkeepPence)
	if netRate <= 0 {
		decision.Reason = "nonlinear marginal fleet upkeep eliminates conservative incremental profit"
		return decision
	}
	guns := min(int64(4), in.Hull.MaxCannons)
	if guns < 1 || in.Hull.HoldCapacityTons <= 2*guns+in.Plan.Quantity {
		decision.Reason = "new hull cannot carry adequate cannons, safe stores and the profitable lot"
		return decision
	}
	hullPrice := *in.Quote.TotalCostPence
	slippage := ceilBPS(hullPrice, 1_000)
	gunPrice := guns * 15_000
	working := growthWorkingCapital(in.Plan)
	capital := hullPrice + slippage + gunPrice + in.InitialStoresPence
	payback := float64(capital) / netRate
	decision = GrowthDecision{
		QuotedHullPence:               hullPrice,
		SlippageReservePence:          slippage,
		CannonsPence:                  gunPrice,
		InitialStoresPence:            in.InitialStoresPence,
		WorkingCapitalPence:           working,
		IncrementalUpkeepPence:        in.IncrementalDailyUpkeepPence,
		IncrementalNetPencePerGameDay: netRate,
		PaybackGameDays:               payback,
	}
	if in.MaxPaybackDays <= 0 || payback > in.MaxPaybackDays {
		decision.Reason = "turnkey hull, cannon and store capital exceeds maximum game-day payback"
		return decision
	}
	if in.Plan.Economics.GameDays <= 0 {
		decision.Reason = "growth lane has no measured repeatable cycle duration"
		return decision
	}
	cycles := max(int64(2), int64(math.Ceil(payback/in.Plan.Economics.GameDays))+1)
	paybackTons := cycles*(in.ExistingCycleTons+in.OtherContractCycleTons+in.Plan.Quantity) + in.OutstandingContractTons
	if in.ContractRemainingTons >= 0 && in.ContractRemainingTons < paybackTons {
		decision.Reason = "finite charter capacity does not cover competing contract demand through capital payback plus a cycle"
		return decision
	}
	if in.Plan.Kind == "spot" && in.ObservedBidCapacityTons < 2*(in.ExistingCycleTons+in.OtherDestinationCycleTons+in.Plan.Quantity)+in.OutstandingDestinationTons {
		decision.Reason = "spot growth needs two cycles of measured spare destination bids; gossip has no executable capacity"
		return decision
	}
	if in.ContractGameDaysRemaining != nil && *in.ContractGameDaysRemaining < payback+in.Plan.Economics.GameDays {
		decision.Reason = "charter expires before capital payback plus a delivery cycle"
		return decision
	}
	required := capital + working + in.ExistingWorkingCapitalPence + 3*in.IncrementalDailyUpkeepPence + in.ReservePence
	if in.BalancePence < required {
		decision.Reason = "growth cannot preserve existing working capital, two new cycles, upkeep horizon and regional reserve"
		return decision
	}
	decision.Accepted = true
	decision.Reason = "two measured profitable cycles, spare live depth, marginal upkeep, armed/store budget and game-day payback pass; quote has no atomic price cap so 10% slippage remains reserved"
	return decision
}

func (r *Runner) considerGrowth(ctx context.Context) (bool, error) {
	if pending, err := r.db.GetMission(ctx, r.account.ID, "__growth"); err != nil || pending != nil {
		return false, err
	}
	// Incumbent commitments carry their not-yet-purchased remainder: bought
	// tons have left the source order book, unbought tons will still be bought.
	// They also carry the undelivered remainder: sold or delivered tons have
	// already landed and no longer consume shared bid or charter capacity.
	type incumbentLot struct {
		plan        Plan
		unbought    int64
		outstanding int64
	}
	var incumbents []incumbentLot
	workingByRegion := make(map[string]int64)
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return false, err
		}
		if mission != nil {
			if mission.Phase == "settling" || mission.Phase == "settled" || mission.Phase == "maintenance_done" {
				// Finish and release reusable capacity before ordering another hull.
				return false, nil
			}
			if data.Plan.Kind != "recovery" && data.Plan.Kind != "maintenance" {
				// Durable delivery totals include sealed freight, which cannot
				// consume the ordinary trade's charter or destination bids.
				deliveredTrade := data.DeliveredQuantity
				for _, obligation := range data.Freight {
					if obligation.Settled {
						deliveredTrade = max(0, deliveredTrade-obligation.Quantity)
					}
				}
				// The original trade remains allocated throughout its sailing return.
				incumbents = append(incumbents, incumbentLot{
					plan:        data.Plan,
					unbought:    max(int64(0), data.Plan.Quantity-data.BoughtQuantity),
					outstanding: max(int64(0), data.Plan.Quantity-deliveredTrade),
				})
				if origin, exists := findPort(r.stateCache.Catalog, data.Plan.Origin); exists {
					workingByRegion[origin.BankingRegion] += growthWorkingCapital(data.Plan)
				}
			}
			continue
		}
		if ship.Ship.Status == "sailing" || ship.Ship.ConvoyID != "" || ship.CargoUsedTons > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
			continue
		}
		// Direct assignment precedes ordinary ship processing, so use its
		// maintenance-first rule before sizing and buying any cargo.
		if changed, err := r.maintain(ctx, ship, nil, nil); changed || err != nil {
			return changed, err
		}
		plans := PlanVoyages(r.planningInput(ship))
		if len(plans) > 0 && plans[0].Accepted {
			return r.startVoyage(ctx, ship, plans[0])
		}
	}
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" && vault.BalancePence < 0 {
			return false, r.hold(ctx, "", "growth deferred while any regional vault is negative; existing fleet may continue earning")
		}
	}
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return false, err
	}
	for region, working := range workingByRegion {
		required[region] = max(required[region], r.opts.ReservePence) + working
	}
	balances := r.planningInput(ShipState{}).Balances
	for region, protected := range required {
		if balances[region] < protected {
			return false, r.hold(ctx, "", "growth deferred: incumbent working capital or protected regional bills are underfunded in "+region+"; existing fleet may continue earning")
		}
	}
	home, exists := findPort(r.stateCache.Catalog, r.stateCache.HomePortID)
	if !exists {
		return false, r.hold(ctx, "", "growth requires the authoritative home billing region")
	}
	// This is discretionary cash only. Actual regional placement and transfer
	// fees are funded before construction, never assumed from aggregate wealth.
	var available int64
	for region, balance := range balances {
		available += max(0, balance-required[region])
	}
	history, err := r.db.SettlementHistory(ctx, r.account.ID, "", 500)
	if err != nil {
		return false, err
	}
	if len(history) < 2 {
		return false, r.decision(ctx, "", "growth_rejected", "no two permanent realized lane settlements yet; chart balances are not proof of profit", nil)
	}
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	for _, port := range r.stateCache.Catalog.Ports {
		if !port.HasShipyard || !r.fresh("books/"+port.ID) || r.stateCache.Markets[port.ID].Visibility != "quay" {
			continue
		}
		for _, hull := range r.stateCache.Catalog.ShipClasses {
			key := port.ID + "|" + hull.ID
			if hull.Phase > r.stateCache.Clock.UnlockedPhase || !r.fresh("yard_quotes/"+key) {
				continue
			}
			candidate := ShipState{}
			candidate.Ship.ID = "growth-candidate"
			candidate.Ship.Status = "berthed"
			candidate.Ship.CurrentPortID = port.ID
			candidate.Ship.ShipClass = hull.ID
			candidate.Ship.CannonsCount = min(4, hull.MaxCannons)
			candidate.HoldCapacityTons = hull.HoldCapacityTons
			candidate.EffectiveHoldCapacityTons = hull.HoldCapacityTons - 2*candidate.Ship.CannonsCount
			candidate.CargoManifest = make(map[string]int64)
			input := r.planningInput(candidate)
			grownFleet[len(grownFleet)-1] = candidate
			deltaUpkeep := DailyUpkeep(grownFleet, r.stateCache.Catalog, len(r.stateCache.Factors)) - oldUpkeep
			input.FleetDailyUpkeepPence = 0
			plans := PlanVoyages(input)
			for _, plan := range plans {
				if !plan.Accepted || len(plan.FreightIDs) > 0 {
					continue
				}
				book := r.stateCache.Markets[port.ID]
				provisions := bookCommodity(book, "provisions")
				if provisions == nil || provisions.Depth == nil {
					if err := r.hold(ctx, "", "growth lacks executable depth for the new hull's full safe stores"); err != nil {
						return false, err
					}
					continue
				}
				storesFill, err := WalkDepth(provisions.Depth.Asks, plan.StoresToBuy, true)
				if err != nil {
					if holdErr := r.hold(ctx, "", "growth cannot fund the new hull's full safe stores: "+err.Error()); holdErr != nil {
						return false, holdErr
					}
					continue
				}
				initialStores := storesFill.GrossPence + ceilBPS(storesFill.GrossPence, book.CustomsBPS) + plan.StoresToBuy
				var proofs []LaneProof
				for _, settlement := range history {
					if settlement.Lane == laneKey(plan) {
						proofs = append(proofs, LaneProof{
							QuantityTons: settlement.QuantityTons,
							GameSeconds:  settlement.GameSeconds,
							NetPence: settlement.ProceedsPence - settlement.AcquisitionPence -
								settlement.FeesPence - settlement.ProvisionsPence - settlement.ExpensesPence,
							CostsKnown: settlement.CostsKnown,
						})
					}
				}
				var existingTons, otherSourceTons, otherDestinationTons, otherContractTons int64
				var outstandingDestination, outstandingContract, unboughtSource int64
				unknownCycle := false
				for _, incumbent := range incumbents {
					sameSource := incumbent.plan.Origin == plan.Origin && incumbent.plan.Commodity == plan.Commodity
					kind, _, _ := strings.Cut(incumbent.plan.Kind, "+")
					sameDestination := kind == "spot" && incumbent.plan.Destination == plan.Destination && incumbent.plan.Commodity == plan.Commodity
					sameContract := plan.ContractID != "" && incumbent.plan.ContractID == plan.ContractID
					if incumbent.plan.Quantity <= 0 || (!sameSource && !sameDestination && !sameContract) {
						continue
					}
					if incumbent.plan.Economics.GameDays <= 0 {
						unknownCycle = true
						break
					}
					tons := int64(math.Ceil(float64(incumbent.plan.Quantity) * plan.Economics.GameDays / incumbent.plan.Economics.GameDays))
					if laneKey(incumbent.plan) == laneKey(plan) && incumbent.plan.ContractID == plan.ContractID {
						// Exact-lane demand shares every pool. The undelivered
						// lot lands once on the shared charter or bid book
						// before repeat demand scales with rates.
						existingTons += tons
						unboughtSource += incumbent.unbought
						if plan.ContractID != "" {
							outstandingContract += incumbent.outstanding
						} else {
							outstandingDestination += incumbent.outstanding
						}
						continue
					}
					if sameSource {
						otherSourceTons += tons
						unboughtSource += incumbent.unbought
					}
					if sameDestination {
						otherDestinationTons += tons
						outstandingDestination += incumbent.outstanding
					}
					if sameContract {
						otherContractTons += tons
						outstandingContract += incumbent.outstanding
					}
				}
				if unknownCycle {
					if err := r.hold(ctx, "", "growth cannot allocate competing committed throughput without its full cycle duration"); err != nil {
						return false, err
					}
					continue
				}
				entry := bookCommodity(r.stateCache.Markets[port.ID], plan.Commodity)
				var depthTons int64
				if entry != nil && entry.Depth != nil {
					for _, level := range entry.Depth.Asks {
						if level.PricePence <= plan.BuyLimitPence {
							depthTons += max(0, level.RemainingQuantity)
						}
					}
				}
				var bidTons int64
				if plan.Kind == "spot" && r.fresh("books/"+plan.Destination) {
					demand := bookCommodity(r.stateCache.Markets[plan.Destination], plan.Commodity)
					if demand != nil && demand.Depth != nil {
						for _, level := range demand.Depth.Bids {
							if level.PricePence >= plan.SellLimitPence {
								bidTons += max(0, level.RemainingQuantity)
							}
						}
					}
				}
				remaining := int64(-1)
				var daysRemaining *float64
				if contract, exists := r.contract(plan.ContractID); exists {
					remaining = contract.RemainingQuantity
					if contract.ExpiresAt != nil {
						days := time.Until(*contract.ExpiresAt).Seconds() * float64(r.stateCache.Clock.Calendar.TimeScale) / 86_400
						daysRemaining = &days
					}
				}
				decision := EvaluateGrowth(GrowthInput{
					Plan:                        plan,
					Quote:                       r.stateCache.Quotes[key],
					Hull:                        hull,
					Proofs:                      proofs,
					BalancePence:                available,
					ExistingCycleTons:           existingTons,
					OtherSourceCycleTons:        otherSourceTons,
					OtherDestinationCycleTons:   otherDestinationTons,
					OtherContractCycleTons:      otherContractTons,
					OutstandingDestinationTons:  outstandingDestination,
					OutstandingContractTons:     outstandingContract,
					UnboughtSourceTons:          unboughtSource,
					ObservedAskCapacityTons:     depthTons,
					ObservedBidCapacityTons:     bidTons,
					ContractRemainingTons:       remaining,
					ContractGameDaysRemaining:   daysRemaining,
					IncrementalDailyUpkeepPence: deltaUpkeep,
					InitialStoresPence:          initialStores,
					MaxPaybackDays:              r.opts.MaxPaybackDays,
				})
				var growthRequired map[string]int64
				needsFunding := false
				if decision.Accepted {
					growthRequired = make(map[string]int64, len(required)+2)
					for region, protected := range required {
						growthRequired[region] = protected
					}
					growthRequired[port.BankingRegion] = max(growthRequired[port.BankingRegion], r.opts.ReservePence)
					growthRequired[port.BankingRegion] += decision.QuotedHullPence + decision.SlippageReservePence +
						decision.CannonsPence + decision.InitialStoresPence + decision.WorkingCapitalPence
					growthRequired[home.BankingRegion] += 3 * deltaUpkeep
					if destination, exists := findPort(r.stateCache.Catalog, plan.Destination); exists {
						growthRequired[destination.BankingRegion] = max(growthRequired[destination.BankingRegion],
							r.opts.ReservePence+3*r.stateCache.Ports[destination.ID].BerthDuesPerDayPence)
					}
					for region, protected := range growthRequired {
						if balances[region] < protected {
							needsFunding = true
							decision.Accepted = false
							decision.Reason = "growth awaits funded regional working capital and home-billed incremental upkeep including observed remittance fees"
							break
						}
					}
				}
				if err := r.decision(ctx, "", "growth:"+key+":"+laneKey(plan), decision.Reason, decision); err != nil {
					return false, err
				}
				if needsFunding {
					changed, err := r.fundTreasury(ctx, balances, growthRequired)
					if changed || err != nil {
						return changed, err
					}
					continue
				}
				if decision.Accepted {
					return r.startGrowth(ctx, port, hull, plan, decision.WorkingCapitalPence+workingByRegion[port.BankingRegion])
				}
			}
		}
	}
	return false, nil
}

func (r *Runner) startGrowth(ctx context.Context, port Port, hull Hull, plan Plan, workingCapital int64) (bool, error) {
	used := decode[[]string](r.snapshot, "_used_ship_names")
	for _, ship := range r.stateCache.Ships {
		used = append(used, ship.Ship.Name)
	}
	name := names.Next(used)
	mission := &store.Mission{
		AccountID:   r.account.ID,
		ShipID:      "__growth",
		Phase:       "growth_build",
		Generation:  uuid.NewString(),
		Destination: port.ID,
	}
	data := missionData{
		Plan:                      plan,
		BuildName:                 name,
		BuildClass:                hull.ID,
		BuildPort:                 port.ID,
		TargetCannons:             min(4, hull.MaxCannons),
		GrowthWorkingCapitalPence: workingCapital,
		CostsKnown:                true,
		StartedAt:                 time.Now().UTC(),
	}
	// The generated name and complete payload become durable before dispatch.
	return true, r.stage(ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{"port_id": port.ID, "name": name, "ship_class": hull.ID, "build_mode": "turnkey_cash"})
}

func (r *Runner) driveGrowth(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute {
		return false, r.hold(ctx, "", "observe mode: persisted growth intent retained")
	}
	if time.Now().Before(data.CooldownUntil) {
		return false, r.hold(ctx, "", "commissioning waits for persisted retry cooldown")
	}
	if data.Action == nil &&
		(mission.Phase == "growth_rejected" || (mission.Phase == "growth_build" && data.BuiltShipID == "")) {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	ship, exists := r.ship(data.BuiltShipID)
	if !exists {
		return false, r.hold(ctx, "", "build outcome not yet present in authoritative fleet; no duplicate build")
	}
	if ship.Ship.Status == "sunk" {
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	if ship.Ship.CurrentPortID != data.BuildPort || ship.Ship.Status != "berthed" {
		return false, r.hold(ctx, ship.Ship.ID, "new hull moved before commissioning; refit/provision awaits actual build port berth")
	}
	// A new hull consumes stores even while its cannon work is waiting.
	if ship.Stores.ProvisionsTons+1e-9 < math.Max(1, data.Plan.TargetStoresTons) {
		return r.provisionMission(ctx, ship, mission, data, math.Max(1, data.Plan.TargetStoresTons))
	}
	if mission.Phase == "growth_refit" {
		if ship.Ship.CannonsCount < data.TargetCannons {
			input := r.planningInput(ship)
			port, exists := findPort(input.Catalog, ship.Ship.CurrentPortID)
			cost := (data.TargetCannons - ship.Ship.CannonsCount) * 15_000
			if !exists || input.Balances[port.BankingRegion]-cost-data.GrowthWorkingCapitalPence < input.ReserveByRegion[port.BankingRegion] {
				return false, r.hold(ctx, ship.Ship.ID, "commissioning cannons must preserve incumbent/new working capital and regional upkeep reserve; existing fleet may continue earning")
			}
			return true, r.stage(ctx, mission, data, ship, "refit", "/v1/shipyard/modify-ship", map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": data.TargetCannons})
		}
		mission.Phase = "growth_provision"
		return true, r.saveMission(ctx, mission, data)
	}
	if mission.Phase == "growth_provision" {
		if ship.Stores.ProvisionsTons+1e-9 < data.Plan.TargetStoresTons {
			return r.provisionMission(ctx, ship, mission, data, math.Max(1, data.Plan.TargetStoresTons))
		}
		var actualHullCost *int64
		if data.CostsKnown {
			actualHullCost = &data.BuildCostPence
		}
		details := map[string]any{
			"ship_id":                ship.Ship.ID,
			"actual_hull_cost_pence": actualHullCost,
			"costs_known":            data.CostsKnown,
			"name":                   data.BuildName,
			"cannons":                ship.Ship.CannonsCount,
			"stores_tons":            ship.Stores.ProvisionsTons,
		}
		if err := r.decision(ctx, ship.Ship.ID, "growth_commissioned", "actual built hull is armed and safely provisioned; ordinary lane optimizer will fund the first cargo independently", details); err != nil {
			return false, err
		}
		return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
	}
	return false, r.hold(ctx, "", "persisted growth stage has no safe transition")
}
