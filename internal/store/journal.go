package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func actionAAD(accountID, id, field string) string {
	return "action\x00" + accountID + "\x00" + id + "\x00" + field
}

func (s *Store) decodeAction(row actionRow) (*Action, error) {
	body, err := s.decrypt(row.BodyCipher, actionAAD(row.AccountID, row.ID, "body"))
	if err != nil {
		return nil, fmt.Errorf("decrypt action request: %w", err)
	}
	var response []byte
	if row.ResponseCipher != nil {
		response, err = s.decrypt(row.ResponseCipher, actionAAD(row.AccountID, row.ID, "response"))
		if err != nil {
			return nil, fmt.Errorf("decrypt action response: %w", err)
		}
	}
	return &Action{
		ID:         row.ID,
		AccountID:  row.AccountID,
		Method:     row.Method,
		Path:       row.Path,
		Body:       body,
		State:      row.State,
		Status:     row.Status,
		Response:   response,
		RetryAfter: row.RetryAfter,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}, nil
}

// HasAction checks an exact account-scoped journal identity without creating a
// row or materializing its encrypted request and response.
func (s *Store) HasAction(ctx context.Context, accountID, id string) (bool, error) {
	if !validID(accountID) || !validID(id) {
		return false, fmt.Errorf("action identity: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var exists bool
	err := db.Raw("SELECT EXISTS (SELECT 1 FROM actions WHERE account_id = ? AND id = ?)", accountID, id).Scan(&exists).Error
	return exists, err
}

func (s *Store) BeginAction(ctx context.Context, action *Action) (*Action, error) {
	if action == nil || !validID(action.ID) || !validID(action.AccountID) || !strings.HasPrefix(action.Path, "/") || len(action.Path) > 4096 {
		return nil, fmt.Errorf("action identity: %w", ErrInvalidInput)
	}
	switch action.Method {
	case "POST", "PATCH", "PUT", "DELETE":
	default:
		return nil, fmt.Errorf("action method: %w", ErrInvalidInput)
	}
	ciphertext, err := s.encrypt(action.Body, actionAAD(action.AccountID, action.ID, "body"))
	if err != nil {
		return nil, err
	}
	var persisted *Action
	var expired bool
	err = s.writeOwned(ctx, action.AccountID, func(tx *gorm.DB) error {
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).First(&account, "id = ?", action.AccountID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		now := time.Now().UTC()
		row := actionRow{
			ID:         action.ID,
			AccountID:  action.AccountID,
			CompanyID:  account.CompanyID,
			Method:     action.Method,
			Path:       action.Path,
			BodyCipher: ciphertext,
			State:      ActionPending,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", action.ID).Error; err != nil {
			return err
		}
		if row.AccountID != action.AccountID || row.Method != action.Method || row.Path != action.Path ||
			row.State == ActionPending && row.CompanyID != account.CompanyID {
			return ErrActionConflict
		}
		var err error
		persisted, err = s.decodeAction(row)
		if err != nil {
			return err
		}
		if !bytes.Equal(persisted.Body, action.Body) {
			return ErrActionConflict
		}
		expired = row.State == ActionPending && !now.Before(row.CreatedAt.Add(ActionReplayWindow))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if expired {
		return persisted, ErrActionExpired
	}
	return persisted, nil
}

func (s *Store) CompleteAction(ctx context.Context, id string, status int, response json.RawMessage, retryAfter *time.Time) error {
	definitive := status >= 200 && status < 300 || status >= 400 && status < 500
	if !definitive || status == 409 && bytes.Contains(response, []byte("idempotency_request_in_flight")) || status == 422 && bytes.Contains(response, []byte("idempotency_key_reused")) {
		return fmt.Errorf("HTTP outcome is not safe to settle: %w", ErrInvalidInput)
	}
	return s.finishAction(ctx, id, ActionComplete, status, response, retryAfter)
}

// ReconcileAction records authoritative REST evidence or proof that a newly
// journaled request was never sent, not a fabricated HTTP response. Local
// no-send proof is valid only before that dispatch's first transport attempt;
// a pre-existing pending request always remains ambiguous until reconciled.
// The runner is responsible for proving the evidence resolves the intent.
func (s *Store) ReconcileAction(ctx context.Context, id string, evidence json.RawMessage) error {
	if !json.Valid(evidence) || bytes.Equal(bytes.TrimSpace(evidence), []byte("null")) {
		return fmt.Errorf("reconciliation evidence: %w", ErrInvalidInput)
	}
	return s.finishAction(ctx, id, ActionReconciled, 0, evidence, nil)
}

func (s *Store) finishAction(ctx context.Context, id, state string, status int, response []byte, retryAfter *time.Time) error {
	if !validID(id) {
		return fmt.Errorf("action ID: %w", ErrInvalidInput)
	}
	if retryAfter != nil {
		normalized := retryAfter.UTC()
		// PostgreSQL stores microseconds; rounding down could retry too early.
		if remainder := normalized.Nanosecond() % int(time.Microsecond); remainder != 0 {
			normalized = normalized.Add(time.Microsecond - time.Duration(remainder))
		}
		retryAfter = &normalized
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return db.Transaction(func(tx *gorm.DB) error {
		var row actionRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&row, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if err := s.guardOwner(ctx, tx, row.AccountID); err != nil {
			return err
		}
		if row.State != ActionPending {
			persisted, err := s.decodeAction(row)
			if err != nil {
				return err
			}
			sameRetryAfter := persisted.RetryAfter == nil && retryAfter == nil ||
				persisted.RetryAfter != nil && retryAfter != nil && persisted.RetryAfter.Equal(*retryAfter)
			if persisted.State == state && persisted.Status == status && bytes.Equal(persisted.Response, response) && sameRetryAfter {
				return nil
			}
			return ErrActionConflict
		}
		if retryAfter != nil && (state != ActionComplete || status != 429) {
			return fmt.Errorf("retry deadline requires a complete HTTP 429: %w", ErrInvalidInput)
		}
		ciphertext, err := s.encrypt(response, actionAAD(row.AccountID, row.ID, "response"))
		if err != nil {
			return err
		}
		return tx.Model(&actionRow{}).Where("id = ?", id).Updates(map[string]any{
			"state":           state,
			"status":          status,
			"response_cipher": ciphertext,
			"retry_after":     retryAfter,
			"updated_at":      time.Now().UTC(),
		}).Error
	})
}

func (s *Store) PendingActions(ctx context.Context, accountID string) ([]Action, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	rows := make([]actionRow, 0)
	if err := db.Where("account_id = ? AND state = ?", accountID, ActionPending).Order("created_at, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	actions := make([]Action, 0, len(rows))
	for _, row := range rows {
		action, err := s.decodeAction(row)
		if err != nil {
			return nil, err
		}
		actions = append(actions, *action)
	}
	return actions, nil
}

func (s *Store) GetMission(ctx context.Context, accountID, shipID string) (*Mission, error) {
	if !validID(accountID) || !validID(shipID) {
		return nil, fmt.Errorf("mission identity: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var mission Mission
	if err := db.Where("account_id = ? AND ship_id = ?", accountID, shipID).Take(&mission).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &mission, nil
}

func (s *Store) PutMission(ctx context.Context, mission *Mission) error {
	if mission == nil || !validID(mission.AccountID) || !validID(mission.ShipID) || mission.Phase == "" {
		return fmt.Errorf("mission fields: %w", ErrInvalidInput)
	}
	row := *mission
	if len(row.Data) == 0 {
		row.Data = json.RawMessage("null")
	}
	if !json.Valid(row.Data) {
		return fmt.Errorf("mission data: %w", ErrInvalidInput)
	}
	row.UpdatedAt = time.Now().UTC()
	err := s.writeOwned(ctx, row.AccountID, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "account_id"}, {Name: "ship_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"phase", "destination", "commodity", "contract_id", "quantity",
				"price_pence", "generation", "data", "updated_at",
			}),
		}).Create(&row).Error
	})
	if err == nil {
		mission.UpdatedAt = row.UpdatedAt
	}
	return err
}

func (s *Store) DeleteMission(ctx context.Context, accountID, shipID string) error {
	if !validID(accountID) || !validID(shipID) {
		return fmt.Errorf("mission identity: %w", ErrInvalidInput)
	}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Where("account_id = ? AND ship_id = ?", accountID, shipID).Delete(&Mission{}).Error
	})
}
