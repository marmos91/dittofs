package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
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

// WriteAt writes data to the cache at the specified file offset.
//
// Write path (per block):
//  1. If the block already has a .blk file on disk and no memBlock in memory,
//     pwrite() directly to the file (tryDirectDiskWrite — avoids 8MB alloc).
//  2. Otherwise, copy into the pre-allocated 8MB memBlock buffer.
//  3. If the memBlock is full (8MB), flush it to disk immediately.
//  4. If memory budget is exceeded, flush the oldest dirty block (backpressure).
//
// No disk I/O for partial block writes that go through the memory path.
func (bc *BlockCache) WriteAt(ctx context.Context, payloadID string, data []byte, offset uint64) error {
	if bc.isClosed() {
		return ErrCacheClosed
	}

	if len(data) == 0 {
		return nil
	}

	remaining := data
	currentOffset := offset

	for len(remaining) > 0 {
		blockIdx := currentOffset / BlockSize
		blockOffset := uint32(currentOffset % BlockSize)

		spaceInBlock := BlockSize - blockOffset
		writeLen := min(uint32(len(remaining)), spaceInBlock)

		if writeLen < BlockSize {
			if bc.tryDirectDiskWrite(ctx, payloadID, blockIdx, blockOffset, remaining[:writeLen]) {
				remaining = remaining[writeLen:]
				currentOffset += uint64(writeLen)
				continue
			}
		}

		// Hard backpressure: if memory far exceeds budget, flush blocks
		// synchronously before allocating more. Prevents OOM during write
		// storms where NFS clients send hundreds of concurrent writes.
		for bc.memUsed.Load() > bc.maxMemory*2 {
			if !bc.flushOldestDirtyBlock(ctx) {
				break // No flushable blocks available
			}
		}

		key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
		mb := bc.getOrCreateMemBlock(key)

		mb.mu.Lock()
		copy(mb.data[blockOffset:blockOffset+writeLen], remaining[:writeLen])

		end := blockOffset + writeLen
		if end > mb.dataSize {
			mb.dataSize = end
		}
		mb.dirty = true
		mb.lastWrite = time.Now()

		isFull := mb.dataSize >= BlockSize
		mb.mu.Unlock()

		if isFull {
			if _, err := bc.flushBlock(ctx, payloadID, blockIdx, mb); err != nil {
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
	return nil
}

// tryDirectDiskWrite does a pwrite() directly to a .blk cache file, bypassing
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
func (bc *BlockCache) tryDirectDiskWrite(ctx context.Context, payloadID string, blockIdx uint64, blockOffset uint32, data []byte) bool {
	// Skip if there's a live memBlock — writes must go through memory for consistency.
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	if mb := bc.getMemBlock(key); mb != nil {
		return false
	}

	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)

	// Try to open the existing cache file for pwrite.
	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
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

	_, err = f.WriteAt(data, int64(blockOffset))
	f.Close()
	if err != nil {
		return false
	}

	// File write succeeded. Update metadata.
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = metadata.NewFileBlock(blockID, path)
	}
	fb.CachePath = path

	end := blockOffset + uint32(len(data))
	if end > fb.DataSize {
		fb.DataSize = end
	}

	// Mark as Sealed (dirty on disk) so the offloader uploads it.
	if fb.State == metadata.BlockStateUploaded || fb.State == 0 {
		fb.State = metadata.BlockStateSealed
	}
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	return true
}

// createBlockFile creates a new .blk cache file, including any parent
// directories. Used by tryDirectDiskWrite for eager file creation.
func (bc *BlockCache) createBlockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create block dir: %w", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0644)
}
