package badger

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// BadgerDB LockStore Implementation
// ============================================================================

// Key prefixes for lock storage. The indexed segment (fileID/ownerID/clientID)
// is hex-encoded so an attacker-controlled ':' cannot forge an extra key
// segment (see lockIndexPrefix).
const (
	prefixLock         = "lock:"     // Primary: lock:{lockID}
	prefixLockByFile   = "lkfile:"   // Index: lkfile:{hex(fileID)}:{lockID}
	prefixLockByOwner  = "lkowner:"  // Index: lkowner:{hex(ownerID)}:{lockID}
	prefixLockByClient = "lkclient:" // Index: lkclient:{hex(clientID)}:{lockID}
	prefixServerEpoch  = "srvepoch"  // Single key for epoch
	keyCleanShutdown   = "srvclean"  // Single key for clean-shutdown marker
)

// lockIndexPrefix builds a secondary-index key prefix for one indexed value.
//
// fileID, ownerID and clientID all carry raw, unauthenticated, network-supplied
// strings — and OwnerID/FileID legitimately contain ':' (e.g. NLM OwnerID
// "nlm:caller:svid:oh", FileID = string(FileHandle)). Embedding them raw in a
// ':'-separated Badger key lets a crafted value inject extra key segments: an
// OwnerID "victim-owner:fake-lock-id" would plant a key that a later prefix scan
// for "victim-owner" matches, letting deleteLocksByIndexPrefixTx remove another
// owner's locks. Hex-encoding the indexed value makes the ':' separator
// unambiguous so the segment boundary cannot be forged. Mirrors the MonName fix
// in clients.go:monNameIndexPrefix.
//
// No on-disk migration is needed for the format change: startup lock recovery
// (service.go) enumerates locks via the primary lock:{lockID} keys
// (ListLocks with only a ShareName filter takes the default primaryKey scan),
// which this change does not touch — so every persisted lock is still found and
// re-graced. Stale legacy index entries left by a pre-upgrade run are
// unreachable by the new scans and harmless: lock state is ephemeral runtime
// state, cleared on clean shutdown and rebuilt under a fresh grace period after
// an unclean one.
func lockIndexPrefix(prefix, value string) string {
	return prefix + hex.EncodeToString([]byte(value)) + ":"
}

// badgerLockStore implements lock.LockStore using BadgerDB.
//
// This implementation is suitable for:
//   - Production deployments requiring lock persistence
//   - Embedded single-node servers
//
// Storage Model:
//   - Primary storage: lock:{lockID} -> JSON(PersistedLock)
//   - Secondary indexes for efficient queries:
//   - lkfile:{fileID}:{lockID} -> lockID
//   - lkowner:{ownerID}:{lockID} -> lockID
//   - lkclient:{clientID}:{lockID} -> lockID
//   - Server epoch: srvepoch -> uint64
//
// Thread Safety:
// All operations use BadgerDB's transaction support for atomicity.
type badgerLockStore struct {
	db *badgerdb.DB
}

// newBadgerLockStore creates a new BadgerDB lock store.
func newBadgerLockStore(db *badgerdb.DB) *badgerLockStore {
	return &badgerLockStore{
		db: db,
	}
}

// PutLock persists a lock with secondary indexes.
func (s *badgerLockStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.putLockTx(txn, lk)
	})
}

// putLockTx persists a lock within an existing transaction.
func (s *badgerLockStore) putLockTx(txn *badgerdb.Txn, lk *lock.PersistedLock) error {
	// Serialize lock to JSON
	data, err := json.Marshal(lk)
	if err != nil {
		return fmt.Errorf("failed to marshal lock: %w", err)
	}

	// Store primary key
	primaryKey := []byte(prefixLock + lk.ID)
	if err := txn.Set(primaryKey, data); err != nil {
		return err
	}

	// Store secondary indexes (value is just the lock ID for reference)
	lockIDBytes := []byte(lk.ID)

	// Index by file
	fileKey := []byte(lockIndexPrefix(prefixLockByFile, lk.FileID) + lk.ID)
	if err := txn.Set(fileKey, lockIDBytes); err != nil {
		return err
	}

	// Index by owner
	ownerKey := []byte(lockIndexPrefix(prefixLockByOwner, lk.OwnerID) + lk.ID)
	if err := txn.Set(ownerKey, lockIDBytes); err != nil {
		return err
	}

	// Index by client
	clientKey := []byte(lockIndexPrefix(prefixLockByClient, lk.ClientID) + lk.ID)
	if err := txn.Set(clientKey, lockIDBytes); err != nil {
		return err
	}

	return nil
}

// GetLock retrieves a lock by ID.
func (s *badgerLockStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var lk *lock.PersistedLock
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		lk, err = s.getLockTx(txn, lockID)
		return err
	})
	return lk, err
}

// getLockTx retrieves a lock within an existing transaction.
func (s *badgerLockStore) getLockTx(txn *badgerdb.Txn, lockID string) (*lock.PersistedLock, error) {
	key := []byte(prefixLock + lockID)
	item, err := txn.Get(key)
	if err == badgerdb.ErrKeyNotFound {
		return nil, &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lock not found",
			Path:    lockID,
		}
	}
	if err != nil {
		return nil, err
	}

	var lk lock.PersistedLock
	err = item.Value(func(val []byte) error {
		return json.Unmarshal(val, &lk)
	})
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal lock: %w", err)
	}

	return &lk, nil
}

// DeleteLock removes a lock and its indexes.
func (s *badgerLockStore) DeleteLock(ctx context.Context, lockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.deleteLockTx(txn, lockID)
	})
}

// deleteLockTx removes a lock within an existing transaction.
func (s *badgerLockStore) deleteLockTx(txn *badgerdb.Txn, lockID string) error {
	// First, get the lock to know which indexes to delete
	lk, err := s.getLockTx(txn, lockID)
	if err != nil {
		return err
	}

	// Delete primary key
	primaryKey := []byte(prefixLock + lockID)
	if err := txn.Delete(primaryKey); err != nil {
		return err
	}

	// Delete secondary indexes
	fileKey := []byte(lockIndexPrefix(prefixLockByFile, lk.FileID) + lockID)
	if err := txn.Delete(fileKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	ownerKey := []byte(lockIndexPrefix(prefixLockByOwner, lk.OwnerID) + lockID)
	if err := txn.Delete(ownerKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	clientKey := []byte(lockIndexPrefix(prefixLockByClient, lk.ClientID) + lockID)
	if err := txn.Delete(clientKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	return nil
}

// deleteLocksByIndexPrefixTx deletes every lock whose ID is referenced by a
// secondary-index entry under prefix, operating entirely within txn so the
// deletions participate in the caller's transaction (atomic with the rest of
// the operation and rolled back on an OCC retry). Returns the number deleted.
func (s *badgerLockStore) deleteLocksByIndexPrefixTx(txn *badgerdb.Txn, prefix []byte) (int, error) {
	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefix
	it := txn.NewIterator(opts)

	var lockIDs []string
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := it.Item().Value(func(val []byte) error {
			lockIDs = append(lockIDs, string(val))
			return nil
		}); err != nil {
			it.Close()
			return 0, err
		}
	}
	// Close the iterator before issuing writes: Badger disallows mutating a
	// txn while one of its iterators is still open.
	it.Close()

	count := 0
	for _, lockID := range lockIDs {
		if err := s.deleteLockTx(txn, lockID); err != nil {
			continue // Ignore errors for individual locks
		}
		count++
	}
	return count, nil
}

// incrementServerEpochTx increments and returns the new epoch within txn.
func (s *badgerLockStore) incrementServerEpochTx(txn *badgerdb.Txn) (uint64, error) {
	epoch, err := s.getServerEpochTx(txn)
	if err != nil {
		return 0, err
	}
	newEpoch := epoch + 1
	val := make([]byte, 8)
	binary.LittleEndian.PutUint64(val, newEpoch)
	if err := txn.Set([]byte(prefixServerEpoch), val); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// setCleanShutdownTx records the clean-shutdown marker within txn.
func (s *badgerLockStore) setCleanShutdownTx(txn *badgerdb.Txn, clean bool) error {
	val := []byte{0}
	if clean {
		val = []byte{1}
	}
	return txn.Set([]byte(keyCleanShutdown), val)
}

// ListLocks returns locks matching the query.
func (s *badgerLockStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var locks []*lock.PersistedLock
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		locks, err = s.listLocksTx(txn, query)
		return err
	})
	return locks, err
}

// listLocksTx lists locks within an existing transaction.
func (s *badgerLockStore) listLocksTx(txn *badgerdb.Txn, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	var prefix string

	// Choose the best index based on query
	switch {
	case query.FileID != "":
		prefix = lockIndexPrefix(prefixLockByFile, query.FileID)
	case query.OwnerID != "":
		prefix = lockIndexPrefix(prefixLockByOwner, query.OwnerID)
	case query.ClientID != "":
		prefix = lockIndexPrefix(prefixLockByClient, query.ClientID)
	default:
		// No specific filter, scan all locks
		prefix = prefixLock
	}

	var locks []*lock.PersistedLock
	prefixBytes := []byte(prefix)

	opts := badgerdb.DefaultIteratorOptions
	opts.Prefix = prefixBytes
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefixBytes); it.ValidForPrefix(prefixBytes); it.Next() {
		item := it.Item()

		var lk *lock.PersistedLock

		if prefix == prefixLock {
			// Scanning primary keys, value is the lock
			err := item.Value(func(val []byte) error {
				lk = &lock.PersistedLock{}
				return json.Unmarshal(val, lk)
			})
			if err != nil {
				continue
			}
		} else {
			// Scanning index, value is lock ID
			var lockID string
			err := item.Value(func(val []byte) error {
				lockID = string(val)
				return nil
			})
			if err != nil {
				continue
			}

			// Fetch the actual lock
			lk, err = s.getLockTx(txn, lockID)
			if err != nil {
				continue
			}
		}

		// Apply additional filters
		if query.ShareName != "" && lk.ShareName != query.ShareName {
			continue
		}
		if query.FileID != "" && lk.FileID != query.FileID {
			continue
		}
		if query.OwnerID != "" && lk.OwnerID != query.OwnerID {
			continue
		}
		if query.ClientID != "" && lk.ClientID != query.ClientID {
			continue
		}

		locks = append(locks, lk)
	}

	return locks, nil
}

// DeleteLocksByClient removes all locks for a client.
func (s *badgerLockStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	count := 0
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		var derr error
		count, derr = s.deleteLocksByIndexPrefixTx(txn, []byte(lockIndexPrefix(prefixLockByClient, clientID)))
		return derr
	})
	return count, err
}

// DeleteLocksByFile removes all locks for a file.
func (s *badgerLockStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	count := 0
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		var derr error
		count, derr = s.deleteLocksByIndexPrefixTx(txn, []byte(lockIndexPrefix(prefixLockByFile, fileID)))
		return derr
	})
	return count, err
}

// GetServerEpoch returns current server epoch.
func (s *badgerLockStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var epoch uint64
	err := s.db.View(func(txn *badgerdb.Txn) error {
		var err error
		epoch, err = s.getServerEpochTx(txn)
		return err
	})
	return epoch, err
}

// getServerEpochTx gets the epoch within an existing transaction.
func (s *badgerLockStore) getServerEpochTx(txn *badgerdb.Txn) (uint64, error) {
	key := []byte(prefixServerEpoch)
	item, err := txn.Get(key)
	if err == badgerdb.ErrKeyNotFound {
		return 0, nil // Fresh start
	}
	if err != nil {
		return 0, err
	}

	var epoch uint64
	err = item.Value(func(val []byte) error {
		if len(val) != 8 {
			return fmt.Errorf("invalid epoch value length: %d", len(val))
		}
		epoch = binary.LittleEndian.Uint64(val)
		return nil
	})
	return epoch, err
}

// IncrementServerEpoch increments and returns new epoch.
func (s *badgerLockStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	var newEpoch uint64
	err := s.db.Update(func(txn *badgerdb.Txn) error {
		var ierr error
		newEpoch, ierr = s.incrementServerEpochTx(txn)
		return ierr
	})
	return newEpoch, err
}

// GetCleanShutdown reports whether the previous run shut down gracefully.
// A missing key (fresh store, or a crash before SetCleanShutdown ran) is
// reported as false (unclean) — the fail-safe default.
func (s *badgerLockStore) GetCleanShutdown(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var clean bool
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get([]byte(keyCleanShutdown))
		if err == badgerdb.ErrKeyNotFound {
			return nil // absent -> unclean (clean stays false)
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			clean = len(val) == 1 && val[0] == 1
			return nil
		})
	})
	return clean, err
}

// SetCleanShutdown records the clean-shutdown marker durably.
func (s *badgerLockStore) SetCleanShutdown(ctx context.Context, clean bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return s.setCleanShutdownTx(txn, clean)
	})
}

// ReclaimLease reclaims an existing lease during grace period.
// Searches for a persisted lease with matching file handle and lease key.
func (s *badgerLockStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.UnifiedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var result *lock.UnifiedLock
	err := s.db.View(func(txn *badgerdb.Txn) error {
		// Search for leases on this file
		locks, err := s.listLocksTx(txn, lock.LockQuery{FileID: string(fileHandle)})
		if err != nil {
			return err
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
			result = enhanced
			return nil
		}

		return &errors.StoreError{
			Code:    errors.ErrLockNotFound,
			Message: "lease not found for reclaim",
			Path:    string(fileHandle),
		}
	})

	return result, err
}

// ============================================================================
// BadgerMetadataStore LockStore Integration
// ============================================================================

// Ensure BadgerMetadataStore implements LockStore
var _ lock.LockStore = (*BadgerMetadataStore)(nil)

// initLockStore ensures the lock store is initialized.
func (s *BadgerMetadataStore) initLockStore() {
	s.lockStoreMu.Lock()
	defer s.lockStoreMu.Unlock()
	if s.lockStore == nil {
		s.lockStore = newBadgerLockStore(s.db)
	}
}

// PutLock persists a lock.
func (s *BadgerMetadataStore) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	s.initLockStore()
	return s.lockStore.PutLock(ctx, lk)
}

// GetLock retrieves a lock by ID.
func (s *BadgerMetadataStore) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	s.initLockStore()
	return s.lockStore.GetLock(ctx, lockID)
}

// DeleteLock removes a lock by ID.
func (s *BadgerMetadataStore) DeleteLock(ctx context.Context, lockID string) error {
	s.initLockStore()
	return s.lockStore.DeleteLock(ctx, lockID)
}

// ListLocks returns locks matching the query.
func (s *BadgerMetadataStore) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	s.initLockStore()
	return s.lockStore.ListLocks(ctx, query)
}

// DeleteLocksByClient removes all locks for a client.
func (s *BadgerMetadataStore) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	s.initLockStore()
	return s.lockStore.DeleteLocksByClient(ctx, clientID)
}

// DeleteLocksByFile removes all locks for a file.
func (s *BadgerMetadataStore) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	s.initLockStore()
	return s.lockStore.DeleteLocksByFile(ctx, fileID)
}

// GetServerEpoch returns current server epoch.
func (s *BadgerMetadataStore) GetServerEpoch(ctx context.Context) (uint64, error) {
	s.initLockStore()
	return s.lockStore.GetServerEpoch(ctx)
}

// IncrementServerEpoch increments and returns new epoch.
func (s *BadgerMetadataStore) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	s.initLockStore()
	return s.lockStore.IncrementServerEpoch(ctx)
}

// GetCleanShutdown reports whether the previous run shut down gracefully.
func (s *BadgerMetadataStore) GetCleanShutdown(ctx context.Context) (bool, error) {
	s.initLockStore()
	return s.lockStore.GetCleanShutdown(ctx)
}

// SetCleanShutdown records the clean-shutdown marker durably.
func (s *BadgerMetadataStore) SetCleanShutdown(ctx context.Context, clean bool) error {
	s.initLockStore()
	return s.lockStore.SetCleanShutdown(ctx, clean)
}

// ReclaimLease reclaims an existing lease during grace period.
func (s *BadgerMetadataStore) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.UnifiedLock, error) {
	s.initLockStore()
	return s.lockStore.ReclaimLease(ctx, fileHandle, leaseKey, clientID)
}

// ============================================================================
// Transaction LockStore Support
// ============================================================================

// Ensure badgerTransaction implements LockStore
var _ lock.LockStore = (*badgerTransaction)(nil)

func (tx *badgerTransaction) PutLock(ctx context.Context, lk *lock.PersistedLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.putLockTx(tx.txn, lk)
}

func (tx *badgerTransaction) GetLock(ctx context.Context, lockID string) (*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.getLockTx(tx.txn, lockID)
}

func (tx *badgerTransaction) DeleteLock(ctx context.Context, lockID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.deleteLockTx(tx.txn, lockID)
}

func (tx *badgerTransaction) ListLocks(ctx context.Context, query lock.LockQuery) ([]*lock.PersistedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.listLocksTx(tx.txn, query)
}

func (tx *badgerTransaction) DeleteLocksByClient(ctx context.Context, clientID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Operate within the caller's transaction so the deletions are atomic with
	// the rest of the operation and are discarded if WithTransaction retries on
	// an OCC conflict (the store-level method would commit out-of-band).
	tx.store.initLockStore()
	return tx.store.lockStore.deleteLocksByIndexPrefixTx(tx.txn, []byte(lockIndexPrefix(prefixLockByClient, clientID)))
}

func (tx *badgerTransaction) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.deleteLocksByIndexPrefixTx(tx.txn, []byte(lockIndexPrefix(prefixLockByFile, fileID)))
}

func (tx *badgerTransaction) GetServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.getServerEpochTx(tx.txn)
}

func (tx *badgerTransaction) IncrementServerEpoch(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// Increment within the caller's transaction. The store-level method opens
	// its own db.Update, so on an OCC retry of the outer transaction the epoch
	// would be bumped once per attempt (double-increment); keeping it in tx.txn
	// makes the increment exactly-once per committed transaction.
	tx.store.initLockStore()
	return tx.store.lockStore.incrementServerEpochTx(tx.txn)
}

func (tx *badgerTransaction) GetCleanShutdown(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.GetCleanShutdown(ctx)
}

func (tx *badgerTransaction) SetCleanShutdown(ctx context.Context, clean bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Write within the caller's transaction so the marker is only durable if
	// the outer transaction commits (the store-level method commits out-of-band
	// regardless of the outer transaction's fate).
	tx.store.initLockStore()
	return tx.store.lockStore.setCleanShutdownTx(tx.txn, clean)
}

func (tx *badgerTransaction) ReclaimLease(ctx context.Context, fileHandle lock.FileHandle, leaseKey [16]byte, clientID string) (*lock.UnifiedLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.ReclaimLease(ctx, fileHandle, leaseKey, clientID)
}
