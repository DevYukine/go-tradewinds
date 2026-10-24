package store

import (
	"encoding/json"

	"gorm.io/gorm"
)

type displayCredentials struct {
	secrets   []string
	available bool
	err       error
}

type displayScrubber struct {
	store    *Store
	tx       *gorm.DB
	accounts map[string]displayCredentials
}

func (d *displayScrubber) credentials(accountID string) displayCredentials {
	if value, exists := d.accounts[accountID]; exists {
		return value
	}
	credentials, err := d.store.credentials(d.tx, accountID)
	value := displayCredentials{secrets: credentialSecrets(credentials), available: err == nil, err: err}
	if d.accounts == nil {
		d.accounts = make(map[string]displayCredentials)
	}
	d.accounts[accountID] = value
	return value
}

func displayText(value string, credentials displayCredentials) string {
	if !credentials.available && value != "" {
		return redacted
	}
	return scrubText(value, credentials.secrets)
}

func displayJSON(raw json.RawMessage, credentials displayCredentials) (json.RawMessage, error) {
	if !credentials.available {
		return json.RawMessage(`"[REDACTED]"`), nil
	}
	return redactJSON(raw, credentials.secrets, 0)
}

func scrubDecision(value *Decision, secrets []string, available bool) error {
	credentials := displayCredentials{secrets: secrets, available: available}
	value.ShipID = displayText(value.ShipID, credentials)
	value.Kind = displayText(value.Kind, credentials)
	value.Reason = displayText(value.Reason, credentials)
	var err error
	value.Details, err = displayJSON(value.Details, credentials)
	return err
}

func (d *displayScrubber) decision(value *Decision) error {
	credentials := d.credentials(value.AccountID)
	return scrubDecision(value, credentials.secrets, credentials.available)
}

func (d *displayScrubber) settlement(value *Settlement) {
	credentials := d.credentials(value.AccountID)
	value.ShipID = displayText(value.ShipID, credentials)
	value.Lane = displayText(value.Lane, credentials)
	value.Kind = displayText(value.Kind, credentials)
	value.Origin = displayText(value.Origin, credentials)
	value.Destination = displayText(value.Destination, credentials)
	value.Commodity = displayText(value.Commodity, credentials)
}

func (d *displayScrubber) event(value *Event) error {
	credentials := d.credentials(value.AccountID)
	value.EventID = displayText(value.EventID, credentials)
	value.EventType = displayText(value.EventType, credentials)
	var err error
	value.Payload, err = displayJSON(value.Payload, credentials)
	return err
}

func (d *displayScrubber) request(value *RequestSummary) {
	credentials := d.credentials(value.AccountID)
	value.Method = displayText(value.Method, credentials)
	value.Path = displayText(value.Path, credentials)
	value.Error = displayText(value.Error, credentials)
}

func (d *displayScrubber) journal(value *JournalRow) {
	credentials := d.credentials(value.AccountID)
	// Action/settlement IDs and row classifications are locally generated.
	// Credential failures censor payload-derived text, not those stable keys.
	value.ShipID = boundedText(displayText(value.ShipID, credentials), 255)
	value.Origin = boundedText(displayText(value.Origin, credentials), 255)
	value.Destination = boundedText(displayText(value.Destination, credentials), 255)
	value.Commodity = boundedText(displayText(value.Commodity, credentials), 255)
	value.Summary = boundedText(displayText(value.Summary, credentials), 512)
}

func (s *Store) settlementRows(tx *gorm.DB) ([]Settlement, error) {
	var stored []settlementRow
	if err := tx.Find(&stored).Error; err != nil {
		return nil, err
	}
	rows := make([]Settlement, 0, len(stored))
	for _, record := range stored {
		value, err := s.decodeSettlement(record)
		if err != nil {
			return nil, err
		}
		rows = append(rows, value)
	}
	return rows, nil
}
