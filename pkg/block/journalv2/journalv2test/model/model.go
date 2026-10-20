package model

import (
	"cmp"
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

	// durable is set once a Sync covering the record returns, or on return
	// for a removal. A record that is not durable when the journal crashes
	// may or may not survive the crash.
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
}

func (r *record) hasMarker() bool { return r.kind.removal() && !r.settled && !r.markerGone }

type file struct {
	id  fileID
	tag shareTag

	recs []*record

	// unresolved marks a file a crash left uncertain, until Compare finds
	// the state the journal shows.
	unresolved bool
}

// Model is the reference model of one journal and the shares it serves. It is
// not safe for concurrent use: the model-based test drives it in the order the
// journal's calls returned.
type Model struct {
	files map[fileID]*file
	seq   uint64

	// maxSeen is the highest content version the model has seen issued or
	// written; every version the journal assigns must exceed it.
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
		if r.kind.changes() {
			st.change = maxVersion(st.change, r.ver)
		}
		if r.hasMarker() {
			st.markers = append(st.markers, r)
		}
	}

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
// offloaded bit.
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
			if last.rec == c.rec && last.offloaded == bit && last.ext.Off+last.ext.Len == off {
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

func (m *Model) mismatch(op string, id fileID, format string, args ...any) error {
	return fmt.Errorf("model: %s %x: %s", op, id.Bytes(), fmt.Sprintf(format, args...))
}
