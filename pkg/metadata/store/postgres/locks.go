package postgres

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// PostgreSQL LockStore Implementation
// ============================================================================

// postgresLockStore implements lock.LockStore using PostgreSQL.
//
// This implementation is suitable for:
//   - Production deployments requiring lock persistence
//   - Distributed multi-node servers
//
// Storage Model:
//   - locks table: stores PersistedLock with indexed columns
//   - server_epoch table: single row tracking server restarts
//
// Thread Safety:
// All operations use database transactions for atomicity.
type postgresLockStore struct {
	pool *pgxpool.Pool
}

// newPostgresLockStore creates a new PostgreSQL lock store.
func newPostgresLockStore(pool *pgxpool.Pool) *postgresLockStore {
	return &postgresLockStore{
		pool: pool,
	}
}

// PutLock persists a lock using upsert.
func (s *postgresLockStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	query := `
		INSERT INTO locks (id, share_name, file_id, owner_id, client_id, lock_type,
		                   byte_offset, byte_length, share_reservation, acquired_at, server_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			share_name = EXCLUDED.share_name,
			file_id = EXCLUDED.file_id,
			owner_id = EXCLUDED.owner_id,
			client_id = EXCLUDED.client_id,
			lock_type = EXCLUDED.lock_type,
			byte_offset = EXCLUDED.byte_offset,
			byte_length = EXCLUDED.byte_length,
			share_reservation = EXCLUDED.share_reservation,
			acquired_at = EXCLUDED.acquired_at,
			server_epoch = EXCLUDED.server_epoch
	`

	_, err := s.pool.Exec(ctx, query,
		lk.ID,
		lk.ShareName,
		lk.FileID,
		lk.OwnerID,
		lk.ClientID,
		lk.LockType,
		lk.Offset,
		lk.Length,
		lk.ShareReservation,
		lk.AcquiredAt,
		lk.ServerEpoch,
	)
	return err
}

// putLockTx persists a lock within an existing transaction.
func (s *postgresLockStore) putLockTx(ctx context.Context, tx pgx.Tx, lk *lock.PersistedLock) error {
	query := `
		INSERT INTO locks (id, share_name, file_id, owner_id, client_id, lock_type,
		                   byte_offset, byte_length, share_reservation, acquired_at, server_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			share_name = EXCLUDED.share_name,
			file_id = EXCLUDED.file_id,
			owner_id = EXCLUDED.owner_id,
			client_id = EXCLUDED.client_id,
			lock_type = EXCLUDED.lock_type,
			byte_offset = EXCLUDED.byte_offset,
			byte_length = EXCLUDED.byte_length,
			share_reservation = EXCLUDED.share_reservation,
			acquired_at = EXCLUDED.acquired_at,
			server_epoch = EXCLUDED.server_epoch
	`

	_, err := tx.Exec(ctx, query,
		lk.ID,
		lk.ShareName,
		lk.FileID,
		lk.OwnerID,
		lk.ClientID,
		lk.LockType,
		lk.Offset,
		lk.Length,
		lk.ShareReservation,
		lk.AcquiredAt,
		lk.ServerEpoch,
	)
	return err
}

// GetLock retrieves a lock by ID.
func (s *postgresLockStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	query := `
		SELECT id, share_name, file_id, owner_id, client_id, lock_type,
		       byte_offset, byte_length, share_reservation, acquired_at, server_epoch
		FROM locks
		WHERE id = $1
	`

	var lk lock.PersistedLock
	err := s.pool.QueryRow(ctx, query, lockID).Scan(
		&lk.ID,
		&lk.ShareName,
		&lk.FileID,
		&lk.OwnerID,
		&lk.ClientID,
		&lk.LockType,
		&lk.Offset,
		&lk.Length,
		&lk.ShareReservation,
		&lk.AcquiredAt,
		&lk.ServerEpoch,
	)
	if err == pgx.ErrNoRows {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	if err != nil {
		return nil, err
	}

	return &lk, nil
}

// getLockTx retrieves a lock within an existing transaction.
func (s *postgresLockStore) getLockTx(ctx context.Context, tx pgx.Tx, lockID string) (*lock.PersistedLock, error) {
	query := `
		SELECT id, share_name, file_id, owner_id, client_id, lock_type,
		       byte_offset, byte_length, share_reservation, acquired_at, server_epoch
		FROM locks
		WHERE id = $1
	`

	var lk lock.PersistedLock
	err := tx.QueryRow(ctx, query, lockID).Scan(
		&lk.ID,
		&lk.ShareName,
		&lk.FileID,
		&lk.OwnerID,
		&lk.ClientID,
		&lk.LockType,
		&lk.Offset,
		&lk.Length,
		&lk.ShareReservation,
		&lk.AcquiredAt,
		&lk.ServerEpoch,
	)
	if err == pgx.ErrNoRows {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	if err != nil {
		return nil, err
	}

	return &lk, nil
}

// DeleteLock removes a lock by ID.
func (s *postgresLockStore) DeleteLock(ctx context.Context, lockID string) error {
	query := `DELETE FROM locks WHERE id = $1`
	result, err := s.pool.Exec(ctx, query, lockID)
	if err != nil {
		return err
	}

	if result.RowsAffected() == 0 {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}

	return nil
}

// deleteLockTx removes a lock within an existing transaction.
func (s *postgresLockStore) deleteLockTx(ctx context.Context, tx pgx.Tx, lockID string) error {
	query := `DELETE FROM locks WHERE id = $1`
	result, err := tx.Exec(ctx, query, lockID)
	if err != nil {
		return err
	}

	if result.RowsAffected() == 0 {
		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}

	return nil
}

// ListLocks returns locks matching the query.
func (s *postgresLockStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	// Build dynamic query
	baseQuery := `
		SELECT id, share_name, file_id, owner_id, client_id, lock_type,
		       byte_offset, byte_length, share_reservation, acquired_at, server_epoch
		FROM locks
		WHERE 1=1
	`

	var args []interface{}
	argIndex := 1

	if query.FileID != "" {
		baseQuery += ` AND file_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.FileID)
		argIndex++
	}
	if query.OwnerID != "" {
		baseQuery += ` AND owner_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.OwnerID)
		argIndex++
	}
	if query.ClientID != "" {
		baseQuery += ` AND client_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.ClientID)
		argIndex++
	}
	if query.ShareName != "" {
		baseQuery += ` AND share_name = $` + strconv.Itoa(argIndex)
		args = append(args, query.ShareName)
	}

	rows, err := s.pool.Query(ctx, baseQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*lock.PersistedLock
	for rows.Next() {
		var lk lock.PersistedLock
		err := rows.Scan(
			&lk.ID,
			&lk.ShareName,
			&lk.FileID,
			&lk.OwnerID,
			&lk.ClientID,
			&lk.LockType,
			&lk.Offset,
			&lk.Length,
			&lk.ShareReservation,
			&lk.AcquiredAt,
			&lk.ServerEpoch,
		)
		if err != nil {
			return nil, err
		}
		locks = append(locks, &lk)
	}

	return locks, rows.Err()
}

// listLocksTx lists locks within an existing transaction.
func (s *postgresLockStore) listLocksTx(ctx context.Context, tx pgx.Tx, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	// Build dynamic query
	baseQuery := `
		SELECT id, share_name, file_id, owner_id, client_id, lock_type,
		       byte_offset, byte_length, share_reservation, acquired_at, server_epoch
		FROM locks
		WHERE 1=1
	`

	var args []interface{}
	argIndex := 1

	if query.FileID != "" {
		baseQuery += ` AND file_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.FileID)
		argIndex++
	}
	if query.OwnerID != "" {
		baseQuery += ` AND owner_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.OwnerID)
		argIndex++
	}
	if query.ClientID != "" {
		baseQuery += ` AND client_id = $` + strconv.Itoa(argIndex)
		args = append(args, query.ClientID)
		argIndex++
	}
	if query.ShareName != "" {
		baseQuery += ` AND share_name = $` + strconv.Itoa(argIndex)
		args = append(args, query.ShareName)
	}

	rows, err := tx.Query(ctx, baseQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*lock.PersistedLock
	for rows.Next() {
		var lk lock.PersistedLock
		err := rows.Scan(
			&lk.ID,
			&lk.ShareName,
			&lk.FileID,
			&lk.OwnerID,
			&lk.ClientID,
			&lk.LockType,
			&lk.Offset,
			&lk.Length,
			&lk.ShareReservation,
			&lk.AcquiredAt,
			&lk.ServerEpoch,
		)
		if err != nil {
			return nil, err
		}
		locks = append(locks, &lk)
	}

	return locks, rows.Err()
}

// DeleteLocksByClient removes all locks for a client.
func (s *postgresLockStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	query := `DELETE FROM locks WHERE client_id = $1`
	result, err := s.pool.Exec(ctx, query, clientID)
	if err != nil {
		return 0, err
	}

	return int(result.RowsAffected()), nil
}

// deleteLocksByClientTx removes locks within an existing transaction.
func (s *postgresLockStore) deleteLocksByClientTx(ctx context.Context, tx pgx.Tx, clientID string) (int, error) {
	query := `DELETE FROM locks WHERE client_id = $1`
	result, err := tx.Exec(ctx, query, clientID)
	if err != nil {
		return 0, err
	}

	return int(result.RowsAffected()), nil
}

// DeleteLocksByFile removes all locks for a file.
func (s *postgresLockStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	query := `DELETE FROM locks WHERE file_id = $1`
	result, err := s.pool.Exec(ctx, query, fileID)
	if err != nil {
		return 0, err
	}

	return int(result.RowsAffected()), nil
}

// deleteLocksByFileTx removes locks within an existing transaction.
func (s *postgresLockStore) deleteLocksByFileTx(ctx context.Context, tx pgx.Tx, fileID string) (int, error) {
	query := `DELETE FROM locks WHERE file_id = $1`
	result, err := tx.Exec(ctx, query, fileID)
	if err != nil {
		return 0, err
	}

	return int(result.RowsAffected()), nil
}

// GetServerEpoch returns current server epoch.
func (s *postgresLockStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	query := `SELECT epoch FROM server_epoch WHERE id = 1`
	var epoch uint64
	err := s.pool.QueryRow(ctx, query).Scan(&epoch)
	if err == pgx.ErrNoRows {
		return 0, nil // Fresh start
	}
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// getServerEpochTx gets epoch within an existing transaction.
func (s *postgresLockStore) getServerEpochTx(ctx context.Context, tx pgx.Tx) (uint64, error) {
	query := `SELECT epoch FROM server_epoch WHERE id = 1`
	var epoch uint64
	err := tx.QueryRow(ctx, query).Scan(&epoch)
	if err == pgx.ErrNoRows {
		return 0, nil // Fresh start
	}
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// IncrementServerEpoch increments and returns new epoch.
func (s *postgresLockStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	query := `
		INSERT INTO server_epoch (id, epoch, updated_at)
		VALUES (1, 1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			epoch = server_epoch.epoch + 1,
			updated_at = NOW()
		RETURNING epoch
	`
	var newEpoch uint64
	err := s.pool.QueryRow(ctx, query).Scan(&newEpoch)
	return newEpoch, err
}

// incrementServerEpochTx increments epoch within an existing transaction.
func (s *postgresLockStore) incrementServerEpochTx(ctx context.Context, tx pgx.Tx) (uint64, error) {
	query := `
		INSERT INTO server_epoch (id, epoch, updated_at)
		VALUES (1, 1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			epoch = server_epoch.epoch + 1,
			updated_at = NOW()
		RETURNING epoch
	`
	var newEpoch uint64
	err := tx.QueryRow(ctx, query).Scan(&newEpoch)
	return newEpoch, err
}

// ReclaimLease reclaims an existing lease during grace period.
// Searches for a persisted lease with matching file handle and lease key.
func (s *postgresLockStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, _ string) (*lock.EnhancedLock, error) {
	// Search for leases on this file with matching lease key
	locks, err := s.ListLocks(ctx, lock.LockQuery{FileID: string(fileHandle)})
	if err != nil {
		return nil, err
	}

	for _, lk := range locks {
		// Must be a lease (has 16-byte key)
		if len(lk.LeaseKey) != 16 {
			continue
		}
		// Match lease key
		var storedKey [16]byte
		copy(storedKey[:], lk.LeaseKey)
		if storedKey != leaseKey {
			continue
		}
		// Found matching lease - convert to EnhancedLock
		enhanced := lock.FromPersistedLock(lk)
		if enhanced.Lease != nil {
			enhanced.Lease.Reclaim = true
		}
		enhanced.Reclaim = true
		return enhanced, nil
	}

	return nil, &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: "lease not found for reclaim",
		Path:    string(fileHandle),
	}
}

// reclaimLeaseTx reclaims a lease within an existing transaction.
func (s *postgresLockStore) reclaimLeaseTx(ctx context.Context, tx pgx.Tx, fileHandle lock.FileHandle, leaseKey [16]byte, _ string) (*lock.EnhancedLock, error) {
	// Search for leases on this file with matching lease key
	locks, err := s.listLocksTx(ctx, tx, lock.LockQuery{FileID: string(fileHandle)})
	if err != nil {
		return nil, err
	}

	for _, lk := range locks {
		// Must be a lease (has 16-byte key)
		if len(lk.LeaseKey) != 16 {
			continue
		}
		// Match lease key
		var storedKey [16]byte
		copy(storedKey[:], lk.LeaseKey)
		if storedKey != leaseKey {
			continue
		}
		// Found matching lease - convert to EnhancedLock
		enhanced := lock.FromPersistedLock(lk)
		if enhanced.Lease != nil {
			enhanced.Lease.Reclaim = true
		}
		enhanced.Reclaim = true
		return enhanced, nil
	}

	return nil, &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: "lease not found for reclaim",
		Path:    string(fileHandle),
	}
}

// ============================================================================
// PostgresMetadataStore LockStore Integration
// ============================================================================

// Ensure PostgresMetadataStore implements LockStore
var _ lock.LockStore = (*PostgresMetadataStore)(nil)

// initLockStore ensures the lock store is initialized.
func (s *PostgresMetadataStore) initLockStore() {
	s.lockStoreMu.Lock()
	defer s.lockStoreMu.Unlock()
	if s.lockStore == nil {
		s.lockStore = newPostgresLockStore(s.pool)
	}
}

// PutLock persists a lock.
func (s *PostgresMetadataStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	s.initLockStore()
	return s.lockStore.PutLock(ctx, lk)
}

// GetLock retrieves a lock by ID.
func (s *PostgresMetadataStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	s.initLockStore()
	return s.lockStore.GetLock(ctx, lockID)
}

// DeleteLock removes a lock by ID.
func (s *PostgresMetadataStore) DeleteLock(ctx context.Context, lockID string) error {
	s.initLockStore()
	return s.lockStore.DeleteLock(ctx, lockID)
}

// ListLocks returns locks matching the query.
func (s *PostgresMetadataStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	s.initLockStore()
	return s.lockStore.ListLocks(ctx, query)
}

// DeleteLocksByClient removes all locks for a client.
func (s *PostgresMetadataStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	s.initLockStore()
	return s.lockStore.DeleteLocksByClient(ctx, clientID)
}

// DeleteLocksByFile removes all locks for a file.
func (s *PostgresMetadataStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	s.initLockStore()
	return s.lockStore.DeleteLocksByFile(ctx, fileID)
}

// GetServerEpoch returns current server epoch.
func (s *PostgresMetadataStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	s.initLockStore()
	return s.lockStore.GetServerEpoch(ctx)
}

// IncrementServerEpoch increments and returns new epoch.
func (s *PostgresMetadataStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	s.initLockStore()
	return s.lockStore.IncrementServerEpoch(ctx)
}

// ReclaimLease reclaims an existing lease during grace period.
func (s *PostgresMetadataStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.EnhancedLock, error) {
	s.initLockStore()
	return s.lockStore.ReclaimLease(ctx, fileHandle, leaseKey, clientID)
}

// ============================================================================
// Transaction LockStore Support
// ============================================================================

// Ensure postgresTransaction implements LockStore
var _ lock.LockStore = (*postgresTransaction)(nil)

func (ptx *postgresTransaction) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	ptx.store.initLockStore()
	return ptx.store.lockStore.putLockTx(ctx, ptx.tx, lk)
}

func (ptx *postgresTransaction) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.getLockTx(ctx, ptx.tx, lockID)
}

func (ptx *postgresTransaction) DeleteLock(ctx context.Context, lockID string) error {
	ptx.store.initLockStore()
	return ptx.store.lockStore.deleteLockTx(ctx, ptx.tx, lockID)
}

func (ptx *postgresTransaction) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.listLocksTx(ctx, ptx.tx, query)
}

func (ptx *postgresTransaction) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.deleteLocksByClientTx(ctx, ptx.tx, clientID)
}

func (ptx *postgresTransaction) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.deleteLocksByFileTx(ctx, ptx.tx, fileID)
}

func (ptx *postgresTransaction) GetServerEpoch(ctx context.Context) (uint64, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.getServerEpochTx(ctx, ptx.tx)
}

func (ptx *postgresTransaction) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.incrementServerEpochTx(ctx, ptx.tx)
}

func (ptx *postgresTransaction) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.EnhancedLock, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.reclaimLeaseTx(ctx, ptx.tx, fileHandle, leaseKey, clientID)
}
