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

	"github.com/marmos91/dittofs/pkg/blockstore"
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
// Implements blockstore.BlockStore.
func (bc *FSStore) Put(ctx context.Context, hash blockstore.ContentHash, data []byte) error {
	if hash.IsZero() {
		return fmt.Errorf("blockstore.fs: Put with zero hash")
	}
	return bc.StoreChunk(ctx, hash, data)
}

// GetRange returns a byte sub-range of the chunk addressed by hash.
//
// Implements blockstore.BlockStore.
func (bc *FSStore) GetRange(ctx context.Context, hash blockstore.ContentHash, offset, length int64) ([]byte, error) {
	if bc.isClosed() {
		return nil, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 {
		return nil, fmt.Errorf("blockstore.fs: GetRange: %w: offset %d", blockstore.ErrInvalidOffset, offset)
	}
	if length <= 0 {
		return nil, fmt.Errorf("blockstore.fs: GetRange: %w: length %d", blockstore.ErrInvalidSize, length)
	}
	path := bc.chunkPath(hash)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, blockstore.ErrChunkNotFound
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
// Implements blockstore.BlockStore.
func (bc *FSStore) Has(ctx context.Context, hash blockstore.ContentHash) (bool, error) {
	return bc.HasChunk(ctx, hash)
}

// Delete removes the object addressed by hash. Idempotent — absent hash
// returns nil. Delegates to DeleteChunk.
//
// Implements blockstore.BlockStore.
func (bc *FSStore) Delete(ctx context.Context, hash blockstore.ContentHash) error {
	return bc.DeleteChunk(ctx, hash)
}

// Head returns Meta for the object addressed by hash without
// transferring the body.
//
// Implements blockstore.BlockStore.
func (bc *FSStore) Head(ctx context.Context, hash blockstore.ContentHash) (blockstore.Meta, error) {
	if bc.isClosed() {
		return blockstore.Meta{}, ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return blockstore.Meta{}, err
	}
	path := bc.chunkPath(hash)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return blockstore.Meta{}, blockstore.ErrChunkNotFound
		}
		return blockstore.Meta{}, fmt.Errorf("blockstore.fs: Head: stat: %w", err)
	}
	return blockstore.Meta{
		Size:         info.Size(),
		LastModified: info.ModTime(),
	}, nil
}

// Walk enumerates every CAS object in the local chunk store. The
// callback receives the content hash and Meta for each object. Returns
// blockstore.ErrStopWalk from the callback for clean early-exit.
//
// Implements blockstore.BlockStore.
func (bc *FSStore) Walk(ctx context.Context, fn func(hash blockstore.ContentHash, meta blockstore.Meta) error) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if _, err := os.Stat(blocksDir); err != nil {
		if os.IsNotExist(err) {
			return nil // empty store
		}
		return fmt.Errorf("blockstore.fs: Walk: stat: %w", err)
	}
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
		if len(name) != blockstore.HashSize*2 {
			return nil
		}
		raw, hexErr := hex.DecodeString(name)
		if hexErr != nil || len(raw) != blockstore.HashSize {
			return nil
		}
		var h blockstore.ContentHash
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
		meta := blockstore.Meta{
			Size:         info.Size(),
			LastModified: info.ModTime(),
		}
		if cbErr := fn(h, meta); cbErr != nil {
			if errors.Is(cbErr, blockstore.ErrStopWalk) {
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
	return walkErr
}

// errStopWalkInternal is the unexported short-circuit sentinel
// FSStore.Walk hands to filepath.WalkDir to abort enumeration when the
// caller's callback returned blockstore.ErrStopWalk. It is never
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
func (bc *FSStore) ListUnsynced(ctx context.Context) iter.Seq2[blockstore.ContentHash, error] {
	return func(yield func(blockstore.ContentHash, error) bool) {
		// Local-only configurations: no remote means no synced markers
		// means nothing to mirror — empty iterator is the strict-subset
		// invariant collapse.
		if bc.syncedHashStore == nil {
			return
		}

		// Snapshot phase: collect every CAS hash currently on disk
		// before any IsSynced lookups, so the dir-walk file handles are
		// released before the per-hash filter loop runs.
		var snapshot []blockstore.ContentHash
		walkErr := bc.Walk(ctx, func(hash blockstore.ContentHash, _ blockstore.Meta) error {
			snapshot = append(snapshot, hash)
			return nil
		})
		if walkErr != nil {
			var zero blockstore.ContentHash
			yield(zero, fmt.Errorf("blockstore.fs: ListUnsynced: snapshot: %w", walkErr))
			return
		}

		// Filter phase: O(1) IsSynced lookup per hash.
		for _, h := range snapshot {
			if err := ctx.Err(); err != nil {
				var zero blockstore.ContentHash
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
