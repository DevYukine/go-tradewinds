package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// GrowthHistory is a recorded-host-time admission cohort, not lifetime profit.
// FutureCompletion vetoes proof even though future rows do not enter its totals.
type GrowthHistory struct {
	EconomicHistory
	FutureCompletion   bool
	ControlCloseoutIDs json.RawMessage
}

// GrowthHistory includes whole settlements completed in [asOf-12h, asOf].
// StartedAt and accelerated game time do not determine cohort membership.
// Relevant future completions remain visible as clock-incoherent evidence.
func (s *Store) GrowthHistory(ctx context.Context, accountID, lane string, asOf time.Time) (GrowthHistory, error) {
	if asOf.IsZero() {
		return GrowthHistory{}, fmt.Errorf("growth history as-of time: %w", ErrInvalidInput)
	}
	return s.economicHistory(ctx, accountID, lane, asOf.UTC(), 12*time.Hour, false)
}
