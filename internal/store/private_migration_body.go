package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// Goose version 5 transforms recovery fields once, without engine decoding.
// A legacy JSON string body is a request token, never already-base64 bytes.
func (s *Store) migrateMissionData(tx *gorm.DB, mission *Mission) error {
	trimmed := bytes.TrimSpace(mission.Data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(mission.Data, &members); err != nil {
		return err
	}
	withdrawn := false
	if mission.ShipID == "__growth" {
		lane, exists := members["growth_lane"]
		if !exists || bytes.Equal(bytes.TrimSpace(lane), []byte("null")) {
			// A legacy allocation has no frozen procurement bounds. Authorize
			// no further procurement while retaining the original attempt and
			// paid accounting for the engine's accounted closing handoff.
			encodedLane, err := json.Marshal(struct {
				AdmissionID                  string `json:"admission_id"`
				QuantityLimit                int64  `json:"quantity_limit"`
				InitialProcurementLimitPence int64  `json:"initial_procurement_limit_pence"`
				InitialProcurementComplete   bool   `json:"initial_procurement_complete"`
			}{
				AdmissionID:                mission.Generation,
				InitialProcurementComplete: true,
			})
			if err != nil {
				return err
			}
			members["growth_lane"] = encodedLane
			withdrawn = true
		}
	}
	rawAction, exists := members["action"]
	if !exists || bytes.Equal(bytes.TrimSpace(rawAction), []byte("null")) {
		if withdrawn {
			encoded, err := json.Marshal(members)
			if err != nil {
				return err
			}
			mission.Data = encoded
		}
		return nil
	}
	var action map[string]json.RawMessage
	if json.Unmarshal(rawAction, &action) != nil || action == nil {
		return errors.New("legacy mission action is not a recovery object")
	}
	body, bodyPresent := action["body"]
	var identity struct {
		ID     string `json:"id"`
		Method string `json:"method"`
		Path   string `json:"path"`
	}
	if err := json.Unmarshal(rawAction, &identity); err != nil {
		return errors.New("legacy mission action identity is invalid")
	}
	var journal actionRow
	journalErr := tx.Select("id, account_id, method, path, body_cipher").Where("id = ?", identity.ID).Take(&journal).Error
	if journalErr != nil && !errors.Is(journalErr, gorm.ErrRecordNotFound) {
		return journalErr
	}
	requestBytes := []byte(body)
	if journalErr == nil {
		if journal.AccountID != mission.AccountID || journal.ID != identity.ID || journal.Method != identity.Method || journal.Path != identity.Path {
			return fmt.Errorf("legacy mission action identity disagrees with its journal: %w", ErrActionConflict)
		}
		journalBody, err := s.decrypt(journal.BodyCipher, actionAAD(journal.AccountID, journal.ID, "body"))
		if err != nil {
			return err
		}
		// A nil RawMessage was serialized as null by the old engine. The
		// immutable journal distinguishes no body from the literal null token.
		noBody := len(journalBody) == 0 && (!bodyPresent || bytes.Equal(bytes.TrimSpace(body), []byte("null")))
		sameBody := noBody || bytes.Equal(body, journalBody)
		if !sameBody && json.Valid(body) && json.Valid(journalBody) {
			// Legacy mission JSONB already uses PostgreSQL's exact numeric and
			// object equality. Never route request numbers through float64.
			if err := tx.Raw("SELECT ?::jsonb = ?::jsonb", string(body), string(journalBody)).Scan(&sameBody).Error; err != nil {
				return err
			}
		}
		if !sameBody {
			return fmt.Errorf("legacy mission request disagrees with its journal: %w", ErrActionConflict)
		}
		requestBytes = journalBody
	}
	if bodyPresent || journalErr == nil {
		encodedBody, err := json.Marshal(requestBytes)
		if err != nil {
			return err
		}
		action["body"] = encodedBody
	}
	encodedAction, err := json.Marshal(action)
	if err != nil {
		return err
	}
	members["action"] = encodedAction
	encoded, err := json.Marshal(members)
	if err != nil {
		return err
	}
	mission.Data = encoded
	return nil
}
