package journal

import (
	"context"
)

// Garbage collection / repack.
//
// deadBytes on a segmentMeta grows whenever an interval-tree node is superseded
// by a newer write (segment.go) or a file is tombstoned (store.go Delete). GC
// picks the sealed segment carrying the most garbage and repacks its still-live
// records into a fresh sealed segment, then unlinks the source, reclaiming the
// dead space. GCDeadRatioForce bounds space amplification: any segment at or
// above that dead fraction is repacked without waiting for a scheduler.
//
// Repack is strictly LOCAL: it relocates cache bytes between segments and never
// touches the remote store. Remote-block refcount reclamation stays with the
// block-GC sweep (pkg/block/gc/gc_block.go), whose per-remote
// serialization is what makes a decrement safe — the union BlockReclaimer
// decrement is unsafe under a concurrent GC (its sweeper lock is per-GCStateRoot,
// not per-remote), so journal GC must never drive it. A tombstone here only drops
// LOCAL records and marks LOCAL space dead; the deleted file's remote blocks are
// freed later by that sweep, off this path.

// gcOptions selects a GC pass's aggressiveness.
type gcOptions struct {
	// Force repacks the single highest-dead-ratio sealed segment in each shard
	// even when its ratio is below GCDeadRatioForce (an explicit / test trigger).
	// Without it, a pass repacks every segment at or above the force threshold.
	Force bool
}

// gcResult reports what a GC pass reclaimed.
type gcResult struct {
	SegmentsRepacked int
	BytesReclaimed   int64 // net local bytes freed (victim size minus relocated live)
}

// GC repacks sealed segments whose dead-byte fraction has grown, freeing the
// space superseded writes and tombstones left behind. It is safe to call from a
// background loop or explicitly; passes serialize on gcMu.
func (s *Store) gc(ctx context.Context, opts gcOptions) (gcResult, error) {
	if err := ctx.Err(); err != nil {
		return gcResult{}, err
	}
	if s.closed.Load() {
		return gcResult{}, errClosed
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()

	var res gcResult
	for _, sh := range s.shards {
		reclaimed, count, err := s.gcShard(ctx, sh, opts)
		res.SegmentsRepacked += count
		res.BytesReclaimed += reclaimed
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// gcShard repacks a shard's qualifying sealed segments. It holds the shard's
// flushMu for the whole pass so carve — which flips synced bits by record offset
// — never runs against a segment repack is relocating (the same segment-busy
// discipline eviction takes).
func (s *Store) gcShard(ctx context.Context, sh *shard, opts gcOptions) (reclaimed int64, count int, err error) {
	sh.flushMu.Lock()
	defer sh.flushMu.Unlock()

	// Live bytes per segment are summed once for the whole pass instead of per
	// victim: rebuilding them means walking every interval of every file under
	// the same lock the read path takes, and a repack only changes the two
	// segments it touches. repackSegment keeps the map in step as it goes.
	live := shardLiveBytes(sh)

	// Bounded: each repack removes one victim from the sealed set, and the fresh
	// target it writes carries no dead bytes, so it is never re-picked.
	for {
		if err := ctx.Err(); err != nil {
			return reclaimed, count, err
		}
		victim := s.pickVictim(sh, opts, live)
		if victim == nil {
			return reclaimed, count, nil
		}
		// Claim the victim so a concurrent eviction can't drop it mid-repack
		// (eviction skips busy segments; a lost claim just re-picks the next).
		if !victim.busy.CompareAndSwap(false, true) {
			continue
		}
		net, err := s.repackSegment(sh, victim, live)
		victim.busy.Store(false)
		if err != nil {
			return reclaimed, count, err
		}
		reclaimed += net
		count++
		if opts.Force {
			// Force targets the single worst offender, not the whole shard.
			return reclaimed, count, nil
		}
	}
}

// shardLiveBytes sums each segment's still-live (non-cold) indexed bytes in one
// pass over the shard's interval index. Cold intervals are excluded: their bytes
// no longer occupy the segment, and counting them would make an evicted segment
// look live. Caller holds no lock.
func shardLiveBytes(sh *shard) map[uint64]int64 {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	live := make(map[uint64]int64, len(sh.sealed))
	for _, fi := range sh.index {
		for _, iv := range fi.ivs {
			if !iv.cold {
				live[iv.loc.SegmentID] += iv.length
			}
		}
	}
	return live
}

// pickVictim returns the sealed segment with the highest dead fraction, or nil
// if none qualifies. A segment carrying no dead bytes is never a victim (there
// is nothing to reclaim), which is also what keeps a tombstone-only segment from
// being repacked into an identical one forever. live carries each segment's
// still-live indexed bytes, summed authoritatively from the interval index
// (robust to the recovery-time deadBytes approximation and to the extra dead a
// crash-during-repack leaves behind); a segment's dead fraction is
// dead/occupied. Without Force, a victim must reach GCDeadRatioForce.
func (s *Store) pickVictim(sh *shard, opts gcOptions, live map[uint64]int64) *segmentMeta {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.sealed) == 0 {
		return nil
	}

	var best *segmentMeta
	var bestRatio float64
	for id, seg := range sh.sealed {
		if seg.busy.Load() {
			continue // claimed by eviction or an in-flight repack
		}
		if seg.corrupt.Load() {
			continue // a previous repack could not verify its records; leave it intact
		}
		if s.pinned(seg) {
			// Pinned by a live snapshot: repack relocates bytes but a crash mid-move
			// plus a rollback needs the pinned records intact at their original
			// versions — keep the whole segment until the snapshot releases.
			continue
		}
		if seg.deadBytes.Load() <= 0 {
			// Nothing to reclaim. This skips a tombstone-only segment (records with
			// no payload: liveBytes==0 AND deadBytes==0) — repacking it would just
			// copy the tombstones into an identical tombstone-only segment and loop
			// forever reclaiming zero. Only a segment carrying dead payload qualifies.
			continue
		}
		occupied := seg.liveBytes.Load() // physical payload bytes ever written here
		var ratio float64
		if occupied <= 0 {
			ratio = 1 // fully-dead payload (deadBytes>0, no live bytes): pure garbage
		} else {
			dead := occupied - live[id]
			if dead <= 0 {
				continue // fully live: nothing to reclaim
			}
			ratio = float64(dead) / float64(occupied)
		}
		if ratio > bestRatio {
			best, bestRatio = seg, ratio
		}
	}
	if best == nil {
		return nil
	}
	if !opts.Force && bestRatio < s.cfg.GCDeadRatioForce {
		return nil
	}
	return best
}
