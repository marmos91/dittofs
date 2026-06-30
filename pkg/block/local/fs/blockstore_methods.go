package fs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// This file lands the BlockStore + BlockStoreAppend method surface on
// *FSStore. Each method delegates to the existing
// chunkstore.go primitives (StoreChunk / ReadChunk / HasChunk /
// DeleteChunk / chunkPath) — there is no new on-disk layout introduced
// here. The CAS chunk store under <baseDir>/blocks/<hh>/<hh>/<hex>
// already implements the byte-level contract; this file is a thin
// adapter that matches the unified interface signatures.

// Put writes data under the key derived from hash. Idempotent on
// (hash, identical bytes) — same as StoreChunk.
//
// Implements block.BlockStore.
func (bc *FSStore) Put(ctx context.Context, hash block.ContentHash, data []byte) error {
	if hash.IsZero() {
		return fmt.Errorf("blockstore.fs: Put with zero hash")
	}
	// Put is the read-through cache's write entry: its production engine
	// callers are the syncer's inline fetch + prefetch in engine/fetch.go (the
	// offline CAS-migration tool and the conformance suite also call it, both
	// with maxDisk == 0, where ensureSpace is a no-op). Unlike the
	// append/rollup write path it does not flow through ensureSpace, so
	// without this a read-only workload over a remote tier grows the local
	// CAS store without bound, past maxDisk, because eviction is never
	// triggered (#1362). Reserve capacity for genuinely new chunks only:
	// StoreChunk is idempotent, and reserving for an already-present chunk
	// would over-evict to make room for bytes that are never written.
	exists, err := bc.Has(ctx, hash)
	if err != nil {
		return fmt.Errorf("blockstore.fs: Put: %w", err)
	}
	if !exists {
		if err := bc.ensureSpace(ctx, int64(len(data))); err != nil {
			return fmt.Errorf("blockstore.fs: Put: %w", err)
		}
	}
	return bc.StoreChunk(ctx, hash, data)
}

// GetRange returns a byte sub-range of the chunk addressed by hash.
//
// Implements block.BlockStore.
func (bc *FSStore) GetRange(ctx context.Context, hash block.ContentHash, offset, length int64) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("blockstore.fs: GetRange: %w: offset %d", block.ErrInvalidOffset, offset)
	}
	if length <= 0 {
		return nil, fmt.Errorf("blockstore.fs: GetRange: %w: length %d", block.ErrInvalidSize, length)
	}
	// Log-blob path: index hit -> read a clipped sub-range from the blob.
	if bc.localChunkIndex != nil {
		loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, hash)
		if err != nil {
			return nil, fmt.Errorf("blockstore.fs: GetRange: get local location: %w", err)
		}
		if ok {
			if offset >= loc.RawLength {
				return nil, fmt.Errorf("blockstore.fs: GetRange: offset %d beyond size %d", offset, loc.RawLength)
			}
			if offset+length > loc.RawLength {
				length = loc.RawLength - offset
			}
			sub := block.LocalChunkLocation{
				LogBlobID: loc.LogBlobID,
				RawOffset: loc.RawOffset + offset,
				RawLength: length,
			}
			buf := make([]byte, length)
			n, rerr := bc.logBlob.ReadAt(ctx, sub, buf)
			if rerr != nil {
				return nil, fmt.Errorf("blockstore.fs: GetRange: logblob read: %w", rerr)
			}
			return buf[:n], nil
		}
	}
	path := bc.chunkPath(hash)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, block.ErrChunkNotFound
		}
		return nil, fmt.Errorf("blockstore.fs: GetRange: open: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("blockstore.fs: GetRange: stat: %w", err)
	}
	if offset >= info.Size() {
		return nil, fmt.Errorf("blockstore.fs: GetRange: offset %d beyond size %d", offset, info.Size())
	}
	// Clamp to remaining bytes.
	if offset+length > info.Size() {
		length = info.Size() - offset
	}
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil, fmt.Errorf("blockstore.fs: GetRange: read: %w", err)
	}
	return buf, nil
}

// Has reports whether the store currently holds an object addressed by
// hash. Delegates to HasChunk.
//
// Implements block.BlockStore.
func (bc *FSStore) Has(ctx context.Context, hash block.ContentHash) (bool, error) {
	return bc.HasChunk(ctx, hash)
}

// Delete removes the object addressed by hash. Idempotent — absent hash
// returns nil. Delegates to DeleteChunk.
//
// Implements block.BlockStore.
func (bc *FSStore) Delete(ctx context.Context, hash block.ContentHash) error {
	return bc.DeleteChunk(ctx, hash)
}

// Head returns Meta for the object addressed by hash without
// transferring the body.
//
// Implements block.BlockStore.
func (bc *FSStore) Head(ctx context.Context, hash block.ContentHash) (block.Meta, error) {
	if bc.isClosed() {
		return block.Meta{}, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return block.Meta{}, err
	}
	// Log-blob path: index hit -> Size from the recorded location, mtime from
	// the blobs directory (per-chunk timestamps are not tracked).
	if bc.localChunkIndex != nil {
		loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, hash)
		if err != nil {
			return block.Meta{}, fmt.Errorf("blockstore.fs: Head: get local location: %w", err)
		}
		if ok {
			return block.Meta{
				Size:         loc.RawLength,
				LastModified: bc.blobsModTime(),
			}, nil
		}
	}
	path := bc.chunkPath(hash)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return block.Meta{}, block.ErrChunkNotFound
		}
		return block.Meta{}, fmt.Errorf("blockstore.fs: Head: stat: %w", err)
	}
	return block.Meta{
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}

// ReadLocalAt reads loc.RawLength bytes from the local log-blob substrate at the
// given location into dst, delegating to the log-blob Manager. It is the read
// half of the engine's block carver (#1414): the carver resolves a chunk's
// LocalChunkLocation, reads its raw plaintext here, seals it, and frames it into
// a block. Returns an error when no log-blob substrate is wired (a CAS-only
// store), which the carver treats as "not log-blob-resident".
func (bc *FSStore) ReadLocalAt(ctx context.Context, loc block.LocalChunkLocation, dst []byte) (int, error) {
	if bc.logBlob == nil {
		return 0, fmt.Errorf("blockstore.fs: ReadLocalAt: no log-blob substrate")
	}
	return bc.logBlob.ReadAt(ctx, loc, dst)
}

// localChunkWalker is the narrow, consumer-defined capability the local store
// uses to enumerate logblob-resident chunks (for Walk / GC). A LocalChunkIndex
// implementation may optionally provide it; absence simply means logblob
// chunks are not enumerated on that backend (no interface-surface growth).
type localChunkWalker interface {
	WalkLocalLocations(ctx context.Context, fn func(block.ContentHash, block.LocalChunkLocation) error) error
}

// blobsModTime returns the modification time of the <baseDir>/blobs directory,
// used as a non-zero LastModified for logblob-resident chunks (which carry no
// per-chunk timestamp). Falls back to a stat of baseDir on error.
func (bc *FSStore) blobsModTime() time.Time {
	if fi, err := os.Stat(filepath.Join(bc.baseDir, "blobs")); err == nil {
		return fi.ModTime()
	}
	if fi, err := os.Stat(bc.baseDir); err == nil {
		return fi.ModTime()
	}
	return time.Now()
}

// Walk enumerates every CAS object in the local chunk store. The
// callback receives the content hash and Meta for each object. Returns
// block.ErrStopWalk from the callback for clean early-exit.
//
// Implements block.BlockStore.
func (bc *FSStore) Walk(ctx context.Context, fn func(hash block.ContentHash, meta block.Meta) error) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// CAS files first (legacy + back-compat). A missing blocks/ dir is not an
	// empty store any more: logblob-resident chunks live only in the index, so
	// skip the CAS walk and fall through to the index enumeration below.
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if _, err := os.Stat(blocksDir); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("blockstore.fs: Walk: stat: %w", err)
		}
	} else {
		walkErr := filepath.WalkDir(blocksDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			name := d.Name()
			if len(name) != block.HashSize*2 {
				return nil
			}
			raw, hexErr := hex.DecodeString(name)
			if hexErr != nil || len(raw) != block.HashSize {
				return nil
			}
			var h block.ContentHash
			copy(h[:], raw)
			info, infoErr := d.Info()
			if infoErr != nil {
				// Race vs concurrent Delete: a file enumerated by WalkDir
				// can disappear before d.Info() runs. Skip vanished
				// entries silently; surface anything else (permission
				// transient I/O) so callers like GC don't miss objects.
				if errors.Is(infoErr, os.ErrNotExist) {
					return nil
				}
				return fmt.Errorf("walk: stat %s: %w", h, infoErr)
			}
			meta := block.Meta{
				Size:         info.Size(),
				LastModified: info.ModTime(),
			}
			if cbErr := fn(h, meta); cbErr != nil {
				if errors.Is(cbErr, block.ErrStopWalk) {
					// Translate the public clean-exit sentinel into an
					// unexported one so we can distinguish "callback
					// asked to stop cleanly" from "filepath.WalkDir or
					// the callback returned ErrStopWalk-wrapped-or-not
					// for some other reason". Using a private sentinel
					// also keeps io.EOF reserved for its real meaning:
					// a callback that legitimately returns io.EOF (or
					// wraps it) must halt with the wrapped error, not
					// be silently treated as a clean exit.
					return errStopWalkInternal
				}
				return fmt.Errorf("walk halted at %s: %w", h, cbErr)
			}
			return nil
		})
		if errors.Is(walkErr, errStopWalkInternal) {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			return walkErr
		}
	}

	// Logblob-resident chunks: enumerate the index when it supports walking.
	walker, ok := bc.localChunkIndex.(localChunkWalker)
	if !ok {
		return nil
	}
	mtime := bc.blobsModTime()
	idxErr := walker.WalkLocalLocations(ctx, func(h block.ContentHash, loc block.LocalChunkLocation) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		meta := block.Meta{Size: loc.RawLength, LastModified: mtime}
		if cbErr := fn(h, meta); cbErr != nil {
			if errors.Is(cbErr, block.ErrStopWalk) {
				return errStopWalkInternal
			}
			return fmt.Errorf("walk halted at %s: %w", h, cbErr)
		}
		return nil
	})
	if errors.Is(idxErr, errStopWalkInternal) {
		return nil
	}
	return idxErr
}

// errStopWalkInternal is the unexported short-circuit sentinel
// FSStore.Walk hands to filepath.WalkDir to abort enumeration when the
// caller's callback returned block.ErrStopWalk. It is never
// surfaced to the caller — Walk maps it back to nil — and is never
// observed by the callback. Keeping it private means a callback that
// returns io.EOF (closed reader, exhausted iterator, …) is treated as
// a halting error and wrapped via the public contract, instead of
// being mistaken for the internal short-circuit token.
var errStopWalkInternal = errors.New("blockstore.fs: stop walk (internal)")

// ListUnsynced returns a push iterator over every CAS hash present in
// the local store that has not yet been marked synced. The iterator
// materializes the hash set up front by running Walk to completion
// then filters that snapshot against the injected SyncedHashStore one
// hash at a time. Snapshot-at-start semantics keep iteration bounded
// even under hot-write workloads: chunks rolled up after Walk returns
// are picked up on the NEXT pass, not chased mid-iteration.
//
// When no SyncedHashStore is wired (local-only configurations), the
// iterator yields nothing — the synced set is empty, so the unsynced
// "everything not in synced" set collapses to the empty set under the
// no-remote-mirror invariant.
//
// The iterator is ctx-cancel-aware: a Done ctx between hashes yields
// (zero, ctx.Err()) and returns. Per-hash IsSynced backend errors
// surface as (hash, wrapped error) at the yield site; the consumer
// decides whether to continue or stop.
//
// Implements local.LocalStore.
func (bc *FSStore) ListUnsynced(ctx context.Context) iter.Seq2[block.ContentHash, error] {
	return func(yield func(block.ContentHash, error) bool) {
		// Local-only configurations: no remote means no synced markers
		// means nothing to mirror — empty iterator is the strict-subset
		// invariant collapse.
		if bc.syncedHashStore == nil {
			return
		}

		// Snapshot phase: collect every CAS hash currently on disk
		// before any IsSynced lookups, so the dir-walk file handles are
		// released before the per-hash filter loop runs.
		var snapshot []block.ContentHash
		walkErr := bc.Walk(ctx, func(hash block.ContentHash, _ block.Meta) error {
			snapshot = append(snapshot, hash)
			return nil
		})
		if walkErr != nil {
			var zero block.ContentHash
			yield(zero, fmt.Errorf("blockstore.fs: ListUnsynced: snapshot: %w", walkErr))
			return
		}

		// Filter phase: O(1) IsSynced lookup per hash.
		for _, h := range snapshot {
			if err := ctx.Err(); err != nil {
				var zero block.ContentHash
				yield(zero, err)
				return
			}
			synced, err := bc.syncedHashStore.IsSynced(ctx, h)
			if err != nil {
				if !yield(h, fmt.Errorf("blockstore.fs: ListUnsynced: synced lookup %s: %w", h, err)) {
					return
				}
				continue
			}
			if synced {
				continue
			}
			if !yield(h, nil) {
				return
			}
		}
	}
}
