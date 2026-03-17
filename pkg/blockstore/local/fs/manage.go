package fs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// SetEvictionEnabled controls whether ensureSpace can evict blocks to make room.
// When disabled (false), ensureSpace returns ErrDiskFull if the store is over its
// disk limit instead of evicting remote blocks. This is used by local-only mode
// where there is no remote store to re-fetch evicted blocks from.
//
// Defaults to true (eviction enabled).
func (bc *FSStore) SetEvictionEnabled(enabled bool) {
	bc.evictionEnabled.Store(enabled)
}

// DeleteBlockFile removes a single block (identified by payloadID + blockIdx)
// from memory, disk, and metadata.
//
// Order of operations:
//  1. Close file descriptors (fdPool + readFDPool) to release OS handles
//  2. Purge in-memory block data
//  3. Look up FileBlock metadata (to get LocalPath and DataSize)
//  4. Delete the .blk block file from disk
//  5. Decrement diskUsed counter
//  6. Delete the FileBlock record from the store (direct call, not async)
//  7. Clear any pending async update in pendingFBs to prevent zombie re-creation
//
// Returns nil if the block does not exist (idempotent).
func (bc *FSStore) DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	// 1. Close file descriptors before removing the file
	bc.fdPool.Evict(blockID)
	bc.readFDPool.Evict(blockID)

	// 2. Purge in-memory block
	bc.purgeMemBlocks(payloadID, func(idx uint64) bool {
		return idx == blockIdx
	})

	// 3. Look up FileBlock metadata for disk cleanup
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, blockstore.ErrFileBlockNotFound) {
			// Block doesn't exist -- idempotent success
			bc.pendingFBs.Delete(blockID)
			return nil
		}
		return err
	}

	// 4-5. Delete .blk file from disk and decrement diskUsed
	if fb.LocalPath != "" {
		fileSize := fileOrFallbackSize(fb.LocalPath, int64(fb.DataSize))

		if rmErr := os.Remove(fb.LocalPath); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("local store: failed to remove block file", "path", fb.LocalPath, "error", rmErr)
		}

		if fileSize > 0 {
			bc.diskUsed.Add(-fileSize)
		}
	}

	// 6. Delete FileBlock metadata from store (direct call)
	if delErr := bc.blockStore.DeleteFileBlock(ctx, blockID); delErr != nil {
		if !errors.Is(delErr, blockstore.ErrFileBlockNotFound) {
			return delErr
		}
	}

	// 7. Clear pendingFBs entry to prevent zombie re-creation
	bc.pendingFBs.Delete(blockID)

	return nil
}

// DeleteAllBlockFiles removes all blocks for a file (identified by payloadID)
// from memory, disk, and metadata.
//
// After deleting all blocks, it also:
//   - Removes the file from the files tracking map
//   - Attempts to remove the empty parent directory (ignores ENOTEMPTY)
func (bc *FSStore) DeleteAllBlockFiles(ctx context.Context, payloadID string) error {
	// List all blocks for this file from the store
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return err
	}

	// Delete each block
	for _, fb := range blocks {
		// Extract blockIdx from the block ID (format: "payloadID/blockIdx")
		blockIdx := extractBlockIdx(fb.ID, payloadID)
		if delErr := bc.DeleteBlockFile(ctx, payloadID, blockIdx); delErr != nil {
			logger.Warn("local store: failed to delete block", "blockID", fb.ID, "error", delErr)
		}
	}

	// Also purge any remaining in-memory blocks (safety net for blocks not yet
	// persisted to the store)
	bc.purgeMemBlocks(payloadID, func(uint64) bool { return true })

	// Clean up files map
	bc.filesMu.Lock()
	delete(bc.files, payloadID)
	bc.filesMu.Unlock()

	// Attempt to remove the payloadID directory (best-effort cleanup)
	// The directory is under the shard prefix: <baseDir>/<first-2-chars>/<payloadID>/
	if len(payloadID) >= 2 {
		payloadDir := filepath.Join(bc.baseDir, payloadID[:2], payloadID)
		_ = os.Remove(payloadDir) // Ignore ENOTEMPTY or ENOENT
	}

	return nil
}

// TruncateBlockFiles removes all blocks whose start offset (blockIdx * BlockSize)
// is at or beyond newSize. Blocks below newSize are kept intact.
//
// This handles the persistent storage side of truncation. The in-memory side
// is handled by Truncate() in fs.go.
func (bc *FSStore) TruncateBlockFiles(ctx context.Context, payloadID string, newSize uint64) error {
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return err
	}

	for _, fb := range blocks {
		blockIdx := extractBlockIdx(fb.ID, payloadID)
		if blockIdx*blockstore.BlockSize >= newSize {
			if delErr := bc.DeleteBlockFile(ctx, payloadID, blockIdx); delErr != nil {
				logger.Warn("local store: failed to delete truncated block", "blockID", fb.ID, "error", delErr)
			}
		}
	}

	return nil
}

// GetStoredFileSize returns the total stored data size for a file by summing
// the DataSize of all FileBlock records in the metadata store.
// Returns 0 for unknown files (no error).
func (bc *FSStore) GetStoredFileSize(ctx context.Context, payloadID string) (uint64, error) {
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return 0, err
	}

	var total uint64
	for _, fb := range blocks {
		total += uint64(fb.DataSize)
	}
	return total, nil
}

// ExistsOnDisk checks if a specific block is present on disk by verifying both
// the FileBlock metadata (LocalPath must be non-empty) and the actual file
// existence via os.Stat.
//
// Returns false for stale metadata (LocalPath set but file deleted from disk).
func (bc *FSStore) ExistsOnDisk(ctx context.Context, payloadID string, blockIdx uint64) (bool, error) {
	blockID := makeBlockID(blockKey{payloadID: payloadID, blockIdx: blockIdx})

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, blockstore.ErrFileBlockNotFound) {
			return false, nil
		}
		return false, err
	}

	if fb.LocalPath == "" {
		return false, nil
	}

	_, statErr := os.Stat(fb.LocalPath)
	return statErr == nil, nil
}

// extractBlockIdx extracts the block index from a blockID string.
// blockID format: "{payloadID}/{blockIdx}"
func extractBlockIdx(blockID, payloadID string) uint64 {
	suffix := blockID[len(payloadID)+1:] // skip "payloadID/"
	idx, _ := strconv.ParseUint(suffix, 10, 64)
	return idx
}
