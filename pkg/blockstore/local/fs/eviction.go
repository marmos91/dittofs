package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ensureSpace makes room for the given number of bytes by evicting remote blocks.
// Uses backpressure: waits up to 30s for syncs to make blocks evictable.
// When evictionEnabled is false, returns ErrDiskFull immediately if over limit
// instead of attempting eviction (used by local-only mode with no remote store).
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
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
			case <-time.After(100 * time.Millisecond):
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
func (bc *FSStore) evictBlock(ctx context.Context, fb *blockstore.FileBlock) error {
	if fb.CachePath == "" {
		return nil
	}

	fileSize := fileOrFallbackSize(fb.CachePath, int64(fb.DataSize))
	cachePath := fb.CachePath

	fb.CachePath = ""
	if err := bc.blockStore.PutFileBlock(ctx, fb); err != nil {
		return fmt.Errorf("update block metadata: %w", err)
	}

	if err := os.Remove(cachePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove cache file: %w", err)
	}

	if fileSize > 0 {
		bc.diskUsed.Add(-fileSize)
	}

	return nil
}

// fileOrFallbackSize returns the file's actual size on disk, falling back to
// fallback if os.Stat fails (e.g., file already deleted).
func fileOrFallbackSize(path string, fallback int64) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return fallback
}

// recalcDiskUsed walks the cache directory and recalculates diskUsed.
func (bc *FSStore) recalcDiskUsed() {
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
