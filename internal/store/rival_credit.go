package store

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	RivalCreditHourlyLimit     = 20
	RivalCreditCompanyInterval = 6 * time.Hour
)

type rivalCompany struct {
	SeasonNumber int
	CompanyID    string
	CompanyName  string
	ObservedAt   time.Time
}

func (rivalCompany) TableName() string { return "rival_companies" }

type rivalCreditAttempt struct {
	ID          int64 `gorm:"primaryKey"`
	AccountID   string
	CompanyID   string
	AttemptedAt time.Time
	RecordedAt  *time.Time
}

func (rivalCreditAttempt) TableName() string { return "rival_credit_attempts" }

type rivalCreditReport struct {
	SeasonNumber int
	CompanyID    string
	ObservedAt   time.Time
	Report       json.RawMessage `gorm:"type:jsonb"`
}

func (rivalCreditReport) TableName() string { return "rival_credit_reports" }

func lockRivalCreditBudget(tx *gorm.DB) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "tradewinds:rival-credit-budget").Error
}

// ReserveRivalCredits fences all runners with one durable, rolling real-hour
// budget under one shared lock. Never refund a reservation: a crash can hide
// a completed HTTP send. An exhausted hour reserves no companies.
func (s *Store) ReserveRivalCredits(ctx context.Context, accountID string, companyIDs []string) ([]string, error) {
	if !validID(accountID) {
		return nil, ErrInvalidInput
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var reserved []string
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := lockRivalCreditBudget(tx); err != nil {
			return err
		}
		var now time.Time
		if err := tx.Raw("SELECT clock_timestamp()").Scan(&now).Error; err != nil {
			return err
		}
		var hourly int64
		if err := tx.Model(&rivalCreditAttempt{}).Where("attempted_at > ?", now.Add(-time.Hour)).Count(&hourly).Error; err != nil {
			return err
		}
		remaining := RivalCreditHourlyLimit - int(hourly)
		if remaining <= 0 || len(companyIDs) == 0 {
			return nil
		}
		var excluded []string
		if err := tx.Raw(`SELECT company_id FROM accounts WHERE company_id <> ''
			UNION SELECT company_id FROM rival_credit_attempts WHERE attempted_at > ?`,
			now.Add(-RivalCreditCompanyInterval)).Scan(&excluded).Error; err != nil {
			return err
		}
		seen := make(map[string]bool, len(excluded))
		for _, id := range excluded {
			seen[id] = true
		}
		attempts := make([]rivalCreditAttempt, 0, remaining)
		for _, id := range companyIDs {
			if !validID(id) || seen[id] {
				continue
			}
			seen[id] = true
			reserved = append(reserved, id)
			attempts = append(attempts, rivalCreditAttempt{AccountID: accountID, CompanyID: id, AttemptedAt: now.UTC()})
			if len(attempts) == remaining {
				break
			}
		}
		if len(attempts) == 0 {
			return nil
		}
		return tx.Create(&attempts).Error
	})
	if err != nil {
		return nil, err
	}
	return reserved, nil
}

func currentRivalSeason(tx *gorm.DB, at time.Time) (int, error) {
	var season int
	err := tx.Raw(`SELECT COALESCE(max(season_number), 0) FROM (
		SELECT season_number FROM standings_history WHERE refreshed_at <= ?
		UNION ALL SELECT season_number FROM rising_publications WHERE refreshed_at <= ?
	) AS publications`, at, at).Scan(&season).Error
	return season, err
}

func recordRivalCompany(tx *gorm.DB, season int, id, name string, at time.Time) error {
	if season < 1 || !validID(id) {
		return nil
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "season_number"}, {Name: "company_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"company_name": gorm.Expr("CASE WHEN EXCLUDED.company_name <> '' THEN EXCLUDED.company_name ELSE rival_companies.company_name END"),
			"observed_at":  at,
		}),
	}).Create(&rivalCompany{SeasonNumber: season, CompanyID: id, CompanyName: name, ObservedAt: at}).Error
}

func recordConfiguredCompanyIdentity(tx *gorm.DB, accountID, companyID, name string, at time.Time) error {
	if !validID(companyID) || strings.TrimSpace(name) == "" {
		return nil
	}
	return tx.Exec(`INSERT INTO configured_company_identities (company_id, name, observed_at)
		SELECT company_id, ?, ? FROM accounts WHERE id = ? AND company_id = ?
		ON CONFLICT (company_id) DO UPDATE SET name = EXCLUDED.name, observed_at = EXCLUDED.observed_at`,
		name, at, accountID, companyID).Error
}

// Only explicit paired fields establish identity. Leaderboards currently publish
// names alone; an absent ID must never be synthesized from rank or home port.
func recordRivalCompanies(tx *gorm.DB, season int, path string, raw json.RawMessage, at time.Time) error {
	var rows []struct {
		CompanyID    string `json:"company_id"`
		Name         string `json:"name"`
		HolderID     string `json:"holder_company_id"`
		HolderName   string `json:"holder_company_name"`
		BorrowerID   string `json:"borrower_company_id"`
		BorrowerName string `json:"borrower_name"`
		CreatorID    string `json:"creator_company_id"`
		SellerID     string `json:"seller_company_id"`
		BuyerID      string `json:"buyer_company_id"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	for _, row := range rows {
		var id, name string
		switch path {
		case "/v1/world/monopolies":
			id, name = row.HolderID, row.HolderName
		case "/v1/bank/list":
			// A bank's name is not its owner's company name.
			id = row.CompanyID
		case "/v1/loan/requests":
			id, name = row.BorrowerID, row.BorrowerName
		case "/v1/future/list":
			for _, party := range []string{row.CreatorID, row.SellerID, row.BuyerID} {
				if err := recordRivalCompany(tx, season, party, "", at); err != nil {
					return err
				}
			}
			continue
		}
		if err := recordRivalCompany(tx, season, id, name, at); err != nil {
			return err
		}
	}
	return nil
}

func recordRivalCredit(tx *gorm.DB, row Request, endpoint *url.URL, at time.Time) error {
	companyID := endpoint.Query().Get("company_id")
	if !validID(companyID) {
		return nil
	}
	var configured Account
	if err := tx.Select("id").Where("company_id = ?", companyID).Limit(1).Find(&configured).Error; err != nil {
		return err
	}
	if configured.ID != "" {
		if row.Status != 200 || row.Error != "" {
			return nil
		}
		var identity struct {
			CompanyID   string `json:"company_id"`
			CompanyName string `json:"company_name"`
		}
		if json.Unmarshal(row.Response, &identity) != nil || identity.CompanyID != companyID || strings.TrimSpace(identity.CompanyName) == "" {
			return nil
		}
		return recordConfiguredCompanyIdentity(tx, configured.ID, companyID, identity.CompanyName, at)
	}
	if err := lockRivalCreditBudget(tx); err != nil {
		return err
	}
	var reservation rivalCreditAttempt
	if err := tx.Where("account_id = ? AND company_id = ? AND recorded_at IS NULL AND attempted_at > ?", row.AccountID, companyID, at.Add(-RivalCreditCompanyInterval)).
		Order("attempted_at DESC, id DESC").Limit(1).Find(&reservation).Error; err != nil {
		return err
	}
	if reservation.ID == 0 {
		if err := tx.Create(&rivalCreditAttempt{AccountID: row.AccountID, CompanyID: companyID, AttemptedAt: at, RecordedAt: &at}).Error; err != nil {
			return err
		}
	} else if err := tx.Model(&reservation).Update("recorded_at", at).Error; err != nil {
		return err
	}
	if row.Status != 200 || row.Error != "" {
		return nil
	}
	var report gameapi.CreditReport
	if json.Unmarshal(row.Response, &report) != nil || report.CompanyID != companyID || report.CompanyName == "" {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(row.Response, &fields) != nil {
		return nil
	}
	for _, field := range []string{"active_loans", "active_owed_pence", "defaulted_open_loans", "defaulted_owed_pence", "loans_repaid", "loans_defaulted", "loans_written_off", "loan_written_off_pence", "loans_walked_away", "loan_walked_away_pence", "open_mortgages", "futures_claims_open_pence", "futures_claims_total", "futures_claims_written_off", "lent_outstanding_pence", "smuggling_convictions"} {
		var value *int64
		if json.Unmarshal(fields[field], &value) != nil || value == nil || *value < 0 {
			return nil
		}
	}
	season, err := currentRivalSeason(tx, at)
	if err != nil || season == 0 {
		return err
	}
	if err := recordRivalCompany(tx, season, companyID, report.CompanyName, at); err != nil {
		return err
	}
	return tx.Create(&rivalCreditReport{SeasonNumber: season, CompanyID: companyID, ObservedAt: at, Report: row.Response}).Error
}

// RivalCreditCandidates includes retained explicit IDs and recent public event
// IDs, not delivery-envelope company_id (which identifies the event recipient).
func (s *Store) RivalCreditCandidates(ctx context.Context, accountID string) ([]string, error) {
	if !validID(accountID) {
		return nil, ErrInvalidInput
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var season int
	if err := db.Raw(`SELECT COALESCE(max(season_number), 0) FROM (
		SELECT season_number FROM rising_publications WHERE refreshed_at <= now()
		UNION ALL SELECT season_number FROM standings_history WHERE refreshed_at <= now()
	) AS publications`).Scan(&season).Error; err != nil {
		return nil, err
	}
	if season == 0 {
		// Without a publication season, a fetched report cannot be retained.
		return nil, ErrNotFound
	}
	var ids []string
	err := db.Raw(`WITH candidates AS (
		SELECT company_id FROM rival_companies WHERE season_number = ?
		UNION SELECT candidate.id FROM events
		CROSS JOIN LATERAL (VALUES
			(CASE WHEN event_type = 'monopoly_granted' THEN payload #>> '{payload,holder_company_id}' END),
			(CASE WHEN event_type = 'loan_request_posted' THEN payload #>> '{payload,borrower_company_id}' END),
			(CASE WHEN event_type = 'futures_listed' THEN payload #>> '{payload,creator_company_id}' END),
			(CASE WHEN event_type = 'futures_listed' THEN payload #>> '{payload,seller_company_id}' END),
			(CASE WHEN event_type = 'futures_listed' THEN payload #>> '{payload,buyer_company_id}' END)
		) AS candidate(id)
		WHERE account_id = ? AND created_at BETWEEN now() - interval '7 days' AND now()
			AND event_type IN ('monopoly_granted', 'loan_request_posted', 'futures_listed')
	)
	SELECT candidates.company_id FROM candidates
	LEFT JOIN rival_companies c ON c.company_id = candidates.company_id AND c.season_number = ?
	LEFT JOIN LATERAL (
		SELECT rank FROM standings_history h WHERE h.season_number = ? AND h.company_name = c.company_name
			AND h.refreshed_at <= now() ORDER BY h.refreshed_at DESC LIMIT 1
	) standing ON true
	LEFT JOIN LATERAL (
		SELECT max(attempted_at) AS attempted_at FROM rival_credit_attempts a WHERE a.company_id = candidates.company_id
	) attempt ON true
	WHERE candidates.company_id IS NOT NULL AND btrim(candidates.company_id) <> ''
		AND octet_length(candidates.company_id) <= 255
		AND NOT EXISTS (SELECT 1 FROM accounts WHERE accounts.company_id = candidates.company_id)
	ORDER BY standing.rank NULLS LAST, attempt.attempted_at NULLS FIRST, candidates.company_id LIMIT 1000`,
		season, accountID, season, season).Scan(&ids).Error
	return ids, err
}
