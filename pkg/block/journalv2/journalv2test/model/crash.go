package model

import (
	"errors"
	"fmt"
	"slices"
)

// maxUncertain bounds the records of one file a crash may leave uncertain.
//
// ponytail: Compare first decides each uncertain write and fill it can from
// the bytes the journal shows, then tries every subset of the rest: O(2^k)
// derivations of O(held bytes) each, for the k records left undecided. Those
// are releases, marks and settled markers, and the writes and fills they keep
// from being decided: under an unsynced release, or beside another record of
// the same version. With 4 KiB writes, 16 undecided records take minutes.
// Decide releases and marks from the bits the journal shows too when the
// model-based test stalls in this search.
const maxUncertain = 16

// Crash applies a crash of the journal's storage. Each record no returned Sync
// covered may or may not have survived: a release or an offloaded mark whole,
// and a write or a fill piece by piece (see decide). A removal is durable on
// return and survives. A marker settled before the crash may come back,
// because settling is not persisted, or may not, because repack may have
// dropped its record.
//
// Every running offer ends. Until Reopen the model takes no call, and until
// Compare finds which of the allowed states the journal shows, it takes no
// call on a file the crash left uncertain. Crash changes nothing and returns
// an error when one file may leave more records undecided than the model can
// search; Compare reports it if a file still does.
func (m *Model) Crash() error {
	var errs []error
	for _, f := range m.files {
		n := 0
		for _, r := range f.recs {
			if crashMayDropMarker(r) || !r.durable && (r.data == nil || mayStayUndecided(f, r)) {
				n++
			}
		}
		if n > maxUncertain {
			errs = append(errs, fmt.Errorf("model: crash: file %x has %d uncertain records the journal's bytes cannot decide, above the %d the model can search; sync it more often", f.id.Bytes(), n, maxUncertain))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	m.crashed = true
	m.offers = nil
	for _, f := range m.files {
		for _, r := range f.recs {
			if !r.durable {
				r.uncertain = true
				f.unresolved = true
			}
			if crashMayDropMarker(r) {
				r.markerOptional = true
				f.unresolved = true
			}
		}
		f.countLive()
	}
	return nil
}

// mayStayUndecided reports whether the bytes the journal shows may leave the
// unsynced write or fill r undecided: an unsynced release outranks it, or
// another record of the same version overlaps it.
func mayStayUndecided(f *file, r *record) bool {
	for _, q := range f.recs {
		if q == r {
			continue
		}
		if _, ok := intersect(q.ext, r.ext); !ok {
			continue
		}
		if !q.durable && q.kind == kindRelease && outranks(q, r) || q.data != nil && q.ver == r.ver {
			return true
		}
	}
	return false
}

// crashMayDropMarker reports whether a crash may leave r's marker absent: it
// was settled, and has not already failed to come back after an earlier crash.
func crashMayDropMarker(r *record) bool {
	return r.kind.removal() && r.settled && !r.markerGone
}

// Reopen applies the reopen of the journal after a crash with floor: every
// version it assigns from now on must exceed floor. The floor carries the
// version limit, so the counter restarts above every version issued before the
// crash; a floor below one is refused, since the journal could then issue it
// again.
func (m *Model) Reopen(floor version) error {
	if floor.Compare(m.issued) < 0 {
		return fmt.Errorf("model: reopen: floor %v is below version %v, already issued", floor, m.issued)
	}
	m.crashed = false
	m.floor = floor
	m.sinceReopen = version{}
	return nil
}

// items returns what a crash left uncertain in f, in a fixed order: the
// records that may not have survived, and the settled markers that may come
// back.
func (f *file) items() []*record {
	var out []*record
	for _, r := range f.recs {
		if r.uncertain || r.markerOptional {
			out = append(out, r)
		}
	}
	return out
}

// candidate is one state a crash allows for a file: which of its uncertain
// items took effect, a surviving record or a marker that came back. The items
// in fixed are decided already; the others are taken where mask has their bit.
type candidate struct {
	fixed map[*record]bool
	items []*record
	mask  uint32
}

func (c candidate) took(r *record) bool {
	if took, ok := c.fixed[r]; ok {
		return took
	}
	for i, it := range c.items {
		if it == r {
			return c.mask&(1<<i) != 0
		}
	}
	return true
}

func (c candidate) derive(f *file) *fileState {
	st := derive(f, func(r *record) bool { return !r.uncertain || c.took(r) })
	for _, r := range f.recs {
		if r.markerOptional && c.took(r) {
			st.markers = append(st.markers, r)
			st.change = maxVersion(st.change, r.ver)
		}
	}
	return st
}

// adopt makes the candidate the file's state: the records that did not
// survive are gone, the ones that did are durable, the markers that came back
// are unsettled again, and only the records still live count in the change
// version.
func (c candidate) adopt(f *file) {
	kept := f.recs[:0]
	for _, r := range f.recs {
		took := c.took(r)
		switch {
		case r.uncertain && !took:
			continue
		case r.uncertain:
			r.uncertain, r.durable = false, true
		case r.markerOptional && took:
			r.markerOptional, r.settled = false, false
		case r.markerOptional:
			r.markerOptional, r.markerGone = false, true
		}
		kept = append(kept, r)
	}
	f.recs = kept
	f.unresolved = false
	f.countLive()
}

// outranks reports whether q wins over r where they overlap: the higher
// version, and between equal versions the later record.
func outranks(q, r *record) bool {
	if c := q.ver.Compare(r.ver); c != 0 {
		return c > 0
	}
	return q.seq > r.seq
}

// evidence is what the journal's bytes say of one byte of an uncertain write
// or fill: that it survived, that it was lost, or nothing, where content that
// outranks it hides it either way.
type evidence uint8

const (
	hidden evidence = iota
	kept
	lost
)

// decide splits each uncertain write and fill of f into pieces where what the
// journal shows of it changes, settles each piece the bytes decide, and
// returns the settled pieces with whether each survived. The pieces left out
// are left to the search.
//
// It decides from the top, the record that outranks the others first, so that
// by the time it reaches a record, everything outranking it is settled or left
// to the search. A byte survived where the journal serves it at the record's
// version, with no other record of that version there to serve it instead. It
// was lost where the journal serves an older version there, or reports it
// missing with no release or removal outranking the record over it: had it
// survived, it or something outranking it would decide that byte. Where only
// content known to be there outranks it, the byte is hidden for good: no later
// call brings back a byte below one that outranks it without removing it too,
// and after a crash a record backing nothing no longer counts in the change
// version, so kept and lost look the same forever and lost is taken. None of
// these answers changes which states match; they only shrink the search.
//
// decision: an uncertain write or fill may survive in part, any of its bytes
// kept and the others lost. The journal stores a write that crosses a segment
// cap as one record per segment, each surviving whole or not at all, so the
// write can come back partly held. The model cannot see where the pieces
// split, so it accepts any split; checking that each piece is whole needs the
// segment boundaries, which only a test placing the write knows. Hold the
// model to whole pieces if the journal ever reports where a write was split.
func decide(f *file, o *observation) map[*record]bool {
	out := map[*record]bool{}
	order := slices.Clone(f.recs)
	slices.SortStableFunc(order, func(a, b *record) int {
		switch {
		case outranks(a, b):
			return -1
		case outranks(b, a):
			return 1
		}
		return 0
	})

	var open []*record // pieces of records above, left to the search
	pieces := map[*record][]*record{}
	for _, r := range order {
		if !r.uncertain || r.data == nil {
			continue
		}
		start, ev := 0, classify(f, r, o, open, 0)
		var mine []*record
		for i := 1; i <= len(r.data); i++ {
			var next evidence
			if i < len(r.data) {
				next = classify(f, r, o, open, i)
				if next == ev {
					continue
				}
			}
			p := r.piece(start, i)
			mine = append(mine, p)
			if ev == hidden {
				open = append(open, p)
			} else {
				out[p] = ev == kept
			}
			start, ev = i, next
		}
		pieces[r] = mine
	}

	var recs []*record
	for _, r := range f.recs {
		if ps, ok := pieces[r]; ok {
			recs = append(recs, ps...)
		} else {
			recs = append(recs, r)
		}
	}
	f.recs = recs
	return out
}

// classify returns what the journal shows of byte i of r, given the pieces of
// records above r left to the search. It reads the file's records as they were
// before decide split any of them.
func classify(f *file, r *record, o *observation, open []*record, i int) evidence {
	off := r.ext.Off + int64(i)
	got, held := o.held[off]
	switch {
	case held && got.ver == r.ver && got.b == r.data[i] && !servedByAnother(f, r, off):
		return kept
	case held && got.ver.Compare(r.ver) < 0:
		return lost
	case !held && o.missing[off] && !removedAbove(f, r, off):
		return lost
	case (held && got.ver.Compare(r.ver) > 0 || !held && o.missing[off]) && !openAbove(f, r, open, off):
		return lost
	}
	return hidden
}

// openAbove reports whether something the crash may have lost, and that the
// bytes have not decided, outranks r at off: a piece above left to the search,
// or a release.
func openAbove(f *file, r *record, open []*record, off int64) bool {
	for _, q := range open {
		if outranks(q, r) && covers(q.ext, off) {
			return true
		}
	}
	for _, q := range f.recs {
		if q.uncertain && q.kind == kindRelease && outranks(q, r) && covers(q.ext, off) {
			return true
		}
	}
	return false
}

// piece returns the part of r from byte i to byte j as a record of its own,
// with r's version and sequence number: deciding bytes by precedence, the
// pieces of r decide exactly what r did.
func (r *record) piece(i, j int) *record {
	p := *r
	p.ext = extent{Off: r.ext.Off + int64(i), Len: int64(j - i)}
	p.data = r.data[i:j:j]
	return &p
}

func servedByAnother(f *file, r *record, off int64) bool {
	for _, q := range f.recs {
		if q != r && q.data != nil && q.ver == r.ver && covers(q.ext, off) {
			return true
		}
	}
	return false
}

func removedAbove(f *file, r *record, off int64) bool {
	for _, q := range f.recs {
		if (q.kind == kindRelease || q.kind.removal()) && outranks(q, r) && covers(q.ext, off) {
			return true
		}
	}
	return false
}
