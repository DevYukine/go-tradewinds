package store

import (
	"encoding/json"
	"strings"

	"gorm.io/gorm"
)

const (
	maxDashboardMissions     = 1000
	maxDashboardMissionBytes = 8 * 1024 * 1024
)

func (s *Store) dashboardMissions(tx *gorm.DB, accountID string, credentials displayCredentials) ([]DashboardMission, bool, error) {
	if !credentials.available {
		return nil, true, nil
	}
	// Ship identity and phase are encrypted. Read bounded metadata first so
	// discarded controller and terminal rows never hydrate their recovery data.
	rows, err := tx.Table("missions").
		Select(`ship_lookup,
			CASE WHEN octet_length(metadata_cipher) <= ? THEN metadata_cipher END AS metadata_cipher,
			octet_length(metadata_cipher)::bigint + octet_length(data_cipher)::bigint AS ciphertext_bytes`, maxDashboardMissionBytes).
		Where("account_id = ?", accountID).Order("ship_lookup").Rows()
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	lookups := make([]string, 0)
	metadata := make(map[string]missionMetadata)
	var total int64
	unavailable := false
	for rows.Next() {
		var header struct {
			ShipLookup      string
			MetadataCipher  []byte
			CiphertextBytes int64
		}
		if err := tx.ScanRows(rows, &header); err != nil {
			return nil, false, err
		}
		var value missionMetadata
		if err := s.decodePrivate(header.MetadataCipher, &value, "missions", "metadata", accountID, header.ShipLookup); err != nil ||
			s.privateLookup("mission-ship", accountID, value.ShipID) != header.ShipLookup {
			unavailable = true
			break
		}
		if !dashboardMissionEligible(value.ShipID, value.Phase) {
			continue
		}
		if len(lookups) == maxDashboardMissions || header.CiphertextBytes < 0 ||
			header.CiphertextBytes > maxDashboardMissionBytes-total {
			unavailable = true
			break
		}
		total += header.CiphertextBytes
		lookups = append(lookups, header.ShipLookup)
		metadata[header.ShipLookup] = value
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if unavailable {
		return nil, true, nil
	}
	projected := make([]DashboardMission, 0, len(lookups))
	if len(lookups) == 0 {
		return projected, false, nil
	}
	// Ciphertext is uncompressed. The eligible-row total bounds hydration and
	// the plaintext subsequently passed to the display-only JSON decoder.
	var stored []missionRow
	if err := tx.Select("account_id, ship_lookup, data_cipher, updated_at").
		Where("account_id = ? AND ship_lookup IN ?", accountID, lookups).
		Order("ship_lookup").Find(&stored).Error; err != nil {
		return nil, false, err
	}
	if len(stored) != len(lookups) {
		return nil, true, nil
	}
	for _, row := range stored {
		data, err := s.decrypt(row.DataCipher, privateAAD("missions", "data", accountID, row.ShipLookup))
		if err != nil {
			return nil, true, nil
		}
		value := metadata[row.ShipLookup]
		mission := Mission{ShipID: value.ShipID, Phase: value.Phase, Data: data, UpdatedAt: row.UpdatedAt}
		view, ok, err := dashboardMission(&mission, credentials)
		if err != nil {
			return nil, true, nil
		}
		if ok {
			projected = append(projected, view)
		}
	}
	return projected, false, nil
}

func dashboardMissionEligible(shipID, phase string) bool {
	if shipID == "" || strings.HasPrefix(shipID, "__") {
		return false
	}
	switch phase {
	case "", "done", "settled", "maintenance_done", "growth_rejected":
		return false
	}
	return true
}

func dashboardMission(mission *Mission, credentials displayCredentials) (DashboardMission, bool, error) {
	if !credentials.available || !dashboardMissionEligible(mission.ShipID, mission.Phase) {
		return DashboardMission{}, false, nil
	}
	// Presence is checked separately from value: omitted fields cannot turn a
	// recovery-only row into a plan or an unrecorded forecast into zero profit.
	var data struct {
		Plan *struct {
			Kind        *string                   `json:"kind"`
			Origin      *string                   `json:"origin"`
			Destination *string                   `json:"destination"`
			Commodity   *string                   `json:"commodity"`
			Quantity    *int64                    `json:"quantity"`
			ContractID  string                    `json:"contract_id"`
			Economics   DashboardMissionEconomics `json:"economics"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(mission.Data, &data); err != nil {
		return DashboardMission{}, false, err
	}
	if data.Plan == nil {
		return DashboardMission{}, false, nil
	}
	plan := data.Plan
	if plan.Kind == nil || strings.TrimSpace(*plan.Kind) == "" || plan.Origin == nil ||
		plan.Destination == nil || plan.Commodity == nil || plan.Quantity == nil {
		return DashboardMission{}, false, nil
	}
	return DashboardMission{
		ShipID:    displayText(mission.ShipID, credentials),
		Phase:     displayText(mission.Phase, credentials),
		UpdatedAt: mission.UpdatedAt,
		Plan: DashboardMissionPlan{
			Kind:        displayText(*plan.Kind, credentials),
			Origin:      displayText(*plan.Origin, credentials),
			Destination: displayText(*plan.Destination, credentials),
			Commodity:   displayText(*plan.Commodity, credentials),
			Quantity:    *plan.Quantity,
			ContractID:  displayText(plan.ContractID, credentials),
			Economics: DashboardMissionEconomics{
				ExpectedNetPence: plan.Economics.ExpectedNetPence,
				Basis:            displayText(plan.Economics.Basis, credentials),
			},
		},
	}, true, nil
}
