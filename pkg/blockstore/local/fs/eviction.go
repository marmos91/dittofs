package fs

import (
	"context"
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
//
// Candidates are gathered from the in-process diskIndex rather than from
// metadata-backend queries (TD-02d / D-19). The diskIndex is populated by the
// write hot path (flushBlock, tryDirectDiskWrite), WriteFromRemote, and
// Recover() so it reflects the authoritative on-disk state.
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
	// Gather eviction candidates from the in-process diskIndex once to avoid
	// repeated full scans. Refreshed only after backpressure waits (state
	// transitions may turn additional blocks Remote and therefore evictable).
	var candidates []*blockstore.FileBlock

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		// Fetch or refresh candidate list from diskIndex.
		if candidates == nil {
			candidates = bc.collectRemoteCandidates()
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

// collectRemoteCandidates returns all Remote-state blocks tracked in the
// in-process diskIndex. Replaces the metadata-backend ListRemoteBlocks query
// that previously drove eviction — decisions are now derived entirely from
// on-disk state (TD-02d / D-19).
func (bc *FSStore) collectRemoteCandidates() []*blockstore.FileBlock {
	var out []*blockstore.FileBlock
	bc.diskIndex.Range(func(_, v any) bool {
		fb := v.(*blockstore.FileBlock)
		if fb.State == blockstore.BlockStateRemote && fb.LocalPath != "" {
			out = append(out, fb)
		}
		return true
	})
	return out
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
		logger.Warn("local store: TTL eviction failed", "blockID", candidates[oldestIdx].ID, "error", err)
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
		logger.Warn("local store: LRU eviction failed", "blockID", candidates[oldestIdx].ID, "error", err)
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

// evictBlock removes a block's local file and clears its LocalPath. The
// metadata update is NOT persisted synchronously to the metadata backend —
// doing so would couple eviction to the backend (TD-02d / D-19). Instead:
//   - the file is removed from disk (authoritative state change),
//   - the in-process diskIndex entry is pruned (eviction-visible immediately),
//   - the mutated block record is queued in pendingFBs for the background
//     SyncFileBlocks drainer to persist eventually.
//
// Consumers that need the updated metadata synchronously (e.g., test
// harnesses) can call SyncFileBlocks to flush pendingFBs on demand.
func (bc *FSStore) evictBlock(_ context.Context, fb *blockstore.FileBlock) error {
	if fb.LocalPath == "" {
		return nil
	}

	fileSize := fileOrFallbackSize(fb.LocalPath, int64(fb.DataSize))
	localPath := fb.LocalPath

	// Close any cached file descriptors so the unlink actually reclaims disk
	// space on platforms where an open fd keeps the inode alive.
	bc.fdPool.Evict(fb.ID)
	bc.readFDPool.Evict(fb.ID)

	if err := os.Remove(localPath); err != nil && !os.IsNotExist(err) {
		return err
	}

	if fileSize > 0 {
		bc.diskUsed.Add(-fileSize)
	}

	// Update in-memory metadata + schedule async persistence. Removing from
	// diskIndex ensures subsequent eviction scans and hot-path lookups do not
	// see this block as locally present.
	fb.LocalPath = ""
	bc.pendingFBs.Store(fb.ID, fb)
	bc.diskIndexDelete(fb.ID)

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

// recalcDiskUsed walks the block store directory and recalculates diskUsed.
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
