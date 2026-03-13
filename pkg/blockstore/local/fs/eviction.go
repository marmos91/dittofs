package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ensureSpace makes room for the given number of bytes by evicting remote blocks.
// Eviction behavior depends on the retention policy:
//   - pin: never evict, return ErrDiskFull if over limit
//   - ttl: only evict blocks whose file last-access exceeds retentionTTL
//   - lru: evict least-recently-accessed blocks first (default)
//
// Uses backpressure: waits up to 30s for syncs to make blocks evictable.
// When evictionEnabled is false, returns ErrDiskFull immediately if over limit
// instead of attempting eviction (used by local-only mode with no remote store).
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	ret := bc.getRetention()

	// Pin mode or eviction disabled (local-only with no remote store):
	// never evict, just check available space.
	if ret.policy == blockstore.RetentionPin || !bc.evictionEnabled.Load() {
		if bc.diskUsed.Load()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	// TTL mode with invalid TTL: treat as non-evictable (same as pin).
	if ret.policy == blockstore.RetentionTTL && ret.ttl <= 0 {
		if bc.diskUsed.Load()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	const maxWait = 30 * time.Second
	deadline := time.Now().Add(maxWait)
	recalculated := false
	// Fetch eviction candidates once to avoid repeated full scans.
	// Refreshed only after backpressure waits (new blocks may become evictable).
	var candidates []*blockstore.FileBlock

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		// Fetch or refresh candidate list.
		if candidates == nil {
			var err error
			candidates, err = bc.blockStore.ListRemoteBlocks(ctx, 0)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				candidates = nil // will retry after backpressure
			}
		}

		var evicted bool
		switch ret.policy {
		case blockstore.RetentionTTL:
			evicted, candidates = bc.evictOneTTL(ctx, candidates, ret.ttl)
		default: // LRU
			evicted, candidates = bc.evictOneLRU(ctx, candidates)
		}

		// Propagate context cancellation immediately instead of entering backpressure.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if !evicted {
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
				candidates = nil // refresh after backpressure wait
				continue
			}
		}
	}

	return nil
}

// evictOneTTL picks the oldest TTL-expired block from candidates, evicts it,
// and returns the remaining candidates. Operates on a pre-fetched list to
// avoid repeated ListRemoteBlocks scans.
func (bc *FSStore) evictOneTTL(ctx context.Context, candidates []*blockstore.FileBlock, ttl time.Duration) (bool, []*blockstore.FileBlock) {
	if len(candidates) == 0 {
		return false, nil
	}

	threshold := time.Now().Add(-ttl)
	accessTimes := bc.accessTracker.FileAccessTimes()

	// Find the oldest TTL-expired block.
	oldestIdx := -1
	var oldestTime time.Time

	for i, fb := range candidates {
		lastAccess := resolveAccessTime(accessTimes, fb)
		if lastAccess.Before(threshold) && (oldestIdx < 0 || lastAccess.Before(oldestTime)) {
			oldestIdx = i
			oldestTime = lastAccess
		}
	}

	if oldestIdx < 0 {
		return false, candidates
	}

	if err := bc.evictBlock(ctx, candidates[oldestIdx]); err != nil {
		logger.Warn("cache: TTL eviction failed", "blockID", candidates[oldestIdx].ID, "error", err)
		return false, candidates
	}

	// Remove evicted entry from candidate list.
	candidates = append(candidates[:oldestIdx], candidates[oldestIdx+1:]...)
	return true, candidates
}

// evictOneLRU picks the least-recently-accessed block from candidates, evicts it,
// and returns the remaining candidates. Operates on a pre-fetched list to
// avoid repeated ListRemoteBlocks scans.
func (bc *FSStore) evictOneLRU(ctx context.Context, candidates []*blockstore.FileBlock) (bool, []*blockstore.FileBlock) {
	if len(candidates) == 0 {
		return false, nil
	}

	accessTimes := bc.accessTracker.FileAccessTimes()

	// Find the least-recently-accessed block.
	oldestIdx := 0
	oldestTime := resolveAccessTime(accessTimes, candidates[0])
	for i, fb := range candidates[1:] {
		t := resolveAccessTime(accessTimes, fb)
		if t.Before(oldestTime) {
			oldestIdx = i + 1
			oldestTime = t
		}
	}

	if err := bc.evictBlock(ctx, candidates[oldestIdx]); err != nil {
		logger.Warn("cache: LRU eviction failed", "blockID", candidates[oldestIdx].ID, "error", err)
		return false, candidates
	}

	// Remove evicted entry from candidate list.
	candidates = append(candidates[:oldestIdx], candidates[oldestIdx+1:]...)
	return true, candidates
}

// extractPayloadID extracts the payloadID from a blockID (format: "payloadID/blockIdx").
func extractPayloadID(blockID string) string {
	if idx := strings.LastIndex(blockID, "/"); idx >= 0 {
		return blockID[:idx]
	}
	return blockID
}

// resolveAccessTime returns the last-access time for a block's file, checking the
// access tracker first and falling back to the FileBlock's own LastAccess field.
func resolveAccessTime(accessTimes map[string]time.Time, fb *blockstore.FileBlock) time.Time {
	payloadID := extractPayloadID(fb.ID)
	if t, ok := accessTimes[payloadID]; ok {
		return t
	}
	return fb.LastAccess
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
