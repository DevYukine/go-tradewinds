package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// PlanningSources supplies the policy context normally owned by a Runner.
// The experiment knobs deliberately remain zero-valued on live inputs.
type PlanningSources struct {
	CompanyID            string
	ReservePence         int64
	Metadata             map[string]DatasetMeta
	Generation           string
	CharterProceedsPence int64
	Competition          *store.CompetitionSummary
}

// PlanningInputFor builds the same input used by the live Runner without I/O.
func PlanningInputFor(now time.Time, ship ShipState, state AccountState, snapshot map[string]json.RawMessage, sources PlanningSources) PlanningInput {
	balances := make(map[string]int64)
	for _, vault := range state.Vaults {
		if vault.AccountType == "vault" {
			balances[vault.BankingRegion] += vault.BalancePence
		}
	}
	marqueZones := make(map[string]bool)
	lower, _, validClock := serverBounds(now, state.Clock, sources.Metadata["clock"], sources.Generation)
	for _, marque := range decode[[]letterOfMarque](snapshot, "marques") {
		if !validClock || lower.Before(marque.ExpiresAt) {
			marqueZones[marque.SeaZoneID] = true
		}
	}
	for _, marque := range decode[map[string]letterOfMarque](snapshot, "_marque_active") {
		if !validClock || lower.Before(marque.ExpiresAt) {
			marqueZones[marque.SeaZoneID] = true
		}
	}
	if pending := decode[marquePending](snapshot, "_marque_pending"); pending.SeaZoneID != "" &&
		(pending.ExpiresAt.IsZero() || !validClock || lower.Before(pending.ExpiresAt)) {
		marqueZones[pending.SeaZoneID] = true
	}
	shocks, eventsValid := decodeMarketShocks(snapshot)
	_, eventsTracked := sources.Metadata["world_events"]
	_, eventsRecorded := snapshot["world_events"]
	return PlanningInput{
		Now:                   now,
		CompanyID:             sources.CompanyID,
		Ship:                  ship,
		Fleet:                 state.Ships,
		HazardQuotes:          state.HazardQuotes,
		Catalog:               state.Catalog,
		Clock:                 state.Clock,
		Zones:                 decode[[]ZoneObservation](snapshot, "zones"),
		MarqueZones:           marqueZones,
		Markets:               state.Markets,
		Ports:                 state.Ports,
		BookObservedAt:        decode[map[string]time.Time](snapshot, "_book_observed"),
		QuayBookMemory:        decode[map[string]RetainedQuayBook](snapshot, "_quay_book_memory"),
		BookDrift:             decode[[]BookDriftSample](snapshot, "_book_drift"),
		RevenueCalibration:    decode[map[string]int64](snapshot, "_revenue_calibration"),
		MarketShocks:          shocks,
		marketEventsInvalid:   !eventsValid && (eventsTracked || eventsRecorded),
		Routes:                state.Routes,
		Contracts:             taperedContracts(state.Contracts, state.Catalog, sources.CharterProceedsPence, ship.EffectiveHoldCapacityTons),
		Balances:              balances,
		ReserveByRegion:       obligationReserves(state, snapshot, sources.ReservePence),
		ReservePence:          sources.ReservePence,
		Metadata:              sources.Metadata,
		Generation:            sources.Generation,
		FleetDailyUpkeepPence: DailyUpkeep(state.Ships, state.Catalog, len(state.Factors)),
		Competition:           sources.Competition,
		Rates:                 state.Rates,
		ReservedPlayerBidTons: restingBidReservations(snapshot),
		PlayerBidSellFills:    decode[[]PlayerBidSellFill](snapshot, "fills"),
	}
}

func PlanSourcingRelocation(input PlanningInput) []Plan {
	return planSourcingRelocation(input)
}

func SizeRelocationCargo(input PlanningInput, probe Plan, charter Contract, spentCostPence int64) Plan {
	return sizeRelocationCargo(input, probe, charter, spentCostPence)
}

// PlanLacksHazardQuote distinguishes missing recorded risk evidence from an
// observed unprofitable or failed source inspection.
func PlanLacksHazardQuote(plan Plan) bool {
	return !plan.Accepted && plan.Reason == hazardUnavailable
}

// TaperedCharterReward applies the live reward estimate to the actual delivery.
func TaperedCharterReward(contract Contract, catalog Catalog, earnedPence, deliveryTons int64) int64 {
	return taperedCharterReward(contract, catalog, earnedPence, deliveryTons)
}

func LaneKey(plan Plan) string { return laneKey(plan) }

// SourcingLaneKey uses the same evidence identity as live probe cooldowns.
func SourcingLaneKey(input PlanningInput, plan Plan) string {
	for _, charter := range input.Contracts {
		if charter.ID == plan.ContractID {
			source, _ := findPort(input.Catalog, plan.Destination)
			_, _, ask := publicSourceEvidence(input, source, charter)
			return sourcingEvidenceKey(plan.Destination, charter, ask)
		}
	}
	return ""
}

func CharterProceedsWindow(scale int64) time.Duration {
	if scale <= 0 {
		return 0
	}
	return time.Duration(charterTaperWindowGameDays*24*int64(time.Hour)) / time.Duration(scale)
}

type FundingTransfer struct {
	Rate  Rate
	Gross int64
	Net   int64
}

// PlanFunding returns the production remittances for an accepted quote. Quotes
// already include their funding fee; re-quoting never charges it a second time.
func PlanFunding(input PlanningInput, plan Plan) (Plan, []FundingTransfer) {
	if !plan.Accepted {
		return plan, nil
	}
	var transfers []observedRemittance
	if plan.Kind == "relocation" {
		source, exists := findPort(input.Catalog, plan.Destination)
		origin, originExists := findPort(input.Catalog, plan.Origin)
		if !exists || !originExists {
			return reject(plan, "sourcing funding lacks its origin or source"), nil
		}
		plan = costRelocation(input, plan)
		if !plan.Accepted {
			return plan, nil
		}
		provisions := bookCommodity(input.Markets[plan.Origin], "provisions")
		if provisions == nil || provisions.Depth == nil {
			return reject(plan, "sourcing store-risk basis lacks current local replacement depth"), nil
		}
		fill, err := WalkDepth(provisions.Depth.Asks, max(1, plan.StoresToBuy), true)
		if err != nil {
			return reject(plan, "sourcing store-risk basis: "+err.Error()), nil
		}
		bounded := input
		bounded.ReserveByRegion = sourcingReserves(input, source)
		// Live creates the durable intent before funding and gives it the
		// origin's replacement-store basis, so its budget method prices the
		// stores the round trip will consume instead of trusting the modeled
		// expense alone. Rebuild that fresh departure basis here and reuse the
		// live projected budget rather than re-deriving its cap arithmetic.
		data := missionData{
			Plan:       plan,
			CostsKnown: true,
			Relocation: &relocationIntent{
				ExpenseCapPence: sourcingAllowance(bounded),
				StoreBasis: &relocationStoreBasis{
					InitialTons:            input.Ship.Stores.ProvisionsTons,
					ReplacementPencePerTon: fill.LimitPence + ceilBPS(fill.LimitPence, input.Markets[plan.Origin].CustomsBPS),
				},
			},
		}
		if !planningRunner(input).relocationProjectedBudget(&data, input.Ship, bounded, plan, 0) {
			return reject(plan, "fresh sourcing costs exceed the durable exploration cap or lack a known expense basis"), nil
		}
		required := relocationFundingReserves(input, origin, source, plan.Economics.StoreCashPence+plan.Economics.BerthDuesPence+plan.Economics.UpkeepPence)
		_, _, transfers, _ = measuredFunding(input.Balances, required, input.Rates)
	} else {
		fees := plan.Economics.FundingFeesPence
		plan.Economics.ExpectedNetPence += fees
		plan.Economics.StressNetPence += fees
		if plan.Economics.BallastRepeatGameDays > 0 {
			plan.Economics.BallastRepeatNetPence += fees
		}
		plan.Economics.FundingFeesPence = 0
		plan.Economics.NetPencePerGameDay = float64(plan.Economics.ExpectedNetPence) / plan.Economics.GameDays
		quote := fundVoyage(input, plan)
		plan, transfers = quote.plan, quote.transfers
	}
	refreshPlanRank(&plan)
	out := make([]FundingTransfer, len(transfers))
	for i, transfer := range transfers {
		out[i] = FundingTransfer{Rate: transfer.Rate, Gross: transfer.Gross, Net: transfer.Net}
	}
	return plan, out
}

// planningRunner adapts a planning input to the live Runner budget methods,
// which verify dataset freshness against wall time while a planning input
// carries its own observation clock. Every dataset the input considers fresh
// is translated into the same verdict against the real clock, so the reused
// live method decides identically offline. The snapshot stays empty, which
// keeps unclaimed ledger bills at zero: the honest offline view, because the
// input carries no unclaimed overhead evidence.
func planningRunner(input PlanningInput) *Runner {
	now := time.Now()
	meta := make(map[string]DatasetMeta, len(input.Metadata))
	for key, value := range input.Metadata {
		if !input.fresh(key) {
			continue
		}
		value.UpdatedAt, value.AttemptedAt, value.UpstreamAsOf = now, now, nil
		meta[key] = value
	}
	return &Runner{
		stateCache:    AccountState{Ships: []ShipState{input.Ship}, Clock: input.Clock},
		meta:          meta,
		generation:    input.Generation,
		expenseOwners: make(map[string]string),
	}
}

// SourcingDeparture uses host receipt time, like readProbeOutcomes, not the
// server envelope timestamp. Cargo presence, rather than value, makes it laden.
type SourcingDeparture struct {
	ShipID string
	Origin string
	At     time.Time
	Laden  bool
}

// SourcingLedger is variant-local permanent accounting and departure evidence.
// Applying it does not mutate the frame or include evidence from the future.
type SourcingLedger struct {
	settlements    []store.Settlement
	departures     []SourcingDeparture
	history        sourcingProbeHistory
	seenRows       map[string]bool
	seenDepartures map[SourcingDeparture]bool
	asOf           time.Time
}

func NewSourcingLedger(rows []store.Settlement, departures []SourcingDeparture) *SourcingLedger {
	return &SourcingLedger{
		settlements: append([]store.Settlement(nil), rows...),
		departures:  append([]SourcingDeparture(nil), departures...),
		history:     sourcingProbeHistory{Version: sourcingProbeHistoryVersion, Sources: make(map[string]probeSourceHistory), Ships: make(map[string]probeShipHistory)},
		seenRows:    make(map[string]bool), seenDepartures: make(map[SourcingDeparture]bool),
	}
}

func (ledger *SourcingLedger) RecordSettlement(row store.Settlement) {
	ledger.settlements = append(ledger.settlements, row)
}

func (ledger *SourcingLedger) RecordDeparture(row SourcingDeparture) {
	ledger.departures = append(ledger.departures, row)
}

func (ledger *SourcingLedger) Apply(input *PlanningInput, snapshot map[string]json.RawMessage) error {
	if input == nil {
		return fmt.Errorf("sourcing ledger: nil planning input")
	}
	if input.Now.Before(ledger.asOf) {
		return fmt.Errorf("sourcing ledger time cannot move backwards")
	}
	if ledger.asOf.IsZero() && len(snapshot["_sourcing_probe_history"]) > 0 {
		var retained sourcingProbeHistory
		if err := json.Unmarshal(snapshot["_sourcing_probe_history"], &retained); err != nil {
			return fmt.Errorf("decode retained sourcing outcomes: %w", err)
		}
		if retained.Version == sourcingProbeHistoryVersion {
			ledger.history = retained
			ledger.history.Cursor = store.ProbeHistoryCursor{}
			if ledger.history.Sources == nil {
				ledger.history.Sources = make(map[string]probeSourceHistory)
			}
			if ledger.history.Ships == nil {
				ledger.history.Ships = make(map[string]probeShipHistory)
			}
		}
	}
	type observation struct {
		at        time.Time
		row       *store.Settlement
		departure *SourcingDeparture
	}
	rows := make([]store.Settlement, 0, len(ledger.settlements))
	for _, row := range ledger.settlements {
		if !row.CompletedAt.After(input.Now) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	observations := make([]observation, 0, len(rows)+len(ledger.departures))
	lanes, completions := make(map[string]bool), make(map[string]time.Time)
	for i := range rows {
		row := &rows[i]
		observations = append(observations, observation{at: row.CompletedAt, row: row})
		if row.Kind == "relocation" {
			lanes[row.Lane] = true
			if row.CompletedAt.After(completions[row.Lane]) {
				completions[row.Lane] = row.CompletedAt
			}
		}
	}
	for i := range ledger.departures {
		departure := &ledger.departures[i]
		if !departure.At.After(input.Now) {
			observations = append(observations, observation{at: departure.At, departure: departure})
		}
	}
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].at.Before(observations[j].at) })
	history := &ledger.history
	idle := decode[map[string]shipIdleState](snapshot, "_ship_idle")
	for _, item := range observations {
		if item.row != nil {
			key := item.row.ID
			if key == "" {
				key = string(encode(item.row))
			}
			if !ledger.seenRows[key] {
				history.settlement(*item.row, idle)
				ledger.seenRows[key] = true
			}
		} else {
			departure := item.departure
			if !ledger.seenDepartures[*departure] {
				history.departure(departure.ShipID, departure.Origin, departure.At, departure.Laden, idle[departure.ShipID])
				ledger.seenDepartures[*departure] = true
			}
		}
	}
	input.SourcingIdleEligible = shipIdleFor(*input, snapshot, sourcingIdleGameHours)
	if input.SourcingIdleEligible {
		history.releaseIdleStreak(input.Ship.Ship.ID, idle[input.Ship.Ship.ID])
	}
	for key, observed := range history.Sources {
		if !input.Now.Before(observed.CatalogueFailedAt.Add(failedCatalogueProbeCooldown)) {
			observed.CatalogueFailedAt = time.Time{}
		}
		if input.Now.Sub(observed.FailedAt) > sourcingConversionWindow && !recentSourceConversion(observed, input.Now) {
			delete(history.Sources, key)
		} else {
			history.Sources[key] = observed
		}
	}
	if err := history.rollingBudget(rows, input.Now, input.Clock.Calendar.TimeScale); err != nil {
		return fmt.Errorf("sourcing ledger rolling budget: %w", err)
	}
	allowance := history.rollingAllowance()
	input.SourcingAllowancePence = &allowance
	input.SourcingProbeExpensePence = history.ProbeExpensePence
	input.SourcingProbeCostsUnknown = history.ProbeCostsUnknown
	input.SourcingProbeLanes, input.SourcingProbeCompletions = lanes, completions
	input.SourcingProbeHistory = *history
	input.SourcingHistoryError = ""
	ledger.asOf = input.Now
	return nil
}
