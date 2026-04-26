package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// ensureSpace makes room for the given number of bytes by evicting CAS chunks
// from the in-process LRU (LSL-08, D-27). Eviction order is least-recently-
// used; the picked chunk file is unlinked from blocks/{hh}/{hh}/{hex} and
// bc.diskUsed is decremented atomically.
//
// Critical invariant: the eviction path does NOT consult the metadata
// store. On the write hot path, eviction must rely entirely on on-disk
// presence and the in-process LRU index — Phase 12+ may change the
// engine API without leaking back into local storage decisions
// (T-11-B-11).
//
// Pin mode and the eviction-disabled flag short-circuit to ErrDiskFull
// without touching the LRU. Retention TTL with a non-positive duration
// behaves the same way (matches the pre-LSL-08 contract that retention
// policy can keep blocks around regardless of LRU position).
//
// Concurrent ReadChunk that races an evict surfaces as
// blockstore.ErrChunkNotFound and the engine refetches from CAS
// (T-11-B-08, accept/refetch posture).
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	ret := bc.getRetention()

	// Pin mode or eviction disabled: never evict, just check available space.
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

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		freed, err := bc.lruEvictOne()
		if errors.Is(err, errLRUEmpty) {
			// No more LRU candidates. Wait briefly for new chunks to land
			// (e.g., async StoreChunk in the rollup pool) up to the
			// deadline before giving up.
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
		if err != nil {
			return fmt.Errorf("ensureSpace: %w", err)
		}
		bc.diskUsed.Add(-freed)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}

	return nil
}

// fileOrFallbackSize returns the file's actual size on disk, falling back
// to fallback if os.Stat fails (e.g., file already deleted). Retained for
// callers in manage.go that delete legacy .blk files outside the LRU.
func fileOrFallbackSize(path string, fallback int64) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return fallback
}
