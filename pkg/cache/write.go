package cache

import (
	"context"
	"os"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

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
		writeLen := uint32(len(remaining))
		if writeLen > spaceInBlock {
			writeLen = spaceInBlock
		}

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
			if err := bc.flushBlock(ctx, payloadID, blockIdx, mb); err != nil {
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

// tryDirectDiskWrite does a pwrite() directly to an existing .blk cache file,
// bypassing the memory buffer. This eliminates the 8MB allocation + flush cycle
// for small random writes to blocks that already have on-disk cache files.
//
// Returns true if the write was handled, false to fall through to the memory path.
func (bc *BlockCache) tryDirectDiskWrite(ctx context.Context, payloadID string, blockIdx uint64, blockOffset uint32, data []byte) bool {
	// Skip if there's a live memBlock — writes must go through memory for consistency.
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	if mb := bc.getMemBlock(key); mb != nil {
		return false
	}

	// Try to open the cache file directly. This avoids a BadgerDB metadata lookup
	// for blocks that have never been cached — os.OpenFile on a non-existent path
	// hits the kernel dentry cache and returns ENOENT in ~1us.
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)

	f, err := os.OpenFile(path, os.O_WRONLY, 0644)
	if err != nil {
		return false // No existing cache file — use memory path.
	}
	_, err = f.WriteAt(data, int64(blockOffset))
	f.Close()
	if err != nil {
		return false
	}

	// File write succeeded. Update metadata (lookup is cheap now since we know
	// the block exists — it will hit the fileBlockQueue or BadgerDB).
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = metadata.NewFileBlock(blockID, path)
	}

	end := blockOffset + uint32(len(data))
	if end > fb.DataSize {
		fb.DataSize = end
	}

	// Dirty the block so the offloader re-uploads it.
	if fb.State == metadata.BlockStateUploaded {
		fb.State = metadata.BlockStateSealed
	}
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	return true
}
