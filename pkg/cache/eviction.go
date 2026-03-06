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

// ensureSpace makes room for the given number of bytes by evicting uploaded blocks.
// Uses backpressure: waits up to 30s for uploads to make blocks evictable.
func (bc *BlockCache) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	const maxWait = 30 * time.Second
	deadline := time.Now().Add(maxWait)
	recalculated := false

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		evictable, err := bc.blockStore.ListEvictable(ctx, 1)
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

// recalcDiskUsed walks the cache directory and recalculates diskUsed.
func (bc *BlockCache) recalcDiskUsed() {
	var actual int64
	_ = filepath.Walk(bc.baseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		actual += info.Size()
		return nil
	})
	bc.diskUsed.Store(actual)
}
