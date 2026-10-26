package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
)

const maxCompetitionEvents = 10000

// Competition reads recorded evidence only. The receipt-time predicate uses the
// partial events index; the pure aggregator applies the upstream activity time
// again so a replay cannot make old traffic appear fresh.
func (s *Store) Competition(ctx context.Context, accountID string, at time.Time) (CompetitionSummary, error) {
	if !validID(accountID) || at.IsZero() {
		return CompetitionSummary{}, fmt.Errorf("competition query: %w", ErrInvalidInput)
	}
	input := CompetitionInput{AccountID: accountID, At: at.UTC(), OwnCompaniesComplete: true}
	db, cancel := s.query(ctx)
	defer cancel()
	var result CompetitionSummary
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := competitionOwners(tx, &input); err != nil {
			return err
		}
		scrubber := displayScrubber{store: s, tx: tx}
		credentials := scrubber.credentials(accountID)
		if !credentials.available {
			return errors.New("competition display credentials unavailable")
		}
		var snapshot struct {
			Catalog  json.RawMessage
			Ports    json.RawMessage
			Metadata json.RawMessage
		}
		var size int64
		if err := tx.Table("snapshots").Select("COALESCE(octet_length(data::text), 0)").Where("account_id = ?", accountID).Scan(&size).Error; err != nil {
			return err
		}
		// PostgreSQL's jsonb text adds whitespace absent from SaveSnapshot's
		// compact JSON; that rendering is bounded by twice the compact cap.
		if size > 2*maxSnapshotBytes {
			return ErrResponseTooLarge
		}
		if err := tx.Table("snapshots").Select("data -> 'catalog' AS catalog, data -> 'ports' AS ports, data -> '_meta' AS metadata").
			Where("account_id = ?", accountID).Scan(&snapshot).Error; err != nil {
			return err
		}
		ports, err := competitionPorts(snapshot.Catalog, snapshot.Ports, snapshot.Metadata, input.At)
		if err != nil {
			return err
		}
		input.Ports = ports
		if err := competitionSailings(tx, &input); err != nil {
			return err
		}
		result = AggregateCompetition(input)
		scrubCompetition(&result, credentials)
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return result, err
}

// CompetitionActivityInput loads bounded local activity and owner-name
// projections only. Trading already has catalog, ports and metadata in memory;
// it must not run the dashboard transaction or decrypt display credentials.
func (s *Store) CompetitionActivityInput(ctx context.Context, accountID string, at time.Time) (CompetitionInput, error) {
	input := CompetitionInput{AccountID: accountID, At: at.UTC(), OwnCompaniesComplete: true}
	if !validID(accountID) || at.IsZero() {
		return input, fmt.Errorf("competition query: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	if err := competitionOwners(db, &input); err != nil {
		return input, err
	}
	err := competitionSailings(db, &input)
	return input, err
}

func competitionOwners(tx *gorm.DB, input *CompetitionInput) error {
	var owners []struct {
		ID              string
		CompanyID       string
		SnapshotCompany string
		Name            string
	}
	if err := tx.Table("accounts AS a").
		Select("a.id, a.company_id, s.data #>> '{profile,company,id}' AS snapshot_company, s.data #>> '{profile,company,name}' AS name").
		Joins("LEFT JOIN snapshots AS s ON s.account_id = a.id").Order("a.id").Limit(maxDashboardAccounts + 1).Scan(&owners).Error; err != nil {
		return err
	}
	if len(owners) > maxDashboardAccounts {
		return ErrResponseTooLarge
	}
	found := false
	for _, owner := range owners {
		found = found || owner.ID == input.AccountID
		if owner.CompanyID == "" {
			continue
		}
		if owner.Name == "" || owner.SnapshotCompany != owner.CompanyID {
			input.OwnCompaniesComplete = false
		}
		if owner.Name != "" {
			input.OwnCompanies = append(input.OwnCompanies, owner.Name)
		}
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

func competitionSailings(db *gorm.DB, input *CompetitionInput) error {
	var rows []struct {
		SourceTime  string
		PortID      string
		CompanyName string
		ShipName    string
		ShipClass   string
		Direction   string
		OtherPortID *string
	}
	if err := db.Table("events").Select(`payload ->> 'timestamp' AS source_time,
		payload #>> '{payload,port_id}' AS port_id,
		payload #>> '{payload,company_name}' AS company_name,
		payload #>> '{payload,ship_name}' AS ship_name,
		payload #>> '{payload,ship_class}' AS ship_class,
		payload #>> '{payload,direction}' AS direction,
		payload #>> '{payload,other_port_id}' AS other_port_id`).
		Where("account_id = ? AND created_at >= ? AND created_at <= ?", input.AccountID, input.At.Add(-CompetitionWindow), input.At).
		Where("event_type IN ('ship_sailed', 'ship_entered_port')").
		Order("created_at DESC, id DESC").Limit(maxCompetitionEvents + 1).Scan(&rows).Error; err != nil {
		return err
	}
	if len(rows) > maxCompetitionEvents {
		input.EventsTruncated = true
		rows = rows[:maxCompetitionEvents]
	}
	input.Sailings = make([]gameapi.SailingNotice, 0, len(rows))
	for _, row := range rows {
		seenAt, err := time.Parse(time.RFC3339Nano, row.SourceTime)
		if err != nil {
			continue
		}
		input.Sailings = append(input.Sailings, gameapi.SailingNotice{
			PortID: row.PortID, CompanyName: row.CompanyName, ShipName: row.ShipName,
			ShipClass: row.ShipClass, Direction: row.Direction, SeenAt: seenAt, OtherPortID: row.OtherPortID,
		})
	}
	return nil
}

// CompetitionPorts projects the runner's already-captured snapshot.
func CompetitionPorts(catalogRaw, portsRaw, metadataRaw json.RawMessage, at time.Time) ([]CompetitionPortObservation, error) {
	return competitionPorts(catalogRaw, portsRaw, metadataRaw, at)
}

func competitionPorts(catalogRaw, portsRaw, metadataRaw json.RawMessage, at time.Time) ([]CompetitionPortObservation, error) {
	var catalog gameapi.Catalog
	var ports map[string]struct {
		Visibility          string                  `json:"visibility"`
		RecentSailings      []gameapi.SailingNotice `json:"recent_sailings"`
		PlayerVolumeTons    *int64                  `json:"rolling_player_volume_tons"`
		PlayerTurnoverPence *int64                  `json:"rolling_player_turnover_pence"`
		OccupiedBerths      *int64                  `json:"occupied_berths"`
		EffectiveBerths     *int64                  `json:"effective_berths"`
		QueuedShips         *int64                  `json:"queued_ships"`
	}
	var metadata map[string]struct {
		UpdatedAt    time.Time  `json:"updated_at"`
		UpstreamAsOf *time.Time `json:"upstream_as_of"`
		Complete     bool       `json:"complete"`
		Stale        bool       `json:"stale"`
		Error        string     `json:"error"`
	}
	for _, dataset := range []struct {
		raw    json.RawMessage
		target any
	}{{catalogRaw, &catalog}, {portsRaw, &ports}, {metadataRaw, &metadata}} {
		if len(dataset.raw) != 0 {
			if err := json.Unmarshal(dataset.raw, dataset.target); err != nil {
				return nil, fmt.Errorf("decode competition snapshot: %w", err)
			}
		}
	}
	result := make([]CompetitionPortObservation, 0, len(catalog.Ports))
	for _, port := range catalog.Ports {
		detail := ports[port.ID]
		meta := metadata["ports/"+port.ID]
		observedAt := meta.UpdatedAt
		if meta.UpstreamAsOf != nil && meta.UpstreamAsOf.Before(observedAt) {
			observedAt = *meta.UpstreamAsOf
		}
		region := port.BankingRegion
		if region == "" {
			region = "unknown"
		}
		result = append(result, CompetitionPortObservation{
			ID: port.ID, Name: port.Name, Region: region, ObservedAt: observedAt,
			QuayVisible:      detail.Visibility == "quay" && detail.RecentSailings != nil,
			Stale:            meta.Stale || meta.Error != "" || !meta.Complete || observedAt.IsZero() || observedAt.After(at) || at.Sub(observedAt) >= 2*time.Minute,
			PlayerVolumeTons: detail.PlayerVolumeTons, PlayerTurnoverPence: detail.PlayerTurnoverPence,
			OccupiedBerths: detail.OccupiedBerths, EffectiveBerths: detail.EffectiveBerths,
			QueuedShips: detail.QueuedShips, Sailings: detail.RecentSailings,
		})
	}
	return result, nil
}

func scrubCompetition(summary *CompetitionSummary, credentials displayCredentials) {
	activity := func(value *CompetitionActivity) {
		for i := range value.TopCompetitors {
			value.TopCompetitors[i].Name = displayText(value.TopCompetitors[i].Name, credentials)
		}
	}
	for i := range summary.Regions {
		region := &summary.Regions[i]
		region.ID = displayText(region.ID, credentials)
		activity(&region.CompetitionActivity)
		for j := range region.Ports {
			port := &region.Ports[j]
			port.ID = displayText(port.ID, credentials)
			port.Name = displayText(port.Name, credentials)
			activity(&port.CompetitionActivity)
			for k := range port.Ships {
				ship := &port.Ships[k]
				ship.CompanyName = displayText(ship.CompanyName, credentials)
				ship.ShipName = displayText(ship.ShipName, credentials)
				ship.ShipClass = displayText(ship.ShipClass, credentials)
				ship.Direction = displayText(ship.Direction, credentials)
				if ship.ReportedOtherPortID != nil {
					value := displayText(*ship.ReportedOtherPortID, credentials)
					ship.ReportedOtherPortID = &value
				}
			}
		}
	}
}
