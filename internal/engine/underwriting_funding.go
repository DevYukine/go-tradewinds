package engine

import (
	"encoding/json"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
)

// Metadata keys are not a published contract. An exact identity reference plus
// the unique matching own-vault debit is evidence; equal cash amounts alone are
// not. If the server does not expose that link, further underwriting stays held.
func underwritingFundingEvidence(position store.UnderwritingPosition, entries []json.RawMessage, vaults []Vault) json.RawMessage {
	var proof json.RawMessage
	for _, payload := range entries {
		var entry struct {
			ID             string    `json:"id"`
			DebitAccountID string    `json:"debit_account_id"`
			AmountPence    int64     `json:"amount_pence"`
			Metadata       any       `json:"metadata"`
			CreatedAt      time.Time `json:"created_at"`
		}
		if json.Unmarshal(payload, &entry) != nil || entry.ID == "" || entry.AmountPence != position.CollateralPence ||
			entry.CreatedAt.Before(position.AcceptedAt.Add(-time.Second)) ||
			!insuranceReference(entry.Metadata, position.ID, position.PolicyID) {
			continue
		}
		region := ""
		for _, vault := range vaults {
			if vault.AccountType == "vault" && vault.AccountID == entry.DebitAccountID {
				region = vault.BankingRegion
				break
			}
		}
		if region == "" {
			continue
		}
		if proof != nil {
			return nil
		}
		proof = encode(map[string]any{"entry": payload, "banking_region": region})
	}
	return proof
}

func insuranceReference(value any, positionID, policyID string) bool {
	switch value := value.(type) {
	case string:
		return value == positionID || value == policyID
	case []any:
		for _, child := range value {
			if insuranceReference(child, positionID, policyID) {
				return true
			}
		}
	case map[string]any:
		for _, child := range value {
			if insuranceReference(child, positionID, policyID) {
				return true
			}
		}
	}
	return false
}
