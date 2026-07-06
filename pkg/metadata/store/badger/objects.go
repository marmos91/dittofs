package badger

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	blockpkg "github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// ============================================================================
// FileChunkStore Implementation for BadgerDB Store
// ============================================================================
//
// This file implements the FileChunkStore interface for the BadgerDB metadata store.
// It provides content-addressed file chunk tracking for deduplication and caching.
//
// The FileChunkStore interface is narrowed to 6 methods. The backend
// retains the legacy GetFileChunk + ListFileChunks helpers as
// concrete methods on the struct (not on the public interface) for
// engine-internal callers.
//
// Key Prefixes:
//   - fb:{id}          - FileChunk data (keyed by UUID)
//   - fb-hash:{hash}   - Hash index: content hash -> block ID
//   - fb-file:{pid}:{n}- Per-file secondary index for ListFileChunks
//
// Thread Safety: All operations use BadgerDB transactions for ACID guarantees.
//
// ============================================================================

const (
	fileChunkPrefix     = "fb:"
	fileChunkHashPrefix = "fb-hash:"
	fileChunkFilePrefix = "fb-file:"
)

// Ensure BadgerMetadataStore implements FileChunkStore
var _ blockpkg.FileChunkStore = (*BadgerMetadataStore)(nil)

// reapBlockTxn deletes a FileChunk's primary key plus every secondary index
// (file, hash) inside the supplied transaction. Shared by Delete and
// the decrement-and-reap paths so the teardown stays in one place. The caller
// has already loaded `block` (its Hash drives the hash-index cleanup).
func reapBlockTxn(txn *badger.Txn, id string, block *metadata.FileChunk) error {
	if err := txn.Delete([]byte(fileChunkPrefix + id)); err != nil {
		return err
	}
	if pid, idx, ok := splitBlockID(id); ok {
		_ = txn.Delete([]byte(fileChunkFilePrefix + pid + ":" + idx))
	}
	if block.IsFinalized() {
		_ = txn.Delete([]byte(fileChunkHashPrefix + block.Hash.String()))
	}
	return nil
}

// ============================================================================
// FileChunk Operations
// ============================================================================

// GetFileChunk retrieves a file chunk by its ID. Not on the narrowed
// FileChunkStore interface; kept as a backend
// method for engine-internal callers.
func (s *BadgerMetadataStore) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	var block metadata.FileChunk
	err := s.db.View(func(txn *badger.Txn) error {
		key := []byte(fileChunkPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileChunkNotFound
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

// Put stores or updates a file chunk. Renamed from PutFileChunk to
// match the narrowed interface.
func (s *BadgerMetadataStore) Put(ctx context.Context, block *metadata.FileChunk) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileChunkPrefix + block.ID)
		val, err := json.Marshal(block)
		if err != nil {
			return fmt.Errorf("marshal file chunk: %w", err)
		}
		if err := txn.Set(key, val); err != nil {
			return err
		}

		// Maintain file index: fb-file:{payloadID}:{blockIdx} -> block.ID
		// This allows ListFileChunks to iterate O(file_blocks) via prefix scan.
		if pid, idx, ok := splitBlockID(block.ID); ok {
			fileKey := []byte(fileChunkFilePrefix + pid + ":" + idx)
			if err := txn.Set(fileKey, []byte(block.ID)); err != nil {
				return err
			}
		}

		// Update hash index for finalized blocks
		if block.IsFinalized() {
			hashKey := []byte(fileChunkHashPrefix + block.Hash.String())
			return txn.Set(hashKey, []byte(block.ID))
		}
		return nil
	})
}

// Delete removes a file chunk by its ID. Renamed from DeleteFileChunk in
func (s *BadgerMetadataStore) Delete(ctx context.Context, id string) error {
	return s.db.Update(func(txn *badger.Txn) error {
		key := []byte(fileChunkPrefix + id)

		// Get block to find hash for index cleanup
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileChunkNotFound
		}
		if err != nil {
			return err
		}

		var block metadata.FileChunk
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
	for attempt := 0; attempt < int(maxTransactionRetries.Load()); attempt++ {
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
		key := []byte(fileChunkPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileChunkNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileChunk
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
		key := []byte(fileChunkPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			return metadata.ErrFileChunkNotFound
		}
		if err != nil {
			return err
		}
		var block metadata.FileChunk
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
// (file, hash) inside the SAME db.Update transaction as the decrement —
// TOCTOU-free against a concurrent AddRef (same retry-on-conflict idiom as
// DecrementRefCount). Returns (0, nil) when the row is already absent.
func (s *BadgerMetadataStore) DecrementRefCountAndReap(ctx context.Context, id string) (uint32, error) {
	var newCount uint32
	err := s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		key := []byte(fileChunkPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			newCount = 0
			return nil // tolerate already-swept row
		}
		if err != nil {
			return err
		}
		var block metadata.FileChunk
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

// AddRef atomically bumps RefCount on the FileChunk row indexed by the
// given content hash. Implements the FileChunkStore.AddRef contract
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
func (s *BadgerMetadataStore) AddRef(ctx context.Context, hash blockpkg.ContentHash, _ string, _ blockpkg.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// badger backend records ref count only — parameters intentionally
	// blanked.
	return s.updateWithConflictRetry(ctx, func(txn *badger.Txn) error {
		// Resolve hash → id via the secondary index.
		hashKey := []byte(fileChunkHashPrefix + hash.String())
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

		// Fetch the FileChunk value.
		key := []byte(fileChunkPrefix + id)
		item, err := txn.Get(key)
		if err == badger.ErrKeyNotFound {
			// Index/value desync — treat as unknown so the LRU
			// caller falls back to the full Put path.
			return metadata.ErrUnknownHash
		}
		if err != nil {
			return err
		}
		var block metadata.FileChunk
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
// Returns nil without error if not found. Renamed from FindFileChunkByHash
func (s *BadgerMetadataStore) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	var block metadata.FileChunk
	var found bool
	err := s.db.View(func(txn *badger.Txn) error {
		// Look up ID via hash index
		hashKey := []byte(fileChunkHashPrefix + hash.String())
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
		key := []byte(fileChunkPrefix + id)
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

// ListFileChunks returns all blocks belonging to a file, ordered by block index.
// Uses the fb-file:{payloadID}: secondary index for efficient O(file_blocks) queries.
// Not on the narrowed FileChunkStore interface;
// kept as a backend method for engine-internal callers.
func (s *BadgerMetadataStore) ListFileChunks(_ context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	var result []*metadata.FileChunk
	err := s.db.View(func(txn *badger.Txn) error {
		result = listFileChunksTxn(txn, payloadID)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetFileChunkAtOffset returns the FileChunk covering absolute byte offset off
// for payloadID — the row with the largest chunkOffset <= off whose range
// [chunkOffset, chunkOffset+DataSize) contains off — or (nil, nil) for a sparse
// hole, an empty payload, or a read past EOF. This is the read hot path: a
// keys-only scan of the fb-file:{payloadID}: secondary index finds the covering
// offset without materializing the whole manifest (no per-row Get, no JSON
// unmarshal, no sort), then two point Gets fetch the winning row.
//
// ponytail: O(n) keys-only scan per read; upgrade to a big-endian fb-off index
// for a true O(log n) reverse-seek only if profiling at real N still shows it.
func (s *BadgerMetadataStore) GetFileChunkAtOffset(_ context.Context, payloadID string, off uint64) (*metadata.FileChunk, error) {
	var result *metadata.FileChunk
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileChunkFilePrefix + payloadID + ":")
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false // keys only — the offset lives in the key
		it := txn.NewIterator(opts)
		defer it.Close()

		var (
			bestOff uint64
			found   bool
		)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			suffix := it.Item().Key()[len(prefix):]
			cand, perr := strconv.ParseUint(string(suffix), 10, 64)
			if perr != nil {
				continue // tolerate a malformed index row, like parseBlockIdx
			}
			if cand <= off && (!found || cand > bestOff) {
				bestOff, found = cand, true
			}
		}
		if !found {
			return nil // nothing starts at or before off — hole
		}

		// Fetch the winning row: index key -> block ID -> primary row. A stale
		// index row (deleted block) is treated as a hole, mirroring
		// listFileChunksTxn's "index stale, block deleted" skip.
		idxItem, gerr := txn.Get([]byte(fileChunkFilePrefix + payloadID + ":" + strconv.FormatUint(bestOff, 10)))
		if gerr != nil {
			return nil
		}
		var blockID string
		if verr := idxItem.Value(func(val []byte) error {
			blockID = string(val)
			return nil
		}); verr != nil {
			return nil
		}
		fbItem, gerr := txn.Get([]byte(fileChunkPrefix + blockID))
		if gerr != nil {
			return nil
		}
		var fc metadata.FileChunk
		if verr := fbItem.Value(func(val []byte) error {
			return json.Unmarshal(val, &fc)
		}); verr != nil {
			return nil
		}
		// Covering guard: the scan guarantees bestOff <= off; require
		// off < bestOff+DataSize, else off falls in a hole past this chunk and
		// must NOT be served this chunk's bytes.
		if off >= bestOff+uint64(fc.DataSize) {
			return nil
		}
		result = &fc
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EnumerateLivePayloadIDs streams every distinct PayloadID referenced by a live
// inode. It scans the f: inode keyspace, decodes each file record, and collects
// distinct non-empty PayloadIDs. Corrupt inode records are skipped so a single
// bad row cannot block the reconcile. Hardlinks share one f: key, so DISTINCT
// yields one payloadID per content. nlink=0 (unlinked) inodes are excluded
// (#1433): their payload is dead, so the reconcile treats it as stranded.
func (s *BadgerMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	seen := make(map[string]struct{})
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(prefixFile)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = true // PayloadID lives in the value, not the key
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			verr := it.Item().Value(func(val []byte) error {
				file, derr := decodeFile(val)
				if derr != nil {
					// Skip corrupt inode rows rather than fail-closed: a single
					// bad row must not block reclaiming everything else.
					return nil
				}
				if fileLinkCountTxn(txn, file) == 0 {
					return nil // unlinked: payload is dead, not live (#1433)
				}
				if pid := string(file.PayloadID); pid != "" {
					seen[pid] = struct{}{}
				}
				return nil
			})
			if verr != nil {
				return verr
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for payloadID := range seen {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumeratePayloads streams every distinct payloadID that has at least one
// FileChunk row through fn. It iterates the fb-file:{payloadID}:{blockIdx}
// secondary index, extracts the payloadID (the substring before the LAST ':')
// from each key, dedupes via a set, and calls fn once per distinct payloadID.
// Unlike the local store's ListFiles, this enumerates the authoritative
// metadata, so it still yields rolled-up payloads whose append log is gone.
func (s *BadgerMetadataStore) EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error {
	seen := make(map[string]struct{})
	err := s.db.View(func(txn *badger.Txn) error {
		prefix := []byte(fileChunkFilePrefix)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		opts.PrefetchValues = false // Keys only — payloadID lives in the key
		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Key form: fb-file:{payloadID}:{blockIdx}. payloadIDs do not
			// contain ':', but split on the LAST ':' to be safe since blockIdx
			// is the trailing numeric segment.
			key := string(it.Item().Key()[len(prefix):])
			i := strings.LastIndex(key, ":")
			if i < 0 {
				continue
			}
			payloadID := key[:i]
			if _, ok := seen[payloadID]; ok {
				continue
			}
			seen[payloadID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for payloadID := range seen {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(payloadID); err != nil {
			return err
		}
	}
	return nil
}

// EnumerateFileChunks streams every FileChunk's ContentHash through fn using
// a Badger prefix iterator over fb:. The iterator yields one row per block
// (no allocation of a full slice in application memory)..
// lifted from FileChunkStore to MetadataStore
// implementation unchanged.
func (s *BadgerMetadataStore) EnumerateFileChunks(ctx context.Context, fn func(blockpkg.ContentHash) error) error {
	return s.db.View(func(txn *badger.Txn) error {
		return enumerateFileChunksTxn(ctx, txn, fn)
	})
}

// listFileChunksTxn / enumerateFileChunksTxn iterate a given
// *badger.Txn so the store-level methods (over a db.View snapshot) and the
// transaction-level methods (over the active write txn, for read-after-write)
// share one implementation. Binding to the caller's txn is what lets a
// tx.Put be observed by a later tx.ListFileChunks in the same WithTransaction.
func listFileChunksTxn(txn *badger.Txn, payloadID string) []*metadata.FileChunk {
	var result []*metadata.FileChunk
	prefix := []byte(fileChunkFilePrefix + payloadID + ":")
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
		fbItem, err := txn.Get([]byte(fileChunkPrefix + blockID))
		if err != nil {
			continue // Index stale, block deleted
		}
		var block metadata.FileChunk
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
		return []*metadata.FileChunk{}
	}
	return result
}

// enumerateFileChunksTxn streams the GC mark live set: the UNION of the CAS
// index (fb: entries) and the per-file manifest (f: File.Blocks). Unioning both
// makes the live set a strict SUPERSET of both structures — the snapshot Backup
// HashSet is built from f: File.Blocks alone, so a hash present only there
// (manifest row without a fb: CAS row, or one already reaped) would otherwise be
// missed by the mark phase and the sweep would reap a still-live chunk once a
// snapshot hold lapsed (data loss). Duplicates across the two passes are
// harmless — GCState.Add deduplicates the live set.
func enumerateFileChunksTxn(ctx context.Context, txn *badger.Txn, fn func(blockpkg.ContentHash) error) error {
	// Each pass is scoped so its iterator is released before the next opens.
	if err := enumeratePrefixHashes(ctx, txn, fileChunkPrefix, func(val []byte) (blockpkg.ContentHash, error) {
		var block metadata.FileChunk
		if err := json.Unmarshal(val, &block); err != nil {
			return blockpkg.ContentHash{}, fmt.Errorf("decode file chunk: %w", err)
		}
		return block.Hash, nil
	}, fn); err != nil {
		return err
	}
	// Per-file manifest (f: File.Blocks). A file carries multiple hashes, so
	// this pass emits each block ref individually.
	return enumeratePrefixFileChunks(ctx, txn, fn)
}

// enumeratePrefixHashes iterates a key prefix, decoding one hash per entry via
// decodeHash and streaming it through fn. Used by the fb: (CAS index) pass.
func enumeratePrefixHashes(
	ctx context.Context,
	txn *badger.Txn,
	prefixStr string,
	decodeHash func([]byte) (blockpkg.ContentHash, error),
	fn func(blockpkg.ContentHash) error,
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
			return fmt.Errorf("enumerate file chunks: %w", err)
		}
		var h blockpkg.ContentHash
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

// enumeratePrefixFileChunks iterates the f: (File) prefix and streams every
// File.Blocks hash through fn. Decode failures are fatal (fail-closed): a
// dropped file entry would shrink the GC live set and let the sweep reap a
// still-live chunk.
func enumeratePrefixFileChunks(ctx context.Context, txn *badger.Txn, fn func(blockpkg.ContentHash) error) error {
	opts := badger.DefaultIteratorOptions
	opts.PrefetchValues = true
	opts.PrefetchSize = 256
	prefix := []byte(prefixFile)
	opts.Prefix = prefix
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("enumerate file chunks: %w", err)
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
			return fmt.Errorf("enumerate file chunks: decode file: %w", err)
		}
		// nlink=0 (unlinked) inodes keep their f: record but the file is dead.
		// Excluding their manifest blocks from the GC live set is what lets the
		// sweep reclaim orphaned chunks (#1433). Snapshot-held blocks are
		// protected independently by the GC HoldProvider, not by this manifest.
		// The authoritative link count lives in the l: key (#1166), not the
		// embedded File.Nlink (which SetLinkCount does not rewrite); read it the
		// same way GetFile overlays it, falling back to the embedded value when
		// the l: key is absent.
		if fileLinkCountTxn(txn, file) == 0 {
			continue
		}
		for _, br := range file.Blocks {
			if err := fn(br.Hash); err != nil {
				return err
			}
		}
	}
	return nil
}

// fileLinkCountTxn returns the authoritative link count for a file, reading the
// l: key (#1166) the same way GetFile overlays it. SetLinkCount writes the l:
// key, not the embedded File.Nlink, so the embedded value alone is unreliable;
// fall back to it only when the l: key is absent.
func fileLinkCountTxn(txn *badger.Txn, file *metadata.File) uint32 {
	item, err := txn.Get(keyLinkCount(file.ID))
	if err != nil {
		// No l: key yet: mirror GetFile's default-by-type so a freshly created
		// file (link count not persisted yet) is never treated as dead. The
		// embedded File.Nlink may be zero/stale and must NOT be trusted here.
		if file.Type == metadata.FileTypeDirectory {
			return 2
		}
		return 1
	}
	nlink := file.Nlink
	_ = item.Value(func(val []byte) error {
		if c, derr := decodeUint32(val); derr == nil {
			nlink = c
		}
		return nil
	})
	return nlink
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

// parseBlockIdx returns the numeric suffix of a block ID ("{payloadID}/{n}"), used as a sort key; 0 if absent.
func parseBlockIdx(id string) int {
	if _, idx, ok := splitBlockID(id); ok {
		if v, err := strconv.Atoi(idx); err == nil {
			return v
		}
	}
	return 0
}

// ============================================================================
// Transaction Support
// ============================================================================

// Ensure badgerTransaction implements FileChunkStore
var _ blockpkg.FileChunkStore = (*badgerTransaction)(nil)

// (review iteration 1): FileChunkStore methods on
// badgerTransaction MUST run against the txn's *badger.Txn so a
// rollback (returning an error from WithTransaction's fn) discards the
// RefCount mutation. Previously every method called `tx.store.X(...)`
// which opened its own db.Update — defeating rollback for any caller
// that bumped RefCount inside WithTransaction then encountered a
// downstream PutFile failure (silent leak).

func (tx *badgerTransaction) GetFileChunk(ctx context.Context, id string) (*metadata.FileChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return nil, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return nil, err
	}
	var block metadata.FileChunk
	if err := item.Value(func(val []byte) error {
		return json.Unmarshal(val, &block)
	}); err != nil {
		return nil, err
	}
	return &block, nil
}

func (tx *badgerTransaction) Put(ctx context.Context, block *metadata.FileChunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := []byte(fileChunkPrefix + block.ID)
	val, err := json.Marshal(block)
	if err != nil {
		return fmt.Errorf("marshal file chunk: %w", err)
	}
	if err := tx.txn.Set(key, val); err != nil {
		return err
	}
	if pid, idx, ok := splitBlockID(block.ID); ok {
		fileKey := []byte(fileChunkFilePrefix + pid + ":" + idx)
		if err := tx.txn.Set(fileKey, []byte(block.ID)); err != nil {
			return err
		}
	}
	if block.IsFinalized() {
		hashKey := []byte(fileChunkHashPrefix + block.Hash.String())
		return tx.txn.Set(hashKey, []byte(block.ID))
	}
	return nil
}

func (tx *badgerTransaction) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return err
	}
	var block metadata.FileChunk
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
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return err
	}
	var block metadata.FileChunk
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
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return 0, metadata.ErrFileChunkNotFound
	}
	if err != nil {
		return 0, err
	}
	var block metadata.FileChunk
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
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return 0, nil // tolerate already-swept row
	}
	if err != nil {
		return 0, err
	}
	var block metadata.FileChunk
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
func (tx *badgerTransaction) AddRef(ctx context.Context, hash metadata.ContentHash, _ string, _ blockpkg.ChunkRef) error {
	// payloadID + blockRef accepted for future GC traceability;
	// badger backend records ref count only — parameters intentionally
	// blanked.
	if err := ctx.Err(); err != nil {
		return err
	}
	hashKey := []byte(fileChunkHashPrefix + hash.String())
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
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return metadata.ErrUnknownHash
	}
	if err != nil {
		return err
	}
	var block metadata.FileChunk
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
func (tx *badgerTransaction) GetByHash(ctx context.Context, hash metadata.ContentHash) (*metadata.FileChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hashKey := []byte(fileChunkHashPrefix + hash.String())
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
	key := []byte(fileChunkPrefix + id)
	item, err := tx.txn.Get(key)
	if err == badger.ErrKeyNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var block metadata.FileChunk
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

// ListFileChunks, EnumerateFileChunks iterate the active txn
// (tx.txn) rather than opening a fresh db.View snapshot. Delegating to the
// store path took a snapshot at call time, so a tx.Put followed by
// tx.ListFileChunks in the same WithTransaction missed the uncommitted write —
// a cross-backend divergence vs memory, whose list helpers read live maps
// under the held lock. Binding to tx.txn gives read-after-write within the tx
// (BadgerDB transactions see their own pending writes).
func (tx *badgerTransaction) ListFileChunks(ctx context.Context, payloadID string) ([]*metadata.FileChunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return listFileChunksTxn(tx.txn, payloadID), nil
}

func (tx *badgerTransaction) EnumerateFileChunks(ctx context.Context, fn func(blockpkg.ContentHash) error) error {
	return enumerateFileChunksTxn(ctx, tx.txn, fn)
}
