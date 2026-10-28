package store

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Read missions before receipts: the engine archives a receipt before releasing
// its mission, so that handoff cannot leave a gap in the protection window.
func (s *Store) passengerRecoveryWindows(ctx context.Context, accountID string) (map[string]time.Time, error) {
	db, cancel := s.query(ctx)
	defer cancel()
	windows := make(map[string]time.Time)
	unreadable := 0
	var afterAccount, afterShip string
	var missions []missionRow
	for {
		query := db.Select("account_id, ship_lookup, metadata_cipher, data_cipher").
			Order("account_id, ship_lookup").Limit(64)
		if accountID != "" {
			query = query.Where("account_id = ?", accountID)
		}
		if afterAccount != "" {
			query = query.Where("(account_id, ship_lookup) > (?, ?)", afterAccount, afterShip)
		}
		if err := query.Find(&missions).Error; err != nil {
			return nil, err
		}
		for _, row := range missions {
			mission, err := s.decodeMission(row)
			if err != nil {
				windows[row.AccountID] = time.Time{}
				unreadable++
				continue
			}
			var intent struct {
				Passage   *json.RawMessage `json:"passage"`
				StartedAt time.Time        `json:"started_at"`
			}
			if err := json.Unmarshal(mission.Data, &intent); err != nil {
				windows[row.AccountID] = time.Time{}
				unreadable++
				continue
			}
			if intent.Passage == nil {
				continue
			}
			// A missing historical start cannot prove older events disposable.
			if since, exists := windows[row.AccountID]; !exists || intent.StartedAt.Before(since) {
				windows[row.AccountID] = intent.StartedAt
			}
		}
		if len(missions) < 64 {
			break
		}
		last := missions[len(missions)-1]
		afterAccount, afterShip = last.AccountID, last.ShipLookup
	}
	if unreadable > 0 {
		s.log.Warn("passenger retention protected unreadable recovery rows", zap.Int("unreadable_rows", unreadable))
	}
	query := db.Table("passenger_receipts AS receipt").
		Joins("LEFT JOIN settlements AS settlement ON settlement.id = receipt.id AND settlement.account_id = receipt.account_id").
		Where("NOT receipt.resolved AND receipt.closed_at IS NULL").
		Select("receipt.account_id, MIN(COALESCE(settlement.started_at, receipt.created_at)) AS since").
		Group("receipt.account_id")
	if accountID != "" {
		query = query.Where("receipt.account_id = ?", accountID)
	}
	var receipts []struct {
		AccountID string
		Since     time.Time
	}
	if err := query.Scan(&receipts).Error; err != nil {
		return nil, err
	}
	for _, receipt := range receipts {
		if since, exists := windows[receipt.AccountID]; !exists || receipt.Since.Before(since) {
			windows[receipt.AccountID] = receipt.Since
		}
	}
	return windows, nil
}

func (s *Store) passengerEventProtection(ctx context.Context) (string, []any, error) {
	windows, err := s.passengerRecoveryWindows(ctx, "")
	if err != nil {
		return "", nil, err
	}
	accounts := make([]string, 0, len(windows))
	for account := range windows {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	clauses := make([]string, 0, len(accounts))
	args := make([]any, 0, len(accounts)*2)
	for _, account := range accounts {
		clauses = append(clauses, "(account_id = ? AND created_at >= ?)")
		args = append(args, account, windows[account])
	}
	if len(clauses) == 0 {
		return "", nil, nil
	}
	return " AND NOT (event_type IN ('passengers_boarded', 'passengers_paid', 'passengers_lost') AND (" + strings.Join(clauses, " OR ") + "))", args, nil
}
