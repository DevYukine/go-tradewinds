package engine

import (
	"context"
	"encoding/json"
	"time"
)

// The game returns the company's whole treasury ledger, newest first. Keeping
// all of it grew the account snapshot past store.MaxSnapshotBytes and stopped
// the runner. The snapshot keeps a recent window plus whatever unresolved
// accounting still needs older entries.
const (
	ledgerRetention = 72 * time.Hour
	// Nothing unresolved can hold the whole history hostage: voyages, funding
	// proof and futures settle within hours, so anything older is stuck. At
	// about 0.5 MB of ledger per active day, four days still leaves the
	// snapshot under its limit.
	ledgerMaxRetention = 96 * time.Hour
	// Ledger stamps are game-server time; tolerate host clock skew.
	ledgerClockMargin = time.Hour
)

// ledgerFloor is the oldest ledger creation time the snapshot must keep:
//   - expense allocation counts entries absent from a mission's
//     InitialLedgerIDs, so entries since that mission started must stay;
//   - underwriting funding proof matches the collateral debit after acceptance;
//   - futures cash flows are matched until the intent's accounting is final.
func (r *Runner) ledgerFloor(ctx context.Context, now time.Time) (time.Time, error) {
	floor := now.Add(-ledgerRetention)
	extend := func(start time.Time) {
		if start.IsZero() {
			return
		}
		if start = start.Add(-ledgerClockMargin); start.Before(floor) {
			floor = start
		}
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return time.Time{}, err
	}
	for _, mission := range missions {
		var data struct {
			StartedAt        time.Time       `json:"started_at"`
			InitialLedgerIDs map[string]bool `json:"initial_ledger_ids"`
		}
		// Control missions such as __futures carry started_at but no baseline
		// and never claim ledger expenses. An empty baseline is still one.
		if json.Unmarshal(mission.Data, &data) != nil || data.InitialLedgerIDs == nil {
			continue
		}
		extend(data.StartedAt)
	}
	// The collateral debit and futures opening flows are booked at acceptance.
	// Past the cap they are gone, so a stale position or intent must not pin
	// the window at the cap for good.
	oldest := now.Add(-ledgerMaxRetention)
	positions, err := r.db.UnderwritingPositions(ctx, r.account.ID)
	if err != nil {
		return time.Time{}, err
	}
	for _, position := range positions {
		if len(position.FundingEvidence) == 0 && position.AcceptedAt.After(oldest) {
			extend(position.AcceptedAt)
		}
	}
	intents, err := r.db.FuturesIntents(ctx, r.account.ID)
	if err != nil {
		return time.Time{}, err
	}
	for _, intent := range intents {
		if !futuresAccounted(intent) && intent.CreatedAt.After(oldest) {
			extend(intent.CreatedAt)
		}
	}
	if floor.Before(oldest) {
		floor = oldest
	}
	return floor, nil
}
