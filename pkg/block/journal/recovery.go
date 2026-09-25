package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// orphanMinAge gates the recovery orphan sweep: a segment file that recovery
// cannot attach to any shard (a torn create with an unreadable header, or an
// empty active segment left over beyond what the shards need) is only unlinked
// once it is at least this old. The gate mirrors the fs store's fsync-then-
// unlink ordering, where a crash before unlink leaves a harmless orphan — young
// enough that it might belong to an operation still in flight is left in place.
const orphanMinAge = 5 * time.Minute

// truncMark records the highest-Version truncate marker seen for a file during
// recovery: the new size and the Version that fences which intervals it clips.
type truncMark struct {
	version uint64
	newSize int64
}

// scanSegmentIDs returns the IDs of every well-formed <id>.seg file in dir.
func scanSegmentIDs(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("journal: readdir %q: %w", dir, err)
	}
	var ids []uint64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), segSuffix) {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), segSuffix)
		id, err := strconv.ParseUint(stem, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// recover rebuilds in-memory state from the segments already on disk. Sealed
// segments must replay completely; the one active (unsealed) segment per shard
// may have an incomplete append, so its torn tail is truncated. All valid
// records feed a fresh interval index — order does not matter because insert
// resolves overlaps by Version — and the global LSN resumes at
// max(observed Version)+1.
//
// The phases run in the order below; each is a method on recoveryState, which
// carries the tables being rebuilt between them.
func (s *Store) recover() error {
	r := newRecoveryState(s)

	ok := false
	defer func() {
		if !ok {
			r.closeOpened()
		}
	}()

	s.sweepIdxSidecars()
	if err := r.scanSegments(); err != nil {
		return err
	}
	if err := r.applyColdLog(); err != nil {
		return err
	}

	s.nextSeg.Store(r.maxSegID + 1)
	// nextVersion increments-then-returns, so storing maxVersion makes the next
	// issued LSN exactly max(observed)+1 — strictly past every replayed record.
	s.version.Store(r.maxVersion)

	applyTombstones(r.indexByShard, r.tombstones)
	applyTruncations(r.indexByShard, r.truncations)

	r.compactColdLog()
	s.unsynced.Store(r.reconcileByteCounters())

	shards, err := r.assignActiveSegments()
	if err != nil {
		return err
	}

	s.sweepOrphans(r.orphans)
	s.shards = shards
	// Reconcile the disk-byte counter with what recovery actually opened: each
	// segment's tail is its on-disk size (header + intact records). createSegment
	// bumped diskBytes for any fresh active it minted; Store the true total over it.
	var disk int64
	for _, sh := range shards {
		if sh.active != nil {
			disk += sh.active.tail.Load()
		}
		for _, seg := range sh.sealed {
			disk += seg.tail.Load()
		}
	}
	s.diskBytes.Store(disk)
	ok = true
	return nil
}

// recoveryState carries the tables recover() rebuilds across its phases: the
// per-shard segment and interval maps, the segments opened so far (all closed
// again if a later phase fails), and the tombstone/truncate markers whose
// clipping is deferred until every record has been replayed.
type recoveryState struct {
	s *Store

	actives       []*segmentMeta // data-bearing unsealed segment per shard
	sealedByShard []map[uint64]*segmentMeta
	indexByShard  []map[FileID]*fileIndex

	emptyPool []*segmentMeta // empty unsealed segments, reusable as any shard's active
	orphans   []uint64       // unattachable segment ids, candidates for the age-gated sweep
	opened    []*segmentMeta // every fd we opened, closed on error

	maxSegID   uint64
	maxVersion uint64
	coldLoaded int // cold-log entries read back, for the compaction ratio

	tombstones  map[FileID]uint64    // deleted file -> highest tombstone version
	truncations map[FileID]truncMark // truncated file -> highest truncate marker
}

func newRecoveryState(s *Store) *recoveryState {
	n := s.cfg.ShardCount
	r := &recoveryState{
		s:             s,
		actives:       make([]*segmentMeta, n),
		sealedByShard: make([]map[uint64]*segmentMeta, n),
		indexByShard:  make([]map[FileID]*fileIndex, n),
		tombstones:    map[FileID]uint64{},
		truncations:   map[FileID]truncMark{},
	}
	for i := 0; i < n; i++ {
		r.sealedByShard[i] = make(map[uint64]*segmentMeta)
		r.indexByShard[i] = make(map[FileID]*fileIndex)
	}
	return r
}

func (r *recoveryState) closeOpened() {
	for _, m := range r.opened {
		_ = m.close()
	}
}

// scanSegments opens every <id>.seg in the store directory in ascending id
// order and folds it into the recovery tables.
func (r *recoveryState) scanSegments() error {
	segIDs, err := scanSegmentIDs(r.s.dir)
	if err != nil {
		return err
	}
	// Deterministic, ascending replay. Version still decides newest-wins, but a
	// stable order keeps recovery reproducible and eases debugging.
	sort.Slice(segIDs, func(i, j int) bool { return segIDs[i] < segIDs[j] })

	for _, id := range segIDs {
		if id > r.maxSegID {
			r.maxSegID = id
		}
		if err := r.loadSegment(id); err != nil {
			return err
		}
	}
	return nil
}

// loadSegment reads one segment's header and record stream, classifies it as
// active / sealed / empty / orphan, truncates an active segment's torn tail,
// and replays its records into the shard's interval index.
func (r *recoveryState) loadSegment(id uint64) error {
	s := r.s
	path := s.segPath(id)
	fd, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("journal: open segment %q: %w", path, err)
	}
	var hdr [segHeaderSize]byte
	if _, rerr := fd.ReadAt(hdr[:], 0); rerr != nil {
		// A header that will not even read back is a torn create; sweep it.
		_ = fd.Close()
		r.orphans = append(r.orphans, id)
		return nil
	}
	hdrID, createdAt, flags, hdrOK := decodeSegHeader(hdr[:])
	if !hdrOK || hdrID != id {
		_ = fd.Close()
		r.orphans = append(r.orphans, id)
		return nil
	}
	sealed := flags&segFlagSealed != 0

	// SegmentSize is the ceiling for a single record's PayloadLen (the append
	// path enforces it), so it doubles as the sanity ceiling that stops a
	// CRC-coincidence torn header from making the scanner trust a bogus length.
	recs, validUpTo := scanValidRecords(fd, s.cfg.SegmentSize, s.cfg.SegmentSize)

	if sealed {
		info, err := fd.Stat()
		if err != nil {
			_ = fd.Close()
			return fmt.Errorf("journal: stat sealed segment %q: %w", path, err)
		}
		// Sealing fsyncs every record before publishing the sealed bit. An
		// unreadable suffix is therefore damaged committed data, not a torn
		// append that can be discarded. Replaying just the valid prefix would
		// present the missing records as unwritten holes, including intact
		// records after the damage. Refuse the open and preserve the file.
		if len(recs) == 0 || validUpTo != info.Size() {
			_ = fd.Close()
			return fmt.Errorf("journal: sealed segment %q is corrupt at offset %d (size %d): %w; "+
				"with the server stopped, restore a consistent backup or quarantine this file as %q "+
				"(quarantine can leave missing or stale file data)",
				path, validUpTo, info.Size(), errTornRecord, path+".quarantine")
		}
	}

	m := &segmentMeta{id: id, createdAt: createdAt, fd: fd}
	m.tail.Store(validUpTo)
	if sealed {
		m.sealed.Store(true)
	}
	r.opened = append(r.opened, m)

	if !sealed && len(recs) == 0 {
		// Empty active segment: no records name its shard. Hold it as a reuse
		// pool entry rather than a data-bearing active.
		r.emptyPool = append(r.emptyPool, m)
		return nil
	}
	if !sealed && validUpTo < fileSize(fd) {
		// Drop the torn tail and make the truncation durable before it is read.
		if terr := fd.Truncate(validUpTo); terr != nil {
			return fmt.Errorf("journal: truncate torn tail %q: %w", path, terr)
		}
		if serr := fd.Sync(); serr != nil {
			return fmt.Errorf("journal: fsync truncated segment %q: %w", path, serr)
		}
	}

	// Every record in a segment belongs to files that hash to one shard, so
	// the first record names the segment's shard.
	sh := s.shardIndex(FileID(recs[0].fileID))

	if !sealed {
		if r.actives[sh] == nil {
			r.actives[sh] = m
		} else {
			// Defensive: two data-bearing unsealed segments for one shard should
			// never happen (one active per shard). Keep the higher id active and
			// demote the other to a sealed, still-readable segment.
			if id > r.actives[sh].id {
				r.actives[sh], m = m, r.actives[sh]
			}
			m.sealed.Store(true)
			r.sealedByShard[sh][m.id] = m
		}
	} else {
		r.sealedByShard[sh][id] = m
	}

	r.replayRecords(m, sh, id, recs)
	return nil
}

// replayRecords folds one segment's valid records into the shard's interval
// index: tombstone and truncate markers are collected for the deferred clipping
// passes, every other record becomes an interval and feeds the segment's byte
// and record counters.
func (r *recoveryState) replayRecords(m *segmentMeta, sh, id uint64, recs []record) {
	idxMap := r.indexByShard[sh]
	for _, rec := range recs {
		if rec.header.Version > r.maxVersion {
			r.maxVersion = rec.header.Version
		}
		// Reconstruct the segment's pin watermark: minVersion is the lowest
		// record Version it holds, so a live snapshot keeps it off GC.
		m.noteMinVersion(rec.header.Version)
		if rec.header.Flags&flagTombstone != 0 {
			fid := FileID(rec.fileID)
			if rec.header.Version > r.tombstones[fid] {
				r.tombstones[fid] = rec.header.Version
			}
			// Rebuild the marker counter the retire paths consult before deciding
			// whether a segment is worth scanning. A restart that lost it would
			// let the first eviction skip the scan and unlink a segment with its
			// markers still inside.
			m.markers.Add(1)
			continue
		}
		if rec.header.Flags&flagTruncate != 0 {
			fid := FileID(rec.fileID)
			if cur, ok := r.truncations[fid]; !ok || rec.header.Version > cur.version {
				r.truncations[fid] = truncMark{version: rec.header.Version, newSize: int64(rec.header.FileOffset)}
			}
			m.markers.Add(1)
			continue
		}
		payloadOff := rec.segOff + recordHeaderSize + int64(len(rec.fileID))
		fid := FileID(rec.fileID)
		fi := idxMap[fid]
		if fi == nil {
			fi = &fileIndex{}
			idxMap[fid] = fi
		}
		synced := rec.header.Flags&flagSynced != 0
		fi.insert(interval{
			fileOff: int64(rec.header.FileOffset),
			length:  int64(rec.header.PayloadLen),
			version: rec.header.Version,
			recOff:  rec.segOff,
			synced:  synced,
			loc: segmentLocation{
				SegmentID: id,
				Offset:    payloadOff,
				Length:    int64(rec.header.PayloadLen),
			},
		})
		// Coarse byte accounting: liveBytes ignores same-segment supersession
		// (GC recomputes deadBytes on repack). unsynced feeds write backpressure.
		m.liveBytes.Add(int64(rec.header.PayloadLen))
		m.records.Add(1)
		if synced {
			m.syncedRecords.Add(1)
		} else if fi.firstDirtyNanos == 0 {
			// A recovered dirty file gets a fresh dirty-age stamp so the carve
			// age gate fires after a restart (approximate — the original write
			// time is not persisted; it is only a batching heuristic).
			fi.firstDirtyNanos = r.s.clock.Now().UnixNano()
		}
	}
}

// applyColdLog replays the cold log (cold.go). A cold interval owns no record —
// eviction unlinked the segment that held its bytes, or the range was seeded
// cold from a surviving manifest — so this side-log is the only thing that keeps
// the range from coming back as a POSIX hole that reads as zeros. Inserted with
// each entry's original Version, so a later warm write shadows it and the
// tombstone/truncate passes clip it exactly like a warm interval.
func (r *recoveryState) applyColdLog() error {
	coldLoaded, validUpTo, err := loadCold(r.s.dir, r.s.log)
	if err != nil {
		return err
	}
	// Repair before anything appends: recovery is the only point where the log
	// is known to end at an intact entry.
	if err := truncateColdTail(r.s.dir, validUpTo, r.s.log); err != nil {
		return err
	}
	r.coldLoaded = len(coldLoaded)
	// Seed the live count with what the log holds, so the ratio gate the
	// background compaction applies has the same input this one does.
	r.s.coldEntries = r.coldLoaded
	for _, e := range coldLoaded {
		if e.length <= 0 {
			continue
		}
		if e.version > r.maxVersion {
			r.maxVersion = e.version
		}
		idxMap := r.indexByShard[r.s.shardIndex(e.id)]
		fi := idxMap[e.id]
		if fi == nil {
			fi = &fileIndex{}
			idxMap[e.id] = fi
		}
		fi.insert(interval{
			fileOff: e.fileOff,
			length:  e.length,
			version: e.version,
			synced:  true,
			cold:    true,
			// Carried so a compaction can write it back out (liveColdEntries).
			provenance: e.provenance,
		})
	}
	return nil
}

// compactColdLog rewrites the cold log once the live set has drifted well below
// what the log holds: entries superseded by a later hydrate, buried by a
// tombstone or clipped by a truncate are dead weight the next recovery would
// replay again. Rewriting is atomic (temp + rename), so a crash here keeps the
// previous log. The in-uptime pass is maybeCompactColdLog.
func (r *recoveryState) compactColdLog() {
	live := liveColdEntries(r.indexByShard)
	if !coldCompactWorthIt(r.coldLoaded, len(live)) {
		return
	}
	if werr := r.s.rewriteCold(live); werr != nil {
		// Non-fatal: a stale-but-valid log costs redundant replay, not
		// correctness, and refusing to open would strand the whole share.
		r.s.log.Warn("journal: cold log compaction failed, keeping the existing log", "err", werr)
	}
}

// reconcileByteCounters recomputes unsynced from the final live coverage rather
// than the raw per-record sum: insert resolves overlaps, so a superseded
// record's bytes are dead and must not count toward the backpressure signal. In
// the same pass it reconstructs each segment's deadBytes from the authoritative
// live coverage (occupied liveBytes − currently-live): replay charges only
// physical records, so tombstones, same-segment overlaps, and truncate clips
// leave dead payload that never touched the counter. Without this, pickVictim's
// deadBytes<=0 gate skips every recovered segment and GC can never reclaim
// pre-restart dead space. Cold intervals are skipped exactly as pickVictim skips
// them so the live totals agree.
func (r *recoveryState) reconcileByteCounters() int64 {
	var unsynced int64
	for i, idxMap := range r.indexByShard {
		live := make(map[uint64]int64)
		for _, fi := range idxMap {
			for k := range fi.ivs {
				if fi.ivs[k].cold {
					continue
				}
				if !fi.ivs[k].synced {
					unsynced += fi.ivs[k].length
				}
				live[fi.ivs[k].loc.SegmentID] += fi.ivs[k].length
			}
		}
		for id, seg := range r.sealedByShard[i] {
			if dead := seg.liveBytes.Load() - live[id]; dead > 0 {
				seg.deadBytes.Store(dead)
			}
		}
		if a := r.actives[i]; a != nil {
			if dead := a.liveBytes.Load() - live[a.id]; dead > 0 {
				a.deadBytes.Store(dead)
			}
		}
	}
	return unsynced
}

// assignActiveSegments gives every shard an active segment: reuse a pooled empty
// one, else mint a fresh one (nextSeg is set past every recovered id by the time
// this runs). The shards are built into a local slice and published by the
// caller only on success, so a mid-build error path never leaves half-open fds
// double-closed by both the recovery defer and the caller's Close. Any pooled
// empty segment left unadopted joins the orphan sweep.
func (r *recoveryState) assignActiveSegments() ([]*shard, error) {
	n := r.s.cfg.ShardCount
	poolPos := 0
	shards := make([]*shard, n)
	for i := 0; i < n; i++ {
		active := r.actives[i]
		if active == nil {
			if poolPos < len(r.emptyPool) {
				active = r.emptyPool[poolPos]
				poolPos++
			} else {
				seg, cerr := r.s.createSegment()
				if cerr != nil {
					return nil, cerr
				}
				r.opened = append(r.opened, seg)
				active = seg
			}
		}
		sh := newShard(active)
		sh.sealed = r.sealedByShard[i]
		sh.index = r.indexByShard[i]
		// Everything the index holds now was read back off the device and passed
		// its CRCs, so it survived the crash by definition: the durable watermark
		// starts at the highest replayed Version rather than at zero.
		sh.lastVersion = r.maxVersion
		sh.syncedVersion.Store(r.maxVersion)
		shards[i] = sh
	}

	for ; poolPos < len(r.emptyPool); poolPos++ {
		r.orphans = append(r.orphans, r.emptyPool[poolPos].id)
		_ = r.emptyPool[poolPos].close()
	}
	return shards, nil
}

// applyTombstones drops each deleted file's intervals older than its tombstone.
// A delete's tombstone Version exceeds every prior write to that file, so a
// rewrite after the delete carries a higher Version, survives, and recreates the
// file.
func applyTombstones(indexByShard []map[FileID]*fileIndex, tombstones map[FileID]uint64) {
	for _, idxMap := range indexByShard {
		for fid, fi := range idxMap {
			tv, deleted := tombstones[fid]
			if !deleted {
				continue
			}
			keepIntervals(idxMap, fid, fi, func(iv interval) (interval, bool) {
				return iv, iv.version > tv
			})
		}
	}
}

// applyTruncations drops (or clips) each truncated file's intervals past
// newSize. A size-down's marker Version exceeds every write it buries, so a
// write that raced past the truncate carries a higher Version, survives, and
// re-extends the file.
func applyTruncations(indexByShard []map[FileID]*fileIndex, truncations map[FileID]truncMark) {
	for _, idxMap := range indexByShard {
		for fid, fi := range idxMap {
			tm, ok := truncations[fid]
			if !ok {
				continue
			}
			keepIntervals(idxMap, fid, fi, func(iv interval) (interval, bool) {
				switch {
				case iv.version > tm.version || iv.end() <= tm.newSize:
					return iv, true
				case iv.fileOff < tm.newSize: // straddles newSize: clip it
					return iv.clamp(iv.fileOff, tm.newSize), true
				default: // entirely past newSize
					return iv, false
				}
			})
		}
	}
}

// keepIntervals rewrites fi's intervals in place to those keep accepts, dropping
// the file from idxMap when nothing survives — an empty fileIndex would read as
// a file that exists and holds no bytes rather than as no file at all.
func keepIntervals(idxMap map[FileID]*fileIndex, fid FileID, fi *fileIndex, keep func(interval) (interval, bool)) {
	kept := fi.ivs[:0]
	for _, iv := range fi.ivs {
		if out, ok := keep(iv); ok {
			kept = append(kept, out)
		}
	}
	if len(kept) == 0 {
		delete(idxMap, fid)
		return
	}
	fi.ivs = kept
}

// sweepIdxSidecars unlinks every .idx sidecar left in the store directory by a
// build that still wrote them. Nothing reads one — the index is rebuilt from the
// .seg records alone — and diskBytes counts segment bytes only, so a leftover
// sidecar is invisible to the MaxLocalBytes gate and no reclaim path would ever
// free it. Best-effort: a file that will not unlink is retried on the next Open.
func (s *Store) sweepIdxSidecars() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Rebuild the name from a parsed segment ID rather than removing
		// whatever matched the suffix. A sidecar this store wrote is always
		// <id>.idx, so anything else in the directory belongs to someone
		// else and is not ours to unlink; going through ParseUint also means
		// no byte read off disk reaches the path passed to os.Remove.
		stem := strings.TrimSuffix(e.Name(), idxSuffix)
		if stem == e.Name() {
			continue
		}
		id, err := strconv.ParseUint(stem, 10, 64)
		if err != nil {
			continue
		}
		// Reformat through segIDFmt so the name removed is one segPath could
		// have produced: a stem that parses but is not zero-padded to width
		// names no segment of this store's.
		_ = os.Remove(filepath.Join(s.dir, fmt.Sprintf(segIDFmt+idxSuffix, id)))
	}
}

// sweepOrphans age-gates the deletion of unattachable segment files. Recovery
// rebuilds all bookkeeping from the segments themselves, so an orphan is a
// genuinely unreferenced file; the age gate only spares one young enough to
// belong to an operation that crashed mid-flight. Deletion is best-effort — a
// leftover file is harmless and retried on the next Open.
func (s *Store) sweepOrphans(ids []uint64) {
	now := s.clock.Now()
	for _, id := range ids {
		path := s.segPath(id)
		age := orphanMinAge
		if fi, err := os.Stat(path); err == nil {
			age = now.Sub(fi.ModTime())
		}
		if age < orphanMinAge {
			continue
		}
		_ = os.Remove(path)
	}
}

// fileSize returns the current size of an open file, or 0 if it cannot be
// stat'd (a scan that read nothing already treats the segment as empty).
func fileSize(fd *os.File) int64 {
	if fi, err := fd.Stat(); err == nil {
		return fi.Size()
	}
	return 0
}
