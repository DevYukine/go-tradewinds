package sim

import (
	"context"
	"fmt"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/engine"
	"github.com/DevYukine/go-tradewinds/internal/store"
)

// loadWindowActions adds only window actions and exact receipts needed by
// settlements crossing its start. Unrelated lifetime bodies never enter memory.
func (t *Tape) loadWindowActions(ctx context.Context, db *store.Store, account string, from, to time.Time) error {
	seen := make(map[string]bool)
	retained := 0
	t.Actions = make([]store.Action, 0)
	appendActions := func(rows []store.Action) error {
		for _, action := range rows {
			if seen[action.ID] {
				continue
			}
			size := 512 + len(action.ID) + len(action.AccountID) + len(action.Method) + len(action.Path) + len(action.State) + cap(action.Body) + cap(action.Response)
			if len(t.Actions) >= store.MaxReplayEvidenceRows || size > store.MaxReplayEvidenceRetainedBytes-retained {
				return fmt.Errorf("window actions and related receipts exceed the retained evidence budget: %w", store.ErrReplayEvidenceBudget)
			}
			retained += size
			seen[action.ID] = true
			t.Actions = append(t.Actions, action)
		}
		return nil
	}
	rows, err := db.ActionsBetween(ctx, account, from, to)
	if err != nil {
		return err
	}
	if err := appendActions(rows); err != nil {
		return err
	}
	pending := make([]string, 0, 1024)
	pendingIDs := make(map[string]bool)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		rows, err := db.ReplayActionsByID(ctx, account, pending, to)
		if err != nil {
			return err
		}
		if err := appendActions(rows); err != nil {
			return err
		}
		pending = pending[:0]
		clear(pendingIDs)
		return nil
	}
	addID := func(id string) error {
		if id == "" || seen[id] || pendingIDs[id] {
			return nil
		}
		pending = append(pending, id)
		pendingIDs[id] = true
		if len(pending) == cap(pending) {
			return flush()
		}
		return nil
	}
	for _, link := range t.Links {
		if err := addID(link.ActionID); err != nil {
			return err
		}
	}
	for _, row := range t.Settlements {
		if err := addID(row.ActionID); err != nil {
			return err
		}
		if row.MissionID == "" || !row.StartedAt.Before(from) {
			continue
		}
		// Preserve the validator's deterministic-ID fallback even when no
		// replay link was retained for a pre-window mission stage.
		for _, stage := range receiptStages {
			for sequence := int64(0); sequence < 512; sequence++ {
				if err := addID(engine.ActionID(account, row.MissionID, stage, sequence)); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func (t *Tape) charterProceedsBefore(since, before time.Time) (int64, bool, error) {
	if t.readCharterPrefix == nil {
		return 0, false, nil
	}
	t.seedMu.Lock()
	defer t.seedMu.Unlock()
	if t.charterCached && t.charterSince.Equal(since) && t.charterBefore.Equal(before) {
		return t.charterProceeds, true, nil
	}
	proceeds, err := t.readCharterPrefix(since, before)
	if err != nil {
		return 0, true, err
	}
	t.charterSince, t.charterBefore = since, before
	t.charterProceeds, t.charterCached = proceeds, true
	return proceeds, true, nil
}

func (t *Tape) probeCompletionsBefore(lanes []string, before time.Time) (map[string]time.Time, error) {
	if t.readProbePrefix == nil || len(lanes) == 0 {
		return nil, nil
	}
	t.seedMu.Lock()
	defer t.seedMu.Unlock()
	if t.probePrefixes == nil {
		t.probePrefixes = make(map[probePrefixKey]time.Time)
	}
	result := make(map[string]time.Time)
	missing := make([]string, 0, min(len(lanes), 1024))
	pending := make(map[string]bool)
	pendingBytes := 0
	flush := func() error {
		if len(missing) == 0 {
			return nil
		}
		rows, err := t.readProbePrefix(missing, before)
		if err != nil {
			return err
		}
		for _, lane := range missing {
			at := rows[lane]
			t.probePrefixes[probePrefixKey{lane, before}] = at
			if !at.IsZero() {
				result[lane] = at
			}
		}
		t.probePrefixBytes += pendingBytes
		missing = missing[:0]
		clear(pending)
		pendingBytes = 0
		return nil
	}
	for _, lane := range lanes {
		if at, exists := t.probePrefixes[probePrefixKey{lane, before}]; exists {
			if !at.IsZero() {
				result[lane] = at
			}
			continue
		}
		if pending[lane] {
			continue
		}
		size := len(lane) + 160
		if size > maxTapeRetainedBytes-t.probePrefixBytes-pendingBytes {
			return nil, fmt.Errorf("necessary probe-prefix cache exceeds %d bytes: %w", maxTapeRetainedBytes, store.ErrReplayEvidenceBudget)
		}
		pendingBytes += size
		pending[lane] = true
		missing = append(missing, lane)
		if len(missing) == cap(missing) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return result, nil
}
