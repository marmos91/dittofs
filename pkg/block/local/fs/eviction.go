package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/local/logblob"
)

// ensureSpace makes room for the given number of bytes by evicting CAS chunks
// from the in-process LRU. Eviction order is least-recently-
// used; the picked chunk file is unlinked from blocks/{hh}/{hh}/{hex} and
// bc.diskUsed is decremented atomically.
//
// Critical invariant (LSL-08): the eviction path must NOT consult the
// FileChunkStore (the engine-level metadata store). On the write hot
// path, eviction relies on on-disk presence and the in-process LRU index
// for its accounting. Future changes to the engine API must not leak
// FileChunkStore calls back into local storage decisions.
//
// It MAY, however, consult the SyncedHashStore — a distinct, narrow
// interface that answers only per-hash sync state (IsSynced). lruEvictOne
// uses it to refuse evicting an unsynced chunk before its first mirror
// (evicting one destroys the only copy). SyncedHashStore is NOT the
// FileChunkStore and is not covered by LSL-08; do not collapse the two
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
		if bc.usedBytes()+needed > bc.maxDisk {
			return ErrDiskFull
		}
		return nil
	}

	// TTL mode with invalid TTL: treat as non-evictable (same as pin).
	if ret.policy == block.RetentionTTL && ret.ttl <= 0 {
		if bc.usedBytes()+needed > bc.maxDisk {
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
		if rec := bc.recordMetrics(); rec != nil {
			rec.RecordBackpressure(stall)
		}
		if bc.bpLogLimiter == nil || bc.bpLogLimiter.Allow() {
			logger.Info("local cache backpressure released",
				"store", bc.baseDir,
				"reason", reason,
				"disk_used", bc.usedBytes(),
				"max_disk", bc.maxDisk,
				"unsynced_bytes", bc.unsyncedBytesOrZero(),
				"remote_healthy", bc.remoteHealthyOrTrue(),
				"stall_ms", stall.Milliseconds())
		}
		engaged = false
	}

	for bc.usedBytes()+needed > bc.maxDisk {
		freed, err := bc.lruEvictOne(ctx)
		if errors.Is(err, errLRUEmpty) {
			// CAS LRU exhausted: fall back to whole-blob eviction of the
			// oldest sealed, fully-synced log blob (#1527 — post blocks-flip
			// the bulk of the local tier lives in log blobs, invisible to the
			// per-chunk LRU). blobEvictOne decrements logBlobDiskUsed itself.
			// Like the lruEvictOne success path, do NOT release() here — the
			// stall (if engaged) ends once the loop exits, keeping the
			// engage/stall bookkeeping to one segment per ensureSpace call.
			if bfreed, berr := bc.blobEvictOne(ctx); berr == nil && bfreed > 0 {
				continue
			} else if berr != nil && !errors.Is(berr, errLRUEmpty) {
				return fmt.Errorf("ensureSpace: %w", berr)
			}

			// No evictable CAS chunk or sealed blob: every remaining byte is
			// still unsynced (or lives in the active blob).
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
							"disk_used", bc.usedBytes(),
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
		bc.subUsed(&bc.diskUsed, freed, "cas")
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

// blobEvictOne evicts the oldest sealed log blob whose bytes are all mirrored
// remotely, freeing its physical file. Returns the freed byte count (already
// subtracted from logBlobDiskUsed), or 0 + errLRUEmpty when there is no
// evictable blob (no substrate, only the active blob left, or the oldest
// sealed blob still holds unsynced chunks — blobs are strictly time-ordered,
// so if the oldest is unsynced, newer ones are too).
//
// Sync check: chunks appended in this process lifetime are recorded in
// blobChunks and verified per-hash via the SyncedHashStore (same source
// lruEvictOne uses; matching its semantics, a nil SyncedHashStore means every
// chunk is evictable — production local-only stores are protected by the
// evictionEnabled gate). Blobs from a previous process have no record and
// fall back to the coarse global gate: evictable only when the syncer reports
// zero unsynced bytes anywhere.
//
// After eviction the recorded index entries are pruned eagerly (skipping
// hashes re-staged into a newer blob); entries for pre-restart blobs are
// dropped lazily by ReadChunk when the read surfaces ErrEvicted /
// ErrBlobNotFound.
func (bc *FSStore) blobEvictOne(ctx context.Context) (int64, error) {
	if bc.logBlob == nil {
		return 0, errLRUEmpty
	}

	// Serialize the whole scan→evict→decrement sequence: EvictBlob returns
	// nil for an already-evicted blob, so without this two concurrent
	// callers could account the same blob twice (see blobEvictMu field doc).
	bc.blobEvictMu.Lock()
	defer bc.blobEvictMu.Unlock()

	infos, err := bc.logBlob.ListBlobs()
	if err != nil {
		return 0, fmt.Errorf("blob evict: list blobs: %w", err)
	}
	// ListBlobs is sorted by blob ID = creation order: scan oldest-first.
	for i := range infos {
		if infos[i].Active {
			continue
		}
		id := infos[i].LogBlobID
		// Already processed by this store but still on disk (unlink-failure
		// orphan, see below): its bytes stay counted; skip it.
		if _, done := bc.blobEvictedIDs[id]; done {
			continue
		}
		if !bc.blobSynced(ctx, id) {
			// Blobs are strictly time-ordered: if the oldest candidate is
			// unsynced, newer ones are too — stop scanning.
			return 0, errLRUEmpty
		}
		// The synced predicate already ran above; EvictBlob re-checks active/
		// already-evicted state itself.
		if err := bc.logBlob.EvictBlob(ctx, id, func(string) bool { return true }); err != nil {
			if errors.Is(err, logblob.ErrActiveBlob) {
				return 0, errLRUEmpty
			}
			// The blob may still have transitioned to evicted in-memory (only
			// the unlink failed). Do NOT mark it processed or adjust the
			// counter: the retry path below settles the accounting.
			return 0, fmt.Errorf("blob evict: %w", err)
		}
		// EvictBlob is idempotent-nil for a blob it already evicted, and it
		// never retries a failed unlink — so nil does NOT guarantee the file
		// is gone. Only subtract the bytes when the file has actually left
		// the disk; otherwise usedBytes would undercount physical usage.
		// An orphan's bytes stay counted until a restart re-seeds from the
		// physical files and retries the eviction with a fresh manager.
		bc.blobEvictedIDs[id] = struct{}{}
		bc.dropBlobIndexEntries(ctx, id)
		orphanPath := filepath.Join(bc.baseDir, "blobs", id+".blob")
		if _, statErr := os.Stat(orphanPath); statErr == nil || !os.IsNotExist(statErr) {
			logger.Warn("local store: evicted log blob still on disk (unlink failed earlier); keeping its bytes counted until restart",
				"blob", id, "bytes", infos[i].Size, "dir", bc.baseDir)
			continue // try the next candidate
		}
		bc.subUsed(&bc.logBlobDiskUsed, infos[i].Size, "logblob")
		if rec := bc.recordMetrics(); rec != nil {
			rec.RecordEviction(infos[i].Size)
		}
		logger.Info("local store: evicted sealed log blob",
			"blob", id, "bytes", infos[i].Size, "dir", bc.baseDir)
		return infos[i].Size, nil
	}
	return 0, errLRUEmpty
}

// blobSynced reports whether every chunk in blobID has been durably mirrored
// to the remote. See blobEvictOne for the tracked / untracked split.
func (bc *FSStore) blobSynced(ctx context.Context, blobID string) bool {
	bc.blobChunksMu.Lock()
	recorded, tracked := bc.blobChunks[blobID]
	hashes := make([]block.ContentHash, len(recorded))
	copy(hashes, recorded)
	bc.blobChunksMu.Unlock()

	if bc.syncedHashStore == nil {
		// Match lruEvictOne: without sync-state tracking every chunk is
		// considered evictable (local-only stores rely on evictionEnabled).
		return true
	}
	if !tracked {
		// Blob predates this process: no per-chunk record. Only evict when
		// the syncer reports nothing unsynced anywhere.
		return bc.bpSource != nil && bc.bpSource.UnsyncedBytes() == 0
	}
	for _, h := range hashes {
		synced, err := bc.syncedHashStore.IsSynced(ctx, h)
		if err != nil {
			logger.Warn("blob evict: IsSynced lookup failed, treating blob as unsynced",
				"blob", blobID, "hash", h.String(), "error", err)
			return false
		}
		if !synced {
			return false
		}
	}
	return true
}

// dropBlobIndexEntries removes the local-index entries recorded for blobID
// and forgets its blobChunks record. A hash whose current index entry points
// at a DIFFERENT blob was re-staged after this blob's copy became stale —
// its live entry is kept. Best-effort: a failed delete leaves a dangling
// entry that ReadChunk drops lazily (ErrEvicted → miss → remote refetch).
func (bc *FSStore) dropBlobIndexEntries(ctx context.Context, blobID string) {
	bc.blobChunksMu.Lock()
	hashes := bc.blobChunks[blobID]
	delete(bc.blobChunks, blobID)
	bc.blobChunksMu.Unlock()

	if bc.localChunkIndex == nil {
		return
	}
	for _, h := range hashes {
		loc, ok, err := bc.localChunkIndex.GetLocalLocation(ctx, h)
		if err != nil || !ok || loc.LogBlobID != blobID {
			continue
		}
		if err := bc.localChunkIndex.DeleteLocalLocation(ctx, h); err != nil {
			logger.Warn("blob evict: failed to drop index entry",
				"blob", blobID, "hash", h.String(), "error", err)
		}
	}
}

// reclaimSpace is the write-path counterpart of ensureSpace: called after a
// rollup pass lands new log-blob bytes, it evicts synced CAS chunks and
// sealed synced blobs until the store is back under maxDisk. Best-effort and
// NON-BLOCKING — when nothing is evictable right now (unsynced tail, only the
// active blob left) it returns immediately rather than stalling the rollup
// worker; the next pass retries after the syncer has drained more chunks.
// Serialized via blobReclaimActive so concurrent rollup workers do not
// stampede; losers skip (the winner is already draining to the limit).
func (bc *FSStore) reclaimSpace(ctx context.Context) {
	if bc.maxDisk <= 0 || !bc.evictionEnabled.Load() {
		return
	}
	ret := bc.getRetention()
	if ret.policy == block.RetentionPin || (ret.policy == block.RetentionTTL && ret.ttl <= 0) {
		return
	}
	if !bc.blobReclaimActive.CompareAndSwap(false, true) {
		return
	}
	defer bc.blobReclaimActive.Store(false)

	for bc.usedBytes() > bc.maxDisk {
		if ctx.Err() != nil {
			return
		}
		if freed, err := bc.lruEvictOne(ctx); err == nil {
			bc.subUsed(&bc.diskUsed, freed, "cas")
			continue
		} else if !errors.Is(err, errLRUEmpty) {
			logger.Warn("reclaimSpace: CAS eviction failed", "dir", bc.baseDir, "error", err)
			return
		}
		if _, err := bc.blobEvictOne(ctx); err != nil {
			if !errors.Is(err, errLRUEmpty) {
				logger.Warn("reclaimSpace: blob eviction failed", "dir", bc.baseDir, "error", err)
			}
			return
		}
	}
}
