package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/gameapi"
	"go.uber.org/zap"
)

const (
	loanHistoryRefreshInterval     = 6 * time.Hour
	loanHistoryRefreshTimeout      = 20 * time.Second
	loanHistoryMetadataSaveTimeout = 5 * time.Second
)

func loanHistoryRefreshDue(meta DatasetMeta, now time.Time) bool {
	last := maxTime(meta.AttemptedAt, meta.UpdatedAt)
	return last.IsZero() || now.Sub(last) >= loanHistoryRefreshInterval
}

// Loan history is reporting evidence, never debt or spending authority. Its
// failures leave the ordinary validated loans capture available to the planner.
func (r *Runner) recordLoanHistory(ctx context.Context, raw json.RawMessage, observedAt time.Time) error {
	if r.db == nil {
		return nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return err
	}
	if err := r.db.SyncLoanHistory(ctx, r.account.ID, rows, observedAt); err != nil {
		return err
	}
	r.meta["loan_history_recording"] = DatasetMeta{UpdatedAt: observedAt, Complete: true}
	return nil
}

func (r *Runner) loanHistoryFailure(ctx context.Context, err error) {
	r.loanHistoryError(ctx, "loan_history", err)
}

func (r *Runner) loanHistoryRecordingFailure(ctx context.Context, err error) {
	r.loanHistoryError(ctx, "loan_history_recording", err)
}

func (r *Runner) loanHistoryError(ctx context.Context, key string, err error) {
	meta := r.meta[key]
	level := zap.WarnLevel
	if meta.Stale && meta.Error != "" {
		level = zap.DebugLevel
	}
	meta.Stale, meta.Complete = true, false
	meta.Error = "loan history recording failed; keeping previously recorded loans"
	if key == "loan_history" {
		meta.Error = "borrower history refresh failed; keeping previously recorded loans"
	}
	r.meta[key] = meta
	if ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		if entry := r.log.Check(level, "loan history unavailable; trading continues"); entry != nil {
			entry.Write(r.scrubOutcomeFields(ctx, nil, []outcomeText{
				{field: "dataset", value: key}, {field: "cause", value: err.Error()},
			})...)
		}
	}
}

func (r *Runner) completeLoanHistoryCapture(result fetchResult) {
	r.meta["loan_history"] = DatasetMeta{
		UpdatedAt: result.completedAt, AttemptedAt: result.attemptedAt,
		Generation: r.generation, Complete: true,
	}
}

func (r *Runner) recordLoanReceipt(ctx context.Context, raw json.RawMessage, observedAt time.Time) {
	rows := encode([]json.RawMessage{raw})
	if err := validateBorrowerLoans(rows, r.account.CompanyID); err != nil {
		r.loanHistoryRecordingFailure(ctx, err)
		return
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	if err := r.recordLoanHistory(ctx, rows, observedAt); err != nil {
		r.loanHistoryRecordingFailure(ctx, err)
	}
}

func (r *Runner) refreshLoanHistory(ctx context.Context, now time.Time) {
	meta := r.meta["loan_history"]
	if !loanHistoryRefreshDue(meta, now) && !r.meta["loan_history_due"].UpdatedAt.After(meta.AttemptedAt) {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, loanHistoryRefreshTimeout)
	defer cancel()
	previous := meta
	meta.AttemptedAt = now
	r.meta["loan_history"] = meta
	r.snapshot["_meta"] = encode(r.meta)
	// Reserve the attempt before reading so restarts and failed requests cannot
	// turn a six-hour backfill into a per-pass history scan.
	if err := r.saveSnapshot(bounded); err != nil {
		r.meta["loan_history"] = previous
		r.loanHistoryRecordingFailure(ctx, err)
		r.snapshot["_meta"] = encode(r.meta)
		return
	}
	defer func() {
		r.snapshot["_meta"] = encode(r.meta)
		persist, cancel := context.WithTimeout(ctx, loanHistoryMetadataSaveTimeout)
		defer cancel()
		if err := r.saveSnapshot(persist); err != nil {
			r.loanHistoryRecordingFailure(ctx, err)
		}
	}()
	raw, err := readLoanHistoryPages(bounded, r.api)
	if err != nil {
		r.loanHistoryFailure(ctx, err)
		return
	}
	raw, skipped, err := reportedLoanHistory(raw, r.account.CompanyID)
	if err != nil {
		r.loanHistoryFailure(ctx, err)
		return
	}
	completed := time.Now().UTC()
	if err := r.recordLoanHistory(bounded, raw, completed); err != nil {
		r.loanHistoryFailure(ctx, err)
		return
	}
	r.completeLoanHistoryCapture(fetchResult{attemptedAt: now, completedAt: completed})
	if skipped > 0 {
		meta := r.meta["loan_history"]
		meta.Complete = false
		meta.Error = fmt.Sprintf("Skipped %d malformed borrower history rows; valid rows were retained.", skipped)
		r.meta["loan_history"] = meta
		r.log.Warn("borrower history contains malformed rows; valid rows retained", zap.Int("skipped_rows", skipped))
	}
}

func reportedLoanHistory(raw json.RawMessage, companyID string) (json.RawMessage, int, error) {
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return nil, 0, errors.New("borrower history is not an array")
	}
	retained := make([]json.RawMessage, 0, len(rows))
	skipped := 0
	for _, row := range rows {
		var loan gameapi.Loan
		if err := json.Unmarshal(row, &loan); err != nil ||
			strings.TrimSpace(loan.ID) == "" || len(loan.ID) > 255 ||
			strings.TrimSpace(loan.Status) == "" || len(loan.Status) > 255 ||
			loan.BorrowerCompanyID != "" && loan.BorrowerCompanyID != companyID ||
			loan.PrincipalRepaidPence < 0 || loan.InterestPaidPence < 0 {
			skipped++
			continue
		}
		retained = append(retained, row)
	}
	result, err := json.Marshal(retained)
	return result, skipped, err
}

func (r *Runner) observeLoanHistoryClosures(raw json.RawMessage, observedAt time.Time) {
	var current []gameapi.Loan
	if err := json.Unmarshal(raw, &current); err != nil {
		return
	}
	open := make(map[string]bool, len(current))
	for _, loan := range current {
		if loan.Status == "active" || loan.Status == "defaulted" {
			open[loan.ID] = true
		}
	}
	for _, loan := range decode[[]gameapi.Loan](r.snapshot, "loans") {
		if (loan.Status == "active" || loan.Status == "defaulted") && !open[loan.ID] {
			r.meta["loan_history_due"] = DatasetMeta{UpdatedAt: observedAt}
			return
		}
	}
}
