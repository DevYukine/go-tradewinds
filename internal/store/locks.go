package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
)

var ErrOwnershipLost = errors.New("account runner ownership was lost")

type leaseContextKey struct{}

type accountLease struct {
	store          *Store
	accountID      string
	generation     int64
	key            int64
	companyKey     int64
	hasCompanyLock bool
	conn           *sql.Conn
	connMu         sync.Mutex
	db             *sql.DB
	cancel         context.CancelFunc
	done           chan struct{}
}

// AcquireAccount obtains an account's exclusive PostgreSQL session lock.
// Runners must use AcquireAccountContext and honor its returned context.
func (s *Store) AcquireAccount(ctx context.Context, accountID string) (func(), error) {
	_, release, err := s.AcquireAccountContext(ctx, accountID)
	return release, err
}

func (s *Store) AcquireAccountContext(ctx context.Context, accountID string) (context.Context, func(), error) {
	if !validID(accountID) {
		return nil, nil, fmt.Errorf("account ID: %w", ErrInvalidInput)
	}
	query, cancel := s.query(ctx)
	var identity struct {
		CompanyID string
	}
	result := query.Table("accounts").Select("company_id").Where("id = ?", accountID).Take(&identity)
	cancel()
	if result.Error != nil && !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, nil, result.Error
	}
	digest := sha256.Sum256([]byte("tradewinds:runner:account:" + accountID))
	key := int64(binary.BigEndian.Uint64(digest[:8]))
	var companyKey int64
	hasCompanyLock := identity.CompanyID != ""
	if hasCompanyLock {
		companyDigest := sha256.Sum256([]byte("tradewinds:runner:company:" + identity.CompanyID))
		companyKey = int64(binary.BigEndian.Uint64(companyDigest[:8]))
		hasCompanyLock = companyKey != key
	}

	// A separate one-connection pool prevents runners from occupying every
	// application query connection and never transparently replaces a lost lock.
	lockDB, err := sql.Open("pgx", s.dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open ownership connection: %w", err)
	}
	lockDB.SetMaxOpenConns(1)
	lockDB.SetMaxIdleConns(0)
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 5*time.Second)
	defer acquireCancel()
	conn, err := lockDB.Conn(acquireCtx)
	if err != nil {
		_ = lockDB.Close()
		return nil, nil, fmt.Errorf("connect ownership session: %w", err)
	}
	var acquired bool
	if err := conn.QueryRowContext(acquireCtx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		_ = conn.Close()
		_ = lockDB.Close()
		return nil, nil, fmt.Errorf("acquire account ownership: %w", err)
	}
	if !acquired {
		_ = conn.Close()
		_ = lockDB.Close()
		return nil, nil, ErrAccountLocked
	}
	if hasCompanyLock {
		if err := conn.QueryRowContext(acquireCtx, "SELECT pg_try_advisory_lock($1)", companyKey).Scan(&acquired); err != nil {
			_ = conn.Close()
			_ = lockDB.Close()
			return nil, nil, fmt.Errorf("acquire company ownership: %w", err)
		}
		if !acquired {
			_ = conn.Close()
			_ = lockDB.Close()
			return nil, nil, ErrAccountLocked
		}
	}

	ownerCtx, ownerCancel := context.WithCancel(ctx)
	lease := &accountLease{
		store:          s,
		accountID:      accountID,
		key:            key,
		companyKey:     companyKey,
		hasCompanyLock: hasCompanyLock,
		conn:           conn,
		db:             lockDB,
		cancel:         ownerCancel,
		done:           make(chan struct{}),
	}
	err = conn.QueryRowContext(
		acquireCtx,
		"UPDATE accounts SET owner_generation = owner_generation + 1 WHERE id = $1 RETURNING owner_generation",
		accountID,
	).Scan(&lease.generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		ownerCancel()
		lease.closeConnection()
		return nil, nil, fmt.Errorf("fence previous account owner: %w", err)
	}
	// Another owner may have changed routes since this Store last held the
	// account. The next save must compare against the current table.
	routeCache := s.snapshotRouteCache(accountID)
	routeCache.mu.Lock()
	routeCache.digests = nil
	clear(routeCache.fingerprint[:])
	routeCache.fingerprintValid = false
	routeCache.mu.Unlock()
	ownerCtx = context.WithValue(ownerCtx, leaseContextKey{}, lease)
	s.leaseMu.Lock()
	if s.closed {
		s.leaseMu.Unlock()
		ownerCancel()
		lease.closeConnection()
		return nil, nil, sql.ErrConnDone
	}
	s.leases[lease] = struct{}{}
	s.leaseMu.Unlock()
	go lease.monitor(ownerCtx)

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			ownerCancel()
			<-lease.done
		})
	}
	return ownerCtx, release, nil
}

func (lease *accountLease) owns(ctx context.Context) (bool, error) {
	// pgx retains its result reader until Scan closes the rows. Serialize the
	// entire result lifetime, not only database/sql's individual driver calls.
	lease.connMu.Lock()
	defer lease.connMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	const query = `SELECT count(*) FROM pg_locks
		WHERE locktype = 'advisory' AND pid = pg_backend_pid()
		  AND ((classid = $1::oid AND objid = $2::oid)
		       OR (classid = $3::oid AND objid = $4::oid))
		  AND objsubid = 1 AND granted`
	companyKey := lease.key
	expected := 1
	if lease.hasCompanyLock {
		companyKey = lease.companyKey
		expected++
	}
	bits := uint64(lease.key)
	companyBits := uint64(companyKey)
	var held int
	err := lease.conn.QueryRowContext(ctx, query,
		int64(bits>>32), int64(uint32(bits)),
		int64(companyBits>>32), int64(uint32(companyBits)),
	).Scan(&held)
	if err != nil {
		return false, databaseError{cause: err}
	}
	return held == expected, nil
}

func (lease *accountLease) monitor(ctx context.Context) {
	defer close(lease.done)
	defer func() {
		lease.closeConnection()
		lease.store.leaseMu.Lock()
		delete(lease.store.leases, lease)
		lease.store.leaseMu.Unlock()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			held, err := lease.owns(checkCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil || !held {
				lease.store.log.Error("account ownership connection lost",
					zap.String("account_id", lease.accountID),
					zap.Bool("lock_held", held),
					zap.Error(err),
				)
				lease.cancel()
				return
			}
		}
	}
}

func (lease *accountLease) closeConnection() {
	lease.connMu.Lock()
	defer lease.connMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := lease.conn.ExecContext(ctx, "SELECT pg_advisory_unlock_all()")
	if err != nil {
		lease.store.log.Warn("release account ownership connection",
			zap.String("account_id", lease.accountID),
			zap.Error(databaseError{cause: err}),
		)
	}
	// With no idle slots and the private pool closed, an uncertain unlock can
	// never return a still-locked physical connection to an application pool.
	if err := lease.conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		lease.store.log.Warn("close account ownership connection",
			zap.String("account_id", lease.accountID),
			zap.Error(databaseError{cause: err}),
		)
	}
	if err := lease.db.Close(); err != nil {
		lease.store.log.Warn("close account ownership pool",
			zap.String("account_id", lease.accountID),
			zap.Error(databaseError{cause: err}),
		)
	}
}

func (s *Store) guardOwner(ctx context.Context, tx *gorm.DB, accountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, ok := ctx.Value(leaseContextKey{}).(*accountLease)
	if !ok {
		return nil
	}
	if lease.store != s || lease.accountID != accountID {
		return ErrOwnershipLost
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	held, err := lease.owns(checkCtx)
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err != nil || !held {
		lease.cancel()
		return ErrOwnershipLost
	}
	var generation struct {
		OwnerGeneration int64
	}
	result := tx.Raw("SELECT owner_generation FROM accounts WHERE id = ? FOR SHARE", accountID).Scan(&generation)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 && lease.generation == 0 {
		return nil // A bootstrap lock may precede the provisional account row.
	}
	if result.RowsAffected != 1 || generation.OwnerGeneration != lease.generation {
		lease.cancel()
		return ErrOwnershipLost
	}
	return nil
}

func (s *Store) writeOwned(ctx context.Context, accountID string, fn func(*gorm.DB) error) error {
	db, cancel := s.query(ctx)
	defer cancel()
	return db.Transaction(func(tx *gorm.DB) error {
		if err := s.guardOwner(ctx, tx, accountID); err != nil {
			return err
		}
		return fn(tx)
	})
}
