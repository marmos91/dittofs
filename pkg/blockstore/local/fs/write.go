package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// directDiskWriteThreshold is the minimum write size for eager .blk file creation.
// Writes at or above this size go directly to disk (bypassing the 8MB memory buffer)
// even when no .blk file exists yet. This eliminates the first-run penalty where
// new files must go through the slow buffer-then-flush path.
//
// 64KiB is chosen because:
//   - NFS wsize=1MiB sequential writes (the bottleneck case) are well above this
//   - 4KiB random writes (which benefit from memory batching) are well below this
//   - A single 64KiB pwrite to disk is cheap (~0.1ms on NVMe)
const directDiskWriteThreshold = 64 * 1024

// WriteAt writes data to the local store at the specified file offset.
//
// Write path (per block):
//  1. If the block already has a .blk file on disk and no memBlock in memory,
//     pwrite() directly to the file (tryDirectDiskWrite -- avoids 8MB alloc).
//  2. Otherwise, copy into the pre-allocated 8MB memBlock buffer.
//  3. If the memBlock is full (8MB), flush it to disk immediately.
//  4. If memory budget is exceeded, flush the oldest dirty block (backpressure).
//
// No disk I/O for partial block writes that go through the memory path.
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

		if writeLen < uint32(blockstore.BlockSize) {
			if bc.tryDirectDiskWrite(ctx, payloadID, blockIdx, blockOffset, remaining[:writeLen]) {
				remaining = remaining[writeLen:]
				currentOffset += uint64(writeLen)
				continue
			}
		}

		// Hard backpressure: if memory exceeds 1.5x budget, flush blocks
		// synchronously before allocating more. Prevents OOM during write
		// storms where NFS clients send hundreds of concurrent writes.
		for bc.memUsed.Load() > bc.maxMemory*3/2 {
			if !bc.flushOldestDirtyBlock(ctx) {
				break // No flushable blocks available
			}
		}

		key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
		mb := bc.getOrCreateMemBlock(key)

		mb.mu.Lock()
		// Re-allocate buffer if this memBlock was previously flushed to disk.
		// Flushed memBlocks stay in the map with data=nil to avoid churn.
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
		// Bump writeGen so a concurrent flushBlock (stage-and-release, TD-09)
		// notices a writer interleaved during disk I/O and preserves the new
		// bytes instead of nilling mb.data on the post-write flag flip.
		mb.writeGen++

		isFull := mb.dataSize >= uint32(blockstore.BlockSize)
		mb.mu.Unlock()

		if isFull {
			// Block-fill flush: no fsync. NFS COMMIT (Flush) supplies the
			// durability fence; fsyncing per filled block would impose a
			// per-block fsync on the write hot path.
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

// tryDirectDiskWrite does a pwrite() directly to a .blk block file, bypassing
// the 8MB memory buffer. For writes >= directDiskWriteThreshold, it creates the
// .blk file eagerly if it doesn't exist yet. This eliminates the "first-run
// penalty" where new files had to go through the slow buffer-then-flush path
// (16.6 MB/s) while subsequent runs with existing .blk files used fast pwrite
// (51 MB/s).
//
// For writes below the threshold (e.g., 4KB random I/O), falls through to the
// memory buffer path which batches many small writes into one 8MB disk write.
//
// Returns true if the write was handled, false to fall through to the memory path.
func (bc *FSStore) tryDirectDiskWrite(ctx context.Context, payloadID string, blockIdx uint64, blockOffset uint32, data []byte) bool {
	// Skip if there's a live memBlock with data -- writes must go through memory
	// for consistency. Flushed memBlocks (data=nil) are fine to skip; they indicate
	// a .blk file exists on disk that we can pwrite to directly.
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			return false
		}
	}

	blockID := makeBlockID(key)

	path := bc.blockPath(blockID)

	// Try pooled fd first, then open from disk.
	f := bc.fdPool.Get(blockID)
	if f == nil {
		var err error
		f, err = os.OpenFile(path, os.O_WRONLY, 0644)
		if err != nil {
			// File doesn't exist. For large writes (>= threshold), create it eagerly
			// so all subsequent writes to this block use the fast pwrite path.
			// Small writes fall through to the memory buffer for batching.
			if len(data) < directDiskWriteThreshold {
				return false
			}
			f, err = bc.createBlockFile(path)
			if err != nil {
				return false
			}
		}
		bc.fdPool.Put(blockID, f)
	}

	if _, err := f.WriteAt(data, int64(blockOffset)); err != nil {
		// Fd may be stale (file was truncated/recreated). Evict and fall through.
		bc.fdPool.Evict(blockID)
		return false
	}

	// File write succeeded. Update metadata via the in-process diskIndex ONLY;
	// the write hot path must not consult the metadata backend (TD-02d / D-19).
	// Eventual persistence still happens asynchronously via queueFileBlockUpdate
	// -> pendingFBs -> SyncFileBlocks (background drainer).
	end := blockOffset + uint32(len(data))
	now := time.Now()

	fb, ok := bc.diskIndexLookup(blockID)
	if !ok {
		// No cached metadata for this on-disk block yet (e.g., a file recovered
		// from disk before its diskIndex entry was rebuilt, or the direct-disk
		// write raced with a concurrent evict). Create a fresh block record —
		// any previously persisted state in the metadata backend will be
		// overwritten by the next async drain; this is acceptable because
		// pwrite is a hot-path best-effort update and state transitions go
		// through the explicit MarkBlock* helpers.
		fb = blockstore.NewFileBlock(blockID, path)
	}

	fb.LocalPath = path
	if end > fb.DataSize {
		fb.DataSize = end
	}
	if fb.State == 0 {
		// New block, never synced -- mark Pending so the syncer picks it up.
		// Pending=0 is the safe zero value, so this assignment is mostly
		// documentary, but kept explicit for symmetry with the (Remote -> Pending)
		// reset path.
		fb.State = blockstore.BlockStatePending
	}
	// Remote blocks: don't revert to Pending on pwrite. Avoids triggering 8MB
	// re-syncs on every 4KB random write. Re-sync on explicit Flush.
	fb.LastAccess = now
	bc.queueFileBlockUpdate(fb)

	_ = ctx // retained in signature for parity with WriteAt; no longer needed on the hot path

	return true
}

// createBlockFile creates a new .blk block file, including any parent
// directories. Used by tryDirectDiskWrite for eager file creation.
func (bc *FSStore) createBlockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create block dir: %w", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
}
