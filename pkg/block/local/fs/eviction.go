package fs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
)

// ensureSpace makes room for the given number of bytes by evicting CAS chunks
// from the in-process LRU. Eviction order is least-recently-
// used; the picked chunk file is unlinked from blocks/{hh}/{hh}/{hex} and
// bc.diskUsed is decremented atomically.
//
// Critical invariant (LSL-08): the eviction path must NOT consult the
// FileBlockStore (the engine-level metadata store). On the write hot
// path, eviction relies on on-disk presence and the in-process LRU index
// for its accounting. Future changes to the engine API must not leak
// FileBlockStore calls back into local storage decisions.
//
// It MAY, however, consult the SyncedHashStore — a distinct, narrow
// interface that answers only per-hash sync state (IsSynced). lruEvictOne
// uses it to refuse evicting an unsynced chunk before its first mirror
// (evicting one destroys the only copy). SyncedHashStore is NOT the
// FileBlockStore and is not covered by LSL-08; do not collapse the two
// when editing this path.
//
// Pin mode and the eviction-disabled flag short-circuit to ErrDiskFull
// without touching the LRU. Retention TTL with a non-positive duration
// behaves the same way: retention policy can keep blocks around
// regardless of LRU position.
//
// Concurrent ReadChunk that races an evict surfaces as
// block.ErrChunkNotFound; the engine refetches from CAS
// (accept/refetch posture).
func (bc *FSStore) ensureSpace(ctx context.Context, needed int64) error {
	if bc.maxDisk <= 0 {
		return nil
	}

	ret := bc.getRetention()

	// Pin mode or eviction disabled: never evict, just check available space.
	if ret.policy == block.RetentionPin || !bc.evictionEnabled.Load() {
		if bc.diskUsed.Load()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	// TTL mode with invalid TTL: treat as non-evictable (same as pin).
	if ret.policy == block.RetentionTTL && ret.ttl <= 0 {
		if bc.diskUsed.Load()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	maxWait := bc.evictMaxWait
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	deadline := time.Now().Add(maxWait)

	// Backpressure stall bookkeeping. engaged is set the first time we hit
	// errLRUEmpty with a healthy remote (the remote-cache stall path) so we
	// log the eventual RELEASE exactly once and account the stall duration.
	var (
		engaged    bool
		stallStart time.Time
	)
	release := func(reason string) {
		if !engaged {
			return
		}
		stall := time.Since(stallStart)
		bc.bpStallNanos.Add(int64(stall))
		if bc.bpLogLimiter == nil || bc.bpLogLimiter.Allow() {
			logger.Info("local cache backpressure released",
				"store", bc.baseDir,
				"reason", reason,
				"disk_used", bc.diskUsed.Load(),
				"max_disk", bc.maxDisk,
				"unsynced_bytes", bc.unsyncedBytesOrZero(),
				"remote_healthy", bc.remoteHealthyOrTrue(),
				"stall_ms", stall.Milliseconds())
		}
		engaged = false
	}

	for bc.diskUsed.Load()+needed > bc.maxDisk {
		freed, err := bc.lruEvictOne(ctx)
		if errors.Is(err, errLRUEmpty) {
			// No more LRU candidates: every cached chunk is still unsynced.
			// Branch on whether a remote-backed syncer is wired:
			//
			//   - remote-backed (bpSource != nil): the local tier is a
			//     write-through cache. If the remote is HEALTHY the syncer
			//     can still drain unsynced chunks and free space, so engage
			//     backpressure and stall up to the (longer) backpressure
			//     window. If the remote is UNHEALTHY the syncer cannot
			//     drain, so fail fast with ErrDiskFull rather than stalling
			//     a writer that cannot make progress.
			//   - local-only (bpSource == nil): keep the legacy behavior —
			//     wait the shorter evictMaxWait for new evictable chunks
			//     (async StoreChunk from the rollup pool) to land.
			if bc.bpSource != nil {
				if !bc.bpSource.IsRemoteHealthy() {
					// Remote cannot drain: fail fast (release if we had
					// been stalling on a previously-healthy remote).
					if engaged {
						release("remote_unhealthy")
					}
					return ErrDiskFull
				}
				if !engaged {
					// First time stalling on the remote-cache path for this
					// request: arm the longer deadline, count the engage, and
					// log it (rate-limited).
					maxWait := bc.effectiveBackpressureMaxWait()
					engaged = true
					stallStart = time.Now()
					deadline = stallStart.Add(maxWait)
					bc.bpEngageCount.Add(1)
					if bc.bpLogLimiter == nil || bc.bpLogLimiter.Allow() {
						logger.Info("local cache backpressure engaged: waiting for syncer to drain",
							"store", bc.baseDir,
							"disk_used", bc.diskUsed.Load(),
							"max_disk", bc.maxDisk,
							"needed", needed,
							"unsynced_bytes", bc.bpSource.UnsyncedBytes(),
							"remote_healthy", true,
							"max_wait_ms", maxWait.Milliseconds())
					}
				}
			}

			if time.Now().After(deadline) {
				release("window_exceeded")
				return ErrDiskFull
			}
			select {
			case <-ctx.Done():
				release("ctx_cancelled")
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
			release("ctx_cancelled")
			return ctx.Err()
		}
	}

	release("space_freed")
	return nil
}

// effectiveBackpressureMaxWait returns the remote-cache stall window,
// defaulting to 60s when unset.
func (bc *FSStore) effectiveBackpressureMaxWait() time.Duration {
	if bc.backpressureMaxWait > 0 {
		return bc.backpressureMaxWait
	}
	return 60 * time.Second
}

// unsyncedBytesOrZero reads the syncer's unsynced-byte counter, returning 0
// when no syncer is wired (local-only / fixtures).
func (bc *FSStore) unsyncedBytesOrZero() int64 {
	if bc.bpSource == nil {
		return 0
	}
	return bc.bpSource.UnsyncedBytes()
}

// remoteHealthyOrTrue reports remote health, defaulting to true when no
// syncer is wired (local-only stores have no remote to be unhealthy).
func (bc *FSStore) remoteHealthyOrTrue() bool {
	if bc.bpSource == nil {
		return true
	}
	return bc.bpSource.IsRemoteHealthy()
}
