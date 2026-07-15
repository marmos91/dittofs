package journal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// ErrLocalStoreFull is returned by the write path when the local cache is at its
// MaxLocalBytes cap and no segment can be evicted because every one is pinned by
// unsynced (dirty) records. The writer must retry after carve drains those bytes
// to the remote store; UnsyncedBytes reports how much is pinned.
var ErrLocalStoreFull = errors.New("journal: local store full, all segments pinned by unsynced bytes")

// evictBackoff is the poll interval the write path waits between eviction
// attempts while dirty-pinned. The tunable knobs are Config.MaxLocalBytes (the
// threshold) and Config.EvictMaxWait (the give-up budget); this fixed step just
// paces the wait and is not worth a config field.
const evictBackoff = 10 * time.Millisecond

// EvictResult reports what an eviction pass reclaimed.
type EvictResult struct {
	SegmentsEvicted int
	BytesFreed      int64
}

// Evict frees whole sealed segments under storage pressure, coldest first
// (approx-LRU by lastAccess). Only sealed, fully-synced segments qualify: a
// segment holding any unsynced record is skipped so eviction never destroys the
// only copy of dirty bytes (the synced-gate, mirroring logblob.EvictBlob). It
// evicts until targetBytes have been freed; targetBytes <= 0 evicts a single
// qualifying segment. It returns what it reclaimed — SegmentsEvicted == 0 means
// nothing qualified (the caller should backpressure, see ensureSpace).
func (s *Store) Evict(ctx context.Context, targetBytes int64) (EvictResult, error) {
	if err := ctx.Err(); err != nil {
		return EvictResult{}, err
	}
	if s.closed.Load() {
		return EvictResult{}, errClosed
	}
	var res EvictResult
	for {
		seg, sh := s.claimColdestEvictable()
		if seg == nil {
			return res, nil
		}
		freed, err := s.evictSegment(sh, seg)
		if err != nil {
			return res, err
		}
		res.SegmentsEvicted++
		res.BytesFreed += freed
		if targetBytes <= 0 || res.BytesFreed >= targetBytes {
			return res, nil
		}
	}
}

// claimColdestEvictable finds the coldest sealed, fully-synced, unclaimed segment
// across all shards and claims it (busy CAS) so eviction and GC never race on the
// same segment. It returns nil only when no segment qualifies. Losing the claim
// to a concurrent GC/evict does not abort the pass: that segment is now busy and
// drops out of the next scan, so the coldest remaining candidate is tried
// instead. The retry terminates because a lost CAS means the segment is busy,
// shrinking the candidate set each round.
func (s *Store) claimColdestEvictable() (*segmentMeta, *shard) {
	for {
		var (
			best       *segmentMeta
			bestShard  *shard
			bestAccess int64
		)
		for _, sh := range s.shards {
			sh.mu.Lock()
			for _, seg := range sh.sealed {
				if !evictable(seg) {
					continue
				}
				if la := seg.lastAccess.Load(); best == nil || la < bestAccess {
					best, bestShard, bestAccess = seg, sh, la
				}
			}
			sh.mu.Unlock()
		}
		if best == nil {
			return nil, nil
		}
		// The synced-gate only ever loosens for a sealed segment (carve raises
		// syncedRecords toward records; records is frozen once sealed), so the sole
		// concurrency hazard is another claimer — the CAS settles it.
		if best.busy.CompareAndSwap(false, true) {
			return best, bestShard
		}
	}
}

// evictable reports whether seg can be dropped whole: sealed, unclaimed, and with
// every record synced to the remote store. Caller holds seg's shard lock.
func evictable(seg *segmentMeta) bool {
	return seg.sealed.Load() && !seg.busy.Load() &&
		seg.syncedRecords.Load() == seg.records.Load()
}

// evictSegment removes one claimed sealed segment. Under the shard lock it
// rewrites every interval-tree entry backed by the segment to a cold marker — so
// a later read of that range fetches from the remote store instead of seeing a
// false POSIX hole, and DataExtents still reports the range as present — then
// drops the segment from the shard. Only after the index no longer references it
// are the .seg/.idx files closed and unlinked; a read that snapshotted the fd
// just before is safe against the close via os.File's own refcounting (it errors
// and the caller refetches). Caller must have claimed seg.busy.
func (s *Store) evictSegment(sh *shard, seg *segmentMeta) (int64, error) {
	sh.mu.Lock()
	for _, fi := range sh.index {
		for k := range fi.ivs {
			if fi.ivs[k].loc.SegmentID == seg.id && !fi.ivs[k].cold {
				fi.ivs[k].cold = true
			}
		}
	}
	freed := seg.tail.Load()
	delete(sh.sealed, seg.id)
	sh.mu.Unlock()

	_ = seg.close()
	if err := os.Remove(s.segPath(seg.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The segment is already gone from the index and its fd is closed —
		// eviction is one-way. The leftover file is a harmless orphan the recovery
		// sweep reclaims; report the error but do not un-evict.
		return 0, fmt.Errorf("journal: evict remove segment %d: %w", seg.id, err)
	}
	_ = os.Remove(s.idxPath(seg.id)) // best-effort: the .idx is rebuildable
	s.diskBytes.Add(-freed)
	return freed, nil
}

// ensureSpace is the write-path capacity gate. With MaxLocalBytes set, it evicts
// cold synced segments to fit the incoming write; when nothing is evictable
// because every segment is dirty-pinned, it backpressures the writer up to
// EvictMaxWait (giving carve time to drain to the remote) and finally returns
// ErrLocalStoreFull. A no-op when MaxLocalBytes is unset. Holds no lock, so the
// eviction it drives never contends with the caller's own shard.
//
// MaxLocalBytes is a soft pressure threshold, not a hard byte quota: admission
// reads diskBytes without reserving, so N concurrent writers across shards can
// each clear the gate and append before any lands, briefly overshooting the cap
// — and eviction is whole-segment anyway, so exact enforcement is neither
// possible nor the goal. The gate relieves pressure (evict) and, failing that,
// backpressures; a later writer's round evicts the overshoot. ErrLocalStoreFull
// means genuinely nothing is evictable, never mere overshoot. A hard ceiling
// would need a global reserved-bytes counter, deliberately not built (the
// eviction design is lazy and pressure-gated).
func (s *Store) ensureSpace(ctx context.Context, needed int64) error {
	if s.cfg.MaxLocalBytes <= 0 {
		return nil
	}
	deadline := time.Now().Add(s.cfg.EvictMaxWait)
	warned := false
	for s.diskBytes.Load()+needed > s.cfg.MaxLocalBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		overage := s.diskBytes.Load() + needed - s.cfg.MaxLocalBytes
		res, err := s.Evict(ctx, overage)
		if err != nil {
			return err
		}
		if res.SegmentsEvicted > 0 {
			continue
		}
		if !warned {
			logger.Warn("journal local store full: every segment pinned by unsynced bytes, backpressuring writes until carve drains to remote",
				"dir", s.dir,
				"disk_bytes", s.diskBytes.Load(),
				"max_local_bytes", s.cfg.MaxLocalBytes,
				"unsynced_bytes", s.unsynced.Load())
			warned = true
		}
		if time.Now().After(deadline) {
			return ErrLocalStoreFull
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(evictBackoff):
		}
	}
	return nil
}
