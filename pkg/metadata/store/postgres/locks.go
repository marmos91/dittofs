package postgres

import (
	"context"
	"database/sql"
	"strconv"
	"time"

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

// lockColumns is the full column list persisted for every PersistedLock. It
// MUST list every field the suite (storetest.RunLockPersistenceSuite) asserts
// is preserved — byte-range identity, lease, and delegation state alike.
const lockColumns = `id, share_name, file_id, owner_id, client_id, lock_type,
	byte_offset, byte_length, is_zero_byte, is_legacy_byte_range,
	share_reservation, acquired_at, server_epoch,
	lease_key, lease_state, lease_epoch, break_to_state, breaking_to_required,
	breaking, parent_lease_key, is_directory, is_traditional_oplock,
	delegation_id, deleg_type, deleg_breaking, deleg_recalled, deleg_revoked,
	deleg_notification_mask, break_started`

// putLockSQL is the upsert statement, parameterized $1..$29 in lockColumns
// order. The ON CONFLICT clause re-syncs every column so an overwrite never
// leaves stale lease/delegation state behind.
const putLockSQL = `
	INSERT INTO locks (` + lockColumns + `)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
	        $14, $15, $16, $17, $18, $19, $20, $21, $22,
	        $23, $24, $25, $26, $27, $28, $29)
	ON CONFLICT (id) DO UPDATE SET
		share_name = EXCLUDED.share_name,
		file_id = EXCLUDED.file_id,
		owner_id = EXCLUDED.owner_id,
		client_id = EXCLUDED.client_id,
		lock_type = EXCLUDED.lock_type,
		byte_offset = EXCLUDED.byte_offset,
		byte_length = EXCLUDED.byte_length,
		is_zero_byte = EXCLUDED.is_zero_byte,
		is_legacy_byte_range = EXCLUDED.is_legacy_byte_range,
		share_reservation = EXCLUDED.share_reservation,
		acquired_at = EXCLUDED.acquired_at,
		server_epoch = EXCLUDED.server_epoch,
		lease_key = EXCLUDED.lease_key,
		lease_state = EXCLUDED.lease_state,
		lease_epoch = EXCLUDED.lease_epoch,
		break_to_state = EXCLUDED.break_to_state,
		breaking_to_required = EXCLUDED.breaking_to_required,
		breaking = EXCLUDED.breaking,
		parent_lease_key = EXCLUDED.parent_lease_key,
		is_directory = EXCLUDED.is_directory,
		is_traditional_oplock = EXCLUDED.is_traditional_oplock,
		delegation_id = EXCLUDED.delegation_id,
		deleg_type = EXCLUDED.deleg_type,
		deleg_breaking = EXCLUDED.deleg_breaking,
		deleg_recalled = EXCLUDED.deleg_recalled,
		deleg_revoked = EXCLUDED.deleg_revoked,
		deleg_notification_mask = EXCLUDED.deleg_notification_mask,
		break_started = EXCLUDED.break_started
`

// putLockArgs returns the argument list for putLockSQL in lockColumns order.
// lease_key/parent_lease_key are stored as NULL when empty so byte-range rows
// don't carry phantom zero-length keys that IsLease() would misclassify.
func putLockArgs(lk *lock.PersistedLock) []interface{} {
	return []interface{}{
		lk.ID,
		lk.ShareName,
		lk.FileID,
		lk.OwnerID,
		lk.ClientID,
		lk.LockType,
		// byte_offset/byte_length are NUMERIC(20) to hold the full uint64 range
		// (NFSv4 unbounded = 0xFFFFFFFFFFFFFFFF > MaxInt64). pgx cannot encode a
		// Go uint64 above MaxInt64 into a numeric param directly, so pass the
		// decimal string form; postgres parses it into NUMERIC losslessly.
		strconv.FormatUint(lk.Offset, 10),
		strconv.FormatUint(lk.Length, 10),
		lk.IsZeroByte,
		lk.IsLegacyByteRange,
		lk.AccessMode,
		lk.AcquiredAt,
		lk.ServerEpoch,
		nilIfEmpty(lk.LeaseKey),
		lk.LeaseState,
		lk.LeaseEpoch,
		lk.BreakToState,
		lk.BreakingToRequired,
		lk.Breaking,
		nilIfEmpty(lk.ParentLeaseKey),
		lk.IsDirectory,
		lk.IsTraditionalOplock,
		lk.DelegationID,
		lk.DelegType,
		lk.DelegBreaking,
		lk.DelegRecalled,
		lk.DelegRevoked,
		lk.DelegNotificationMask,
		// break_started is nullable: a zero BreakStarted (no in-flight break)
		// stores as SQL NULL and scans back to the zero time, matching the
		// memory/badger JSON round-trip.
		nullTimeIfZero(lk.BreakStarted),
	}
}

// nilIfEmpty maps an empty byte slice to a typed nil so it stores as SQL NULL.
func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullTimeIfZero maps a zero time.Time to a NULL sql.NullTime so a zeroed
// nullable timestamp column round-trips as the zero time rather than a driver
// default, and a non-zero time stores faithfully.
func nullTimeIfZero(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}

// PutLock persists a lock using upsert.
func (s *postgresLockStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	_, err := s.pool.Exec(ctx, putLockSQL, putLockArgs(lk)...)
	return err
}

// putLockTx persists a lock within an existing transaction.
func (s *postgresLockStore) putLockTx(ctx context.Context, tx pgx.Tx, lk *lock.PersistedLock) error {
	_, err := tx.Exec(ctx, putLockSQL, putLockArgs(lk)...)
	return err
}

// selectByID is the single-row select, columns in lockColumns order so it
// shares the scanLock destination layout.
const selectByID = `SELECT ` + lockColumns + ` FROM locks WHERE id = $1`

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows so a single
// scanLock helper serves every read path.
type rowScanner interface {
	Scan(dest ...interface{}) error
}

// scanLock scans one row into a PersistedLock. byte_offset/byte_length are
// NUMERIC(20) (full uint64 range) and are scanned as decimal strings, then
// parsed: pgx cannot scan a numeric above MaxInt64 into a Go uint64 directly.
func scanLock(row rowScanner) (*lock.PersistedLock, error) {
	var lk lock.PersistedLock
	var offsetStr, lengthStr string
	var breakStarted sql.NullTime
	if err := row.Scan(scanArgs(&lk, &offsetStr, &lengthStr, &breakStarted)...); err != nil {
		return nil, err
	}
	off, err := strconv.ParseUint(offsetStr, 10, 64)
	if err != nil {
		return nil, err
	}
	length, err := strconv.ParseUint(lengthStr, 10, 64)
	if err != nil {
		return nil, err
	}
	lk.Offset = off
	lk.Length = length
	// break_started is nullable; a NULL leaves BreakStarted at its zero value.
	if breakStarted.Valid {
		lk.BreakStarted = breakStarted.Time
	}
	return &lk, nil
}

// scanArgs returns the Scan destination pointers in lockColumns order. The
// byte_offset/byte_length NUMERIC columns scan into the caller's string holders
// (offsetStr/lengthStr); scanLock parses them into lk.Offset/lk.Length. A new
// column is wired in exactly one place here.
func scanArgs(lk *lock.PersistedLock, offsetStr, lengthStr *string, breakStarted *sql.NullTime) []interface{} {
	return []interface{}{
		&lk.ID,
		&lk.ShareName,
		&lk.FileID,
		&lk.OwnerID,
		&lk.ClientID,
		&lk.LockType,
		offsetStr,
		lengthStr,
		&lk.IsZeroByte,
		&lk.IsLegacyByteRange,
		&lk.AccessMode,
		&lk.AcquiredAt,
		&lk.ServerEpoch,
		&lk.LeaseKey,
		&lk.LeaseState,
		&lk.LeaseEpoch,
		&lk.BreakToState,
		&lk.BreakingToRequired,
		&lk.Breaking,
		&lk.ParentLeaseKey,
		&lk.IsDirectory,
		&lk.IsTraditionalOplock,
		&lk.DelegationID,
		&lk.DelegType,
		&lk.DelegBreaking,
		&lk.DelegRecalled,
		&lk.DelegRevoked,
		&lk.DelegNotificationMask,
		breakStarted,
	}
}

// GetLock retrieves a lock by ID.
func (s *postgresLockStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	lk, err := scanLock(s.pool.QueryRow(ctx, selectByID, lockID))
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

	return lk, nil
}

// getLockTx retrieves a lock within an existing transaction.
func (s *postgresLockStore) getLockTx(ctx context.Context, tx pgx.Tx, lockID string) (*lock.PersistedLock, error) {
	lk, err := scanLock(tx.QueryRow(ctx, selectByID, lockID))
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

	return lk, nil
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

// buildListQuery assembles the dynamic SELECT + WHERE for a LockQuery, sharing
// the lockColumns layout with scanArgs so reads stay in sync with the schema.
func buildListQuery(query lock.LockQuery) (string, []interface{}) {
	baseQuery := `SELECT ` + lockColumns + ` FROM locks WHERE 1=1`

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

	return baseQuery, args
}

// ListLocks returns locks matching the query.
func (s *postgresLockStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	baseQuery, args := buildListQuery(query)

	rows, err := s.pool.Query(ctx, baseQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*lock.PersistedLock
	for rows.Next() {
		lk, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		locks = append(locks, lk)
	}

	return locks, rows.Err()
}

// listLocksTx lists locks within an existing transaction.
func (s *postgresLockStore) listLocksTx(ctx context.Context, tx pgx.Tx, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	baseQuery, args := buildListQuery(query)

	rows, err := tx.Query(ctx, baseQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*lock.PersistedLock
	for rows.Next() {
		lk, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		locks = append(locks, lk)
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

// GetCleanShutdown reports whether the previous run shut down gracefully.
// An absent singleton row (fresh store) is reported as false (unclean) — the
// fail-safe default.
func (s *postgresLockStore) GetCleanShutdown(ctx context.Context) (bool, error) {
	query := `SELECT clean_shutdown FROM server_epoch WHERE id = 1`
	var clean bool
	err := s.pool.QueryRow(ctx, query).Scan(&clean)
	if err == pgx.ErrNoRows {
		return false, nil // fresh start -> unclean
	}
	if err != nil {
		return false, err
	}
	return clean, nil
}

// SetCleanShutdown records the clean-shutdown marker on the server_epoch
// singleton row. It upserts so the marker can be written before any
// IncrementServerEpoch has created the row.
func (s *postgresLockStore) SetCleanShutdown(ctx context.Context, clean bool) error {
	query := `
		INSERT INTO server_epoch (id, epoch, clean_shutdown, updated_at)
		VALUES (1, 0, $1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			clean_shutdown = EXCLUDED.clean_shutdown,
			updated_at = NOW()
	`
	_, err := s.pool.Exec(ctx, query, clean)
	return err
}

// getCleanShutdownTx reads the marker within an existing transaction.
func (s *postgresLockStore) getCleanShutdownTx(ctx context.Context, tx pgx.Tx) (bool, error) {
	query := `SELECT clean_shutdown FROM server_epoch WHERE id = 1`
	var clean bool
	err := tx.QueryRow(ctx, query).Scan(&clean)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return clean, nil
}

// setCleanShutdownTx writes the marker within an existing transaction.
func (s *postgresLockStore) setCleanShutdownTx(ctx context.Context, tx pgx.Tx, clean bool) error {
	query := `
		INSERT INTO server_epoch (id, epoch, clean_shutdown, updated_at)
		VALUES (1, 0, $1, NOW())
		ON CONFLICT (id) DO UPDATE SET
			clean_shutdown = EXCLUDED.clean_shutdown,
			updated_at = NOW()
	`
	_, err := tx.Exec(ctx, query, clean)
	return err
}

// ReclaimLease reclaims an existing lease during grace period.
// Searches for a persisted lease with matching file handle and lease key.
func (s *postgresLockStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, _ string) (*lock.UnifiedLock, error) {
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
		// Found matching lease - convert to UnifiedLock
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
func (s *postgresLockStore) reclaimLeaseTx(ctx context.Context, tx pgx.Tx, fileHandle lock.FileHandle, leaseKey [16]byte, _ string) (*lock.UnifiedLock, error) {
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
		// Found matching lease - convert to UnifiedLock
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

// GetCleanShutdown reports whether the previous run shut down gracefully.
func (s *PostgresMetadataStore) GetCleanShutdown(ctx context.Context) (bool, error) {
	s.initLockStore()
	return s.lockStore.GetCleanShutdown(ctx)
}

// SetCleanShutdown records the clean-shutdown marker durably.
func (s *PostgresMetadataStore) SetCleanShutdown(ctx context.Context, clean bool) error {
	s.initLockStore()
	return s.lockStore.SetCleanShutdown(ctx, clean)
}

// ReclaimLease reclaims an existing lease during grace period.
func (s *PostgresMetadataStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.UnifiedLock, error) {
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

func (ptx *postgresTransaction) GetCleanShutdown(ctx context.Context) (bool, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.getCleanShutdownTx(ctx, ptx.tx)
}

func (ptx *postgresTransaction) SetCleanShutdown(ctx context.Context, clean bool) error {
	ptx.store.initLockStore()
	return ptx.store.lockStore.setCleanShutdownTx(ctx, ptx.tx, clean)
}

func (ptx *postgresTransaction) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.UnifiedLock, error) {
	ptx.store.initLockStore()
	return ptx.store.lockStore.reclaimLeaseTx(ctx, ptx.tx, fileHandle, leaseKey, clientID)
}
