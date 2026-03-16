package fs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ReadAt reads data from the local store at the specified file offset into dest.
//
// Two-tier lookup per block:
//  1. Memory -- checks for an unflushed memBlock (dirty write data)
//  2. Disk -- reads from .blk file via FileBlockStore metadata lookup
//
// Returns (true, nil) if all requested bytes were found locally,
// (false, nil) on miss for any block in the range.
// The caller (I/O layer) handles misses by downloading from remote.
func (bc *FSStore) ReadAt(ctx context.Context, payloadID string, dest []byte, offset uint64) (bool, error) {
	if bc.isClosed() {
		return false, ErrStoreClosed
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
			return false, nil // Block not available locally
		}

		remaining = remaining[readLen:]
		currentOffset += uint64(readLen)
	}

	// Update per-file access time for eviction ordering (batched, no I/O).
	bc.accessTracker.Touch(payloadID)

	return true, nil
}

// readFromDisk reads block data from a .blk file on disk.
// Returns (true, nil) on success, (false, nil) when block is not on disk.
//
// Optimized for random read throughput:
//   - Uses read fd pool to avoid open+close syscalls per read
//   - Skips dropPageCache (OS page cache benefits subsequent random reads)
//   - Skips LastAccess update (avoids write amplification on read path)
func (bc *FSStore) readFromDisk(ctx context.Context, payloadID string, blockIdx uint64, offset, length uint32, dest []byte) (bool, error) {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	// Fast path: try pooled read fd first (no metadata lookup needed).
	if f := bc.readFDPool.Get(blockID); f != nil {
		_, err := f.ReadAt(dest[:length], int64(offset))
		if err == nil {
			return true, nil
		}
		// Fd may be stale -- evict and fall through to slow path.
		bc.readFDPool.Evict(blockID)
	}

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, blockstore.ErrFileBlockNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("get block metadata: %w", err)
	}
	if fb.LocalPath == "" {
		return false, nil
	}
	if fb.DataSize > 0 && offset+length > fb.DataSize {
		return false, nil
	}
	path := fb.LocalPath

	// Seed access tracker from persisted LastAccess on first read after restart.
	// This ensures eviction decisions remain reasonable before the file is
	// actively touched via ReadAt/WriteAt (which calls Touch with time.Now()).
	if !fb.LastAccess.IsZero() {
		bc.accessTracker.TouchIfAbsent(payloadID, fb.LastAccess)
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("open block file: %w", err)
	}

	_, err = f.ReadAt(dest[:length], int64(offset))
	if err != nil {
		_ = f.Close()
		if err == io.EOF {
			return false, nil
		}
		return false, fmt.Errorf("pread: %w", err)
	}

	// Pool the fd for subsequent reads to this block.
	bc.readFDPool.Put(blockID, f)

	return true, nil
}
