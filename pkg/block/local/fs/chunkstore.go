package fs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
)

// StoreChunk appends data to the log-blob substrate and records its location
// in the local chunk index.
//
// Idempotent: if the chunk already exists (HasChunk returns true for h)
// StoreChunk is a no-op and returns nil. This is what lets the rollup pool
// retry safely after a crash between StoreChunk and CommitChunks.
//
// Caller is responsible for asserting that BLAKE3(data) == h before calling;
// StoreChunk trusts its inputs (threat accept). The rollup pool
// is the only production caller.
func (bc *FSStore) StoreChunk(ctx context.Context, h block.ContentHash, data []byte) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := bc.HasChunk(ctx, h)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return bc.storeChunkLogBlob(ctx, h, data)
}

// storeChunkLogBlob appends data to the log-blob substrate and records the
// resulting location in the local chunk index. The idempotency / existence
// check already ran in StoreChunk, so this only handles a fresh chunk.
//
// A zero-length chunk is NOT appended (the substrate rejects empty payloads);
// instead a zero-RawLength location is recorded, which ReadChunk reconstructs
// as an empty slice. Per-chunk eviction does not apply on this path — the LRU
// tracks only cas/<hash> files; blob-level eviction is handled separately.
func (bc *FSStore) storeChunkLogBlob(ctx context.Context, h block.ContentHash, data []byte) error {
	var loc block.LocalChunkLocation
	if len(data) > 0 {
		var err error
		loc, err = bc.logBlob.Append(ctx, data)
		if err != nil {
			return fmt.Errorf("chunkstore: logblob append: %w", err)
		}
		// NOTE: if PutLocalLocation fails after a successful Append, the bytes
		// written to the blob at loc are orphaned — they have no index entry and
		// are not reachable by content hash. There is no dedicated reclaim pass
		// for such indexless extents: compaction rewrites per-payload append
		// logs, not the log-blob substrate, and eviction operates on whole
		// sealed blobs. The orphaned bytes are therefore space-only waste that
		// is reclaimed only when the entire sealed blob they live in is
		// eventually evicted. Only in this specific failure path — where no
		// index entry was written — does a subsequent StoreChunk retry
		// re-append: HasChunk consults the index and, once an entry exists,
		// dedups.
		//
		// Crash-durability differs by writer. The rollup writer
		// (stageRollupChunk/commitStagedChunk) FENCES this window: the index
		// entry is committed only after the blob is fsynced in Phase C, so a
		// crash before the fsync leaves no index entry and the bytes are
		// re-appended cleanly from the append-log the rollup drains. This
		// direct path (read-through staging via FSStore.Put) does NOT fsync
		// before indexing, so a crash in the append-then-index window can
		// leave a durable index entry pointing at un-fsynced blob bytes. That
		// is tolerated because Put only stages chunks already durable on the
		// remote: on the next read ReadChunk detects the torn tail (short
		// log-blob read), drops the dangling index entry, and reports a miss,
		// routing the engine to refetch the authoritative bytes from the
		// remote and re-stage. Neither writer can lose or hard-error an
		// acknowledged/remote-durable chunk.
	}
	if err := bc.localChunkIndex.PutLocalLocation(ctx, h, loc); err != nil {
		return fmt.Errorf("chunkstore: put local location: %w", err)
	}
	// logBlobDiskUsed is incremented only after a successful PutLocalLocation so
	// the counter reflects indexed (reachable) bytes, not bytes that may have
	// been orphaned by a failed index write above.
	if len(data) > 0 {
		bc.logBlobDiskUsed.Add(int64(len(data)))
		bc.trackBlobChunk(loc.LogBlobID, h)
	}
	// Fire the chunk-completion callback (engine Cache.Put). The path argument
	// is empty: logblob chunks have no per-chunk on-disk path, and the engine
	// discards it.
	if cb := bc.onChunkComplete.Load(); cb != nil && cb.fn != nil {
		cb.fn(h, data, "")
	}
	return nil
}

// stagedChunk is a chunk appended to the log-blob during a rollup pass whose
// durable local-index write has been DEFERRED to Phase C. It carries just
// enough to commit the index entry (and the disk-usage delta) once the blob
// has been fsynced.
type stagedChunk struct {
	h      block.ContentHash
	loc    block.LocalChunkLocation
	size   int
	staged bool
}

// stageRollupChunk stores chunk h for a rollup pass. On the log-blob path it
// APPENDS the bytes to the blob substrate but DEFERS the durable local-index
// write (PutLocalLocation) to commitStagedChunk, called in Phase C only after
// logBlob.Sync succeeds. Fencing the index write behind the blob fsync upholds
// the durability invariant: no durable index entry may point at bytes that are
// not yet durable on disk. A crash in the append→fsync window therefore leaves
// NO index entry, so the replay's HasChunk misses and the bytes are re-appended
// cleanly (they survive in the append-log the rollup drains from).
//
// In-window reads are unaffected: until Phase C advances the rollup fence the
// bytes are still served from the append-log (ReadPayloadAt step 1); the
// log-blob index only becomes the resolving source after the fence advances,
// which happens strictly after the fsync + index commit.
//
// Returns staged=true with the location to commit for a freshly appended
// log-blob chunk; staged=false when nothing needs a deferred index write — a
// dedup hit against an already-durable chunk.
func (bc *FSStore) stageRollupChunk(ctx context.Context, h block.ContentHash, data []byte) (stagedChunk, error) {
	// Dedup against chunks already made durable by a prior pass. (Intra-pass
	// duplicate hashes are deduped by the caller's per-pass set, since staged
	// chunks are not yet in the index.)
	exists, err := bc.HasChunk(ctx, h)
	if err != nil {
		return stagedChunk{}, err
	}
	if exists {
		return stagedChunk{}, nil
	}

	// Append the bytes now but hold the index write until Phase C's fsync.
	var loc block.LocalChunkLocation
	if len(data) > 0 {
		loc, err = bc.logBlob.Append(ctx, data)
		if err != nil {
			return stagedChunk{}, fmt.Errorf("chunkstore: logblob append: %w", err)
		}
	}
	// Populate the engine read cache eagerly (content-addressed, so correct even
	// if this pass is later abandoned). This mirrors storeChunkLogBlob's callback
	// timing and keeps freshly rolled-up chunks warm.
	if cb := bc.onChunkComplete.Load(); cb != nil && cb.fn != nil {
		cb.fn(h, data, "")
	}
	return stagedChunk{h: h, loc: loc, size: len(data), staged: true}, nil
}

// commitStagedChunk makes a staged log-blob chunk's local-index entry durable.
// It MUST be called only after logBlob.Sync has succeeded for the pass, so the
// index never references un-fsynced blob bytes.
func (bc *FSStore) commitStagedChunk(ctx context.Context, sc stagedChunk) error {
	if err := bc.localChunkIndex.PutLocalLocation(ctx, sc.h, sc.loc); err != nil {
		return fmt.Errorf("chunkstore: put local location: %w", err)
	}
	if sc.size > 0 {
		bc.logBlobDiskUsed.Add(int64(sc.size))
		bc.trackBlobChunk(sc.loc.LogBlobID, sc.h)
	}
	return nil
}

// ReadChunk returns the bytes of the chunk addressed by h.
// Returns block.ErrChunkNotFound if the chunk is absent.
//
// The local chunk index is consulted for the chunk's log-blob location; an
// index miss is a clean absent (block.ErrChunkNotFound).
func (bc *FSStore) ReadChunk(ctx context.Context, h block.ContentHash) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("chunkstore: get local location: %w", err)
	}
	if !ok {
		return nil, block.ErrChunkNotFound
	}
	if loc.RawLength == 0 {
		return []byte{}, nil
	}
	dst := make([]byte, loc.RawLength)
	n, rerr := bc.logBlob.ReadAt(ctx, loc, dst)
	if rerr != nil {
		// A short read (io.EOF before RawLength bytes) means the blob
		// tail was torn by a crash in the append→fsync window: the
		// durable index entry now points past the bytes that actually
		// reached disk. Rather than hard-error, drop the dangling index
		// entry and report the chunk as absent so the engine's
		// miss→remote-refetch→re-stage path recovers it from the
		// authoritative remote copy (torn ≡ evicted: both leave no
		// durable local bytes and no index entry). Any other error is a
		// genuine I/O failure and surfaces unchanged.
		if errors.Is(rerr, io.EOF) {
			return nil, bc.dropTornIndexEntry(ctx, h)
		}
		// An evicted or missing blob means the index entry is a
		// dangling leftover of blob-level eviction (entries for blobs
		// written by a previous process are cleaned up lazily here,
		// and eager cleanup in blobEvictOne is best-effort). Same
		// recovery routing as the torn tail: drop the entry and
		// report a miss so the engine refetches from the remote.
		if errors.Is(rerr, logblob.ErrEvicted) || errors.Is(rerr, logblob.ErrBlobNotFound) {
			return nil, bc.dropTornIndexEntry(ctx, h)
		}
		return nil, fmt.Errorf("chunkstore: logblob read: %w", rerr)
	}
	if int64(n) < loc.RawLength {
		// Defensive: nil error but fewer bytes than the index recorded
		// (a torn tail landing exactly at EOF). Same recovery routing.
		return nil, bc.dropTornIndexEntry(ctx, h)
	}
	return dst[:n], nil
}

// readChunkRangeInto serves [subOffset, subOffset+len(dst)) of CAS chunk h
// directly into dst via a ranged pread, avoiding the whole-chunk allocation and
// read that ReadChunk performs. It is a best-effort fast path for the warm read
// path: it returns served=true only when every requested byte was delivered from
// the local logblob tier. On any miss — chunk not in the local index (legacy
// .blk tier or evicted), torn tail, or short read — it returns (false, nil) so
// the caller falls back to the whole-chunk ReadChunk/Get path, which owns the
// miss -> remote-refetch -> re-stage recovery (and drops any torn index entry).
//
// It performs NO integrity check: the ranged bytes cannot be BLAKE3-verified in
// isolation, so callers MUST only use it for chunks already known-good (present
// in the verified set), whose whole body was verified on an earlier read.
func (bc *FSStore) readChunkRangeInto(ctx context.Context, h block.ContentHash, dst []byte, subOffset int64) (bool, error) {
	if bc.isClosed() {
		return false, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(dst) == 0 {
		return false, nil
	}
	loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil {
		return false, fmt.Errorf("chunkstore: get local location: %w", err)
	}
	if !ok || subOffset < 0 || subOffset+int64(len(dst)) > loc.RawLength {
		return false, nil
	}
	n, rerr := bc.logBlob.ReadAtRange(ctx, loc, dst, subOffset)
	if rerr != nil || n < len(dst) {
		// Torn/evicted/missing/short: fall back to the whole-chunk path rather
		// than error — ReadChunk hits the same condition and drops the dangling
		// index entry, routing the read to the remote refetch.
		return false, nil
	}
	return true, nil
}

// dropTornIndexEntry removes the dangling local-index entry for a chunk whose
// log-blob bytes were lost to a torn tail, then reports the chunk as absent.
// After this the chunk reads as a clean miss (HasChunk consults the index),
// which routes the engine to refetch from the durable remote copy and re-stage
// — the read-through equivalent of the rollup path's Phase C fsync fence.
func (bc *FSStore) dropTornIndexEntry(ctx context.Context, h block.ContentHash) error {
	if err := bc.localChunkIndex.DeleteLocalLocation(ctx, h); err != nil {
		return fmt.Errorf("chunkstore: drop torn index entry: %w", err)
	}
	return block.ErrChunkNotFound
}

// Get implements local.LocalStore.Get by delegating to ReadChunk.
// See ReadChunk for semantics, error contract, and LRU touch
// behavior. Signature is forward-compatible with the unified
// BlockStore.Get interface.
func (bc *FSStore) Get(ctx context.Context, h block.ContentHash) ([]byte, error) {
	return bc.ReadChunk(ctx, h)
}

// HasChunk reports whether the chunk exists in the local chunk store.
// Returns (true, nil) for an existing chunk, (false, nil) for a missing
// chunk, or (false, err) for any I/O error other than ENOENT.
func (bc *FSStore) HasChunk(ctx context.Context, h block.ContentHash) (bool, error) {
	if bc.isClosed() {
		return false, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Index-resident (logblob) chunks are located via the local chunk index —
	// consulting it dedups a second rollup pass from re-Appending the bytes.
	_, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
	if err != nil {
		return false, fmt.Errorf("chunkstore: get local location: %w", err)
	}
	return ok, nil
}

// DeleteChunk removes the chunk file. Treats missing-file as success
// (matches DeleteBlockFile semantics in manage.go). Decrements diskUsed by
// the deleted file's size.
//
// DeleteChunk is not called from any live code path; the method exists
// for conformance tests and the mark-sweep GC.
func (bc *FSStore) DeleteChunk(ctx context.Context, h block.ContentHash) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Remove the index entry so a logblob-resident chunk reads as a miss
	// afterwards. Idempotent. The blob bytes themselves are reclaimed by
	// blob-level eviction, handled separately — DeleteChunk does not rewrite
	// the blob or adjust logBlobDiskUsed here.
	if err := bc.localChunkIndex.DeleteLocalLocation(ctx, h); err != nil {
		return fmt.Errorf("chunkstore: delete local location: %w", err)
	}
	return nil
}
