package fs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// This file lands the BlockStore + BlockStoreAppend method surface on
// *FSStore (Phase 17 Plan 07). Each method delegates to the existing
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
			// entries silently; surface anything else (permission,
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
				return io.EOF // sentinel for clean exit out of WalkDir
			}
			return fmt.Errorf("walk halted at %s: %w", h, cbErr)
		}
		return nil
	})
	if walkErr == io.EOF {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return walkErr
}

// DeleteLog removes the per-file append log and tracked intervals for
// payloadID. Wraps DeleteAppendLog so the *FSStore satisfies
// blockstore.BlockStoreAppend.
//
// Implements blockstore.BlockStoreAppend.
func (bc *FSStore) DeleteLog(ctx context.Context, payloadID string) error {
	return bc.DeleteAppendLog(ctx, payloadID)
}
