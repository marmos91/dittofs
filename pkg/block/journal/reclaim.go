package journal

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// Segment reclamation: two distinct policies over one shared retirement tail.
//
//   - Eviction (disk pressure) marks a sealed, fully-synced segment's intervals
//     cold — its bytes now live only remotely, reads refetch — then retires it.
//   - GC/repack (dead-byte ratio) relocates a victim's still-live records into a
//     fresh local segment and repoints the index — data stays warm — then retires
//     the victim.
//
// Both end by dropping the segment from the sealed set, draining in-flight preads,
// closing the fd, unlinking the files, and decrementing diskBytes: retireSegment.

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
// nothing qualified.
//
// Evict is the explicit force-evict entrypoint (the shares evict admin's
// DrainLocalSynced). When the sealed set cannot satisfy the reclaim it force-seals
// the fully-synced active segments so their bytes become evictable too: a working
// set smaller than the segment-roll threshold otherwise sits entirely in the
// never-sealed active segment, where nothing can reclaim it.
func (s *Store) Evict(ctx context.Context, targetBytes int64) (EvictResult, error) {
	return s.evict(ctx, targetBytes, true)
}

// evict is the shared eviction loop. allowActiveSeal enables the force-seal
// fall-through used by the explicit Evict path; the write-path capacity gate
// (ensureSpace) passes false so a sustained writer backpressures on its own
// unsynced bytes instead of sealing the very segment it is appending into.
func (s *Store) evict(ctx context.Context, targetBytes int64, allowActiveSeal bool) (EvictResult, error) {
	if err := ctx.Err(); err != nil {
		return EvictResult{}, err
	}
	if s.closed.Load() {
		return EvictResult{}, errClosed
	}
	if s.evictionDisabled.Load() {
		return EvictResult{}, nil
	}
	var res EvictResult
	sealedActives := false
	for {
		seg, sh := s.claimColdestEvictable()
		if seg == nil {
			// Sealed set exhausted. On an explicit force-evict, seal the
			// fully-synced active segments once so their bytes become evictable —
			// the next iteration drains them. Bounded to a single pass so a fresh
			// (empty) active segment is never sealed in a spin.
			if allowActiveSeal && !sealedActives {
				sealedActives = true
				sealed, err := s.sealSyncedActives(ctx)
				if err != nil {
					return res, err
				}
				if sealed {
					continue
				}
			}
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

// sealableActive reports whether act can be force-sealed: it holds at least one
// record, every record is synced (remote-durable), no other pass has claimed it,
// and no live snapshot pins its bytes. records raises monotonically and
// syncedRecords only rises toward it, so the fully-synced check is stable while
// the caller holds the shard lock.
func (s *Store) sealableActive(act *segmentMeta) bool {
	return act != nil && act.records.Load() > 0 &&
		act.syncedRecords.Load() == act.records.Load() &&
		!act.busy.Load() && !s.pinned(act)
}

// sealSyncedActives force-seals each shard's active segment when it holds only
// synced (remote-durable) records, moving it into the sealed set so eviction can
// reclaim it. It never seals an empty active (nothing to gain, and a spin would
// spew empty segments), an active holding any unsynced record (sealing would not
// make it evictable and its dirty bytes must stay the local copy), or a pinned
// active (a live snapshot needs those bytes local). A seal here is the same
// primitive as a rotation, so it produces an identically valid sealed footer.
// It returns whether it sealed any segment and surfaces a seal failure (fsync or
// next-segment creation) rather than masking it as a no-op, and stops early if
// ctx is cancelled. Caller holds no lock.
func (s *Store) sealSyncedActives(ctx context.Context) (bool, error) {
	sealedAny := false
	for _, sh := range s.shards {
		if err := ctx.Err(); err != nil {
			return sealedAny, err
		}
		sh.mu.Lock()
		if s.sealableActive(sh.active) {
			err := s.sealSegment(sh)
			sh.mu.Unlock()
			if err != nil {
				return sealedAny, err
			}
			sealedAny = true
			continue
		}
		sh.mu.Unlock()
	}
	return sealedAny, nil
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
				if s.pinned(seg) {
					// Pinned by a live snapshot: its bytes are the only durable copy
					// of an at-or-below-watermark record (local-only) — never evict.
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

// evictSegment removes one claimed sealed segment. Every interval-tree entry
// backed by the segment becomes a cold marker — so a later read of that range
// fetches from the remote store instead of seeing a false POSIX hole, and
// DataExtents still reports the range as present — and only then is the segment
// retired. Caller must have claimed seg.busy.
//
// The markers are persisted to the cold log FIRST, while the segment is still on
// disk: they are the only remaining record that those ranges hold data, so a
// crash between marking and unlinking must not be able to lose them. Persisting
// first can at worst leave a marker for bytes that are still local (the eviction
// did not complete), which costs a needless remote fetch — the opposite order
// would serve zeros.
//
// A failure drops the claim (as reclaimEmptied does) so a later pass can retry the
// segment: the claim is exclusive, so keeping it would bar the segment from every
// subsequent eviction and GC pass, stranding its bytes under the very disk pressure
// eviction serves. Retrying is safe because the failure leaves the index and the
// segment untouched; at worst it left markers for bytes that are still local, which
// the persist-first order already tolerates.
func (s *Store) evictSegment(sh *shard, seg *segmentMeta) (freed int64, err error) {
	defer func() {
		if err != nil {
			seg.busy.Store(false)
		}
	}()

	sh.mu.Lock()
	var entries []coldEntry
	for id, fi := range sh.index {
		for k := range fi.ivs {
			if fi.ivs[k].loc.SegmentID == seg.id && !fi.ivs[k].cold {
				entries = append(entries, coldEntry{
					id:      id,
					fileOff: fi.ivs[k].fileOff,
					length:  fi.ivs[k].length,
					version: fi.ivs[k].version,
				})
			}
		}
	}
	sh.mu.Unlock()

	// The files this segment backs are exactly the ones the scan produced an
	// entry for, deduplicated.
	backed := make(map[FileID]struct{}, len(entries))
	for _, e := range entries {
		backed[e.id] = struct{}{}
	}

	if err = s.appendCold(entries); err != nil {
		// Without a durable marker the range would come back from a restart as a
		// hole, so keep the segment (and its bytes) instead of evicting blind.
		return 0, err
	}

	// Flip only the files the scan above found backed by this segment, rather
	// than walking the shard index a second time. No file can join that set in
	// between: the segment is sealed (nothing appends to it) and claimed, so no
	// repack can repoint an interval into it either. A file that LEFT the set —
	// its interval superseded while the cold log was being written — simply has
	// nothing left to flip.
	//
	// ponytail: one full index walk remains, in the scan above. Removing it needs
	// a segment→intervals reverse index, worth building only if eviction shows up
	// in a profile.
	sh.mu.Lock()
	for id := range backed {
		fi := sh.index[id]
		if fi == nil {
			continue
		}
		for k := range fi.ivs {
			if fi.ivs[k].loc.SegmentID == seg.id && !fi.ivs[k].cold {
				fi.ivs[k].cold = true
			}
		}
	}
	sh.mu.Unlock()
	return s.retireSegment(sh, seg)
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
	lastUnsynced := s.unsynced.Load()
	warned := false
	for s.diskBytes.Load()+needed > s.cfg.MaxLocalBytes {
		if err := ctx.Err(); err != nil {
			return err
		}
		overage := s.diskBytes.Load() + needed - s.cfg.MaxLocalBytes
		// Write-path eviction never force-seals the active segment: the bytes
		// pinning the cap are this writer's own unsynced records, so the fix is to
		// wait for carve to drain them, not to seal-and-strand them.
		res, err := s.evict(ctx, overage, false)
		if err != nil {
			return err
		}
		if res.SegmentsEvicted > 0 {
			deadline = time.Now().Add(s.cfg.EvictMaxWait) // reclaimed: extend the budget
			lastUnsynced = s.unsynced.Load()
			continue
		}
		// Nothing evictable. As long as carve keeps draining unsynced bytes to the
		// remote, backpressure (slow) rather than error: refresh the budget on every
		// observed drain so a writer that merely outpaces a live syncer waits for it
		// instead of failing. ErrLocalStoreFull stays reserved for a genuine stall —
		// the cap is pinned by unsynced bytes and no drain progress happens for the
		// whole EvictMaxWait budget (no syncer, or sync disabled) — so the wait is
		// bounded and never hangs.
		if cur := s.unsynced.Load(); cur < lastUnsynced {
			lastUnsynced = cur
			deadline = time.Now().Add(s.cfg.EvictMaxWait)
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

// retireSegment is the shared reclamation tail both eviction and GC end on. The
// caller has already done its policy-specific index work (evict: mark intervals
// cold; GC: repoint the index to the relocation target) and claimed seg.busy.
// This drops the segment from the sealed set under sh.mu, drains in-flight
// unlocked preads via the exclusive readGuard — taken WITHOUT sh.mu so it can't
// deadlock a reader that holds sh.mu while acquiring the shared guard — closes
// the fd, unlinks the .seg/.idx files, and decrements diskBytes by the reclaimed
// on-disk bytes. It returns those bytes.
//
// On unlink failure the segment is already out of the index and its fd closed —
// reclaim is one-way. The leftover .seg is a harmless orphan the recovery sweep
// reclaims; report the error and leave diskBytes counting it, since it still
// occupies disk.
func (s *Store) retireSegment(sh *shard, seg *segmentMeta) (int64, error) {
	sh.mu.Lock()
	freed := seg.tail.Load()
	delete(sh.sealed, seg.id)
	sh.mu.Unlock()

	seg.readGuard.Lock()
	_ = seg.close()
	seg.readGuard.Unlock()

	if err := os.Remove(s.segPath(seg.id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("journal: retire remove segment %d: %w", seg.id, err)
	}
	_ = os.Remove(s.idxPath(seg.id)) // best-effort: the .idx is rebuildable
	s.diskBytes.Add(-freed)
	return freed, nil
}

// reclaimEmptied retires every segment in sh that now holds no live (non-cold)
// interval and has every record synced — the state a just-completed tombstone
// leaves behind when the removed file was a segment's only occupant. The most
// visible case is the ACTIVE segment after a cold read hydrated the removed
// file's bytes back into the local tier: eviction and GC only ever reclaim the
// SEALED set, so without this an unlink-after-cold-read strands those bytes on
// disk until the next rotation or an explicit force-evict.
//
// It only touches segments that back no live data, so it frees dead space
// without evicting any warm read-cache and can never drop live bytes. A pinned
// segment (a live snapshot's watermark) is left for the snapshot to release. A
// fully-dead, fully-synced active segment is sealed first — the same force-seal
// primitive the explicit evict path uses — so the reclaim tail can drop it.
// carveMu is held so a concurrent carve can't flip synced bits on a segment
// mid-retire; sealed victims are claimed via busy so eviction/GC never race the
// same retire. Best-effort: a retire failure is returned (and its disk stays
// counted, reclaimed later by the recovery sweep) but never wedges the unlink.
func (s *Store) reclaimEmptied(sh *shard) error {
	sh.carveMu.Lock()
	defer sh.carveMu.Unlock()

	sh.mu.Lock()
	// Segments still backing at least one live (non-cold) interval must survive.
	liveSegs := make(map[uint64]struct{})
	for _, fi := range sh.index {
		for _, iv := range fi.ivs {
			if !iv.cold {
				liveSegs[iv.loc.SegmentID] = struct{}{}
			}
		}
	}
	// Seal a fully-dead, fully-synced active segment so the retire below can drop
	// it.
	if act := sh.active; s.sealableActive(act) {
		if _, live := liveSegs[act.id]; !live {
			if err := s.sealSegment(sh); err != nil {
				sh.mu.Unlock()
				return err
			}
		}
	}
	// Collect the sealed segments this shard can now drop whole.
	var victims []*segmentMeta
	for id, seg := range sh.sealed {
		if _, live := liveSegs[id]; live {
			continue
		}
		if !evictable(seg) || s.pinned(seg) {
			continue
		}
		victims = append(victims, seg)
	}
	sh.mu.Unlock()

	for _, seg := range victims {
		if !seg.busy.CompareAndSwap(false, true) {
			continue // claimed by a concurrent eviction/GC — it will retire it
		}
		if _, err := s.retireSegment(sh, seg); err != nil {
			seg.busy.Store(false)
			logger.Warn("journal: reclaim emptied segment", "segment", seg.id, "err", err)
			return err
		}
	}
	return nil
}

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
// engine's block-GC sweep (pkg/block/engine/gc_block.go), whose per-remote
// serialization is what makes a decrement safe — the union BlockReclaimer
// decrement is unsafe under a concurrent GC (its sweeper lock is per-GCStateRoot,
// not per-remote), so journal GC must never drive it. A tombstone here only drops
// LOCAL records and marks LOCAL space dead; the deleted file's remote blocks are
// freed later by that sweep, off this path.

// GCOptions selects a GC pass's aggressiveness.
type GCOptions struct {
	// Force repacks the single highest-dead-ratio sealed segment in each shard
	// even when its ratio is below GCDeadRatioForce (an explicit / test trigger).
	// Without it, a pass repacks every segment at or above the force threshold.
	Force bool
}

// GCResult reports what a GC pass reclaimed.
type GCResult struct {
	SegmentsRepacked int
	BytesReclaimed   int64 // net local bytes freed (victim size minus relocated live)
}

// GC repacks sealed segments whose dead-byte fraction has grown, freeing the
// space superseded writes and tombstones left behind. It is safe to call from a
// background loop or explicitly; passes serialize on gcMu.
func (s *Store) GC(ctx context.Context, opts GCOptions) (GCResult, error) {
	if err := ctx.Err(); err != nil {
		return GCResult{}, err
	}
	if s.closed.Load() {
		return GCResult{}, errClosed
	}
	s.gcMu.Lock()
	defer s.gcMu.Unlock()

	var res GCResult
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
// carveMu for the whole pass so carve — which flips synced bits by record offset
// — never runs against a segment repack is relocating (the same segment-busy
// discipline eviction takes).
func (s *Store) gcShard(ctx context.Context, sh *shard, opts GCOptions) (reclaimed int64, count int, err error) {
	sh.carveMu.Lock()
	defer sh.carveMu.Unlock()

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

// pinned reports whether a live snapshot's watermark protects any record in seg:
// its lowest record Version is at or below pinVersion. Whole-segment granularity —
// segments fill sequentially, so a non-empty segment's records span a contiguous
// version range and minVersion alone decides. pinVersion 0 (no live snapshot) or a
// still-empty segment (minVersion 0) pins nothing.
func (s *Store) pinned(seg *segmentMeta) bool {
	pv := s.pinVersion.Load()
	if pv == 0 {
		return false
	}
	mv := seg.minVersion.Load()
	return mv != 0 && mv <= pv
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
func (s *Store) pickVictim(sh *shard, opts GCOptions, live map[uint64]int64) *segmentMeta {
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

// liveRec is one still-live interval to relocate out of a repack victim.
type liveRec struct {
	id      FileID
	fileOff int64
	length  int64
	version uint64
	synced  bool
	srcOff  int64 // payload offset in the victim
	recOff  int64 // owning record's header offset in the victim
}

// movesByVersion buckets a repack's moves by record Version so the index-repoint
// loop can find a move's covering range without rescanning the whole slice per
// interval. A Version identifies one written record, so a bucket holds only the
// remnants a later overwrite split that record into — a handful, not len(moves).
func movesByVersion(moves []liveRec) map[uint64][]int {
	byVer := make(map[uint64][]int, len(moves))
	for i := range moves {
		byVer[moves[i].version] = append(byVer[moves[i].version], i)
	}
	return byVer
}

// findMove returns the index of the move whose logical range fully contains
// [lo, hi) at the given version, or -1. A concurrent overwrite may have trimmed
// or split a victim interval since the snapshot; the trimmed remnant is still a
// sub-range of exactly one original move, so this repoints it to the right byte.
func findMove(moves []liveRec, byVer map[uint64][]int, lo, hi int64, ver uint64) int {
	for _, i := range byVer[ver] {
		m := moves[i]
		if m.fileOff <= lo && hi <= m.fileOff+m.length {
			return i
		}
	}
	return -1
}

// repackSegment relocates a victim's still-live records (and carries its
// tombstones forward) into a fresh sealed target, then retires the victim.
//
// Ordering invariant — bytes-durable-before-index-durable-before-reclaim:
//  1. write survivors into the target;
//  2. fsync + seal the target (its dir entry was fsynced at create) — bytes durable;
//  3. repoint the in-memory index to the target (the index is not itself durable
//     — recovery rebuilds it from the .seg records, which are now durable);
//  4. only then retire the victim.
//
// A crash before step 4 leaves the victim as a harmless orphan: its records
// duplicate the target's with the identical Version, so recovery replays both
// byte-identically and the next GC pass (seeing the victim fully superseded)
// reclaims it. Version and the synced flag are copied verbatim, never reissued,
// so newest-wins survives the physical move.
// live is the caller's per-segment live-byte view: repackSegment retires the
// victim's entry and records the target's so a following pickVictim sees the
// move without re-summing the shard.
func (s *Store) repackSegment(sh *shard, victim *segmentMeta, live map[uint64]int64) (int64, error) {
	// 1a. Snapshot the victim's live intervals under the shard lock.
	sh.mu.Lock()
	var moves []liveRec
	for id, fi := range sh.index {
		for _, iv := range fi.ivs {
			if iv.cold || iv.loc.SegmentID != victim.id {
				continue
			}
			moves = append(moves, liveRec{
				id: id, fileOff: iv.fileOff, length: iv.length,
				version: iv.version, synced: iv.synced,
				srcOff: iv.loc.Offset, recOff: iv.recOff,
			})
		}
	}
	occupied := victim.liveBytes.Load()
	sh.mu.Unlock()

	// quarantine condemns the victim: it keeps its bytes (a repack that cannot
	// verify a record must not copy it forward under a fresh CRC) and is skipped
	// by every later pass, so one damaged segment does not stall the shard.
	quarantine := func(err error) error {
		victim.corrupt.Store(true)
		logf("journal: WARN segment %d failed a repack integrity check; leaving it in place and skipping it: %v", victim.id, err)
		return err
	}

	// 1b. Carry forward the victim's tombstones and truncate markers so deletes
	// and size-downs stay durable until every record they fence is gone.
	markers, err := victimMarkers(victim, s.cfg.SegmentSize)
	if err != nil {
		return 0, quarantine(err)
	}

	if len(moves) == 0 && len(markers) == 0 {
		return s.dropVictim(sh, victim, occupied)
	}

	// 2. Write survivors + tombstones into a fresh target.
	target, err := s.createSegment()
	if err != nil {
		return 0, err
	}
	cleanup := func() {
		_ = target.close()
		_ = os.Remove(s.segPath(target.id))
		_ = os.Remove(s.idxPath(target.id))
	}
	newOff := make([]int64, len(moves))
	var relocated, syncedCount int64
	for i := range moves {
		m := moves[i]
		if m.length < 0 || m.length > maxPayloadLen {
			cleanup()
			return 0, quarantine(fmt.Errorf("journal: repack implausible record length %d in segment %d", m.length, victim.id))
		}
		// Verify the source record before copying it forward: writeDataRecord
		// stamps a fresh CRC over whatever it is handed, so an unverified copy
		// would launder on-disk bit rot into a record that passes every later
		// integrity check. One record in RAM at a time.
		rec, rerr := readVerifiedRecord(victim.fd, m.recOff, s.cfg.SegmentSize, m.id, nil)
		if rerr != nil {
			cleanup()
			return 0, quarantine(fmt.Errorf("journal: repack verify victim %d record %d for %q@%d: %w",
				victim.id, m.recOff, m.id, m.fileOff, rerr))
		}
		data, ok := rec.payloadRange(m.srcOff, m.length)
		if !ok {
			cleanup()
			return 0, quarantine(fmt.Errorf("journal: repack victim %d record %d does not frame %q@%d+%d: %w",
				victim.id, m.recOff, m.id, m.fileOff, m.length, errTornRecord))
		}
		poff, werr := writeDataRecord(target, m.id, m.fileOff, m.version, m.synced, data)
		if werr != nil {
			cleanup()
			return 0, werr
		}
		newOff[i] = poff
		relocated += m.length
		if m.synced {
			syncedCount++
		}
		target.noteMinVersion(m.version)
	}
	for _, mk := range markers {
		var werr error
		if mk.flags&flagTruncate != 0 {
			_, werr = writeTruncateRecord(target, mk.id, mk.version, mk.newSize)
		} else {
			_, werr = writeTombstoneRecord(target, mk.id, mk.version)
		}
		if werr != nil {
			cleanup()
			return 0, werr
		}
		target.noteMinVersion(mk.version)
	}

	// 3. Bytes durable: fsync + seal the target before any index entry names it.
	if err := target.sealInPlace(); err != nil {
		cleanup()
		return 0, err
	}
	target.liveBytes.Store(relocated)
	target.syncedRecords.Store(syncedCount)
	// Account the relocated records + tombstones now durable in the target;
	// writeDataRecord/writeTombstoneRecord don't touch diskBytes (unlike the
	// append path), and createSegment already counted segHeaderSize.
	s.diskBytes.Add(target.tail.Load() - segHeaderSize)

	// 4. Repoint the index, skipping intervals a concurrent write superseded.
	byVer := movesByVersion(moves)
	sh.mu.Lock()
	remaining := 0
	relocatedLive := int64(0)
	for _, fi := range sh.index {
		for k := range fi.ivs {
			iv := &fi.ivs[k]
			if iv.cold || iv.loc.SegmentID != victim.id {
				continue
			}
			mi := findMove(moves, byVer, iv.fileOff, iv.end(), iv.version)
			if mi < 0 {
				// No source move covers it — must never drop the victim while a
				// live interval still points into it.
				remaining++
				continue
			}
			delta := iv.fileOff - moves[mi].fileOff
			iv.loc.SegmentID = target.id
			iv.loc.Offset = newOff[mi] + delta
			iv.recOff = newOff[mi] - recordHeaderSize - int64(len(moves[mi].id))
			relocatedLive += iv.length
		}
	}
	sh.sealed[target.id] = target
	sh.mu.Unlock()

	if remaining != 0 {
		// Defensive: nothing writes to a sealed segment, so this cannot happen;
		// keep the victim to preserve those bytes rather than lose data. The
		// target is a redundant orphan the next pass reclaims.
		logf("journal: WARN repack left %d live interval(s) in segment %d; keeping it", remaining, victim.id)
		return 0, nil
	}

	if testStopBeforeUnlink {
		// Test seam: model a crash after the target is durable and indexed but
		// before the victim is reclaimed. On disk the victim's records duplicate
		// the target's with identical Version, so recovery replays both
		// byte-identically and the next pass reclaims the orphan.
		return 0, nil
	}

	// 5. Retire the victim: drop it, drain in-flight preads, close, unlink.
	if _, err := s.retireSegment(sh, victim); err != nil {
		return 0, err
	}
	// The victim is gone and the target now holds its survivors. Every early
	// return above leaves the victim in place, so its entry stays valid there.
	// live is private to the GC pass, which is single-goroutine under gcMu.
	delete(live, victim.id)
	live[target.id] = relocatedLive

	net := occupied - relocated
	if net < 0 {
		net = 0
	}
	return net, nil
}

// testStopBeforeUnlink, when set by a test, makes repackSegment return after the
// target is durable and the index repointed but before the victim is reclaimed,
// reproducing a crash between those steps. Always false in production.
var testStopBeforeUnlink bool

// dropVictim reclaims a fully-dead segment: no live records, no tombstones. It
// re-checks under the shard lock that nothing references it, then retires it.
func (s *Store) dropVictim(sh *shard, victim *segmentMeta, occupied int64) (int64, error) {
	sh.mu.Lock()
	for _, fi := range sh.index {
		for _, iv := range fi.ivs {
			if !iv.cold && iv.loc.SegmentID == victim.id {
				sh.mu.Unlock()
				return 0, nil // a concurrent path re-touched it; leave it
			}
		}
	}
	sh.mu.Unlock()
	if _, err := s.retireSegment(sh, victim); err != nil {
		return 0, err
	}
	return occupied, nil
}

// markRec is a tombstone or truncate marker carried forward across a repack.
// newSize is meaningful only for truncate markers (flags&flagTruncate != 0).
type markRec struct {
	id      FileID
	version uint64
	flags   uint8
	newSize int64
}

// victimMarkers scans a sealed victim's record stream for the non-data records
// (tombstones and truncate markers) a repack must carry forward so the deletes
// and size-downs they encode survive the source segment's reclamation.
//
// A scan that stops short of the segment's record end means a record failed its
// integrity check, and every marker behind it would be silently dropped — the
// delete or size-down it encodes would come back to life once the victim is
// reclaimed. That reports an error so the repack leaves the victim alone.
func victimMarkers(seg *segmentMeta, segSize int64) ([]markRec, error) {
	recs, validUpTo := scanValidRecords(seg.fd, segSize, segSize)
	if tail := seg.tail.Load(); validUpTo < tail {
		return nil, fmt.Errorf("journal: segment %d record stream ends at %d, want %d: %w",
			seg.id, validUpTo, tail, errTornRecord)
	}
	var out []markRec
	for _, rec := range recs {
		switch {
		case rec.header.Flags&flagTombstone != 0:
			out = append(out, markRec{id: FileID(rec.fileID), version: rec.header.Version, flags: flagTombstone})
		case rec.header.Flags&flagTruncate != 0:
			out = append(out, markRec{
				id:      FileID(rec.fileID),
				version: rec.header.Version,
				flags:   flagTruncate,
				newSize: int64(rec.header.FileOffset),
			})
		}
	}
	return out, nil
}

// writeDataRecord frames one data record at seg's tail, preserving the caller's
// Version and synced flag, and returns its payload offset. Used only by repack;
// it never touches the index (repack repoints the index after the target is
// durable).
func writeDataRecord(seg *segmentMeta, id FileID, fileOff int64, version uint64, synced bool, data []byte) (payloadOff int64, err error) {
	fileID := []byte(id)
	recStart := seg.tail.Load()
	var flags uint8
	if synced {
		flags |= flagSynced
	}
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: uint64(fileOff),
		PayloadLen: uint32(len(data)),
		Version:    version,
		Flags:      flags,
	}, fileID)
	payloadOff = recStart + int64(len(hdr))
	if _, err = seg.fd.WriteAt(hdr, recStart); err != nil {
		return 0, fmt.Errorf("journal: repack write header: %w", err)
	}
	if _, err = seg.fd.WriteAt(data, payloadOff); err != nil {
		return 0, fmt.Errorf("journal: repack write payload: %w", err)
	}
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], recordCRC(fileID, data))
	if _, err = seg.fd.WriteAt(crcBuf[:], payloadOff+int64(len(data))); err != nil {
		return 0, fmt.Errorf("journal: repack write CRC: %w", err)
	}
	seg.tail.Store(recStart + recordLen(len(fileID), len(data)))
	return payloadOff, nil
}
