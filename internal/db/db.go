package db

import (
	"context"
	"database/sql"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"moul.io/zapgorm2"

	"github.com/DevYukine/go-tradewinds/internal/config"
)

const (
	maxOpenConns    = 25
	maxIdleConns    = 5
	connMaxLifetime = 5 * time.Minute
)

// Module provides the GORM database connection and retention pruner to the fx DI container.
var Module = fx.Module("db",
	fx.Provide(NewConnection),
	fx.Invoke(RegisterRetentionPruner),
)

// NewConnection creates a GORM database connection, runs auto-migrations,
// configures connection pooling, and registers lifecycle hooks for clean shutdown.
func NewConnection(lc fx.Lifecycle, cfg *config.Config, logger *zap.Logger) (*gorm.DB, error) {
	log := logger.Named("db")

	gormLog := zapgorm2.New(log)
	gormLog.IgnoreRecordNotFoundError = true
	gormLog.SetAsDefault()

	gormCfg := &gorm.Config{
		Logger: gormLog,
	}

	db, err := gorm.Open(postgres.Open(cfg.DB.DSN()), gormCfg)
	if err != nil {
		return nil, err
	}

	// Configure connection pool.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)

	// Run auto-migrations.
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return nil, err
	}

	log.Info("database connected and migrated",
		zap.String("host", cfg.DB.Host),
		zap.String("database", cfg.DB.Name),
	)

	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			log.Info("closing database connection")
			return closeDB(sqlDB)
		},
	})

	return db, nil
}

// closeDB closes the underlying sql.DB connection pool.
func closeDB(sqlDB *sql.DB) error {
	return sqlDB.Close()
}
