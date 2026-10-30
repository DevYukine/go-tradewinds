package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type standingCapture struct {
	SeasonNumber  int                  `json:"season_number"`
	RefreshedAt   *time.Time           `json:"refreshed_at"`
	NextRefreshAt *time.Time           `json:"next_refresh_at"`
	Entries       []standingEntry      `json:"entries"`
	You           *standingEntry       `json:"you"`
	Rising        *gameapi.RisingBoard `json:"rising"`
}

type standingEntry struct {
	Rank          int             `json:"rank"`
	CompanyName   string          `json:"company_name"`
	HomePortID    string          `json:"home_port_id"`
	NetWorthPence *int64          `json:"net_worth_pence"`
	Titles        json.RawMessage `json:"titles"`
}

type standingRow struct {
	SeasonNumber  int
	RefreshedAt   time.Time
	Rank          int
	CompanyName   string
	HomePortID    string
	NetWorthPence int64
	Titles        json.RawMessage `gorm:"type:jsonb"`
	NextRefreshAt *time.Time
	ObservedAt    time.Time
}

func (standingRow) TableName() string { return "standings_history" }

type standingAccount struct {
	AccountID    string
	SeasonNumber int
	RefreshedAt  time.Time
	Rank         int
}

func (standingAccount) TableName() string { return "standings_accounts" }

// The source publishes no player IDs. A rank identifies one player within an
// immutable hourly snapshot; only its published best-company name can link
// observations over time. Never stitch different companies or seasons together.
func recordStandings(tx *gorm.DB, accountID string, raw json.RawMessage, observedAt time.Time) error {
	var capture standingCapture
	if len(raw) == 0 || json.Unmarshal(raw, &capture) != nil || capture.SeasonNumber < 1 || capture.RefreshedAt == nil || capture.RefreshedAt.IsZero() {
		return nil
	}
	entries := capture.Entries
	if capture.You != nil {
		found := false
		for _, entry := range entries {
			if entry.Rank == capture.You.Rank {
				if entry.CompanyName != capture.You.CompanyName || entry.NetWorthPence == nil || capture.You.NetWorthPence == nil || *entry.NetWorthPence != *capture.You.NetWorthPence {
					return nil
				}
				found = true
			}
		}
		if !found {
			entries = append(entries, *capture.You)
		}
	}
	rows := make([]standingRow, 0, len(entries))
	seen := make(map[int]bool, len(entries))
	for _, entry := range entries {
		if entry.Rank < 1 || entry.CompanyName == "" || entry.NetWorthPence == nil || seen[entry.Rank] {
			return nil
		}
		seen[entry.Rank] = true
		encodedTitles, err := json.Marshal(entry.Titles)
		if err != nil {
			return err
		}
		rows = append(rows, standingRow{
			SeasonNumber: capture.SeasonNumber, RefreshedAt: capture.RefreshedAt.UTC(),
			Rank: entry.Rank, CompanyName: entry.CompanyName, HomePortID: entry.HomePortID,
			NetWorthPence: *entry.NetWorthPence, Titles: encodedTitles,
			NextRefreshAt: capture.NextRefreshAt, ObservedAt: observedAt.UTC(),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
		return err
	}
	if capture.You != nil {
		return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&standingAccount{
			AccountID: accountID, SeasonNumber: capture.SeasonNumber,
			RefreshedAt: capture.RefreshedAt.UTC(), Rank: capture.You.Rank,
		}).Error
	}
	return nil
}

type risingPublication struct {
	AccountID     string
	SeasonNumber  int
	RefreshedAt   time.Time
	NextRefreshAt *time.Time
	Board         json.RawMessage `gorm:"type:jsonb"`
	ObservedAt    time.Time
}

func (risingPublication) TableName() string { return "rising_publications" }

func recordRising(tx *gorm.DB, accountID string, raw json.RawMessage, observedAt time.Time) error {
	var capture standingCapture
	if json.Unmarshal(raw, &capture) != nil || capture.SeasonNumber < 1 || capture.RefreshedAt == nil || capture.RefreshedAt.IsZero() {
		return nil
	}
	if capture.Rising == nil || capture.Rising.RankedPlayers < 0 {
		return nil
	}
	board := capture.Rising
	seen := make(map[int32]bool, len(board.Entries))
	for _, entry := range board.Entries {
		if entry.Rank < 1 || entry.CompanyName == "" || entry.WindowStartAt.IsZero() || entry.WindowStartAt.After(*capture.RefreshedAt) || seen[entry.Rank] {
			return nil
		}
		seen[entry.Rank] = true
	}
	if board.You != nil && (board.You.Rank < 1 || board.You.CompanyName == "" ||
		board.You.WindowStartAt.IsZero() || board.You.WindowStartAt.After(*capture.RefreshedAt)) {
		return nil
	}
	if board.Entries == nil {
		board.Entries = make([]gameapi.RisingEntry, 0)
	}
	encoded, err := json.Marshal(board)
	if err != nil {
		return err
	}
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&risingPublication{
		AccountID: accountID, SeasonNumber: capture.SeasonNumber, RefreshedAt: capture.RefreshedAt.UTC(),
		NextRefreshAt: capture.NextRefreshAt, Board: encoded, ObservedAt: observedAt.UTC(),
	}).Error
}

func latestRising(tx *gorm.DB, accountID string, at time.Time) (gameapi.RisingBoard, *time.Time, int, error) {
	board := gameapi.RisingBoard{Entries: make([]gameapi.RisingEntry, 0)}
	var publication risingPublication
	if err := tx.Raw(`SELECT * FROM rising_publications WHERE refreshed_at <= ?
		AND season_number = (SELECT max(season_number) FROM (
			SELECT season_number FROM rising_publications WHERE refreshed_at <= ?
			UNION ALL SELECT season_number FROM standings_history WHERE refreshed_at <= ?
		) AS publications)
		ORDER BY refreshed_at DESC, (account_id = ?) DESC LIMIT 1`, at, at, at, accountID).
		Scan(&publication).Error; err != nil {
		return board, nil, 0, err
	}
	if publication.RefreshedAt.IsZero() {
		return board, nil, 0, nil
	}
	if err := json.Unmarshal(publication.Board, &board); err != nil {
		return board, nil, 0, err
	}
	if publication.AccountID != accountID {
		board.You = nil
	}
	return board, &publication.RefreshedAt, publication.SeasonNumber, nil
}

type StandingPoint struct {
	At            time.Time `json:"at"`
	NetWorthPence int64     `json:"net_worth_pence"`
}

type StandingSeries struct {
	SeasonNumber       int             `json:"season_number"`
	CompanyName        string          `json:"company_name"`
	Rank               int             `json:"rank"`
	IsUs               bool            `json:"is_us"`
	Charted            bool            `json:"charted"`
	FirstAt            time.Time       `json:"first_at"`
	LastAt             time.Time       `json:"last_at"`
	FirstNetWorthPence int64           `json:"first_net_worth_pence"`
	LastNetWorthPence  int64           `json:"last_net_worth_pence"`
	ChangePence        *int64          `json:"change_pence"`
	Observations       int64           `json:"observations"`
	Points             []StandingPoint `json:"points" gorm:"-"`
}

type StandingsHistory struct {
	AccountID     string           `json:"account_id"`
	GeneratedAt   time.Time        `json:"generated_at"`
	From          time.Time        `json:"from"`
	To            time.Time        `json:"to"`
	LatestAt      *time.Time       `json:"latest_at"`
	NextRefreshAt *time.Time       `json:"next_refresh_at"`
	BucketSeconds int64            `json:"bucket_seconds"`
	Top           int              `json:"top"`
	Players       []StandingSeries `json:"players"`
}

// Range bounds are inclusive official valuation times, not local fetch times.
// First/last values and change use exact observations, never chart downsampling.
func (s *Store) StandingsHistory(ctx context.Context, accountID string, from, to time.Time, top int) (StandingsHistory, error) {
	if !validID(accountID) || to.IsZero() || !from.IsZero() && from.After(to) || top < 1 || top > 20 {
		return StandingsHistory{}, fmt.Errorf("standings history range: %w", ErrInvalidInput)
	}
	result := StandingsHistory{
		AccountID: accountID, GeneratedAt: time.Now().UTC(), From: from.UTC(), To: to.UTC(),
		Top: top, Players: make([]StandingSeries, 0),
	}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		if from.IsZero() {
			var first sql.NullTime
			if err := tx.Raw("SELECT min(refreshed_at) FROM standings_history WHERE refreshed_at <= ?", to).Row().Scan(&first); err != nil {
				return err
			}
			result.From = result.To
			if first.Valid {
				result.From = first.Time.UTC()
			}
		}
		result.BucketSeconds = moneyBucketSeconds(result.To.Unix() - result.From.Unix() + 1)
		var latest struct {
			At   *time.Time
			Next *time.Time
		}
		if err := tx.Raw(`SELECT refreshed_at AS at, next_refresh_at AS next FROM standings_history
			WHERE refreshed_at >= ? AND refreshed_at <= ? ORDER BY refreshed_at DESC, rank LIMIT 1`, result.From, to).Scan(&latest).Error; err != nil {
			return err
		}
		result.LatestAt, result.NextRefreshAt = latest.At, latest.Next
		if err := tx.Raw(`WITH identities AS (
			SELECT h.season_number, h.company_name, min(h.refreshed_at) AS first_at,
				max(h.refreshed_at) AS last_at, count(*) AS observations, bool_or(a.account_id IS NOT NULL) AS is_us
			FROM standings_history h LEFT JOIN standings_accounts a
				ON a.season_number = h.season_number AND a.refreshed_at = h.refreshed_at AND a.rank = h.rank AND a.account_id = ?
			WHERE h.refreshed_at >= ? AND h.refreshed_at <= ? GROUP BY h.season_number, h.company_name
		)
		SELECT identities.*, first.net_worth_pence AS first_net_worth_pence,
			last.net_worth_pence AS last_net_worth_pence, last.rank,
			CASE WHEN observations > 1 THEN last.net_worth_pence::numeric - first.net_worth_pence END AS change_pence
		FROM identities
		JOIN standings_history first ON first.season_number = identities.season_number
			AND first.company_name = identities.company_name AND first.refreshed_at = first_at
		JOIN standings_history last ON last.season_number = identities.season_number
			AND last.company_name = identities.company_name AND last.refreshed_at = last_at
		ORDER BY identities.season_number DESC, last_at DESC, last.rank, identities.company_name`,
			accountID, result.From, to).Scan(&result.Players).Error; err != nil {
			return err
		}
		for index := range result.Players {
			player := &result.Players[index]
			player.Points = make([]StandingPoint, 0)
			player.Charted = player.IsUs || result.LatestAt != nil && player.LastAt.Equal(*result.LatestAt) && player.Rank <= top
			if !player.Charted {
				continue
			}
			// One actual latest observation per UTC bucket, plus the exact start.
			// The identity index bounds probes as the permanent history grows.
			if err := tx.Raw(`WITH bounds AS (
				SELECT ?::timestamptz AS lower_at, ?::timestamptz AS upper_at, ? * interval '1 second' AS width
			), sampled AS (
				SELECT point.refreshed_at AS at, point.net_worth_pence FROM bounds
				CROSS JOIN LATERAL generate_series(date_bin(width, lower_at, 'epoch'::timestamptz),
					date_bin(width, upper_at, 'epoch'::timestamptz), width) AS buckets(at)
				CROSS JOIN LATERAL (
					SELECT refreshed_at, net_worth_pence FROM standings_history
					WHERE season_number = ? AND company_name = ?
						AND refreshed_at >= greatest(lower_at, buckets.at) AND refreshed_at < buckets.at + width AND refreshed_at <= upper_at
					ORDER BY refreshed_at DESC LIMIT 1
				) AS point
			)
			SELECT at, net_worth_pence FROM sampled
			UNION SELECT ?::timestamptz, ?::bigint ORDER BY at`, result.From, to, result.BucketSeconds,
				player.SeasonNumber, player.CompanyName, player.FirstAt, player.FirstNetWorthPence).Scan(&player.Points).Error; err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return StandingsHistory{}, err
	}
	return result, nil
}
