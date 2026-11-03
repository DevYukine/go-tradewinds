package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type loanHistoryRow struct {
	AccountID       string          `gorm:"primaryKey"`
	LoanID          string          `gorm:"primaryKey"`
	Data            json.RawMessage `gorm:"type:jsonb"`
	Status          string
	ClosedAt        *time.Time
	ObservedAt      time.Time
	SourceUpdatedAt *time.Time
}

func (loanHistoryRow) TableName() string { return "loan_history" }

// SyncLoanHistory retains every observed row, including closed loans. Absence
// from a later active-only capture is not evidence of repayment or deletion.
func (s *Store) SyncLoanHistory(ctx context.Context, accountID string, rows []json.RawMessage, observedAt time.Time) error {
	if !validID(accountID) || observedAt.IsZero() {
		return fmt.Errorf("loan history observation: %w", ErrInvalidInput)
	}
	seen := make(map[string]bool, len(rows))
	loans := make([]gameapi.Loan, len(rows))
	remainingBytes := MaxSnapshotBytes
	for index, raw := range rows {
		if len(raw) > remainingBytes {
			return ErrResponseTooLarge
		}
		remainingBytes -= len(raw)
		if err := ValidateSnapshotRouteNumbers(raw); err != nil {
			return err
		}
		if err := json.Unmarshal(raw, &loans[index]); err != nil {
			return fmt.Errorf("loan history row: %w", ErrInvalidInput)
		}
		loan := loans[index]
		if !validID(loan.ID) || loan.BorrowerCompanyID != "" && !validID(loan.BorrowerCompanyID) || !validID(loan.Status) || seen[loan.ID] {
			return fmt.Errorf("loan history identity: %w", ErrInvalidInput)
		}
		if loan.PrincipalRepaidPence < 0 || loan.InterestPaidPence < 0 {
			return fmt.Errorf("loan history payment counters: %w", ErrInvalidInput)
		}
		seen[loan.ID] = true
	}
	if len(rows) == 0 {
		return nil
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	records := make([]loanHistoryRow, len(rows))
	remainingBytes = MaxSnapshotBytes
	for index, raw := range rows {
		clean, err := redactJSON(raw, secrets, 0)
		if err != nil {
			return err
		}
		if len(clean) > remainingBytes {
			return ErrResponseTooLarge
		}
		remainingBytes -= len(clean)
		if err := ValidateSnapshotRouteNumbers(clean); err != nil {
			return err
		}
		loan := loans[index]
		if scrubText(loan.ID, secrets) != loan.ID || scrubText(loan.BorrowerCompanyID, secrets) != loan.BorrowerCompanyID || scrubText(loan.Status, secrets) != loan.Status {
			return fmt.Errorf("loan history private identity: %w", ErrInvalidInput)
		}
		records[index] = loanHistoryRow{
			AccountID: accountID, LoanID: loan.ID, Data: clean, Status: loan.Status,
			ClosedAt: loan.ClosedAt, ObservedAt: observedAt.UTC(), SourceUpdatedAt: loanTime(loan.UpdatedAt),
		}
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var account Account
		// Serialize this account's retained-size decisions before inserting rows.
		if err := tx.Clauses(clause.Locking{Strength: "NO KEY UPDATE"}).First(&account, "id = ?", accountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		for _, loan := range loans {
			if loan.BorrowerCompanyID != "" && loan.BorrowerCompanyID != account.CompanyID {
				return fmt.Errorf("loan history borrower: %w", ErrInvalidInput)
			}
		}
		if err := validateLoanHistoryReadback(tx, records); err != nil {
			return err
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}, {Name: "loan_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"data", "status", "closed_at", "observed_at", "source_updated_at"}),
			Where: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: `
				EXCLUDED.observed_at >= loan_history.observed_at
				AND (loan_history.source_updated_at IS NULL
					OR EXCLUDED.source_updated_at >= loan_history.source_updated_at)`}}},
		}).CreateInBatches(&records, 200).Error; err != nil {
			return err
		}
		remainingBytes, err := preflightLoanReport(tx, []string{accountID})
		if err != nil {
			return err
		}
		// The reader charges escaped lender names separately from recorded JSON.
		// Count snapshot-only entries too; duplicates only make this conservative.
		var lenderCount int64
		if err := tx.Raw(`SELECT
			(SELECT count(*) FROM loan_history WHERE account_id = ?) +
			COALESCE((SELECT CASE WHEN jsonb_typeof(data -> 'loans') = 'array'
				THEN jsonb_array_length(data -> 'loans') ELSE 0 END
				FROM snapshots WHERE account_id = ?), 0)`,
			accountID, accountID).Scan(&lenderCount).Error; err != nil {
			return err
		}
		if lenderCount > remainingBytes/(maxLoanLenderNameBytes*6) {
			return ErrResponseTooLarge
		}
		return nil
	})
}

func validateLoanHistoryReadback(tx *gorm.DB, records []loanHistoryRow) error {
	remainingExpanded, remainingCanonical := int64(MaxSnapshotBytes), MaxSnapshotBytes
	batch := make([]json.RawMessage, min(200, len(records)))
	for start := 0; start < len(records); start += len(batch) {
		batch = batch[:min(len(batch), len(records)-start)]
		for index := range batch {
			batch[index] = records[start+index].Data
		}
		encoded, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("encode loan history batch: %w", err)
		}
		var expanded []struct {
			Data          json.RawMessage
			ExpandedBytes int64
		}
		// The numeric-width guard has already run on these redacted rows.
		// Inspect JSONB's actual decimal rendering before hydrating it into Go.
		if err := tx.Raw(`WITH incoming AS MATERIALIZED (
			SELECT value AS data FROM jsonb_array_elements(?::jsonb)
		), sizes AS MATERIALIZED (
			SELECT COALESCE(sum(octet_length(data::text)::bigint), 0) AS expanded_bytes
			FROM incoming
		) SELECT expanded_bytes, CASE WHEN expanded_bytes <= ? THEN data END AS data
			FROM incoming CROSS JOIN sizes`,
			string(encoded), remainingExpanded).Scan(&expanded).Error; err != nil {
			return err
		}
		for index, row := range expanded {
			if row.ExpandedBytes > remainingExpanded {
				return ErrResponseTooLarge
			}
			decoder := json.NewDecoder(bytes.NewReader(row.Data))
			decoder.UseNumber()
			var value any
			if err := decoder.Decode(&value); err != nil {
				return fmt.Errorf("decode expanded loan history: %w", err)
			}
			canonical, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("encode expanded loan history: %w", err)
			}
			// HTML and U+2028/U+2029 escaping can outgrow PostgreSQL text.
			if len(canonical) > remainingCanonical {
				return ErrResponseTooLarge
			}
			remainingCanonical -= len(canonical)
			if index == len(expanded)-1 {
				remainingExpanded -= row.ExpandedBytes
			}
		}
	}
	return nil
}

func loanTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}

func loanFundedStatus(status string) bool {
	switch status {
	case "active", "defaulted", "repaid", "recovered", "written_off":
		return true
	default:
		return false
	}
}

func loanUnfundedStatus(status string) bool {
	switch status {
	case "offered", "withdrawn", "declined", "lapsed":
		return true
	default:
		return false
	}
}

func loanOpenStatus(status string) bool {
	return status == "active" || status == "defaulted"
}

func loanObservationNewer(incoming, previous gameapi.Loan, observedAt, previousObservedAt time.Time) bool {
	if observedAt.Before(previousObservedAt) {
		return false
	}
	return previous.UpdatedAt.IsZero() || !incoming.UpdatedAt.IsZero() && !incoming.UpdatedAt.Before(previous.UpdatedAt)
}
