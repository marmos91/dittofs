package index

import (
	"errors"
	"iter"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
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
}

// NewModelIndex returns an empty index-backed harness: no extent bound, no
// bound on versions, as the fuzz never reaches either.
func NewModelIndex() *ModelIndex {
	m := &ModelIndex{fs: &files{}, recs: map[RecordAt]ModelRecord{}}
	m.fs.setExtentLimit(1 << 40)
	_ = m.ctr.SetVersionLimit(core.Version{Counter: 1 << 40})
	return m
}

// BucketOf returns the floor bucket id falls in, so a test can check where a
// file lands.
func BucketOf(id core.FileID) int { return bucketOf(id) }

// place draws the record home for one record: one more placement into the
// journal's fifth segment, and the numbers the journal would have drawn.
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
	err = m.fs.commit(id, adm, func(f *file) error {
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
	return v, err
}

// Fill holds p at version v, as the journal's Fill does, unless the file is
// stale against asOf. The record's payload is stored with mtime 0, as a fill
// carries no time. A fill the index accepted claims the file and notes its
// record, as every append does; the note happens after the acceptance, since
// files.fill commits through update and a second admission would
// double-count.
func (m *ModelIndex) Fill(tag core.ShareTag, id core.FileID, off int64, data []byte, v, asOf core.Version) error {
	if err := m.Check(tag, id); err != nil {
		return err
	}
	r, n := m.place(v)
	e := core.Extent{Off: off, Len: int64(len(data))}
	if _, ok := m.recs[r]; !ok {
		m.recs[r] = ModelRecord{Data: slices.Clone(data)}
	}
	if err := m.fs.fill(id, Placed{Extent: e, Record: r, Version: v}, asOf); err != nil {
		return err
	}
	return m.fs.update(id, func(f *file) error {
		return f.recorded(r.Segment, e, n, tag, id)
	})
}

// Mark reports offloaded the content inside e between versions oldest and
// newest, as the journal does for an offer's report.
func (m *ModelIndex) Mark(tag core.ShareTag, id core.FileID, e core.Extent, oldest, newest core.Version) error {
	if err := m.Check(tag, id); err != nil {
		return err
	}
	r, n := m.place(newest)
	return m.fs.update(id, func(f *file) error {
		f.held.markOffloaded(e, oldest, newest)
		return f.recorded(r.Segment, e, n, tag, id)
	})
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
	err = m.fs.commit(id, adm, func(f *file) error {
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
	err = m.fs.update(id, func(f *file) error {
		e := toEnd(size)
		if err := f.recorded(r.Segment, e, n, tag, id); err != nil {
			return err
		}
		f.truncate(size, v)
		return nil
	})
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
	err = m.fs.commit(id, adm, func(f *file) error {
		if err := f.recorded(r.Segment, e, n, tag, id); err != nil {
			return err
		}
		return f.deallocate(e, v)
	})
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
	err = m.fs.update(id, func(f *file) error {
		if err := f.recorded(r.Segment, toEnd(0), n, tag, id); err != nil {
			return err
		}
		f.delete(v)
		return nil
	})
	return v, err
}

// Settle settles id's removals at or below v, as the journal's Settle does.
func (m *ModelIndex) Settle(tag core.ShareTag, id core.FileID, v core.Version) {
	if m.Check(tag, id) == nil {
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
func (m *ModelIndex) File(id core.FileID) []Entry { return m.fs.inspect(id, nil) }

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
		return &accountingError{"the read-out counts " + itoa(entries) + " entries, extentCount() says " + itoa(m.fs.extentCount())}
	}
	for seg := core.SegmentID(1); seg <= m.last; seg++ {
		want := SegmentBytes{}
		if b, ok := perSeg[seg]; ok {
			want = b
		}
		if got := m.fs.segmentBytes(seg); got != want {
			return &accountingError{"segment " + itoa(int64(seg)) + " counts " +
				"{" + itoa(got.Held) + " " + itoa(got.Retained) + "}, the read-out says " +
				"{" + itoa(want.Held) + " " + itoa(want.Retained) + "}"}
		}
	}
	return nil
}

// accountingError is an index-accounting mismatch the fuzz found.
type accountingError struct{ msg string }

func (e *accountingError) Error() string { return "index: " + e.msg }

// itoa is the harness's own small formatter, kept to the standard library.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [24]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
