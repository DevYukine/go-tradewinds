package sim

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

type Variant struct {
	Name                  string `json:"name"`
	NextRungReceiptStress bool   `json:"next_rung_receipt_stress"`
	SourcingCashBPS       *int64 `json:"sourcing_cash_bps,omitempty"`
}

func DefaultVariants() []Variant {
	zero, fifty, fourHundred := int64(0), int64(50), int64(400)
	return []Variant{
		{Name: "baseline"},
		{Name: "i4", NextRungReceiptStress: true},
		{Name: "probe0", SourcingCashBPS: &zero},
		{Name: "probe50", SourcingCashBPS: &fifty},
		{Name: "probe400", SourcingCashBPS: &fourHundred},
	}
}

type ContractDelivery struct {
	ContractID string    `json:"contract_id"`
	Quantity   int64     `json:"quantity"`
	At         time.Time `json:"at"`
}

func charterDeliveryReceipts(actions []store.Action, from time.Time) []ContractDelivery {
	var deliveries []ContractDelivery
	for _, action := range actions {
		if action.Path != "/v1/contract/deliver-charter" || action.Status < 200 || action.Status >= 300 || !action.UpdatedAt.After(from) {
			continue
		}
		var body struct {
			ContractID string `json:"contract_id"`
		}
		var receipt struct {
			ContractID string `json:"contract_id"`
			Quantity   int64  `json:"delivered_quantity"`
		}
		if json.Unmarshal(action.Body, &body) != nil || json.Unmarshal(action.Response, &receipt) != nil || receipt.Quantity <= 0 {
			continue
		}
		if receipt.ContractID == "" {
			receipt.ContractID = body.ContractID
		}
		if receipt.ContractID != "" && (body.ContractID == "" || body.ContractID == receipt.ContractID) {
			deliveries = append(deliveries, ContractDelivery{ContractID: receipt.ContractID, Quantity: receipt.Quantity, At: action.UpdatedAt})
		}
	}
	return deliveries
}

// EffectiveContracts removes the historical company's post-cutover deliveries
// already reflected in an observation, then applies only this world's deliveries.
// A receipt after the observation cannot replenish that older observation.
func EffectiveContracts(view FrameView, from time.Time, actions []store.Action, deliveries []ContractDelivery) []engine.Contract {
	return effectiveContracts(view, from, charterDeliveryReceipts(actions, from), deliveries)
}

func effectiveContracts(view FrameView, from time.Time, recorded, deliveries []ContractDelivery) []engine.Contract {
	observed := view.Meta["contracts"].UpdatedAt
	if observed.IsZero() || observed.After(view.At) {
		observed = view.At
	}
	addbacks := make(map[string]int64)
	for _, delivery := range recorded {
		if delivery.At.After(from) && !delivery.At.After(observed) {
			addbacks[delivery.ContractID] += delivery.Quantity
		}
	}
	consumed := make(map[string]int64)
	for _, delivery := range deliveries {
		if !delivery.At.Before(from) && !delivery.At.After(view.At) && delivery.Quantity > 0 {
			consumed[delivery.ContractID] += delivery.Quantity
		}
	}
	contracts := make([]engine.Contract, 0, len(view.State.Contracts))
	for _, contract := range view.State.Contracts {
		if contract.ContractType == "charter" {
			contract.RemainingQuantity = max(0, contract.RemainingQuantity+addbacks[contract.ID]-consumed[contract.ID])
			contract.QuantityDelivered = max(0, contract.QuantityDelivered-addbacks[contract.ID]+consumed[contract.ID])
			if contract.Status == "fulfilled" && contract.RemainingQuantity > 0 {
				contract.Status = "open"
			}
		}
		contracts = append(contracts, contract)
	}
	sort.SliceStable(contracts, func(i, j int) bool { return contracts[i].ID < contracts[j].ID })
	return contracts
}

type idleObservation struct {
	LastValidatedGameHour int64     `json:"last_validated_game_hour"`
	PortID                string    `json:"port_id"`
	EarnedGameHours       int64     `json:"earned_game_hours"`
	LastClockUTC          time.Time `json:"last_clock_utc"`
	Validated             bool      `json:"validated"`
}

func (world *variantWorld) planningInput(view FrameView, ship engine.ShipState) (engine.PlanningInput, error) {
	ships := world.fleet.Ships()
	visible := view.VisibleTo(ships)
	state := visible.State
	state.Ships = ships
	state.Vaults = vaultsFor(world.accounting.Balances())
	state.Contracts = effectiveContracts(view, world.options.From, world.recordedDeliveries, world.deliveries)
	// Freight is outside this model, including freight bundles and its ordinary
	// opportunity priority over relocation. No variant may accept sealed work.
	charters := make([]engine.Contract, 0, len(state.Contracts))
	for _, contract := range state.Contracts {
		if contract.ContractType != "freight" {
			charters = append(charters, contract)
		}
	}
	state.Contracts = charters
	state.Markets = make(map[string]engine.Market)
	meta := cloneMap(visible.Meta)
	const generation = "simulation"
	fresh := func(key string) {
		meta[key] = engine.DatasetMeta{Generation: generation, UpdatedAt: view.At, AttemptedAt: view.At, Complete: true}
	}
	// Fleet and vault state are authoritative inside a variant. The shared
	// world context is the last recorded context, never the historical fleet.
	for _, key := range []string{"ships", "vaults", "clock"} {
		fresh(key)
	}
	for _, key := range []string{"catalog", "zones", "contracts", "rates"} {
		if original, exists := meta[key]; exists && original.Complete && !original.Stale && original.Error == "" {
			fresh(key)
		}
	}
	for _, port := range sortedKeys(view.Books) {
		observed := view.Books[port]
		book, coverage := world.market.Book(port)
		if coverage == Dark || observed.Status == Dark {
			delete(meta, "books/"+port)
			continue
		}
		if visible.State.Markets[port].Visibility != "quay" {
			book.Visibility = "public"
		}
		state.Markets[port] = book
		fresh("books/" + port)
	}
	for _, port := range sortedKeys(state.Ports) {
		key := "ports/" + port
		original, exists := meta[key]
		if !exists {
			original, exists = meta["ports"]
		}
		if exists && original.Complete && !original.Stale && original.Error == "" &&
			!original.UpdatedAt.After(view.At) && view.At.Sub(original.UpdatedAt) <= PublicViewLimit {
			fresh(key)
		}
	}
	for _, key := range sortedKeys(state.Routes) {
		// TapeReader already discards routes from a previous phase.
		if original, exists := meta["routes/"+key]; exists && original.Complete && !original.Stale && original.Error == "" {
			fresh("routes/" + key)
		}
	}
	snapshot := cloneMap(visible.Snapshot)
	// Only this world's own fills enter the input below.
	delete(snapshot, "fills")
	idle := make(map[string]idleObservation, len(ships))
	for _, member := range ships {
		id := member.Ship.ID
		if member.Voyage == nil && member.CargoUsedTons == 0 && member.Ship.CurrentPortID != "" {
			idle[id] = idleObservation{
				LastValidatedGameHour: state.Clock.Calendar.TotalGameHours,
				PortID:                member.Ship.CurrentPortID, EarnedGameHours: int64(world.idleHours[id]),
				LastClockUTC: state.Clock.RealUTC, Validated: true,
			}
		}
	}
	var err error
	snapshot["_ship_idle"], err = json.Marshal(idle)
	if err != nil {
		return engine.PlanningInput{}, fmt.Errorf("encode simulated idle state: %w", err)
	}
	snapshot["_sourcing_probe_history"] = world.initialProbeHistory
	var competition *store.CompetitionSummary
	if raw := snapshot["_competition"]; len(raw) > 0 && string(raw) != "null" {
		var summary store.CompetitionSummary
		if err := json.Unmarshal(raw, &summary); err != nil {
			return engine.PlanningInput{}, fmt.Errorf("decode recorded competition: %w", err)
		}
		competition = &summary
	}
	// A slower recorded clock can enlarge the rolling window. Reload only
	// pre-tape evidence at this world's original cutover, never later receipts.
	if scale := state.Clock.Calendar.TimeScale; scale != world.sourcingScale {
		prefix, err := world.tape.sourcingSettlementsBefore(world.options.From, scale)
		if err != nil {
			return engine.PlanningInput{}, fmt.Errorf("load rolling sourcing prefix: %w", err)
		}
		if world.seedSettlementIDs == nil {
			world.seedSettlementIDs = make(map[string]bool, len(world.seedSettlements)+len(prefix))
			for _, row := range world.seedSettlements {
				world.seedSettlementIDs[row.ID] = true
			}
		}
		for _, row := range prefix {
			if !world.seedSettlementIDs[row.ID] {
				world.ledger.RecordSettlement(row)
				world.seedSettlements = append(world.seedSettlements, row)
				world.seedSettlementIDs[row.ID] = true
			}
		}
		world.sourcingScale = scale
	}
	proceeds, err := world.charterProceedsPence(view)
	if err != nil {
		return engine.PlanningInput{}, err
	}
	input := engine.PlanningInputFor(view.At, ship, state, snapshot, engine.PlanningSources{
		CompanyID: world.options.CompanyID, ReservePence: world.options.ReservePence,
		Metadata: meta, Generation: generation, CharterProceedsPence: proceeds, Competition: competition,
	})
	input.NextRungReceiptStress = world.variant.NextRungReceiptStress
	input.SourcingCashBPS = world.variant.SourcingCashBPS
	input.PlayerBidSellFills = world.sellFills
	if err := world.ledger.Apply(&input, snapshot); err != nil {
		return input, err
	}
	if lanes := engine.SourcingHistoryLanes(input, ships); len(lanes) > 0 {
		completions, err := world.tape.probeCompletionsBefore(lanes, world.options.From)
		if err != nil {
			return input, fmt.Errorf("load historical probe completions: %w", err)
		}
		for lane, completed := range completions {
			input.SourcingProbeLanes[lane] = true
			if completed.After(input.SourcingProbeCompletions[lane]) {
				input.SourcingProbeCompletions[lane] = completed
			}
		}
	}
	return input, nil
}

func (world *variantWorld) recordHazardGap(plan engine.Plan) bool {
	if !engine.PlanLacksHazardQuote(plan) {
		return false
	}
	world.result.HazardUnavailablePlans++
	if world.result.HazardUnavailablePlans == 1 {
		world.result.Omissions = append(world.result.Omissions, "UNTRUSTED: recorded Lloyd's quotes unavailable for evaluated simulated lanes")
	}
	world.result.HazardAvailable = false
	return true
}

func (world *variantWorld) bestOrdinary(input engine.PlanningInput) (engine.Plan, bool) {
	var best engine.Plan
	for _, plan := range engine.PlanVoyages(input) {
		world.recordHazardGap(plan)
		if !best.Accepted && plan.Accepted && len(plan.FreightIDs) == 0 && !strings.Contains(plan.Kind, "freight") {
			best = plan
		}
	}
	return best, best.Accepted
}

func (world *variantWorld) bestProbe(input engine.PlanningInput) (engine.Plan, bool) {
	var best engine.Plan
	for _, plan := range engine.PlanSourcingRelocation(input) {
		world.recordHazardGap(plan)
		if !best.Accepted && plan.Accepted {
			best = plan
		}
	}
	return best, best.Accepted
}

func vaultsFor(balances map[string]int64) []engine.Vault {
	regions := sortedKeys(balances)
	vaults := make([]engine.Vault, 0, len(regions))
	for _, region := range regions {
		vaults = append(vaults, engine.Vault{BankingRegion: region, AccountType: "vault", BalancePence: balances[region]})
	}
	return vaults
}
