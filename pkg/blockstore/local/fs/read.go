package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ReadAt reads data from the cache at the specified file offset into dest.
//
// Two-tier lookup per block:
//  1. Memory -- checks for an unflushed memBlock (dirty write data)
//  2. Disk -- reads from .blk file via FileBlockStore metadata lookup
//
// Returns (true, nil) if all requested bytes were found in cache,
// (false, nil) on cache miss for any block in the range.
// The caller (I/O layer) handles cache misses by downloading from S3.
func (bc *FSStore) ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	if bc.isClosed() {
		return false, ErrCacheClosed
	}

	if len(dest) == 0 {
		return true, nil
	}

	remaining := dest
	currentOffset := offset

	for len(remaining) > 0 {
		blockIdx := currentOffset / blockstore.BlockSize
		blockOffset := uint32(currentOffset % blockstore.BlockSize)

		readLen := uint32(len(remaining))
		spaceInBlock := uint32(blockstore.BlockSize) - blockOffset
		if readLen > spaceInBlock {
			readLen = spaceInBlock
		}

		key := blockKey{payloadID: payloadID, blockIdx: blockIdx}

		// 1. Check memory (dirty block)
		if mb := bc.getMemBlock(key); mb != nil {
			mb.mu.RLock()
			if mb.data != nil && blockOffset+readLen <= mb.dataSize {
				copy(remaining[:readLen], mb.data[blockOffset:blockOffset+readLen])
				mb.mu.RUnlock()
				remaining = remaining[readLen:]
				currentOffset += uint64(readLen)
				continue
			}
			mb.mu.RUnlock()
		}

		// 2. Check disk (.blk file via FileBlockStore)
		found, err := bc.readFromDisk(ctx, payloadID, blockIdx, blockOffset, readLen, remaining[:readLen])
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil // Cache miss
		}

		remaining = remaining[readLen:]
		currentOffset += uint64(readLen)
	}

	return true, nil
}

// readFromDisk reads block data from a .blk file on disk.
// Returns (true, nil) on success, (false, nil) for cache miss.
//
// Optimized for random read throughput:
//   - Uses read fd cache to avoid open+close syscalls per read
//   - Skips dropPageCache (OS page cache benefits random reads)
//   - Skips LastAccess update (avoids write amplification on read path)
func (bc *FSStore) readFromDisk(ctx context.Context, payloadID string, blockIdx uint64, offset, length uint32, dest []byte) (bool, error) {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	// Fast path: try cached read fd first (no metadata lookup needed).
	if f := bc.readFDCache.Get(blockID); f != nil {
		_, err := f.ReadAt(dest[:length], int64(offset))
		if err == nil {
			return true, nil
		}
		// Fd may be stale -- evict and fall through to slow path.
		bc.readFDCache.Evict(blockID)
	}

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, blockstore.ErrFileBlockNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get block metadata: %w", err)
	}
	if fb.CachePath == "" {
		return false, nil
	}
	if fb.DataSize > 0 && offset+length > fb.DataSize {
		return false, nil
	}
	path := fb.CachePath

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open cache file: %w", err)
	}

	_, err = f.ReadAt(dest[:length], int64(offset))
	if err != nil {
		_ = f.Close()
		if err == io.EOF {
			return false, nil
		}
		return false, fmt.Errorf("pread: %w", err)
	}

	// Cache the fd for subsequent reads to this block.
	bc.readFDCache.Put(blockID, f)

	return true, nil
}
