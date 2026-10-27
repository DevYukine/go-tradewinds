package main

import (
	"context"
	"errors"
	"time"

	"github.com/DevYukine/go-tradewinds/internal/store"
	"go.uber.org/zap"
)

const warningPersistEvery = 30 * time.Minute

type warningKey struct {
	accountID string
	code      string
}

type activeWarning struct {
	warning    store.Warning
	label      string
	raisedAt   time.Time
	lastLogged time.Time
}

// The process-wide watcher survives an individual account runner stopping. Read
// failures preserve active warnings: unavailable data is never an all-clear.
func watchWarnings(ctx context.Context, log *zap.Logger, source func(context.Context, time.Time) ([]store.AccountWarnings, error), now func() time.Time, tick <-chan time.Time) {
	active := make(map[warningKey]activeWarning)
	seen := make(map[warningKey]bool)
	assessed := make(map[string]bool)
	accountUnavailable := make(map[string]bool)
	unavailable := false
	for {
		if ctx.Err() != nil {
			return
		}
		at := now().UTC()
		accounts, err := source(ctx, at)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !unavailable {
				log.Warn("engine warnings unavailable", zap.String("error", warningReadError(err)))
				unavailable = true
			}
		} else {
			unavailable = false
			clear(seen)
			clear(assessed)
			for _, account := range accounts {
				if !account.Available {
					if !accountUnavailable[account.AccountID] {
						log.Warn("engine warnings unavailable",
							zap.String("account", account.Label), zap.String("account_id", account.AccountID),
							zap.String("error", warningAccountError(account.ErrorCode)))
						accountUnavailable[account.AccountID] = true
					}
					continue
				}
				delete(accountUnavailable, account.AccountID)
				assessed[account.AccountID] = true
				for _, code := range account.Suppressed {
					seen[warningKey{accountID: account.AccountID, code: code}] = true
				}
				for _, warning := range account.Warnings {
					key := warningKey{accountID: account.AccountID, code: warning.Code}
					seen[key] = true
					previous, exists := active[key]
					if !exists {
						log.Warn("engine warning", warningLogFields(account.AccountID, account.Label, warning)...)
						previous.raisedAt, previous.lastLogged = at, at
					} else if at.Sub(previous.lastLogged) >= warningPersistEvery {
						log.Warn("engine warning persists", warningLogFields(account.AccountID, account.Label, warning)...)
						previous.lastLogged = at
					}
					previous.warning, previous.label = warning, account.Label
					active[key] = previous
				}
			}
			for key, previous := range active {
				if !assessed[key.accountID] || seen[key] {
					continue
				}
				fields := warningLogFields(key.accountID, previous.label, previous.warning)
				fields = append(fields, zap.Duration("active_for", at.Sub(previous.raisedAt)))
				log.Info("engine warning cleared", fields...)
				delete(active, key)
			}
		}
		select {
		case <-ctx.Done():
			return
		case _, open := <-tick:
			if !open {
				return
			}
		}
	}
}

func warningReadError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	return "database_unavailable"
}

func warningAccountError(code string) string {
	switch code {
	case store.WarningCredentialsUnavailable, store.WarningSnapshotInvalid, store.WarningSnapshotTooLarge:
		return code
	default:
		return "account_unavailable"
	}
}

func warningLogFields(accountID, label string, warning store.Warning) []zap.Field {
	fields := make([]zap.Field, 0, 6+len(warning.Facts)+len(warning.Names))
	fields = append(fields,
		zap.String("account", label), zap.String("account_id", accountID),
		zap.String("code", warning.Code), zap.String("severity", warning.Severity), zap.Time("since", warning.Since),
	)
	for key, value := range warning.Facts {
		fields = append(fields, zap.Int64(key, value))
	}
	for key, value := range warning.Names {
		fields = append(fields, zap.String(key, value))
	}
	if warning.Reason != "" {
		fields = append(fields, zap.String("reason", warning.Reason))
	}
	return fields
}
