package badger

import (
	"context"
	"errors"
	"fmt"

	"github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// SyncedHashStore Implementation for BadgerDB Store
// ============================================================================
//
// Persists per-CAS-hash local→remote sync state markers. Presence of a key
// under the synced: prefix means the corresponding chunk has been
// successfully mirrored to the remote store at least once; absence means
// the chunk is local-only (or has been intentionally reset).
//
// All three methods are idempotent by design: MarkSynced on an already-
// marked hash is a no-op (Badger Set overwrites with the same empty value),
// DeleteSynced on an absent hash returns nil (Badger Delete is idempotent),
// IsSynced on an absent hash returns (false, nil). No sentinel-error
// coordination is required between callers.
//
// Key Namespace:
//   - synced:{32-byte-hash}  empty value (presence == synced)
//
// The 32-byte hash bytes are appended raw (matching rollup_offset's compact
// binary key encoding) rather than hex-encoded — Badger does not require
// printable keys, and raw bytes keep the key half the size on disk.
// ============================================================================

const syncedHashPrefix = "synced:"

// Compile-time assertion: the Badger engine implements SyncedHashStore.
var _ metadata.SyncedHashStore = (*BadgerMetadataStore)(nil)

// keySyncedHash generates the key for a hash's synced marker.
func keySyncedHash(hash block.ContentHash) []byte {
	return append([]byte(syncedHashPrefix), hash[:]...)
}

// IsSynced reports whether hash has been mirrored to remote. Returns
// (false, nil) when no entry exists for hash.
func (s *BadgerMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var present bool
	err := s.db.View(func(txn *badger.Txn) error {
		_, err := txn.Get(keySyncedHash(hash))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		present = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("badger synced get: %w", err)
	}
	return present, nil
}

// MarkSynced records that hash has been mirrored to remote. Idempotent:
// re-applying the same hash is a no-op and returns nil (Badger Set
// overwrites the existing empty value).
func (s *BadgerMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(keySyncedHash(hash), nil)
	})
	if err != nil {
		return fmt.Errorf("badger synced mark: %w", err)
	}
	return nil
}

// DeleteSynced removes the synced marker for hash. Idempotent: deleting
// an absent hash returns nil (Badger's txn.Delete does not error on
// missing keys).
func (s *BadgerMetadataStore) DeleteSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(keySyncedHash(hash))
	})
	if err != nil {
		return fmt.Errorf("badger synced delete: %w", err)
	}
	return nil
}
