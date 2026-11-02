package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"go.uber.org/zap"
)

// Retain unreadable timestamps for normal validators. RawMessage preserves fields
// outside the engine's models without maintaining a second row representation.
func retainRecentRows(raw json.RawMessage, cutoff time.Time, timestamp func(json.RawMessage) time.Time) (json.RawMessage, error) {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || rows == nil {
		return raw, nil
	}
	kept := rows[:0]
	for _, row := range rows {
		if at := timestamp(row); !at.IsZero() && at.Before(cutoff) {
			continue
		}
		kept = append(kept, row)
	}
	if len(kept) == len(rows) {
		return raw, nil
	}
	return json.Marshal(kept)
}

type snapshotRetention struct {
	keepContracts bool
	keepInsurance bool
	keepAll       bool
}

func snapshotNeedsRetentionState(key string) bool {
	return key == "contracts" || key == "insurance" || key == "insurance_owned"
}

func (r *Runner) snapshotRetention(ctx context.Context, now time.Time) *snapshotRetention {
	retention := &snapshotRetention{}
	if r.db == nil {
		return retention
	}
	keepAll := func(err error) *snapshotRetention {
		r.log.Warn("snapshot retention skipped; pending recovery state unavailable", zap.Error(err))
		return &snapshotRetention{keepAll: true}
	}
	missions, err := r.db.Missions(ctx, r.account.ID)
	if err != nil {
		return keepAll(err)
	}
	// One day's margin protects a seven-day history before an unresolved
	// mission or action can outlive it. Normal voyages finish within hours.
	cutoff := now.Add(-6 * 24 * time.Hour)
	for _, mission := range missions {
		var data missionData
		if err := json.Unmarshal(mission.Data, &data); err != nil {
			return keepAll(err)
		}
		if data.Action == nil && (data.Settlement != nil || mission.Phase == "done") {
			continue
		}
		// Maintenance and control missions do not reconcile these histories.
		var contractMission, policyMission bool
		switch data.Plan.Kind {
		case "":
			contractMission = data.Commission != nil
		case "spot", "charter", "freight", "spot+freight", "charter+freight", "relocation", "recovery", "aborted":
			contractMission = true
			fallthrough
		case "passage":
			policyMission = len(data.VoyageInsurance) > 0 ||
				data.Action != nil && strings.HasPrefix(data.Action.Path, "/v1/insurance/")
		case "underwriting":
			policyMission = data.Underwriting != nil
		}
		if !contractMission && !policyMission {
			continue
		}
		startedAt := data.StartedAt
		if startedAt.IsZero() {
			startedAt = mission.UpdatedAt
			if data.Action != nil && !data.Action.CreatedAt.IsZero() {
				startedAt = data.Action.CreatedAt
			}
		}
		old := !startedAt.IsZero() && startedAt.Before(cutoff)
		if data.Action != nil && !data.Action.CreatedAt.IsZero() && data.Action.CreatedAt.Before(cutoff) {
			old = true
		}
		if old {
			retention.keepContracts = retention.keepContracts || contractMission
			retention.keepInsurance = retention.keepInsurance || policyMission
		}
	}
	actions, err := r.db.PendingActions(ctx, r.account.ID)
	if err != nil {
		return keepAll(err)
	}
	for _, action := range actions {
		if action.CreatedAt.IsZero() || !action.CreatedAt.Before(cutoff) {
			continue
		}
		retention.keepContracts = retention.keepContracts || strings.HasPrefix(action.Path, "/v1/contract/")
		retention.keepInsurance = retention.keepInsurance || strings.HasPrefix(action.Path, "/v1/insurance/")
	}
	positions, err := r.db.UnderwritingPositions(ctx, r.account.ID)
	if err != nil {
		return keepAll(err)
	}
	for _, position := range positions {
		if !position.AcceptedAt.IsZero() && position.AcceptedAt.Before(cutoff) && (position.ClosedAt == nil || position.ClosedAt.IsZero()) {
			retention.keepInsurance = true
			break
		}
	}
	return retention
}

func snapshotRowTimestamp(key string, raw json.RawMessage) time.Time {
	var row struct {
		Status    string          `json:"status"`
		CreatedAt json.RawMessage `json:"created_at"`
		UpdatedAt json.RawMessage `json:"updated_at"`
		ExpiresAt json.RawMessage `json:"expires_at"`
	}
	if json.Unmarshal(raw, &row) != nil {
		return time.Time{}
	}
	stamp := row.ExpiresAt
	switch key {
	case "fills":
		stamp = row.CreatedAt
	case "contracts":
		if row.Status == "completed" || row.Status == "cancelled" || row.Status == "defaulted" {
			stamp = row.UpdatedAt
		}
	case "insurance", "insurance_owned":
		if row.Status == "settled_safe" || row.Status == "claimed_loss" || row.Status == "cancelled" {
			stamp = row.UpdatedAt
		}
	}
	var at time.Time
	_ = json.Unmarshal(stamp, &at)
	return at
}
