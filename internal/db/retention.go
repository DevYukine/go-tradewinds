package db

import (
	"context"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

const (
	retentionCheckInterval = 1 * time.Hour

	retentionCompanyLog       = 24 * time.Hour      // 1 day
	retentionPriceObservation = 7 * 24 * time.Hour  // 7 days
	retentionAgentDecisionLog = 30 * 24 * time.Hour // 30 days
	retentionQuoteFailureLog  = 7 * 24 * time.Hour  // 7 days
)

// RetentionPruner periodically deletes old records to prevent unbounded DB growth.
type RetentionPruner struct {
	db     *gorm.DB
	logger *zap.Logger
}

// RegisterRetentionPruner starts the background retention pruner via fx lifecycle.
func RegisterRetentionPruner(lc fx.Lifecycle, db *gorm.DB, logger *zap.Logger) {
	pruner := &RetentionPruner{
		db:     db,
		logger: logger.Named("retention"),
	}

	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			go pruner.run(ctx)
			return nil
		},
		OnStop: func(_ context.Context) error {
			cancel()
			return nil
		},
	})
}

// run executes the pruning loop at regular intervals.
func (p *RetentionPruner) run(ctx context.Context) {
	// Run an initial prune on startup.
	p.prune()

	ticker := time.NewTicker(retentionCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("retention pruner stopped")
			return
		case <-ticker.C:
			p.prune()
		}
	}
}

// prune deletes records older than their retention period.
func (p *RetentionPruner) prune() {
	p.pruneTable("company_logs", retentionCompanyLog)
	p.pruneTable("price_observations", retentionPriceObservation)
	p.pruneTable("agent_decision_logs", retentionAgentDecisionLog)
	p.pruneTable("quote_failure_logs", retentionQuoteFailureLog)
}

// pruneTable deletes rows from the given table where created_at is older than maxAge.
func (p *RetentionPruner) pruneTable(table string, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)

	result := p.db.
		Table(table).
		Where("created_at < ?", cutoff).
		Delete(nil)

	if result.Error != nil {
		p.logger.Error("retention prune failed",
			zap.String("table", table),
			zap.Error(result.Error),
		)
		return
	}

	if result.RowsAffected > 0 {
		p.logger.Info("retention prune completed",
			zap.String("table", table),
			zap.Int64("rows_deleted", result.RowsAffected),
			zap.Time("cutoff", cutoff),
		)
	}
}
