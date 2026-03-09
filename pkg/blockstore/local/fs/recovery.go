package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// Recover scans the cache directory for .blk files and reconciles them with
// the FileBlockStore (BadgerDB). Called on startup to restore cache state:
//
//   - Rebuilds the in-memory files map (payloadID -> fileSize) from disk
//   - Deletes orphan .blk files that have no FileBlock metadata
//   - Fixes stale CachePaths (e.g., cache directory was moved)
//   - Reverts interrupted syncs (Syncing -> Local) for retry
func (bc *FSStore) Recover(ctx context.Context) error {
	logger.Info("cache: starting recovery", "dir", bc.baseDir)

	var totalSize int64
	var filesFound, orphansDeleted, syncsReverted int

	err := filepath.WalkDir(bc.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".blk") {
			return nil
		}

		filesFound++

		// Extract blockID from the full path, reversing blockPath's sharding.
		// blockPath creates: <baseDir>/<shard>/<blockID>.blk where shard = blockID[:2].
		rel, relErr := filepath.Rel(bc.baseDir, path)
		if relErr != nil {
			logger.Warn("cache: recovery skipping file", "path", path, "error", relErr)
			return nil
		}
		rel = strings.TrimSuffix(rel, ".blk")
		// Remove the 2-char shard directory prefix.
		var blockID string
		if parts := strings.SplitN(rel, string(filepath.Separator), 2); len(parts) == 2 {
			blockID = parts[1]
		} else {
			blockID = rel
		}

		fb, err := bc.blockStore.GetFileBlock(ctx, blockID)
		if err != nil {
			if errors.Is(err, blockstore.ErrFileBlockNotFound) {
				if rmErr := os.Remove(path); rmErr != nil {
					logger.Warn("cache: recovery failed to remove orphan", "path", path, "error", rmErr)
				}
				orphansDeleted++
			} else {
				logger.Warn("cache: recovery skipping block due to transient error", "blockID", blockID, "error", err)
			}
			return nil
		}

		needsUpdate := false

		// Fix cache path if it changed (e.g., moved cache directory)
		if fb.CachePath != path {
			fb.CachePath = path
			needsUpdate = true
		}

		// Blocks with a BlockStoreKey but still Dirty -> already synced to remote
		if fb.BlockStoreKey != "" && fb.State == blockstore.BlockStateDirty {
			fb.State = blockstore.BlockStateRemote
			needsUpdate = true
		}

		// Revert interrupted syncs so they get retried
		if fb.State == blockstore.BlockStateSyncing {
			fb.State = blockstore.BlockStateLocal
			needsUpdate = true
			syncsReverted++
		}

		if needsUpdate {
			if putErr := bc.blockStore.PutFileBlock(ctx, fb); putErr != nil {
				logger.Warn("cache: recovery failed to update block metadata", "blockID", blockID, "error", putErr)
			}
		}

		payloadID, blockIdx := parseBlockID(blockID)
		if payloadID != "" {
			end := (blockIdx + 1) * blockstore.BlockSize
			if fb.DataSize > 0 && fb.DataSize < uint32(blockstore.BlockSize) {
				end = blockIdx*blockstore.BlockSize + uint64(fb.DataSize)
			}
			bc.updateFileSize(payloadID, end)
		}

		if info, err := d.Info(); err == nil {
			totalSize += info.Size()
		}
		return nil
	})

	if err != nil {
		return fmt.Errorf("walk cache dir: %w", err)
	}

	bc.diskUsed.Store(totalSize)

	logger.Info("cache: recovery complete",
		"filesFound", filesFound,
		"orphansDeleted", orphansDeleted,
		"syncsReverted", syncsReverted,
		"totalSize", totalSize)

	return nil
}

// parseBlockID extracts payloadID and blockIdx from a blockID ("{payloadID}/{blockIdx}").
// Returns empty payloadID if format is invalid.
func parseBlockID(blockID string) (string, uint64) {
	lastSlash := strings.LastIndex(blockID, "/")
	if lastSlash < 0 {
		return "", 0
	}
	payloadID := blockID[:lastSlash]
	idx, err := strconv.ParseUint(blockID[lastSlash+1:], 10, 64)
	if err != nil {
		return "", 0
	}
	return payloadID, idx
}
