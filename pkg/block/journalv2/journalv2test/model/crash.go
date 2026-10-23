package model

import (
	"errors"
	"fmt"
)

// maxUncertain bounds the records of one file a crash may leave uncertain.
//
// ponytail: Compare first decides each uncertain write and fill it can from
// the bytes the journal shows, then tries every subset of the rest: O(2^k)
// derivations of O(held bytes) each, for the k records left undecided. Those
// are releases, marks and settled markers, and any write or fill served at
// its own version, with its own bytes, beside another record of that version,
// which a release and a refill of the same content produce. With 4 KiB writes,
// 16 undecided records take minutes.
// Decide releases and marks from the bits the journal shows too when the
// model-based test stalls in this search.
const maxUncertain = 16

// Crash applies a crash of the journal's storage. Each record no returned Sync
// covered may or may not have survived: a release or an offloaded mark whole,
// and a write or a fill in part, since it may be several records. A removal is durable on
// return and survives. A marker settled before the crash may come back,
// because settling is not persisted, or may not, because repack may have
// dropped its record.
//
// Every running offer ends. Until Reopen the model takes no call, and until
// Compare finds which of the allowed states the journal shows, it takes no
// call on a file the crash left uncertain. Crash changes nothing and returns
// an error when one file has more releases, marks and settled markers left
// uncertain than the model can search. Writes and fills are decided from the
// bytes, except where the search is needed beside a record of the same
// version; Compare reports a file that still leaves too many undecided.
func (m *Model) Crash() error {
	var errs []error
	for _, f := range m.files {
		n := 0
		for _, r := range f.recs {
			if crashMayDropMarker(r) || !r.durable && r.data == nil {
				n++
			}
		}
		if n > maxUncertain {
			errs = append(errs, m.misuse("crash", f.id, "%d unsynced releases, marks and settled markers, above the %d the model can search; sync the file more often", n, maxUncertain))
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

// crashMayDropMarker reports whether a crash may leave r's marker absent: it
// was settled, and has not already failed to come back after an earlier crash.
func crashMayDropMarker(r *record) bool {
	return r.kind.removal() && r.settled && !r.markerGone
}

// Reopen applies the reopen of the journal after a crash with floor: every
// version it assigns from now on must exceed floor. Pass the highest floor the
// journal was given: the one Open took, or a higher one a Share took. The
// floor carries the version limit, so the counter restarts above every
// version issued before the crash; a floor below one is refused, since the
// journal could then issue it again.
func (m *Model) Reopen(floor version) error {
	if floor.Compare(m.issued) < 0 {
		return m.misuse("reopen", fileID{}, "floor %v is below %v, a version already issued; pass the floor the journal opened with", floor, m.issued)
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

// evidence is what the journal's bytes say of one byte of an uncertain write
// or fill: that it survived; that it was lost; that it is hidden, outranked by
// something that makes kept and lost look the same; or nothing, where another
// record of the same version may be what the journal serves.
type evidence uint8

const (
	undecided evidence = iota
	kept
	lost
	hidden
)

// decide splits each uncertain write and fill of f into pieces where what the
// journal shows of it changes, and settles each piece the bytes decide. It
// returns the settled pieces with whether each survived, and the ones of them
// the journal shows lost, as opposed to hidden. The pieces left out are left
// to the search.
//
// A byte survived where the journal serves it at the record's version, with
// its bytes, and no other record of that version there to serve it instead.
// It was lost where the journal serves an older version there, other bytes at
// the record's version from an earlier record of that version, or nothing
// with no release or removal outranking the record: had it survived, it or
// something outranking it would decide that byte. It is hidden where the
// journal serves a newer version, the record's version from a later record of
// it, or nothing under a release or removal that outranks it. A hidden byte is
// settled as lost: whatever outranks it is durable once the crash is resolved,
// no later call brings back a byte below one that outranks it without removing
// it too, and after a crash a record backing nothing no longer counts in the
// change version, so kept and lost look the same forever. Only a byte served
// at its own version, with its own bytes, beside another record of that
// version, is left to the search. None of these answers changes anything the
// journal can show later; they only shrink the search.
//
// decision: an uncertain write or fill may survive in part, as a prefix of
// itself. The journal stores a write that crosses a segment cap as one record
// per segment, each surviving whole or not at all, and seals and syncs a
// segment before it takes the next, so a later piece survives only with every
// earlier one. The model cannot see where the pieces split, so it accepts any
// prefix, with hidden bytes anywhere; checking that each piece is whole needs
// the segment boundaries, which only a test placing the write knows. Hold the
// model to whole pieces if the journal ever reports where a write was split.
func decide(f *file, o *observation) (settled, shownLost map[*record]bool) {
	settled, shownLost = map[*record]bool{}, map[*record]bool{}
	var recs []*record
	for _, r := range f.recs {
		if !r.uncertain || r.data == nil {
			recs = append(recs, r)
			continue
		}
		start, ev := 0, classify(f, r, o, 0)
		for i := 1; i <= len(r.data); i++ {
			var next evidence
			if i < len(r.data) {
				next = classify(f, r, o, i)
				if next == ev {
					continue
				}
			}
			p := r.piece(start, i)
			recs = append(recs, p)
			switch ev {
			case kept:
				settled[p] = true
			case lost:
				settled[p] = false
				shownLost[p] = true
			case hidden:
				settled[p] = false
			}
			start, ev = i, next
		}
	}
	f.recs = recs
	return settled, shownLost
}

// classify returns what the journal shows of byte i of r. It reads the file's
// records as they were before decide split any of them.
func classify(f *file, r *record, o *observation, i int) evidence {
	off := r.ext.Off + int64(i)
	got, held := o.held[off]
	switch {
	case held && got.ver == r.ver && got.b == r.data[i] && !servedByAnother(f, r, off):
		return kept
	case held && got.ver.Compare(r.ver) < 0:
		return lost
	case held && got.ver.Compare(r.ver) > 0:
		return hidden
	case held && got.b != r.data[i]:
		if servedLater(f, r, off) {
			return hidden
		}
		return lost
	case !held && o.missing[off]:
		if removedAbove(f, r, off) {
			return hidden
		}
		return lost
	}
	return undecided
}

// prefixBroken returns how the settled pieces break the order a write
// survives in, or "" when they do not: a piece the journal shows lost before
// a piece of the same write it kept, or that is durable.
//
// ponytail: the order is checked on the pieces the journal's bytes settle,
// not on the search's candidates. A piece left to the search is served at its
// own version with its own bytes beside another record of that version, which
// only a refill after a release makes, and a refill looks the same kept or
// lost; so no candidate the search adopts differs from an ordered one in
// anything the journal shows. Check candidates too if the search ever decides
// a piece whose survival shows.
func prefixBroken(f *file, settled, shownLost map[*record]bool) string {
	firstLost := map[uint64]*record{}
	for _, r := range f.recs {
		if shownLost[r] && (firstLost[r.seq] == nil || r.ext.Off < firstLost[r.seq].ext.Off) {
			firstLost[r.seq] = r
		}
	}
	for _, r := range f.recs {
		l := firstLost[r.seq]
		if l == nil || r.data == nil || r.ext.Off < l.ext.Off {
			continue
		}
		if survived, ok := settled[r]; ok && survived || !r.uncertain {
			return fmt.Sprintf("the journal holds %s of the %s at %s but lost %s before it: a write survives a crash as a prefix of itself", fmtExtent(r.ext), r.kind, fmtVersion(r.ver), fmtExtent(l.ext))
		}
	}
	return ""
}

// servedLater reports whether a record of r's version appended after it covers
// off: had both survived, that record would serve the byte.
func servedLater(f *file, r *record, off int64) bool {
	for _, q := range f.recs {
		if q.seq > r.seq && q.data != nil && q.ver == r.ver && covers(q.ext, off) {
			return true
		}
	}
	return false
}

// removedAbove reports whether a release or removal outranking r covers off:
// it makes the byte absent whether or not r survived.
func removedAbove(f *file, r *record, off int64) bool {
	for _, q := range f.recs {
		if (q.kind == kindRelease || q.kind.removal()) && covers(q.ext, off) &&
			(q.ver.Compare(r.ver) > 0 || q.ver == r.ver && q.seq > r.seq) {
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
