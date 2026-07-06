package badger

import (
	"context"
	"encoding/binary"
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
//   - synced:{32-byte-hash}  value = 8 big-endian nanos of first-mirror time,
//     OPTIONALLY followed by a block-locator suffix (presence == synced; legacy
//     markers carry an empty value, decoded as a zero time + standalone locator)
//
// The 32-byte hash bytes are appended raw (matching rollup_offset's compact
// binary key encoding) rather than hex-encoded — Badger does not require
// printable keys, and raw bytes keep the key half the size on disk.
//
// Locator encoding (#1414): a standalone chunk (ChunkLocator.BlockID == "")
// writes ONLY the 8-byte timestamp — byte-for-byte identical to a pre-locator
// marker — because its location is fully implied by its hash. A block-resident
// chunk appends, after the timestamp: a uint16 BlockID length, the BlockID bytes,
// then int64 Offset and int64 Length (all big-endian). A value of <= 8 bytes
// therefore decodes to a standalone locator with no migration of existing rows.
// ============================================================================

const syncedHashPrefix = "synced:"

// encodeSyncedValue builds the marker value: an 8-byte first-mirror timestamp
// followed, only for a block-resident chunk, by the locator suffix described
// above. Standalone chunks produce the legacy 8-byte form.
func encodeSyncedValue(nanos int64, loc block.ChunkLocator) []byte {
	val := encodeInt64(nanos)
	if loc.IsStandalone() {
		return val
	}
	suffix := make([]byte, 2+len(loc.BlockID)+16)
	binary.BigEndian.PutUint16(suffix[0:2], uint16(len(loc.BlockID)))
	n := 2 + copy(suffix[2:], loc.BlockID)
	binary.BigEndian.PutUint64(suffix[n:n+8], uint64(loc.WireOffset))
	binary.BigEndian.PutUint64(suffix[n+8:n+16], uint64(loc.WireLength))
	return append(val, suffix...)
}

// decodeSyncedLocator extracts the block locator from a marker value. A value of
// 8 bytes or fewer (legacy/standalone) yields the zero (standalone) locator. A
// malformed suffix also falls back to standalone — fail-safe, since standalone
// is always a valid resolution.
func decodeSyncedLocator(val []byte) block.ChunkLocator {
	if len(val) <= 8 {
		return block.ChunkLocator{}
	}
	suffix := val[8:]
	if len(suffix) < 2 {
		return block.ChunkLocator{}
	}
	idLen := int(binary.BigEndian.Uint16(suffix[0:2]))
	if len(suffix) < 2+idLen+16 {
		return block.ChunkLocator{}
	}
	blockID := string(suffix[2 : 2+idLen])
	off := int64(binary.BigEndian.Uint64(suffix[2+idLen : 2+idLen+8]))
	length := int64(binary.BigEndian.Uint64(suffix[2+idLen+8 : 2+idLen+16]))
	return block.ChunkLocator{BlockID: blockID, WireOffset: off, WireLength: length}
}

// Compile-time assertions: the Badger engine and its transaction implement
// SyncedHashStore.
var (
	_ metadata.SyncedHashStore = (*BadgerMetadataStore)(nil)
	_ metadata.SyncedHashStore = (*badgerTransaction)(nil)
)

// keySyncedHash generates the key for a hash's synced marker.
func keySyncedHash(hash block.ContentHash) []byte {
	return append([]byte(syncedHashPrefix), hash[:]...)
}

// ============================================================================
// Shared per-Txn bodies
// ============================================================================
//
// The store-level methods run each body in its own View/Update; the
// transaction-level methods (metadata.Transaction embeds SyncedHashStore) run
// them against the enclosing WithTransaction txn, where Badger natively gives
// read-your-writes: a Set/Delete inside the txn is visible to a later Get in
// the same txn, so DeleteSynced-then-MarkSynced records the new locator.

// isSyncedTxn reports marker presence within txn.
func isSyncedTxn(txn *badger.Txn, hash block.ContentHash) (bool, error) {
	_, err := txn.Get(keySyncedHash(hash))
	if errors.Is(err, badger.ErrKeyNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// markSyncedTxn writes the marker within txn. First-write-wins: an existing
// marker (committed or pending in this txn) is preserved untouched.
func markSyncedTxn(txn *badger.Txn, hash block.ContentHash, loc block.ChunkLocator) error {
	key := keySyncedHash(hash)
	if _, gerr := txn.Get(key); gerr == nil {
		return nil // already marked — preserve first-write timestamp + locator
	} else if !errors.Is(gerr, badger.ErrKeyNotFound) {
		return gerr
	}
	return txn.Set(key, encodeSyncedValue(time.Now().UnixNano(), loc))
}

// getLocatorTxn reads the marker's locator within txn.
func getLocatorTxn(txn *badger.Txn, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	item, gerr := txn.Get(keySyncedHash(hash))
	if errors.Is(gerr, badger.ErrKeyNotFound) {
		return block.ChunkLocator{}, false, nil
	}
	if gerr != nil {
		return block.ChunkLocator{}, false, gerr
	}
	var loc block.ChunkLocator
	if err := item.Value(func(val []byte) error {
		loc = decodeSyncedLocator(val)
		return nil
	}); err != nil {
		return block.ChunkLocator{}, false, err
	}
	return loc, true, nil
}

// deleteSyncedTxn removes the marker within txn. Idempotent (Badger's
// txn.Delete does not error on missing keys).
func deleteSyncedTxn(txn *badger.Txn, hash block.ContentHash) error {
	return txn.Delete(keySyncedHash(hash))
}

// IsSynced reports whether hash has been mirrored to remote. Returns
// (false, nil) when no entry exists for hash.
func (s *BadgerMetadataStore) IsSynced(ctx context.Context, hash block.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	var present bool
	err := s.db.View(func(txn *badger.Txn) error {
		var verr error
		present, verr = isSyncedTxn(txn, hash)
		return verr
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
func (s *BadgerMetadataStore) MarkSynced(ctx context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	err := s.db.Update(func(txn *badger.Txn) error {
		return markSyncedTxn(txn, hash, loc)
	})
	if err != nil {
		return fmt.Errorf("badger synced mark: %w", err)
	}
	return nil
}

// GetLocator returns the recorded remote locator for hash. (zero, false, nil)
// when unsynced; a synced hash with no block-locator suffix (standalone/legacy)
// yields the zero (standalone) locator with found == true.
func (s *BadgerMetadataStore) GetLocator(ctx context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	if err := ctx.Err(); err != nil {
		return block.ChunkLocator{}, false, err
	}

	var (
		loc   block.ChunkLocator
		found bool
	)
	err := s.db.View(func(txn *badger.Txn) error {
		var verr error
		loc, found, verr = getLocatorTxn(txn, hash)
		return verr
	})
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("badger synced get locator: %w", err)
	}
	return loc, found, nil
}

// EnumerateSynced streams every synced marker with its locator and first-mirror
// time via a prefix scan over synced:. Both the timestamp and the locator suffix
// live in the same marker value, so yielding the locator here lets callers
// resolve locators in a single scan instead of a GetLocator round trip per hash
// (#1554). Collects under a read txn then calls fn outside iteration so the
// callback never runs with the iterator open. A marker with no locator suffix
// yields the zero (standalone) locator.
func (s *BadgerMetadataStore) EnumerateSynced(ctx context.Context, fn func(hash block.ContentHash, loc block.ChunkLocator, syncedAt time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	prefix := []byte(syncedHashPrefix)

	type entry struct {
		hash     block.ContentHash
		loc      block.ChunkLocator
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
			var (
				syncedAt time.Time
				loc      block.ChunkLocator
			)
			if verr := item.Value(func(val []byte) error {
				if nanos := decodeInt64(val); nanos != 0 {
					syncedAt = time.Unix(0, nanos)
				}
				loc = decodeSyncedLocator(val)
				return nil
			}); verr != nil {
				return verr
			}
			entries = append(entries, entry{hash: h, loc: loc, syncedAt: syncedAt})
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
		if err := fn(e.hash, e.loc, e.syncedAt); err != nil {
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
		return deleteSyncedTxn(txn, hash)
	})
	if err != nil {
		return fmt.Errorf("badger synced delete: %w", err)
	}
	return nil
}

// ============================================================================
// Transaction-level SyncedHashStore
// ============================================================================
//
// Same executor plumbing as the transaction-level BlockRecordStore /
// LocalChunkIndex (block_record_store.go): operate directly on the enclosing
// tx.txn. Badger gives read-your-writes within a txn, so a MarkSynced after a
// DeleteSynced in the same transaction records the new locator. Conflicting
// concurrent writers surface as ErrConflict at commit, which the store's
// WithTransaction retry loop already handles.

func (tx *badgerTransaction) IsSynced(_ context.Context, hash block.ContentHash) (bool, error) {
	present, err := isSyncedTxn(tx.txn, hash)
	if err != nil {
		return false, fmt.Errorf("badger tx synced get: %w", err)
	}
	return present, nil
}

func (tx *badgerTransaction) MarkSynced(_ context.Context, hash block.ContentHash, loc block.ChunkLocator) error {
	if err := markSyncedTxn(tx.txn, hash, loc); err != nil {
		return fmt.Errorf("badger tx synced mark: %w", err)
	}
	return nil
}

func (tx *badgerTransaction) GetLocator(_ context.Context, hash block.ContentHash) (block.ChunkLocator, bool, error) {
	loc, found, err := getLocatorTxn(tx.txn, hash)
	if err != nil {
		return block.ChunkLocator{}, false, fmt.Errorf("badger tx synced get locator: %w", err)
	}
	return loc, found, nil
}

func (tx *badgerTransaction) DeleteSynced(_ context.Context, hash block.ContentHash) error {
	if err := deleteSyncedTxn(tx.txn, hash); err != nil {
		return fmt.Errorf("badger tx synced delete: %w", err)
	}
	return nil
}
