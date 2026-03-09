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
		// Re-allocate buffer if this memBlock was previously flushed to disk.
		// Flushed memBlocks stay in the map with data=nil to avoid churn.
		if mb.data == nil {
			mb.data = getBlockBuf()
			bc.memUsed.Add(int64(BlockSize))
		}
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
			if _, _, err := bc.flushBlock(ctx, payloadID, blockIdx, mb); err != nil {
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

	// Use direct payload store path if available (filesystem backend optimization).
	// This eliminates double-write: data goes straight to the payload store,
	// skipping cache .blk files entirely. Blocks are marked Uploaded immediately.
	var path string
	var isDirect bool
	if bc.directWritePath != nil {
		if p := bc.directWritePath(payloadID, blockIdx); p != "" {
			path = p
			isDirect = true
		}
	}
	if path == "" {
		path = bc.blockPath(blockID)
	}

	// Try cached fd first, then open from disk.
	f := bc.fdCache.Get(blockID)
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
		bc.fdCache.Put(blockID, f)
	}

	if _, err := f.WriteAt(data, int64(blockOffset)); err != nil {
		// Fd may be stale (file was truncated/recreated). Evict and fall through.
		bc.fdCache.Evict(blockID)
		return false
	}

	// File write succeeded. Update metadata.
	// Fast path: if we already have a pending FileBlock update, mutate it
	// in-place instead of doing a full lookupFileBlock (which hits BadgerDB).
	end := blockOffset + uint32(len(data))
	now := time.Now()

	if v, ok := bc.pendingFBs.Load(blockID); ok {
		fb := v.(*metadata.FileBlock)
		fb.CachePath = path
		if end > fb.DataSize {
			fb.DataSize = end
		}
		if isDirect {
			fb.State = metadata.BlockStateUploaded
			fb.BlockStoreKey = FormatStoreKey(payloadID, blockIdx)
		} else if fb.State == 0 {
			// New block, never uploaded — seal so the uploader picks it up.
			fb.State = metadata.BlockStateSealed
		}
		// If Uploaded: leave as-is. The on-disk cache has newer data than S3,
		// but re-sealing on every 4KB pwrite would cause a storm of 8MB
		// re-uploads via the periodic uploader. Re-upload happens on explicit
		// Flush (GetDirtyBlocks) when the file is finalized.
		fb.LastAccess = now
		bc.pendingFBs.Store(blockID, fb)
		return true
	}

	// Slow path: lookup from store or create new
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = metadata.NewFileBlock(blockID, path)
	}
	fb.CachePath = path

	if end > fb.DataSize {
		fb.DataSize = end
	}

	if isDirect {
		fb.State = metadata.BlockStateUploaded
		fb.BlockStoreKey = FormatStoreKey(payloadID, blockIdx)
	} else if fb.State == 0 {
		// New block — seal for initial upload.
		fb.State = metadata.BlockStateSealed
	}
	// Uploaded blocks: don't re-seal on pwrite. Avoids triggering 8MB
	// re-uploads on every 4KB random write. Re-upload on explicit Flush.
	fb.LastAccess = now
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
