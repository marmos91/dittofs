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

// ensureSpace makes room for the given number of bytes by evicting remote blocks.
// Uses backpressure: waits up to 30s for syncs to make blocks evictable.
// When evictionEnabled is false, returns ErrDiskFull immediately if over limit
// instead of attempting eviction (used by local-only mode with no remote store).
func (bc *BlockCache) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	if !bc.evictionEnabled.Load() {
		if bc.diskUsed.Load()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	const maxWait = 30 * time.Second
	deadline := time.Now().Add(maxWait)
	recalculated := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		evictable, err := bc.blockStore.ListRemoteBlocks(ctx, 1)
		if err != nil || len(evictable) == 0 {
			if !recalculated {
				recalculated = true
				bc.recalcDiskUsed()
				if bc.diskUsed.Load()+needed <= bc.maxDisk {
					break
				}
			}
			if time.Now().After(deadline) {
				return ErrDiskFull
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				continue
			}
		}

		if err := bc.evictBlock(ctx, evictable[0]); err != nil {
			logger.Warn("cache: eviction failed", "blockID", evictable[0].ID, "error", err)
			continue
		}
	}

	return nil
}

// evictBlock removes a block's cache file and clears its CachePath.
func (bc *BlockCache) evictBlock(ctx context.Context, fb *metadata.FileBlock) error {
	if fb.CachePath == "" {
		return nil
	}

	info, err := os.Stat(fb.CachePath)
	var fileSize int64
	if err == nil {
		fileSize = info.Size()
	} else {
		fileSize = int64(fb.DataSize)
	}

	cachePath := fb.CachePath

	// Remove cache file first, then update metadata. If file removal succeeds
	// but metadata update fails, the block will be re-downloaded on next access.
	// The reverse order would leave an orphaned file if metadata update succeeds
	// but removal fails.
	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cache file: %w", err)
	}

	// Decrement disk usage immediately after successful file removal, regardless
	// of whether the metadata update succeeds. The file is already gone from disk.
	if fileSize > 0 {
		bc.diskUsed.Add(-fileSize)
	}

	fb.CachePath = ""
	if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
		return fmt.Errorf("update block metadata: %w", err)
	}

	return nil
}

// recalcDiskUsed walks the cache directory and recalculates diskUsed.
func (bc *BlockCache) recalcDiskUsed() {
	var actual int64
	_ = filepath.WalkDir(bc.baseDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, infoErr := d.Info(); infoErr == nil {
			actual += info.Size()
		}
		return nil
	})
	bc.diskUsed.Store(actual)
}
