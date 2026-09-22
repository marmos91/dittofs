package journal

import (
	"errors"
	"fmt"
	"os"
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

// retireSegment is the shared reclamation tail both eviction and GC end on. The
// caller has already done its policy-specific index work (evict: mark intervals
// cold; GC: repoint the index to the relocation target) and claimed seg.busy.
// This drops the segment from the sealed set under sh.mu, drains in-flight
// unlocked preads via the exclusive readGuard — taken WITHOUT sh.mu so it can't
// deadlock a reader that holds sh.mu while acquiring the shared guard — closes
// the fd, unlinks the .seg file, and decrements diskBytes by the reclaimed
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
	s.diskBytes.Add(-freed)
	return freed, nil
}

// evictable reports whether a segment can be unlinked outright: sealed, idle,
// and every record it holds already durable on the remote.
//
// It has two callers on two different paths, and lives here rather than beside
// either of them for that reason: claimColdestEvictable scans for a disk-pressure
// eviction victim, and reclaimEmptied below gates the post-delete retire of a
// segment a tombstone just emptied. A change here moves both — narrowing it to
// protect eviction also pins bytes the delete path would otherwise reclaim.
//
// records counts only payload-bearing records — markers (tombstone, truncate)
// never raise it, on the append path, the recovery replay, or the repack
// carry-forward. A segment holding nothing but markers therefore reports
// 0 == 0, reads as fully synced, and would be unlinked with the delete's only
// durable trace inside it while the records it buries sit in another segment;
// the next recovery replays them and the file comes back. Requiring a record
// excludes that segment, the same reason sealableActive requires one.
//
// decision: this covers the marker-ONLY segment and nothing more. A segment
// holding one synced data record PLUS a marker reports 1 == 1, passes here,
// and loses the marker exactly the same way — that hazard is open, predates
// this predicate, and is not closed by it. The obvious widening (refuse any
// segment holding a marker) is wrong: evictable is also the post-delete
// reclaim gate via reclaimEmptied, so it would pin a full segment's payload
// behind one 0-payload marker and make ErrLocalStoreFull reachable under
// delete-heavy pressure. The fix is to carry markers forward on evict the way
// repackSegment already does, which is more than a predicate can do.
//
// decision: excluding a marker-only segment makes it permanent, because
// pickVictim skips it too — deadBytes stays 0, so a repack would copy it into
// an identical segment forever. The cost is that segment's tail plus its open
// fd, so the real ceiling is RLIMIT_NOFILE, not disk. Withdraw it for a rule
// that can prove a marker's records are all reclaimed — a store-wide minimum
// live Version would do it — never for disk pressure alone.
func evictable(seg *segmentMeta) bool {
	return seg.sealed.Load() && !seg.busy.Load() && seg.records.Load() > 0 &&
		seg.syncedRecords.Load() == seg.records.Load()
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
// flushMu is held so a concurrent carve can't flip synced bits on a segment
// mid-retire; sealed victims are claimed via busy so eviction/GC never race the
// same retire. Best-effort: a retire failure is returned (and its disk stays
// counted, reclaimed later by the recovery sweep) but never wedges the unlink.
func (s *Store) reclaimEmptied(sh *shard) error {
	sh.flushMu.Lock()
	defer sh.flushMu.Unlock()

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
			s.log.Warn("journal: reclaim emptied segment", "segment", seg.id, "err", err)
			return err
		}
	}
	return nil
}
