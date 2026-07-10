package engine

import (
	"context"
	"errors"
	"fmt"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// dataplaneMetrics returns the engine's data-plane metrics sink, or nil when no
// recorder was injected. Call sites must guard the result: it is a plain
// interface, not a nil-safe *Metrics.  Mirrors the Syncer's dataplaneMetrics().
func (bs *Store) dataplaneMetrics() DataplaneMetrics {
	if p := bs.metrics.Load(); p != nil {
		return *p
	}
	return nil
}

// blockRefHashes extracts the ContentHash slice from a ChunkRef list
// for OnRead's hint API.
func blockRefHashes(refs []block.ChunkRef) []block.ContentHash {
	out := make([]block.ContentHash, len(refs))
	for i, r := range refs {
		out[i] = r.Hash
	}
	return out
}

// readAtInternal reads from the primary payloadID. Always goes through
// the local store (with remote-fallback on miss); the cache is hint-only
// and does not serve bytes here.
//
// The primary entry is LocalStore.ReadPayloadAt — a payload-keyed read
// that consults BOTH the in-flight append log (pre-rollup bytes) AND
// the rolled-up CAS chunks via the FileChunk manifest. This closes the
// pre-rollup read-after-write window where freshly-appended bytes would
// otherwise return zeros until the async rollup commits FileChunk rows.
//
// On a local miss (ErrFileChunkNotFound), fall back to the CAS-hash
// walk (readLocalByHash, used for chunks that the manifest knows about
// but the LocalStore did not surface — e.g., post-eviction reads where
// only the metadata row survived) and finally to remote-fetch via the
// syncer.
func (bs *Store) readAtInternal(ctx context.Context, payloadID string, data []byte, offset uint64) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	// Primary: payload-keyed local read. Covers both the pre-rollup
	// append-log window and the post-rollup CAS path.
	n, err := bs.local.ReadPayloadAt(ctx, payloadID, data, offset)
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, block.ErrFileChunkNotFound) {
		return 0, fmt.Errorf("local read failed: %w", err)
	}

	// Local miss — try the CAS-hash walk (handles edge cases where the
	// FileChunkStore manifest is reachable via the engine's
	// fileChunkStore field but not the LocalStore-internal one).
	found, err := bs.readLocalByHash(ctx, payloadID, data, offset)
	if err != nil {
		return 0, fmt.Errorf("local read failed: %w", err)
	}
	if found {
		return len(data), nil
	}

	if err := bs.ensureAndReadFromLocal(ctx, payloadID, data, offset); err != nil {
		return 0, err
	}

	return len(data), nil
}

// ensureAndReadFromLocal downloads blocks from remote if needed and reads from local store.
func (bs *Store) ensureAndReadFromLocal(ctx context.Context, payloadID string, dest []byte, offset uint64) error {
	length := uint32(len(dest))

	// Fast path: direct-serve copies S3 data directly to dest, skipping a second read.
	filled, err := bs.syncer.EnsureAvailableAndRead(ctx, payloadID, offset, length, dest)
	if err != nil {
		return fmt.Errorf("direct download failed: %w", err)
	}
	if filled {
		return nil
	}

	found, err := bs.readLocalByHash(ctx, payloadID, dest, offset)
	if err != nil {
		return fmt.Errorf("read after download failed: %w", err)
	}
	if !found {
		clear(dest)
		logger.Debug("Sparse block: miss after download, returning zeros",
			"payloadID", payloadID)
	}

	return nil
}

// readLocalByHash serves [offset, offset+len(dest)) by walking the
// payload's CAS chunk manifest (via FileChunkStore.ListFileChunks)
// finding each chunk whose absolute byte range intersects the
// requested window, and copying the matching slice of the local CAS
// chunk into dest. Returns (true, nil) when every requested byte was
// satisfied locally and (false, nil) when any portion of the window
// could not be served from local CAS — the caller treats the false
// outcome as "must fall back to remote-fetch".
//
// On any unexpected error (FileChunk store failure, local chunk store
// I/O error other than ErrChunkNotFound) the function returns
// (false, err) so the engine can surface it to the protocol layer.
//
// Chunk geometry: under the unified CAS surface chunk boundaries are
// FastCDC-derived (variable size, absolute Offset stored on the
// FileChunk row's ID-derived blockIdx slot). The walk is O(N) over
// the per-payload row list — acceptable for the test fixtures (small
// N) and for the steady-state production stream where N is bounded
// by the payload's total size divided by the average chunk size
// (~4 MiB).
func (bs *Store) readLocalByHash(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	if len(dest) == 0 {
		return true, nil
	}
	// The engine consults the same EngineFileChunkStore the syncer
	// uses. resolveCovering resolves the single chunk covering each
	// offset via the badger index (falling back to a ListFileChunks
	// walk for other backends); a nil result is sparse and the caller
	// falls back to the remote-fetch + zero-fill path.
	if bs.fileChunkStore == nil {
		return false, nil
	}
	endOff := offset + uint64(len(dest))
	for currentOffset := offset; currentOffset < endOff; {
		fb, absOffset, err := resolveCovering(ctx, bs.fileChunkStore, payloadID, currentOffset)
		if err != nil {
			return false, err
		}
		if fb == nil || fb.Hash.IsZero() {
			return false, nil
		}
		data, err := bs.local.Get(ctx, fb.Hash)
		if err != nil {
			if errors.Is(err, block.ErrChunkNotFound) {
				return false, nil
			}
			return false, err
		}
		// Integrity check: verify the buffer we already have against the
		// chunk's content hash.  No double-read — we verify in-place.
		computed := block.ContentHash(blake3.Sum256(data))
		if computed != fb.Hash {
			var healErr error
			data, healErr = bs.healLocalChunk(ctx, fb.Hash, fb)
			if healErr != nil {
				return false, healErr
			}
		}
		// Clamp the visible data to FileChunk.DataSize so a padded
		// on-disk chunk surface doesn't leak garbage past the
		// rollup-emitted byte count.
		dataLen := uint64(len(data))
		if uint64(fb.DataSize) > 0 && uint64(fb.DataSize) < dataLen {
			dataLen = uint64(fb.DataSize)
		}
		chunkAbsEnd := absOffset + dataLen
		if currentOffset >= chunkAbsEnd {
			// Should not happen if resolveCovering returned a row
			// covering currentOffset — surface as sparse and let
			// the caller fall back.
			return false, nil
		}
		srcOff := currentOffset - absOffset
		copyLen := chunkAbsEnd - currentOffset
		if copyLen > endOff-currentOffset {
			copyLen = endOff - currentOffset
		}
		copy(dest[currentOffset-offset:currentOffset-offset+copyLen], data[srcOff:srcOff+copyLen])
		currentOffset += copyLen
	}
	return true, nil
}

// rowWithOffset bundles a FileChunk row with the absolute payload
// offset of its first byte. The persister encodes the chunk's
// absolute offset directly as the numeric component of the row ID
// ("<payloadID>/<chunkOffset>"), so absOffset is the parsed
// component verbatim.
type rowWithOffset struct {
	fb        *block.FileChunk
	absOffset uint64
}

// findRowCoveringOffset returns the row whose absolute byte range
// [absOffset, absOffset+DataSize) contains target, or nil if no row
// in rows covers it. The walk is O(N) over the per-payload row
// list — acceptable for the FastCDC steady-state (chunks average ~4 MiB
// so even a 4 GiB file produces ~1000 rows).
func findRowCoveringOffset(rows []*block.FileChunk, target uint64) *rowWithOffset {
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		if target >= abs && target < abs+uint64(fb.DataSize) {
			return &rowWithOffset{fb: fb, absOffset: abs}
		}
	}
	return nil
}

// chunkAtOffsetResolver is the indexed covering-chunk lookup, implemented only
// by the badger metadata backend. resolveCovering type-asserts for it and falls
// back to a ListFileChunks walk otherwise.
type chunkAtOffsetResolver interface {
	GetFileChunkAtOffset(ctx context.Context, payloadID string, off uint64) (*block.FileChunk, error)
}

// resolveCovering returns the FileChunk covering absolute byte offset off and
// its parsed absolute start offset, or (nil, 0, nil) for a hole. When the store
// implements chunkAtOffsetResolver (badger) it uses the indexed single-chunk
// lookup that avoids enumerating the whole per-payload manifest; otherwise it
// falls back to ListFileChunks + findRowCoveringOffset (memory/sqlite/postgres —
// not the profiled hot path).
func resolveCovering(ctx context.Context, store block.EngineFileChunkStore, payloadID string, off uint64) (*block.FileChunk, uint64, error) {
	if store == nil {
		return nil, 0, nil
	}
	if r, ok := store.(chunkAtOffsetResolver); ok {
		fb, err := r.GetFileChunkAtOffset(ctx, payloadID, off)
		if err != nil || fb == nil {
			return nil, 0, err
		}
		abs, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			return nil, 0, fmt.Errorf("resolveCovering: malformed FileChunk ID %q", fb.ID)
		}
		return fb, abs, nil
	}
	rows, err := store.ListFileChunks(ctx, payloadID)
	if err != nil {
		if errors.Is(err, block.ErrFileChunkNotFound) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	rw := findRowCoveringOffset(rows, off)
	if rw == nil {
		return nil, 0, nil
	}
	return rw.fb, rw.absOffset, nil
}

// healLocalChunk attempts to repair a locally corrupt chunk by re-fetching it
// from the remote store.  It records corruption + outcome metrics and returns
// the healed bytes (or nil + error on unrecoverable failure).
//
// Decision tree:
//  1. Record local corruption unconditionally.
//  2. If no SyncedHashStore is wired, or IsSynced returns false/error:
//     RecordSelfHealFailure(1) + return (nil, ErrChunkContentMismatch).
//  3. Delete the corrupt local entry (removes the bad hash pointer).
//  4. Fetch from remote via dispatchRemoteFetch (already blake3-verified).
//     If fetch fails or returns nil data: RecordSelfHealFailure(1) + (nil, ErrChunkContentMismatch).
//  5. Try to re-stage locally via local.Put.
//     - Put succeeds: markFetchedSynced + RecordSelfHealSuccess(1) + return data.
//     - Put fails (disk full etc.): RecordSelfHealFailure(1) + return data (serve degraded).
func (bs *Store) healLocalChunk(ctx context.Context, hash block.ContentHash, fb *block.FileChunk) ([]byte, error) {
	dm := bs.dataplaneMetrics()
	if dm != nil {
		dm.RecordLocalCorruption(1)
	}

	// Only synced chunks have a retrievable remote copy.
	shs := bs.syncedHashStore
	if shs == nil {
		logger.Debug("self-heal: no SyncedHashStore wired, cannot heal chunk", "hash", hash)
		if dm != nil {
			dm.RecordSelfHealFailure(1)
		}
		return nil, block.ErrChunkContentMismatch
	}
	synced, err := shs.IsSynced(ctx, hash)
	if err != nil || !synced {
		logger.Debug("self-heal: chunk not synced, cannot heal", "hash", hash)
		if dm != nil {
			dm.RecordSelfHealFailure(1)
		}
		return nil, block.ErrChunkContentMismatch
	}

	// Drop the corrupt pointer so a subsequent Put can land cleanly.
	if delErr := bs.local.Delete(ctx, hash); delErr != nil {
		logger.Debug("self-heal: delete corrupt chunk failed", "hash", hash, "error", delErr)
		// Non-fatal: proceed with the remote fetch regardless.
	}

	// Re-fetch from remote (dispatchRemoteFetch always blake3-verifies).
	_, data, fetchErr := bs.syncer.dispatchRemoteFetch(ctx, fb)
	if fetchErr != nil || data == nil {
		logger.Debug("self-heal: remote fetch failed", "hash", hash, "error", fetchErr)
		if dm != nil {
			dm.RecordSelfHealFailure(1)
		}
		return nil, block.ErrChunkContentMismatch
	}

	// Try to re-stage locally for future reads.
	if putErr := bs.local.Put(ctx, hash, data); putErr != nil {
		// Degraded: serve correct bytes but don't count as a full success.
		logger.Debug("self-heal: local re-stage failed (degraded), serving bytes",
			"hash", hash, "error", putErr)
		if dm != nil {
			dm.RecordSelfHealFailure(1)
		}
		return data, nil
	}

	// Full success: re-staged + synced marker updated.
	bs.syncer.markFetchedSynced(ctx, hash)
	if dm != nil {
		dm.RecordSelfHealSuccess(1)
	}
	logger.Debug("self-heal: chunk repaired successfully", "hash", hash)
	return data, nil
}
