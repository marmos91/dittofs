package journal

import (
	"fmt"
	"log"
	"os"
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

// logf is recovery's warning sink, overridable in tests. Recovery is otherwise
// silent; it only speaks up for events an operator should notice (a rebuilt
// index sidecar, a swept orphan).
var logf = log.Printf

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
		emptyPool  []*segmentMeta // empty unsealed segments, reusable as any shard's active
		orphans    []uint64       // unattachable segment ids, candidates for the age-gated sweep
		maxSegID   uint64
		maxVersion uint64
		unsynced   int64
		missingIdx int
		opened     []*segmentMeta // every fd we opened, closed on error
		ok         bool
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
			if rec.header.Flags&flagTombstone != 0 {
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
			if synced {
				m.syncedRecords.Add(1)
			} else {
				unsynced += int64(rec.header.PayloadLen)
				// A recovered dirty file gets a fresh dirty-age stamp so the carve
				// age gate fires after a restart (approximate — the original write
				// time is not persisted; it is only a batching heuristic).
				if fi.firstDirtyNanos == 0 {
					fi.firstDirtyNanos = s.clock.Now().UnixNano()
				}
			}
		}
	}

	if missingIdx > 0 {
		logf("journal: WARN %d segment(s) missing .idx sidecar, rebuilding from segment scan (recovery slower)", missingIdx)
	}

	s.nextSeg.Store(maxSegID + 1)
	// nextVersion increments-then-returns, so storing maxVersion makes the next
	// issued LSN exactly max(observed)+1 — strictly past every replayed record.
	s.version.Store(maxVersion)
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
		shards[i] = sh
	}

	// Any pooled empty segment we did not adopt is an orphan.
	for ; poolPos < len(emptyPool); poolPos++ {
		orphans = append(orphans, emptyPool[poolPos].id)
		_ = emptyPool[poolPos].close()
	}

	s.sweepOrphans(orphans)
	s.shards = shards
	ok = true
	return nil
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
