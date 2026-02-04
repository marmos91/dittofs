package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// BadgerDB LockStore Implementation
// ============================================================================

// Key prefixes for lock storage
const (
	prefixLock        = "lock:"       // Primary: lock:{lockID}
	prefixLockByFile  = "lkfile:"     // Index: lkfile:{fileID}:{lockID}
	prefixLockByOwner = "lkowner:"    // Index: lkowner:{ownerID}:{lockID}
	prefixLockByClient = "lkclient:" // Index: lkclient:{clientID}:{lockID}
	prefixServerEpoch = "srvepoch"   // Single key for epoch
)

// badgerLockStore implements lock.LockStore using BadgerDB.
//
// This implementation is suitable for:
//   - Production deployments requiring lock persistence
//   - Embedded single-node servers
//
// Storage Model:
//   - Primary storage: lock:{lockID} -> JSON(PersistedLock)
//   - Secondary indexes for efficient queries:
//     - lkfile:{fileID}:{lockID} -> lockID
//     - lkowner:{ownerID}:{lockID} -> lockID
//     - lkclient:{clientID}:{lockID} -> lockID
//   - Server epoch: srvepoch -> uint64
//
// Thread Safety:
// All operations use BadgerDB's transaction support for atomicity.
type badgerLockStore struct {
	db *badgerdb.DB
	mu sync.RWMutex
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
	fileKey := []byte(prefixLockByFile + lk.FileID + ":" + lk.ID)
	if err := txn.Set(fileKey, lockIDBytes); err != nil {
		return err
	}

	// Index by owner
	ownerKey := []byte(prefixLockByOwner + lk.OwnerID + ":" + lk.ID)
	if err := txn.Set(ownerKey, lockIDBytes); err != nil {
		return err
	}

	// Index by client
	clientKey := []byte(prefixLockByClient + lk.ClientID + ":" + lk.ID)
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
	fileKey := []byte(prefixLockByFile + lk.FileID + ":" + lockID)
	if err := txn.Delete(fileKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	ownerKey := []byte(prefixLockByOwner + lk.OwnerID + ":" + lockID)
	if err := txn.Delete(ownerKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	clientKey := []byte(prefixLockByClient + lk.ClientID + ":" + lockID)
	if err := txn.Delete(clientKey); err != nil && err != badgerdb.ErrKeyNotFound {
		return err
	}

	return nil
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
		prefix = prefixLockByFile + query.FileID + ":"
	case query.OwnerID != "":
		prefix = prefixLockByOwner + query.OwnerID + ":"
	case query.ClientID != "":
		prefix = prefixLockByClient + query.ClientID + ":"
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
		// Find all locks for this client
		prefix := []byte(prefixLockByClient + clientID + ":")
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		var lockIDs []string
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			err := it.Item().Value(func(val []byte) error {
				lockIDs = append(lockIDs, string(val))
				return nil
			})
			if err != nil {
				return err
			}
		}

		// Delete each lock
		for _, lockID := range lockIDs {
			if err := s.deleteLockTx(txn, lockID); err != nil {
				continue // Ignore errors for individual locks
			}
			count++
		}

		return nil
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
		// Find all locks for this file
		prefix := []byte(prefixLockByFile + fileID + ":")
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()

		var lockIDs []string
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			err := it.Item().Value(func(val []byte) error {
				lockIDs = append(lockIDs, string(val))
				return nil
			})
			if err != nil {
				return err
			}
		}

		// Delete each lock
		for _, lockID := range lockIDs {
			if err := s.deleteLockTx(txn, lockID); err != nil {
				continue
			}
			count++
		}

		return nil
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
		epoch = uint64(val[0]) | uint64(val[1])<<8 | uint64(val[2])<<16 | uint64(val[3])<<24 |
			uint64(val[4])<<32 | uint64(val[5])<<40 | uint64(val[6])<<48 | uint64(val[7])<<56
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
		epoch, err := s.getServerEpochTx(txn)
		if err != nil {
			return err
		}

		newEpoch = epoch + 1
		key := []byte(prefixServerEpoch)
		val := make([]byte, 8)
		val[0] = byte(newEpoch)
		val[1] = byte(newEpoch >> 8)
		val[2] = byte(newEpoch >> 16)
		val[3] = byte(newEpoch >> 24)
		val[4] = byte(newEpoch >> 32)
		val[5] = byte(newEpoch >> 40)
		val[6] = byte(newEpoch >> 48)
		val[7] = byte(newEpoch >> 56)
		return txn.Set(key, val)
	})
	return newEpoch, err
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
	// This requires iterating and deleting, which is complex within a transaction
	// For now, delegate to the store (will use its own transaction)
	tx.store.initLockStore()
	return tx.store.lockStore.DeleteLocksByClient(ctx, clientID)
}

func (tx *badgerTransaction) DeleteLocksByFile(ctx context.Context, fileID string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tx.store.initLockStore()
	return tx.store.lockStore.DeleteLocksByFile(ctx, fileID)
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
	tx.store.initLockStore()
	return tx.store.lockStore.IncrementServerEpoch(ctx)
}
