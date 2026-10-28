package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

var receiptStages = []string{"loan-accept", "loan-repay", "marque", "underwrite", "buy", "sell", "provision", "accept-freight", "deliver-freight", "deliver-charter", "depart", "dock", "offshore", "hire-factor", "dismiss-factor", "settings", "remit", "sourcing-remit", "build", "refit"}

type SettlementCheck struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	CostsKnown bool   `json:"costs_known"`
	Deltas     Money  `json:"deltas"`
	Reason     string `json:"reason,omitempty"`
}

type OrderCheck struct {
	ActionID            string `json:"action_id"`
	PortID              string `json:"port_id"`
	ActualQuantity      int64  `json:"actual_quantity"`
	PredictedQuantity   int64  `json:"predicted_quantity"`
	ActualGrossPence    int64  `json:"actual_gross_pence"`
	PredictedGrossPence int64  `json:"predicted_gross_pence"`
}

type VoyageCheck struct {
	ActionID           string  `json:"action_id"`
	Hull               string  `json:"hull"`
	OutOfSample        bool    `json:"out_of_sample"`
	ActualGameHours    float64 `json:"actual_game_hours"`
	PredictedGameHours float64 `json:"predicted_game_hours"`
	AbsoluteError      float64 `json:"absolute_error"`
}

type ValidationReport struct {
	Trusted               bool                   `json:"trusted"`
	FailedRules           []string               `json:"failed_rules"`
	EvidenceIssues        []string               `json:"evidence_issues,omitempty"`
	Settlements           []SettlementCheck      `json:"settlements"`
	Orders                []OrderCheck           `json:"orders"`
	Voyages               []VoyageCheck          `json:"voyages"`
	CountsByKind          map[string]int         `json:"counts_by_kind"`
	UnlinkedActionsByPath map[string]int         `json:"unlinked_actions_by_path"`
	CoveredOrdersByPort   map[string]int         `json:"covered_orders_by_port"`
	Speeds                map[string]SpeedFactor `json:"speeds"`
	Coverage              []PortCoverage         `json:"coverage"`
}

func gateKind(kind string) bool { return kind == "spot" || kind == "charter" || kind == "relocation" }

func (r *ValidationReport) EvaluateGate() {
	r.FailedRules = append(make([]string, 0, len(r.EvidenceIssues)), r.EvidenceIssues...)
	eligible, linkable, known, exact := 0, 0, 0, 0
	for _, row := range r.Settlements {
		if !gateKind(row.Kind) {
			continue
		}
		eligible++
		if row.Status == "unlinkable" {
			continue
		}
		linkable++
		if row.CostsKnown {
			known++
			if row.Status == "exact" {
				exact++
			}
		}
	}
	if eligible == 0 || float64(linkable)/float64(eligible) < 0.8 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("settlement linkability: %d/%d; require at least 80%%", linkable, eligible))
	}
	if known == 0 || float64(exact)/float64(known) < 0.95 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("known-cost exactness: %d/%d; require at least 95%%", exact, known))
	}
	quantityExact, grossClose := 0, 0
	for _, order := range r.Orders {
		if order.ActualQuantity == order.PredictedQuantity {
			quantityExact++
		}
		errorPence := math.Abs(float64(order.PredictedGrossPence - order.ActualGrossPence))
		if errorPence <= float64(order.ActualGrossPence)*0.01 {
			grossClose++
		}
	}
	if len(r.Orders) < 10 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("covered orders: %d; require at least 10", len(r.Orders)))
	}
	if len(r.Orders) == 0 || float64(quantityExact)/float64(len(r.Orders)) < 0.9 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("exact filled tons: %d/%d; require at least 90%%", quantityExact, len(r.Orders)))
	}
	if len(r.Orders) == 0 || float64(grossClose)/float64(len(r.Orders)) < 0.9 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("gross error within 1%%: %d/%d; require at least 90%%", grossClose, len(r.Orders)))
	}
	var durationErrors []float64
	for _, voyage := range r.Voyages {
		if voyage.OutOfSample {
			durationErrors = append(durationErrors, voyage.AbsoluteError)
		}
	}
	if len(durationErrors) < 10 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("out-of-sample voyages: %d; require at least 10", len(durationErrors)))
	}
	if len(durationErrors) == 0 || percentile(durationErrors, 0.5) > 0.05 {
		r.FailedRules = append(r.FailedRules, fmt.Sprintf("median voyage duration error: %.2f%%; require at most 5%%", percentile(durationErrors, 0.5)*100))
	}
	r.Trusted = len(r.FailedRules) == 0
}

type ledgerEvidence struct {
	ID     string `json:"id"`
	Debit  string `json:"debit_account_id"`
	Credit string `json:"credit_account_id"`
	Amount int64  `json:"amount_pence"`
	Reason string `json:"reason"`
}

func validationAccounting(tape *Tape, before time.Time) (map[string]string, map[string]ledgerEvidence, map[string]bool, map[string]string, error) {
	owners := make(map[string]string)
	bills := make(map[string]ledgerEvidence)
	owned := make(map[string]bool)
	contracts := make(map[string]string)
	retained := 0
	reserve := func(change int) error {
		if change > maxTapeRetainedBytes-retained {
			return fmt.Errorf("accounting evidence exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
		}
		retained += change
		return nil
	}
	billBytes := func(row ledgerEvidence) int {
		return len(row.ID) + len(row.Debit) + len(row.Credit) + len(row.Reason) + 64
	}
	add := func(snapshot map[string]json.RawMessage) error {
		var allocations map[string]string
		_ = json.Unmarshal(snapshot["_expense_allocations"], &allocations)
		for id, owner := range allocations {
			change := len(owner) - len(owners[id])
			if _, exists := owners[id]; !exists {
				change += len(id) + 64
			}
			if err := reserve(change); err != nil {
				return err
			}
			owners[id] = owner
		}
		var ledger []ledgerEvidence
		_ = json.Unmarshal(snapshot["ledger"], &ledger)
		for _, row := range ledger {
			change := billBytes(row)
			if old, exists := bills[row.ID]; exists {
				change -= billBytes(old)
			}
			if err := reserve(change); err != nil {
				return err
			}
			bills[row.ID] = row
		}
		var vaults struct {
			Vaults []struct {
				AccountID string `json:"account_id"`
			} `json:"vaults"`
		}
		_ = json.Unmarshal(snapshot["vaults"], &vaults)
		for _, vault := range vaults.Vaults {
			if !owned[vault.AccountID] {
				if err := reserve(len(vault.AccountID) + 32); err != nil {
					return err
				}
				owned[vault.AccountID] = true
			}
		}
		var rows []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		_ = json.Unmarshal(snapshot["contracts"], &rows)
		for _, contract := range rows {
			change := len(contract.Status) - len(contracts[contract.ID])
			if _, exists := contracts[contract.ID]; !exists {
				change += len(contract.ID) + 64
			}
			if err := reserve(change); err != nil {
				return err
			}
			contracts[contract.ID] = contract.Status
		}
		return nil
	}
	err := tape.eachFrame(before, func(frame store.ReplayFrame) error {
		snapshot := make(map[string]json.RawMessage, 4)
		for _, key := range []string{"_expense_allocations", "ledger", "vaults", "contracts"} {
			snapshot[key] = frame.Datasets[key].Data
		}
		return add(snapshot)
	})
	if err == nil {
		err = add(tape.AccountingSnapshot)
	}
	return owners, bills, owned, contracts, err
}

func Validate(tape *Tape, from, to time.Time) (ValidationReport, error) {
	r := ValidationReport{Settlements: make([]SettlementCheck, 0), Orders: make([]OrderCheck, 0), Voyages: make([]VoyageCheck, 0), CountsByKind: make(map[string]int), UnlinkedActionsByPath: make(map[string]int), CoveredOrdersByPort: make(map[string]int)}
	actions := make(map[string]store.Action, len(tape.Actions))
	for _, action := range tape.Actions {
		actions[action.ID] = action
	}
	byMission := make(map[string][]store.ReplayActionLink)
	for _, link := range tape.Links {
		byMission[link.MissionID] = append(byMission[link.MissionID], link)
	}
	owners, bills, owned, contracts, err := validationAccounting(tape, to)
	if err != nil {
		return r, err
	}
	used := make(map[string]bool)
	rows := append([]store.Settlement(nil), tape.Settlements...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CompletedAt.Equal(rows[j].CompletedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CompletedAt.Before(rows[j].CompletedAt)
	})
	for _, settlement := range rows {
		if settlement.CompletedAt.Before(from) || !settlement.CompletedAt.Before(to) {
			continue
		}
		r.CountsByKind[settlement.Kind]++
		check := SettlementCheck{ID: settlement.ID, Kind: settlement.Kind, CostsKnown: settlement.CostsKnown, Status: "exact"}
		links := append([]store.ReplayActionLink(nil), byMission[settlement.MissionID]...)
		linkedIDs := make(map[string]bool)
		for _, link := range links {
			linkedIDs[link.ActionID] = true
		}
		for _, stage := range receiptStages {
			for sequence := int64(0); sequence < 512; sequence++ {
				id := engine.ActionID(settlement.AccountID, settlement.MissionID, stage, sequence)
				if _, exists := actions[id]; exists && !linkedIDs[id] {
					links = append(links, store.ReplayActionLink{ActionID: id, MissionID: settlement.MissionID, Stage: stage, Sequence: sequence})
					linkedIDs[id] = true
				}
			}
		}
		sort.Slice(links, func(i, j int) bool {
			if links[i].Sequence == links[j].Sequence {
				return links[i].ActionID < links[j].ActionID
			}
			return links[i].Sequence < links[j].Sequence
		})
		var rebuilt Money
		evidenced := make(map[string]bool)
		if _, ok := actions[settlement.ActionID]; !ok || len(links) == 0 {
			check.Status, check.Reason = "unlinkable", "terminal receipt or mission links missing"
		}
		for _, link := range links {
			action, exists := actions[link.ActionID]
			if !exists || len(action.Response) == 0 || action.State == store.ActionPending {
				check.Status, check.Reason = "unlinkable", "linked receipt missing"
				continue
			}
			used[action.ID] = true
			if action.Status < 200 || action.Status >= 300 {
				continue
			}
			if err := ApplyReceipt(&rebuilt, link.Stage, action.Response); err != nil {
				check.Status, check.Reason = "unlinkable", err.Error()
			}
			evidenced[link.Stage] = true
			if link.Stage == "accept-freight" {
				var receipt struct {
					ContractID string `json:"contract_id"`
					Collateral int64  `json:"collateral_pence"`
				}
				_ = json.Unmarshal(action.Response, &receipt)
				if contracts[receipt.ContractID] == "defaulted" {
					rebuilt.ExpensesPence += receipt.Collateral
					evidenced["bills"] = true
				}
			}
		}
		for id, owner := range owners {
			if owner != settlement.MissionID && owner != settlement.ID {
				continue
			}
			bill, exists := bills[id]
			if !exists {
				check.Status, check.Reason = "unlinkable", "allocated ledger bill missing"
				continue
			}
			if len(owned) != 0 && (!owned[bill.Debit] || owned[bill.Credit]) {
				continue
			}
			for _, reason := range []string{"upkeep", "rent", "berth", "toll"} {
				if strings.Contains(bill.Reason, reason) {
					rebuilt.ExpensesPence += bill.Amount
					evidenced["bills"] = true
					break
				}
			}
		}
		if settlement.AcquisitionPence > 0 && !evidenced["buy"] ||
			settlement.ProceedsPence > 0 && !evidenced["sell"] && !evidenced["deliver-charter"] && !evidenced["deliver-freight"] ||
			settlement.ProvisionsPence > 0 && !evidenced["provision"] ||
			settlement.ExpensesPence > 0 && !evidenced["bills"] {
			check.Status, check.Reason = "unlinkable", "cost or proceeds field has no surviving receipt or allocated bill"
		}
		check.Deltas = rebuilt.minus(settlementMoney(settlement))
		if check.Status != "unlinkable" && check.Deltas != (Money{}) {
			check.Status = "mismatch"
		}
		r.Settlements = append(r.Settlements, check)
	}
	for _, action := range tape.Actions {
		if !used[action.ID] && !action.CreatedAt.Before(from) && action.CreatedAt.Before(to) {
			r.UnlinkedActionsByPath[action.Path]++
		}
	}
	orders, err := validateOrders(tape, from, to)
	if err != nil {
		return r, err
	}
	r.Orders = orders
	for _, order := range orders {
		r.CoveredOrdersByPort[order.PortID]++
	}
	fitUntil := from.Add(to.Sub(from) / 2)
	observations, err := voyageObservations(tape, from, to)
	if err != nil {
		return r, err
	}
	r.Speeds = fitSpeedFactors(observations, fitUntil)
	for _, observation := range observations {
		factor := r.Speeds[observation.hull].AtLoad(observation.cargo, observation.capacity)
		predicted := observation.baseHours / factor
		r.Voyages = append(r.Voyages, VoyageCheck{ActionID: observation.actionID, Hull: observation.hull, OutOfSample: !observation.at.Before(fitUntil), ActualGameHours: observation.hours, PredictedGameHours: predicted, AbsoluteError: math.Abs(predicted/observation.hours - 1)})
	}
	r.Coverage, r.EvidenceIssues, err = tape.coverage(from, to)
	if err != nil {
		return r, err
	}
	r.EvaluateGate()
	return r, nil
}

func validateOrders(tape *Tape, from, to time.Time) ([]OrderCheck, error) {
	actions := append([]store.Action(nil), tape.Actions...)
	sort.Slice(actions, func(i, j int) bool {
		if actions[i].CreatedAt.Equal(actions[j].CreatedAt) {
			return actions[i].ID < actions[j].ID
		}
		return actions[i].CreatedAt.Before(actions[j].CreatedAt)
	})
	stages := make(map[string]string)
	for _, link := range tape.Links {
		stages[link.ActionID] = link.Stage
	}
	reader := tape.Reader()
	market := NewMarketModel(1)
	orders := make([]OrderCheck, 0)
	for _, action := range actions {
		if action.Status < 200 || action.Status >= 300 || !action.CreatedAt.Before(to) {
			continue
		}
		var request struct {
			Port      string `json:"port_id"`
			Commodity string `json:"commodity_id"`
			Side      string `json:"side"`
			Quantity  int64  `json:"quantity"`
			Price     int64  `json:"price_pence"`
		}
		var receipt struct {
			Filled *int64 `json:"filled_quantity"`
			Gross  *int64 `json:"total_gross_pence"`
		}
		if json.Unmarshal(action.Body, &request) != nil || json.Unmarshal(action.Response, &receipt) != nil || receipt.Filled == nil || receipt.Gross == nil || request.Port == "" {
			continue
		}
		if request.Side == "" {
			request.Side = stages[action.ID]
			if request.Side == "provision" {
				request.Side = "buy"
			}
		}
		request.Side = tradeSide(request.Side)
		if request.Side != "buy" && request.Side != "sell" {
			continue
		}
		view, err := reader.Advance(action.CreatedAt)
		if err != nil {
			return nil, err
		}
		market.Observe(view)
		observation := view.Books[request.Port]
		if observation.Status != Observed || observation.AsOf.After(action.CreatedAt) || action.CreatedAt.Sub(observation.AsOf) > 2*time.Minute {
			continue
		}
		predicted, err := market.Execute(request.Port, request.Commodity, request.Side, request.Quantity, request.Price)
		if err != nil {
			continue
		}
		if action.CreatedAt.Before(from) {
			continue
		}
		orders = append(orders, OrderCheck{ActionID: action.ID, PortID: request.Port, ActualQuantity: *receipt.Filled, PredictedQuantity: predicted.Quantity, ActualGrossPence: *receipt.Gross, PredictedGrossPence: predicted.GrossPence})
	}
	return orders, nil
}

func (r ValidationReport) WriteTo(writer io.Writer) (int64, error) {
	var text strings.Builder
	label := "TRUSTED"
	if !r.Trusted {
		label = "UNTRUSTED"
	}
	fmt.Fprintf(&text, "Validation gate: %s\n", label)
	for _, failure := range r.FailedRules {
		fmt.Fprintf(&text, "FAIL %s\n", failure)
	}
	for _, kind := range sortedKeys(r.CountsByKind) {
		fmt.Fprintf(&text, "Settlements %-18s %d\n", kind, r.CountsByKind[kind])
	}
	for _, check := range r.Settlements {
		if check.Status != "exact" {
			fmt.Fprintf(&text, "%s %s %s deltas=%+v %s\n", check.Status, check.ID, check.Kind, check.Deltas, check.Reason)
		}
	}
	for _, port := range sortedKeys(r.CoveredOrdersByPort) {
		fmt.Fprintf(&text, "Covered orders %-24s %d\n", port, r.CoveredOrdersByPort[port])
	}
	for _, hull := range sortedKeys(r.Speeds) {
		speed := r.Speeds[hull]
		fmt.Fprintf(&text, "Speed %-16s empty=%.4f laden=%.4f n=%d\n", hull, speed.Empty, speed.Laden, speed.N)
	}
	for _, coverage := range r.Coverage {
		fmt.Fprintf(&text, "Coverage %-24s observed=%.3fh carried=%.3fh dark=%.3fh\n", coverage.Port, coverage.ObservedHours, coverage.CarriedHours, coverage.DarkHours)
	}
	for _, path := range sortedKeys(r.UnlinkedActionsByPath) {
		fmt.Fprintf(&text, "Unlinked actions %s %d\n", path, r.UnlinkedActionsByPath[path])
	}
	n, err := io.WriteString(writer, text.String())
	return int64(n), err
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
