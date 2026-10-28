package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const failedCatalogueProbeCooldown = 24 * time.Hour
const sourcingConversionWindow = 7 * 24 * time.Hour
const sourcingBudgetGameDays = 60
const sourcingProbeHistoryVersion = 3
const sourcingConversionLedgerLimit = 200

type probeConversion struct {
	SettlementID string    `json:"settlement_id"`
	Source       string    `json:"source"`
	Commodity    string    `json:"commodity"`
	ProbeAt      time.Time `json:"probe_at"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	NetPence     int64     `json:"net_pence"`
}

type probeSourceHistory struct {
	ConvertedAt       time.Time `json:"converted_at"`
	FailedAt          time.Time `json:"failed_at"`
	CatalogueFailedAt time.Time `json:"catalogue_failed_at"`
}

type pendingSourceProbe struct {
	Source    string    `json:"source"`
	Commodity string    `json:"commodity"`
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	Catalogue bool      `json:"catalogue"`
	Scout     bool      `json:"scout,omitempty"`
}

type probeShipHistory struct {
	Failures        int                 `json:"failures"`
	LastDepartureAt time.Time           `json:"last_departure_at"`
	Pending         *pendingSourceProbe `json:"pending,omitempty"`
	IdlePort        string              `json:"idle_port,omitempty"`
	IdleHours       int64               `json:"idle_hours"`
}

type scoutPortHistory struct {
	CompletedAt     time.Time `json:"completed_at"`
	LastDepartureAt time.Time `json:"last_departure_at"`
}

// Persist bounded outcomes and rolling money evidence. A version change rebuilds
// from accounting and retained departures, never an old policy allowance.
type sourcingProbeHistory struct {
	Version            int                           `json:"version"`
	Cursor             store.ProbeHistoryCursor      `json:"cursor"`
	Sources            map[string]probeSourceHistory `json:"sources"`
	Ships              map[string]probeShipHistory   `json:"ships"`
	ScoutFailures      int                           `json:"scout_failures,omitempty"`
	ScoutResetAt       time.Time                     `json:"scout_reset_at,omitempty"`
	ScoutPorts         map[string]scoutPortHistory   `json:"scout_ports,omitempty"`
	BudgetAsOf         time.Time                     `json:"budget_as_of"`
	BudgetWindowStart  time.Time                     `json:"budget_window_start"`
	ConversionNetPence int64                         `json:"conversion_net_pence"`
	ProbeExpensePence  int64                         `json:"probe_expense_pence"`
	ProbeCostsUnknown  bool                          `json:"probe_costs_unknown"`
	Conversions        []probeConversion             `json:"conversions,omitempty"`
}

type sourcingHistoryCache struct {
	Ready       bool
	Error       string
	Outcomes    sourcingProbeHistory
	Allowance   int64
	Expense     int64
	UnknownCost bool
	Lanes       map[string]bool
	Completions map[string]time.Time
}

func probeSourceKey(source, commodity string) string { return source + "|" + commodity }

func recentSourceConversion(observed probeSourceHistory, now time.Time) bool {
	return !observed.ConvertedAt.IsZero() && !observed.ConvertedAt.After(now) && now.Sub(observed.ConvertedAt) <= sourcingConversionWindow
}

func (h sourcingProbeHistory) sourceRank(now time.Time, source, commodity string, freshAsk bool) int {
	observed := h.Sources[probeSourceKey(source, commodity)]
	if recentSourceConversion(observed, now) {
		return 0
	}
	if freshAsk {
		return 1
	}
	if observed.FailedAt.IsZero() || now.Sub(observed.FailedAt) > sourcingConversionWindow {
		return 2
	}
	return 3
}

func (h *sourcingProbeHistory) failedProbe(ship *probeShipHistory, probe pendingSourceProbe, idle shipIdleState) {
	if probe.Scout {
		h.failedProbeStreak(ship, probe, idle)
		return
	}
	key := probeSourceKey(probe.Source, probe.Commodity)
	observed := h.Sources[key]
	if probe.At.After(observed.FailedAt) {
		observed.FailedAt = probe.At
	}
	if probe.Catalogue && probe.At.After(observed.CatalogueFailedAt) {
		observed.CatalogueFailedAt = probe.At
	}
	h.Sources[key] = observed
	h.failedProbeStreak(ship, probe, idle)
}

func (h *sourcingProbeHistory) failedProbeStreak(ship *probeShipHistory, probe pendingSourceProbe, idle shipIdleState) {
	ship.Failures = min(2, ship.Failures+1)
	if ship.Failures >= 2 {
		ship.IdlePort, ship.IdleHours = probe.Source, 0
		if idle.PortID == probe.Source {
			ship.IdleHours = idle.EarnedGameHours
		}
	}
}

func (h *sourcingProbeHistory) settlement(row store.Settlement, idle map[string]shipIdleState) {
	lane := strings.SplitN(row.Lane, "|", 4)
	if row.Kind != "relocation" || row.ShipID == "" || row.Commodity == "" || row.CompletedAt.IsZero() ||
		row.Origin == row.Destination || len(lane) != 4 || lane[0] != row.Destination || lane[2] != row.Commodity {
		return
	}
	ship := h.Ships[row.ShipID]
	if row.CompletedAt.Before(ship.LastDepartureAt) || ship.Pending != nil && !row.CompletedAt.After(ship.Pending.At) {
		return
	}
	probe := pendingSourceProbe{Source: row.Destination, Commodity: row.Commodity, At: row.CompletedAt, Outcome: row.ProbeOutcome,
		Catalogue: strings.HasPrefix(lane[3], "probe:0:") || lane[3] == "charter", Scout: lane[3] == "scout"}
	ship.Pending = &probe
	if probe.Scout {
		if h.ScoutPorts == nil {
			h.ScoutPorts = make(map[string]scoutPortHistory)
		}
		port := h.ScoutPorts[probe.Source]
		if probe.At.After(port.CompletedAt) {
			port.CompletedAt = probe.At
		}
		h.ScoutPorts[probe.Source] = port
		// A completed scout is unconverted until an actual laden departure
		// from a scouted port. Idle time cannot replenish this fleet cap.
		if probe.At.After(h.ScoutResetAt) {
			h.ScoutFailures = min(2, h.ScoutFailures+1)
		}
	}
	if row.ProbeOutcome == store.ProbeOutcomeFailed {
		h.failedProbe(&ship, probe, idle[row.ShipID])
	}
	h.Ships[row.ShipID] = ship
}

func (h *sourcingProbeHistory) departure(shipID, source string, at time.Time, laden bool, idle shipIdleState) {
	ship := h.Ships[shipID]
	if !at.After(ship.LastDepartureAt) {
		return
	}
	if probe := ship.Pending; probe != nil && source == probe.Source && at.After(probe.At) {
		if laden && !probe.Scout {
			key := probeSourceKey(source, probe.Commodity)
			observed := h.Sources[key]
			observed.ConvertedAt = at
			h.Sources[key] = observed
		} else if !laden && probe.Outcome == store.ProbeOutcomeContinuation {
			h.failedProbe(&ship, *probe, idle)
		} else if !laden && probe.Outcome != store.ProbeOutcomeFailed {
			// Uninspected probes still count against this ship's empty-leg
			// streak, but cannot establish a source-wide supply failure.
			h.failedProbeStreak(&ship, *probe, idle)
		}
		ship.Pending = nil
	}
	if laden {
		ship.Failures = 0
	}
	if source != "" {
		if h.ScoutPorts == nil {
			h.ScoutPorts = make(map[string]scoutPortHistory)
		}
		port := h.ScoutPorts[source]
		if laden && port.CompletedAt.After(h.ScoutResetAt) && at.After(port.CompletedAt) {
			h.ScoutFailures = 0
			h.ScoutResetAt = at
		}
		if at.After(port.LastDepartureAt) {
			port.LastDepartureAt = at
		}
		h.ScoutPorts[source] = port
	}
	ship.LastDepartureAt = at
	h.Ships[shipID] = ship
}

// The caller already validated idle clock continuity with shipIdleEligible.
func (h *sourcingProbeHistory) releaseIdleStreak(shipID string, idle shipIdleState) {
	ship := h.Ships[shipID]
	if ship.Failures < 2 {
		return
	}
	if ship.IdlePort != idle.PortID || ship.IdleHours > idle.EarnedGameHours {
		ship.IdlePort, ship.IdleHours = idle.PortID, idle.EarnedGameHours
	} else if idle.EarnedGameHours-ship.IdleHours >= sourcingIdleGameHours {
		ship.Failures = 0
	}
	h.Ships[shipID] = ship
}

func (r *Runner) readProbeOutcomes(ctx context.Context, input PlanningInput) (sourcingProbeHistory, error) {
	var history sourcingProbeHistory
	if raw := r.snapshot["_sourcing_probe_history"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &history); err != nil {
			return history, fmt.Errorf("decode sourcing outcomes: %w", err)
		}
	}
	if history.Version != sourcingProbeHistoryVersion {
		history = sourcingProbeHistory{Version: sourcingProbeHistoryVersion}
	}
	if history.Sources == nil {
		history.Sources = make(map[string]probeSourceHistory)
	}
	if history.Ships == nil {
		history.Ships = make(map[string]probeShipHistory)
	}
	settlements, departures, err := r.db.ProbeHistorySince(ctx, r.account.ID, history.Cursor)
	if err != nil {
		return history, err
	}
	type observation struct {
		at         time.Time
		settlement *store.Settlement
		shipID     string
		origin     string
		laden      bool
	}
	observations := make([]observation, 0, len(settlements)+len(departures))
	for i := range settlements {
		row := &settlements[i]
		observations = append(observations, observation{at: row.CompletedAt, settlement: row})
		history.Cursor.SettlementAt, history.Cursor.SettlementID = row.CreatedAt, row.ID
	}
	for _, event := range departures {
		history.Cursor.DepartureID = event.ID
		var identity struct {
			CompanyID string `json:"company_id"`
		}
		if err := json.Unmarshal(event.Payload, &identity); err != nil {
			r.log.Warn("skipping departure with undecodable company identity", zap.Int64("event_sequence", event.ID))
			continue
		}
		if identity.CompanyID != input.CompanyID {
			continue
		}
		var envelope struct {
			Payload struct {
				ShipID        string           `json:"ship_id"`
				Origin        string           `json:"origin_port_id"`
				CargoManifest map[string]int64 `json:"cargo_manifest"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(event.Payload, &envelope); err != nil {
			r.log.Warn("skipping undecodable own sourcing departure", zap.Int64("event_sequence", event.ID))
			continue
		}
		if envelope.Payload.ShipID == "" || envelope.Payload.CargoManifest == nil {
			r.log.Warn("skipping incomplete own sourcing departure", zap.Int64("event_sequence", event.ID))
			continue
		}
		laden := false
		for _, quantity := range envelope.Payload.CargoManifest {
			laden = laden || quantity > 0
		}
		// Both event receipt and settlement completion use host time. The
		// server envelope timestamp cannot order local accounting boundaries.
		observations = append(observations, observation{at: event.CreatedAt, shipID: envelope.Payload.ShipID, origin: envelope.Payload.Origin, laden: laden})
	}
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].at.Before(observations[j].at) })
	idle := decode[map[string]shipIdleState](r.snapshot, "_ship_idle")
	for _, item := range observations {
		if item.settlement != nil {
			history.settlement(*item.settlement, idle)
		} else {
			history.departure(item.shipID, item.origin, item.at, item.laden, idle[item.shipID])
		}
	}
	for _, ship := range r.stateCache.Ships {
		if history.Ships[ship.Ship.ID].Failures < 2 {
			continue
		}
		if r.shipIdleEligible(r.planningInput(ship)) {
			history.releaseIdleStreak(ship.Ship.ID, idle[ship.Ship.ID])
		}
	}
	if input.fresh("ships") {
		for id := range history.Ships {
			if ship, exists := r.ship(id); !exists || ship.Ship.Status == "sunk" {
				delete(history.Ships, id)
			}
		}
	}
	if input.fresh("catalog") {
		for port := range history.ScoutPorts {
			if _, exists := findPort(input.Catalog, port); !exists {
				delete(history.ScoutPorts, port)
			}
		}
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
	return history, nil
}

// Rebuild money evidence from authoritative accounting, not the capped display
// ledger or the incremental outcome cursor. Late settlements and changed window
// bounds must not lose costs or attribute a delivery to a later probe.
func (h *sourcingProbeHistory) rollingBudget(rows []store.Settlement, now time.Time, scale int64) error {
	if now.IsZero() || scale <= 0 {
		return fmt.Errorf("rolling exploration budget requires a current positive game clock")
	}
	h.BudgetAsOf = now
	h.BudgetWindowStart = now.Add(-time.Duration(sourcingBudgetGameDays*24*int64(time.Hour)) / time.Duration(scale))
	h.ConversionNetPence, h.ProbeExpensePence = 0, 0
	h.ProbeCostsUnknown = false
	h.Conversions = nil
	probes := make(map[string][]time.Time)
	for _, row := range rows {
		if row.Kind != "relocation" || row.CompletedAt.IsZero() || row.CompletedAt.After(now) {
			continue
		}
		if row.Origin != row.Destination && row.Destination != "" {
			probes[row.Destination] = append(probes[row.Destination], row.CompletedAt)
		}
		if strings.HasSuffix(row.Lane, "|scout") || row.CompletedAt.Before(h.BudgetWindowStart) {
			continue
		}
		if !row.CostsKnown {
			h.ProbeCostsUnknown = true
			continue
		}
		expense := int64(0)
		if row.ExplorationExpensePence != nil {
			expense = *row.ExplorationExpensePence
		} else {
			for _, cost := range []int64{row.AcquisitionPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence} {
				if cost < 0 || cost > math.MaxInt64-expense {
					return fmt.Errorf("rolling exploration expense exceeds integer-pence bounds")
				}
				expense += cost
			}
		}
		if expense < 0 || expense > math.MaxInt64-h.ProbeExpensePence {
			return fmt.Errorf("rolling exploration expense exceeds integer-pence bounds")
		}
		h.ProbeExpensePence += expense
	}
	for _, row := range rows {
		switch row.Kind {
		case "spot", "charter", "freight", "spot+freight", "charter+freight", "recovery":
		default:
			continue
		}
		if !row.CostsKnown || row.QuantityTons <= 0 || row.StartedAt.IsZero() || row.CompletedAt.IsZero() ||
			row.CompletedAt.Before(row.StartedAt) || row.CompletedAt.Before(h.BudgetWindowStart) || row.CompletedAt.After(now) {
			continue
		}
		var probeAt time.Time
		for _, at := range probes[row.Origin] {
			if !at.After(row.StartedAt) && row.StartedAt.Sub(at) <= sourcingConversionWindow && at.After(probeAt) {
				probeAt = at
			}
		}
		if probeAt.IsZero() {
			continue
		}
		net := row.ProceedsPence
		for _, cost := range []int64{row.AcquisitionPence, row.FeesPence, row.ProvisionsPence, row.ExpensesPence} {
			if cost < 0 || net < math.MinInt64+cost {
				return fmt.Errorf("rolling conversion net exceeds integer-pence bounds")
			}
			net -= cost
		}
		if net > 0 && h.ConversionNetPence > math.MaxInt64-net || net < 0 && h.ConversionNetPence < math.MinInt64-net {
			return fmt.Errorf("rolling conversion net exceeds integer-pence bounds")
		}
		h.ConversionNetPence += net
		h.Conversions = append(h.Conversions, probeConversion{SettlementID: row.ID, Source: row.Origin, Commodity: row.Commodity,
			ProbeAt: probeAt, StartedAt: row.StartedAt, CompletedAt: row.CompletedAt, NetPence: net})
	}
	sort.Slice(h.Conversions, func(i, j int) bool {
		if h.Conversions[i].CompletedAt.Equal(h.Conversions[j].CompletedAt) {
			return h.Conversions[i].SettlementID > h.Conversions[j].SettlementID
		}
		return h.Conversions[i].CompletedAt.After(h.Conversions[j].CompletedAt)
	})
	if len(h.Conversions) > sourcingConversionLedgerLimit {
		h.Conversions = h.Conversions[:sourcingConversionLedgerLimit:sourcingConversionLedgerLimit]
	}
	return nil
}

func (h sourcingProbeHistory) rollingAllowance() int64 {
	if h.ProbeCostsUnknown {
		return 0
	}
	// Floor, rather than Go's truncation toward zero, also applies to losses.
	halfNet := h.ConversionNetPence / 2
	if h.ConversionNetPence < 0 && h.ConversionNetPence%2 != 0 {
		halfNet--
	}
	if halfNet <= -20_000 {
		return 0
	}
	return max(0, 20_000+halfNet-h.ProbeExpensePence)
}

func (r *Runner) loadRollingSourcingBudget(ctx context.Context, input *PlanningInput) error {
	history := r.sourcingHistory.Outcomes
	if history.Version != sourcingProbeHistoryVersion {
		var err error
		history, err = r.readProbeOutcomes(ctx, *input)
		if err != nil {
			return err
		}
	}
	if input.Now.IsZero() || input.Clock.Calendar.TimeScale <= 0 {
		return fmt.Errorf("rolling exploration budget requires a current positive game clock")
	}
	window := time.Duration(sourcingBudgetGameDays*24*int64(time.Hour)) / time.Duration(input.Clock.Calendar.TimeScale)
	rows, err := r.db.SourcingBudgetSettlements(ctx, r.account.ID, input.Now.Add(-window), input.Now)
	if err != nil {
		return err
	}
	if err := history.rollingBudget(rows, input.Now, input.Clock.Calendar.TimeScale); err != nil {
		return err
	}
	r.snapshot["_sourcing_probe_history"] = encode(history)
	r.sourcingHistory.Outcomes = history
	input.SourcingProbeHistory = history
	allowance := history.rollingAllowance()
	input.SourcingAllowancePence = &allowance
	input.SourcingProbeExpensePence = history.ProbeExpensePence
	input.SourcingProbeCostsUnknown = history.ProbeCostsUnknown
	return nil
}

// Incremental outcomes refresh even when no ship can start sourcing. Input
// builders copy this frame; the allowance and lane queries are loaded on demand.
func (r *Runner) refreshSourcingHistory(ctx context.Context, input PlanningInput) {
	r.sourcingHistory.Ready = false
	r.sourcingHistory.Error = ""
	outcomes, err := r.readProbeOutcomes(ctx, input)
	if err != nil {
		// Malformed derived JSON cannot be left in the otherwise healthy
		// account frame: snapshot persistence must still be able to encode it.
		if raw := r.snapshot["_sourcing_probe_history"]; len(raw) > 0 && !json.Valid(raw) {
			delete(r.snapshot, "_sourcing_probe_history")
		}
		r.holdSourcingHistory(ctx, err)
		return
	}
	r.sourcingHistory.Outcomes = outcomes
	r.snapshot["_sourcing_probe_history"] = encode(outcomes)
}

func (r *Runner) refreshSourcingBudget(ctx context.Context, input PlanningInput) {
	if err := r.loadSourcingHistory(ctx, &input); err != nil {
		r.holdSourcingHistory(ctx, err)
		return
	}
	r.sourcingHistory = sourcingHistoryCache{Ready: true, Error: r.sourcingHistory.Error, Outcomes: r.sourcingHistory.Outcomes, Allowance: *input.SourcingAllowancePence,
		Expense: input.SourcingProbeExpensePence, UnknownCost: input.SourcingProbeCostsUnknown, Lanes: input.SourcingProbeLanes, Completions: input.SourcingProbeCompletions}
}

func (r *Runner) holdSourcingHistory(ctx context.Context, err error) {
	r.sourcingHistory.Error = "new sourcing admission blocked: sourcing history unavailable: " + err.Error()
	if holdErr := r.hold(ctx, "", r.sourcingHistory.Error); holdErr != nil {
		r.log.Warn("cannot record sourcing-history hold")
	}
}

func (r *Runner) applySourcingHistory(input *PlanningInput) {
	cached := &r.sourcingHistory
	input.SourcingHistoryError = cached.Error
	input.SourcingProbeHistory = cached.Outcomes
	if !cached.Ready && input.SourcingHistoryError == "" {
		input.SourcingHistoryError = "new sourcing admission blocked: sourcing history has not been captured"
	}
	if cached.Ready {
		input.SourcingAllowancePence = &cached.Allowance
		input.SourcingProbeExpensePence = cached.Expense
		input.SourcingProbeCostsUnknown = cached.UnknownCost
		input.SourcingProbeLanes = cached.Lanes
		input.SourcingProbeCompletions = cached.Completions
	}
	input.SourcingIdleEligible = r.shipIdleEligible(*input)
}
