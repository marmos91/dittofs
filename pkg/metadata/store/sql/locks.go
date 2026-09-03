package sql

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// LockColumns is the full column list persisted for every PersistedLock, in
// the order PutLockArgs supplies values and ScanLock reads them back. It MUST
// list every field the suite (storetest.RunLockPersistenceSuite) asserts is
// preserved — byte-range identity, lease, and delegation state alike.
//
// Each dialect builds its own statements from this constant, so a new column
// is added here and wired in PutLockArgs and lockScanArgs, and nowhere else.
const LockColumns = `id, share_name, file_id, owner_id, client_id, lock_type,
	byte_offset, byte_length, is_zero_byte, is_legacy_byte_range,
	share_reservation, acquired_at, server_epoch,
	lease_key, lease_state, lease_epoch, break_to_state, breaking_to_required,
	breaking, parent_lease_key, is_directory, is_traditional_oplock,
	delegation_id, deleg_type, deleg_breaking, deleg_recalled, deleg_revoked,
	deleg_notification_mask, break_started`

// Three lock statements carry no placeholder and no clock function, so both
// dialects spell them identically and they stay here rather than in
// LockQueries.
const (
	// listLocksBase is the prefix ListLocks appends a WHERE tail to. The
	// trailing 1=1 lets every filter append unconditionally.
	listLocksBase = `SELECT ` + LockColumns + ` FROM locks WHERE 1=1`
	// getServerEpoch selects the singleton epoch row.
	getServerEpoch = `SELECT epoch FROM server_epoch WHERE id = 1`
	// getCleanShutdown selects the singleton clean-shutdown marker.
	getCleanShutdown = `SELECT clean_shutdown FROM server_epoch WHERE id = 1`
)

// LockQueries holds the lock statements that differ between the dialects:
// placeholder syntax, and CURRENT_TIMESTAMP versus NOW().
type LockQueries struct {
	// Put upserts one lock row, re-syncing every column so an overwrite never
	// leaves stale lease or delegation state behind. Twenty-nine parameters,
	// in LockColumns order.
	Put string
	// SelectByID selects one lock row, columns in LockColumns order. One
	// parameter: the lock id.
	SelectByID string
	// Delete removes one lock row. One parameter: the lock id.
	Delete string
	// DeleteByClient removes every lock a client holds. One parameter: the
	// client id.
	DeleteByClient string
	// DeleteByFile removes every lock on a file. One parameter: the file id.
	DeleteByFile string
	// IncrementEpoch bumps the singleton epoch row, creating it at 1 when
	// absent, and returns the new value. No parameters.
	IncrementEpoch string
	// SetCleanShutdown upserts the clean-shutdown marker on the singleton row,
	// so it can be written before any IncrementEpoch has created that row. One
	// parameter: the marker.
	SetCleanShutdown string

	// ListWhere renders the WHERE tail and arguments for a LockQuery, to be
	// appended to listLocksBase.
	//
	// It is a function rather than statement text because this is the one lock
	// query assembled at run time, from a set of filters that may each be
	// present or absent — so there is no constant to spell. The dialects
	// disagree on how to name the Nth argument (sqlite binds anonymous `?` in
	// append order; postgres numbers `$1`, `$2`, …), and that naming is the
	// whole of the difference.
	ListWhere func(query lock.LockQuery) (string, []any)
}

// PutLockArgs returns the argument list for LockQueries.Put in LockColumns
// order.
func PutLockArgs(lk *lock.PersistedLock) []any {
	return []any{
		lk.ID,
		lk.ShareName,
		lk.FileID,
		lk.OwnerID,
		lk.ClientID,
		lk.LockType,
		// byte_offset/byte_length hold the full uint64 range (NFSv4 unbounded
		// = 0xFFFFFFFFFFFFFFFF > MaxInt64), which neither a SQLite signed
		// INTEGER nor a pgx uint64 encode. Both columns are decimal text on
		// the wire — TEXT on sqlite, NUMERIC(20) on postgres — and parse back
		// losslessly.
		strconv.FormatUint(lk.Offset, 10),
		strconv.FormatUint(lk.Length, 10),
		lk.IsZeroByte,
		lk.IsLegacyByteRange,
		lk.AccessMode,
		lk.AcquiredAt,
		lk.ServerEpoch,
		// lease_key/parent_lease_key store as NULL when empty so byte-range
		// rows don't carry phantom zero-length keys that IsLease() would
		// misclassify.
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

// ScanLock scans one row into a PersistedLock. The byte-offset and byte-length
// columns are decimal text (see PutLockArgs) and are parsed back into uint64
// here.
func ScanLock(row Row) (*lock.PersistedLock, error) {
	var lk lock.PersistedLock
	var offsetStr, lengthStr string
	var breakStarted sql.NullTime
	if err := row.Scan(lockScanArgs(&lk, &offsetStr, &lengthStr, &breakStarted)...); err != nil {
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

// lockScanArgs returns the Scan destination pointers in LockColumns order. The
// byte-offset and byte-length columns scan into the caller's string holders;
// ScanLock parses them into lk.Offset/lk.Length.
func lockScanArgs(lk *lock.PersistedLock, offsetStr, lengthStr *string, breakStarted *sql.NullTime) []any {
	return []any{
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

// lockNotFound is the not-found error every lookup and every delete of a
// missing row reports.
func lockNotFound(what, id string) error {
	return &errors.StoreError{
		Code:    errors.ErrLockNotFound,
		Message: what,
		Path:    id,
	}
}

// PutLock persists a lock, overwriting any row with the same id.
func (c *Core) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	_, err := c.X.Exec(ctx, c.D.Locks().Put, PutLockArgs(lk)...)
	return err
}

// GetLock retrieves a lock by id.
func (c *Core) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	lk, err := ScanLock(c.X.QueryRow(ctx, c.D.Locks().SelectByID, lockID))
	if c.D.IsNoRows(err) {
		return nil, lockNotFound("lock not found", lockID)
	}
	if err != nil {
		return nil, err
	}
	return lk, nil
}

// DeleteLock removes a lock by id, reporting not-found when no row matched.
func (c *Core) DeleteLock(ctx context.Context, lockID string) error {
	result, err := c.X.Exec(ctx, c.D.Locks().Delete, lockID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return lockNotFound("lock not found", lockID)
	}
	return nil
}

// ListLocks returns the locks matching query.
//
// The IsLease filter is not applied here: no backend implements it, and every
// caller re-classifies the rows it gets back. A query that sets it is answered
// as if it had not.
func (c *Core) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	where, args := c.D.Locks().ListWhere(query)

	rows, err := c.X.Query(ctx, listLocksBase+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []*lock.PersistedLock
	for rows.Next() {
		lk, err := ScanLock(rows)
		if err != nil {
			return nil, err
		}
		locks = append(locks, lk)
	}

	return locks, rows.Err()
}

// DeleteLocksByClient removes every lock a client holds and reports how many.
func (c *Core) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	result, err := c.X.Exec(ctx, c.D.Locks().DeleteByClient, clientID)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// DeleteLocksByFile removes every lock on a file and reports how many.
func (c *Core) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	result, err := c.X.Exec(ctx, c.D.Locks().DeleteByFile, fileID)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// GetServerEpoch returns the current server epoch, 0 on a fresh store.
func (c *Core) GetServerEpoch(ctx context.Context) (uint64, error) {
	var epoch uint64
	err := c.X.QueryRow(ctx, getServerEpoch).Scan(&epoch)
	if c.D.IsNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// IncrementServerEpoch increments the epoch and returns the new value.
func (c *Core) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	var newEpoch uint64
	err := c.X.QueryRow(ctx, c.D.Locks().IncrementEpoch).Scan(&newEpoch)
	return newEpoch, err
}

// GetCleanShutdown reports whether the previous run shut down gracefully. An
// absent singleton row (fresh store) is reported as false — the fail-safe
// default, which sends the boot path into the lock-recovery grace period.
func (c *Core) GetCleanShutdown(ctx context.Context) (bool, error) {
	var clean bool
	err := c.X.QueryRow(ctx, getCleanShutdown).Scan(&clean)
	if c.D.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return clean, nil
}

// SetCleanShutdown records the clean-shutdown marker.
func (c *Core) SetCleanShutdown(ctx context.Context, clean bool) error {
	_, err := c.X.Exec(ctx, c.D.Locks().SetCleanShutdown, clean)
	return err
}

// ReclaimLease finds a persisted lease on fileHandle whose key matches
// leaseKey and returns it marked for reclaim.
//
// clientID is not consulted: a row is matched on its lease key alone, as it is
// on the other backends.
func (c *Core) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, _ string) (*lock.UnifiedLock, error) {
	locks, err := c.ListLocks(ctx, lock.LockQuery{FileID: string(fileHandle)})
	if err != nil {
		return nil, err
	}

	for _, lk := range locks {
		// A lease is exactly a row carrying a 16-byte key.
		if len(lk.LeaseKey) != 16 {
			continue
		}
		var storedKey [16]byte
		copy(storedKey[:], lk.LeaseKey)
		if storedKey != leaseKey {
			continue
		}
		enhanced := lock.FromPersistedLock(lk)
		if enhanced.Lease != nil {
			enhanced.Lease.Reclaim = true
		}
		enhanced.Reclaim = true
		return enhanced, nil
	}

	return nil, lockNotFound("lease not found for reclaim", string(fileHandle))
}

// Core serves the lock surface on both the pool and an open transaction: the
// store and its transaction each embed one, over their own executor.
var _ lock.LockStore = (*Core)(nil)
