package fs

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// WriteAt is the engine-consumed transitional admin write path. It
// buffers data into the 8 MiB in-memory memBlock and flushes filled
// blocks; the path-keyed direct-disk-write fast path was deleted in
// Phase 17 alongside the legacy <share>/<file>/<idx>.blk writer (the
// CAS rollup path replaces it on the production read/write surface).
//
// Engine consumer at engine.go:320 — Phase 18's Syncer simplification
// rewrites that call site onto BlockStore.Put + AppendWrite, at which
// point this method and the surrounding memBlock infrastructure are
// deleted in their entirety.
//
// Deprecated: removed in Phase 18 (Syncer simplification rewrites these consumers onto BlockStore.Put/Get/Walk).
func (bc *FSStore) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if bc.isClosed() {
		return ErrStoreClosed
	}

	if len(data) == 0 {
		return nil
	}

	remaining := data
	currentOffset := offset

	for len(remaining) > 0 {
		blockIdx := currentOffset / blockstore.BlockSize
		blockOffset := uint32(currentOffset % blockstore.BlockSize)

		spaceInBlock := uint32(blockstore.BlockSize) - blockOffset
		writeLen := min(uint32(len(remaining)), spaceInBlock)

		// Hard backpressure: if memory exceeds 1.5x budget, flush
		// blocks synchronously before allocating more. Prevents OOM
		// during write storms.
		for bc.memUsed.Load() > bc.maxMemory*3/2 {
			if !bc.flushOldestDirtyBlock(ctx) {
				break // No flushable blocks available
			}
		}

		key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
		mb := bc.getOrCreateMemBlock(key)

		mb.mu.Lock()
		// Re-allocate buffer if this memBlock was previously flushed
		// to disk. Flushed memBlocks stay in the map with data=nil to
		// avoid churn.
		if mb.data == nil {
			mb.data = getBlockBuf()
			bc.memUsed.Add(int64(blockstore.BlockSize))
		}
		copy(mb.data[blockOffset:blockOffset+writeLen], remaining[:writeLen])

		end := blockOffset + writeLen
		if end > mb.dataSize {
			mb.dataSize = end
		}
		mb.dirty = true
		mb.lastWrite = time.Now()
		// Bump writeGen so a concurrent flushBlock (stage-and-release,
		// TD-09) notices a writer interleaved during disk I/O and
		// preserves the new bytes instead of nilling mb.data on the
		// post-write flag flip.
		mb.writeGen++

		isFull := mb.dataSize >= uint32(blockstore.BlockSize)
		mb.mu.Unlock()

		if isFull {
			// Block-fill flush: no fsync. NFS COMMIT (Flush) supplies
			// the durability fence; fsyncing per filled block would
			// impose a per-block fsync on the write hot path.
			if _, _, err := bc.flushBlock(ctx, payloadID, blockIdx, mb, false); err != nil {
				return err
			}
		}

		if bc.memUsed.Load() > bc.maxMemory {
			bc.flushOldestDirtyBlock(ctx)
		}

		remaining = remaining[writeLen:]
		currentOffset += uint64(writeLen)
	}

	bc.updateFileSize(payloadID, offset+uint64(len(data)))

	// Update per-file access time for eviction ordering (batched, no I/O).
	bc.accessTracker.Touch(payloadID)

	return nil
}
