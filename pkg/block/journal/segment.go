package journal

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Segment on-disk header, written at offset 0 of every .seg file
// (segHeaderSize bytes, little-endian). Records begin at segHeaderSize.
//
//	off  size  field
//	0    8     Magic       "DFSJRN1\0"
//	8    8     SegmentID
//	16   8     CreatedAt   unix nanos
//	24   4     Flags       bit0=sealed
//	28   4     HeaderCRC32 covers bytes [0,28)
//	32   32    Reserved
//
// The header is the source of truth on recovery: a set sealed bit means the
// segment is immutable and trusted without a full tail re-scan.
const (
	segHeaderSize      = 64
	segHeaderCRCCovers = 28
	segFlagSealed      = 1 << 0

	segIDFmt  = "%016d"
	segSuffix = ".seg"
	idxSuffix = ".idx"
)

var segMagic = [8]byte{'D', 'F', 'S', 'J', 'R', 'N', '1', 0}

// maxFileIDLen is the largest FileID a record can frame (FileIDLen is uint16).
const maxFileIDLen = 1<<16 - 1

// maxPayloadLen is the largest payload a record can frame (PayloadLen is uint32).
const maxPayloadLen int64 = 1<<32 - 1

// segmentMeta is the in-memory handle for one on-disk segment. The append
// cursor lives in tail; the byte counters feed eviction and GC.
type segmentMeta struct {
	id            uint64
	createdAt     time.Time // preserved across the seal header rewrite for age-gating
	sealed        atomic.Bool
	tail          atomic.Int64 // next append offset (also the on-disk file size)
	liveBytes     atomic.Int64
	deadBytes     atomic.Int64 // superseded/tombstoned payload bytes (GC input)
	records       atomic.Int64 // physical records appended (eviction synced-gate denominator)
	syncedRecords atomic.Int64 // records with the synced flag set (eviction gate)
	lastAccess    atomic.Int64 // unix nanos, approx-LRU victim key; 0 = never stamped
	// minVersion is the lowest record Version stored in this segment (0 = empty).
	// Segments fill sequentially, so a segment's records span a contiguous
	// [minVersion, maxVersion]; minVersion<=pinVersion means the segment holds a
	// record at or below a live snapshot's watermark and must not be evicted or
	// GC-repacked while that snapshot lives. Set on the first append and
	// reconstructed during the recovery scan and repack.
	minVersion atomic.Uint64
	// busy claims the segment for an exclusive whole-segment operation (evict or
	// GC repack). A claimer CAS-sets it true so eviction and GC never touch the
	// same sealed segment concurrently; warm reads and carve don't claim.
	busy atomic.Bool
	// corrupt marks a segment whose records failed an integrity check while a
	// repack was copying them forward. GC skips it until the store is reopened, so
	// one damaged segment cannot stall reclamation for the rest of the shard, and
	// its bytes stay on disk untouched for the read path to report or heal from
	// remote.
	corrupt atomic.Bool
	fd      *os.File
	idxFD   *os.File // persistent append handle for the .idx sidecar (nil if unavailable)
	// readGuard coordinates unlocked preads against a GC/eviction that unlinks the
	// segment: readers hold it shared across a pread, the reclaimer holds it
	// exclusive around close+unlink so no read touches a closed fd.
	readGuard sync.RWMutex
	// ponytail: the quotient-filter membership hint (rebuilt on repack) arrives
	// with GC; a linear index scan suffices until segments are large.
}

// noteMinVersion records v as a candidate lowest record Version for the segment,
// keeping the minimum ever seen (0 = unset). The pin predicate (reclaim.go) reads
// it to keep a segment holding at-or-below-pinVersion records off the eviction/GC
// path. Caller holds the shard lock on the append path; the CAS loop tolerates the
// recovery/repack callers that set it without one.
func (m *segmentMeta) noteMinVersion(v uint64) {
	for {
		cur := m.minVersion.Load()
		if cur != 0 && cur <= v {
			return
		}
		if m.minVersion.CompareAndSwap(cur, v) {
			return
		}
	}
}

// close closes the segment's data and index file descriptors.
func (m *segmentMeta) close() error {
	var firstErr error
	if m.idxFD != nil {
		if err := m.idxFD.Close(); err != nil {
			firstErr = err
		}
		m.idxFD = nil
	}
	if m.fd != nil {
		if err := m.fd.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// decodeSegHeader validates and parses a segment header. It checks the magic
// and header CRC; a failure means the file is not a well-formed segment (a torn
// create or unrelated file) and recovery treats it as an orphan.
func decodeSegHeader(buf []byte) (id uint64, createdAt time.Time, flags uint32, ok bool) {
	if len(buf) < segHeaderSize {
		return 0, time.Time{}, 0, false
	}
	if [8]byte(buf[0:8]) != segMagic {
		return 0, time.Time{}, 0, false
	}
	want := binary.LittleEndian.Uint32(buf[28:32])
	if crc(buf[:segHeaderCRCCovers]) != want {
		return 0, time.Time{}, 0, false
	}
	id = binary.LittleEndian.Uint64(buf[8:16])
	createdAt = time.Unix(0, int64(binary.LittleEndian.Uint64(buf[16:24])))
	flags = binary.LittleEndian.Uint32(buf[24:28])
	return id, createdAt, flags, true
}

func encodeSegHeader(id uint64, createdAt time.Time, flags uint32) []byte {
	buf := make([]byte, segHeaderSize)
	copy(buf[0:8], segMagic[:])
	binary.LittleEndian.PutUint64(buf[8:16], id)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(createdAt.UnixNano()))
	binary.LittleEndian.PutUint32(buf[24:28], flags)
	binary.LittleEndian.PutUint32(buf[28:32], crc(buf[:segHeaderCRCCovers]))
	return buf
}

// createSegment allocates the next global segment ID, creates its file and
// writes an unsealed header. The returned segment is ready to append at
// segHeaderSize.
func (s *Store) createSegment() (*segmentMeta, error) {
	id := s.nextSeg.Add(1) - 1
	path := s.segPath(id)
	fd, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: create segment %q: %w", path, err)
	}
	createdAt := s.clock.Now()
	if _, err := fd.WriteAt(encodeSegHeader(id, createdAt, 0), 0); err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("journal: write segment header %q: %w", path, err)
	}
	// Make the header and the directory entry durable before any record can be
	// committed into this segment: a later Commit fsyncs record bytes, but that
	// is worthless after a crash if the file itself or its header never reached
	// disk. Recovery trusts the header to tell active from sealed.
	if err := fd.Sync(); err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("journal: fsync segment header %q: %w", path, err)
	}
	if err := fsyncDir(s.dir); err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("journal: fsync dir %q: %w", s.dir, err)
	}
	// The .idx sidecar is best-effort: if it can't be opened, records still
	// append and the index is rebuildable from the .seg on recovery.
	idxFD, _ := os.OpenFile(s.idxPath(id), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	m := &segmentMeta{id: id, createdAt: createdAt, fd: fd, idxFD: idxFD}
	m.tail.Store(segHeaderSize)
	s.diskBytes.Add(segHeaderSize)
	return m, nil
}

// evictionAge is the approx-LRU victim key: when the segment was last touched,
// or when it was created if nothing has touched it. Only the append and read
// paths stamp lastAccess, so a segment can become evictable while still zero —
// a repack target is filled by writeDataRecord, and every sealed segment
// rebuilt at open starts unstamped. Zero would sort ahead of every real
// timestamp, offering exactly those segments to eviction first: the ones GC
// just consolidated, and after a restart every segment that survived it.
// createdAt is durable (it round-trips through the segment header, including
// the seal rewrite), so it orders unstamped segments by age instead.
func (seg *segmentMeta) evictionAge() int64 {
	if la := seg.lastAccess.Load(); la != 0 {
		return la
	}
	return seg.createdAt.UnixNano()
}

// fsyncDir flushes a directory's entries so a freshly created file survives a
// crash. Skipped on Windows: opening a directory read-only and calling fsync
// returns "Access is denied", and NTFS makes the create durable without an
// explicit dir flush — same treatment the fs block store uses (fs/compaction.go).
func fsyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// sealInPlace makes an appended-into segment immutable: fsync its record bytes,
// set the on-disk sealed bit, fsync again, then close its .idx append handle.
// Durability boundary: data is fsynced BEFORE the sealed bit so recovery never
// trusts a header whose records did not reach disk. The caller moves it into the
// sealed set. Used both by rotation and by GC when it seals a repack target.
func (m *segmentMeta) sealInPlace() error {
	if err := m.fd.Sync(); err != nil {
		return fmt.Errorf("journal: fsync before seal: %w", err)
	}
	// Preserve the original CreatedAt so age-gating stays correct.
	if _, err := m.fd.WriteAt(encodeSegHeader(m.id, m.createdAt, segFlagSealed), 0); err != nil {
		return fmt.Errorf("journal: set sealed bit: %w", err)
	}
	if err := m.fd.Sync(); err != nil {
		return fmt.Errorf("journal: fsync after seal: %w", err)
	}
	if m.idxFD != nil {
		_ = m.idxFD.Close()
		m.idxFD = nil
	}
	m.sealed.Store(true)
	return nil
}

// sealSegment seals the shard's active segment, moves it into the sealed set and
// opens a fresh active segment. The segment's existing fd stays open and serves
// warm reads from the sealed set; GC/eviction swap it only under its readGuard,
// so readRecord's snapshot-then-unlocked-read never touches a closed fd.
//
// Caller must hold sh.mu.
func (s *Store) sealSegment(sh *shard) error {
	old := sh.active
	if err := old.sealInPlace(); err != nil {
		sh.syncFailed.Store(true)
		return err
	}
	// Sealing fsyncs the segment, and every earlier segment of this shard was
	// fsynced when it was sealed, so every record appended so far is now durable:
	// a rotation is a durability point just like a Commit.
	sh.markSynced(sh.lastVersion)
	sh.sealed[old.id] = old

	seg, err := s.createSegment()
	if err != nil {
		return err
	}
	sh.active = seg
	return nil
}

// appendRecord frames data as a record and appends it to the file's shard.
// It assigns a fresh Version, writes header+FileID, payload and payload CRC as
// separate positioned writes (so a large payload is never copied), then indexes
// the payload's location. It never fsyncs.
//
// ponytail: the shard lock is held across those three writes, so records complete
// in Version order and the scalar sh.lastVersion is a sound fsync ceiling for
// groupCommit and DurableExtent. Writing unlocked would break that in either
// arrangement — stamp the version before the writes and a commit can fsync past
// bytes that have not been issued yet; stamp it after and a faster later record
// raises the ceiling over an earlier one still in flight — and both ack a version
// as durable while its bytes are only reserved, which is a hole of zeros after a
// crash. Dropping the lock therefore needs the watermark replaced by an in-flight
// version set (ceiling = lowest in-flight Version minus one) first; build that
// only if append contention on a shard actually shows up in a profile.
//
// synced marks a hydrate, whose range is gated below; notAfter is its caller's
// sampled WriteVersion, or 0 for no bound. See Store.Hydrate.
func (s *Store) appendRecord(ctx context.Context, id FileID, offset int64, data []byte, synced bool, notAfter uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed.Load() {
		return fmt.Errorf("journal: closed")
	}
	if len(data) == 0 {
		return nil
	}
	if offset < 0 {
		return fmt.Errorf("journal: negative offset %d", offset)
	}
	// Enforce the local-storage cap before buffering the write: evict cold synced
	// segments to make room, or backpressure when every segment is dirty-pinned.
	// A no-op when MaxLocalBytes is unset. Runs before the shard lock so eviction
	// (which locks shards) never contends with this writer's own shard.
	if err := s.ensureSpace(ctx, recordLen(len(id), len(data))); err != nil {
		return err
	}
	fileID := []byte(id)
	if len(fileID) > maxFileIDLen {
		return fmt.Errorf("journal: FileID length %d exceeds max %d", len(fileID), maxFileIDLen)
	}
	if int64(len(data)) > maxPayloadLen {
		return fmt.Errorf("journal: payload length %d exceeds max %d", len(data), int64(maxPayloadLen))
	}
	recLen := recordLen(len(fileID), len(data))
	if maxRec := s.cfg.SegmentSize - segHeaderSize; recLen > maxRec {
		return fmt.Errorf("journal: record size %d exceeds segment capacity %d", recLen, maxRec)
	}

	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	// A hydrate fills; it never overwrites. The range is re-tested here, under
	// the same lock that stamps the version, so a write landing between the
	// caller's split and this append cannot end up beneath older bytes. Anything
	// short of the whole range is dropped rather than trimmed — the caller split
	// on the same predicate, so a short answer means the range changed underneath
	// and the fetched bytes are the wrong ones.
	if synced {
		n := int64(len(data))
		if hydrateFenced(sh, id, notAfter) || !coversWhole(sh.index[id].hydratable(offset, n, notAfter), offset, n) {
			return nil
		}
	}

	if sh.active.tail.Load()+recLen > s.cfg.SegmentSize {
		if err := s.sealSegment(sh); err != nil {
			return err
		}
	}
	seg := sh.active
	segOff := seg.tail.Load()

	var flags uint8
	if synced {
		flags |= flagSynced
	}
	version := s.nextVersion()
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: uint64(offset),
		PayloadLen: uint32(len(data)),
		Version:    version,
		Flags:      flags,
	}, fileID)

	payloadOff := segOff + int64(len(hdr))
	if _, err := seg.fd.WriteAt(hdr, segOff); err != nil {
		return fmt.Errorf("journal: write record header: %w", err)
	}
	if _, err := seg.fd.WriteAt(data, payloadOff); err != nil {
		return fmt.Errorf("journal: write payload: %w", err)
	}
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], recordCRC(fileID, data))
	if _, err := seg.fd.WriteAt(crcBuf[:], payloadOff+int64(len(data))); err != nil {
		return fmt.Errorf("journal: write payload CRC: %w", err)
	}
	seg.tail.Store(segOff + recLen)
	// Stamped under sh.mu, after the record's writes returned: a reader holding
	// sh.mu therefore sees a Version only once its bytes are in the page cache,
	// which is what makes it safe for groupCommit to treat as the fsync's ceiling.
	sh.lastVersion = version
	seg.liveBytes.Add(int64(len(data)))
	seg.records.Add(1)
	seg.noteMinVersion(version)
	seg.lastAccess.Store(s.clock.Now().UnixNano())
	s.diskBytes.Add(recLen)
	if synced {
		seg.syncedRecords.Add(1)
	}

	// Best-effort .idx sidecar via the segment's persistent append handle; a
	// failure is a rebuildable performance event, never a lost write. The write
	// runs under sh.mu, so appends to idxFD stay ordered.
	if seg.idxFD != nil {
		_, _ = seg.idxFD.Write(idxEntry{
			FileIDHash: fnv1a(string(id)),
			FileOffset: uint64(offset),
			PayloadLen: uint32(len(data)),
			Version:    version,
			SegOffset:  uint64(payloadOff),
			Flags:      flags,
		}.encode())
	}

	fi := sh.indexFor(id)
	dirtyRemoved, dirtyAdded, dead := fi.insert(interval{
		fileOff: offset,
		length:  int64(len(data)),
		version: version,
		recOff:  segOff,
		synced:  synced,
		loc:     SegmentLocation{SegmentID: seg.id, Offset: payloadOff, Length: int64(len(data))},
	})
	// Charge superseded bytes to their segment's dead counter: this is the
	// size-tiered GC victim signal. sh.segment needs sh.mu, held here.
	for _, d := range dead {
		if ds := sh.segment(d.seg); ds != nil {
			ds.deadBytes.Add(d.bytes)
		}
	}

	s.writes.Add(1)
	// unsynced tracks live dirty bytes: add this write if dirty, always drop the
	// dirty bytes this write superseded (dead now, not evictable-pending), and add
	// any warm fragments this write re-marked dirty for re-carve. A synced
	// (Hydrate) write adds nothing itself but can still supersede or
	// re-dirty.
	dirtyDelta := dirtyAdded - dirtyRemoved
	if !synced {
		dirtyDelta += int64(len(data))
	}
	// Stamp the file's dirty age on the first dirty record so carve's age gate has
	// a reference without a per-interval timestamp — including a write that only
	// re-dirtied a straddle remainder.
	if fi.firstDirtyNanos == 0 && (!synced || dirtyAdded > 0) {
		fi.firstDirtyNanos = s.clock.Now().UnixNano()
	}
	if dirtyDelta != 0 {
		s.unsynced.Add(dirtyDelta)
	}
	return nil
}

// appendTombstone frames a zero-payload tombstone record for id and fsyncs the
// shard's active segment, returning the tombstone's Version. The fsync makes the
// delete at least as durable as any data record it shadows (which was durable
// only if fsynced), so recovery can never replay data whose tombstone was lost.
// The record is indexed too (an idxEntry with flagTombstone), so rebuildIdx and
// recovery suppress the file's older records without rescanning the .seg.
func (s *Store) appendTombstone(ctx context.Context, id FileID) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fileID := []byte(id)
	if len(fileID) > maxFileIDLen {
		return 0, fmt.Errorf("journal: FileID length %d exceeds max %d", len(fileID), maxFileIDLen)
	}
	if s.failTombstone != "" && id == s.failTombstone {
		// Test seam: model a durability failure (e.g. a failed fsync) before any
		// state is persisted. Delete must then leave its in-memory index untouched.
		return 0, fmt.Errorf("journal: injected tombstone failure for %q", id)
	}
	recLen := recordLen(len(fileID), 0)

	sh := s.shardFor(id)
	sh.mu.Lock()
	if sh.active.tail.Load()+recLen > s.cfg.SegmentSize {
		if err := s.sealSegment(sh); err != nil {
			sh.mu.Unlock()
			return 0, err
		}
	}
	seg := sh.active
	version := s.nextVersion()
	// Published in the same critical section that mints the Version, which is the
	// only place it closes the whole window. A hydrate that takes sh.mu after this
	// point is refused if its caller sampled a bound at or below the tombstone;
	// one that got in before it stamped a lower Version and the scrub in Delete
	// buries it. Stamping earlier (from Delete, before this call) leaves the fence
	// below the Version this mints, so a hydrate holding a bound in between passes
	// the fence, appends ABOVE the tombstone, and the scrub then reads it as a
	// rewrite that raced past the delete and deliberately keeps it. Stamping later
	// cannot help either: by then that record is already committed.
	sh.fenceDelete(id, version)
	recStart, err := writeTombstoneRecord(seg, id, version)
	if err != nil {
		sh.mu.Unlock()
		return 0, err
	}
	seg.noteMinVersion(version)
	if seg.idxFD != nil {
		_, _ = seg.idxFD.Write(idxEntry{
			FileIDHash: fnv1a(string(id)),
			Version:    version,
			SegOffset:  uint64(recStart + recordHeaderSize + int64(len(fileID))),
			Flags:      flagTombstone,
		}.encode())
	}
	sh.mu.Unlock()
	// Durability goes through the shard's commit leader rather than a private
	// fd.Sync: the record is already written, so a barrier that starts now covers
	// it, and a burst of concurrent deletes on one shard rides a single fsync
	// instead of one each.
	if err := sh.groupCommit(); err != nil {
		return 0, fmt.Errorf("journal: fsync tombstone: %w", err)
	}
	return version, nil
}

// appendTruncateMarker frames a zero-payload truncate marker for id (FileOffset
// carries newSize) and fsyncs the shard's active segment, returning the marker's
// Version. Like appendTombstone, the fsync makes the truncation at least as
// durable as any data record it clips, so recovery can never replay bytes past
// newSize whose marker was lost.
func (s *Store) appendTruncateMarker(ctx context.Context, id FileID, newSize int64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	fileID := []byte(id)
	if len(fileID) > maxFileIDLen {
		return 0, fmt.Errorf("journal: FileID length %d exceeds max %d", len(fileID), maxFileIDLen)
	}
	if s.failTruncate != "" && id == s.failTruncate {
		return 0, fmt.Errorf("journal: injected truncate failure for %q", id)
	}
	recLen := recordLen(len(fileID), 0)

	sh := s.shardFor(id)
	sh.mu.Lock()
	if sh.active.tail.Load()+recLen > s.cfg.SegmentSize {
		if err := s.sealSegment(sh); err != nil {
			sh.mu.Unlock()
			return 0, err
		}
	}
	seg := sh.active
	version := s.nextVersion()
	recStart, err := writeTruncateRecord(seg, id, version, newSize)
	if err != nil {
		sh.mu.Unlock()
		return 0, err
	}
	seg.noteMinVersion(version)
	if seg.idxFD != nil {
		_, _ = seg.idxFD.Write(idxEntry{
			FileIDHash: fnv1a(string(id)),
			FileOffset: uint64(newSize),
			Version:    version,
			SegOffset:  uint64(recStart + recordHeaderSize + int64(len(fileID))),
			Flags:      flagTruncate,
		}.encode())
	}
	sh.mu.Unlock()
	// Same commit-leader path as the tombstone: the marker's bytes are written,
	// so any barrier that starts after this point makes them durable.
	if err := sh.groupCommit(); err != nil {
		return 0, fmt.Errorf("journal: fsync truncate marker: %w", err)
	}
	return version, nil
}

// writeTruncateRecord frames a zero-payload truncate marker at seg's tail, with
// FileOffset carrying newSize, and advances the tail. Shared by the live
// truncate path and repack's marker carry-forward.
func writeTruncateRecord(seg *segmentMeta, id FileID, version uint64, newSize int64) (recStart int64, err error) {
	fileID := []byte(id)
	recStart = seg.tail.Load()
	hdr := encodeHeader(recordHeader{
		FileIDLen:  uint16(len(fileID)),
		FileOffset: uint64(newSize),
		Version:    version,
		Flags:      flagTruncate,
	}, fileID)
	if _, err = seg.fd.WriteAt(hdr, recStart); err != nil {
		return 0, fmt.Errorf("journal: write truncate header: %w", err)
	}
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], recordCRC(fileID, nil))
	if _, err = seg.fd.WriteAt(crcBuf[:], recStart+int64(len(hdr))); err != nil {
		return 0, fmt.Errorf("journal: write truncate CRC: %w", err)
	}
	seg.tail.Store(recStart + recordLen(len(fileID), 0))
	return recStart, nil
}

// writeTombstoneRecord frames a zero-payload tombstone at seg's tail and advances
// it. Shared by the delete path and by repack's tombstone carry-forward.
func writeTombstoneRecord(seg *segmentMeta, id FileID, version uint64) (recStart int64, err error) {
	fileID := []byte(id)
	recStart = seg.tail.Load()
	hdr := encodeHeader(recordHeader{
		FileIDLen: uint16(len(fileID)),
		Version:   version,
		Flags:     flagTombstone,
	}, fileID)
	if _, err = seg.fd.WriteAt(hdr, recStart); err != nil {
		return 0, fmt.Errorf("journal: write tombstone header: %w", err)
	}
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], recordCRC(fileID, nil))
	if _, err = seg.fd.WriteAt(crcBuf[:], recStart+int64(len(hdr))); err != nil {
		return 0, fmt.Errorf("journal: write tombstone CRC: %w", err)
	}
	seg.tail.Store(recStart + recordLen(len(fileID), 0))
	return recStart, nil
}

// readRecord resolves a segment under the shard lock, then reads and verifies
// the record at recOff, handing the whole verified record back for the caller to
// slice. It snapshots the segment fd under the lock and reads unlocked. Carve is
// its only caller and holds the shard's carveMu, which GC/eviction also hold
// before closing a segment, so the fd cannot close under it. (ReadAt does its
// own resolve-and-guard — see readAt.)
func (s *Store) readRecord(sh *shard, segID uint64, recOff int64, id FileID) (record, error) {
	sh.mu.Lock()
	seg := sh.segment(segID)
	sh.mu.Unlock()
	if seg == nil {
		return record{}, fmt.Errorf("journal: unknown segment %d", segID)
	}
	seg.lastAccess.Store(s.clock.Now().UnixNano())
	rec, err := readVerifiedRecord(seg.fd, recOff, s.cfg.SegmentSize, id, nil)
	if err != nil {
		return record{}, fmt.Errorf("journal: read segment %d record %d: %w", segID, recOff, err)
	}
	return rec, nil
}
