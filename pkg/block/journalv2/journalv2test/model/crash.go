package model

import (
	"errors"
	"fmt"
)

// maxUncertain bounds the records of one file a crash may leave uncertain.
//
// ponytail: Compare finds the state a crash left by trying every subset of a
// file's uncertain records, O(2^k) derivations. 16 keeps that under a second
// for test-sized files. Replace the search with inference per record, which
// the bytes the journal shows mostly decide on their own, when the model-based
// test needs longer runs of unsynced calls on one file.
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
// version it assigns from now on must exceed floor.
func (m *Model) Reopen(floor version) {
	m.crashed = false
	m.floor = floor
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
// items took effect, a surviving record or a marker that came back.
type candidate struct {
	items []*record
	mask  uint32
}

func (c candidate) took(r *record) bool {
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
