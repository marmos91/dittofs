package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// Flush writes all dirty in-memory blocks for a file to disk as .blk files.
// Called on NFS COMMIT to ensure data reaches stable storage before responding
// to the client. Each flushed block transitions to BlockStateLocal, meaning
// it is on disk and ready for the syncer to sync to remote.
//
// Returns the list of blocks that were flushed, so GetDirtyBlocks can use them
// directly without a BadgerDB round-trip (SyncFileBlocksForFile + ListLocalBlocks).
//
// When skipFsync is set (e.g. S3 backends), fsync is skipped -- data durability
// comes from remote sync, not local disk. The cache is just a staging buffer.
func (bc *FSStore) Flush(ctx context.Context, payloadID string) ([]local.FlushedBlock, error) {
	if bc.isClosed() {
		return nil, ErrCacheClosed
	}

	// Collect keys for this payloadID via secondary index (O(1) lookup).
	bc.blocksMu.RLock()
	var keys []blockKey
	if fm := bc.fileBlocks[payloadID]; fm != nil {
		keys = make([]blockKey, 0, len(fm))
		for blockIdx := range fm {
			keys = append(keys, blockKey{payloadID: payloadID, blockIdx: blockIdx})
		}
	}
	bc.blocksMu.RUnlock()

	// Track flushed blocks for callers (GetDirtyBlocks) and fsync.
	var flushed []local.FlushedBlock

	for _, key := range keys {
		mb := bc.getMemBlock(key)
		if mb == nil {
			continue
		}
		mb.mu.RLock()
		isDirty := mb.dirty
		mb.mu.RUnlock()
		if isDirty {
			path, dataSize, err := bc.flushBlock(ctx, key.payloadID, key.blockIdx, mb)
			if err != nil {
				return nil, err
			}
			if path != "" {
				flushed = append(flushed, local.FlushedBlock{
					BlockIndex: key.blockIdx,
					CachePath:  path,
					DataSize:   dataSize,
				})
			}
		}
	}

	// fsync all flushed files now (COMMIT path only).
	// Skipped for S3 backends where cache is a staging buffer, not durable store.
	if !bc.skipFsync {
		for _, fb := range flushed {
			if err := syncFile(fb.CachePath); err != nil {
				logger.Warn("cache: fsync on COMMIT failed", "path", fb.CachePath, "error", err)
			}
		}
	}

	return flushed, nil
}

// flushBlock writes a single memBlock to disk as a .blk file and releases the
// 8MB memory buffer. The block transitions from Dirty -> Local in the FileBlockStore.
//
// This does NOT call fsync -- the write is buffered in the OS page cache for
// maximum throughput. fsync is deferred to Flush() (NFS COMMIT) which guarantees
// durability only when the client explicitly requests it.
//
// The memBlock is NOT removed from the map -- it stays as a placeholder with
// data=nil. Subsequent writes to the same block will re-allocate a buffer.
// This avoids map churn (delete+re-insert) and prevents a race condition where
// a concurrent writer could see a stale memBlock with nil data between the
// map delete and the next getOrCreateMemBlock call.
//
// Returns the path and dataSize of the flushed file, or empty string if no flush was needed.
func (bc *FSStore) flushBlock(ctx context.Context, payloadID string, blockIdx uint64, mb *memBlock) (string, uint32, error) {
	mb.mu.Lock()

	if !mb.dirty || mb.data == nil {
		mb.mu.Unlock()
		return "", 0, nil
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	path := bc.blockPath(blockID)

	// Evict fds from cache before truncating the file (O_TRUNC invalidates them)
	bc.fdCache.Evict(blockID)
	bc.readFDCache.Evict(blockID)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		mb.mu.Unlock()
		return "", 0, fmt.Errorf("create block dir: %w", err)
	}

	// Track previous file size to compute diskUsed delta (not always +dataSize).
	// Without this, re-flushing the same block drifts diskUsed upward.
	var prevDiskSize int64
	if fi, statErr := os.Stat(path); statErr == nil {
		prevDiskSize = fi.Size()
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		mb.mu.Unlock()
		return "", 0, fmt.Errorf("create cache file: %w", err)
	}

	dataSize := mb.dataSize
	if _, err := f.Write(mb.data[:dataSize]); err != nil {
		_ = f.Close()
		mb.mu.Unlock()
		return "", 0, fmt.Errorf("write cache file: %w", err)
	}

	// No fsync here -- deferred to Flush() for throughput.
	// The data is in OS page cache and will survive process crashes
	// (only lost on power failure, which NFS UNSTABLE semantics allow).
	_ = f.Close()

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if !errors.Is(err, blockstore.ErrFileBlockNotFound) {
			mb.mu.Unlock()
			return "", 0, fmt.Errorf("lookup file block: %w", err)
		}
		fb = blockstore.NewFileBlock(blockID, path)
	}
	fb.CachePath = path
	fb.DataSize = dataSize
	fb.State = blockstore.BlockStateLocal
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	// Release the buffer but keep the mb in the map as a placeholder.
	// The next write to this block will re-allocate via ensureData().
	bufToReturn := mb.data
	mb.data = nil
	mb.dataSize = 0
	mb.dirty = false
	bc.memUsed.Add(-int64(blockstore.BlockSize))
	bc.diskUsed.Add(int64(dataSize) - prevDiskSize)
	mb.mu.Unlock()

	// Return buffer to pool for reuse (avoids 8MB zeroing on next alloc).
	putBlockBuf(bufToReturn)

	return path, dataSize, nil
}

// flushOldestDirtyBlock finds the in-memory block with the oldest lastWrite
// timestamp and flushes it to disk. Returns true if a block was flushed.
// Called by WriteAt when the memory budget is exceeded (backpressure).
func (bc *FSStore) flushOldestDirtyBlock(ctx context.Context) bool {
	var oldestKey blockKey
	var oldestMB *memBlock
	var oldestTime time.Time

	bc.blocksMu.RLock()
	for key, mb := range bc.memBlocks {
		mb.mu.RLock()
		if mb.dirty && mb.data != nil {
			if oldestMB == nil || mb.lastWrite.Before(oldestTime) {
				oldestKey = key
				oldestMB = mb
				oldestTime = mb.lastWrite
			}
		}
		mb.mu.RUnlock()
	}
	bc.blocksMu.RUnlock()

	if oldestMB != nil {
		if _, _, err := bc.flushBlock(ctx, oldestKey.payloadID, oldestKey.blockIdx, oldestMB); err != nil {
			logger.Warn("cache: failed to flush oldest block", "error", err)
			return false
		}
		return true
	}
	return false
}

// syncFile opens a file and calls fsync on it.
// Used by Flush() to ensure durability on the NFS COMMIT path.
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	return err
}

// blockPath returns the cache file path for a block ID.
// Sharded: <baseDir>/<first-2-chars>/<blockID>.blk
func (bc *FSStore) blockPath(blockID string) string {
	if len(blockID) < 2 {
		return filepath.Join(bc.baseDir, blockID+".blk")
	}
	return filepath.Join(bc.baseDir, blockID[:2], blockID+".blk")
}
