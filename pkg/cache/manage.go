package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// SetEvictionEnabled controls whether ensureSpace can evict blocks to make room.
// When disabled (false), ensureSpace returns ErrDiskFull if the cache is over its
// disk limit instead of evicting remote blocks. This is used by local-only mode
// where there is no remote store to re-fetch evicted blocks from.
//
// Defaults to true (eviction enabled).
func (bc *BlockCache) SetEvictionEnabled(enabled bool) {
	bc.evictionEnabled.Store(enabled)
}

// DeleteBlockFile removes a single block (identified by payloadID + blockIdx)
// from memory, disk, and metadata.
//
// Order of operations:
//  1. Close file descriptors (fdCache + readFDCache) to release OS handles
//  2. Purge in-memory block data
//  3. Look up FileBlock metadata (to get CachePath and DataSize)
//  4. Delete the .blk cache file from disk
//  5. Decrement diskUsed counter
//  6. Delete the FileBlock record from the store (direct call, not async)
//  7. Clear any pending async update in pendingFBs to prevent zombie re-creation
//
// Returns nil if the block does not exist (idempotent).
func (bc *BlockCache) DeleteBlockFile(ctx context.Context, payloadID string, blockIdx uint64) error {
	key := blockKey{payloadID: payloadID, blockIdx: blockIdx}
	blockID := makeBlockID(key)

	// 1. Close file descriptors before removing the file
	bc.fdCache.Evict(blockID)
	bc.readFDCache.Evict(blockID)

	// 2. Purge in-memory block
	bc.purgeMemBlocks(payloadID, func(idx uint64) bool {
		return idx == blockIdx
	})

	// 3. Look up FileBlock metadata for disk cleanup
	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, metadata.ErrFileBlockNotFound) {
			// Block doesn't exist -- idempotent success
			bc.pendingFBs.Delete(blockID)
			return nil
		}
		return err
	}

	// 4-5. Delete .blk file from disk and decrement diskUsed
	if fb.CachePath != "" {
		fileSize := fileOrFallbackSize(fb.CachePath, int64(fb.DataSize))

		if rmErr := os.Remove(fb.CachePath); rmErr != nil {
			if !os.IsNotExist(rmErr) {
				return fmt.Errorf("cache: failed to remove block file %s: %w", fb.CachePath, rmErr)
			}
			// File already gone — skip diskUsed adjustment
		} else if fileSize > 0 {
			bc.diskUsed.Add(-fileSize)
		}
	}

	// 6. Clear pendingFBs entry unconditionally BEFORE store delete to prevent
	// zombie re-creation by the async syncer if the store delete fails.
	bc.pendingFBs.Delete(blockID)

	// 7. Delete FileBlock metadata from store (direct call)
	if delErr := bc.blockStore.DeleteFileBlock(ctx, blockID); delErr != nil {
		if !errors.Is(delErr, metadata.ErrFileBlockNotFound) {
			return delErr
		}
	}

	return nil
}

// DeleteAllBlockFiles removes all blocks for a file (identified by payloadID)
// from memory, disk, and metadata.
//
// After deleting all blocks, it also:
//   - Removes the file from the files tracking map
//   - Attempts to remove the empty parent directory (ignores ENOTEMPTY)
func (bc *BlockCache) DeleteAllBlockFiles(ctx context.Context, payloadID string) error {
	// List all blocks for this file from the store
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return err
	}

	// Delete each block (best-effort: try all, return first error)
	var firstErr error
	for _, fb := range blocks {
		// Extract blockIdx from the block ID (format: "payloadID/blockIdx")
		blockIdx, ok := extractBlockIdx(fb.ID, payloadID)
		if !ok {
			logger.Warn("cache: skipping malformed block ID", "blockID", fb.ID)
			continue
		}
		if delErr := bc.DeleteBlockFile(ctx, payloadID, blockIdx); delErr != nil {
			logger.Warn("cache: failed to delete block", "blockID", fb.ID, "error", delErr)
			if firstErr == nil {
				firstErr = delErr
			}
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

	return firstErr
}

// TruncateBlockFiles removes all blocks whose start offset (blockIdx * BlockSize)
// is at or beyond newSize. Blocks below newSize are kept intact.
//
// This handles the persistent storage side of truncation. The in-memory side
// is handled by Truncate() in cache.go.
func (bc *BlockCache) TruncateBlockFiles(ctx context.Context, payloadID string, newSize uint64) error {
	blocks, err := bc.blockStore.ListFileBlocks(ctx, payloadID)
	if err != nil {
		return err
	}

	var firstErr error
	for _, fb := range blocks {
		blockIdx, ok := extractBlockIdx(fb.ID, payloadID)
		if !ok {
			logger.Warn("cache: skipping malformed block ID during truncate", "blockID", fb.ID)
			continue
		}
		if blockIdx*BlockSize >= newSize {
			if delErr := bc.DeleteBlockFile(ctx, payloadID, blockIdx); delErr != nil {
				logger.Warn("cache: failed to delete truncated block", "blockID", fb.ID, "error", delErr)
				if firstErr == nil {
					firstErr = delErr
				}
			}
		}
	}

	return firstErr
}

// GetStoredFileSize returns the total stored data size for a file by summing
// the DataSize of all FileBlock records in the metadata store.
// Returns 0 for unknown files (no error).
func (bc *BlockCache) GetStoredFileSize(ctx context.Context, payloadID string) (uint64, error) {
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
// the FileBlock metadata (CachePath must be non-empty) and the actual file
// existence via os.Stat.
//
// Returns false for stale metadata (CachePath set but file deleted from disk).
func (bc *BlockCache) ExistsOnDisk(ctx context.Context, payloadID string, blockIdx uint64) (bool, error) {
	blockID := makeBlockID(blockKey{payloadID: payloadID, blockIdx: blockIdx})

	fb, err := bc.lookupFileBlock(ctx, blockID)
	if err != nil {
		if errors.Is(err, metadata.ErrFileBlockNotFound) {
			return false, nil
		}
		return false, err
	}

	if fb.CachePath == "" {
		return false, nil
	}

	_, statErr := os.Stat(fb.CachePath)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil
		}
		return false, fmt.Errorf("cache: stat block file %s: %w", fb.CachePath, statErr)
	}
	return true, nil
}

// extractBlockIdx extracts the block index from a blockID string.
// blockID format: "{payloadID}/{blockIdx}"
// Returns (blockIdx, true) on success, (0, false) if the format is invalid.
func extractBlockIdx(blockID, payloadID string) (uint64, bool) {
	prefix := payloadID + "/"
	if !strings.HasPrefix(blockID, prefix) || len(blockID) <= len(prefix) {
		return 0, false
	}
	suffix := blockID[len(prefix):]
	idx, err := strconv.ParseUint(suffix, 10, 64)
	if err != nil {
		return 0, false
	}
	return idx, true
}
