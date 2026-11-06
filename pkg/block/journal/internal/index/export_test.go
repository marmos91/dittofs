package index

import (
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/cubbit/dittofs/pkg/block/journal/internal/core"
)

// decision: the model-based fuzz runs in the external test package, index_test,
// and reaches the index through this file's exported harness. The model
// imports the journal's public package, which will import this one once the
// journal is wired, so a test of package index importing the model would be
// an import cycle; an external test package may import both.

// ModelRecord is the payload the fuzz harness keeps for one record it
// placed: the bytes the record's first extent begins with, and the mtime the
// write named.
type ModelRecord struct {
	Data  []byte
	Mtime int64
}

// ModelIndex drives the index the way the journal will: each operation checks
// the file's tag first, draws its version, admits and commits with the record
// noted, so the fuzz sees the index through the same order of steps the
// running journal's calls take.
type ModelIndex struct {
	fs    *files
	ctr   Counters
	recs  map[RecordAt]ModelRecord
	slots uint32         // records placed so far
	last  core.SegmentID // highest segment placed

	// sum is the running sum of every delta an update returned, and
	// dirtyFiles the files the Dirty transitions say are on the dirty-file
	// list: what CheckDeltas compares with the read-out.
	sum        heldTotals
	byTag      map[core.ShareTag]heldTotals // the same sums by each delta's tag
	dirtyFiles map[core.FileID]bool

	// err is the first transition that contradicted the dirty-file list.
	err error
}

// NewModelIndex returns an empty index-backed harness: no extent bound, no
// bound on versions, as the fuzz never reaches either.
func NewModelIndex() *ModelIndex {
	m := &ModelIndex{fs: &files{}, recs: map[RecordAt]ModelRecord{}, dirtyFiles: map[core.FileID]bool{}, byTag: map[core.ShareTag]heldTotals{}}
	m.fs.setExtentLimit(1 << 40)
	_ = m.ctr.SetVersionLimit(core.Version{Counter: 1 << 40})
	return m
}

// BucketOf returns the floor bucket id falls in, so a test can check where a
// file lands.
func BucketOf(id core.FileID) int { return bucketOf(id) }

// place draws the record home for one record: the next free 4 KiB slot of the
// segment the placements have reached, sixteen records to a segment, and the
// numbers the journal would have drawn. Every record gets a home of its own,
// so no record shares a placement with another.
func (m *ModelIndex) place(v core.Version) (RecordAt, Numbers) {
	r := RecordAt{Segment: 1 + core.SegmentID(m.slots/16), Offset: (m.slots % 16) * 4096}
	seq, err := m.ctr.NextSeq()
	if err != nil {
		panic(err) // the fuzz never reaches the sequence limit
	}
	m.slots++
	if r.Segment > m.last {
		m.last = r.Segment
	}
	return r, Numbers{Version: v, Seq: seq}
}

// Issued returns the highest version the counters have issued, which the
// driver uses for throughs and fill bounds.
func (m *ModelIndex) Issued() core.Version {
	return core.Version{Counter: m.ctr.NextVersion().Counter - 1}
}

// Check refuses tag on id, as the journal's every call does first. A file the
// index has no entry for is nobody's and checks clean.
func (m *ModelIndex) Check(tag core.ShareTag, id core.FileID) error {
	err := error(nil)
	if !m.fs.view(id, func(f *file) { err = f.check(tag, id) }) {
		return nil
	}
	return err
}

// Write holds p as the journal's WriteAt does: check, then draw, then place a
// record, then admit and commit the write with the record noted, retaining
// what it supersedes when retain is set. It returns the version the write
// drew and the error the check made.
func (m *ModelIndex) Write(tag core.ShareTag, id core.FileID, off int64, data []byte, mtime int64, retain bool) (core.Version, error) {
	if err := m.Check(tag, id); err != nil {
		return core.Version{}, err
	}
	v, err := m.ctr.Draw()
	if err != nil {
		return core.Version{}, err
	}
	e := core.Extent{Off: off, Len: int64(len(data))}
	adm, err := m.fs.admitWrite(id, e, retain)
	if err != nil {
		return v, err
	}
	r, n := m.place(v)
	m.recs[r] = ModelRecord{Data: slices.Clone(data), Mtime: mtime}
	d, err := m.fs.commit(id, adm, func(f *file) error {
		if err := f.recorded(r.Segment, e, n, tag, id); err != nil {
			return err
		}
		p := Placed{Extent: e, Record: r, Version: v}
		if retain {
			f.writeRetain(p)
		} else {
			f.write(p)
		}
		return nil
	})
	m.note(d)
	return v, err
}

// Fill holds p at version v, as the journal's Fill does, unless the file is
// stale against asOf. The record's payload is stored with mtime 0, as a fill
// carries no time. A fill the index accepted claims the file and notes its
// record, as every append does; the note happens after the acceptance.
//
// decision: the harness skips the fill's admission on purpose. files.fill
// commits through update, which applies the true entry delta of an
// admission of zero, so a preceding admitFill — which reserves the entries
// fillAdds predicts — would double-count them into the count the fuzz checks
// with CheckAccounting. The skip leaves admitFill and fillAdds exercised by
// nothing here; they stay covered by their own unit tests until the journal
// wires the real Fill, whose claimed admission must then be brought over
// with the count semantics settled the same way.
func (m *ModelIndex) Fill(tag core.ShareTag, id core.FileID, off int64, data []byte, v, asOf core.Version) error {
	if err := m.Check(tag, id); err != nil {
		return err
	}
	r, n := m.place(v)
	e := core.Extent{Off: off, Len: int64(len(data))}
	m.recs[r] = ModelRecord{Data: slices.Clone(data)}
	d, err := m.fs.fill(tag, id, Placed{Extent: e, Record: r, Version: v}, asOf)
	m.note(d)
	if err != nil {
		return err
	}
	d, err = m.fs.update(id, func(f *file) error {
		return f.recorded(r.Segment, e, n, tag, id)
	})
	m.note(d)
	return err
}

// Mark reports offloaded the content inside e between versions oldest and
// newest, as the journal does for an offer's report.
func (m *ModelIndex) Mark(tag core.ShareTag, id core.FileID, e core.Extent, oldest, newest core.Version) error {
	if err := m.Check(tag, id); err != nil {
		return err
	}
	r, n := m.place(newest)
	d, err := m.fs.update(id, func(f *file) error {
		f.held.markOffloaded(e, oldest, newest)
		return f.recorded(r.Segment, e, n, tag, id)
	})
	m.note(d)
	return err
}

// Release stops holding e, as the journal's Release does, and returns the
// pieces it released with their versions.
func (m *ModelIndex) Release(tag core.ShareTag, id core.FileID, e core.Extent) ([]core.Held, error) {
	if err := m.Check(tag, id); err != nil {
		return nil, err
	}
	adm, err := m.fs.admitRemoval(id, e, 1)
	if err != nil {
		return nil, err
	}
	var out []core.Held
	d, err := m.fs.commit(id, adm, func(f *file) error {
		o, err := f.held.release(e, nil)
		if err != nil {
			return err
		}
		out = o
		if len(o) > 0 {
			// The model claims the file only when it appends a release record,
			// so the harness notes its record only then too.
			highest := o[0].Version
			for _, h := range o[1:] {
				if h.Version.Compare(highest) > 0 {
					highest = h.Version
				}
			}
			r, n := m.place(highest)
			return f.recorded(r.Segment, e, n, tag, id)
		}
		return nil
	})
	m.note(d)
	if errors.Is(err, ErrNotOffloaded) {
		return out, &core.NotOffloadedError{File: id, Extents: []core.Extent{e}}
	}
	return out, err
}

// Truncate stops holding everything at or beyond size at a fresh version, as
// the journal's Truncate does.
func (m *ModelIndex) Truncate(tag core.ShareTag, id core.FileID, size int64) (core.Version, error) {
	if err := m.Check(tag, id); err != nil {
		return core.Version{}, err
	}
	v, err := m.ctr.Draw()
	if err != nil {
		return core.Version{}, err
	}
	r, n := m.place(v)
	d, err := m.fs.update(id, func(f *file) error {
		e := toEnd(size)
		if err := f.recorded(r.Segment, e, n, tag, id); err != nil {
			return err
		}
		f.truncate(size, v)
		return nil
	})
	m.note(d)
	return v, err
}

// Deallocate stops holding e at a fresh version, as the journal's Deallocate
// does. A zero-length e is refused without drawing a version.
func (m *ModelIndex) Deallocate(tag core.ShareTag, id core.FileID, e core.Extent) (core.Version, error) {
	if err := m.Check(tag, id); err != nil {
		return core.Version{}, err
	}
	if e.Len == 0 {
		return core.Version{}, core.ErrZeroLength
	}
	v, err := m.ctr.Draw()
	if err != nil {
		return core.Version{}, err
	}
	adm, err := m.fs.admitRemoval(id, e, 2)
	if err != nil {
		return v, err
	}
	r, n := m.place(v)
	d, err := m.fs.commit(id, adm, func(f *file) error {
		if err := f.recorded(r.Segment, e, n, tag, id); err != nil {
			return err
		}
		return f.deallocate(e, v)
	})
	m.note(d)
	return v, err
}

// Delete stops holding every extent of the file at a fresh version, as the
// journal's Delete does.
func (m *ModelIndex) Delete(tag core.ShareTag, id core.FileID) (core.Version, error) {
	if err := m.Check(tag, id); err != nil {
		return core.Version{}, err
	}
	v, err := m.ctr.Draw()
	if err != nil {
		return core.Version{}, err
	}
	r, n := m.place(v)
	d, err := m.fs.update(id, func(f *file) error {
		if err := f.recorded(r.Segment, toEnd(0), n, tag, id); err != nil {
			return err
		}
		f.delete(v)
		return nil
	})
	m.note(d)
	return v, err
}

// Settle settles id's removals at or below v, as the journal's Settle does.
func (m *ModelIndex) Settle(tag core.ShareTag, id core.FileID, v core.Version) {
	if m.Check(tag, id) == nil {
		// Markers carry no bytes, so the settle's delta is zero and nothing is noted.
		m.fs.settleRemovals(id, v)
	}
}

// Read resolves e of id as the journal's read does: the pieces held inside it
// and the exact gaps between them, and the version the file is checked
// against. A file the index has no entry for is entirely missing.
func (m *ModelIndex) Read(id core.FileID, e core.Extent) (placed []Placed, missing []core.Extent, asOf core.Version) {
	if !m.fs.view(id, func(f *file) {
		placed, missing = f.held.span(e, placed, missing)
		asOf = m.fs.checkedAgainst(f, id)
	}) {
		return nil, []core.Extent{e}, m.fs.asOf(id)
	}
	return placed, missing, asOf
}

// AsOf returns the change version a read of id reports.
func (m *ModelIndex) AsOf(id core.FileID) core.Version { return m.fs.asOf(id) }

// Since returns what the journal's Since would yield for id above from.
func (m *ModelIndex) Since(id core.FileID, from core.Version) []Change {
	return m.fs.since(id, from, nil)
}

// File returns id's entries as the inspection read-out does.
func (m *ModelIndex) File(id core.FileID) []Entry { return m.fs.inspect(id) }

// All yields every file's entries as the bulk read-out does.
func (m *ModelIndex) All() iter.Seq2[core.FileID, []Entry] { return m.fs.inspectAll() }

// Payload returns the bytes and mtime the harness stored for the record at r.
func (m *ModelIndex) Payload(r RecordAt) (ModelRecord, bool) {
	rec, ok := m.recs[r]
	return rec, ok
}

// CheckAccounting recomputes the index's counts from the read-out: the entry
// total against the count, and the held and retained bytes of every segment
// it has placed against the per-segment counters. It returns the first way
// the two disagree.
func (m *ModelIndex) CheckAccounting() error {
	var entries int64
	perSeg := map[core.SegmentID]SegmentBytes{}
	for _, es := range m.fs.inspectAll() {
		for _, e := range es {
			entries++
			switch e.Kind {
			case EntryHeld, EntryRetained:
				b := perSeg[e.Record.Segment]
				if e.Kind == EntryHeld {
					b.Held += e.Extent.Len
				} else {
					b.Retained += e.Extent.Len
				}
				perSeg[e.Record.Segment] = b
			}
		}
	}
	if entries != m.fs.extentCount() {
		return fmt.Errorf("index: the read-out counts %d entries, extentCount() says %d", entries, m.fs.extentCount())
	}
	for seg := core.SegmentID(1); seg <= m.last; seg++ {
		want := SegmentBytes{}
		if b, ok := perSeg[seg]; ok {
			want = b
		}
		if got := m.fs.segmentBytes(seg); got != want {
			return fmt.Errorf("index: segment %d counts %+v, the read-out says %+v", seg, got, want)
		}
	}
	return nil
}

// note adds one update's delta to the running sums, and moves its file on or
// off the dirty-file list as the delta's transition says. A transition that
// contradicts the list, a file made dirty that is on it or made clean that is
// not, is kept as the first error for CheckDeltas to return.
func (m *ModelIndex) note(d HeldDelta) {
	m.sum.held += d.HeldBytes
	m.sum.dirty += d.DirtyBytes
	m.sum.retained += d.RetainedBytes
	t := m.byTag[d.Tag]
	t.held += d.HeldBytes
	t.dirty += d.DirtyBytes
	t.retained += d.RetainedBytes
	m.byTag[d.Tag] = t
	switch d.Dirty {
	case BecameDirty:
		if m.dirtyFiles[d.File] && m.err == nil {
			m.err = fmt.Errorf("index: BecameDirty for %v, already on the dirty-file list (%+v)", d.File, d)
		}
		m.dirtyFiles[d.File] = true
	case BecameClean:
		if !m.dirtyFiles[d.File] && m.err == nil {
			m.err = fmt.Errorf("index: BecameClean for %v, not on the dirty-file list (%+v)", d.File, d)
		}
		delete(m.dirtyFiles, d.File)
	}
}

// CheckDeltas compares the running sums of the deltas the harness's updates
// returned with the read-out: held and retained bytes against the per-segment
// counters and against the entries, dirty bytes against the entries whose
// offloaded bit is unset, and the dirty-file list against the files holding
// such an entry, held or retained. It returns the first way they disagree.
func (m *ModelIndex) CheckDeltas() error {
	if m.err != nil {
		return m.err
	}
	return checkDeltaSums(m.fs, m.last, m.sum, m.byTag, m.dirtyFiles)
}

// checkDeltaSums is CheckDeltas over any index whose records sit in segments
// 1 through last.
func checkDeltaSums(fs *files, last core.SegmentID, sum heldTotals, byTag map[core.ShareTag]heldTotals, dirtyFiles map[core.FileID]bool) error {
	var held, dirty, retained int64
	onList := map[core.FileID]bool{}
	owned := map[core.ShareTag]heldTotals{}
	for id, es := range fs.inspectAll() {
		var tag core.ShareTag
		fs.view(id, func(f *file) { tag, _ = f.owner() })
		t := owned[tag]
		for _, e := range es {
			switch e.Kind {
			case EntryHeld:
				held += e.Extent.Len
				t.held += e.Extent.Len
				if !e.Offloaded {
					dirty += e.Extent.Len
					t.dirty += e.Extent.Len
					onList[id] = true
				}
			case EntryRetained:
				retained += e.Extent.Len
				t.retained += e.Extent.Len
				if !e.Offloaded {
					onList[id] = true
				}
			}
		}
		owned[tag] = t
	}
	// A delta's tag is its file's owner, so each tag's deltas sum to what the
	// files it owns hold; bytes under tag 0 would belong to no share.
	for tag := range byTag {
		if _, ok := owned[tag]; !ok {
			owned[tag] = heldTotals{}
		}
	}
	for tag, want := range owned {
		if got := byTag[tag]; got.held != want.held || got.dirty != want.dirty || got.retained != want.retained {
			return fmt.Errorf("index: tag %d's deltas sum held %d dirty %d retained %d, the files it owns hold %d, %d, %d",
				tag, got.held, got.dirty, got.retained, want.held, want.dirty, want.retained)
		}
	}
	var segHeld, segRetained int64
	for seg := core.SegmentID(1); seg <= last; seg++ {
		b := fs.segmentBytes(seg)
		segHeld += b.Held
		segRetained += b.Retained
	}
	if sum.held != held || sum.held != segHeld {
		return fmt.Errorf("index: deltas sum %d held bytes, the read-out has %d, the segment counters %d", sum.held, held, segHeld)
	}
	if sum.retained != retained || sum.retained != segRetained {
		return fmt.Errorf("index: deltas sum %d retained bytes, the read-out has %d, the segment counters %d", sum.retained, retained, segRetained)
	}
	if sum.dirty != dirty {
		return fmt.Errorf("index: deltas sum %d dirty bytes, the read-out has %d", sum.dirty, dirty)
	}
	if !maps.Equal(dirtyFiles, onList) {
		return fmt.Errorf("index: the deltas' dirty-file list is %v, the read-out's is %v", dirtyFiles, onList)
	}
	return nil
}
