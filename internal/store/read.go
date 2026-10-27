package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

func (s *Store) Dashboard(ctx context.Context) (Dashboard, error) {
	result := Dashboard{
		UpdatedAt:   time.Now().UTC(),
		Accounts:    make([]DashboardAccount, 0),
		Metrics:     make([]Metric, 0),
		Decisions:   make([]Decision, 0),
		Events:      make([]Event, 0),
		Requests:    make([]RequestSummary, 0),
		Settlements: make([]Settlement, 0),
	}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		scrubber := displayScrubber{store: s, tx: tx}
		accounts := make([]Account, 0)
		if err := tx.Order("created_at, id").Limit(maxDashboardAccounts + 1).Find(&accounts).Error; err != nil {
			return err
		}
		if len(accounts) > maxDashboardAccounts {
			return ErrResponseTooLarge
		}
		ids := make([]string, 0, len(accounts))
		indices := make(map[string]int, len(accounts))
		for _, account := range accounts {
			indices[account.ID] = len(result.Accounts)
			ids = append(ids, account.ID)
			result.Accounts = append(result.Accounts, DashboardAccount{
				Account:  account,
				Snapshot: make(map[string]json.RawMessage),
			})
		}
		if len(ids) != 0 {
			var sizes []struct {
				DataSize int64
			}
			if err := tx.Table("snapshots").Select("octet_length(data::text) AS data_size").Where("account_id IN ?", ids).Find(&sizes).Error; err != nil {
				return err
			}
			var total int64
			for _, size := range sizes {
				total += size.DataSize
				if total > maxDashboardSnapshotBytes {
					return ErrResponseTooLarge
				}
			}
			var snapshots []snapshotRow
			if err := tx.Where("account_id IN ?", ids).Find(&snapshots).Error; err != nil {
				return err
			}
			for _, snapshot := range snapshots {
				index := indices[snapshot.AccountID]
				updatedAt := snapshot.UpdatedAt
				result.Accounts[index].UpdatedAt = &updatedAt
				credentials := scrubber.credentials(snapshot.AccountID)
				if !credentials.available {
					// Withhold typed datasets rather than replacing their values
					// with a string or inventing a successful empty observation.
					// Retained metadata may contain secrets too; publish only
					// fixed local errors, never its original keys or messages.
					result.Accounts[index].Snapshot["_meta"] = json.RawMessage(`{
						"_engine": {
							"complete": false,
							"stale": true,
							"error": "Account credentials unavailable; retained data cannot be safely displayed."
						},
						"ships": {
							"complete": false,
							"stale": true,
							"error": "Account credentials unavailable; retained data cannot be safely displayed."
						},
						"profile": {
							"complete": false,
							"stale": true,
							"error": "Account credentials unavailable; retained data cannot be safely displayed."
						},
						"catalog": {
							"complete": false,
							"stale": true,
							"error": "Account credentials unavailable; retained data cannot be safely displayed."
						}
					}`)
					continue
				}
				if err := json.Unmarshal(snapshot.Data, &result.Accounts[index].Snapshot); err != nil {
					return fmt.Errorf("decode dashboard snapshot: %w", err)
				}
				for key, raw := range result.Accounts[index].Snapshot {
					clean, err := displayJSON(raw, credentials)
					if err != nil {
						return err
					}
					result.Accounts[index].Snapshot[key] = clean
				}
			}
		}
		if err := tx.Order("recorded_at DESC, id DESC").Limit(500).Find(&result.Metrics).Error; err != nil {
			return err
		}
		if err := tx.Order("created_at DESC, id DESC").Limit(100).Find(&result.Decisions).Error; err != nil {
			return err
		}
		for i := range result.Decisions {
			if err := scrubber.decision(&result.Decisions[i]); err != nil {
				return err
			}
		}
		if err := tx.Order("created_at DESC, id DESC").Limit(100).Find(&result.Events).Error; err != nil {
			return err
		}
		for i := range result.Events {
			if err := scrubber.event(&result.Events[i]); err != nil {
				return err
			}
		}
		if err := tx.Table("requests").Select("account_id, method, path, status, duration_ms, error, created_at").
			Order("created_at DESC, id DESC").Limit(100).Find(&result.Requests).Error; err != nil {
			return err
		}
		for i := range result.Requests {
			scrubber.request(&result.Requests[i])
		}
		settlements, err := s.settlementRows(tx.Order("completed_at DESC, id").Limit(100))
		if err != nil {
			return err
		}
		for i := range settlements {
			scrubber.settlement(&settlements[i])
		}
		result.Settlements = settlements
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return Dashboard{}, err
	}
	return result, err
}

func (s *Store) History(ctx context.Context, accountID, kind string, limit int) (any, error) {
	if limit == 0 {
		limit = 100
	}
	if (accountID != "" || kind == "journal") && !validID(accountID) || limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("history query: %w", ErrInvalidInput)
	}
	if kind == "journal" {
		return s.journalHistory(ctx, accountID, limit)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var result any
	err := db.Transaction(func(tx *gorm.DB) error {
		scrubber := displayScrubber{store: s, tx: tx}
		query := tx
		if accountID != "" {
			query = query.Where("account_id = ?", accountID)
		}
		switch kind {
		case "events":
			rows := make([]Event, 0)
			if err := query.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
				return err
			}
			for i := range rows {
				if err := scrubber.event(&rows[i]); err != nil {
					return err
				}
			}
			result = rows
		case "requests":
			rows := make([]Request, 0)
			if err := query.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
				return err
			}
			for i := range rows {
				scrubber.request(&rows[i].RequestSummary)
				credentials := scrubber.credentials(rows[i].AccountID)
				var err error
				rows[i].Request, err = displayJSON(rows[i].Request, credentials)
				if err != nil {
					return err
				}
				rows[i].Response, err = displayJSON(rows[i].Response, credentials)
				if err != nil {
					return err
				}
			}
			result = rows
		case "decisions":
			rows := make([]Decision, 0)
			if err := query.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
				return err
			}
			for i := range rows {
				if err := scrubber.decision(&rows[i]); err != nil {
					return err
				}
			}
			result = rows
		case "metrics":
			rows := make([]Metric, 0)
			if err := query.Order("recorded_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
				return err
			}
			result = rows
		case "settlements":
			rows, err := s.settlementRows(query.Order("completed_at DESC, id").Limit(limit))
			if err != nil {
				return err
			}
			for i := range rows {
				scrubber.settlement(&rows[i])
			}
			result = rows
		default:
			return fmt.Errorf("unsupported history kind: %w", ErrInvalidInput)
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) ServiceStats(ctx context.Context) (ServiceStats, error) {
	stats := ServiceStats{Counters: make([]Metric, 0), Metrics: make([]Metric, 0)}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Account{}).Count(&stats.Accounts).Error; err != nil {
			return err
		}
		if err := tx.Model(&Account{}).Where("enabled = true").Count(&stats.EnabledAccounts).Error; err != nil {
			return err
		}
		if err := tx.Model(&actionRow{}).Where("state = ?", ActionPending).Count(&stats.PendingActions).Error; err != nil {
			return err
		}
		if err := tx.Table("metric_counters").Order("account_id, name").Limit(1001).Find(&stats.Counters).Error; err != nil {
			return err
		}
		if len(stats.Counters) > 1000 {
			return ErrResponseTooLarge
		}
		if err := tx.Raw(`SELECT DISTINCT ON (account_id, name)
			account_id, name, value, recorded_at FROM metrics
			ORDER BY account_id, name, recorded_at DESC, id DESC LIMIT 1001`).Scan(&stats.Metrics).Error; err != nil {
			return err
		}
		if len(stats.Metrics) > 1000 {
			return ErrResponseTooLarge
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err == nil {
		s.replayErrors.Range(func(key, value any) bool {
			stats.Counters = append(stats.Counters, Metric{
				AccountID: key.(string), Name: "replay_record_errors_total",
				Value: float64(value.(*atomic.Int64).Load()), RecordedAt: time.Now().UTC(),
			})
			return true
		})
	}
	return stats, err
}
