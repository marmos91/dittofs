package model

import (
	"errors"
	"fmt"
)

// maxUncertain bounds the records of one file a crash may leave uncertain.
//
// ponytail: Compare first decides each uncertain write and fill it can from
// the bytes the journal shows, then tries every subset of the rest: O(2^k)
// derivations of O(held bytes) each, for the k records the bytes leave
// undecided, which are releases, marks, settled markers and writes hidden
// under newer content. With 4 KiB writes, 16 undecided records take minutes.
// Decide releases and marks from the bits the journal shows too when the
// model-based test stalls in this search.
const maxUncertain = 16

// Crash applies a crash of the journal's storage. Each record no returned Sync
// covered may or may not have survived, whole: a write, a fill, a release or
// an offloaded mark. A removal is durable on return and survives. A marker
// settled before the crash may come back, because settling is not persisted,
// or may not, because repack may have dropped its record.
//
// Every running offer ends. Until Reopen the model takes no call, and until
// Compare finds which of the allowed states the journal shows, it takes no
// call on a file the crash left uncertain. Crash returns an error when one
// file has more uncertain records than the model can search.
func (m *Model) Crash() error {
	m.crashed = true
	m.offers = nil
	var errs []error
	for _, f := range m.files {
		n := 0
		for _, r := range f.recs {
			if !r.durable {
				r.uncertain = true
				n++
			}
			if r.kind.removal() && r.settled && !r.markerGone {
				r.markerOptional = true
				n++
			}
		}
		f.unresolved = f.unresolved || n > 0
		if n > maxUncertain {
			errs = append(errs, fmt.Errorf("model: crash: file %x has %d uncertain records, above the %d the model can search; sync it more often", f.id.Bytes(), n, maxUncertain))
		}
	}
	return errors.Join(errs...)
}

// Reopen applies the reopen of the journal after a crash with floor: every
// version it assigns from now on must exceed floor. The floor carries the
// version limit, so the counter restarts above every version issued before the
// crash; a floor below one is refused, since the journal could then issue it
// again.
func (m *Model) Reopen(floor version) error {
	if floor.Compare(m.maxSeen) < 0 {
		return fmt.Errorf("model: reopen: floor %v is below version %v, already issued", floor, m.maxSeen)
	}
	m.crashed = false
	m.floor = floor
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
// survive are gone, the ones that did are durable, and the markers that came
// back are unsettled again.
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
}

// outranks reports whether q wins over r where they overlap: the higher
// version, and between equal versions the later record.
func outranks(q, r *record) bool {
	if c := q.ver.Compare(r.ver); c != 0 {
		return c > 0
	}
	return q.seq > r.seq
}

// decide settles each uncertain write and fill of f that the bytes the journal
// shows decide on their own, and returns them with whether each survived.
//
// A record survived where the journal serves one of its bytes at its version,
// with no other record of that version there to serve it instead. A record was
// lost where the journal serves an older version inside it, or reports a byte
// of it missing that no release or removal outranking it covers: had it
// survived, it or something outranking it would decide that byte.
func decide(f *file, o *observation) map[*record]bool {
	out := map[*record]bool{}
	for _, r := range f.recs {
		if !r.uncertain || r.data == nil {
			continue
		}
		survived, lost := false, false
		for i, b := range r.data {
			off := r.ext.Off + int64(i)
			got, held := o.held[off]
			switch {
			case held && got.ver == r.ver && got.b == b && !servedByAnother(f, r, off):
				survived = true
			case held && got.ver.Compare(r.ver) < 0:
				lost = true
			case !held && o.missing[off] && !removedAbove(f, r, off):
				lost = true
			}
		}
		if survived != lost {
			out[r] = survived
		}
	}
	return out
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
