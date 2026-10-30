package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
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
	FundingFeesPence            int64
	MaxPaybackDays              float64
}

type GrowthDecision struct {
	Accepted                      bool                   `json:"accepted"`
	EconomicallyViable            bool                   `json:"economically_viable"`
	CashShortfallPence            int64                  `json:"cash_shortfall_pence"`
	Reason                        string                 `json:"reason"`
	QuotedHullPence               int64                  `json:"quoted_hull_pence"`
	SlippageReservePence          int64                  `json:"slippage_reserve_pence"`
	CannonsPence                  int64                  `json:"cannons_pence"`
	InitialStoresPence            int64                  `json:"initial_stores_pence"`
	WorkingCapitalPence           int64                  `json:"working_capital_pence"`
	FundingFeesPence              int64                  `json:"funding_fees_pence"`
	IncrementalUpkeepPence        int64                  `json:"incremental_upkeep_pence"`
	UpkeepReservePence            int64                  `json:"upkeep_reserve_pence"`
	IncrementalNetPencePerGameDay float64                `json:"incremental_net_pence_per_game_day"`
	DepthNetPencePerGameDay       float64                `json:"depth_net_pence_per_game_day"`
	PaybackGameDays               float64                `json:"payback_game_days"`
	EvidenceBasis                 string                 `json:"evidence_basis"`
	Competition                   CompetitionEvidence    `json:"competition"`
	DepthCompetition              CompetitionEvidence    `json:"depth_competition"`
	Mortgage                      *gameapi.MortgageTerms `json:"mortgage,omitempty"`
	competitionComparable         bool
	depthCompetitionComparable    bool
}

func growthCapital(decision GrowthDecision) int64 {
	price := decision.QuotedHullPence
	if decision.Mortgage != nil {
		if decision.Mortgage.PrincipalPence <= 0 || decision.Mortgage.PrincipalPence > price {
			return math.MaxInt64
		}
		price -= decision.Mortgage.PrincipalPence
	}
	return growthCashTotal(price, decision.SlippageReservePence, decision.CannonsPence, decision.InitialStoresPence)
}

func growthWorkingCapital(plan Plan) int64 {
	stores := plan.Economics.StoreCashPence
	if plan.Economics.Basis != "destination" {
		stores = plan.Economics.ProvisionsPence
	}
	purchase, customs := plan.Economics.PurchasePence, plan.Economics.CustomsPence
	if plan.Quantity > 0 && plan.BuyLimitPence > 0 {
		if plan.BuyLimitPence > math.MaxInt64/plan.Quantity {
			return math.MaxInt64
		}
		worstPurchase := max(purchase, plan.Quantity*plan.BuyLimitPence)
		if worstPurchase > purchase && customs > 0 {
			if purchase <= 0 {
				return math.MaxInt64
			}
			// Existing per-fill rounding makes this effective customs ratio
			// conservative at the immutable IOC limit. Keep it integer exact.
			high, low := bits.Mul64(uint64(customs), uint64(worstPurchase))
			if high >= uint64(purchase) {
				return math.MaxInt64
			}
			quotient, remainder := bits.Div64(high, low, uint64(purchase))
			if quotient >= math.MaxInt64 {
				return math.MaxInt64
			}
			customs = int64(quotient)
			if remainder != 0 {
				customs++
			}
		}
		purchase = worstPurchase
	}
	return growthCashTotal(purchase, customs, plan.Economics.CollateralPence,
		plan.Economics.BerthDuesPence, stores, plan.Economics.TollPence)
}

func growthCashTotal(costs ...int64) int64 {
	var total int64
	for _, cost := range costs {
		if cost < 0 || cost > math.MaxInt64-total {
			return math.MaxInt64
		}
		total += cost
	}
	return total
}

type growthProof struct {
	positiveCycles  int64
	quantity        int64
	positiveSeconds int64
	seconds         int64
	net             int64
	positiveNet     int64
	unknown         bool
	future          bool
	newLane         bool
}

// EvaluateGrowth keeps the pure proof-slice seam; production supplies exact
// SQL aggregates from the recorded 12-real-hour completed-settlement cohort.
func EvaluateGrowth(in GrowthInput) GrowthDecision {
	var proof growthProof
	for _, row := range in.Proofs {
		if !row.CostsKnown || row.GameSeconds <= 0 || row.QuantityTons < 0 ||
			row.GameSeconds > math.MaxInt64-proof.seconds ||
			row.NetPence > 0 && proof.net > math.MaxInt64-row.NetPence ||
			row.NetPence < 0 && proof.net < math.MinInt64-row.NetPence {
			proof.unknown = true
			break
		}
		proof.seconds += row.GameSeconds
		proof.net += row.NetPence
		if row.NetPence <= 0 || row.QuantityTons == 0 {
			continue
		}
		if row.QuantityTons > math.MaxInt64-proof.quantity ||
			row.GameSeconds > math.MaxInt64-proof.positiveSeconds ||
			row.NetPence > math.MaxInt64-proof.positiveNet {
			proof.unknown = true
			break
		}
		proof.positiveCycles++
		proof.quantity += row.QuantityTons
		proof.positiveSeconds += row.GameSeconds
		proof.positiveNet += row.NetPence
	}
	return evaluateGrowth(in, proof)
}

func evaluateGrowth(in GrowthInput, proof growthProof) GrowthDecision {
	decision := GrowthDecision{Reason: "growth requires a fresh available turnkey quote and an accepted executable lane"}
	if !in.Plan.Accepted || !in.Quote.Available || in.Quote.BuildMode != "turnkey_cash" ||
		in.Quote.TotalCostPence == nil || *in.Quote.TotalCostPence <= 0 ||
		*in.Quote.TotalCostPence > maximumPence || len(in.Plan.FreightIDs) > 0 {
		return decision
	}
	if proof.future {
		decision.Reason = "growth proof contains a completion after the recorded host UTC evaluation time"
		return decision
	}
	if proof.unknown {
		decision.Reason = "the recorded 12-real-hour completed-settlement cohort has unknown costs or unmeasured duration"
		return decision
	}
	if proof.positiveCycles < 2 || proof.quantity <= 0 || proof.seconds <= 0 || proof.positiveSeconds <= 0 {
		decision.Reason = "growth needs two positive delivered cycles in the recorded 12-real-hour cohort with known integer cost basis"
		return decision
	}
	if proof.net <= 0 {
		decision.Reason = "recorded 12-real-hour realized net including failed procurement, probes and loss-making voyages is not positive"
		return decision
	}
	sourceCycle := growthCashTotal(in.ExistingCycleTons, in.OtherSourceCycleTons, in.Plan.Quantity)
	sourceRequired := growthCashTotal(sourceCycle, sourceCycle, in.UnboughtSourceTons)
	if in.Plan.Quantity <= 0 || sourceRequired == math.MaxInt64 || in.ObservedAskCapacityTons < sourceRequired {
		decision.Reason = "measured source depth cannot fund two cycles after competing source demand"
		return decision
	}
	cycleDays := in.Plan.Economics.GameDays
	forecastRate := in.Plan.Economics.NetPencePerGameDay
	if in.Plan.Economics.Basis == "destination" {
		cycleDays = in.Plan.Economics.BallastRepeatGameDays
		if cycleDays > 0 {
			forecastRate = float64(in.Plan.Economics.BallastRepeatNetPence) / cycleDays
		}
	}
	if cycleDays <= 0 || math.IsNaN(cycleDays) || math.IsInf(cycleDays, 0) ||
		forecastRate <= 0 || math.IsNaN(forecastRate) || math.IsInf(forecastRate, 0) {
		decision.Reason = "growth requires a positive candidate-specific forecast with a funded safe empty return"
		return decision
	}
	// Scale only the live candidate's supported throughput. Losses reduce both
	// the realized unit margin and productive-time fraction; a larger or faster
	// candidate is no longer capped at the incumbent's absolute earning rate.
	utilization := float64(proof.positiveSeconds) / float64(proof.seconds)
	scaledRate := float64(proof.net) / float64(proof.quantity) * float64(in.Plan.Quantity) / cycleDays * utilization
	basis := "recorded 12-real-hour loss-inclusive lane margin capped by live candidate ballast-repeat forecast"
	if proof.newLane {
		// General execution is not a price or rate for an untraveled lane.
		// Its own measured depth and terms provide the forecast; historical
		// loss retention can only reduce, never enhance, that forecast.
		scaledRate = forecastRate * min(1, float64(proof.net)/float64(proof.positiveNet)) * utilization
		basis = "new-lane live candidate forecast discounted by recorded 12-real-hour loss-inclusive execution evidence"
	}
	netRate := min(scaledRate, forecastRate) - float64(in.IncrementalDailyUpkeepPence)
	if netRate <= 0 {
		decision.Reason = "nonlinear marginal fleet upkeep eliminates conservative incremental profit"
		return decision
	}
	guns := min(int64(4), in.Hull.MaxCannons)
	if guns < 1 || in.Hull.HoldCapacityTons <= 2*guns ||
		in.Plan.Quantity > in.Hull.HoldCapacityTons-2*guns ||
		in.Plan.TargetStoresTons < 0 || math.IsNaN(in.Plan.TargetStoresTons) || math.IsInf(in.Plan.TargetStoresTons, 0) ||
		float64(in.Hull.HoldCapacityTons-2*guns-in.Plan.Quantity) < max(1, math.Ceil(in.Plan.TargetStoresTons)) {
		decision.Reason = "new hull cannot carry adequate cannons, safe stores and the profitable lot"
		return decision
	}
	hullPrice := *in.Quote.TotalCostPence
	slippage := ceilBPS(hullPrice, 1_000)
	gunPrice := guns * 15_000
	stores := in.Plan.Economics.StoreCashPence
	if in.Plan.Economics.Basis != "destination" {
		stores = in.Plan.Economics.ProvisionsPence
	}
	if in.InitialStoresPence < stores || in.InitialStoresPence < 0 || in.FundingFeesPence < 0 {
		decision.Reason = "growth initial stores or measured funding fees lack a complete cash budget"
		return decision
	}
	// Initial store cash is committed once, as commissioning capital. The
	// recoverable cargo/collateral cycle must not purchase the same stores again.
	cycleCash := growthWorkingCapital(in.Plan)
	upkeepDays := math.Ceil(max(3, cycleDays))
	if cycleCash == math.MaxInt64 || in.IncrementalDailyUpkeepPence < 0 ||
		upkeepDays >= float64(math.MaxInt64) ||
		in.IncrementalDailyUpkeepPence > math.MaxInt64/int64(upkeepDays) {
		decision.Reason = "growth cash commitments exceed integer-pence bounds"
		return decision
	}
	upkeepReserve := int64(upkeepDays) * in.IncrementalDailyUpkeepPence
	working := cycleCash - stores
	capital := growthCashTotal(hullPrice, slippage, gunPrice, in.InitialStoresPence, in.FundingFeesPence)
	if capital == math.MaxInt64 {
		decision.Reason = "growth capital exceeds integer-pence bounds"
		return decision
	}
	payback := float64(capital) / netRate
	decision = GrowthDecision{
		QuotedHullPence:               hullPrice,
		SlippageReservePence:          slippage,
		CannonsPence:                  gunPrice,
		InitialStoresPence:            in.InitialStoresPence,
		WorkingCapitalPence:           working,
		FundingFeesPence:              in.FundingFeesPence,
		IncrementalUpkeepPence:        in.IncrementalDailyUpkeepPence,
		UpkeepReservePence:            upkeepReserve,
		IncrementalNetPencePerGameDay: netRate,
		PaybackGameDays:               payback,
		EvidenceBasis:                 basis,
	}
	if in.MaxPaybackDays <= 0 || math.IsNaN(in.MaxPaybackDays) || math.IsInf(in.MaxPaybackDays, 0) || payback > in.MaxPaybackDays {
		decision.Reason = "turnkey hull, cannon, store and measured funding capital exceeds maximum game-day payback"
		return decision
	}
	cycleCount := math.Ceil(payback/cycleDays) + 1
	cycleDemand := growthCashTotal(in.ExistingCycleTons, in.OtherContractCycleTons, in.Plan.Quantity)
	if cycleCount >= float64(math.MaxInt64) || cycleDemand == math.MaxInt64 ||
		in.OutstandingContractTons < 0 || max(2, int64(cycleCount)) > (math.MaxInt64-in.OutstandingContractTons)/cycleDemand {
		decision.Reason = "growth payback demand exceeds measured finite capacity bounds"
		return decision
	}
	cycles := max(int64(2), int64(cycleCount))
	paybackTons := cycles*cycleDemand + in.OutstandingContractTons
	if in.ContractRemainingTons >= 0 && in.ContractRemainingTons < paybackTons {
		decision.Reason = "finite charter capacity does not cover competing contract demand through capital payback plus a cycle"
		return decision
	}
	destinationCycle := growthCashTotal(in.ExistingCycleTons, in.OtherDestinationCycleTons, in.Plan.Quantity)
	destinationRequired := growthCashTotal(destinationCycle, destinationCycle, in.OutstandingDestinationTons)
	if in.Plan.Kind == "spot" && (destinationRequired == math.MaxInt64 || in.ObservedBidCapacityTons < destinationRequired) {
		decision.Reason = "spot growth needs two cycles of measured spare destination bids; gossip has no executable capacity"
		return decision
	}
	if in.ContractGameDaysRemaining != nil && *in.ContractGameDaysRemaining < payback+cycleDays {
		decision.Reason = "charter expires before capital payback plus a delivery cycle"
		return decision
	}
	required := growthCashTotal(capital, working, in.ExistingWorkingCapitalPence, upkeepReserve, in.ReservePence)
	if required == math.MaxInt64 || in.BalancePence < required {
		decision.Reason = "growth cannot preserve existing working capital, one new cycle, upkeep horizon and regional reserve"
		return decision
	}
	decision.Accepted = true
	decision.Reason = "two positive delivered cycles and loss-inclusive net in the recorded 12-real-hour cohort, live spare demand, funded repeat stores, marginal upkeep and capital payback pass; 10% quote slippage remains reserved"
	return decision
}

type growthCommitment struct {
	plan        Plan
	unbought    int64
	outstanding int64
}

type growthEvidence struct {
	incumbents      []growthCommitment
	workingByRegion map[string]int64
	required        map[string]int64
	balances        map[string]int64
	asOf            time.Time
	history         store.GrowthHistory
	lanes           map[string]store.GrowthHistory
	markets         map[string]Market
	home            Port
	available       int64
}

func (r *Runner) reserveGrowthMission(evidence *growthEvidence, mission *store.Mission, data *missionData) bool {
	if data.Relocation != nil || mission.Phase == "maintenance_done" {
		return false
	}
	if data.Plan.Kind == "recovery" {
		return true
	}
	owner, _ := r.ship(mission.ShipID)
	if mission.ShipID == "__growth" {
		owner, _ = r.ship(data.BuiltShipID)
	}
	if r.loadingReservationSuspended(mission, data, owner) {
		return false
	}
	if evidence.workingByRegion != nil {
		r.reserveMissionCash(evidence.workingByRegion, mission, data, owner)
	}
	if data.Plan.Kind == "maintenance" && data.Plan.StoresToBuy == 0 {
		return false
	}
	if data.Settlement == nil && mission.Phase != "settling" && mission.Phase != "settled" {
		deliveredTrade := data.DeliveredQuantity
		for _, obligation := range data.Freight {
			if obligation.Settled {
				deliveredTrade = max(0, deliveredTrade-obligation.Quantity)
			}
		}
		unbought := int64(0)
		if !data.BuyDone && data.Plan.Kind != "freight" {
			unbought = max(0, data.Plan.Quantity-data.BoughtQuantity)
		}
		plan := data.Plan
		if ship, exists := r.ship(mission.ShipID); exists && ship.Ship.CurrentPortID == plan.Origin && ship.Voyage == nil {
			plan.StoresToBuy = max(0, int64(math.Ceil(plan.TargetStoresTons-ship.Stores.ProvisionsTons)))
			if data.Plan.Kind == "maintenance" {
				plan.StoresToBuy = min(plan.StoresToBuy, data.Plan.StoresToBuy)
				if data.Action != nil && data.Action.Stage == "provision" {
					plan.StoresToBuy = data.Plan.StoresToBuy
				}
			}
		} else if mission.ShipID != "__growth" {
			plan.StoresToBuy = 0
		}
		outstanding := max(0, data.Plan.Quantity-deliveredTrade)
		if unbought > 0 || outstanding > 0 || data.GrowthLane == nil || data.GrowthLane.NextPlan == nil {
			evidence.incumbents = append(evidence.incumbents, growthCommitment{
				plan: plan, unbought: unbought, outstanding: outstanding,
			})
		}
	}
	if data.GrowthLane != nil && data.GrowthLane.NextPlan != nil {
		plan := *data.GrowthLane.NextPlan
		evidence.incumbents = append(evidence.incumbents, growthCommitment{
			plan: plan, unbought: plan.Quantity, outstanding: plan.Quantity,
		})
		return true
	}
	return data.Plan.Kind != "maintenance" && mission.Phase != "settling" && mission.Phase != "settled"
}

func (r *Runner) growthCashFunding(ctx context.Context, evidence *growthEvidence) (string, error) {
	for _, vault := range r.stateCache.Vaults {
		if vault.AccountType == "vault" && vault.BalancePence < 0 {
			return "growth deferred while any regional vault is negative; existing fleet may continue earning", nil
		}
	}
	required, err := r.treasuryReserves(ctx)
	if err != nil {
		return "", err
	}
	for region, working := range evidence.workingByRegion {
		required[region] = growthCashTotal(max(required[region], r.opts.ReservePence), working)
	}
	balances := r.planningInput(ShipState{}).Balances
	for region, protected := range required {
		if balances[region] < protected {
			return fmt.Sprintf("growth deferred: incumbent working capital or protected regional bills are underfunded in %s; existing fleet may continue earning", region), nil
		}
	}
	home, exists := findPort(r.stateCache.Catalog, r.stateCache.HomePortID)
	if !exists {
		return "growth requires the authoritative home billing region", nil
	}
	evidence.asOf = time.Now().UTC()
	evidence.required = required
	evidence.balances = balances
	evidence.home = home
	evidence.lanes = make(map[string]store.GrowthHistory)
	evidence.markets = withoutOwnOrders(r.stateCache.Markets, r.ownedOrders())
	evidence.available = 0
	for region, balance := range balances {
		evidence.available = growthCashTotal(evidence.available, max(0, balance-required[region]))
	}
	return "", nil
}

func (r *Runner) growthLaneProof(ctx context.Context, evidence *growthEvidence) (string, error) {
	history, err := r.db.GrowthHistory(ctx, r.account.ID, "", evidence.asOf)
	if err != nil {
		return "", err
	}
	if history.FutureCompletion {
		return "growth proof contains a completion after the recorded host UTC evaluation time", nil
	}
	if history.PositiveCycles < 2 {
		return "no two positive delivered cycles in the recorded 12-real-hour cohort yet; chart balances are not proof of profit", nil
	}
	if history.UnknownCosts || history.UnmeasuredDuration || history.NetPence <= 0 {
		return "growth requires positive recorded 12-real-hour realized net after every in-cohort loss and probe, with no unknown costs or cycle duration", nil
	}
	evidence.history = history
	return "", nil
}

func (r *Runner) growthFunding(ctx context.Context, evidence *growthEvidence) (string, error) {
	if reason, err := r.growthCashFunding(ctx, evidence); err != nil || reason != "" {
		return reason, err
	}
	return r.growthLaneProof(ctx, evidence)
}

func growthCandidate(port Port, hull Hull) ShipState {
	candidate := ShipState{}
	candidate.Ship.ID = "growth-candidate"
	candidate.Ship.Status = "berthed"
	candidate.Ship.CurrentPortID = port.ID
	candidate.Ship.ShipClass = hull.ID
	candidate.Ship.CannonsCount = min(4, hull.MaxCannons)
	candidate.HoldCapacityTons = hull.HoldCapacityTons
	candidate.EffectiveHoldCapacityTons = hull.HoldCapacityTons - 2*candidate.Ship.CannonsCount
	return candidate
}

func (r *Runner) growthQuote(port Port, hull Hull) (YardQuote, error) {
	key := port.ID + "|" + hull.ID
	if until := decode[map[string]time.Time](r.snapshot, "_growth_build_backoffs")[port.ID+"|"+hull.ID]; time.Now().Before(until) {
		r.wakeBy(until)
		return YardQuote{}, fmt.Errorf("growth build for this yard and hull backs off until %s after a definitive refusal", until.UTC().Format(time.RFC3339))
	}
	quote := r.stateCache.Quotes[key]
	if !port.HasShipyard || hull.CanSealDispatches || port.Phase > r.stateCache.Clock.UnlockedPhase || hull.Phase > r.stateCache.Clock.UnlockedPhase ||
		!r.fresh("books/"+port.ID) || r.stateCache.Markets[port.ID].Visibility != "quay" || !r.fresh("yard_quotes/"+key) ||
		quote.PortID != port.ID || quote.ShipClass != hull.ID || quote.BuildMode != "turnkey_cash" ||
		!quote.Available || quote.TotalCostPence == nil || *quote.TotalCostPence <= 0 {
		return YardQuote{}, errors.New("growth requires its matching fresh available turnkey quote and executable local quay")
	}
	if r.fresh("ships") {
		for _, ship := range r.stateCache.Ships {
			if ship.Ship.CurrentPortID == port.ID && ship.Voyage == nil &&
				(ship.Ship.Status == "berthed" || ship.Ship.Status == "anchored_roadstead") {
				return quote, nil
			}
		}
	}
	if r.fresh("factors") {
		for _, factorPort := range r.stateCache.Factors {
			if factorPort == port.ID {
				return quote, nil
			}
		}
	}
	return YardQuote{}, errors.New("growth requires fresh stationary ship or factor presence at the quoted yard")
}

func (r *Runner) evaluateGrowthCandidate(ctx context.Context, port Port, hull Hull, plan *Plan, quoted PlanningInput, quote YardQuote, deltaUpkeep int64, evidence *growthEvidence) (GrowthDecision, map[string]int64, error) {
	lane := laneKey(*plan)
	history, exists := evidence.lanes[lane]
	if !exists {
		var err error
		history, err = r.db.GrowthHistory(ctx, r.account.ID, lane, evidence.asOf)
		if err != nil {
			return GrowthDecision{}, nil, err
		}
		evidence.lanes[lane] = history
	}
	newLane := history.PositiveCycles < 2
	unknownLane := history.UnknownCosts || history.UnmeasuredDuration
	futureLane := history.FutureCompletion
	if newLane {
		history = evidence.history
	}
	proof := growthProof{
		positiveCycles:  history.PositiveCycles,
		quantity:        history.PositiveQuantityTons,
		positiveSeconds: history.PositiveGameSeconds,
		seconds:         history.GameSeconds,
		net:             history.NetPence,
		positiveNet:     history.PositiveNetPence,
		unknown:         history.UnknownCosts || history.UnmeasuredDuration || unknownLane,
		future:          history.FutureCompletion || futureLane,
		newLane:         newLane,
	}
	book := evidence.markets[port.ID]
	cycleDays := plan.Economics.BallastRepeatGameDays
	storeDemand := 2 * plan.StoresToBuy
	var existingTons, otherSourceTons, otherDestinationTons, otherContractTons int64
	var outstandingDestination, outstandingContract, unboughtSource int64
	for _, incumbent := range evidence.incumbents {
		sameSource := incumbent.plan.Origin == plan.Origin && incumbent.plan.Commodity == plan.Commodity
		kind, _, _ := strings.Cut(incumbent.plan.Kind, "+")
		incumbentDays := incumbent.plan.Economics.GameDays
		if incumbent.plan.Economics.Basis == "destination" && incumbent.plan.Economics.BallastRepeatGameDays > 0 {
			incumbentDays = incumbent.plan.Economics.BallastRepeatGameDays
		}
		if incumbent.plan.Origin == plan.Origin && incumbent.plan.StoresToBuy > 0 {
			if incumbentDays <= 0 || cycleDays <= 0 {
				return GrowthDecision{Reason: "growth cannot allocate shared safe-store demand without a measured cycle duration"}, nil, nil
			}
			storeDemand += int64(math.Ceil(2 * float64(incumbent.plan.StoresToBuy) * cycleDays / incumbentDays))
		}
		if kind != "spot" && kind != "charter" {
			continue
		}
		sameDestination := kind == "spot" && incumbent.plan.Destination == plan.Destination && incumbent.plan.Commodity == plan.Commodity
		sameContract := plan.ContractID != "" && incumbent.plan.ContractID == plan.ContractID
		if incumbent.plan.Quantity <= 0 || (!sameSource && !sameDestination && !sameContract) {
			continue
		}
		if incumbentDays <= 0 || cycleDays <= 0 {
			return GrowthDecision{Reason: "growth cannot allocate competing committed throughput without its full cycle duration"}, nil, nil
		}
		tons := int64(math.Ceil(float64(incumbent.plan.Quantity) * cycleDays / incumbentDays))
		if laneKey(incumbent.plan) == laneKey(*plan) && incumbent.plan.ContractID == plan.ContractID {
			// The undelivered lot lands once before repeat demand scales.
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
	// Price the marginal supported lot, not a cheap first lot repeatedly.
	// Incumbent repeat demand and outstanding lots reserve depth once in their
	// respective pools; this candidate's first lot precedes its second quote.
	reservedSource := growthCashTotal(existingTons, existingTons, otherSourceTons, otherSourceTons, unboughtSource, plan.Quantity)
	reservedBids := growthCashTotal(existingTons, existingTons, otherDestinationTons, otherDestinationTons, outstandingDestination, plan.Quantity)
	originBook := reserveGrowthBook(book, plan.Commodity, reservedSource, 0, max(0, storeDemand-plan.StoresToBuy))
	reservedDestination := int64(0)
	if plan.Kind == "spot" {
		reservedDestination = reservedBids
	}
	destinationBook := reserveGrowthBook(evidence.markets[plan.Destination], plan.Commodity, 0, reservedDestination, 0)
	quoted.Markets = map[string]Market{port.ID: originBook, plan.Destination: destinationBook}
	revenue := int64(0)
	if plan.Economics.UncalibratedRevenuePence != nil {
		revenue = *plan.Economics.UncalibratedRevenuePence
	}
	*plan = pricePlan(quoted, hull, port, *plan, revenue, 0)
	if !plan.Accepted {
		return GrowthDecision{Reason: "marginal repeat lot: " + plan.Reason}, nil, nil
	}
	entry := bookCommodity(book, plan.Commodity)
	var depthTons int64
	if entry != nil && entry.Depth != nil {
		depthTons = depthCapacity(entry.Depth.Asks, plan.BuyLimitPence, true)
	}
	provisions := bookCommodity(book, "provisions")
	if provisions == nil || provisions.Depth == nil {
		return GrowthDecision{Reason: "growth lacks executable depth for shared safe stores"}, nil, nil
	}
	if plan.Commodity == "provisions" {
		depthTons = max(0, depthTons-storeDemand)
	} else if _, err := WalkDepth(provisions.Depth.Asks, storeDemand, true); err != nil {
		return GrowthDecision{Reason: "growth safe-store depth is already consumed by competing store demand"}, nil, nil
	}
	var bidTons int64
	if plan.Kind == "spot" && r.fresh("books/"+plan.Destination) {
		demand := bookCommodity(evidence.markets[plan.Destination], plan.Commodity)
		if demand != nil && demand.Depth != nil {
			bidTons = depthCapacity(demand.Depth.Bids, plan.SellLimitPence, false)
		}
	}
	remaining := int64(-1)
	var daysRemaining *float64
	if contract, exists := r.contract(plan.ContractID); exists {
		remaining = contract.RemainingQuantity
		if contract.ExpiresAt != nil {
			_, upper, ok := r.serverBounds()
			if !ok {
				return GrowthDecision{Reason: "growth requires current clock authority for charter payback"}, nil, nil
			}
			days := contract.ExpiresAt.Sub(upper).Seconds() * float64(r.stateCache.Clock.Calendar.TimeScale) / 86_400
			daysRemaining = &days
		}
	}
	input := GrowthInput{
		Plan:                        *plan,
		Quote:                       quote,
		Hull:                        hull,
		BalancePence:                evidence.available,
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
		InitialStoresPence:          plan.Economics.StoreCashPence,
		MaxPaybackDays:              r.opts.MaxPaybackDays,
	}
	decision := evaluateGrowth(input, proof)
	decision.Competition = quoted.destinationCompetition(plan.Destination)
	if !decision.Accepted {
		return decision, nil, nil
	}
	required := clonePence(evidence.required)
	required[port.BankingRegion] = max(required[port.BankingRegion], r.opts.ReservePence)
	required[port.BankingRegion] = growthCashTotal(required[port.BankingRegion], growthCapital(decision), decision.WorkingCapitalPence)
	required[evidence.home.BankingRegion] += decision.UpkeepReservePence
	if destination, exists := findPort(r.stateCache.Catalog, plan.Destination); exists {
		required[destination.BankingRegion] = max(required[destination.BankingRegion],
			r.opts.ReservePence+3*r.stateCache.Ports[destination.ID].BerthDuesPerDayPence)
		if destination.BankingRegion != port.BankingRegion {
			required[destination.BankingRegion] = growthCashTotal(required[destination.BankingRegion],
				r.stateCache.Ports[destination.ID].BerthDuesPerDayPence)
		}
	}
	_, fees, transfers, funded := measuredFunding(evidence.balances, required, r.stateCache.Rates)
	if !funded {
		decision.Accepted = false
		decision.Reason = "growth cannot fund the candidate regions without spending protected obligations and measured remittance fees"
		return decision, nil, nil
	}
	if fees > 0 {
		input.FundingFeesPence = fees
		decision = evaluateGrowth(input, proof)
		decision.Competition = quoted.destinationCompetition(plan.Destination)
		if !decision.Accepted {
			return decision, nil, nil
		}
	}
	if len(transfers) > 0 {
		decision.Reason += "; regional funding must complete before build"
	}
	return decision, required, nil
}

type growthPackage struct {
	port           Port
	hull           Hull
	plan           Plan
	decision       GrowthDecision
	required       map[string]int64
	committedPence int64
}

func betterGrowthPackage(candidate, other growthPackage) bool {
	candidateReturn := candidate.decision.IncrementalNetPencePerGameDay / float64(candidate.committedPence)
	otherReturn := other.decision.IncrementalNetPencePerGameDay / float64(other.committedPence)
	if candidateReturn != otherReturn {
		return candidateReturn > otherReturn
	}
	if candidate.decision.IncrementalNetPencePerGameDay != other.decision.IncrementalNetPencePerGameDay {
		return candidate.decision.IncrementalNetPencePerGameDay > other.decision.IncrementalNetPencePerGameDay
	}
	if candidate.decision.Competition.PreferenceShips != other.decision.Competition.PreferenceShips {
		return candidate.decision.Competition.PreferenceShips < other.decision.Competition.PreferenceShips
	}
	if candidate.port.ID != other.port.ID {
		return candidate.port.ID < other.port.ID
	}
	if candidate.hull.ID != other.hull.ID {
		return candidate.hull.ID < other.hull.ID
	}
	return betterPlan(candidate.plan, other.plan)
}

// growthAtYard compares each whole-ton package on capital return. Voyage
// contribution alone cannot choose the best size once cargo cash is tied up.
func (r *Runner) growthAtYard(ctx context.Context, port Port, hull Hull, quote YardQuote, deltaUpkeep int64, evidence *growthEvidence, pinned *Plan) (growthPackage, bool, error) {
	var best growthPackage
	found := false
	input := r.planningInput(growthCandidate(port, hull))
	input.Markets = evidence.markets
	input.FleetDailyUpkeepPence = 0
	for _, dataset := range []string{"catalog", "clock", "zones", "vaults", "contracts", "ships", "rates"} {
		if !input.fresh(dataset) {
			return best, false, nil
		}
	}
	if input.Clock.Paused || input.Clock.Calendar.TimeScale <= 0 ||
		input.Clock.Calendar.Month < 1 || input.Clock.Calendar.Month > 12 ||
		input.Clock.Calendar.Day < 1 || input.Clock.Calendar.Day > 30 {
		return best, false, nil
	}
	consider := func(base Plan, maximum, rewardPerTon int64) error {
		if pinned != nil && (laneKey(base) != laneKey(*pinned) || base.ContractID != pinned.ContractID) {
			return nil
		}
		quoted := input
		quoted.ReserveByRegion = clonePence(evidence.required)
		quoted.ReserveByRegion[port.BankingRegion] = max(quoted.ReserveByRegion[port.BankingRegion], r.opts.ReservePence)
		destination, exists := findPort(input.Catalog, base.Destination)
		if !exists {
			return nil
		}
		quoted.ReserveByRegion[destination.BankingRegion] = max(quoted.ReserveByRegion[destination.BankingRegion],
			r.opts.ReservePence+3*input.Ports[destination.ID].BerthDuesPerDayPence)
		target := clonePence(quoted.ReserveByRegion)
		if destination.BankingRegion != port.BankingRegion {
			// The delivery bill is payable cash above the protected reserve.
			// Do not sweep it to the yard while finding maximum source funds.
			target[destination.BankingRegion] = growthCashTotal(target[destination.BankingRegion],
				input.Ports[destination.ID].BerthDuesPerDayPence)
		}
		balances, _, _, funded := measuredFunding(evidence.balances, target, input.Rates)
		if !funded {
			return nil
		}
		// Push only affordable observed-fee surplus toward this yard for the
		// sizing bound. Final packages reprice the exact required transfers.
		target[port.BankingRegion] += evidence.available
		quoted.Balances, _, _, _ = measuredFunding(balances, target, input.Rates)
		first := int64(1)
		maximum = min(maximum, quoted.Ship.EffectiveHoldCapacityTons)
		if pinned != nil {
			first = pinned.Quantity
			maximum = min(maximum, pinned.Quantity)
		}
		for quantity := first; quantity <= maximum; quantity++ {
			if quantity <= 0 || rewardPerTon > 0 && quantity > math.MaxInt64/rewardPerTon {
				continue
			}
			plan := base
			plan.Quantity = quantity
			stores, err := ballastRepeatStoreTarget(quoted, hull, plan)
			if err != nil {
				continue
			}
			plan.TargetStoresTons = stores
			plan = pricePlan(quoted, hull, port, plan, quantity*rewardPerTon, 0)
			if !plan.Accepted || plan.Economics.BallastRepeatGameDays <= 0 || plan.Economics.BallastRepeatNetPence <= 0 {
				continue
			}
			decision, required, err := r.evaluateGrowthCandidate(ctx, port, hull, &plan, quoted, quote, deltaUpkeep, evidence)
			if err != nil {
				return err
			}
			if !decision.Accepted {
				continue
			}
			committed := growthCashTotal(decision.FundingFeesPence)
			for region, protected := range required {
				committed = growthCashTotal(committed, max(0, protected-evidence.required[region]))
			}
			if committed <= 0 || committed == math.MaxInt64 {
				continue
			}
			candidate := growthPackage{
				port: port, hull: hull, plan: plan, decision: decision,
				required: required, committedPence: committed,
			}
			if !found || betterGrowthPackage(candidate, best) {
				best, found = candidate, true
			}
		}
		return nil
	}
	for _, contract := range input.Contracts {
		if contract.ContractType != "charter" || contract.OriginPortID == port.ID ||
			contract.Status != "open" || contract.Stale || contract.RemainingQuantity <= 0 ||
			contract.RewardPerTonPence == nil || *contract.RewardPerTonPence <= 0 {
			continue
		}
		plan := Plan{Kind: "charter", ShipID: input.Ship.Ship.ID, Origin: port.ID,
			Destination: contract.OriginPortID, Commodity: contract.CommodityID,
			ContractID: contract.ID, Deadline: contract.ExpiresAt}
		if err := consider(plan, contract.RemainingQuantity, *contract.RewardPerTonPence); err != nil {
			return best, found, err
		}
	}
	for _, destination := range input.Catalog.Ports {
		if destination.ID == port.ID || destination.Phase > input.Clock.UnlockedPhase {
			continue
		}
		for _, commodity := range input.Markets[port.ID].Books {
			if commodity.Depth == nil || len(commodity.Depth.Asks) == 0 || !portTrades(destination, commodity.CommodityID) {
				continue
			}
			plan := Plan{Kind: "spot", ShipID: input.Ship.Ship.ID, Origin: port.ID,
				Destination: destination.ID, Commodity: commodity.CommodityID}
			if err := consider(plan, input.Ship.EffectiveHoldCapacityTons, 0); err != nil {
				return best, found, err
			}
		}
	}
	return best, found, nil
}

func (r *Runner) growthBuildReady(ctx context.Context, data *missionData) error {
	if !cheapCapacityHull(Hull{ID: data.BuildClass}) {
		return errors.New("new build requires a cheap reinvestment hull")
	}
	for _, dataset := range []string{"vaults", "ships", "catalog", "clock", "zones", "rates", "factors", "profile", "warehouses", "contracts"} {
		if !r.fresh(dataset) {
			return fmt.Errorf("new build requires current successful %s authority", dataset)
		}
	}
	port, exists := findPort(r.stateCache.Catalog, data.BuildPort)
	if !exists {
		return errors.New("new build yard is absent from the current catalog")
	}
	hull, exists := findHull(r.stateCache.Catalog, data.BuildClass)
	if !exists {
		return errors.New("new build hull is absent from the current catalog")
	}
	quote, err := r.growthQuote(port, hull)
	if err != nil {
		return err
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
			if !r.reserveGrowthMission(&evidence, mission, &current) {
				return errors.New("new build waits for existing relocation or reusable settling capacity")
			}
			continue
		}
		if ship.Ship.Status != "sailing" && ship.Ship.ConvoyID == "" && ship.CargoUsedTons == 0 && len(r.activeFreight(ship.Ship.ID)) == 0 {
			input, err := r.committedPlanningInputExcept(ctx, ship, "__growth")
			if err != nil {
				return err
			}
			if hasAcceptedVoyage(input) {
				return errors.New("new build must reuse an idle ship's currently executable profitable lane first")
			}
		}
	}
	reason, err := r.growthFunding(ctx, &evidence)
	if err != nil {
		return err
	}
	if reason != "" {
		return errors.New(reason)
	}
	candidate := growthCandidate(port, hull)
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	grownFleet[len(grownFleet)-1] = candidate
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	deltaUpkeep := DailyUpkeep(grownFleet, r.stateCache.Catalog, len(r.stateCache.Factors)) - oldUpkeep
	pack, found, err := r.growthAtYard(ctx, port, hull, quote, deltaUpkeep, &evidence, &data.Plan)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("new build no longer has its exact quantity's funded positive repeat and recorded 12-real-hour execution evidence")
	}
	if data.GrowthLane == nil || data.GrowthLane.QuantityLimit != data.Plan.Quantity ||
		pack.plan.Quantity > data.GrowthLane.QuantityLimit ||
		growthCargoCash(pack.plan, r.stateCache.Markets[port.ID].CustomsBPS) > data.GrowthLane.InitialProcurementLimitPence ||
		pack.plan.TargetStoresTons > data.Plan.TargetStoresTons+1e-9 {
		return errors.New("new build no longer fits its frozen own cargo, customs and safe-store allocation")
	}
	for region, required := range pack.required {
		if evidence.balances[region] < required {
			return errors.New("new build must await completed regional funding before transport")
		}
	}
	return nil
}

func (r *Runner) considerGrowth(ctx context.Context) (bool, error) {
	if pending, err := r.db.GetMission(ctx, r.account.ID, "__growth"); err != nil || pending != nil {
		return false, err
	}
	pending, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		return r.holdCapacityGrowth(ctx, "capacity growth waits for every unresolved account action")
	}
	for _, id := range []string{"__treasury", "__underwriting", "__marque", "__futures"} {
		mission, data, err := r.loadMission(ctx, id)
		if err != nil {
			return false, err
		}
		if mission != nil && data.Action != nil {
			return r.holdCapacityGrowth(ctx, "capacity growth waits for every unresolved control action")
		}
	}
	evidence := growthEvidence{}
	workingByRegion := make(map[string]int64)
	laneEligible := true
	for _, ship := range r.stateCache.Ships {
		if ship.Ship.Status == "sunk" {
			continue
		}
		mission, data, err := r.loadMission(ctx, ship.Ship.ID)
		if err != nil {
			return false, err
		}
		if mission != nil {
			if data.Action != nil {
				return r.holdCapacityGrowth(ctx, "capacity growth waits for every unresolved mission action")
			}
			r.reserveMissionCash(workingByRegion, mission, &data, ship)
			if !r.reserveGrowthMission(&evidence, mission, &data) {
				laneEligible = false
			}
			continue
		}
		if ship.Ship.Status == "sailing" || ship.Ship.ConvoyID != "" || ship.CargoUsedTons > 0 || len(r.activeFreight(ship.Ship.ID)) > 0 {
			continue
		}
		// Direct assignment precedes ordinary ship processing, so use its
		// maintenance-first rule before sizing and buying any cargo.
		if r.opts.Execute {
			if changed, err := r.maintain(ctx, ship, nil, nil); changed || err != nil {
				return changed, err
			}
		}
		input, err := r.committedPlanningInput(ctx, ship)
		if err != nil {
			return false, err
		}
		if !r.opts.Execute {
			if hasAcceptedVoyage(input) {
				return r.holdCapacityGrowth(ctx, "capacity growth must reuse an idle ship's currently executable profitable lane first")
			}
			continue
		}
		plans := PlanVoyages(input)
		if len(plans) > 0 && plans[0].Accepted {
			return r.startVoyage(ctx, ship, plans[0])
		}
	}
	evidence.workingByRegion = workingByRegion
	if reason, err := r.growthCashFunding(ctx, &evidence); err != nil {
		return false, err
	} else if reason != "" {
		return r.holdCapacityGrowth(ctx, reason)
	}
	if !laneEligible {
		return r.considerCapacityGrowth(ctx, &evidence)
	}
	if reason, err := r.growthLaneProof(ctx, &evidence); err != nil {
		return false, err
	} else if reason != "" {
		return r.considerCapacityGrowth(ctx, &evidence)
	}
	oldUpkeep := DailyUpkeep(r.stateCache.Ships, r.stateCache.Catalog, len(r.stateCache.Factors))
	grownFleet := make([]ShipState, len(r.stateCache.Ships)+1)
	copy(grownFleet, r.stateCache.Ships)
	var best growthPackage
	found := false
	for _, port := range r.stateCache.Catalog.Ports {
		if !port.HasShipyard || !r.fresh("books/"+port.ID) || r.stateCache.Markets[port.ID].Visibility != "quay" {
			continue
		}
		for _, hull := range r.stateCache.Catalog.ShipClasses {
			if !cheapCapacityHull(hull) {
				continue
			}
			quote, err := r.growthQuote(port, hull)
			if err != nil {
				continue
			}
			candidate := growthCandidate(port, hull)
			grownFleet[len(grownFleet)-1] = candidate
			deltaUpkeep := DailyUpkeep(grownFleet, r.stateCache.Catalog, len(r.stateCache.Factors)) - oldUpkeep
			pack, feasible, err := r.growthAtYard(ctx, port, hull, quote, deltaUpkeep, &evidence, nil)
			if err != nil {
				return false, err
			}
			if feasible && (!found || betterGrowthPackage(pack, best)) {
				best, found = pack, true
			}
		}
	}
	if !found {
		return r.considerCapacityGrowth(ctx, &evidence)
	}
	savingsTarget, err := r.liveCapacitySavingsTarget(ctx)
	if err != nil {
		return false, err
	}
	if savingsTarget.PortID != "" {
		lotPence, err := r.capacityLotCash(ctx, evidence.asOf)
		if err != nil {
			return false, err
		}
		savingsRate, err := r.capacitySavingsRate(ctx, evidence.asOf)
		if err != nil {
			return false, err
		}
		wait, err := r.capacitySavingsWait(ctx, savingsTarget, best.decision, evidence.available, savingsRate, lotPence)
		if err != nil {
			return false, err
		}
		if wait {
			r.snapshot["_capacity_growth_port"] = encode("")
			if savingsTarget.RequiredPence <= evidence.available {
				return r.considerCapacityGrowthAt(ctx, &evidence, savingsTarget.PortID, savingsTarget.HullID)
			}
			return r.holdCapacitySavings(ctx, savingsTarget, best.decision, evidence.available, savingsRate)
		}
		if err := r.setCapacitySavingsTarget(ctx, capacitySavingsTarget{}); err != nil {
			return false, err
		}
	}
	key := "growth:" + best.port.ID + "|" + best.hull.ID + ":" + laneKey(best.plan)
	if err := r.decision(ctx, "", key, best.decision.Reason, best.decision); err != nil {
		return false, err
	}
	if !r.opts.Execute {
		return false, r.hold(ctx, "", "observe mode: admitted lane-growth package is not executed")
	}
	if changed, err := r.fundTreasury(ctx, evidence.balances, best.required); changed || err != nil {
		return changed, err
	}
	for region, required := range best.required {
		if evidence.balances[region] < required {
			return false, r.holdExpected(ctx, "", "best capital-return package awaits completed regional funding")
		}
	}
	return r.startGrowth(ctx, best.port, best.hull, best.plan, best.decision.WorkingCapitalPence)
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
		Plan: plan,
		GrowthLane: &growthLaneIntent{
			AdmissionID:                  mission.Generation,
			QuantityLimit:                plan.Quantity,
			InitialProcurementLimitPence: growthCargoCash(plan, r.stateCache.Markets[port.ID].CustomsBPS),
		},
		BuildName:                 name,
		BuildClass:                hull.ID,
		BuildPort:                 port.ID,
		TargetCannons:             min(4, hull.MaxCannons),
		GrowthWorkingCapitalPence: workingCapital,
		CostsKnown:                true,
		ProvenLane:                laneKey(plan),
		StartedGameHours:          r.stateCache.Clock.Calendar.TotalGameHours,
		InitialLedgerIDs:          r.ledgerIDs(),
		StartedAt:                 time.Now().UTC(),
	}
	// The generated name and complete payload become durable before dispatch.
	return true, r.stage(ctx, mission, &data, ShipState{}, "build", "/v1/shipyard/build-ship", map[string]any{"port_id": port.ID, "name": name, "ship_class": hull.ID, "build_mode": "turnkey_cash"})
}

func (r *Runner) driveGrowth(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if !r.opts.Execute {
		return false, r.hold(ctx, "", "observe mode: persisted growth intent retained")
	}
	capacityRefitFinished := data.Capacity != nil && (data.Capacity.RefitRefused || data.Capacity.RefitAttempts >= 3)
	if time.Now().Before(data.CooldownUntil) && !capacityRefitFinished {
		return false, r.hold(ctx, "", "commissioning waits for persisted retry cooldown")
	}
	if data.Action != nil {
		return false, r.hold(ctx, "", "commissioning retains its unresolved immutable action before any allocation transition")
	}
	if data.Capacity != nil && data.Capacity.Financing != nil && data.BuiltShipID == "" {
		return r.driveCapacityFinancing(ctx, mission, data)
	}
	if mission.Phase == "settling" || mission.Phase == "settled" {
		return r.finishGrowthControl(ctx, mission, data)
	}
	if data.Action == nil &&
		(mission.Phase == "growth_rejected" || (mission.Phase == "growth_build" && data.BuiltShipID == "")) {
		return r.finishGrowthControl(ctx, mission, data)
	}
	ship, exists := r.ship(data.BuiltShipID)
	if !exists {
		return false, r.hold(ctx, "", "build outcome not yet present in authoritative fleet; no duplicate build")
	}
	if ship.Ship.Status == "sunk" {
		return r.finishGrowthControl(ctx, mission, data)
	}
	if data.Capacity != nil {
		return r.driveCapacityGrowth(ctx, ship, mission, data)
	}
	if data.GrowthLane == nil || data.GrowthLane.AdmissionID != mission.Generation {
		return false, r.hold(ctx, ship.Ship.ID, "commissioning lacks its durable admitted cargo allocation")
	}
	if data.GrowthLane.withdrawn() && (data.Plan.Kind != "aborted" || mission.Phase != "commissioning") {
		data.Plan = Plan{
			Kind: "aborted", ShipID: ship.Ship.ID, Destination: ship.Ship.CurrentPortID,
			Reason: "the unsupported allocation was explicitly withdrawn; close the actual paid hull without further discretionary commissioning",
		}
		data.GrowthWorkingCapitalPence = 0
		mission.Phase = "commissioning"
		return true, r.saveMission(ctx, mission, data)
	}
	if mission.Phase == "commissioning" && data.Plan.Kind == "aborted" {
		if ship.Voyage != nil || ship.Ship.Status != "berthed" && ship.Ship.Status != "anchored_roadstead" {
			return false, r.hold(ctx, ship.Ship.ID, "withdrawn commissioning waits for the actual hull's authoritative stationary arrival")
		}
		if !emptyCargo(ship) || len(r.activeFreight(ship.Ship.ID)) > 0 {
			return false, r.hold(ctx, ship.Ship.ID, "withdrawn commissioning preserves actual cargo and sealed obligations")
		}
		data.Plan.Destination = ship.Ship.CurrentPortID
		return r.handoffGrowthVoyage(ctx, ship, mission, data)
	}
	if ship.Ship.CurrentPortID != data.BuildPort || ship.Ship.Status != "berthed" {
		return false, r.hold(ctx, ship.Ship.ID, "new hull moved before commissioning; refit/provision awaits actual build port berth")
	}
	input, err := r.committedPlanningInput(ctx, ship)
	if err != nil {
		return false, err
	}
	if !r.growthSourceAuthority(input, data.BuildPort) {
		return false, r.hold(ctx, ship.Ship.ID, "commissioning requires current lawful source quay access")
	}
	if err := growthOperatingAuthority(input, data.Plan); err != nil {
		return false, r.hold(ctx, ship.Ship.ID, err.Error())
	}
	port, exists := findPort(input.Catalog, data.BuildPort)
	if !exists {
		return false, r.hold(ctx, ship.Ship.ID, "commissioning source is absent from the current catalog")
	}
	addedCannons := max(0, data.TargetCannons-ship.Ship.CannonsCount)
	reserveGrowthCommissioning(&input, data)
	quote := quoteGrowthDelivery(input, data.Plan, data.GrowthLane, true, nil)
	if !quote.plan.Accepted {
		data.Plan = Plan{
			Kind: "aborted", ShipID: ship.Ship.ID, Destination: ship.Ship.CurrentPortID,
			Reason: "fresh commissioning economics no longer fit the admitted allocation: " + quote.plan.Reason,
		}
		data.GrowthLane.QuantityLimit = 0
		data.GrowthLane.InitialProcurementLimitPence = 0
		data.GrowthLane.InitialProcurementComplete = true
		data.GrowthLane.NextPlan = nil
		data.GrowthWorkingCapitalPence = 0
		mission.Phase = "commissioning"
		return true, r.saveMission(ctx, mission, data)
	}
	data.Plan = quote.plan
	data.GrowthWorkingCapitalPence = data.GrowthLane.InitialProcurementLimitPence
	if len(quote.transfers) > 0 {
		return true, r.stageGrowthFunding(ctx, ship, mission, data, quote.transfers[0])
	}
	if mission.Phase == "growth_refit" {
		if ship.Ship.CannonsCount < data.TargetCannons {
			cost := addedCannons * 15_000
			if input.Balances[port.BankingRegion]-cost-data.GrowthWorkingCapitalPence < input.ReserveByRegion[port.BankingRegion]-cost {
				return false, r.hold(ctx, ship.Ship.ID, "commissioning cannons must preserve incumbent commitments, own cargo allocation and regional reserves")
			}
			return true, r.stage(ctx, mission, data, ship, "refit", "/v1/shipyard/modify-ship", map[string]any{"ship_id": ship.Ship.ID, "target_cannons_count": data.TargetCannons})
		}
		mission.Phase = "growth_provision"
		return true, r.saveMission(ctx, mission, data)
	}
	if ship.Stores.ProvisionsTons+1e-9 < math.Max(1, data.Plan.TargetStoresTons) {
		return r.provisionMission(ctx, ship, mission, data, math.Max(1, data.Plan.TargetStoresTons))
	}
	if mission.Phase == "growth_provision" {
		mission.Phase = "commissioning"
		return true, r.saveMission(ctx, mission, data)
	}
	if mission.Phase == "commissioning" {
		return r.handoffGrowthVoyage(ctx, ship, mission, data)
	}
	return false, r.hold(ctx, "", "persisted growth stage has no safe transition")
}

func (r *Runner) handoffGrowthVoyage(ctx context.Context, ship ShipState, mission *store.Mission, data *missionData) (bool, error) {
	r.normalizeGrowthAccounting(data)
	plan := data.Plan
	plan.ShipID = ship.Ship.ID
	next, nextData := r.newVoyage(ship, plan)
	next.Generation = growthHandoffGeneration(mission.Generation, ship.Ship.ID)
	lane := *data.GrowthLane
	nextData.GrowthLane = &lane
	// Retain committed capital as first-episode provenance, not a future
	// reservation or an operating expense.
	nextData.BuiltShipID = data.BuiltShipID
	nextData.BuildCostPence = data.BuildCostPence
	nextData.AcquisitionPence = data.AcquisitionPence
	nextData.ProceedsPence = data.ProceedsPence
	nextData.FeesPence = data.FeesPence
	nextData.ProvisionsPence = data.ProvisionsPence
	nextData.ExpensesPence = data.ExpensesPence
	nextData.CostsKnown = data.CostsKnown
	nextData.LastActionID = data.LastActionID
	nextData.StartedAt = data.StartedAt
	nextData.StartedGameHours = data.StartedGameHours
	nextData.InitialLedgerIDs = data.InitialLedgerIDs
	if data.ProvenLane != "" {
		nextData.ProvenLane = data.ProvenLane
	}
	reason := "actual built hull is armed, safely provisioned and atomically assigned its admitted bounded first cargo"
	if plan.Kind == "aborted" {
		next.Phase = "closing"
		nextData.BuyDone = true
		lane.InitialProcurementComplete = true
		reason = "freshly disproved initial lane released the actual built hull through an atomic action-free closing handoff"
	}
	next.Data = encode(nextData)
	if _, err := r.db.HandoffGrowthMission(ctx, r.account.ID, mission.Generation, next); err != nil {
		return false, err
	}
	var actualHullCost *int64
	if data.CostsKnown {
		actualHullCost = &data.BuildCostPence
	}
	return true, r.decision(ctx, ship.Ship.ID, "growth_commissioned", reason, map[string]any{
		"ship_id":                         ship.Ship.ID,
		"actual_hull_cost_pence":          actualHullCost,
		"costs_known":                     data.CostsKnown,
		"name":                            data.BuildName,
		"cannons":                         ship.Ship.CannonsCount,
		"stores_tons":                     ship.Stores.ProvisionsTons,
		"quantity_limit":                  lane.QuantityLimit,
		"initial_procurement_limit_pence": lane.InitialProcurementLimitPence,
	})
}

// Older accounting boundaries cannot authorize assigning unrelated old ledger
// debits to this mission. Keep measured payments and expose the missing basis.
func (r *Runner) normalizeGrowthAccounting(data *missionData) bool {
	changed := false
	if data.InitialLedgerIDs == nil {
		data.InitialLedgerIDs = r.ledgerIDs()
		data.StartedGameHours = r.stateCache.Clock.Calendar.TotalGameHours
		data.CostsKnown = false
		changed = true
	}
	if data.StartedAt.IsZero() {
		data.StartedAt = time.Now().UTC()
		data.CostsKnown = false
		changed = true
	}
	return changed
}

func (r *Runner) finishGrowthControl(ctx context.Context, mission *store.Mission, data *missionData) (bool, error) {
	if data.Action != nil {
		return false, r.hold(ctx, "", "growth closeout preserves the unresolved original action")
	}
	if data.Settlement == nil {
		basisChanged := r.normalizeGrowthAccounting(data)
		if data.LastActionID == "" {
			if data.CostsKnown && data.BuiltShipID == "" && data.BuildCostPence == 0 &&
				(mission.Phase == "growth_build" || mission.Phase == "growth_rejected") &&
				data.AcquisitionPence == 0 && data.ProceedsPence == 0 && data.FeesPence == 0 &&
				data.ProvisionsPence == 0 && data.ExpensesPence == 0 {
				return true, r.db.DeleteMission(ctx, r.account.ID, mission.ShipID)
			}
			if basisChanged {
				return true, r.saveMission(ctx, mission, data)
			}
			return false, r.hold(ctx, data.BuiltShipID, "growth closeout retains paid or uncertain exposure without genuine terminal action evidence")
		}
		data.Plan.Kind = "aborted"
		data.Plan.Quantity = 0
		data.DeliveredQuantity = 0
		data.ReturnJourney = false
		data.FinishAtDestination = false
		if data.GrowthLane != nil {
			data.GrowthLane.QuantityLimit = 0
			data.GrowthLane.InitialProcurementLimitPence = 0
			data.GrowthLane.InitialProcurementComplete = true
			data.GrowthLane.NextPlan = nil
		}
	}
	actor, exists := r.ship(data.BuiltShipID)
	if !exists {
		// Empty means company-only accounting, not an invented built ship.
		actor.Ship.ID = data.BuiltShipID
	}
	return r.finishMission(ctx, actor, mission, data)
}
