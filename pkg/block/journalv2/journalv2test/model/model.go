package model

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

type (
	version  = journalv2.Version
	fileID   = journalv2.FileID
	extent   = journalv2.Extent
	shareTag = journalv2.ShareTag
)

// toEnd is the length of an extent that runs from off to the end of the file.
func toEnd(off int64) int64 { return math.MaxInt64 - off }

func covers(e extent, off int64) bool { return off >= e.Off && off-e.Off < e.Len }

func maxVersion(a, b version) version {
	if a.Compare(b) >= 0 {
		return a
	}
	return b
}

type recordKind uint8

const (
	kindWrite recordKind = iota + 1
	kindFill
	kindRelease
	kindMark
	kindTruncate
	kindDeallocate
	kindDelete
)

func (k recordKind) removal() bool {
	return k == kindTruncate || k == kindDeallocate || k == kindDelete
}

// changes reports whether a record of kind k changes the file's extents and so
// raises its change version. A fill and a release carry a version already
// counted, and a mark changes no byte.
func (k recordKind) changes() bool { return k == kindWrite || k.removal() }

func (k recordKind) String() string {
	return [...]string{"", "write", "fill", "release", "mark", "truncate", "deallocate", "delete"}[k]
}

// record is one thing the journal appended for a file, as the caller can
// infer it from the call that appended it.
type record struct {
	kind  recordKind
	ext   extent
	ver   version
	seq   uint64 // the model's own append order: calls are applied in order
	data  []byte // a write's or a fill's bytes, ext.Len of them
	mtime int64

	// durable is set once a Sync covering the record returns, on return for
	// a removal, when an offer captures the record's bytes, and when a crash
	// it was uncertain in is resolved with it kept. A record that is not
	// durable when the journal crashes may or may not survive the crash.
	durable bool

	// uncertain marks a record not durable at a crash, until Compare learns
	// whether it survived.
	uncertain bool

	// settled, for a removal, records that Settle covered its marker.
	settled bool

	// markerOptional, for a removal settled before a crash, records that its
	// marker may or may not come back: repack may have dropped its record.
	markerOptional bool

	// markerGone, for a removal whose marker did not come back after a
	// crash, records that its marker never will. Its effect on the bytes
	// stays.
	markerGone bool

	// uncounted marks a record that may hold the file's change version up no
	// longer: after a crash, one that backed nothing and carried no live
	// marker, which repack may have dropped before it.
	uncounted bool

	// bound, for a record a crash left uncertain, is the first version the
	// journal issued after that crash; zero until one is issued. The record
	// cannot have survived at or above it. Only a fill can reach it: every
	// other record carries a version issued before the crash, below every
	// version issued after it.
	bound version
}

func (r *record) hasMarker() bool { return r.kind.removal() && !r.settled && !r.markerGone }

type file struct {
	id  fileID
	tag shareTag

	recs []*record

	// unresolved marks a file a crash left uncertain, until Compare finds
	// the state the journal shows.
	unresolved bool

	// reopened marks a file the journal reopened since it last changed: the
	// reopen may have raised its change version to the floor.
	reopened bool
}

// Model is the reference model of one journal and the shares it serves. It is
// not safe for concurrent use: the model-based test drives it in the order the
// journal's calls returned.
type Model struct {
	files map[fileID]*file
	seq   uint64

	// issued is the highest content version the journal has issued: every
	// version it assigns must exceed it, across crashes too, since the floor
	// of a reopen carries the version limit.
	issued version
	// maxSeen is the highest content version the model has seen, issued or
	// carried by a fill; no answer of the journal names a change above it or
	// the floor.
	maxSeen version
	// floor is the floor of the last reopen.
	floor version

	// offers is the offer each file's running callback was given.
	offers map[fileID]*offer

	// crashed is set between Crash and Reopen.
	crashed bool
}

// New returns a model of an empty journal.
func New() *Model {
	return &Model{files: map[fileID]*file{}}
}

func (m *Model) append(f *file, r *record) {
	m.seq++
	r.seq = m.seq
	f.recs = append(f.recs, r)
	if r.kind.changes() {
		f.reopened = false
	}
}

// cell is one held byte: its value and the record it is read from.
type cell struct {
	b   byte
	rec *record
}

// fileState is a file's content as decided from a set of its records.
type fileState struct {
	held    map[int64]cell
	bit     map[int64]bool // offloaded bit of each held byte
	markers []*record      // removals whose marker is live
	change  version        // the highest version of a record that changed the file

	// inexact reports that the journal may answer with a change version
	// above change: a crash may have dropped a record of the file the model
	// cannot see, or a reopen raised the file to the floor.
	inexact bool
}

// derive decides every byte of f among the records keep admits.
//
// ponytail: the state is one map entry per held byte and every derivation
// repaints every record, O(records × held bytes). The model is an oracle for
// test-sized files; switch to an interval map if the model-based test needs
// files of more than a few megabytes.
func derive(f *file, keep func(*record) bool) *fileState {
	recs := slices.DeleteFunc(slices.Clone(f.recs), func(r *record) bool { return !keep(r) })
	slices.SortFunc(recs, func(a, b *record) int {
		if c := a.ver.Compare(b.ver); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})

	st := &fileState{held: map[int64]cell{}, bit: map[int64]bool{}}
	var marks []*record
	var dropped version // the newest change a crash may have dropped
	for _, r := range recs {
		switch {
		case r.kind == kindWrite || r.kind == kindFill:
			for i, b := range r.data {
				st.held[r.ext.Off+int64(i)] = cell{b, r}
			}
		case r.kind == kindRelease || r.kind.removal():
			for off := range st.held {
				if covers(r.ext, off) {
					delete(st.held, off)
				}
			}
		case r.kind == kindMark:
			marks = append(marks, r)
		}
		// After a crash the journal rebuilds a file's change version from the
		// records on disk, and repack may have dropped a removal whose
		// settled marker did not come back, or a record backing nothing.
		if r.kind.changes() && !r.markerOptional && !r.markerGone && !r.uncounted {
			st.change = maxVersion(st.change, r.ver)
		}
		if r.kind.changes() && (r.markerOptional || r.markerGone || r.uncounted) {
			dropped = maxVersion(dropped, r.ver)
		}
		if r.hasMarker() {
			st.markers = append(st.markers, r)
		}
	}

	st.inexact = f.reopened || dropped.Compare(st.change) > 0

	// A mark sets the bit of the content of its version only, so it never
	// marks a newer write over the bytes it names. Filled content came from
	// the remote tier and is offloaded by construction.
	for off, c := range st.held {
		if c.rec.kind == kindFill {
			st.bit[off] = true
			continue
		}
		for _, mk := range marks {
			if mk.ver == c.rec.ver && covers(mk.ext, off) {
				st.bit[off] = true
				break
			}
		}
	}
	return st
}

func all(*record) bool { return true }

// current is the state of file id with every record it has, the state the
// journal shows while it runs.
func (m *Model) current(id fileID) *fileState {
	f := m.files[id]
	if f == nil {
		return &fileState{held: map[int64]cell{}, bit: map[int64]bool{}}
	}
	return derive(f, all)
}

// run is a maximal stretch of held bytes read from one record with one
// offloaded bit. The pieces a record was split into are one record here: they
// share its sequence number, which no other record has.
type run struct {
	ext       extent
	ver       version
	rec       *record
	offloaded bool
}

func (st *fileState) runs() []run {
	offs := slices.Sorted(maps.Keys(st.held))
	var out []run
	for _, off := range offs {
		c, bit := st.held[off], st.bit[off]
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.rec.seq == c.rec.seq && last.offloaded == bit && last.ext.Off+last.ext.Len == off {
				last.ext.Len++
				continue
			}
		}
		out = append(out, run{ext: extent{Off: off, Len: 1}, ver: c.rec.ver, rec: c.rec, offloaded: bit})
	}
	return out
}

// holdsAnything reports whether f still holds an extent or a removal marker,
// so that another share's call on it is certainly refused.
func (f *file) holdsAnything() bool {
	st := derive(f, all)
	return len(st.held) > 0 || len(st.markers) > 0
}

// The two kinds of error the model returns, for errors.Is.
var (
	// ErrDiverged is the journal doing what the RFC does not allow: an answer
	// or a state no allowed outcome explains. It is a bug in the journal, or
	// in the model's reading of the RFC.
	ErrDiverged = errors.New("the journal did what the RFC does not allow")

	// ErrMisuse is a call the model cannot take: made out of order, outside
	// what the model decides, or past what it can search. It is a bug in the
	// caller driving the model, never evidence about the journal.
	ErrMisuse = errors.New("the model cannot take this call")
)

// mismatch returns an ErrDiverged for op on file id.
func (m *Model) mismatch(op string, id fileID, format string, args ...any) error {
	return report(ErrDiverged, op, id, format, args)
}

// misuse returns an ErrMisuse for op on file id.
func (m *Model) misuse(op string, id fileID, format string, args ...any) error {
	return report(ErrMisuse, op, id, format, args)
}

// report formats an error of kind for op on file id. Versions print as v5
// (e1.v5 with an epoch) and extents as [off,end), so a message reads in the
// caller's terms.
func report(kind error, op string, id fileID, format string, args []any) error {
	for i, a := range args {
		switch a := a.(type) {
		case version:
			args[i] = fmtVersion(a)
		case extent:
			args[i] = fmtExtent(a)
		}
	}
	where := ""
	if id != (fileID{}) {
		where = fmt.Sprintf(" file %x", id.Bytes())
	}
	return fmt.Errorf("model: %s%s: %s: %w", op, where, fmt.Sprintf(format, args...), kind)
}

func fmtVersion(v version) string {
	if v.Epoch != 0 {
		return fmt.Sprintf("e%d.v%d", v.Epoch, v.Counter)
	}
	return fmt.Sprintf("v%d", v.Counter)
}

func fmtExtent(e extent) string {
	if end(e) >= toEnd(0) {
		return fmt.Sprintf("[%d,EOF)", e.Off)
	}
	return fmt.Sprintf("[%d,%d)", e.Off, end(e))
}

// Unresolved returns the files a crash left unknown, which Compare must see
// before any other call on them.
func (m *Model) Unresolved() []fileID {
	var out []fileID
	for id, f := range m.files {
		if f.unresolved {
			out = append(out, id)
		}
	}
	slices.SortFunc(out, func(a, b fileID) int {
		x, y := a.Bytes(), b.Bytes()
		return slices.Compare(x[:], y[:])
	})
	return out
}

// onDisk returns the highest version of a record the model holds that a crash
// has not left uncertain: every version the journal assigns must exceed it.
func (m *Model) onDisk() version {
	var v version
	for _, f := range m.files {
		for _, r := range f.recs {
			if !r.uncertain {
				v = maxVersion(v, r.ver)
			}
		}
	}
	return v
}

// sync makes durable the bytes of each record that captured names, splitting
// a record into pieces where only part of it is named. The bytes of the same
// write before them are durable too, in the same record or in an earlier
// segment sealed before the captured one was taken; the order a write
// survives in already requires them, so they are left as they are.
func (f *file) sync(captured map[*record][]extent) {
	var recs []*record
	for _, r := range f.recs {
		es := merge(captured[r])
		if len(es) == 0 {
			recs = append(recs, r)
			continue
		}
		in := func(i int) bool {
			off := r.ext.Off + int64(i)
			return slices.ContainsFunc(es, func(e extent) bool { return covers(e, off) })
		}
		start := 0
		for i := 1; i <= len(r.data); i++ {
			if i < len(r.data) && in(i) == in(start) {
				continue
			}
			p := r.piece(start, i)
			p.durable = p.durable || in(start)
			recs = append(recs, p)
			start = i
		}
	}
	f.recs = recs
}

// countLive marks which records of f hold its change version up after a
// crash: recovery rebuilds a file's change version from the records on disk.
//
// decision: after a crash, a record backing no held byte and carrying no live
// marker no longer holds the change version up. Repack drops a record once it
// backs nothing and outranks nothing left on disk, which the model cannot
// see, so it takes every such record as possibly dropped. That only lowers
// the bound the change version a read reports must reach, and what a stale
// fill must be refused below; it never refuses a journal. Count a record
// again if the model ever tracks what each segment still holds.
func (f *file) countLive() {
	st := derive(f, all)
	live := map[*record]bool{}
	for _, c := range st.held {
		live[c.rec] = true
	}
	for _, r := range f.recs {
		r.uncounted = r.kind.changes() && !live[r] && !r.hasMarker()
	}
}
