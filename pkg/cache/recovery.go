package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Recover scans the cache directory for .blk files and reconciles them with
// the FileBlockStore (BadgerDB). Called on startup to restore cache state:
//
//   - Rebuilds the in-memory files map (payloadID → fileSize) from disk
//   - Deletes orphan .blk files that have no FileBlock metadata
//   - Fixes stale CachePaths (e.g., cache directory was moved)
//   - Reverts interrupted uploads (Uploading → Sealed) for retry
func (bc *BlockCache) Recover(ctx context.Context) error {
	logger.Info("cache: starting recovery", "dir", bc.baseDir)

	var totalSize int64
	var filesFound, orphansDeleted, uploadsReverted int

	err := filepath.WalkDir(bc.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".blk") {
			return nil
		}

		filesFound++
		blockID := strings.TrimSuffix(d.Name(), ".blk")

		fb, err := bc.blockStore.GetFileBlock(ctx, blockID)
		if err != nil {
			os.Remove(path)
			orphansDeleted++
			return nil
		}

		needsUpdate := false

		// Fix cache path if it changed (e.g., moved cache directory)
		if fb.CachePath != path {
			fb.CachePath = path
			needsUpdate = true
		}

		// Blocks with a BlockStoreKey but still Dirty → already uploaded
		if fb.BlockStoreKey != "" && fb.State == metadata.BlockStateDirty {
			fb.State = metadata.BlockStateUploaded
			needsUpdate = true
		}

		// Revert interrupted uploads so they get retried
		if fb.State == metadata.BlockStateUploading {
			fb.State = metadata.BlockStateSealed
			needsUpdate = true
			uploadsReverted++
		}

		if needsUpdate {
			_ = bc.blockStore.PutFileBlock(ctx, fb)
		}

		payloadID, blockIdx := parseBlockID(blockID)
		if payloadID != "" {
			end := (blockIdx + 1) * BlockSize
			if fb.DataSize > 0 && fb.DataSize < BlockSize {
				end = blockIdx*BlockSize + uint64(fb.DataSize)
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
		"uploadsReverted", uploadsReverted,
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
