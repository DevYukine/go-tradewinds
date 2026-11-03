package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// companyLedgerFlow reports the signed amount of a ledger entry that real net
// profit must add on top of settlement net. Settlements keep totals rather than
// ledger IDs for remittance fees, voyage insurance and default seizures, so
// those reasons stay with settlements wholesale instead of counting twice.
func companyLedgerFlow(entry ledgerBill, owned map[string]bool, claimed map[string]string) (int64, bool) {
	if entry.ID == "" || entry.Reason == "" || entry.Amount <= 0 || entry.CreatedAt.IsZero() || claimed[entry.ID] != "" ||
		owned[entry.Debit] == owned[entry.Credit] {
		return 0, false
	}
	// Futures and underwriting results are reported outside this series. Their
	// ledger entries carry the position identity; reason names are a fallback.
	if entry.Metadata.FuturesID != "" || entry.Metadata.UnderwritingID != "" {
		return 0, false
	}
	for _, family := range []string{"principal", "disbursement", "escrow", "collateral_release", "refund", "future", "underwrit"} {
		if strings.Contains(entry.Reason, family) {
			return 0, false
		}
	}
	switch entry.Reason {
	case "spot_market_buy", "spot_market_sell", "port_customs_duty",
		"royal_charter_payout", "admiralty_charter_payout", "freight_contract_reward_payout", "passenger_fare_payout",
		"ach_remittance_fee", "freight_contract_default_collateral_seizure",
		"insurance_premium_payout", "insurance_claim_payout":
		return 0, false
	case "bank_draft", "seed_capital", "initial_capital",
		"shipyard_construction", "shipyard_turnkey_markup", "ship_cannon_fitting",
		"ship_purchase", "ship_sale", "hull_purchase", "hull_sale":
		return 0, false
	}
	if strings.HasPrefix(entry.Reason, "admiralty_commission_") || strings.HasPrefix(entry.Reason, "commission_") ||
		strings.Contains(entry.Reason, "provision") {
		return 0, false
	}
	if owned[entry.Debit] {
		return -entry.Amount, true
	}
	return entry.Amount, true
}

// recordCompanyFlows writes only classification changes. The first pass after
// start syncs the whole retained ledger, which is the backfill. The caller
// supplies the same fresh ledger and owned accounts as recordBalances.
func (r *Runner) recordCompanyFlows(ctx context.Context, ledger []ledgerBill, owned map[string]bool) error {
	first := r.companyFlowsRecorded == nil
	if first {
		// A crash can leave newer bill claims in durable missions than in the
		// snapshot. Mission recovery owns merging them into expenseOwners (with
		// its conflict check); until it does, count them as claimed here so
		// backfill cannot republish a settled bill.
		missions, err := r.db.Missions(ctx, r.account.ID)
		if err != nil {
			return err
		}
		r.companyFlowsMissionClaims = make(map[string]string)
		for _, mission := range missions {
			var data missionData
			if err := json.Unmarshal(mission.Data, &data); err != nil {
				return err
			}
			for _, id := range data.SettlementExpenseIDs {
				r.companyFlowsMissionClaims[id] = mission.Generation
			}
		}
	}
	claimed := r.expenseOwners
	for id := range r.companyFlowsMissionClaims {
		if r.expenseOwners[id] != "" {
			delete(r.companyFlowsMissionClaims, id)
		}
	}
	if len(r.companyFlowsMissionClaims) > 0 {
		claimed = make(map[string]string, len(r.expenseOwners)+len(r.companyFlowsMissionClaims))
		for id, owner := range r.expenseOwners {
			claimed[id] = owner
		}
		for id, owner := range r.companyFlowsMissionClaims {
			claimed[id] = owner
		}
	}
	var additions []store.CompanyLedgerFlow
	var removals []string
	changes := make(map[string]bool)
	inLedger := make(map[string]bool, len(ledger))
	for _, entry := range ledger {
		if entry.ID == "" {
			continue
		}
		inLedger[entry.ID] = true
		amount, include := companyLedgerFlow(entry, owned, claimed)
		recorded, seen := r.companyFlowsRecorded[entry.ID]
		if seen && recorded == include {
			continue
		}
		changes[entry.ID] = include
		if include {
			additions = append(additions, store.CompanyLedgerFlow{
				LedgerID: entry.ID, Reason: entry.Reason, AmountPence: amount, CreatedAt: entry.CreatedAt,
			})
		} else if first || recorded || claimed[entry.ID] != "" {
			removals = append(removals, entry.ID)
		}
	}
	// A claim can name a bill that has already left the ledger window but was
	// recorded before; delete each newly seen claim once.
	for id := range claimed {
		if _, changed := changes[id]; changed {
			continue
		}
		if recorded, seen := r.companyFlowsRecorded[id]; !seen || recorded {
			removals = append(removals, id)
			changes[id] = false
		}
	}
	if err := r.db.SyncCompanyFlows(ctx, r.account.ID, additions, removals); err != nil {
		return err
	}
	if first {
		r.companyFlowsRecorded = make(map[string]bool, len(changes))
	}
	for id, recorded := range changes {
		r.companyFlowsRecorded[id] = recorded
	}
	// Entries that left the window cannot return; claims stay so each one is
	// deleted only once.
	for id := range r.companyFlowsRecorded {
		if !inLedger[id] && claimed[id] == "" {
			delete(r.companyFlowsRecorded, id)
		}
	}
	return nil
}

// recordCompanyRemittanceFee records the fee of a treasury or loan transfer,
// which no settlement carries. Ledger fees stay excluded because ship-mission
// fees are inside settlements without ledger IDs; the action ID keys this row.
func (r *Runner) recordCompanyRemittanceFee(ctx context.Context, action *durableAction, fee int64) error {
	if fee <= 0 {
		return nil
	}
	at := action.ReceiptAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return r.db.SyncCompanyFlows(ctx, r.account.ID, []store.CompanyLedgerFlow{{
		LedgerID: "remit-action:" + action.ID, Reason: "ach_remittance_fee", AmountPence: -fee, CreatedAt: at,
	}}, nil)
}
