package store

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
)

type RivalCompanyObservation struct {
	CompanyID        string
	CompanyName      string
	Credit           *gameapi.CreditReport
	CreditObservedAt *time.Time
}

type RivalExpansionContribution struct {
	TargetPhase int   `json:"target_phase"`
	Tons        int64 `json:"tons"`
	ShareBPS    int64 `json:"share_bps"`
}

func rivalOverviewRows(input RivalsInput, rows map[string]RivalRow) {
	add := func(name string) {
		if name == "" {
			return
		}
		if _, exists := rows[name]; !exists {
			rows[name] = RivalRow{CompanyName: name, Hunch: rivalUnmeasured, Fact: RivalFact{Kind: "no_baseline"}}
		}
	}
	companies := make(map[string]RivalCompanyObservation, len(input.Companies))
	ambiguous := make(map[string]bool)
	for _, company := range input.Companies {
		add(company.CompanyName)
		if previous, exists := companies[company.CompanyName]; exists && previous.CompanyID != company.CompanyID {
			ambiguous[company.CompanyName] = true
		}
		companies[company.CompanyName] = company
	}
	rising := make(map[string]gameapi.RisingEntry, len(input.Rising.Entries)+1)
	for _, entry := range input.Rising.Entries {
		add(entry.CompanyName)
		rising[entry.CompanyName] = entry
	}
	if you := input.Rising.You; you != nil {
		add(you.CompanyName)
		rising[you.CompanyName] = gameapi.RisingEntry{Rank: you.Rank, CompanyName: you.CompanyName,
			GrowthBPS: you.GrowthBPS, GainedPence: you.GainedPence, WindowStartAt: you.WindowStartAt}
	}
	ships := make(map[competitionIdentity]gameapi.SailingNotice)
	observe := func(notice gameapi.SailingNotice) {
		if notice.CompanyName == "" || notice.ShipName == "" || notice.ShipClass == "" || notice.SeenAt.IsZero() ||
			notice.SeenAt.Before(input.At.Add(-rivalEvidenceWindow)) || notice.SeenAt.After(input.At) {
			return
		}
		identity := competitionIdentity{company: notice.CompanyName, ship: notice.ShipName}
		if previous, exists := ships[identity]; !exists || notice.SeenAt.After(previous.SeenAt) ||
			notice.SeenAt.Equal(previous.SeenAt) && notice.ShipClass < previous.ShipClass {
			ships[identity] = notice
		}
		add(notice.CompanyName)
	}
	for _, notice := range input.ShipSightings {
		observe(notice)
	}
	for _, notice := range input.Sailings {
		observe(notice)
	}
	for _, port := range input.Ports {
		for _, notice := range port.Sailings {
			observe(notice)
		}
	}
	for name := range input.Expansion {
		add(name)
	}
	for name, row := range rows {
		row.ObservedShips24h = make(map[string]int)
		row.ObservedShips7d = make(map[string]int)
		row.ExpansionContributions = input.Expansion[name]
		if row.ExpansionContributions == nil {
			row.ExpansionContributions = make([]RivalExpansionContribution, 0)
		}
		if company, exists := companies[name]; exists && !ambiguous[name] {
			row.CompanyID = new(company.CompanyID)
			row.Credit, row.CreditObservedAt = company.Credit, company.CreditObservedAt
		}
		if entry, exists := rising[name]; exists {
			row.RisingRank, row.GrowthBPS, row.GainedPence = new(entry.Rank), new(entry.GrowthBPS), new(entry.GainedPence)
		}
		rows[name] = row
	}
	for _, ship := range ships {
		row := rows[ship.CompanyName]
		row.ObservedShips7d[ship.ShipClass]++
		if !ship.SeenAt.Before(input.At.Add(-CompetitionWindow)) {
			row.ObservedShips24h[ship.ShipClass]++
		}
	}
}

func rivalOwnNames(tx *gorm.DB, season int, at time.Time) ([]string, bool, error) {
	var owners []struct {
		CompanyID string
		Name      string
	}
	if err := tx.Raw(`SELECT a.company_id, COALESCE(
		NULLIF(CASE WHEN s.data #>> '{profile,company,id}' = a.company_id THEN s.data #>> '{profile,company,name}' END, ''),
		NULLIF(own.name, ''),
		NULLIF(c.company_name, ''), '') AS name
		FROM accounts a LEFT JOIN snapshots s ON s.account_id = a.id
		LEFT JOIN configured_company_identities own ON own.company_id = a.company_id AND own.observed_at <= ?
		LEFT JOIN rival_companies c ON c.company_id = a.company_id AND c.season_number = ? AND c.observed_at <= ?
		ORDER BY a.id LIMIT ?`, at, season, at, maxDashboardAccounts+1).Scan(&owners).Error; err != nil {
		return nil, false, err
	}
	complete := len(owners) <= maxDashboardAccounts
	if !complete {
		owners = owners[:maxDashboardAccounts]
	}
	names := make([]string, 0, len(owners))
	for _, owner := range owners {
		if owner.CompanyID == "" {
			continue
		}
		if owner.Name == "" {
			complete = false
		} else {
			names = append(names, owner.Name)
		}
	}
	var published []string
	if err := tx.Raw(`SELECT DISTINCT h.company_name FROM standings_accounts a
		JOIN accounts registered ON registered.id = a.account_id
		JOIN standings_history h ON h.season_number = a.season_number AND h.refreshed_at = a.refreshed_at AND h.rank = a.rank
		WHERE a.season_number = ? AND a.refreshed_at <= ? LIMIT 1001`, season, at).Scan(&published).Error; err != nil {
		return nil, false, err
	}
	if len(published) > 1000 {
		complete = false
		published = published[:1000]
	}
	return append(names, published...), complete, nil
}

func rivalOverviewInput(tx *gorm.DB, input *RivalsInput, season int, expansionRaw, metadataRaw json.RawMessage) error {
	names, complete, err := rivalOwnNames(tx, season, input.At)
	if err != nil {
		return err
	}
	input.OwnCompanies = append(input.OwnCompanies, names...)
	input.OwnCompaniesComplete = input.OwnCompaniesComplete && complete
	var companies []struct {
		CompanyID   string
		CompanyName string
		ObservedAt  *time.Time
		Report      json.RawMessage
	}
	if err := tx.Raw(`SELECT c.company_id, c.company_name, credit.observed_at, credit.report
		FROM rival_companies c LEFT JOIN LATERAL (
			SELECT observed_at, report FROM rival_credit_reports r WHERE r.season_number = c.season_number
				AND r.company_id = c.company_id AND r.observed_at <= ? ORDER BY r.observed_at DESC LIMIT 1
		) credit ON true WHERE c.season_number = ? AND c.observed_at <= ?
		ORDER BY c.company_id LIMIT 1001`, input.At, season, input.At).Scan(&companies).Error; err != nil {
		return err
	}
	if len(companies) > 1000 {
		return ErrResponseTooLarge
	}
	for _, company := range companies {
		observation := RivalCompanyObservation{CompanyID: company.CompanyID, CompanyName: company.CompanyName}
		if len(company.Report) != 0 {
			if err := json.Unmarshal(company.Report, &observation.Credit); err != nil {
				return err
			}
			observation.CreditObservedAt = company.ObservedAt
		}
		input.Companies = append(input.Companies, observation)
	}
	var notices []struct {
		SourceTime  string
		CompanyName string
		ShipName    string
		ShipClass   string
		PortID      string
		Direction   string
		OtherPortID *string
		CreatedAt   time.Time
	}
	if err := tx.Table("events").Select(`created_at, payload ->> 'timestamp' AS source_time,
		payload #>> '{payload,company_name}' AS company_name,
		payload #>> '{payload,port_id}' AS port_id,
		payload #>> '{payload,direction}' AS direction,
		payload #>> '{payload,other_port_id}' AS other_port_id,
		payload #>> '{payload,ship_name}' AS ship_name, payload #>> '{payload,ship_class}' AS ship_class`).
		Where("account_id = ? AND created_at BETWEEN ? AND ?", input.AccountID, input.At.Add(-rivalEvidenceWindow), input.At).
		Where("event_type IN ('ship_sailed', 'ship_entered_port')").
		Order("created_at DESC, id DESC").Limit(maxCompetitionEvents + 1).Scan(&notices).Error; err != nil {
		return err
	}
	if len(notices) > maxCompetitionEvents {
		input.ShipSightingsTruncated = true
		if !notices[maxCompetitionEvents].CreatedAt.Before(input.At.Add(-CompetitionWindow)) {
			input.EventsTruncated = true
		}
		notices = notices[:maxCompetitionEvents]
	}
	for _, notice := range notices {
		seenAt, err := time.Parse(time.RFC3339Nano, notice.SourceTime)
		if err == nil {
			sailing := gameapi.SailingNotice{
				CompanyName: notice.CompanyName, ShipName: notice.ShipName, ShipClass: notice.ShipClass, SeenAt: seenAt,
				PortID: notice.PortID, Direction: notice.Direction, OtherPortID: notice.OtherPortID,
			}
			input.ShipSightings = append(input.ShipSightings, sailing)
			if !notice.CreatedAt.Before(input.At.Add(-CompetitionWindow)) {
				input.Sailings = append(input.Sailings, sailing)
			}
		}
	}
	var metadata map[string]WarningDataset
	if len(metadataRaw) != 0 {
		if err := json.Unmarshal(metadataRaw, &metadata); err != nil {
			return err
		}
	}
	meta := metadata["expansion"]
	if len(expansionRaw) == 0 || !meta.Complete || meta.UpdatedAt.IsZero() || meta.UpdatedAt.After(input.At) {
		return nil
	}
	type tier struct {
		TargetPhase  int `json:"target_phase"`
		Contributors []struct {
			CompanyName string `json:"company_name"`
			Tons        int64  `json:"tons"`
			ShareBPS    int64  `json:"share_bps"`
		} `json:"contributors"`
	}
	var expansion struct {
		Completed []tier `json:"completed_tiers"`
		Current   *tier  `json:"current_tier"`
	}
	if err := json.Unmarshal(expansionRaw, &expansion); err != nil {
		return err
	}
	if expansion.Current != nil {
		expansion.Completed = append(expansion.Completed, *expansion.Current)
	}
	input.Expansion = make(map[string][]RivalExpansionContribution)
	for _, tier := range expansion.Completed {
		for _, contributor := range tier.Contributors {
			if tier.TargetPhase > 0 && contributor.CompanyName != "" {
				input.Expansion[contributor.CompanyName] = append(input.Expansion[contributor.CompanyName], RivalExpansionContribution{
					TargetPhase: tier.TargetPhase, Tons: contributor.Tons, ShareBPS: contributor.ShareBPS,
				})
			}
		}
	}
	for name := range input.Expansion {
		sort.Slice(input.Expansion[name], func(i, j int) bool {
			return input.Expansion[name][i].TargetPhase < input.Expansion[name][j].TargetPhase
		})
	}
	return nil
}
