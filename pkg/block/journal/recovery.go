package journal

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
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
// segments are trusted via their header's sealed bit ("header is truth") and
// only replayed; the one active (unsealed) segment per shard is tail-scanned and
// its torn tail truncated. All valid records feed a fresh interval index — order
// does not matter because insert resolves overlaps by Version — and the global
// LSN resumes at max(observed Version)+1.
func (s *Store) recover() error {
	segIDs, err := scanSegmentIDs(s.dir)
	if err != nil {
		return err
	}
	// Deterministic, ascending replay. Version still decides newest-wins, but a
	// stable order keeps recovery reproducible and eases debugging.
	sort.Slice(segIDs, func(i, j int) bool { return segIDs[i] < segIDs[j] })

	n := s.cfg.ShardCount
	actives := make([]*segmentMeta, n) // data-bearing unsealed segment per shard
	sealedByShard := make([]map[uint64]*segmentMeta, n)
	indexByShard := make([]map[FileID]*fileIndex, n)
	for i := 0; i < n; i++ {
		sealedByShard[i] = make(map[uint64]*segmentMeta)
		indexByShard[i] = make(map[FileID]*fileIndex)
	}

	var (
		emptyPool   []*segmentMeta // empty unsealed segments, reusable as any shard's active
		orphans     []uint64       // unattachable segment ids, candidates for the age-gated sweep
		maxSegID    uint64
		maxVersion  uint64
		unsynced    int64
		missingIdx  int
		opened      []*segmentMeta           // every fd we opened, closed on error
		tombstones  = map[FileID]uint64{}    // deleted file -> highest tombstone version
		truncations = map[FileID]truncMark{} // truncated file -> highest truncate marker
		ok          bool
	)
	defer func() {
		if !ok {
			for _, m := range opened {
				_ = m.close()
			}
		}
	}()

	for _, id := range segIDs {
		if id > maxSegID {
			maxSegID = id
		}
		path := s.segPath(id)
		fd, err := os.OpenFile(path, os.O_RDWR, 0o644)
		if err != nil {
			return fmt.Errorf("journal: open segment %q: %w", path, err)
		}
		var hdr [segHeaderSize]byte
		if _, rerr := fd.ReadAt(hdr[:], 0); rerr != nil {
			// A header that will not even read back is a torn create; sweep it.
			_ = fd.Close()
			orphans = append(orphans, id)
			continue
		}
		hdrID, createdAt, flags, hdrOK := decodeSegHeader(hdr[:])
		if !hdrOK || hdrID != id {
			_ = fd.Close()
			orphans = append(orphans, id)
			continue
		}
		sealed := flags&segFlagSealed != 0

		// SegmentSize is the ceiling for a single record's PayloadLen (the append
		// path enforces it), so it doubles as the sanity ceiling that stops a
		// CRC-coincidence torn header from making the scanner trust a bogus length.
		recs, validUpTo := scanValidRecords(fd, s.cfg.SegmentSize, s.cfg.SegmentSize)

		if sealed && len(recs) == 0 {
			// No write path can produce this: sealing fsyncs the records before it
			// sets the sealed bit, and every seal is gated on the segment already
			// holding a record. A sealed header over a record stream that scans
			// empty therefore means the bytes under it were damaged after the fact.
			// Skip the segment — no record names its shard, so there is nothing to
			// attach it to — but leave the file on disk rather than sweeping it:
			// nothing in the rebuilt index points into it, so unlinking it would
			// destroy the only remaining copy of whatever payload is still down
			// there.
			_ = fd.Close()
			logger.Warn("journal: sealed segment scans as zero valid records (damaged first record), skipping it; left in place for inspection",
				"segment_path", path)
			continue
		}

		m := &segmentMeta{id: id, createdAt: createdAt, fd: fd}
		m.tail.Store(validUpTo)
		if sealed {
			m.sealed.Store(true)
		}
		opened = append(opened, m)

		if !sealed && len(recs) == 0 {
			// Empty active segment: no records name its shard. Hold it as a reuse
			// pool entry rather than a data-bearing active.
			emptyPool = append(emptyPool, m)
			continue
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
			m.idxFD, _ = os.OpenFile(s.idxPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if actives[sh] == nil {
				actives[sh] = m
			} else {
				// Defensive: two data-bearing unsealed segments for one shard should
				// never happen (one active per shard). Keep the higher id active and
				// demote the other to a sealed, still-readable segment.
				if id > actives[sh].id {
					actives[sh], m = m, actives[sh]
				}
				m.sealed.Store(true)
				if m.idxFD != nil {
					_ = m.idxFD.Close()
					m.idxFD = nil
				}
				sealedByShard[sh][m.id] = m
			}
		} else {
			sealedByShard[sh][id] = m
		}

		if s.idxMissing(id) {
			if rerr := s.rebuildIdx(id, recs); rerr != nil {
				return rerr
			}
			missingIdx++
		}

		idxMap := indexByShard[sh]
		for _, rec := range recs {
			if rec.header.Version > maxVersion {
				maxVersion = rec.header.Version
			}
			// Reconstruct the segment's pin watermark: minVersion is the lowest
			// record Version it holds, so a live snapshot keeps it off GC.
			m.noteMinVersion(rec.header.Version)
			if rec.header.Flags&flagTombstone != 0 {
				fid := FileID(rec.fileID)
				if rec.header.Version > tombstones[fid] {
					tombstones[fid] = rec.header.Version
				}
				continue
			}
			if rec.header.Flags&flagTruncate != 0 {
				fid := FileID(rec.fileID)
				if cur, ok := truncations[fid]; !ok || rec.header.Version > cur.version {
					truncations[fid] = truncMark{version: rec.header.Version, newSize: int64(rec.header.FileOffset)}
				}
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
				loc: SegmentLocation{
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
				fi.firstDirtyNanos = s.clock.Now().UnixNano()
			}
		}
	}

	if missingIdx > 0 {
		logger.Warn("journal: segments missing .idx sidecar, rebuilding from segment scan (recovery slower)",
			"segments", missingIdx)
	}

	// Replay the cold log (cold.go). A cold interval owns no record — eviction
	// unlinked the segment that held its bytes, or the range was seeded cold from
	// a surviving manifest — so this side-log is the only thing that keeps the
	// range from coming back as a POSIX hole that reads as zeros. Inserted with
	// each entry's original Version, so a later warm write shadows it and the
	// tombstone/truncate passes below clip it exactly like a warm interval.
	coldLoaded, err := loadCold(s.dir)
	if err != nil {
		return err
	}
	for _, e := range coldLoaded {
		if e.length <= 0 {
			continue
		}
		if e.version > maxVersion {
			maxVersion = e.version
		}
		idxMap := indexByShard[s.shardIndex(e.id)]
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
		})
	}

	s.nextSeg.Store(maxSegID + 1)
	// nextVersion increments-then-returns, so storing maxVersion makes the next
	// issued LSN exactly max(observed)+1 — strictly past every replayed record.
	s.version.Store(maxVersion)

	applyTombstones(indexByShard, tombstones)
	applyTruncations(indexByShard, truncations)

	// Compact the cold log once the live set has drifted well below what the log
	// holds: entries superseded by a later hydrate, buried by a tombstone or
	// clipped by a truncate are dead weight the next recovery would replay again.
	// Recovery is the only place the surviving set is known, and rewriting is
	// atomic (temp + rename), so a crash here keeps the previous log.
	// ponytail: compaction only at open — the log grows within one uptime only as
	// fast as eviction marks new ranges cold, which is bounded by the local cap.
	if live := liveColdEntries(indexByShard); len(coldLoaded) > 2*len(live)+coldCompactFloor {
		if werr := s.rewriteCold(live); werr != nil {
			// Non-fatal: a stale-but-valid log costs redundant replay, not
			// correctness, and refusing to open would strand the whole share.
			logger.Warn("journal: cold log compaction failed, keeping the existing log", "err", werr)
		}
	}

	// Recompute unsynced from the final live coverage rather than the raw
	// per-record sum: insert resolves overlaps, so a superseded record's bytes are
	// dead and must not count toward the backpressure signal. In the same pass,
	// reconstruct each segment's deadBytes from the authoritative live coverage
	// (occupied liveBytes − currently-live): replay charges only physical records,
	// so tombstones, same-segment overlaps, and truncate clips leave dead payload
	// that never touched the counter. Without this, pickVictim's deadBytes<=0 gate
	// skips every recovered segment and GC can never reclaim pre-restart dead space.
	// Skip cold intervals exactly as pickVictim does so the live totals agree.
	for i, idxMap := range indexByShard {
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
		for id, seg := range sealedByShard[i] {
			if dead := seg.liveBytes.Load() - live[id]; dead > 0 {
				seg.deadBytes.Store(dead)
			}
		}
		if a := actives[i]; a != nil {
			if dead := a.liveBytes.Load() - live[a.id]; dead > 0 {
				a.deadBytes.Store(dead)
			}
		}
	}
	s.unsynced.Store(unsynced)

	// Give every shard an active segment: reuse a pooled empty one, else mint a
	// fresh one (nextSeg is now set past every recovered id). Build into a local
	// slice and publish it only on success so a mid-build error path never leaves
	// half-open fds double-closed by both the defer and the caller's Close.
	poolPos := 0
	shards := make([]*shard, n)
	for i := 0; i < n; i++ {
		active := actives[i]
		if active == nil {
			if poolPos < len(emptyPool) {
				active = emptyPool[poolPos]
				poolPos++
				active.idxFD, _ = os.OpenFile(s.idxPath(active.id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			} else {
				seg, cerr := s.createSegment()
				if cerr != nil {
					return cerr
				}
				opened = append(opened, seg)
				active = seg
			}
		}
		sh := newShard(active)
		sh.sealed = sealedByShard[i]
		sh.index = indexByShard[i]
		// Everything the index holds now was read back off the device and passed
		// its CRCs, so it survived the crash by definition: the durable watermark
		// starts at the highest replayed Version rather than at zero.
		sh.lastVersion = maxVersion
		sh.syncedVersion.Store(maxVersion)
		shards[i] = sh
	}

	// Any pooled empty segment we did not adopt is an orphan.
	for ; poolPos < len(emptyPool); poolPos++ {
		orphans = append(orphans, emptyPool[poolPos].id)
		_ = emptyPool[poolPos].close()
	}

	s.sweepOrphans(orphans)
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

// idxMissing reports whether a segment's .idx sidecar is absent.
func (s *Store) idxMissing(id uint64) bool {
	_, err := os.Stat(s.idxPath(id))
	return os.IsNotExist(err)
}

// rebuildIdx rewrites a segment's .idx sidecar from its scanned records. The
// sidecar is only ever rebuilt from the .seg, so a lost or partial one is
// regenerated in full and fsynced.
func (s *Store) rebuildIdx(id uint64, recs []record) error {
	path := s.idxPath(id)
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("journal: rebuild idx %q: %w", path, err)
	}
	for _, rec := range recs {
		payloadOff := rec.segOff + recordHeaderSize + int64(len(rec.fileID))
		if _, werr := fd.Write(idxEntry{
			FileIDHash: fnv1a(string(rec.fileID)),
			FileOffset: rec.header.FileOffset,
			PayloadLen: rec.header.PayloadLen,
			Version:    rec.header.Version,
			SegOffset:  uint64(payloadOff),
			Flags:      rec.header.Flags,
		}.encode()); werr != nil {
			_ = fd.Close()
			return fmt.Errorf("journal: write rebuilt idx %q: %w", path, werr)
		}
	}
	if serr := fd.Sync(); serr != nil {
		_ = fd.Close()
		return fmt.Errorf("journal: fsync rebuilt idx %q: %w", path, serr)
	}
	return fd.Close()
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
		_ = os.Remove(s.idxPath(id))
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
