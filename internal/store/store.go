package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlog "gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrations embed.FS

var migrationMu sync.Mutex

const queryTimeout = 10 * time.Second

type Store struct {
	db        *gorm.DB
	sqlDB     *sql.DB
	dsn       string
	aead      cipher.AEAD
	lookupKey []byte
	log       *zap.Logger

	leaseMu sync.Mutex
	leases  map[*accountLease]struct{}
	closed  bool
}

func Open(ctx context.Context, dsn string, masterKey []byte, log *zap.Logger) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("database URL: %w", ErrInvalidInput)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("credential master key: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("credential encryption: %w", err)
	}
	if log == nil {
		log = zap.NewNop()
	}

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", databaseError{cause: err})
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(openCtx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", databaseError{cause: err})
	}

	migrationCtx, migrationCancel := context.WithTimeout(ctx, time.Minute)
	defer migrationCancel()
	privateStore := &Store{
		sqlDB: sqlDB, dsn: dsn, aead: aead, lookupKey: deriveLookupKey(masterKey),
		log: log, leases: make(map[*accountLease]struct{}),
	}
	if err := migrate(migrationCtx, sqlDB, log, privateStore); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing: true,
		Logger:               gormlog.Default.LogMode(gormlog.Silent),
		TranslateError:       true,
	})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initialize database models: %w", databaseError{cause: err})
	}
	authCtx, authCancel := context.WithTimeout(ctx, queryTimeout)
	defer authCancel()
	if err := privateStore.verifyKey(db.WithContext(authCtx)); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	privateStore.db = db
	return privateStore, nil
}

func migrate(ctx context.Context, db *sql.DB, log *zap.Logger, privateStore *Store) error {
	migrationMu.Lock()
	defer migrationMu.Unlock()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", databaseError{cause: err})
	}
	defer conn.Close()
	const migrationLock int64 = 6077406759557356871
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("acquire migration lock: %w", databaseError{cause: err})
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationLock); err != nil {
			log.Warn("release migration lock", zap.Error(databaseError{cause: err}))
			_ = conn.Raw(func(any) error {
				return driver.ErrBadConn
			})
		}
	}()
	migrationFS, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationFS,
		goose.WithLogger(migrationLogger{log: log}),
		goose.WithDisableGlobalRegistry(true),
		goose.WithGoMigrations(goose.NewGoMigration(5, &goose.GoFunc{RunTx: privateStore.migratePrivateEvidence}, &goose.GoFunc{
			RunTx: func(context.Context, *sql.Tx) error {
				return errors.New("private evidence cutover requires restoring a pre-migration backup")
			},
		})),
	)
	if err != nil {
		return fmt.Errorf("configure migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("migrate PostgreSQL: %w", databaseError{cause: err})
	}
	return nil
}

// Driver errors may embed the entire DSN, TLS configuration or SQL values.
// Keep the cause for errors.Is/As, but only expose safe classifications in text.
type databaseError struct {
	cause error
}

func (e databaseError) Error() string {
	switch {
	case errors.Is(e.cause, context.Canceled):
		return context.Canceled.Error()
	case errors.Is(e.cause, context.DeadlineExceeded):
		return context.DeadlineExceeded.Error()
	}
	var parseError *pgconn.ParseConfigError
	if errors.As(e.cause, &parseError) {
		return "invalid DATABASE_URL (connection details withheld)"
	}
	var postgresError *pgconn.PgError
	if errors.As(e.cause, &postgresError) {
		return "PostgreSQL rejected operation (SQLSTATE " + postgresError.Code + ")"
	}
	return "database operation failed (connection details withheld)"
}

func (e databaseError) Unwrap() error {
	return e.cause
}

type migrationLogger struct {
	log *zap.Logger
}

func (l migrationLogger) Printf(format string, args ...any) {
	if entry := l.log.Check(zap.DebugLevel, "database migration"); entry != nil {
		entry.Write(zap.String("detail", migrationMessage(format, args)))
	}
}

func (l migrationLogger) Fatalf(format string, args ...any) {
	l.log.Error("database migration failed", zap.String("detail", migrationMessage(format, args)))
}

func migrationMessage(format string, args []any) string {
	for i, value := range args {
		if err, ok := value.(error); ok {
			args[i] = databaseError{cause: err}
		}
	}
	return fmt.Sprintf(format, args...)
}

func (s *Store) Close() error {
	s.leaseMu.Lock()
	if s.closed {
		s.leaseMu.Unlock()
		return nil
	}
	s.closed = true
	leases := make([]*accountLease, 0, len(s.leases))
	for lease := range s.leases {
		leases = append(leases, lease)
	}
	s.leaseMu.Unlock()
	for _, lease := range leases {
		lease.cancel()
	}
	for _, lease := range leases {
		<-lease.done
	}
	return s.sqlDB.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.sqlDB.PingContext(ctx)
}

func (s *Store) query(ctx context.Context) (*gorm.DB, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	return s.db.WithContext(ctx), cancel
}

func (s *Store) encrypt(plaintext []byte, associatedData string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize(), s.aead.NonceSize()+len(plaintext)+s.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, []byte(associatedData)), nil
}

func (s *Store) decrypt(ciphertext []byte, associatedData string) ([]byte, error) {
	if len(ciphertext) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, errors.New("encrypted value is truncated")
	}
	nonce := ciphertext[:s.aead.NonceSize()]
	plaintext, err := s.aead.Open(nil, nonce, ciphertext[s.aead.NonceSize():], []byte(associatedData))
	if err != nil {
		return nil, errors.New("encrypted value authentication failed; verify the master key and account identity")
	}
	return plaintext, nil
}

func validID(id string) bool {
	return strings.TrimSpace(id) != "" && len(id) <= 255
}

func (s *Store) SaveAccount(ctx context.Context, account Account, credentials Credentials) error {
	if !validID(account.ID) || len(account.Label) > 255 || len(account.CompanyID) > 255 {
		return fmt.Errorf("account fields: %w", ErrInvalidInput)
	}
	plaintext, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	ciphertext, err := s.encrypt(plaintext, account.ID)
	if err != nil {
		return err
	}
	if account.CreatedAt.IsZero() {
		account.CreatedAt = time.Now().UTC()
	}
	return s.writeOwned(ctx, account.ID, func(tx *gorm.DB) error {
		var existing Account
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&existing, "id = ?", account.ID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && existing.CompanyID != account.CompanyID {
			var pending int64
			if err := tx.Model(&actionRow{}).Where("account_id = ? AND state = ?", account.ID, ActionPending).Count(&pending).Error; err != nil {
				return err
			}
			if pending != 0 {
				return fmt.Errorf("cannot change company with pending actions: %w", ErrActionConflict)
			}
		}
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{"label", "company_id", "enabled"}),
		}).Create(&account).Error; err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"ciphertext"}),
		}).Create(&accountSecret{AccountID: account.ID, Ciphertext: ciphertext}).Error
	})
}

func (s *Store) Accounts(ctx context.Context) ([]Account, error) {
	accounts := make([]Account, 0)
	db, cancel := s.query(ctx)
	defer cancel()
	err := db.Order("created_at, id").Find(&accounts).Error
	return accounts, err
}

func (s *Store) Credentials(ctx context.Context, id string) (Credentials, error) {
	if !validID(id) {
		return Credentials{}, fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	return s.credentials(db, id)
}

func (s *Store) credentials(db *gorm.DB, id string) (Credentials, error) {
	var row accountSecret
	if err := db.First(&row, "account_id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return Credentials{}, ErrNotFound
		}
		return Credentials{}, err
	}
	return s.decodeCredentials(row)
}

func (s *Store) decodeCredentials(row accountSecret) (Credentials, error) {
	plaintext, err := s.decrypt(row.Ciphertext, row.AccountID)
	if err != nil {
		return Credentials{}, err
	}
	var credentials Credentials
	if err := json.Unmarshal(plaintext, &credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode stored credentials: %w", err)
	}
	return credentials, nil
}

func (s *Store) authenticatedSecrets(db *gorm.DB) (map[string][]string, error) {
	var rows []accountSecret
	if err := db.Find(&rows).Error; err != nil {
		return nil, databaseError{cause: err}
	}
	secrets := make(map[string][]string, len(rows))
	for _, row := range rows {
		credentials, err := s.decodeCredentials(row)
		if err != nil {
			return nil, err
		}
		secrets[row.AccountID] = credentialSecrets(credentials)
	}
	return secrets, nil
}

func (s *Store) SaveSnapshot(ctx context.Context, accountID string, data map[string]json.RawMessage) error {
	secrets, err := s.secrets(ctx, accountID)
	if err != nil {
		return err
	}
	if data == nil {
		data = make(map[string]json.RawMessage)
	}
	remaining := maxSnapshotBytes
	for key, raw := range data {
		if len(key) > remaining {
			return ErrResponseTooLarge
		}
		remaining -= len(key)
		if len(raw) > remaining {
			return ErrResponseTooLarge
		}
		remaining -= len(raw)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	encoded, err = redactJSON(encoded, secrets, 0)
	if err != nil {
		return err
	}
	if len(encoded) > maxSnapshotBytes {
		return ErrResponseTooLarge
	}
	row := snapshotRow{AccountID: accountID, Data: encoded, UpdatedAt: time.Now().UTC()}
	return s.writeOwned(ctx, accountID, func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "account_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"data", "updated_at"}),
		}).Create(&row).Error
	})
}

func (s *Store) Snapshot(ctx context.Context, accountID string) (map[string]json.RawMessage, error) {
	if !validID(accountID) {
		return nil, fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	db, cancel := s.query(ctx)
	defer cancel()
	var row snapshotRow
	if err := db.First(&row, "account_id = ?", accountID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return make(map[string]json.RawMessage), nil
		}
		return nil, err
	}
	data := make(map[string]json.RawMessage)
	if err := json.Unmarshal(row.Data, &data); err != nil {
		return nil, fmt.Errorf("decode stored snapshot: %w", err)
	}
	return data, nil
}
