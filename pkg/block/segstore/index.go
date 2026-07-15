package segstore

import (
	"context"
	"encoding/binary"
	"os"
	"sort"
)

// SegmentLocation points at a record payload inside a shared segment. It
// replaces the old one-file-per-payload location: because segments are shared,
// an index entry can no longer assume it names its own file's blob.
type SegmentLocation struct {
	SegmentID uint64
	Offset    int64 // byte offset of the payload within the segment
	Length    int64 // payload length
}

// interval maps a logical file range to the segment bytes holding it. version
// disambiguates overlapping writes: highest version wins.
type interval struct {
	fileOff int64
	length  int64
	version uint64
	loc     SegmentLocation
}

func (iv interval) end() int64 { return iv.fileOff + iv.length }

// fileIndex is the per-file coverage set. Newest-wins is resolved at query
// time by picking the highest-version interval covering a position.
//
// ponytail: an append-only slice with a boundary-walk query, O(n) per read.
// The interval-tree rekeying that makes this sublinear lands in the index PR;
// this holds correctly (arbitrary overlaps included) until segments get large.
type fileIndex struct {
	ivs []interval
}

func (fi *fileIndex) insert(iv interval) { fi.ivs = append(fi.ivs, iv) }

// piece is one contiguous span of a read: either a segment read or a hole.
type piece struct {
	dstStart int64
	dstEnd   int64
	loc      SegmentLocation
	subOff   int64
	hole     bool
}

// plan resolves the read [offset, offset+n) into contiguous pieces. Boundaries
// fall at every interval start/end, so within a piece exactly one interval is
// the newest covering it.
func (fi *fileIndex) plan(offset, n int64) []piece {
	end := offset + n
	var pieces []piece
	pos := offset
	for pos < end {
		// Newest interval covering pos, and the next boundary after pos.
		var winner *interval
		nextBoundary := end
		for i := range fi.ivs {
			iv := &fi.ivs[i]
			if iv.fileOff <= pos && pos < iv.end() {
				if winner == nil || iv.version > winner.version {
					winner = iv
				}
			}
			if iv.fileOff > pos && iv.fileOff < nextBoundary {
				nextBoundary = iv.fileOff
			}
			if iv.end() > pos && iv.end() < nextBoundary {
				nextBoundary = iv.end()
			}
		}
		spanEnd := nextBoundary
		if winner != nil && winner.end() < spanEnd {
			spanEnd = winner.end()
		}
		p := piece{dstStart: pos - offset, dstEnd: spanEnd - offset}
		if winner == nil {
			p.hole = true
		} else {
			p.loc = winner.loc
			p.subOff = pos - winner.fileOff
		}
		pieces = append(pieces, p)
		pos = spanEnd
	}
	return pieces
}

// ReadAt fills dst with the file's bytes at offset. Ranges never written locally
// are POSIX holes and are zero-filled. cold reports whether any range was served
// from the remote store (always false until eviction/cold-read land).
func (s *Store) ReadAt(ctx context.Context, id FileID, offset int64, dst []byte) (n int, cold bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if len(dst) == 0 {
		return 0, false, nil
	}
	s.reads.Add(1)

	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	var pieces []piece
	if fi != nil {
		pieces = fi.plan(offset, int64(len(dst)))
	}
	sh.mu.Unlock()

	if len(pieces) == 0 { // unknown file: all hole
		for i := range dst {
			dst[i] = 0
		}
		return len(dst), false, nil
	}

	for _, p := range pieces {
		seg := dst[p.dstStart:p.dstEnd]
		if p.hole {
			for i := range seg {
				seg[i] = 0
			}
			continue
		}
		if _, err := s.readPayload(sh, p.loc, p.subOff, seg); err != nil {
			return int(p.dstStart), false, err
		}
	}
	return len(dst), false, nil
}

// DataExtents returns the merged [start,end) byte ranges that hold data,
// clamped to fileSize. A caller uses it to answer SEEK_DATA/SEEK_HOLE.
func (s *Store) DataExtents(ctx context.Context, id FileID, fileSize int64) ([][2]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	var ivs []interval
	if fi != nil {
		ivs = append(ivs, fi.ivs...)
	}
	sh.mu.Unlock()
	if len(ivs) == 0 {
		return nil, nil
	}

	sort.Slice(ivs, func(i, j int) bool { return ivs[i].fileOff < ivs[j].fileOff })
	var out [][2]uint64
	curStart, curEnd := ivs[0].fileOff, ivs[0].end()
	for _, iv := range ivs[1:] {
		if iv.fileOff <= curEnd { // overlap or adjacency
			if iv.end() > curEnd {
				curEnd = iv.end()
			}
			continue
		}
		out = appendExtent(out, curStart, curEnd, fileSize)
		curStart, curEnd = iv.fileOff, iv.end()
	}
	out = appendExtent(out, curStart, curEnd, fileSize)
	return out, nil
}

func appendExtent(out [][2]uint64, start, end, fileSize int64) [][2]uint64 {
	if end > fileSize {
		end = fileSize
	}
	if start >= end {
		return out
	}
	return append(out, [2]uint64{uint64(start), uint64(end)})
}

// idxEntry is one 40-byte record in a segment's .idx sidecar. The sidecar is
// lazy and best-effort: losing it only forces a re-scan of the sibling .seg.
//
//	off  size  field
//	0    8     FileIDHash  FNV-1a of FileID (same hash as shardFor)
//	8    8     FileOffset
//	16   4     PayloadLen
//	20   8     Version
//	28   8     SegOffset   payload byte offset in the .seg
//	36   1     Flags
//	37   3     pad
type idxEntry struct {
	FileIDHash uint64
	FileOffset uint64
	PayloadLen uint32
	Version    uint64
	SegOffset  uint64
	Flags      uint8
}

const idxEntrySize = 40

func (e idxEntry) encode() []byte {
	buf := make([]byte, idxEntrySize)
	binary.LittleEndian.PutUint64(buf[0:8], e.FileIDHash)
	binary.LittleEndian.PutUint64(buf[8:16], e.FileOffset)
	binary.LittleEndian.PutUint32(buf[16:20], e.PayloadLen)
	binary.LittleEndian.PutUint64(buf[20:28], e.Version)
	binary.LittleEndian.PutUint64(buf[28:36], e.SegOffset)
	buf[36] = e.Flags
	return buf
}

// appendIdx appends an entry to a segment's .idx sidecar, best-effort: any
// failure is swallowed (the sidecar is rebuildable from the .seg).
func (s *Store) appendIdx(segID uint64, e idxEntry) {
	f, err := os.OpenFile(s.idxPath(segID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(e.encode())
	_ = f.Close()
}
