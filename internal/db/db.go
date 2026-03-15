package db

import (
	"context"
	"database/sql"
	"embed"
	"time"

	"github.com/pressly/goose/v3"
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

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Module provides the GORM database connection and retention pruner to the fx DI container.
var Module = fx.Module("db",
	fx.Provide(NewConnection),
	fx.Invoke(RegisterRetentionPruner),
)

// NewConnection creates a GORM database connection, runs goose migrations,
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

	// Run goose migrations.
	if err := runMigrations(sqlDB, log); err != nil {
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

// runMigrations applies pending goose migrations from the embedded filesystem.
func runMigrations(sqlDB *sql.DB, logger *zap.Logger) error {
	goose.SetLogger(&gooseLogger{logger: logger.Named("goose")})
	goose.SetBaseFS(migrationsFS)

	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}

	return goose.Up(sqlDB, "migrations")
}

// closeDB closes the underlying sql.DB connection pool.
func closeDB(sqlDB *sql.DB) error {
	return sqlDB.Close()
}
