package index

import (
	"errors"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// ErrNotOffloaded refuses a release over held content whose offloaded bit is
// unset. A refused release changes nothing.
var ErrNotOffloaded = errors.New("index: release of content not offloaded")

// RecordAt names a record by where it sits: its segment, and its offset in
// that segment. Two pieces with equal RecordAt are pieces of one record.
type RecordAt struct {
	Segment core.SegmentID
	Offset  uint32
}

// Placed is what a read is served from: an extent of the file, the record
// holding its bytes, where in that record's payload the extent's first byte
// is, and the content version of those bytes. It carries no offloaded bit,
// because a read is never answered from the bit.
type Placed struct {
	Extent  core.Extent
	Record  RecordAt
	Skip    uint32
	Version core.Version
}

// Piece is one held extent as the index keeps it: its placement and its
// offloaded bit.
type Piece struct {
	Extent    core.Extent
	Record    RecordAt
	Skip      uint32
	Version   core.Version
	Offloaded bool
}

// The record offset, skip and length of a piece are kept in 32 bits, which
// holds because a record lies inside one segment. This conversion stops
// compiling if segments outgrow them.
const _ = uint32(core.SegmentCap)

// held is the tree value of one piece; the piece's start offset is its key.
// It is 40 bytes.
//
// ponytail: one entry per contiguous held piece of one record, which with the
// tree around it costs about 60 bytes for extents written front to back and
// about 82 for extents written in random order, so content written in small
// writes costs an entry a write until it is offloaded and released or
// repacked. Upgrade to entries that span consecutive records of one file with
// a version range, splitting on demand, when the extent bound is reached in
// production on a workload of small sequential writes.
type held struct {
	version   core.Version
	segment   core.SegmentID
	recordOff uint32
	skip      uint32
	len       uint32
	offloaded bool
}

func toHeld(p Piece) held {
	return held{
		version:   p.Version,
		segment:   p.Record.Segment,
		recordOff: p.Record.Offset,
		skip:      p.Skip,
		len:       uint32(p.Extent.Len),
		offloaded: p.Offloaded,
	}
}

func (h held) piece(k uint64) Piece {
	return Piece{
		Extent:    core.Extent{Off: int64(k), Len: int64(h.len)},
		Record:    RecordAt{Segment: h.segment, Offset: h.recordOff},
		Skip:      h.skip,
		Version:   h.version,
		Offloaded: h.offloaded,
	}
}

// joins reports whether r continues h as one piece of one record: the same
// record, the next payload byte, the same version and the same bit. Two such
// entries side by side are merged, so a piece is never kept as several. A
// record has one version, so the version test only guards against a caller
// that names one record with two.
//
// decision: a piece is split where its offloaded bit changes, so one record
// piece can be held as several entries, against the rule that a piece is one
// entry. The bit must be exact per byte: a release refuses anything not
// offloaded, and a report marks exactly what it names, so a block's commit
// can end inside a record, as an offer's artificial end does. Each mark adds
// at most two entries, and the extent bound never refuses a mark, though a
// mark can split entries. Revisit if marks are
// found splitting files toward that bound, by keeping the bit as ranges inside
// an entry instead.
func (h held) joins(r held) bool {
	return h.segment == r.segment && h.recordOff == r.recordOff && h.version == r.version &&
		h.offloaded == r.offloaded && uint64(h.skip)+uint64(h.len) == uint64(r.skip)
}

// clip returns the part of p inside e, its skip advanced to the part's first
// byte. p and e must overlap.
func clip(p Piece, e core.Extent) Piece {
	lo := max(p.Extent.Off, e.Off)
	hi := min(p.Extent.Off+p.Extent.Len, e.Off+e.Len)
	p.Skip += uint32(lo - p.Extent.Off)
	p.Extent = core.Extent{Off: lo, Len: hi - lo}
	return p
}

// heldSet is one file's held extents: disjoint pieces in a B-tree keyed on
// their start offsets, so the piece covering an offset is the predecessor of
// that offset. Every piece is one contiguous piece of one record, and two that
// join are kept as one entry.
//
// Reads (lookup, span, allOffloaded, pieces) change nothing, not even scratch
// space, so they can run together. Writes are serialised by the caller,
// against each other and against reads; the scratch slices are theirs, kept so that a write in steady
// state allocates nothing beyond the nodes the tree splits.
//
// The set decides overlaps by arrival: what is put last wins where it lands,
// and entries carry no sequence number. Rebuilding from records in any order
// must therefore decide precedence before records reach the set, or keep the
// numbers beside it.
//
// Offsets and lengths are trusted: an extent's offset is not negative, its end
// does not overflow, and a piece is no longer than a segment. Refusing others
// is the journal API's job, as for the tree.
type heldSet struct {
	tree    btree[held]
	scratch []Piece // the pieces cut is changing
	work    []Piece // the pieces a mark, fill or repoint will put, or a removal will cut
}

// len returns the number of entries.
func (s *heldSet) len() int { return s.tree.len() }

// visit calls fn on every piece overlapping e, in offset order and unclipped,
// until fn returns false. It costs O(log n + k) comparisons for k pieces.
func (s *heldSet) visit(e core.Extent, fn func(Piece) bool) {
	if e.Len <= 0 {
		return
	}
	start, end := uint64(e.Off), uint64(e.Off+e.Len)
	if k, v, ok := s.tree.floor(start); ok && k < start && k+uint64(v.len) > start {
		if !fn(v.piece(k)) {
			return
		}
	}
	s.tree.ascend(start, func(k uint64, v held) bool {
		return k < end && fn(v.piece(k))
	})
}

// lookup returns the entry covering off, or false where nothing is held. It
// costs one descent. The entry's bounds can end where its offloaded bit
// changes, which span hides; the record, version and payload offset it gives
// for off do not depend on the bit.
func (s *heldSet) lookup(off int64) (Placed, bool) {
	k, v, ok := s.tree.floor(uint64(off))
	if !ok || k+uint64(v.len) <= uint64(off) {
		return Placed{}, false
	}
	p := v.piece(k)
	return Placed{Extent: p.Extent, Record: p.Record, Skip: p.Skip, Version: p.Version}, true
}

// span appends to placed the pieces held inside e, clipped to it, and to missing
// the exact gaps between them, both in offset order. Neighbouring pieces of
// one record that differ only in their offloaded bit are returned as one, so
// nothing a read is told depends on the bit.
func (s *heldSet) span(e core.Extent, placed []Placed, missing []core.Extent) ([]Placed, []core.Extent) {
	if e.Len <= 0 {
		return placed, missing
	}
	first, cursor := len(placed), e.Off
	s.visit(e, func(p Piece) bool {
		q := clip(p, e)
		if q.Extent.Off > cursor {
			missing = append(missing, core.Extent{Off: cursor, Len: q.Extent.Off - cursor})
		}
		cursor = q.Extent.Off + q.Extent.Len
		// The version test, as in joins, only guards against a caller that
		// names one record with two versions.
		if n := len(placed); n > first {
			if last := &placed[n-1]; last.Extent.Off+last.Extent.Len == q.Extent.Off &&
				last.Record == q.Record && last.Version == q.Version &&
				uint64(last.Skip)+uint64(last.Extent.Len) == uint64(q.Skip) {
				last.Extent.Len += q.Extent.Len
				return true
			}
		}
		placed = append(placed, Placed{Extent: q.Extent, Record: q.Record, Skip: q.Skip, Version: q.Version})
		return true
	})
	if end := e.Off + e.Len; end > cursor {
		missing = append(missing, core.Extent{Off: cursor, Len: end - cursor})
	}
	return placed, missing
}

// insert holds p as a client write: dirty, superseding whatever was held in
// its extent. A piece it covers whole is dropped, and one it covers in part is
// narrowed, or split in three when p lies inside it. It takes no offloaded
// bit, so a write can never hold content that eviction may drop.
func (s *heldSet) insert(p Placed) {
	if p.Extent.Len <= 0 {
		return
	}
	s.put(Piece{Extent: p.Extent, Record: p.Record, Skip: p.Skip, Version: p.Version})
}

// fill holds p, marked offloaded, only where nothing is held: a held piece
// keeps its record, version and bit. Like every Placed, p names one extent
// of one record, whose bytes sit in p.Record from p.Skip on; the gaps take
// the matching parts of it. A fill that packs several absent extents into one
// record calls fill once for each of them, with each one's own skip.
func (s *heldSet) fill(p Placed) {
	s.work = s.work[:0]
	cursor, end := p.Extent.Off, p.Extent.Off+p.Extent.Len
	gap := func(lo, hi int64) {
		s.work = append(s.work, Piece{
			Extent:    core.Extent{Off: lo, Len: hi - lo},
			Record:    p.Record,
			Skip:      p.Skip + uint32(lo-p.Extent.Off),
			Version:   p.Version,
			Offloaded: true,
		})
	}
	s.visit(p.Extent, func(q Piece) bool {
		if q.Extent.Off > cursor {
			gap(cursor, q.Extent.Off)
		}
		cursor = q.Extent.Off + q.Extent.Len
		return true
	})
	if end > cursor {
		gap(cursor, end)
	}
	for _, q := range s.work {
		s.put(q)
	}
}

// allOffloaded reports whether every piece held inside e has its offloaded
// bit set. A gap has no bit and does not make it false.
func (s *heldSet) allOffloaded(e core.Extent) bool {
	all := true
	s.visit(e, func(p Piece) bool {
		all = p.Offloaded
		return all
	})
	return all
}

// release stops holding e, splitting or narrowing the pieces it covers in
// part, and appends to out each piece released with its version, which the
// release records name. It leaves no marker. It is refused with
// ErrNotOffloaded, changing nothing, when any piece held inside e is not
// offloaded.
//
// decision: a gap inside e is not a refusal. Nothing is held there, so there
// is no content whose only copy a release could drop, and refusing would stop
// an eviction over an extent part of which was already released or never
// filled. Revisit if a caller is found to need "release exactly what I name"
// to fail when part of it is gone.
func (s *heldSet) release(e core.Extent, out []core.Held) ([]core.Held, error) {
	if !s.allOffloaded(e) {
		return out, ErrNotOffloaded
	}
	s.visit(e, func(p Piece) bool {
		q := clip(p, e)
		out = append(out, core.Held{Extent: q.Extent, Version: q.Version})
		return true
	})
	s.cut(e)
	return out, nil
}

// markOffloaded sets the offloaded bit of the content held inside e whose
// version lies in [oldest, newest], and only inside e, splitting a piece at
// e's ends. A newer version written over reported content stays dirty: the
// report is about the bytes that were offloaded, not the ones that replaced
// them. It returns the number of pieces it marked, and costs O(log n) per
// piece, never a scan of the file's other pieces.
//
// ponytail: each marked piece goes through put, about nine descents: cutting,
// setting and checking both ends for a merge. A piece the report covers whole
// could have its bit flipped in place and its two ends checked, about half
// that. Do so when report cost shows in a profile of offload.
func (s *heldSet) markOffloaded(e core.Extent, oldest, newest core.Version) int {
	s.work = s.work[:0]
	s.visit(e, func(p Piece) bool {
		if !p.Offloaded && p.Version.Compare(oldest) >= 0 && p.Version.Compare(newest) <= 0 {
			q := clip(p, e)
			q.Offloaded = true
			s.work = append(s.work, q)
		}
		return true
	})
	for _, q := range s.work {
		s.put(q)
	}
	return len(s.work)
}

// repoint moves the pieces inside e held from record from to record to,
// keeping their extents, skips, versions and bits: a record re-appended after
// a failed sync is the same record in a new place. Only pieces of from move,
// so carrying forward a header-only record, which no piece is held from,
// changes nothing. It returns the number of pieces moved.
func (s *heldSet) repoint(e core.Extent, from, to RecordAt) int {
	s.work = s.work[:0]
	s.visit(e, func(p Piece) bool {
		if p.Record == from {
			q := clip(p, e)
			q.Record = to
			s.work = append(s.work, q)
		}
		return true
	})
	for _, q := range s.work {
		s.put(q)
	}
	return len(s.work)
}

// pieces appends every piece to out, in offset order. It costs O(n).
func (s *heldSet) pieces(out []Piece) []Piece {
	s.tree.ascend(0, func(k uint64, v held) bool {
		out = append(out, v.piece(k))
		return true
	})
	return out
}

// put holds p over whatever is held in its extent, then merges it with a
// neighbour it joins.
func (s *heldSet) put(p Piece) {
	s.cut(p.Extent)
	s.tree.set(uint64(p.Extent.Off), toHeld(p))
	s.coalesce(p.Extent.Off + p.Extent.Len)
	s.coalesce(p.Extent.Off)
}

// cutBelow stops holding the parts inside e of every piece whose version is
// below v, whatever its offloaded bit, and keeps the rest: a removal marker's
// rule. At run time a removal's version is above everything held, so it cuts
// all of e; a rebuild applying records in any order can meet content at or
// above it, which stays. It costs O(log n) per piece cut.
func (s *heldSet) cutBelow(e core.Extent, v core.Version) {
	s.work = s.work[:0]
	s.visit(e, func(p Piece) bool {
		if p.Version.Compare(v) < 0 {
			s.work = append(s.work, clip(p, e))
		}
		return true
	})
	for _, p := range s.work {
		s.cut(p.Extent)
	}
}

// cut stops holding e: pieces inside it are dropped, and pieces crossing its
// ends keep their parts outside it.
func (s *heldSet) cut(e core.Extent) {
	s.scratch = s.scratch[:0]
	s.visit(e, func(p Piece) bool {
		s.scratch = append(s.scratch, p)
		return true
	})
	end := e.Off + e.Len
	for _, p := range s.scratch {
		pend := p.Extent.Off + p.Extent.Len
		if p.Extent.Off < e.Off {
			head := p
			head.Extent.Len = e.Off - p.Extent.Off
			s.tree.set(uint64(p.Extent.Off), toHeld(head))
		} else {
			s.tree.delete(uint64(p.Extent.Off))
		}
		if pend > end {
			tail := clip(p, core.Extent{Off: end, Len: pend - end})
			s.tree.set(uint64(end), toHeld(tail))
		}
	}
}

// coalesce merges the piece starting at off into the one ending there, when
// it continues it as one piece of one record.
func (s *heldSet) coalesce(off int64) {
	if off <= 0 {
		return
	}
	at := uint64(off)
	k, r, ok := s.tree.floor(at)
	if !ok || k != at {
		return
	}
	lk, l, ok := s.tree.floor(at - 1)
	if !ok || lk+uint64(l.len) != at || !l.joins(r) {
		return
	}
	s.tree.delete(at)
	l.len += r.len
	s.tree.set(lk, l)
}
