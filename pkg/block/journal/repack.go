package journal

import (
	"encoding/binary"
	"fmt"
	"os"
)

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
		s.log.Warn("journal: segment failed a repack integrity check; leaving it in place and skipping it",
			"segment", victim.id, "err", err)
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
	// Data records only — the markers carried forward above are excluded, matching
	// the append path and the recovery replay so a restart reconstructs the same
	// denominator. Leaving it zero makes evictable's synced-gate compare against
	// zero: a target holding only unsynced records reads as fully synced and
	// eviction discards the sole copy of dirty bytes, while one holding synced
	// records never reaches equality again and its space is never reclaimed.
	target.records.Store(int64(len(moves)))
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
		s.log.Warn("journal: repack left live intervals in the victim segment; keeping it",
			"segment", victim.id, "live_intervals", remaining)
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
