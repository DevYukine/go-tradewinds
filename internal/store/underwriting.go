package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UnderwritingPolicyRecord keeps the first terms fingerprint even when a later
// public observation changes the policy. TermsChanged is permanent censorship.
type UnderwritingPolicyRecord struct {
	AccountID       string          `json:"account_id" gorm:"primaryKey"`
	PolicyID        string          `json:"policy_id" gorm:"primaryKey"`
	TermsHash       string          `json:"terms_hash"`
	Status          string          `json:"status"`
	Payload         json.RawMessage `json:"payload" gorm:"type:jsonb"`
	SourceUpdatedAt time.Time       `json:"source_updated_at"`
	ObservedAt      time.Time       `json:"observed_at"`
	TermsChanged    bool            `json:"terms_changed"`
}

func (UnderwritingPolicyRecord) TableName() string { return "underwriting_policies" }

// Closure pointers distinguish unknown cash accounting from an observed zero.
// Returned collateral is principal, never premium income or realized profit.
type UnderwritingPosition struct {
	ID                      string          `json:"id" gorm:"primaryKey"`
	AccountID               string          `json:"account_id" gorm:"primaryKey"`
	ActionID                string          `json:"action_id"`
	PolicyID                string          `json:"policy_id"`
	CompanyID               string          `json:"company_id"`
	SliceBPS                int64           `json:"slice_bps"`
	CollateralPence         int64           `json:"collateral_pence"`
	PremiumPence            int64           `json:"premium_pence"`
	FrozenPolicy            json.RawMessage `json:"frozen_policy" gorm:"type:jsonb"`
	FundingEvidence         json.RawMessage `json:"funding_evidence" gorm:"type:jsonb"`
	AcceptedAt              time.Time       `json:"accepted_at"`
	Status                  string          `json:"status"`
	EarnedPremiumPence      *int64          `json:"earned_premium_pence"`
	ClaimsPence             *int64          `json:"claims_pence"`
	ReturnedCollateralPence *int64          `json:"returned_collateral_pence"`
	FeesPence               *int64          `json:"fees_pence"`
	NetPence                *int64          `json:"net_pence"`
	ClosedAt                *time.Time      `json:"closed_at"`
}

func (UnderwritingPosition) TableName() string { return "underwriting_positions" }

type UnderwritingClosure struct {
	AccountID               string          `json:"account_id"`
	ID                      string          `json:"id"`
	Status                  string          `json:"status"`
	EarnedPremiumPence      int64           `json:"earned_premium_pence"`
	ClaimsPence             int64           `json:"claims_pence"`
	ReturnedCollateralPence int64           `json:"returned_collateral_pence"`
	FeesPence               int64           `json:"fees_pence"`
	ClosedAt                time.Time       `json:"closed_at"`
	Evidence                json.RawMessage `json:"evidence"`
}

type UnderwritingRisk struct {
	InitialPence      int64 `json:"initial_pence"`
	OutstandingPence  int64 `json:"outstanding_pence"`
	RealizedNetPence  int64 `json:"realized_net_pence"`
	RealizedLossPence int64 `json:"realized_loss_pence"`
	RemainingPence    int64 `json:"remaining_pence"`
}

// Only documented public policy fields enter permanent encrypted evidence.
// Numeric pointers also reject omitted/null money in an HTTP receipt.
type underwritingPolicyEvidence struct {
	ID                string     `json:"id"`
	InsuredCompanyID  string     `json:"insured_company_id"`
	ShipID            string     `json:"ship_id"`
	OriginPortID      string     `json:"origin_port_id"`
	DestPortID        string     `json:"dest_port_id"`
	InsuredValuePence *int64     `json:"insured_value_pence"`
	HullValuePence    *int64     `json:"hull_value_pence"`
	PremiumBPS        *int64     `json:"premium_bps"`
	TotalPremiumPence *int64     `json:"total_premium_pence"`
	CoveredBPS        *int64     `json:"covered_bps"`
	Status            string     `json:"status"`
	CreatedAt         *time.Time `json:"created_at,omitempty"`
	UpdatedAt         *time.Time `json:"updated_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

const maxUnderwritingPence int64 = 1_000_000_000_000

func decodeUnderwritingPolicy(raw json.RawMessage) (underwritingPolicyEvidence, error) {
	var policy underwritingPolicyEvidence
	if err := json.Unmarshal(raw, &policy); err != nil {
		return policy, fmt.Errorf("underwriting policy JSON: %w", ErrInvalidInput)
	}
	if !validID(policy.ID) || !validID(policy.InsuredCompanyID) || !validID(policy.ShipID) ||
		!validID(policy.OriginPortID) || !validID(policy.DestPortID) || policyClassification(policy.Status) == "unknown" ||
		policy.InsuredValuePence == nil || *policy.InsuredValuePence <= 0 || *policy.InsuredValuePence > maxUnderwritingPence ||
		policy.HullValuePence == nil || *policy.HullValuePence <= 0 || *policy.HullValuePence > maxUnderwritingPence ||
		policy.PremiumBPS == nil || *policy.PremiumBPS < 0 || *policy.PremiumBPS > 10000 ||
		policy.TotalPremiumPence == nil || *policy.TotalPremiumPence < 0 || *policy.TotalPremiumPence > maxUnderwritingPence ||
		policy.CoveredBPS == nil || *policy.CoveredBPS < 0 || *policy.CoveredBPS > 10000 {
		return policy, fmt.Errorf("underwriting policy fields: %w", ErrInvalidInput)
	}
	return policy, nil
}

func sameUnderwritingTerms(a, b underwritingPolicyEvidence) bool {
	return a.ID == b.ID && a.InsuredCompanyID == b.InsuredCompanyID && a.ShipID == b.ShipID &&
		a.OriginPortID == b.OriginPortID && a.DestPortID == b.DestPortID &&
		*a.InsuredValuePence == *b.InsuredValuePence && *a.HullValuePence == *b.HullValuePence &&
		*a.PremiumBPS == *b.PremiumBPS && *a.TotalPremiumPence == *b.TotalPremiumPence &&
		sameUnderwritingTime(a.CreatedAt, b.CreatedAt) && sameUnderwritingTime(a.ExpiresAt, b.ExpiresAt)
}

func sameUnderwritingTime(a, b *time.Time) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Equal(*b)
}

func terminalUnderwritingPolicy(status string) bool {
	return status == "settled_safe" || status == "claimed_loss" || status == "cancelled"
}

func (s *Store) PutUnderwritingPolicies(ctx context.Context, accountID string, records []UnderwritingPolicyRecord) error {
	if !validID(accountID) {
		return fmt.Errorf("underwriting policy account: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		for _, record := range records {
			if record.AccountID != accountID || !validID(record.PolicyID) || !validID(record.TermsHash) ||
				record.SourceUpdatedAt.IsZero() || record.ObservedAt.IsZero() {
				return fmt.Errorf("underwriting policy observation: %w", ErrInvalidInput)
			}
			policy, err := decodeUnderwritingPolicy(record.Payload)
			if err != nil {
				return err
			}
			if policy.ID != record.PolicyID || policy.Status != record.Status {
				return fmt.Errorf("underwriting policy identity: %w", ErrInvalidInput)
			}
			record.Payload, err = json.Marshal(policy)
			if err != nil {
				return err
			}
			record.SourceUpdatedAt = record.SourceUpdatedAt.UTC().Truncate(time.Microsecond)
			record.ObservedAt = record.ObservedAt.UTC().Truncate(time.Microsecond)
			privateRow, err := s.encodeUnderwritingPolicy(record)
			if err != nil {
				return err
			}
			result := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "account_id"}, {Name: "policy_lookup"}}, DoNothing: true,
			}).Create(&privateRow)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 0 {
				continue
			}
			var stored underwritingPolicyRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("account_id = ? AND policy_lookup = ?", accountID, privateRow.PolicyLookup).Take(&stored).Error; err != nil {
				return err
			}
			persisted, err := s.decodeUnderwritingPolicy(stored)
			if err != nil {
				return err
			}
			if persisted.PolicyID != record.PolicyID {
				return ErrActionConflict
			}
			previous, err := decodeUnderwritingPolicy(persisted.Payload)
			if err != nil {
				return err
			}
			previousTerminal := terminalUnderwritingPolicy(persisted.Status)
			incomingTerminal := terminalUnderwritingPolicy(record.Status)
			changed := persisted.TermsChanged || record.TermsChanged || persisted.TermsHash != record.TermsHash ||
				!sameUnderwritingTerms(previous, policy) ||
				previousTerminal && incomingTerminal && persisted.Status != record.Status ||
				previousTerminal && persisted.Status != record.Status && !record.SourceUpdatedAt.Before(persisted.SourceUpdatedAt) ||
				incomingTerminal && !previousTerminal && record.SourceUpdatedAt.Before(persisted.SourceUpdatedAt)
			observedAt := persisted.ObservedAt
			if record.ObservedAt.After(observedAt) {
				observedAt = record.ObservedAt
			}
			updates := map[string]any{"terms_changed": changed, "observed_at": observedAt}
			if !record.SourceUpdatedAt.Before(persisted.SourceUpdatedAt) &&
				(!previousTerminal || incomingTerminal) &&
				(!record.SourceUpdatedAt.Equal(persisted.SourceUpdatedAt) || !record.ObservedAt.Before(persisted.ObservedAt)) {
				updates["status"] = privateRow.Status
				updates["payload_cipher"] = privateRow.PayloadCipher
				updates["source_updated_at"] = record.SourceUpdatedAt
			}
			if err := tx.Model(&underwritingPolicyRow{}).
				Where("account_id = ? AND policy_lookup = ?", accountID, privateRow.PolicyLookup).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) UnderwritingPolicies(ctx context.Context, accountID string) ([]UnderwritingPolicyRecord, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("underwriting policy account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	stored := make([]underwritingPolicyRow, 0)
	if err := db.Where("account_id = ?", accountID).Find(&stored).Error; err != nil {
		return nil, err
	}
	rows := make([]UnderwritingPolicyRecord, 0, len(stored))
	for _, record := range stored {
		value, err := s.decodeUnderwritingPolicy(record)
		if err != nil {
			return nil, err
		}
		rows = append(rows, value)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].PolicyID < rows[j].PolicyID
	})
	return rows, nil
}

func (s *Store) PutUnderwritingPosition(ctx context.Context, position *UnderwritingPosition) error {
	if position == nil || !validID(position.AccountID) || !validID(position.ID) || !validID(position.ActionID) ||
		!validID(position.PolicyID) || !validID(position.CompanyID) || position.SliceBPS != 500 ||
		position.CollateralPence <= 0 || position.CollateralPence > maxUnderwritingPence ||
		position.PremiumPence < 0 || position.PremiumPence > maxUnderwritingPence || position.AcceptedAt.IsZero() ||
		position.Status != "active" || position.EarnedPremiumPence != nil || position.ClaimsPence != nil ||
		position.ReturnedCollateralPence != nil || position.FeesPence != nil || position.NetPence != nil || position.ClosedAt != nil {
		return fmt.Errorf("underwriting position fields: %w", ErrInvalidInput)
	}
	row := *position
	// Funding proof is recorded independently, never inferred from acceptance.
	row.FundingEvidence = nil
	frozen, err := decodeUnderwritingPolicy(row.FrozenPolicy)
	if err != nil {
		return err
	}
	if frozen.ID != row.PolicyID || frozen.InsuredCompanyID == row.CompanyID || *frozen.InsuredValuePence > *frozen.HullValuePence {
		return fmt.Errorf("underwriting frozen policy: %w", ErrInvalidInput)
	}
	row.FrozenPolicy, err = json.Marshal(frozen)
	if err != nil {
		return err
	}
	row.AcceptedAt = row.AcceptedAt.UTC().Truncate(time.Microsecond)
	return s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		var action actionRow
		if err := tx.Where("id = ? AND account_id = ?", row.ActionID, row.AccountID).Take(&action).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if action.State != ActionComplete || action.Status != http.StatusOK || action.Method != http.MethodPost ||
			action.Path != "/v1/insurance/underwrite" || action.CompanyID != row.CompanyID {
			return fmt.Errorf("underwriting action evidence: %w", ErrActionConflict)
		}
		decoded, err := s.decodeAction(action)
		if err != nil {
			return err
		}
		var request struct {
			PolicyID string `json:"policy_id"`
			SliceBPS *int64 `json:"slice_bps"`
		}
		var response struct {
			Policy        json.RawMessage `json:"policy"`
			Underwritings []struct {
				ID              string `json:"id"`
				PolicyID        string `json:"policy_id"`
				CompanyID       string `json:"underwriter_company_id"`
				SliceBPS        *int64 `json:"slice_bps"`
				CollateralPence *int64 `json:"collateral_locked_pence"`
				PremiumPence    *int64 `json:"premium_share_pence"`
			} `json:"underwritings"`
		}
		if json.Unmarshal(decoded.Body, &request) != nil || json.Unmarshal(decoded.Response, &response) != nil ||
			request.PolicyID != row.PolicyID || request.SliceBPS == nil || *request.SliceBPS != row.SliceBPS {
			return fmt.Errorf("underwriting receipt request: %w", ErrActionConflict)
		}
		policy, err := decodeUnderwritingPolicy(response.Policy)
		if err != nil || !sameUnderwritingTerms(frozen, policy) {
			return fmt.Errorf("underwriting receipt policy: %w", ErrActionConflict)
		}
		owned := 0
		seen := make(map[string]struct{}, len(response.Underwritings))
		for _, receipt := range response.Underwritings {
			if _, duplicate := seen[receipt.ID]; duplicate || !validID(receipt.ID) || receipt.PolicyID != row.PolicyID ||
				!validID(receipt.CompanyID) || receipt.SliceBPS == nil || *receipt.SliceBPS <= 0 || *receipt.SliceBPS > 10000 ||
				receipt.CollateralPence == nil || *receipt.CollateralPence < 0 || *receipt.CollateralPence > maxUnderwritingPence ||
				receipt.PremiumPence == nil || *receipt.PremiumPence < 0 || *receipt.PremiumPence > maxUnderwritingPence {
				return fmt.Errorf("underwriting receipt fields: %w", ErrActionConflict)
			}
			seen[receipt.ID] = struct{}{}
			if receipt.CompanyID == row.CompanyID {
				owned++
				if receipt.ID != row.ID || *receipt.SliceBPS != row.SliceBPS ||
					*receipt.CollateralPence != row.CollateralPence || *receipt.PremiumPence != row.PremiumPence {
					return fmt.Errorf("underwriting receipt values: %w", ErrActionConflict)
				}
			}
		}
		if owned != 1 {
			return fmt.Errorf("underwriting owned receipt count: %w", ErrActionConflict)
		}
		lookup := s.privateLookup("underwriting-position", row.AccountID, row.ID)
		var stored underwritingPositionRow
		findErr := tx.Where("account_id = ? AND position_lookup = ?", row.AccountID, lookup).Take(&stored).Error
		if findErr != nil && !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if errors.Is(findErr, gorm.ErrRecordNotFound) {
			credentials, err := s.credentials(tx, row.AccountID)
			if err != nil {
				return err
			}
			if len(row.ID) != 36 || uuid.Validate(row.ID) != nil || scrubText(row.ID, credentialSecrets(credentials)) != row.ID {
				return fmt.Errorf("underwriting owned receipt identity: %w", ErrActionConflict)
			}
			privateRow, err := s.encodeUnderwritingPosition(row)
			if err != nil {
				return err
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&privateRow)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 0 {
				return nil
			}
		}
		if err := tx.Where("account_id = ? AND position_lookup = ?", row.AccountID, lookup).Take(&stored).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrActionConflict
			}
			return err
		}
		persisted, err := s.decodeUnderwritingPosition(stored)
		if err != nil {
			return err
		}
		previous, err := decodeUnderwritingPolicy(persisted.FrozenPolicy)
		if err != nil {
			return err
		}
		canonical, err := json.Marshal(previous)
		if err != nil {
			return err
		}
		if persisted.ID != row.ID || persisted.ActionID != row.ActionID || persisted.PolicyID != row.PolicyID || persisted.CompanyID != row.CompanyID ||
			persisted.SliceBPS != row.SliceBPS || persisted.CollateralPence != row.CollateralPence || persisted.PremiumPence != row.PremiumPence ||
			!persisted.AcceptedAt.Equal(row.AcceptedAt) || !bytes.Equal(canonical, row.FrozenPolicy) {
			return ErrActionConflict
		}
		return nil
	})
}

func (s *Store) UnderwritingPositions(ctx context.Context, accountID string) ([]UnderwritingPosition, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("underwriting position account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	stored := make([]underwritingPositionRow, 0)
	if err := db.Where("account_id = ?", accountID).Find(&stored).Error; err != nil {
		return nil, err
	}
	rows := make([]UnderwritingPosition, 0, len(stored))
	for _, record := range stored {
		value, err := s.decodeUnderwritingPosition(record)
		if err != nil {
			return nil, err
		}
		rows = append(rows, value)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].AcceptedAt.Equal(rows[j].AcceptedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].AcceptedAt.Before(rows[j].AcceptedAt)
	})
	return rows, nil
}

func (s *Store) RecordUnderwritingFunding(ctx context.Context, accountID, id string, evidence json.RawMessage) error {
	var proof struct {
		Entry         json.RawMessage `json:"entry"`
		BankingRegion string          `json:"banking_region"`
	}
	if !validID(accountID) || !validID(id) || json.Unmarshal(evidence, &proof) != nil ||
		!validID(proof.BankingRegion) || !bytes.HasPrefix(bytes.TrimSpace(proof.Entry), []byte("{")) {
		return fmt.Errorf("underwriting funding proof: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	evidence, err = redactJSON(evidence, secrets, 0)
	if err != nil {
		return err
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		var row underwritingPositionRow
		lookup := s.privateLookup("underwriting-position", accountID, id)
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("account_id = ? AND position_lookup = ?", accountID, lookup).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		position, err := s.decodeUnderwritingPosition(row)
		if err != nil {
			return err
		}
		if position.ID != id {
			return ErrActionConflict
		}
		if row.FundingEvidence != nil {
			var sameEvidence bool
			if err := tx.Raw("SELECT ?::jsonb = ?::jsonb", string(row.FundingEvidence), string(evidence)).Scan(&sameEvidence).Error; err != nil {
				return err
			}
			if !sameEvidence {
				return ErrActionConflict
			}
			return nil
		}
		return tx.Model(&underwritingPositionRow{}).Where("account_id = ? AND position_lookup = ?", accountID, lookup).
			Update("funding_evidence", evidence).Error
	})
}

func (s *Store) CloseUnderwritingPosition(ctx context.Context, closure UnderwritingClosure) error {
	if !validID(closure.AccountID) || !validID(closure.ID) || closure.ClosedAt.IsZero() ||
		(closure.Status != "settled_safe" && closure.Status != "claimed_loss") ||
		closure.EarnedPremiumPence < 0 || closure.EarnedPremiumPence > maxUnderwritingPence ||
		closure.ClaimsPence < 0 || closure.ClaimsPence > maxUnderwritingPence ||
		closure.ReturnedCollateralPence < 0 || closure.ReturnedCollateralPence > maxUnderwritingPence ||
		closure.FeesPence < 0 || closure.FeesPence > maxUnderwritingPence ||
		!json.Valid(closure.Evidence) || !bytes.HasPrefix(bytes.TrimSpace(closure.Evidence), []byte("{")) {
		return fmt.Errorf("underwriting closure fields: %w", ErrInvalidInput)
	}
	secrets, err := s.secrets(ctx, closure.AccountID)
	if err != nil {
		return err
	}
	evidence, err := redactJSON(closure.Evidence, secrets, 0)
	if err != nil {
		return err
	}
	closure.ClosedAt = closure.ClosedAt.UTC().Truncate(time.Microsecond)
	return s.writeOwned(ctx, closure.AccountID, func(tx *gorm.DB) error {
		var row underwritingPositionRow
		lookup := s.privateLookup("underwriting-position", closure.AccountID, closure.ID)
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("account_id = ? AND position_lookup = ?", closure.AccountID, lookup).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		position, err := s.decodeUnderwritingPosition(row)
		if err != nil {
			return err
		}
		if position.ID != closure.ID {
			return ErrActionConflict
		}
		if row.ClosedAt != nil {
			if row.Status != closure.Status || row.EarnedPremiumPence == nil || *row.EarnedPremiumPence != closure.EarnedPremiumPence ||
				row.ClaimsPence == nil || *row.ClaimsPence != closure.ClaimsPence ||
				row.ReturnedCollateralPence == nil || *row.ReturnedCollateralPence != closure.ReturnedCollateralPence ||
				row.FeesPence == nil || *row.FeesPence != closure.FeesPence || !row.ClosedAt.Equal(closure.ClosedAt) {
				return ErrActionConflict
			}
			var sameEvidence bool
			if err := tx.Raw("SELECT ?::jsonb = ?::jsonb", string(row.ClosureEvidence), string(evidence)).Scan(&sameEvidence).Error; err != nil {
				return err
			}
			if !sameEvidence {
				return ErrActionConflict
			}
			return nil
		}
		if closure.ClosedAt.Before(row.AcceptedAt) || closure.EarnedPremiumPence > row.PremiumPence ||
			closure.ClaimsPence > row.CollateralPence || closure.ReturnedCollateralPence != row.CollateralPence-closure.ClaimsPence ||
			closure.Status == "settled_safe" && closure.ClaimsPence != 0 ||
			closure.Status == "claimed_loss" && closure.ClaimsPence != row.CollateralPence {
			return fmt.Errorf("underwriting closure cash accounting: %w", ErrInvalidInput)
		}
		net := closure.EarnedPremiumPence
		for _, cost := range []int64{closure.ClaimsPence, closure.FeesPence} {
			if net < math.MinInt64+cost {
				return fmt.Errorf("underwriting closure net overflows integer pence: %w", ErrInvalidInput)
			}
			net -= cost
		}
		return tx.Model(&underwritingPositionRow{}).Where("account_id = ? AND position_lookup = ?", closure.AccountID, lookup).
			Updates(map[string]any{
				"status": closure.Status, "earned_premium_pence": closure.EarnedPremiumPence,
				"claims_pence": closure.ClaimsPence, "returned_collateral_pence": closure.ReturnedCollateralPence,
				"fees_pence": closure.FeesPence, "net_pence": net, "closed_at": closure.ClosedAt,
				"closure_evidence": evidence,
			}).Error
	})
}

func (s *Store) EnsureUnderwritingBudget(ctx context.Context, accountID string, initialPence int64) (UnderwritingRisk, error) {
	var risk UnderwritingRisk
	if !validID(accountID) || initialPence < 0 {
		return risk, fmt.Errorf("underwriting budget seed: %w", ErrInvalidInput)
	}
	err := s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO underwriting_budgets (account_id, initial_pence)
			VALUES (?, ?) ON CONFLICT (account_id) DO NOTHING`, accountID, initialPence).Error; err != nil {
			return err
		}
		var err error
		risk, err = underwritingRisk(tx, accountID)
		return err
	})
	return risk, err
}

func (s *Store) UnderwritingRisk(ctx context.Context, accountID string) (UnderwritingRisk, error) {
	if !validID(accountID) {
		return UnderwritingRisk{}, fmt.Errorf("underwriting risk account: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return underwritingRisk(db, accountID)
}

func underwritingRisk(db *gorm.DB, accountID string) (UnderwritingRisk, error) {
	var risk UnderwritingRisk
	// Numeric arithmetic preserves losses and principal exactly, including sums
	// beyond bigint intermediates. Out-of-range int64 results fail the scan.
	err := db.Raw(`
		WITH positions AS (
			SELECT collateral_pence::numeric AS collateral, net_pence::numeric AS net,
				closed_at IS NOT NULL AND net_pence IS NOT NULL
					AND status IN ('settled_safe', 'claimed_loss') AS closed
			FROM underwriting_positions WHERE account_id = ?
		), totals AS (
			SELECT COALESCE(SUM(collateral) FILTER (WHERE NOT closed), 0) AS outstanding,
				COALESCE(SUM(net) FILTER (WHERE closed), 0) AS realized,
				COALESCE(SUM(-net) FILTER (WHERE closed AND net < 0), 0) AS losses,
				COALESCE(SUM(net) FILTER (WHERE closed AND net > 0), 0) AS positive
			FROM positions
		), budget AS (
			SELECT initial_pence::numeric AS initial FROM underwriting_budgets WHERE account_id = ?
		)
		SELECT initial AS initial_pence, outstanding AS outstanding_pence,
			realized AS realized_net_pence, losses AS realized_loss_pence,
			GREATEST(0::numeric, initial + FLOOR(positive / 10) - losses - outstanding) AS remaining_pence
		FROM totals CROSS JOIN budget`, accountID, accountID).Scan(&risk).Error
	return risk, err
}
