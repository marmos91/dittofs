package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Flush writes all dirty in-memory blocks for a file to disk as .blk files.
// Called on NFS COMMIT to ensure data reaches stable storage before responding
// to the client. Each flushed block transitions to BlockStateSealed, meaning
// it is on disk and ready for the offloader to upload to S3.
func (bc *BlockCache) Flush(ctx context.Context, payloadID string) error {
	if bc.isClosed() {
		return ErrCacheClosed
	}

	bc.mu.RLock()
	var keys []blockKey
	for key := range bc.memBlocks {
		if key.payloadID == payloadID {
			keys = append(keys, key)
		}
	}
	bc.mu.RUnlock()

	for _, key := range keys {
		mb := bc.getMemBlock(key)
		if mb == nil {
			continue
		}
		mb.mu.RLock()
		isDirty := mb.dirty
		mb.mu.RUnlock()
		if isDirty {
			if err := bc.flushBlock(ctx, key.payloadID, key.blockIdx, mb); err != nil {
				return err
			}
		}
	}

	return nil
}

// flushBlock writes a single memBlock to disk as a .blk file and releases the
// 8MB memory buffer. The block transitions from Dirty → Sealed in the FileBlockStore.
//
// Lock ordering: mb.mu is released BEFORE acquiring bc.mu to avoid deadlock
// with flushOldestDirtyBlock (which iterates memBlocks under bc.mu.RLock,
// then peeks at mb.mu.RLock).
func (bc *BlockCache) flushBlock(ctx context.Context, payloadID string, blockIdx uint64, mb *memBlock) error {
	mb.mu.Lock()

	if !mb.dirty || mb.data == nil {
		mb.mu.Unlock()
		return nil
	}

	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		mb.mu.Unlock()
		return fmt.Errorf("create block dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		mb.mu.Unlock()
		return fmt.Errorf("create cache file: %w", err)
	}

	dataSize := mb.dataSize
	if _, err := f.Write(mb.data[:dataSize]); err != nil {
		f.Close()
		mb.mu.Unlock()
		return fmt.Errorf("write cache file: %w", err)
	}

	dropPageCache(f)
	f.Close()

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		fb = metadata.NewFileBlock(blockID, path)
	}
	fb.CachePath = path
	fb.DataSize = dataSize
	fb.State = metadata.BlockStateSealed
	fb.LastAccess = time.Now()
	bc.queueFileBlockUpdate(fb)

	mb.data = nil
	mb.dirty = false
	bc.memUsed.Add(-int64(BlockSize))
	bc.diskUsed.Add(int64(dataSize))
	mb.mu.Unlock()

	// Acquire bc.mu AFTER releasing mb.mu to maintain lock ordering.
	bc.mu.Lock()
	delete(bc.memBlocks, key)
	bc.mu.Unlock()

	return nil
}

// flushOldestDirtyBlock finds the in-memory block with the oldest lastWrite
// timestamp and flushes it to disk. Returns true if a block was flushed.
// Called by WriteAt when the memory budget is exceeded (backpressure).
func (bc *BlockCache) flushOldestDirtyBlock(ctx context.Context) bool {
	bc.mu.RLock()
	var oldestKey blockKey
	var oldestMB *memBlock
	var oldestTime time.Time

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
	bc.mu.RUnlock()

	if oldestMB != nil {
		if err := bc.flushBlock(ctx, oldestKey.payloadID, oldestKey.blockIdx, oldestMB); err != nil {
			logger.Warn("cache: failed to flush oldest block", "error", err)
			return false
		}
		return true
	}
	return false
}

// blockPath returns the cache file path for a block ID.
// Sharded: <baseDir>/<first-2-chars>/<blockID>.blk
func (bc *BlockCache) blockPath(blockID string) string {
	if len(blockID) < 2 {
		return filepath.Join(bc.baseDir, blockID+".blk")
	}
	return filepath.Join(bc.baseDir, blockID[:2], blockID+".blk")
}
