package badger

import (
	"context"
	"errors"
	"fmt"
	"time"

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
//   - synced:{32-byte-hash}  value = 8 big-endian nanos of first-mirror time
//     (presence == synced; legacy markers carry an empty value)
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

// MarkSynced records that hash has been mirrored to remote, stamping the
// marker value with the current time as 8 big-endian nanos. Idempotent and
// first-write-wins: re-applying an already-marked hash is a no-op that
// preserves the original timestamp (matching the SQL backends' ON CONFLICT DO
// NOTHING), so EnumerateSynced reports when the hash was FIRST mirrored — the
// grace anchor for the LIST-free sweep. Markers written before timestamps
// existed carry an empty value and decode to a zero time (fail-closed: the
// sweep leaves them for the periodic reconcile).
func (s *BadgerMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.db.Update(func(txn *badger.Txn) error {
		key := keySyncedHash(hash)
		if _, gerr := txn.Get(key); gerr == nil {
			return nil // already marked — preserve first-write timestamp
		} else if !errors.Is(gerr, badger.ErrKeyNotFound) {
			return gerr
		}
		return txn.Set(key, encodeInt64(time.Now().UnixNano()))
	})
	if err != nil {
		return fmt.Errorf("badger synced mark: %w", err)
	}
	return nil
}

// EnumerateSynced streams every synced marker with its first-mirror time via a
// prefix scan over synced:. Collects under a read txn then calls fn outside
// iteration so the callback never runs with the iterator open. Used by the
// LIST-free GC sweep to compute remote-orphan candidates without an S3 LIST.
func (s *BadgerMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prefix := []byte(syncedHashPrefix)

	type entry struct {
		hash     block.ContentHash
		syncedAt time.Time
	}
	var entries []entry

	err := s.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			key := item.KeyCopy(nil)
			raw := key[len(prefix):]
			if len(raw) != len(block.ContentHash{}) {
				continue
			}
			var h block.ContentHash
			copy(h[:], raw)
			var syncedAt time.Time
			if verr := item.Value(func(val []byte) error {
				if nanos := decodeInt64(val); nanos != 0 {
					syncedAt = time.Unix(0, nanos)
				}
				return nil
			}); verr != nil {
				return verr
			}
			entries = append(entries, entry{hash: h, syncedAt: syncedAt})
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("badger synced enumerate: %w", err)
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(e.hash, e.syncedAt); err != nil {
			return err
		}
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
