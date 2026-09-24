package badger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dgraph-io/badger/v4"
	blockpkg "github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/internal/txretry"
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
	if block.IsRemote() {
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

// Put stores or updates a file chunk.
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
		if block.IsRemote() {
			hashKey := []byte(fileChunkHashPrefix + block.Hash.String())
			return txn.Set(hashKey, []byte(block.ID))
		}
		return nil
	})
}

// Delete removes a file chunk by its ID.
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
// conflict when the retry budget is spent; non-conflict errors
// short-circuit.
func (s *BadgerMetadataStore) updateWithConflictRetry(ctx context.Context, fn func(*badger.Txn) error) error {
	// Started at the first conflict, not here, so an attempt that outlasts the
	// budget on its own cannot leave that conflict with nothing to spend. See
	// withTransaction.
	var deadline time.Time

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
			// Record the SSI abort on the same counter WithTransaction uses,
			// so a workload's conflict rate is visible no matter which retry
			// loop it went through.
			s.txnConflicts.Add(1)
			// Same jittered exponential backoff and deadline bound as
			// WithTransaction.
			if deadline.IsZero() {
				deadline = txretry.Deadline(ctx)
			}
			if txretry.Backoff(ctx, deadline, attempt) {
				continue
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			break
		}
		return err
	}
	// Retries exhausted. Map the raw sentinel to StoreError{Code: ErrConflict}
	// the way WithTransaction does: IsConflictError and the object-ID conflict
	// rules match only the wrapped form, so returning the bare sentinel would
	// leave an exhausted conflict unclassified on this backend while the SQL
	// backends classify the same condition.
	return mapBadgerError(lastErr, "updateWithConflictRetry", "")
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
// Returns nil without error if not found.
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
		var lerr error
		result, lerr = listFileChunksTxn(txn, payloadID)
		return lerr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// loadFileChunkAtIndexOffset resolves the FileChunk whose secondary-index key is
// fb-file:{payloadID}:{off} via index key -> block ID -> primary row. It returns
// (nil, nil) when either row is missing (ErrKeyNotFound) — a stale index entry
// left by a deleted block, treated as absent, mirroring listFileChunksTxn's skip.
// Any other Get/Value/unmarshal error is real IO or corruption and propagates.
func loadFileChunkAtIndexOffset(txn *badger.Txn, payloadID string, off uint64) (*metadata.FileChunk, error) {
	idxItem, gerr := txn.Get([]byte(fileChunkFilePrefix + payloadID + ":" + strconv.FormatUint(off, 10)))
	if errors.Is(gerr, badger.ErrKeyNotFound) {
		return nil, nil
	} else if gerr != nil {
		return nil, gerr
	}
	var blockID string
	if verr := idxItem.Value(func(val []byte) error {
		blockID = string(val)
		return nil
	}); verr != nil {
		return nil, verr
	}
	return loadFileChunkByID(txn, blockID)
}

// loadFileChunkByID fetches the fb:{blockID} primary row. A row that is not
// there (ErrKeyNotFound) yields (nil, nil): the secondary index outlives the
// block it names, so a missing primary row reads as absent. Every other
// Get/Value/unmarshal failure propagates.
//
// decision: absence is the only failure this tolerates, and only because a
// deleted block legitimately leaves its index entry behind. An undecodable
// value is NOT absence — it is a row whose range is unknown, the class
// engine.DataExtents legislates for (read the invariant there; it is the
// authority and this is not a second copy of it). Dropping such a row hands the
// caller a manifest one row short with no error, which under-reports the hole
// map, and a hole is read back as zeros without consulting the block store.
// Widen this only if rows are ever deliberately written in an encoding this
// reader is not expected to understand; a merely older encoding does not
// qualify, because the decoder still reads those.
func loadFileChunkByID(txn *badger.Txn, blockID string) (*metadata.FileChunk, error) {
	fbItem, gerr := txn.Get([]byte(fileChunkPrefix + blockID))
	if errors.Is(gerr, badger.ErrKeyNotFound) {
		return nil, nil
	} else if gerr != nil {
		return nil, gerr
	}
	var fc metadata.FileChunk
	if verr := fbItem.Value(func(val []byte) error {
		return json.Unmarshal(val, &fc)
	}); verr != nil {
		return nil, verr
	}
	return &fc, nil
}

// scanChunkIndexOffsets walks the keys-only fb-file:{payloadID}: index and
// returns the offset best keeps, plus the first key suffix that does not parse
// as one. Such a suffix is a row ID's suffix verbatim, so the row sits at an
// unknown offset and its caller must refuse rather than report a hole the reader
// would zero-fill.
//
// A suffix above MaxInt64 counts as not parsing here, matching the ceiling
// block.ParseChunkOffset applies to the row's own ID. Were it kept instead, this
// scan would place a row the covering guard then refuses, and the offset would
// be reported as a hole rather than as the unreadable row it is.
func scanChunkIndexOffsets(txn *badger.Txn, payloadID string, keep func(cand, best uint64, have bool) bool) (bestOff uint64, found bool, unplaceable string) {
	prefix := []byte(fileChunkFilePrefix + payloadID + ":")
	opts := badger.DefaultIteratorOptions
	opts.Prefix = prefix
	opts.PrefetchValues = false // keys only — the offset lives in the key
	it := txn.NewIterator(opts)
	defer it.Close()

	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		suffix := string(it.Item().Key()[len(prefix):])
		cand, perr := strconv.ParseUint(suffix, 10, 64)
		if perr != nil || cand > math.MaxInt64 {
			if unplaceable == "" {
				unplaceable = suffix
			}
			continue
		}
		if keep(cand, bestOff, found) {
			bestOff, found = cand, true
		}
	}
	return bestOff, found, unplaceable
}

// errUnplaceableRow reports that off cannot be resolved because the manifest
// holds a row whose range is unknown.
func errUnplaceableRow(payloadID, suffix string, off uint64) error {
	return fmt.Errorf("%w: cannot resolve offset %d for payload %q: manifest holds unplaceable row %q",
		blockpkg.ErrManifestInconsistent, off, payloadID, payloadID+"/"+suffix)
}

// GetFileChunkAtOffset returns the FileChunk covering absolute byte offset off
// for payloadID — the row with the largest chunkOffset <= off whose range
// [chunkOffset, chunkOffset+DataSize) contains off — or (nil, nil) for a sparse
// hole, an empty payload, or a read past EOF. This is the read hot path: a
// keys-only scan of the fb-file:{payloadID}: secondary index finds the covering
// offset without materializing the whole manifest (no per-row Get, no JSON
// unmarshal, no sort), then two point Gets fetch the winning row.
//
// Largest-start only decides anything if rows overlap, which a truncate followed
// by a re-carving write can produce: the narrowed survivor starts before the row
// the later carve emits, so the greater start is the newer row there. The
// engine's ListFileChunks fallback picks the same row, so both paths answer a
// read alike.
//
// Answering alike is why candidates are tried in descending start order rather
// than only the largest one. The largest start at or below off need not reach
// off at all — a row nested inside a straddler ends before it while the
// straddler still holds those bytes — and stopping at that row would report a
// hole the reader zero-fills over live data, where the walk finds the straddler.
// A start whose row the index no longer resolves is passed over for the same
// reason: it is the index that is stale, not the manifest that is empty there.
//
// An unplaceable row only matters when nothing else covers off: one bad row must
// not make a whole payload unreadable. This mirrors block.FindRowCoveringOffset, the
// walk used by the backends with no such index.
//
// ponytail: O(n) keys-only scan per candidate, and only an overlap yields more
// than one candidate; upgrade to a big-endian fb-off index for a true O(log n)
// reverse-seek only if profiling at real N still shows it.
func (s *BadgerMetadataStore) GetFileChunkAtOffset(_ context.Context, payloadID string, off uint64) (*metadata.FileChunk, error) {
	var result *metadata.FileChunk
	err := s.db.View(func(txn *badger.Txn) error {
		for limit := off; ; {
			bestOff, found, unplaceable := scanChunkIndexOffsets(txn, payloadID,
				func(cand, best uint64, have bool) bool {
					return cand <= limit && (!have || cand > best)
				})
			// A hole, unless an unplaceable row leaves it in doubt.
			uncovered := func() error {
				if unplaceable == "" {
					return nil
				}
				return errUnplaceableRow(payloadID, unplaceable, off)
			}
			if !found {
				return uncovered()
			}

			fc, ferr := loadFileChunkAtIndexOffset(txn, payloadID, bestOff)
			if ferr != nil {
				return ferr
			}
			// Covering guard keyed on the row's OWN start offset (the source of
			// truth), not the index key, so an inconsistent index can't serve a
			// neighbour chunk's bytes into a hole. off-abs is overflow-free since
			// the scan guarantees abs <= off.
			if fc != nil {
				abs, ok := blockpkg.ParseChunkOffset(fc.ID)
				if ok && off >= abs && off-abs < uint64(fc.DataSize) {
					result = fc
					return nil
				}
			}
			if bestOff == 0 {
				return uncovered() // no earlier start left to try
			}
			limit = bestOff - 1
		}
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetFileChunkAtOrAfterOffset returns the FileChunk with the smallest
// chunkOffset >= off for payloadID — the next data boundary at or after a sparse
// hole — or (nil, nil) when no chunk starts at or after off (a read past the last
// chunk). The read path uses it to skip a hole straight to the next chunk in one
// indexed keys-only scan instead of probing byte by byte. Same index, cost model,
// and stale-index-as-absent semantics as GetFileChunkAtOffset. No covering guard:
// the successor is returned regardless of whether it contains off.
//
// An unplaceable row is fatal here whatever else the scan finds, unlike in
// GetFileChunkAtOffset: sitting at an unknown offset, it may be the true
// successor, and returning a later one would reclassify the bytes it holds as
// hole for the caller to zero-fill.
//
// ponytail: O(n) keys-only scan per hole; shares the fb-off-index upgrade path
// with GetFileChunkAtOffset if profiling at real N ever demands O(log n).
func (s *BadgerMetadataStore) GetFileChunkAtOrAfterOffset(_ context.Context, payloadID string, off uint64) (*metadata.FileChunk, error) {
	var result *metadata.FileChunk
	err := s.db.View(func(txn *badger.Txn) error {
		bestOff, found, unplaceable := scanChunkIndexOffsets(txn, payloadID,
			func(cand, best uint64, have bool) bool {
				return cand >= off && (!have || cand < best)
			})
		if unplaceable != "" {
			return errUnplaceableRow(payloadID, unplaceable, off)
		}
		if !found {
			return nil // nothing starts at or after off — past the last chunk
		}
		fc, ferr := loadFileChunkAtIndexOffset(txn, payloadID, bestOff)
		if ferr != nil {
			return ferr
		}
		result = fc // may be nil for a stale index entry — caller treats as end
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// EnumerateLivePayloadIDs streams every distinct PayloadID referenced by a live
// inode. It scans the f: inode keyspace, decodes each file record, and collects
// distinct non-empty PayloadIDs. Hardlinks share one f: key, so DISTINCT yields
// one payloadID per content. nlink=0 (unlinked) inodes are excluded (#1433):
// their payload is dead, so the reconcile treats it as stranded.
//
// The scan is best-effort by design: an undecodable inode row is skipped so a
// single bad row cannot block reclaiming everything else. It is then counted,
// and a non-zero count makes this return ErrLiveSetIncomplete AFTER streaming
// every payload it did determine. The set is still delivered; what the error
// says is that it is a SUBSET of the live set, so absence from it is not
// evidence a payload is dead.
//
// decision: fail-open on the scan, fail-closed at the caller. Skipping is right
// for a reclaim — the alternative is one corrupt inode pinning a whole store's
// garbage forever — and wrong for a diff, because reapStrandedRows deletes
// exactly what the set omits. Splitting it this way keeps both: the skip stays,
// and the error is what stops it from authorizing a deletion. Reconsider only
// if a caller appears that must reap from a partial set; it would have to state
// why deleting a live payload is acceptable there.
//
// This is the opposite posture from the GC MARK pass (enumerateFileChunksTxn
// below), which aborts on the first undecodable row. Both are fail-closed where
// it counts and they differ only in where "it counts" falls: the mark pass
// builds the set that PROTECTS chunks, so a row it drops gets swept — it must
// not produce a partial set at all. This scan builds a set that AUTHORIZES
// deletion, so a partial set is safe to hand to a reader and unsafe to hand to
// the reaper, which is the distinction the error draws.
func (s *BadgerMetadataStore) EnumerateLivePayloadIDs(ctx context.Context, fn func(payloadID string) error) error {
	seen := make(map[string]struct{})
	skipped := 0
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
					// Skipped, not ignored: the count below turns the gap into
					// ErrLiveSetIncomplete once the scan finishes.
					skipped++
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
	if skipped > 0 {
		return fmt.Errorf("%w: %d undecodable inode rows skipped", metadata.ErrLiveSetIncomplete, skipped)
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
// (no allocation of a full slice in application memory).
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
//
// listFileChunksTxn returns the whole manifest or an error: it never returns a
// short list. A row the index names but that no longer exists is the one
// tolerated gap (see loadFileChunkByID) — anything else, including a value that
// will not decode, fails the call the way GetFileChunk already fails it.
func listFileChunksTxn(txn *badger.Txn, payloadID string) ([]*metadata.FileChunk, error) {
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
			return nil, err
		}
		block, err := loadFileChunkByID(txn, blockID)
		if err != nil {
			return nil, err
		}
		if block == nil {
			continue // Index stale, block deleted
		}
		result = append(result, block)
	}
	// Keys are lexicographically sorted (fb-file:{payloadID}:0, :1, :10, :2...)
	// which gives wrong numeric order for multi-digit indices. Sort by parsed index.
	sort.Slice(result, func(i, j int) bool {
		return parseBlockIdx(result[i].ID) < parseBlockIdx(result[j].ID)
	})
	if result == nil {
		return []*metadata.FileChunk{}, nil
	}
	return result, nil
}

// enumerateFileChunksTxn streams the GC mark live set. The set it builds is what
// PROTECTS chunks from the sweep, so a row missing from it is a reaped live
// chunk: an undecodable f: row or manifest aborts the walk rather than being
// skipped, and a link count that cannot be read resolves to alive (see
// defaultLinkCount) rather than to the nlink=0 that would drop the file.
//
// Do not read that as the posture of every scan over the same keyspace.
// EnumerateLivePayloadIDs above skips an undecodable inode row on purpose, and
// is fail-closed a layer up instead: it counts the skips and returns
// ErrLiveSetIncomplete so its one destructive consumer refuses, while a
// read-only consumer still gets the partial set. The two differ because the
// meaning of a missing row is opposite — here it costs a live chunk, there it
// costs a reclaim that can be retried.
//
// The set itself is the UNION of the CAS
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
		// The manifest lives in fm:<uuid> (legacy blobs embed it); the GC live
		// set is fail-closed, so a missed load would let the sweep reap a live
		// chunk. loadManifest errors must abort the walk, not skip the file.
		if err := loadManifest(txn, file); err != nil {
			return fmt.Errorf("enumerate file chunks: load manifest: %w", err)
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
		return defaultLinkCount(file)
	}
	var (
		nlink uint32
		read  bool
	)
	_ = item.Value(func(val []byte) error {
		if c, derr := decodeUint32(val); derr == nil {
			nlink, read = c, true
		}
		return nil
	})
	if !read {
		// An l: key that will not decode says nothing about the link count, so
		// it is treated exactly like one that is not there yet. The embedded
		// File.Nlink is NOT the fallback: encodeFile never writes it, so it
		// decodes as 0 — and 0 is the one value that means "dead" to every
		// caller below.
		return defaultLinkCount(file)
	}
	return nlink
}

// defaultLinkCount is the link count assumed when the l: key cannot be read —
// absent, or present and undecodable. It mirrors GetFile's default-by-type.
//
// decision: an unreadable link count resolves to ALIVE, never to dead, in all
// three consumers of this value. Dead is the terminal answer everywhere it is
// used: EnumerateLivePayloadIDs drops the payload from the live set and the
// stranded-row reaper deletes its rows; the GC mark pass drops its chunks and
// the sweep reclaims them; snapshot Backup omits its hashes from the durability
// claim. Alive only costs a reclaim that a later pass can still make, once the
// row is repaired. The cost of this rule is that a payload behind a corrupt l:
// key is never reclaimed; overturn it only if something appears that must
// distinguish dead from unreadable, and it would have to read the link count
// from somewhere this function cannot.
func defaultLinkCount(file *metadata.File) uint32 {
	if file.Type == metadata.FileTypeDirectory {
		return 2
	}
	return 1
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

// FileChunkStore methods on badgerTransaction MUST run against the txn's
// *badger.Txn so a rollback (returning an error from WithTransaction's fn)
// discards the RefCount mutation. Calling tx.store.X(...) instead would open
// its own db.Update and defeat rollback for any caller that bumps RefCount
// inside WithTransaction and then hits a downstream UpdateAttrs failure.

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
	if block.IsRemote() {
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
	return listFileChunksTxn(tx.txn, payloadID)
}

func (tx *badgerTransaction) EnumerateFileChunks(ctx context.Context, fn func(blockpkg.ContentHash) error) error {
	return enumerateFileChunksTxn(ctx, tx.txn, fn)
}

// DecrementRefCountAndReapMany reaps every id under the active badger.Txn, so a
// rollback discards the whole set. Badger is embedded, so the batch is the
// per-id sequence.
func (tx *badgerTransaction) DecrementRefCountAndReapMany(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if _, err := tx.DecrementRefCountAndReap(ctx, id); err != nil {
			return err
		}
	}
	return nil
}
