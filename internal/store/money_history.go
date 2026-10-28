package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// MoneyHistory keeps sampled balance observations separate from additive,
// durable settlement accounting. No shared row limit can crowd out a series.
type MoneyHistory struct {
	AccountID     string        `json:"account_id"`
	GeneratedAt   time.Time     `json:"generated_at"`
	From          time.Time     `json:"from"`
	To            time.Time     `json:"to"`
	BucketSeconds int64         `json:"bucket_seconds"`
	Metrics       []Metric      `json:"metrics"`
	Settlements   []MoneyPeriod `json:"settlements"`
}

// Gross includes all recorded proceeds. Costs and net include only settlements
// with a known basis, and remain null when a period has none. Missing periods
// are not filled with invented zero activity.
type MoneyPeriod struct {
	At                   time.Time `json:"at"`
	To                   time.Time `json:"to"`
	Count                int64     `json:"count"`
	KnownCount           int64     `json:"known_count"`
	GrossPence           int64     `json:"gross_pence"`
	UnknownProceedsCount int64     `json:"unknown_proceeds_count,omitempty"`
	CostsPence           *int64    `json:"costs_pence"`
	NetPence             *int64    `json:"net_pence"`
}

func (value MoneyPeriod) MarshalJSON() ([]byte, error) {
	type moneyPeriodJSON MoneyPeriod
	var gross *int64
	if value.UnknownProceedsCount == 0 {
		gross = &value.GrossPence
	}
	return json.Marshal(struct {
		moneyPeriodJSON
		GrossPence *int64 `json:"gross_pence"`
	}{moneyPeriodJSON: moneyPeriodJSON(value), GrossPence: gross})
}

func moneyBucketSeconds(spanSeconds int64) int64 {
	for _, seconds := range []int64{60, 300, 1800, 7200, 21600, 86400, 604800} {
		if spanSeconds <= seconds*480 {
			return seconds
		}
	}
	return spanSeconds/480 + 1
}

// MoneyHistory reads the inclusive host-time range in one consistent snapshot.
// A zero from means all recorded history up to to, not the latest 500 records.
func (s *Store) MoneyHistory(ctx context.Context, accountID string, from, to time.Time) (MoneyHistory, error) {
	if !validID(accountID) || to.IsZero() || !from.IsZero() && from.After(to) {
		return MoneyHistory{}, fmt.Errorf("money history range: %w", ErrInvalidInput)
	}
	result := MoneyHistory{
		AccountID: accountID, GeneratedAt: time.Now().UTC(), From: from.UTC(), To: to.UTC(),
		Metrics: make([]Metric, 0), Settlements: make([]MoneyPeriod, 0),
	}
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Transaction(func(tx *gorm.DB) error {
		if from.IsZero() {
			var first sql.NullTime
			if err := tx.Raw(`SELECT min(at) FROM (
				SELECT earliest.recorded_at AS at FROM
					(VALUES ('liquid_funds_pence'), ('net_worth_pence'), ('cash_outflow_pence'), ('expenses_pence')) AS series(name)
				CROSS JOIN LATERAL (
					SELECT recorded_at FROM metrics WHERE account_id = ? AND name = series.name AND recorded_at <= ?
					ORDER BY recorded_at ASC, id ASC LIMIT 1
				) AS earliest
				UNION ALL
				(SELECT completed_at FROM settlements WHERE account_id = ? AND completed_at <= ?
					ORDER BY completed_at ASC LIMIT 1)
			) AS starts`, accountID, to, accountID, to).Row().Scan(&first); err != nil {
				return err
			}
			result.From = result.To
			if first.Valid {
				result.From = first.Time.UTC()
			}
		}
		result.BucketSeconds = moneyBucketSeconds(result.To.Unix() - result.From.Unix() + 1)
		// Each bucket probes metrics_latest_idx for one actual observation per
		// series. Work stays bounded as retained metric history grows; overlapping
		// ledger snapshots are neither summed nor scanned for a coverage count.
		if err := tx.Raw(`WITH bounds AS (
				SELECT ?::timestamptz AS lower_at, ?::timestamptz AS upper_at, ? * interval '1 second' AS width
			)
			SELECT sampled.id, sampled.account_id, sampled.name, sampled.value, sampled.recorded_at
			FROM bounds
			CROSS JOIN LATERAL generate_series(
				date_bin(bounds.width, bounds.lower_at, 'epoch'::timestamptz),
				date_bin(bounds.width, bounds.upper_at, 'epoch'::timestamptz), bounds.width
			) AS buckets(at)
			CROSS JOIN (VALUES ('liquid_funds_pence'), ('net_worth_pence'), ('cash_outflow_pence'), ('expenses_pence')) AS series(name)
			CROSS JOIN LATERAL (
				SELECT id, account_id, name, value, recorded_at FROM metrics
				WHERE account_id = ? AND name = series.name
					AND recorded_at >= greatest(bounds.lower_at, buckets.at)
					AND recorded_at < buckets.at + bounds.width AND recorded_at <= bounds.upper_at
				ORDER BY recorded_at DESC, id DESC LIMIT 1
			) AS sampled
			ORDER BY sampled.recorded_at, sampled.id`, result.From, result.To, result.BucketSeconds, accountID).
			Scan(&result.Metrics).Error; err != nil {
			return err
		}
		// Numeric arithmetic avoids bigint overflow before aggregation. Scanning
		// back to integer pence fails rather than silently rounding an overflow.
		// Edge periods expose only their requested span, not a full bucket label.
		return tx.Raw(`WITH bucketed AS (
				SELECT *, date_bin(? * interval '1 second', completed_at, 'epoch'::timestamptz) AS bucket
				FROM settlements WHERE account_id = ? AND completed_at >= ? AND completed_at <= ?
			)
			SELECT greatest(bucket, ?::timestamptz) AS at,
				least(bucket + ? * interval '1 second', ?::timestamptz) AS to,
				count(*) AS count, count(*) FILTER (WHERE costs_known) AS known_count,
				sum(proceeds_pence::numeric) AS gross_pence,
				count(*) FILTER (WHERE proceeds_unknown) AS unknown_proceeds_count,
				sum(acquisition_pence::numeric + fees_pence + provisions_pence + expenses_pence)
					FILTER (WHERE costs_known) AS costs_pence,
				sum(proceeds_pence::numeric - acquisition_pence - fees_pence - provisions_pence - expenses_pence)
					FILTER (WHERE costs_known) AS net_pence
			FROM bucketed GROUP BY bucket ORDER BY bucket`,
			result.BucketSeconds, accountID, result.From, result.To, result.From, result.BucketSeconds, result.To).
			Scan(&result.Settlements).Error
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return MoneyHistory{}, err
	}
	return result, nil
}
