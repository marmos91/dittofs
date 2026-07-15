package journal

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
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
	tail          atomic.Int64 // next append offset
	liveBytes     atomic.Int64
	deadBytes     atomic.Int64 // superseded/tombstoned payload bytes (GC input)
	syncedRecords atomic.Int64 // records with the synced flag set (eviction gate)
	lastAccess    atomic.Int64 // unix nanos, approx-LRU victim key
	fd            *os.File
	idxFD         *os.File // persistent append handle for the .idx sidecar (nil if unavailable)
	// ponytail: the quotient-filter membership hint (rebuilt on repack) arrives
	// with GC; a linear index scan suffices until segments are large.
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
	return m, nil
}

// fsyncDir flushes a directory's entries so a freshly created file survives a
// crash. A directory Open+Sync is the portable way to fsync directory metadata.
func fsyncDir(dir string) error {
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

// sealSegment fsyncs the shard's active segment, flips its on-disk sealed bit,
// fsyncs again, moves it into the sealed set and opens a fresh active segment.
// Durability boundary: data is fsynced BEFORE the sealed bit is set, so a
// commit that a size-cap rotation sealed between checkpoints never loses bytes.
//
// Caller must hold sh.mu.
func (s *Store) sealSegment(sh *shard) error {
	old := sh.active
	if err := old.fd.Sync(); err != nil {
		return fmt.Errorf("journal: fsync before seal: %w", err)
	}
	// Rewrite the header with the sealed bit set, preserving the original
	// CreatedAt so age-gating stays correct.
	if _, err := old.fd.WriteAt(encodeSegHeader(old.id, old.createdAt, segFlagSealed), 0); err != nil {
		return fmt.Errorf("journal: set sealed bit: %w", err)
	}
	if err := old.fd.Sync(); err != nil {
		return fmt.Errorf("journal: fsync after seal: %w", err)
	}
	// A sealed segment is immutable: close its .idx append handle (the file
	// stays on disk, read-only).
	if old.idxFD != nil {
		_ = old.idxFD.Close()
		old.idxFD = nil
	}
	// Downgrade the segment's fd to O_RDONLY: sealed segments serve warm reads
	// only, and a read-only handle makes that immutability OS-enforced (mirrors
	// logblob's sealed read-only fd pool). If the reopen fails we keep the
	// existing writable fd — reads still work, we just skip the downgrade.
	if ro, err := os.Open(s.segPath(old.id)); err == nil {
		_ = old.fd.Close()
		old.fd = ro
	}
	old.sealed.Store(true)
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
func (s *Store) appendRecord(ctx context.Context, id FileID, offset int64, data []byte, synced bool) error {
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
	binary.LittleEndian.PutUint32(crcBuf[:], crc(data))
	if _, err := seg.fd.WriteAt(crcBuf[:], payloadOff+int64(len(data))); err != nil {
		return fmt.Errorf("journal: write payload CRC: %w", err)
	}
	seg.tail.Store(segOff + recLen)
	seg.liveBytes.Add(int64(len(data)))
	seg.lastAccess.Store(s.clock.Now().UnixNano())
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

	sh.indexFor(id).insert(interval{
		fileOff: offset,
		length:  int64(len(data)),
		version: version,
		loc:     SegmentLocation{SegmentID: seg.id, Offset: payloadOff, Length: int64(len(data))},
	})

	s.writes.Add(1)
	if !synced {
		s.unsynced.Add(int64(len(data)))
	}
	return nil
}

// readPayload preads length bytes of a record payload starting subOffset into
// the payload identified by loc, into dst. It snapshots the segment fd under
// the shard lock, then preads unlocked.
func (s *Store) readPayload(sh *shard, loc SegmentLocation, subOffset int64, dst []byte) (int, error) {
	sh.mu.Lock()
	seg := sh.segment(loc.SegmentID)
	sh.mu.Unlock()
	if seg == nil {
		return 0, fmt.Errorf("journal: unknown segment %d", loc.SegmentID)
	}
	seg.lastAccess.Store(s.clock.Now().UnixNano())
	n, err := seg.fd.ReadAt(dst, loc.Offset+subOffset)
	if err != nil {
		return n, fmt.Errorf("journal: read segment %d@%d: %w", loc.SegmentID, loc.Offset+subOffset, err)
	}
	return n, nil
}
