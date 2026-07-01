package fs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/block"
)

// chunkPath returns the content-addressed chunk path under baseDir/blocks/.
// Layout: <baseDir>/blocks/<hex[0:2]>/<hex[2:4]>/<hex> (two-level shard).
//
// Path components are derived exclusively from hex.EncodeToString(h[:]) — the
// characters are constrained to [0-9a-f], so path traversal via crafted hash
// input is not possible (threat).
func (bc *FSStore) chunkPath(h block.ContentHash) string {
	hex := h.String()
	return filepath.Join(bc.baseDir, "blocks", hex[0:2], hex[2:4], hex)
}

// StoreChunk writes data under its content-addressed path. Atomic via
// .tmp + rename; fsyncs the chunk file and the containing directory so the
// rename is durable (step 1 CAS durability, torn-write safety —
// threat).
//
// Idempotent: if the chunk already exists (HasChunk returns true for h)
// StoreChunk is a no-op and returns nil. This is what lets the rollup pool
// retry safely after a crash between StoreChunk and CommitChunks.
//
// Caller is responsible for asserting that BLAKE3(data) == h before calling;
// StoreChunk trusts its inputs (threat accept). The rollup pool
// is the only production caller.
//
// TRANSITIONAL-NEXT-MILESTONE: zstd compression (see #519 "Deferred to
// v0.17+"). Compression would wrap the data slice before disk write
// OnChunkComplete still receives the UNCOMPRESSED data so the engine
// Cache can serve reads without an extra decompress hop.
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

	// Log-blob path: when an index is wired, append the chunk bytes to the
	// log-blob substrate and record the location in the index instead of
	// writing a per-chunk cas/<hash> file. Legacy CAS writer below is the
	// fallback for index-less fixtures (quarantined, not deleted).
	if bc.logBlob != nil && bc.localChunkIndex != nil {
		return bc.storeChunkLogBlob(ctx, h, data)
	}

	path := bc.chunkPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("chunkstore: mkdir: %w", err)
	}

	// Use a unique temp filename per attempt so two concurrent StoreChunk
	// calls for the same hash (whether on Unix or Windows) do not race on
	// the same .tmp file. The destination is content-addressed and idempotent
	// if the rename target already exists from a winning concurrent call
	// treat that as success after re-stating the destination.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("chunkstore: create tmp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: fsync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// On Windows, os.Rename fails if the destination already exists.
		// CAS writes are idempotent — if the destination is already there
		// with the same content (a concurrent winner stored the same hash)
		// treat that as success and clean up our tmp.
		if _, statErr := os.Stat(path); statErr == nil {
			_ = os.Remove(tmp)
			return nil
		}
		_ = os.Remove(tmp)
		return fmt.Errorf("chunkstore: rename: %w", err)
	}
	// Fsync the parent dir so the rename durably reaches stable storage.
	// Best-effort: a failing dir fsync does not invalidate the data (the file
	// is fully written + fsynced above); log-free to match flush.go's
	// syncFile posture on read-only dir handles.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	bc.diskUsed.Add(int64(len(data)))
	// register the chunk with the in-process LRU so eviction can
	// reach it. This is the canonical post-write touch — readers use a
	// separate ReadChunk wiring to promote on cache hits.
	//
	// TRANSITIONAL-NEXT-MILESTONE: pinned hot-tail RAM (see #519 "Deferred
	// to v0.17+"). When pinned hot-tail RAM lands, StoreChunk may bypass
	// the disk write entirely for recently-rolled-up chunks, requiring
	// the OnChunkComplete callback to fire from the RAM-tier path instead.
	bc.lruTouch(h, int64(len(data)), path)

	// Fire the chunk-completion callback after disk store + LRU touch
	// succeed. Exactly-once-per-successful-touch contract. Nil-safe —
	// the legacy behavior is preserved when no callback is installed.
	// Fires OUTSIDE the lruMu lock (lruTouch
	// released it on return) to avoid widening the hot lock window across
	// an unrelated lock (the typical consumer is engine.Cache.Put, which
	// takes its own lock). Error paths above already returned before this
	// point — the callback is invariant on successful StoreChunk only.
	if cb := bc.onChunkComplete.Load(); cb != nil && cb.fn != nil {
		cb.fn(h, data, path)
	}
	return nil
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
		// dedups. This direct path indexes without an fsync; crash-durable
		// ordering (index committed only after the blob is fsynced) is provided
		// by the rollup's Phase C fence via stageRollupChunk/commitStagedChunk,
		// which is the only production writer of log-blob chunks.
	}
	if err := bc.localChunkIndex.PutLocalLocation(ctx, h, loc); err != nil {
		return fmt.Errorf("chunkstore: put local location: %w", err)
	}
	// logBlobDiskUsed is incremented only after a successful PutLocalLocation so
	// the counter reflects indexed (reachable) bytes, not bytes that may have
	// been orphaned by a failed index write above.
	if len(data) > 0 {
		bc.logBlobDiskUsed.Add(int64(len(data)))
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
// dedup hit against an already-durable chunk, or the legacy CAS path (index-less
// fixtures), which fsyncs the cas/<hash> file inline in StoreChunk.
func (bc *FSStore) stageRollupChunk(ctx context.Context, h block.ContentHash, data []byte) (stagedChunk, error) {
	// Legacy CAS path: no log-blob / index wired. StoreChunk fsyncs the
	// cas/<hash> file before returning, so durability needs no deferral.
	if bc.logBlob == nil || bc.localChunkIndex == nil {
		if err := bc.StoreChunk(ctx, h, data); err != nil {
			return stagedChunk{}, err
		}
		return stagedChunk{}, nil
	}

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
	}
	return nil
}

// ReadChunk returns the bytes of the chunk addressed by h.
// Returns block.ErrChunkNotFound if the chunk is absent.
//
// Dual-mode: a wired local chunk index is consulted first (logblob-resident
// chunks); on a miss it falls back to the legacy cas/<hash> file so
// pre-existing CAS data stays readable (back-compat until PR4 migrates it).
func (bc *FSStore) ReadChunk(ctx context.Context, h block.ContentHash) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bc.localChunkIndex != nil {
		loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil {
			return nil, fmt.Errorf("chunkstore: get local location: %w", err)
		}
		if ok {
			if loc.RawLength == 0 {
				return []byte{}, nil
			}
			dst := make([]byte, loc.RawLength)
			n, rerr := bc.logBlob.ReadAt(ctx, loc, dst)
			if rerr != nil {
				return nil, fmt.Errorf("chunkstore: logblob read: %w", rerr)
			}
			return dst[:n], nil
		}
	}
	path := bc.chunkPath(h)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("chunkstore: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	// .blk chunk files are immutable content-addressed blobs: StoreChunk
	// writes the full slice then atomically renames into place, so the
	// on-disk size equals the content size and never changes afterward.
	// Stat the open fd (not the path) so a concurrent lruEvictOne unlink
	// can't change the size out from under us — the open fd still reads on
	// POSIX. A single stat-sized allocation + io.ReadFull avoids the
	// repeated doubling+copy that io.ReadAll incurs on the big-read path.
	var data []byte
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("chunkstore: read: %w", err)
	}
	if size := fi.Size(); size > 0 {
		buf := make([]byte, size)
		if _, err := io.ReadFull(f, buf); err != nil {
			return nil, fmt.Errorf("chunkstore: read: %w", err)
		}
		data = buf
	} else {
		// Zero-byte stat (or an unexpected non-regular file): fall back to
		// io.ReadAll, which correctly handles a size we can't trust.
		data, err = io.ReadAll(f)
		if err != nil {
			return nil, fmt.Errorf("chunkstore: read: %w", err)
		}
	}
	// promote on cache hit so frequently-read chunks survive
	// eviction. A concurrent lruEvictOne may have unlinked the file
	// between os.Open and here — the open fd still reads on POSIX, but
	// re-inserting into the LRU would create a ghost entry that points at
	// a path that no longer exists. Re-stat before re-inserting; if the
	// file is gone, skip the touch (the next read will surface
	// ErrChunkNotFound under the engine's accept-and-refetch posture).
	if _, statErr := os.Stat(path); statErr == nil {
		bc.lruTouch(h, int64(len(data)), path)
	}
	return data, nil
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
	// Index-resident (logblob) chunks have no cas/<hash> file, so the index
	// must be consulted first — otherwise a second rollup pass would re-Append
	// the same bytes. Falls back to the legacy CAS stat for back-compat data.
	if bc.localChunkIndex != nil {
		_, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil {
			return false, fmt.Errorf("chunkstore: get local location: %w", err)
		}
		if ok {
			return true, nil
		}
	}
	_, err := os.Stat(bc.chunkPath(h))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("chunkstore: stat: %w", err)
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
	// Remove the index entry first so a logblob-resident chunk reads as a miss
	// afterwards. Idempotent. The blob bytes themselves are reclaimed by
	// blob-level eviction, handled separately — DeleteChunk does not rewrite
	// the blob or adjust logBlobDiskUsed here.
	if bc.localChunkIndex != nil {
		if err := bc.localChunkIndex.DeleteLocalLocation(ctx, h); err != nil {
			return fmt.Errorf("chunkstore: delete local location: %w", err)
		}
	}
	path := bc.chunkPath(h)
	st, statErr := os.Stat(path)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("chunkstore: remove: %w", err)
	}
	if statErr == nil && st.Size() > 0 {
		bc.diskUsed.Add(-st.Size())
	}
	// prune from the in-process LRU so a future ensureSpace doesn't
	// try to unlink a file we just deleted.
	bc.lruMu.Lock()
	if el, ok := bc.lruIndex[h]; ok {
		bc.lruList.Remove(el)
		delete(bc.lruIndex, h)
	}
	bc.lruMu.Unlock()
	return nil
}
