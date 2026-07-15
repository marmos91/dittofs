package journal

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
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
// is the record's global LSN; it breaks ties when overlapping writes are indexed
// out of arrival order (recovery/repack), where a lower version must never
// supersede a higher one. loc.Offset always points at the segment byte for
// fileOff, so splitting an interval only advances loc.Offset by the trimmed
// prefix.
type interval struct {
	fileOff int64
	length  int64
	version uint64
	loc     SegmentLocation
	// recOff is the byte offset of the owning record's header start within its
	// segment. Carve flips that record's synced flag in place at recOff, so it
	// survives an interval split (trimming the live range never moves the record).
	recOff int64
	// synced is true once the bytes are durable on the remote store (a Hydrate
	// write, or a carved record whose block committed). A dirty (synced=false,
	// non-cold) interval is what a carve pass snapshots and packs.
	synced bool
	// cold marks a range that was written and is durable remotely but whose
	// local bytes have been evicted. Distinct from an absent interval (a true
	// hole): a cold range is served by fetching from the remote store.
	cold bool
}

func (iv interval) end() int64 { return iv.fileOff + iv.length }

// clamp returns the sub-interval over [lo, hi), advancing the segment location's
// offset by the trimmed prefix so it keeps pointing at the byte for lo. Caller
// guarantees iv.fileOff <= lo < hi <= iv.end().
func (iv interval) clamp(lo, hi int64) interval {
	frontTrim := lo - iv.fileOff
	return interval{
		fileOff: lo,
		length:  hi - lo,
		version: iv.version,
		recOff:  iv.recOff,
		synced:  iv.synced,
		cold:    iv.cold,
		loc: SegmentLocation{
			SegmentID: iv.loc.SegmentID,
			Offset:    iv.loc.Offset + frontTrim,
			Length:    hi - lo,
		},
	}
}

// without returns the fragments of iv left after removing [rs, re): the left
// and/or right survivors, loc offsets adjusted. A non-overlapping range returns
// iv unchanged; a range covering iv entirely returns nothing.
func (iv interval) without(rs, re int64) []interval {
	var out []interval
	if iv.fileOff < rs {
		out = append(out, iv.clamp(iv.fileOff, min(iv.end(), rs)))
	}
	if iv.end() > re {
		out = append(out, iv.clamp(max(iv.fileOff, re), iv.end()))
	}
	return out
}

// fileIndex is the per-file coverage set: a slice of non-overlapping intervals
// sorted by fileOff. Newest-wins is resolved at insert time by punching the new
// write's range out of the older intervals it overlaps (version breaks ties for
// out-of-order inserts), so a query is a binary search plus a forward walk
// rather than an O(n) version scan.
type fileIndex struct {
	ivs []interval
	// firstDirtyNanos is the clock time the file's first still-dirty interval was
	// recorded (0 when the file is fully synced). It drives the carve age gate
	// without a per-interval timestamp; carve clears it once no dirty interval
	// remains. Approximate by design — it is a batching heuristic, not a deadline.
	firstDirtyNanos int64
}

// findRecord returns the index of the live interval that starts at or before
// fileOff, covers it, and carries the given version, or -1 if no such interval
// exists (it was superseded by a newer write since the caller sampled it).
func (fi *fileIndex) findRecord(fileOff int64, version uint64) int {
	k := sort.Search(len(fi.ivs), func(i int) bool { return fi.ivs[i].end() > fileOff })
	if k < len(fi.ivs) && fi.ivs[k].fileOff <= fileOff && fi.ivs[k].version == version {
		return k
	}
	return -1
}

// insert records iv, resolving overlaps by version (highest wins). Existing
// intervals with a higher version shadow iv; those with a lower version are
// trimmed to make room. The result stays sorted and non-overlapping.
func (fi *fileIndex) insert(iv interval) {
	ns, ne := iv.fileOff, iv.end()
	// First interval that can overlap [ns, ne): end() is strictly increasing
	// across the non-overlapping sorted set, so the predicate is monotone.
	lo := sort.Search(len(fi.ivs), func(k int) bool { return fi.ivs[k].end() > ns })

	newFrags := []interval{iv}
	var survivors []interval
	hi := lo
	for hi < len(fi.ivs) && fi.ivs[hi].fileOff < ne {
		e := fi.ivs[hi]
		if e.version > iv.version {
			// Existing wins the overlap: keep it whole, carve iv around it.
			survivors = append(survivors, e)
			newFrags = subtractAll(newFrags, e.fileOff, e.end())
		} else {
			// New wins the overlap: keep only the parts of e outside iv.
			survivors = append(survivors, e.without(ns, ne)...)
		}
		hi++
	}

	merged := append(survivors, newFrags...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].fileOff < merged[j].fileOff })
	fi.ivs = slices.Replace(fi.ivs, lo, hi, merged...)
}

// subtractAll removes [rs, re) from every fragment, flattening the survivors.
func subtractAll(frags []interval, rs, re int64) []interval {
	var out []interval
	for _, f := range frags {
		out = append(out, f.without(rs, re)...)
	}
	return out
}

// piece is one contiguous span of a read: a segment read, a hole, or a cold
// (evicted, remote-resident) range.
type piece struct {
	dstStart int64
	dstEnd   int64
	loc      SegmentLocation
	subOff   int64
	hole     bool
	cold     bool
}

// plan resolves the read [offset, offset+n) into contiguous pieces. Intervals
// are non-overlapping and sorted, so a covering interval is found by binary
// search and gaps between them become holes.
func (fi *fileIndex) plan(offset, n int64) []piece {
	end := offset + n
	i := sort.Search(len(fi.ivs), func(k int) bool { return fi.ivs[k].end() > offset })
	var pieces []piece
	pos := offset
	for pos < end {
		if i >= len(fi.ivs) || fi.ivs[i].fileOff >= end {
			pieces = append(pieces, piece{dstStart: pos - offset, dstEnd: end - offset, hole: true})
			break
		}
		iv := fi.ivs[i]
		if pos < iv.fileOff { // gap before this interval
			pieces = append(pieces, piece{dstStart: pos - offset, dstEnd: iv.fileOff - offset, hole: true})
			pos = iv.fileOff
		}
		spanEnd := min(iv.end(), end)
		p := piece{dstStart: pos - offset, dstEnd: spanEnd - offset}
		if iv.cold {
			p.cold = true
		} else {
			p.loc = iv.loc
			p.subOff = pos - iv.fileOff
		}
		pieces = append(pieces, p)
		pos = spanEnd
		i++
	}
	return pieces
}

// ReadAt fills dst with the file's bytes at offset. Ranges never written locally
// are POSIX holes and are zero-filled. Ranges written but evicted are reported
// via cold so the caller can hydrate from the remote store and retry; their dst
// bytes are zero-filled as a placeholder until that cold-read path is wired.
func (s *Store) ReadAt(ctx context.Context, id FileID, offset int64, dst []byte) (n int, cold bool, err error) {
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if s.closed.Load() {
		return 0, false, errClosed
	}
	if offset < 0 {
		return 0, false, fmt.Errorf("journal: negative offset %d", offset)
	}
	if len(dst) == 0 {
		return 0, false, nil
	}
	s.reads.Add(1)

	sh := s.shardFor(id)
	sh.mu.Lock()
	fi := sh.index[id]
	if fi == nil { // unknown file: all hole
		sh.mu.Unlock()
		clear(dst)
		return len(dst), false, nil
	}
	pieces := fi.plan(offset, int64(len(dst)))
	sh.mu.Unlock()

	for _, p := range pieces {
		seg := dst[p.dstStart:p.dstEnd]
		switch {
		case p.hole:
			clear(seg)
		case p.cold:
			clear(seg)
			cold = true
			s.coldReads.Add(1)
		default:
			if _, err := s.readPayload(sh, p.loc, p.subOff, seg); err != nil {
				return int(p.dstStart), false, err
			}
		}
	}
	return len(dst), cold, nil
}

// DataExtents returns the merged [start, end) byte ranges that hold written data
// — including evicted (cold) ranges, which are still logically present — clamped
// to fileSize. A caller uses it to answer SEEK_DATA/SEEK_HOLE.
func (s *Store) DataExtents(ctx context.Context, id FileID, fileSize int64) ([][2]uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed.Load() {
		return nil, errClosed
	}
	if fileSize < 0 {
		return nil, fmt.Errorf("journal: negative fileSize %d", fileSize)
	}
	sh := s.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	fi := sh.index[id]
	if fi == nil || len(fi.ivs) == 0 {
		return nil, nil
	}

	var out [][2]uint64
	curStart, curEnd := fi.ivs[0].fileOff, fi.ivs[0].end()
	for _, iv := range fi.ivs[1:] {
		if iv.fileOff <= curEnd { // adjacent or overlapping (overlap only via versioned splits)
			curEnd = max(curEnd, iv.end())
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
