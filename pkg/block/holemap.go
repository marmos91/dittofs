package block

import "sort"

// holemap.go is the shared hole-tracking foundation for the NFSv4.2 sparse-file
// operations (SEEK, READ_PLUS, DEALLOCATE, ALLOCATE; RFC 7862).
//
// DittoFS does not store an explicit hole bitmap. The authoritative hole map is
// the file's content-addressed block list (FileAttr.Blocks, sorted by Offset):
// any byte in [0, fileSize) that no BlockRef covers is a hole and reads back as
// zeros. The functions here derive data/hole boundaries from that list so SEEK,
// READ_PLUS and DEALLOCATE all report consistent results from one source of
// truth.
//
// Always-correct fallback: a file whose blocks contiguously cover [0, fileSize)
// (or one with no block list at all but where the caller treats the extent as
// data) yields a single data segment — the dense-file behavior. Conversely a
// file with size > 0 and no covering blocks is entirely a hole. Both follow
// from the same gap analysis below.

// SegmentKind classifies a byte range of a file as data or hole.
type SegmentKind uint8

const (
	// SegmentData is a range backed by stored content.
	SegmentData SegmentKind = iota
	// SegmentHole is a range with no backing content (reads as zeros).
	SegmentHole
)

// Segment is a maximal contiguous run of one kind within [0, fileSize).
// Start is inclusive, End is exclusive (End > Start for every emitted segment).
type Segment struct {
	Kind  SegmentKind
	Start uint64
	End   uint64
}

// Len returns the segment length in bytes.
func (s Segment) Len() uint64 { return s.End - s.Start }

// normalizedExtents returns the block-covered byte ranges within [0, fileSize),
// sorted by start and merged across touching/overlapping refs. A ref straddling
// fileSize is clamped to fileSize; refs entirely at/after fileSize are dropped.
// The result is the data coverage from which holes are the complement.
func normalizedExtents(refs []BlockRef, fileSize uint64) [][2]uint64 {
	if fileSize == 0 || len(refs) == 0 {
		return nil
	}
	ext := make([][2]uint64, 0, len(refs))
	for _, r := range refs {
		start := r.Offset
		if start >= fileSize {
			continue
		}
		end := r.Offset + uint64(r.Size)
		if end > fileSize {
			end = fileSize
		}
		if end <= start {
			continue
		}
		ext = append(ext, [2]uint64{start, end})
	}
	if len(ext) == 0 {
		return nil
	}
	// Caller invariant is sorted-by-offset, but DEALLOCATE splits and the
	// dual-read shim can produce unsorted input, so sort defensively (cheap;
	// block lists are small).
	sort.Slice(ext, func(i, j int) bool { return ext[i][0] < ext[j][0] })

	merged := ext[:1]
	for _, e := range ext[1:] {
		last := &merged[len(merged)-1]
		if e[0] <= last[1] { // touching or overlapping -> extend
			if e[1] > last[1] {
				last[1] = e[1]
			}
			continue
		}
		merged = append(merged, e)
	}
	return merged
}

// Segments returns the ordered data/hole segmentation of [0, fileSize) derived
// from refs. Segments tile the whole extent with no gaps or overlaps; an empty
// file (fileSize == 0) returns nil. A file with no covering blocks returns a
// single hole segment; a file fully covered by blocks returns a single data
// segment. Used by READ_PLUS to emit data and NFS4_CONTENT_HOLE runs.
func Segments(refs []BlockRef, fileSize uint64) []Segment {
	if fileSize == 0 {
		return nil
	}
	extents := normalizedExtents(refs, fileSize)
	if len(extents) == 0 {
		return []Segment{{Kind: SegmentHole, Start: 0, End: fileSize}}
	}

	segs := make([]Segment, 0, len(extents)*2+1)
	var pos uint64
	for _, e := range extents {
		if e[0] > pos {
			segs = append(segs, Segment{Kind: SegmentHole, Start: pos, End: e[0]})
		}
		segs = append(segs, Segment{Kind: SegmentData, Start: e[0], End: e[1]})
		pos = e[1]
	}
	if pos < fileSize {
		segs = append(segs, Segment{Kind: SegmentHole, Start: pos, End: fileSize})
	}
	return segs
}

// NextDataOffset implements SEEK_DATA: it returns the offset of the next byte
// at or after `from` that is backed by data, and true. If there is no data at
// or after `from` (the rest of the file is a hole, or `from` is at/after
// fileSize), it returns (0, false) — the caller maps this to NFS4ERR_NXIO per
// RFC 7862 Section 15.11.
func NextDataOffset(refs []BlockRef, fileSize, from uint64) (uint64, bool) {
	if from >= fileSize {
		return 0, false
	}
	for _, e := range normalizedExtents(refs, fileSize) {
		if e[1] <= from { // extent entirely before `from`
			continue
		}
		if e[0] <= from { // `from` is inside a data extent
			return from, true
		}
		return e[0], true // next data extent starts after `from`
	}
	return 0, false
}

// NextHoleOffset implements SEEK_HOLE: it returns the offset of the next byte
// at or after `from` that is not backed by data. Because the implicit region at
// and beyond EOF is always a (virtual) hole, a hole is always found for any
// `from <= fileSize`: when `from` sits inside the file's final data extent the
// returned offset is fileSize itself (RFC 7862 Section 15.11). For `from >=
// fileSize` it returns (0, false) so the caller can map it to NFS4ERR_NXIO.
func NextHoleOffset(refs []BlockRef, fileSize, from uint64) (uint64, bool) {
	if from >= fileSize {
		return 0, false
	}
	for _, e := range normalizedExtents(refs, fileSize) {
		if e[1] <= from { // extent entirely before `from`
			continue
		}
		if e[0] <= from {
			// `from` is inside this data extent; the hole begins where it ends.
			return e[1], true
		}
		// `from` is in a hole that precedes this extent.
		return from, true
	}
	// No data extent covers or follows `from`: `from` is already in a hole.
	return from, true
}

// PunchHole removes the byte range [offset, offset+length) from refs, returning
// the resulting block list (sorted by Offset). It is the metadata side of
// DEALLOCATE: a ref lying ENTIRELY within [offset, offset+length) is dropped (it
// is wholly consumed by the hole); every other ref — including one that only
// partially overlaps the range at its head or tail — is KEPT unchanged.
// length == 0 is a no-op.
//
// Why partial-overlap refs are kept (not clipped): a BlockRef is content
// addressed — its Hash covers exactly the original [Offset, Offset+Size) bytes,
// so it cannot be split or shortened without invalidating the hash. Crucially,
// the bytes of a partially-overlapping ref that fall OUTSIDE the punch range are
// real data that must survive the DEALLOCATE; clipping the ref would turn them
// into a (zero-reading) hole and lose data. Keeping the whole ref preserves
// those bytes. The punched sub-range itself is made to read as zeros by the
// block store (engine.PunchHole zero-overwrites exactly [offset, offset+length))
// — so the hole semantics hold without touching the surrounding data.
//
// This "drop only fully-contained refs" predicate matches engine.PunchHole's
// reap predicate, keeping the metadata and block-store views consistent.
func PunchHole(refs []BlockRef, offset, length uint64) []BlockRef {
	if length == 0 || len(refs) == 0 {
		return refs
	}
	end := offset + length
	out := make([]BlockRef, 0, len(refs))
	for _, r := range refs {
		rEnd := r.Offset + uint64(r.Size)
		// Drop only refs entirely inside the punched range; keep everything else
		// (non-overlapping and partially-overlapping) untouched so out-of-range
		// data bytes survive.
		if r.Offset >= offset && rEnd <= end {
			continue
		}
		out = append(out, r)
	}
	sortBlockRefsByOffset(out)
	return out
}
