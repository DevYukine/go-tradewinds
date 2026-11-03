package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type CompanyLedgerFlow struct {
	LedgerID    string    `json:"ledger_id"`
	Reason      string    `json:"reason"`
	AmountPence int64     `json:"amount_pence"`
	CreatedAt   time.Time `json:"created_at"`
}

// SyncCompanyFlows applies only observed additions and reclassifications. An
// entry leaving the retained game ledger never removes its permanent history.
func (s *Store) SyncCompanyFlows(ctx context.Context, accountID string, flows []CompanyLedgerFlow, removedIDs []string) error {
	if !validID(accountID) {
		return fmt.Errorf("company flow account: %w", ErrInvalidInput)
	}
	for _, flow := range flows {
		if !validID(flow.LedgerID) || !validID(flow.Reason) || flow.AmountPence == 0 || flow.CreatedAt.IsZero() {
			return fmt.Errorf("company flow fields: %w", ErrInvalidInput)
		}
	}
	for _, id := range removedIDs {
		if !validID(id) {
			return fmt.Errorf("company flow removal: %w", ErrInvalidInput)
		}
	}
	if len(flows) == 0 && len(removedIDs) == 0 {
		return nil
	}
	if flows == nil {
		flows = make([]CompanyLedgerFlow, 0)
	}
	if removedIDs == nil {
		removedIDs = make([]string, 0)
	}
	additions, err := json.Marshal(flows)
	if err != nil {
		return err
	}
	removals, err := json.Marshal(removedIDs)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Exec(`WITH removed AS (
				DELETE FROM company_ledger_flows
				WHERE account_id = ? AND ledger_id IN (SELECT jsonb_array_elements_text(?::jsonb))
			)
			INSERT INTO company_ledger_flows (account_id, ledger_id, reason, amount_pence, created_at)
			SELECT ?, ledger_id, reason, amount_pence, created_at
			FROM jsonb_to_recordset(?::jsonb) AS entries (
				ledger_id text, reason text, amount_pence bigint, created_at timestamptz
			)
			ON CONFLICT (account_id, ledger_id) DO NOTHING`, accountID, string(removals), accountID, string(additions)).Error
	})
}
