package badger

import (
	"context"
	"encoding/json"
	"fmt"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// ============================================================================
// BadgerDB ClientRecoveryStore Implementation
// ============================================================================

// Key prefix for NFSv4 client-recovery storage.
//
//	crec:{clientIDString} -> JSON(V4ClientRecoveryRecord)
//
// The JSON encoding of BootVerifier [8]byte is a numeric array, which
// round-trips byte-exact. List is a prefix scan.
const prefixV4ClientRecovery = "crec:"

// badgerRecoveryStore implements lock.ClientRecoveryStore using BadgerDB.
//
// Storage Model:
//   - crec:{clientIDString} -> JSON(V4ClientRecoveryRecord)
//
// Thread Safety:
// All operations use BadgerDB's transaction support for atomicity.
type badgerRecoveryStore struct {
	db *badgerdb.DB
}

// newBadgerRecoveryStore creates a new BadgerDB client recovery store.
func newBadgerRecoveryStore(db *badgerdb.DB) *badgerRecoveryStore {
	return &badgerRecoveryStore{db: db}
}

// PutClientRecovery stores or replaces the record for a confirmed client.
func (s *badgerRecoveryStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("failed to marshal client recovery record: %w", err)
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Set([]byte(prefixV4ClientRecovery+rec.ClientIDString), data)
	})
}

// DeleteClientRecovery removes the record for a client.
func (s *badgerRecoveryStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		err := txn.Delete([]byte(prefixV4ClientRecovery + clientIDString))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		return err
	})
}

// ListClientRecovery returns all stored records via a prefix scan.
func (s *badgerRecoveryStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	result := make([]*lock.V4ClientRecoveryRecord, 0)

	err := s.db.View(func(txn *badgerdb.Txn) error {
		opts := badgerdb.DefaultIteratorOptions
		opts.Prefix = []byte(prefixV4ClientRecovery)

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			err := it.Item().Value(func(val []byte) error {
				rec := &lock.V4ClientRecoveryRecord{}
				if err := json.Unmarshal(val, rec); err != nil {
					return err
				}
				result = append(result, rec)
				return nil
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RecordReclaimComplete marks the client's record reclaim-complete. The
// read-modify-write happens inside one transaction for atomicity. Missing
// record => no-op, not an error.
func (s *badgerRecoveryStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	return s.db.Update(func(txn *badgerdb.Txn) error {
		key := []byte(prefixV4ClientRecovery + clientIDString)
		item, err := txn.Get(key)
		if err == badgerdb.ErrKeyNotFound {
			return nil // Nothing to mark
		}
		if err != nil {
			return err
		}

		var rec lock.V4ClientRecoveryRecord
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &rec)
		}); err != nil {
			return err
		}

		if rec.ReclaimComplete {
			return nil // Already marked
		}
		rec.ReclaimComplete = true

		data, err := json.Marshal(&rec)
		if err != nil {
			return fmt.Errorf("failed to marshal client recovery record: %w", err)
		}
		return txn.Set(key, data)
	})
}

// ============================================================================
// BadgerMetadataStore ClientRecoveryStore Integration
// ============================================================================

// Ensure BadgerMetadataStore implements ClientRecoveryStore.
var _ lock.ClientRecoveryStore = (*BadgerMetadataStore)(nil)

// getRecoveryStore returns the recovery store, initializing if needed.
func (s *BadgerMetadataStore) getRecoveryStore() *badgerRecoveryStore {
	s.recoveryStoreMu.Lock()
	defer s.recoveryStoreMu.Unlock()
	if s.recoveryStore == nil {
		s.recoveryStore = newBadgerRecoveryStore(s.db)
	}
	return s.recoveryStore
}

// PutClientRecovery stores or replaces a client recovery record.
func (s *BadgerMetadataStore) PutClientRecovery(ctx context.Context, rec *lock.V4ClientRecoveryRecord) error {
	return s.getRecoveryStore().PutClientRecovery(ctx, rec)
}

// DeleteClientRecovery removes a client recovery record.
func (s *BadgerMetadataStore) DeleteClientRecovery(ctx context.Context, clientIDString string) error {
	return s.getRecoveryStore().DeleteClientRecovery(ctx, clientIDString)
}

// ListClientRecovery returns all stored client recovery records.
func (s *BadgerMetadataStore) ListClientRecovery(ctx context.Context) ([]*lock.V4ClientRecoveryRecord, error) {
	return s.getRecoveryStore().ListClientRecovery(ctx)
}

// RecordReclaimComplete marks a client's recovery record reclaim-complete.
func (s *BadgerMetadataStore) RecordReclaimComplete(ctx context.Context, clientIDString string) error {
	return s.getRecoveryStore().RecordReclaimComplete(ctx, clientIDString)
}

// ClientRecoveryStore returns this store as a ClientRecoveryStore.
// This allows direct access to the interface for handler initialization.
func (s *BadgerMetadataStore) ClientRecoveryStore() lock.ClientRecoveryStore {
	return s
}
