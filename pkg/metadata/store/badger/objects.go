package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileBlockStore Implementation for BadgerDB Store
// ============================================================================
//
// This file implements the FileBlockStore interface for the BadgerDB metadata store.
// It provides content-addressed file block tracking for deduplication and caching.
//
// The FileBlockStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileBlock + ListFileBlocks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers.
//
// Key Prefixes:
//   - fb:{id}          - FileBlock data (keyed by UUID)
//   - fb-hash:{hash}   - Hash index: content hash -> block ID
//   - fb-local:{id}    - Local-state secondary index for ListPending
//   - fb-file:{pid}:{n}- Per-file secondary index for ListFileBlocks
//
// Thread Safety: All operations use BadgerDB transactions for ACID guarantees.
//
// ============================================================================

const (
	fileBlockPrefix      = "fb:"
	fileBlockHashPrefix  = "fb-hash:"
	fileBlockLocalPrefix = "fb-local:"
	fileBlockFilePrefix  = "fb-file:"
)

// Ensure BadgerMetadataStore implements FileBlockStore
var _ blockstore.FileBlockStore = (*BadgerMetadataStore)(nil)

// reapBlockTxn deletes a FileBlock's primary key plus every secondary index
// (local, file, hash) inside the supplied transaction. Shared by Delete and
// the decrement-and-reap paths so the teardown stays in one place. The caller
// has already loaded `block` (its Hash drives the hash-index cleanup).
func reapBlockTxn(txn *badger.Txn, id string, block *metadata.FileBlock) error {
	if err := txn.Delete([]byte(fileBlockPrefix + id)); err != nil {
		return err
	}
	_ = txn.Delete([]byte(fileBlockLocalPrefix + id))
	if pid, idx, ok := splitBlockID(id); ok {
		_ = txn.Delete([]byte(fileBlockFilePrefix + pid + ":" + idx))
	}
	if block.IsFinalized() {
		_ = txn.Delete([]byte(fileBlockHashPrefix + block.Hash.String()))
	}
	return nil
}

// ============================================================================
// FileBlock Operations
// ============================================================================

// GetFileBlock retrieves a file block by its ID. Not on the narrowed
// FileBlockStore interface; kept as a backend
// method for engine-internal callers.
func (s *BadgerMetadataStore) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	var block metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		})
	})
	if err != nil {
		return nil, err
	}
	return &block, nil
}

// Put stores or updates a file block. Renamed from PutFileBlock to
// match the narrowed interface.
func (s *BadgerMetadataStore) Put(ctx context.Context, block *metadata.FileBlock) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + block.ID)
		val, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("marshal file block: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Maintain local index: add when Pending, remove otherwise.
		// This allows ListPending to iterate O(local) instead of O(all).
		localKey := []byte(fileBlockLocalPrefix + block.ID)
		if block.State == metadata.BlockStatePending {
			if err := txn.Set(localKey, nil); err != nil {
				return err
			}
		} else {
			_ = txn.Delete(localKey) // Ignore ErrKeyNotFound
		}

		// Maintain file index: fb-file:{payloadID}:{blockIdx} -> block.ID
		// This allows ListFileBlocks to iterate O(file_blocks) via prefix scan.
		if pid, idx, ok := splitBlockID(block.ID); ok {
			fileKey := []byte(fileBlockFilePrefix + pid + ":" + idx)
			if err := txn.Set(fileKey, []byte(block.ID)); err != nil {
				return err
			}
		}

		// Update hash index for finalized blocks
		if block.IsFinalized() {
			hashKey := []byte(fileBlockHashPrefix + block.Hash.String())
			return txn.Set(hashKey, []byte(block.ID))
		}
		return nil
	})
}

// Delete removes a file block by its ID. Renamed from DeleteFileBlock in
func (s *BadgerMetadataStore) Delete(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)

		// Get block to find hash for index cleanup
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}

		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}

		return reapBlockTxn(txn, id, &block)
	})
}

// updateWithConflictRetry wraps s.db.Update with the same retry-on-
// ErrConflict loop used by WithTransaction (transaction.go). BadgerDB's
// optimistic concurrency control surfaces ErrConflict when two
// in-flight Updates touch the same key — the retry converts that into
// the "atomic, TOCTOU-free" contract mandates for the refcount
// mutators (IncrementRefCount, DecrementRefCount, AddRef). Returns the
// last conflict error if all retries are exhausted; non-conflict
// errors short-circuit.
func (s *BadgerMetadataStore) updateWithConflictRetry(ctx context.Context, fn func(*badger.Txn) error) error {
	var lastErr error
	for attempt := 0; attempt < maxTransactionRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.db.Update(fn)
		if err == nil {
			return nil
		}
		if err == badger.ErrConflict {
			lastErr = err
			baseDelay := time.Duration(1+attempt) * time.Millisecond
			jitter := time.Duration(attempt) * time.Millisecond
			time.Sleep(baseDelay + jitter)
			continue
		}
		return err
	}
	return lastErr
}

// IncrementRefCount atomically increments a block's RefCount. Retries
// on badger.ErrConflict so contended +1/-1/AddRef workloads converge.
func (s *BadgerMetadataStore) IncrementRefCount(ctx context.Context, id string) error {
	return s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		block.RefCount++
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
}

// DecrementRefCount atomically decrements a block's RefCount. Retries
// on badger.ErrConflict so contended +1/-1/AddRef workloads converge.
func (s *BadgerMetadataStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileBlockNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		if block.RefCount > 0 {
			block.RefCount--
		}
		newCount = block.RefCount
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// DecrementRefCountAndReap atomically decrements a block's RefCount and, when
// the new count is 0, deletes the fb:{id} row plus its secondary indexes
// (local, file, hash) inside the SAME db.Update transaction as the decrement —
// TOCTOU-free against a concurrent AddRef (same retry-on-conflict idiom as
// DecrementRefCount). Returns (0, nil) when the row is already absent.
func (s *BadgerMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			newCount = 0
			return nil // tolerate already-swept row
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		if block.RefCount > 0 {
			block.RefCount--
		}
		newCount = block.RefCount
		if block.RefCount == 0 {
			// Reap so the hash leaves GetByHash / the GC live set.
			return reapBlockTxn(txn, id, &block)
		}
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
	return newCount, err
}

// AddRef atomically bumps RefCount on the FileBlock row indexed by the
// given content hash. Implements the FileBlockStore.AddRef contract
// used by the in-memory hash dedup LRU hit path to
// reference an already-stored block without creating a new row.
//
// Atomicity: the entire hash→id secondary-index lookup, fb:{id} fetch,
// RefCount++, and Set run inside a single s.db.Update transaction so
// AddRef is TOCTOU-free against concurrent DecrementRefCount cascade
// (matches the existing IncrementRefCount idiom).
//
// Returns metadata.ErrUnknownHash on:
//   - fb-hash:{hash} secondary-index miss (the hash has never been Put), AND
//   - fb:{id} value miss after a successful index hit (index/value desync
//     — defends against orphan-index scenarios; should not normally
//     happen but maps to the same caller behavior: fall back to full Put).
//
// RefCount is the ONLY field mutated. BlockState is preserved
// across the read-modify-write (Pending stays Pending, Remote stays
// Remote — no transition is fired by the hit path).
func (s *BadgerMetadataStore) AddRef(ctx context.Context, hash blockstore.ContentHash, _ string, _ blockstore.BlockRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// badger backend records ref count only — parameters intentionally
	// blanked.
	return s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		// Resolve hash → id via the secondary index.
		hashKey := []byte(fileBlockHashPrefix + hash.String())
		hashItem, err := txn.Get(hashKey)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrUnknownHash
		}
		if err != nil {
			return err
		}
		var id string
		if err := hashItem.Value(func(val []byte) error {
			id = string(val)
			return nil
		}); err != nil {
			return err
		}

		// Fetch the FileBlock value.
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			// Index/value desync — treat as unknown so the LRU
			// caller falls back to the full Put path.
			return metadata.ErrUnknownHash
		}
		if err != nil {
			return err
		}
		var block metadata.FileBlock
		if err := item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return err
		}
		block.RefCount++
		val, err := json.Marshal(&block)
		if err != nil {
			return err
		}
		return txn.Set(key, val)
	})
}

// GetByHash looks up a finalized block by its content hash.
// Returns nil without error if not found. Renamed from FindFileBlockByHash
func (s *BadgerMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	var block metadata.FileBlock
	var found bool
	err := s.db.View(func(txn *badger.Txn) error {
		// Look up ID via hash index
		hashKey := []byte(fileBlockHashPrefix + hash.String())
		hashItem, err := txn.Get(hashKey)
		if err == badger.ErrKeyNotFound {
			return nil // Not found
		}
		if err != nil {
			return err
		}

		var id string
		if err := hashItem.Value(func(val []byte) error {
			id = string(val)
			return nil
		}); err != nil {
			return err
		}

		// Fetch the block
		key := []byte(fileBlockPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return nil // Index stale, block deleted
		}
		if err != nil {
			return err
		}
		found = true
		return item.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		})
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	// Only return remote blocks for dedup safety
	if !block.IsRemote() {
		return nil, nil
	}
	return &block, nil
}

// GetByHashAllStates resolves a block by content hash regardless of state.
// The fb-hash: secondary index only carries finalized (Remote) rows, so a
// Pending/Syncing row cannot be found through it — this scans the fb: prefix
// for the first row whose Hash matches. Returns (nil, nil) when none match.
// Implements the FileBlockStore.GetByHashAllStates contract (reap path only).
func (s *BadgerMetadataStore) GetByHashAllStates(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	var result *metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		var ferr error
		result, ferr = getByHashAllStatesTxn(ctx, txn, hash)
		return ferr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// getByHashAllStatesTxn scans the fb: prefix in the supplied txn for the first
// FileBlock whose Hash equals the target (any state). Shared by the store-level
// View path and the transaction path so a tx-bound Put is visible to a later
// reap in the same WithTransaction.
func getByHashAllStatesTxn(ctx context.Context, txn *badger.Txn, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	if hash.IsZero() {
		return nil, nil
	}
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.PrefetchSize = 256
	prefix := []byte(fileBlockPrefix)
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("get by hash all states: %w", err)
		}
		var block metadata.FileBlock
		if err := it.Item().Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			return nil, fmt.Errorf("decode file block: %w", err)
		}
		if block.Hash == hash {
			b := block
			return &b, nil
		}
	}
	return nil, nil
}

// ListPending returns blocks in Pending state (complete, on disk, not yet
// synced to remote) older than the given duration. Renamed from
// ListLocalBlocks; the underlying semantics already match ("Local" was
// renamed Pending).
// If limit > 0, at most limit blocks are returned.
//
// Uses the fb-local: secondary index for O(local) iteration instead of
// scanning all fb: entries. This eliminates the BadgerDB full-table scan
// that was the root cause of sequential write throughput degradation.
func (s *BadgerMetadataStore) ListPending(_ context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		result = listPendingTxn(txn, olderThan, limit)
		return nil
	})
	return result, err
}

// ListFileBlocks returns all blocks belonging to a file, ordered by block index.
// Uses the fb-file:{payloadID}: secondary index for efficient O(file_blocks) queries.
// Not on the narrowed FileBlockStore interface;
// kept as a backend method for engine-internal callers.
func (s *BadgerMetadataStore) ListFileBlocks(_ context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	var result []*metadata.FileBlock
	err := s.db.View(func(txn *badger.Txn) error {
		result = listFileBlocksTxn(txn, payloadID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EnumerateFileBlocks streams every FileBlock's ContentHash through fn using
// a Badger prefix iterator over fb:. The iterator yields one row per block
// (no allocation of a full slice in application memory)..
// lifted from FileBlockStore to MetadataStore
// implementation unchanged.
func (s *BadgerMetadataStore) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return s.db.View(func(txn *badger.Txn) error {
		return enumerateFileBlocksTxn(ctx, txn, fn)
	})
}

// listPendingTxn / listFileBlocksTxn / enumerateFileBlocksTxn iterate a given
// *badger.Txn so the store-level methods (over a db.View snapshot) and the
// transaction-level methods (over the active write txn, for read-after-write)
// share one implementation. Binding to the caller's txn is what lets a
// tx.Put be observed by a later tx.ListFileBlocks in the same WithTransaction.
func listPendingTxn(txn *badger.Txn, olderThan time.Duration, limit int) []*metadata.FileBlock {
	// olderThan <= 0 means "no age filter" — return every local block.
	var cutoff time.Time
	filterByAge := olderThan > 0
	if filterByAge {
		cutoff = time.Now().Add(-olderThan)
	}
	var result []*metadata.FileBlock
	prefix := []byte(fileBlockLocalPrefix)
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = false // Keys only — values are empty
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		id := string(it.Item().Key()[len(prefix):])
		fbItem, err := txn.Get([]byte(fileBlockPrefix + id))
		if err != nil {
			continue // Index stale, block deleted
		}
		var block metadata.FileBlock
		if err := fbItem.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			continue
		}
		if !block.HasLocalFile() {
			continue
		}
		if filterByAge && !block.LastAccess.Before(cutoff) {
			continue
		}
		result = append(result, &block)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func listFileBlocksTxn(txn *badger.Txn, payloadID string) []*metadata.FileBlock {
	var result []*metadata.FileBlock
	prefix := []byte(fileBlockFilePrefix + payloadID + ":")
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = true
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		var blockID string
		if err := it.Item().Value(func(val []byte) error {
			blockID = string(val)
			return nil
		}); err != nil {
			continue
		}
		fbItem, err := txn.Get([]byte(fileBlockPrefix + blockID))
		if err != nil {
			continue // Index stale, block deleted
		}
		var block metadata.FileBlock
		if err := fbItem.Value(func(val []byte) error {
			return json.Unmarshal(val, &block)
		}); err != nil {
			continue
		}
		result = append(result, &block)
	}
	// Keys are lexicographically sorted (fb-file:{payloadID}:0, :1, :10, :2...)
	// which gives wrong numeric order for multi-digit indices. Sort by parsed index.
	sort.Slice(result, func(i, j int) bool {
		return parseBlockIdx(result[i].ID) < parseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileBlock{}
	}
	return result
}

// enumerateFileBlocksTxn streams the GC mark live set: the UNION of the CAS
// index (fb: entries) and the per-file manifest (f: File.Blocks). Unioning both
// makes the live set a strict SUPERSET of both structures — the snapshot Backup
// HashSet is built from f: File.Blocks alone, so a hash present only there
// (manifest row without a fb: CAS row, or one already reaped) would otherwise be
// missed by the mark phase and the sweep would reap a still-live chunk once a
// snapshot hold lapsed (data loss). Duplicates across the two passes are
// harmless — GCState.Add deduplicates the live set.
func enumerateFileBlocksTxn(ctx context.Context, txn *badger.Txn, fn func(blockstore.ContentHash) error) error {
	// Each pass is scoped so its iterator is released before the next opens.
	if err := enumeratePrefixHashes(ctx, txn, fileBlockPrefix, func(val []byte) (blockstore.ContentHash, error) {
		var block metadata.FileBlock
		if err := json.Unmarshal(val, &block); err != nil {
			return blockstore.ContentHash{}, fmt.Errorf("decode file block: %w", err)
		}
		return block.Hash, nil
	}, fn); err != nil {
		return err
	}
	// Per-file manifest (f: File.Blocks). A file carries multiple hashes, so
	// this pass emits each block ref individually.
	return enumeratePrefixFileBlocks(ctx, txn, fn)
}

// enumeratePrefixHashes iterates a key prefix, decoding one hash per entry via
// decodeHash and streaming it through fn. Used by the fb: (CAS index) pass.
func enumeratePrefixHashes(
	ctx context.Context,
	txn *badger.Txn,
	prefixStr string,
	decodeHash func([]byte) (blockstore.ContentHash, error),
	fn func(blockstore.ContentHash) error,
) error {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.PrefetchSize = 256
	prefix := []byte(prefixStr)
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		var h blockstore.ContentHash
		if err := it.Item().Value(func(val []byte) error {
			var derr error
			h, derr = decodeHash(val)
			return derr
		}); err != nil {
			return err
		}
		if err := fn(h); err != nil {
			return err
		}
	}
	return nil
}

// enumeratePrefixFileBlocks iterates the f: (File) prefix and streams every
// File.Blocks hash through fn. Decode failures are fatal (fail-closed): a
// dropped file entry would shrink the GC live set and let the sweep reap a
// still-live chunk.
func enumeratePrefixFileBlocks(ctx context.Context, txn *badger.Txn, fn func(blockstore.ContentHash) error) error {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.PrefetchSize = 256
	prefix := []byte(prefixFile)
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file blocks: %w", err)
		}
		var file *metadata.File
		if err := it.Item().Value(func(val []byte) error {
			f, derr := decodeFile(val)
			if derr != nil {
				return derr
			}
			file = f
			return nil
		}); err != nil {
			return fmt.Errorf("enumerate file blocks: decode file: %w", err)
		}
		for _, br := range file.Blocks {
			if err := fn(br.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitBlockID splits a block ID into (payloadID, blockIdx) on the LAST
// "/" separator. Nested payloadIDs (e.g. "share/dir/file/0") produce the
// correct payloadID prefix for the fb-file: secondary index. Returns
// ("", "", false) when the ID contains no "/".
func splitBlockID(id string) (pid, idx string, ok bool) {
	lastSlash := strings.LastIndex(id, "/")
	if lastSlash <= 0 {
		return "", "", false
	}
	return id[:lastSlash], id[lastSlash+1:], true
}

// parseBlockIdx extracts the numeric block index from a block ID ("{payloadID}/{blockIdx}").
func parseBlockIdx(id string) int {
	if _, idx, ok := splitBlockID(id); ok {
		var v int
		if _, err := fmt.Sscanf(idx, "%d", &v); err == nil {
			return v
		}
	}
	return 0
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure badgerTransaction implements FileBlockStore
var _ blockstore.FileBlockStore = (*badgerTransaction)(nil)

// (review iteration 1): FileBlockStore methods on
// badgerTransaction MUST run against the txn's *badger.Txn so a
// rollback (returning an error from WithTransaction's fn) discards the
// RefCount mutation. Previously every method called `tx.store.X(...)`
// which opened its own db.Update — defeating rollback for any caller
// that bumped RefCount inside WithTransaction then encountered a
// downstream PutFile failure (silent leak).

func (tx *badgerTransaction) GetFileBlock(ctx context.Context, id string) (*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return nil, metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return nil, err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return nil, err
	}
	return &block, nil
}

func (tx *badgerTransaction) Put(ctx context.Context, block *metadata.FileBlock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := []byte(fileBlockPrefix + block.ID)
	val, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("marshal file block: %w", err)
	}
	if err := tx.txn.Set(key, val); err != nil {
		return err
	}
	localKey := []byte(fileBlockLocalPrefix + block.ID)
	if block.State == metadata.BlockStatePending {
		if err := tx.txn.Set(localKey, nil); err != nil {
			return err
		}
	} else {
		_ = tx.txn.Delete(localKey)
	}
	if pid, idx, ok := splitBlockID(block.ID); ok {
		fileKey := []byte(fileBlockFilePrefix + pid + ":" + idx)
		if err := tx.txn.Set(fileKey, []byte(block.ID)); err != nil {
			return err
		}
	}
	if block.IsFinalized() {
		hashKey := []byte(fileBlockHashPrefix + block.Hash.String())
		return tx.txn.Set(hashKey, []byte(block.ID))
	}
	return nil
}

func (tx *badgerTransaction) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return err
	}
	return reapBlockTxn(tx.txn, id, &block)
}

// IncrementRefCount runs the +1 read-modify-write under the active
// badger.Txn so a subsequent rollback discards the mutation (fix).
func (tx *badgerTransaction) IncrementRefCount(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return err
	}
	block.RefCount++
	val, err := json.Marshal(&block)
	if err != nil {
		return err
	}
	return tx.txn.Set(key, val)
}

// DecrementRefCount runs the -1 read-modify-write under the active
// badger.Txn so a subsequent rollback discards the mutation (fix).
func (tx *badgerTransaction) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return 0, metadata.ErrFileBlockNotFound
	}
	if err != nil {
		return 0, err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return 0, err
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	newCount := block.RefCount
	val, err := json.Marshal(&block)
	if err != nil {
		return 0, err
	}
	if err := tx.txn.Set(key, val); err != nil {
		return 0, err
	}
	return newCount, nil
}

// DecrementRefCountAndReap runs the -1 read-modify-write + reap-at-zero under
// the active badger.Txn so a subsequent rollback discards both the decrement
// and the row deletion. Returns (0, nil) when the row is already absent.
func (tx *badgerTransaction) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return 0, nil // tolerate already-swept row
	}
	if err != nil {
		return 0, err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return 0, err
	}
	if block.RefCount > 0 {
		block.RefCount--
	}
	newCount := block.RefCount
	if block.RefCount == 0 {
		return 0, reapBlockTxn(tx.txn, id, &block)
	}
	val, err := json.Marshal(&block)
	if err != nil {
		return 0, err
	}
	if err := tx.txn.Set(key, val); err != nil {
		return 0, err
	}
	return newCount, nil
}

// AddRef runs the hash→id resolve + RefCount++ read-modify-write under
// the active badger.Txn so a subsequent rollback discards the mutation
// (mirrors the fix applied to IncrementRefCount). Returns
// metadata.ErrUnknownHash on index miss or value miss.
func (tx *badgerTransaction) AddRef(ctx context.Context, hash metadata.ContentHash, _ string, _ blockstore.BlockRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// badger backend records ref count only — parameters intentionally
	// blanked.
	if err := ctx.Err(); err != nil {
		return err
	}
	hashKey := []byte(fileBlockHashPrefix + hash.String())
	hashItem, err := tx.txn.Get(hashKey)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrUnknownHash
	}
	if err != nil {
		return err
	}
	var id string
	if err := hashItem.Value(func(val []byte) error {
		id = string(val)
		return nil
	}); err != nil {
		return err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrUnknownHash
	}
	if err != nil {
		return err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return err
	}
	block.RefCount++
	val, err := json.Marshal(&block)
	if err != nil {
		return err
	}
	return tx.txn.Set(key, val)
}

// GetByHash runs against the active badger.Txn (BadgerDB transactions
// see snapshot-isolated reads, so this returns the value AS modified by
// any prior tx-bound mutations — important when the coordinator does
// GetByHash → IncrementRefCount inside the same tx).
func (tx *badgerTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hashKey := []byte(fileBlockHashPrefix + hash.String())
	hashItem, err := tx.txn.Get(hashKey)
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id string
	if err := hashItem.Value(func(val []byte) error {
		id = string(val)
		return nil
	}); err != nil {
		return nil, err
	}
	key := []byte(fileBlockPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var block metadata.FileBlock
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return nil, err
	}
	if !block.IsRemote() {
		return nil, nil
	}
	return &block, nil
}

// GetByHashAllStates runs against the active badger.Txn so a tx-bound Put is
// observed (snapshot-isolated reads see the tx's own pending writes).
func (tx *badgerTransaction) GetByHashAllStates(ctx context.Context, hash metadata.ContentHash) (*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return getByHashAllStatesTxn(ctx, tx.txn, hash)
}

// ListPending, ListFileBlocks, EnumerateFileBlocks iterate the active txn
// (tx.txn) rather than opening a fresh db.View snapshot. Delegating to the
// store path took a snapshot at call time, so a tx.Put followed by
// tx.ListFileBlocks in the same WithTransaction missed the uncommitted write —
// a cross-backend divergence vs memory, whose list helpers read live maps
// under the held lock. Binding to tx.txn gives read-after-write within the tx
// (BadgerDB transactions see their own pending writes).
func (tx *badgerTransaction) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listPendingTxn(tx.txn, olderThan, limit), nil
}

func (tx *badgerTransaction) ListFileBlocks(ctx context.Context, payloadID string) ([]*metadata.FileBlock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listFileBlocksTxn(tx.txn, payloadID), nil
}

func (tx *badgerTransaction) EnumerateFileBlocks(ctx context.Context, fn func(blockstore.ContentHash) error) error {
	return enumerateFileBlocksTxn(ctx, tx.txn, fn)
}
