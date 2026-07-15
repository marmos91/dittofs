package segstore

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
//	0    8     Magic       "DFSSEG1\0"
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

var segMagic = [8]byte{'D', 'F', 'S', 'S', 'E', 'G', '1', 0}

// segmentMeta is the in-memory handle for one on-disk segment. The append
// cursor lives in tail; the byte counters feed eviction and GC.
type segmentMeta struct {
	id            uint64
	sealed        atomic.Bool
	tail          atomic.Int64 // next append offset
	liveBytes     atomic.Int64
	deadBytes     atomic.Int64 // superseded/tombstoned payload bytes (GC input)
	syncedRecords atomic.Int64 // records with the synced flag set (eviction gate)
	lastAccess    atomic.Int64 // unix nanos, approx-LRU victim key
	fd            *os.File
	// ponytail: the quotient-filter membership hint (rebuilt on repack) arrives
	// with GC; a linear index scan suffices until segments are large.
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
		return nil, fmt.Errorf("segstore: create segment %q: %w", path, err)
	}
	if _, err := fd.WriteAt(encodeSegHeader(id, s.clock.Now(), 0), 0); err != nil {
		_ = fd.Close()
		return nil, fmt.Errorf("segstore: write segment header %q: %w", path, err)
	}
	m := &segmentMeta{id: id, fd: fd}
	m.tail.Store(segHeaderSize)
	return m, nil
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
		return fmt.Errorf("segstore: fsync before seal: %w", err)
	}
	if _, err := old.fd.WriteAt(encodeSegHeader(old.id, time.Unix(0, 0), segFlagSealed), 0); err != nil {
		return fmt.Errorf("segstore: set sealed bit: %w", err)
	}
	if err := old.fd.Sync(); err != nil {
		return fmt.Errorf("segstore: fsync after seal: %w", err)
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
		return fmt.Errorf("segstore: closed")
	}
	if len(data) == 0 {
		return nil
	}
	fileID := []byte(id)
	recLen := recordLen(len(fileID), len(data))

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
		return fmt.Errorf("segstore: write record header: %w", err)
	}
	if _, err := seg.fd.WriteAt(data, payloadOff); err != nil {
		return fmt.Errorf("segstore: write payload: %w", err)
	}
	var crcBuf [payloadCRCSize]byte
	binary.LittleEndian.PutUint32(crcBuf[:], crc(data))
	if _, err := seg.fd.WriteAt(crcBuf[:], payloadOff+int64(len(data))); err != nil {
		return fmt.Errorf("segstore: write payload CRC: %w", err)
	}
	seg.tail.Store(segOff + recLen)
	seg.liveBytes.Add(int64(len(data)))
	seg.lastAccess.Store(s.clock.Now().UnixNano())
	if synced {
		seg.syncedRecords.Add(1)
	}

	// Best-effort .idx sidecar; a failure is a rebuildable performance event,
	// never a lost write.
	s.appendIdx(seg.id, idxEntry{
		FileIDHash: fnv1a(string(id)),
		FileOffset: uint64(offset),
		PayloadLen: uint32(len(data)),
		Version:    version,
		SegOffset:  uint64(payloadOff),
		Flags:      flags,
	})

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
		return 0, fmt.Errorf("segstore: unknown segment %d", loc.SegmentID)
	}
	seg.lastAccess.Store(s.clock.Now().UnixNano())
	n, err := seg.fd.ReadAt(dst, loc.Offset+subOffset)
	if err != nil {
		return n, fmt.Errorf("segstore: read segment %d@%d: %w", loc.SegmentID, loc.Offset+subOffset, err)
	}
	return n, nil
}
