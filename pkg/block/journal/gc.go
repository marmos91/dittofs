package journal

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
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

	// Bounded: each repack removes one victim from the sealed set, and the fresh
	// target it writes carries no dead bytes, so it is never re-picked.
	for {
		if err := ctx.Err(); err != nil {
			return reclaimed, count, err
		}
		victim := s.pickVictim(sh, opts)
		if victim == nil {
			return reclaimed, count, nil
		}
		// Claim the victim so a concurrent eviction can't drop it mid-repack
		// (eviction skips busy segments; a lost claim just re-picks the next).
		if !victim.busy.CompareAndSwap(false, true) {
			continue
		}
		net, err := s.repackSegment(sh, victim)
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

// pickVictim returns the sealed segment with the highest dead fraction, or nil
// if none qualifies. A segment carrying no dead bytes is never a victim (there
// is nothing to reclaim), which is also what keeps a tombstone-only segment from
// being repacked into an identical one forever. Live bytes are summed
// authoritatively from the interval index (robust to the recovery-time deadBytes
// approximation and to the extra dead a crash-during-repack leaves behind); a
// segment's dead fraction is dead/occupied. Without Force, a victim must reach
// GCDeadRatioForce.
func (s *Store) pickVictim(sh *shard, opts GCOptions) *segmentMeta {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if len(sh.sealed) == 0 {
		return nil
	}
	live := make(map[uint64]int64, len(sh.sealed))
	for _, fi := range sh.index {
		for _, iv := range fi.ivs {
			if iv.cold {
				continue
			}
			if _, ok := sh.sealed[iv.loc.SegmentID]; ok {
				live[iv.loc.SegmentID] += iv.length
			}
		}
	}

	var best *segmentMeta
	var bestRatio float64
	for id, seg := range sh.sealed {
		if seg.busy.Load() {
			continue // claimed by eviction or an in-flight repack
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
}

// findMove returns the index of the move whose logical range fully contains
// [lo, hi) at the given version, or -1. A concurrent overwrite may have trimmed
// or split a victim interval since the snapshot; the trimmed remnant is still a
// sub-range of exactly one original move, so this repoints it to the right byte.
func findMove(moves []liveRec, lo, hi int64, ver uint64) int {
	for i := range moves {
		m := moves[i]
		if m.version == ver && m.fileOff <= lo && hi <= m.fileOff+m.length {
			return i
		}
	}
	return -1
}

// repackSegment relocates a victim's still-live records (and carries its
// tombstones forward) into a fresh sealed target, then unlinks the victim.
//
// Ordering invariant — bytes-durable-before-index-durable-before-reclaim:
//  1. write survivors into the target;
//  2. fsync + seal the target (its dir entry was fsynced at create) — bytes durable;
//  3. repoint the in-memory index to the target (the index is not itself durable
//     — recovery rebuilds it from the .seg records, which are now durable);
//  4. only then unlink the victim.
//
// A crash before step 4 leaves the victim as a harmless orphan: its records
// duplicate the target's with the identical Version, so recovery replays both
// byte-identically and the next GC pass (seeing the victim fully superseded)
// reclaims it. Version and the synced flag are copied verbatim, never reissued,
// so newest-wins survives the physical move.
func (s *Store) repackSegment(sh *shard, victim *segmentMeta) (int64, error) {
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
				version: iv.version, synced: iv.synced, srcOff: iv.loc.Offset,
			})
		}
	}
	occupied := victim.liveBytes.Load()
	sh.mu.Unlock()

	// 1b. Carry forward the victim's tombstones so deletes stay durable until
	// every shadowed data record is gone.
	tombs := victimTombstones(victim, s.cfg.SegmentSize)

	if len(moves) == 0 && len(tombs) == 0 {
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
			return 0, fmt.Errorf("journal: repack implausible record length %d in segment %d", m.length, victim.id)
		}
		data := make([]byte, int(m.length)) // one interval in RAM at a time
		if _, rerr := victim.fd.ReadAt(data, m.srcOff); rerr != nil {
			cleanup()
			return 0, fmt.Errorf("journal: repack read victim %d@%d: %w", victim.id, m.srcOff, rerr)
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
	}
	for _, t := range tombs {
		if _, werr := writeTombstoneRecord(target, t.id, t.version); werr != nil {
			cleanup()
			return 0, werr
		}
	}

	// 3. Bytes durable: fsync + seal the target before any index entry names it.
	if err := target.sealInPlace(); err != nil {
		cleanup()
		return 0, err
	}
	target.liveBytes.Store(relocated)
	target.syncedRecords.Store(syncedCount)

	// 4. Repoint the index, skipping intervals a concurrent write superseded.
	sh.mu.Lock()
	remaining := 0
	for _, fi := range sh.index {
		for k := range fi.ivs {
			iv := &fi.ivs[k]
			if iv.cold || iv.loc.SegmentID != victim.id {
				continue
			}
			mi := findMove(moves, iv.fileOff, iv.end(), iv.version)
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

	// 5. Reclaim: drop the victim from the set so no new read resolves it, drain
	// in-flight preads via the exclusive guard, then close and unlink. The guard
	// is taken WITHOUT holding sh.mu so it can never deadlock against a reader
	// that holds sh.mu while acquiring the shared guard.
	sh.mu.Lock()
	delete(sh.sealed, victim.id)
	sh.mu.Unlock()
	victim.readGuard.Lock()
	_ = victim.close()
	victim.readGuard.Unlock()
	_ = os.Remove(s.segPath(victim.id))
	_ = os.Remove(s.idxPath(victim.id))

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
// re-checks under the shard lock that nothing references it, then unlinks.
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
	delete(sh.sealed, victim.id)
	sh.mu.Unlock()
	victim.readGuard.Lock()
	_ = victim.close()
	victim.readGuard.Unlock()
	_ = os.Remove(s.segPath(victim.id))
	_ = os.Remove(s.idxPath(victim.id))
	return occupied, nil
}

// tombRec is a tombstone carried forward across a repack.
type tombRec struct {
	id      FileID
	version uint64
}

// victimTombstones scans a sealed victim's record stream for tombstones. A
// sealed segment is trusted intact, so a torn tail (should never occur) simply
// yields the records read so far.
func victimTombstones(seg *segmentMeta, segSize int64) []tombRec {
	recs, _ := scanValidRecords(seg.fd, segSize, segSize)
	var out []tombRec
	for _, rec := range recs {
		if rec.header.Flags&flagTombstone != 0 {
			out = append(out, tombRec{id: FileID(rec.fileID), version: rec.header.Version})
		}
	}
	return out
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
	binary.LittleEndian.PutUint32(crcBuf[:], crc(data))
	if _, err = seg.fd.WriteAt(crcBuf[:], payloadOff+int64(len(data))); err != nil {
		return 0, fmt.Errorf("journal: repack write CRC: %w", err)
	}
	seg.tail.Store(recStart + recordLen(len(fileID), len(data)))
	return payloadOff, nil
}
