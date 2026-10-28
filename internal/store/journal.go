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

// ActionsBetween returns exact replay request/receipt projections in the
// half-open creation-time window. It authenticates every bounded row, including
// pending and rejected actions, without retaining rich diagnostic payloads.
// An evidence budget failure returns no partial history.
func (s *Store) ActionsBetween(ctx context.Context, accountID string, from, to time.Time) ([]Action, error) {
	if !validID(accountID) || from.IsZero() || !to.After(from) {
		return nil, fmt.Errorf("action history window: %w", ErrInvalidInput)
	}
	return s.replayActions(ctx, accountID, from, to, nil)
}

// ReplayActionsByID reads only exact related receipts before the window's
// exclusive end. It supplements ActionsBetween for missions crossing its start.
func (s *Store) ReplayActionsByID(ctx context.Context, accountID string, ids []string, before time.Time) ([]Action, error) {
	if !validID(accountID) || before.IsZero() || len(ids) > 1024 {
		return nil, fmt.Errorf("related replay actions: %w", ErrInvalidInput)
	}
	for _, id := range ids {
		if !validID(id) {
			return nil, fmt.Errorf("related replay action identity: %w", ErrInvalidInput)
		}
	}
	if len(ids) == 0 {
		return []Action{}, nil
	}
	return s.replayActions(ctx, accountID, time.Time{}, before, ids)
}

func (s *Store) replayActions(ctx context.Context, accountID string, from, to time.Time, ids []string) ([]Action, error) {
	actions := make([]Action, 0)
	page := make([]replayEvidenceRowMeta, 0, replayEvidencePageSize)
	var cursor replayEvidenceRowMeta
	var budget replayEvidenceBudget
	for {
		db, cancel := s.query(ctx)
		query := db.Model(&actionRow{}).
			Select("CASE WHEN "+replayActionMetadataBytesSQL+" <= ? THEN id ELSE '' END AS id, created_at AS at, "+
				replayActionCiphertextBytesSQL+" AS ciphertext_bytes, "+replayActionMetadataBytesSQL+" AS metadata_bytes", MaxReplayEvidenceMetadataBytes).
			Where("account_id = ? AND created_at < ?", accountID, to)
		if len(ids) > 0 {
			query = query.Where("id IN ?", ids)
		} else {
			query = query.Where("created_at >= ?", from)
		}
		if cursor.ID != "" {
			query = query.Where("(created_at, id) > (?, ?)", cursor.At, cursor.ID)
		}
		page = page[:0]
		err := query.Order("created_at, id").Limit(replayEvidencePageSize).Find(&page).Error
		cancel()
		if err != nil {
			return nil, err
		}
		if err := budget.addRows(len(page), "action history"); err != nil {
			return nil, err
		}
		for _, meta := range page {
			if err := meta.check("action history"); err != nil {
				return nil, err
			}
			action, err := s.replayActionEvidence(ctx, accountID, meta.ID)
			if err != nil {
				return nil, err
			}
			size := replayEvidenceRowOverhead + len(action.ID) + len(action.AccountID) +
				len(action.Method) + len(action.Path) + len(action.State) + cap(action.Body) + cap(action.Response)
			if err := budget.retain(int64(size), "action history"); err != nil {
				return nil, err
			}
			actions = append(actions, action)
		}
		if len(page) < replayEvidencePageSize {
			return actions, nil
		}
		cursor = page[len(page)-1]
	}
}

func (s *Store) replayActionEvidence(ctx context.Context, accountID, id string) (Action, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	var row actionRow
	err := db.Select("id, account_id, method, path, body_cipher, state, status, response_cipher, retry_after, created_at, updated_at").
		Where("account_id = ? AND id = ?", accountID, id).
		Where("("+replayActionCiphertextBytesSQL+") <= ? AND ("+replayActionMetadataBytesSQL+") <= ?",
			MaxReplayEvidenceCiphertextBytes, MaxReplayEvidenceMetadataBytes).
		Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Action{}, fmt.Errorf("action history row changed or exceeds the guarded read budget: %w", ErrReplayEvidenceBudget)
	}
	if err != nil {
		return Action{}, err
	}
	action, err := s.decodeAction(row)
	if err != nil {
		return Action{}, err
	}
	action.Body = projectReplayEvidenceJSON(action.Body, replayEvidenceRequest)
	action.Response = projectReplayEvidenceJSON(action.Response, replayEvidenceResponse)
	return *action, nil
}
