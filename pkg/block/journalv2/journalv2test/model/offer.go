package model

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// offer is what the model remembers of the offer a callback is running on.
type offer struct {
	tag        shareTag
	pieces     []journalv2.OfferedExtent // Dirty and Superseded: what a report marks
	neighbours []journalv2.Held
	ended      bool
}

// end returns the offset one past e, clamped so an extent running past the
// largest offset ends there.
func end(e extent) int64 {
	if e.Len > toEnd(e.Off) {
		return toEnd(0)
	}
	return e.Off + e.Len
}

func intersect(a, b extent) (extent, bool) {
	lo, hi := max(a.Off, b.Off), min(end(a), end(b))
	if lo >= hi {
		return extent{}, false
	}
	return extent{Off: lo, Len: hi - lo}, true
}

// coveredBy reports whether every byte of e lies in one of the extents of set.
func coveredBy(e extent, set []extent) bool {
	sorted := slices.SortedFunc(slices.Values(set), func(a, b extent) int { return cmp.Compare(a.Off, b.Off) })
	at := e.Off
	for _, s := range sorted {
		if at >= end(e) {
			break
		}
		if s.Off > at {
			return false
		}
		at = max(at, end(s))
	}
	return at >= end(e)
}

// Offer checks the offer the journal passed to an Offload callback for file
// id through share tag, called with through, limit and widen, and remembers it
// for Report. Call it first in the callback, before applying any operation
// that ran while the callback did.
//
// The model checks that the offer is sound, not that it is complete: limit
// and the bounds on pinned segments let the journal stop early, and a later
// call offers the rest. Every Dirty extent must be content the file holds, or
// held before a version above through superseded it, at its version, dirty
// and unremoved below through, with the bytes the model has for that version.
// The offer must not be empty while dirty content at or below through is
// held, must keep to limit, must be in offset order, and must carry no stamp,
// since the model gives none. Superseded extents, kept for snapshot holds, are
// not checked.
func (m *Model) Offer(tag shareTag, id fileID, through version, limit, widen int64, o journalv2.Offer) error {
	const op = "offer"
	f, _, mis := m.enter(op, tag, id, nil)
	if mis != nil {
		return mis
	}
	if o.ID != id {
		return m.mismatch(op, id, "offered file %x", o.ID.Bytes())
	}
	if f == nil {
		f = &file{id: id, tag: tag}
	}
	cur := derive(f, all)
	below := derive(f, func(r *record) bool { return r.ver.Compare(through) <= 0 })

	var total int64
	var prev extent
	versions := []version{}
	for i, d := range o.Dirty {
		switch {
		case d.Extent.Len <= 0:
			return m.mismatch(op, id, "offered the empty extent %+v", d.Extent)
		case i > 0 && d.Extent.Off < end(prev):
			return m.mismatch(op, id, "offered %+v after %+v, not in offset order", d.Extent, prev)
		case d.Stamped:
			return m.mismatch(op, id, "offered %+v stamped, but no stamp was given", d.Extent)
		}
		for off := d.Extent.Off; off < end(d.Extent); off++ {
			c, held := below.held[off]
			switch {
			case !held:
				return m.mismatch(op, id, "offered byte %d, not held at or below %v", off, through)
			case c.rec.ver != d.Version || c.rec.kind != kindWrite || below.bit[off]:
				return m.mismatch(op, id, "offered byte %d at %v, but it is %s content at %v (offloaded %t)", off, d.Version, c.rec.kind, c.rec.ver, below.bit[off])
			}
		}
		if err := readsAs(o.Offered, d.Extent, below); err != "" {
			return m.mismatch(op, id, "%s", err)
		}
		total += d.Extent.Len
		prev = d.Extent
		versions = append(versions, d.Version)
	}
	if limit > 0 && total > limit {
		return m.mismatch(op, id, "offered %d bytes, above the limit of %d", total, limit)
	}
	// The first extent always fits, so an offer is empty only once no dirty
	// content outside segments under repack remains. The model is driven one
	// call at a time, so no repack pass runs during an offer, and it models
	// Offload, not OffloadMany, whose later files may get nothing.
	if len(o.Dirty) == 0 {
		for off, c := range cur.held {
			if c.rec.ver.Compare(through) <= 0 && !cur.bit[off] {
				return m.mismatch(op, id, "offered nothing while byte %d is dirty at %v", off, c.rec.ver)
			}
		}
	}

	if err := m.checkNeighbours(id, widen, o, cur); err != nil {
		return err
	}
	for _, n := range o.Neighbours {
		versions = append(versions, n.Version)
	}
	if len(versions) > 0 && len(o.Superseded) == 0 {
		lo, hi := slices.MinFunc(versions, version.Compare), slices.MaxFunc(versions, version.Compare)
		if o.Oldest != lo || o.Newest != hi {
			return m.mismatch(op, id, "offer bounded by %v..%v, but its extents span %v..%v", o.Oldest, o.Newest, lo, hi)
		}
	}
	for _, s := range o.Superseded {
		if s.Stamped {
			return m.mismatch(op, id, "offered superseded %+v stamped, but no stamp was given", s.Extent)
		}
	}

	if len(o.Dirty) > 0 {
		if probe := o.Dirty[0].Extent.Off - 1; probe >= 0 && !offeredAt(o, probe) {
			if _, err := o.Offered.ReadAt(make([]byte, 1), probe); err == nil {
				return m.mismatch(op, id, "Offered read byte %d, outside the offer", probe)
			}
		}
	}

	// An offer syncs the records it captures before calling the callback, so
	// a crash from now on cannot lose them.
	for _, d := range o.Dirty {
		for off := d.Extent.Off; off < end(d.Extent); off++ {
			below.held[off].rec.durable = true
		}
	}
	for _, n := range o.Neighbours {
		for off := n.Extent.Off; off < end(n.Extent); off++ {
			cur.held[off].rec.durable = true
		}
	}

	if m.offers == nil {
		m.offers = map[fileID]*offer{}
	}
	m.offers[id] = &offer{tag: tag, pieces: slices.Concat(o.Dirty, o.Superseded), neighbours: o.Neighbours}
	return nil
}

// checkNeighbours checks the offloaded neighbours of an offer: held now with
// the bit set, at their version, with the model's bytes, and contiguous with a
// dirty extent through other offered extents, at most widen bytes beyond it.
func (m *Model) checkNeighbours(id fileID, widen int64, o journalv2.Offer, cur *fileState) error {
	const op = "offer"
	if len(o.Neighbours) > 0 && widen <= 0 {
		return m.mismatch(op, id, "offered neighbours with widen %d", widen)
	}
	type span struct {
		e     extent
		dirty bool
	}
	var spans []span
	for _, d := range o.Dirty {
		spans = append(spans, span{d.Extent, true})
	}
	for _, n := range o.Neighbours {
		for off := n.Extent.Off; off < end(n.Extent); off++ {
			c, held := cur.held[off]
			if !held || c.rec.ver != n.Version || !cur.bit[off] {
				return m.mismatch(op, id, "offered neighbour byte %d at %v, not offloaded content held at that version", off, n.Version)
			}
		}
		if err := readsAs(o.Offered, n.Extent, cur); err != "" {
			return m.mismatch(op, id, "neighbour: %s", err)
		}
		spans = append(spans, span{n.Extent, false})
	}
	slices.SortFunc(spans, func(a, b span) int { return cmp.Compare(a.e.Off, b.e.Off) })

	// Split the offered extents into runs of contiguous ones; a neighbour's
	// run must hold a dirty extent, and the neighbour lie within widen of
	// the nearest one on its side.
	for i := 0; i < len(spans); {
		j := i + 1
		for j < len(spans) && spans[j].e.Off == end(spans[j-1].e) {
			j++
		}
		run := spans[i:j]
		first, last := -1, -1
		for k, s := range run {
			if s.dirty {
				if first < 0 {
					first = k
				}
				last = k
			}
		}
		for k, s := range run {
			if s.dirty {
				continue
			}
			switch {
			case first < 0:
				return m.mismatch(op, id, "offered neighbour %+v contiguous with no dirty extent", s.e)
			case k < first && run[first].e.Off-s.e.Off > widen:
				return m.mismatch(op, id, "offered neighbour %+v more than widen %d before %+v", s.e, widen, run[first].e)
			case k > last && end(s.e)-end(run[last].e) > widen:
				return m.mismatch(op, id, "offered neighbour %+v more than widen %d after %+v", s.e, widen, run[last].e)
			}
		}
		i = j
	}
	return nil
}

func offeredAt(o journalv2.Offer, off int64) bool {
	for _, d := range o.Dirty {
		if covers(d.Extent, off) {
			return true
		}
	}
	for _, n := range o.Neighbours {
		if covers(n.Extent, off) {
			return true
		}
	}
	for _, s := range o.Superseded {
		if covers(s.Extent, off) {
			return true
		}
	}
	return false
}

// readsAs reads e from r and returns what differs from the bytes st holds
// there, or "" when nothing does.
func readsAs(r io.ReaderAt, e extent, st *fileState) string {
	if r == nil {
		return "the offer has no Offered reader"
	}
	buf := make([]byte, e.Len)
	if _, err := r.ReadAt(buf, e.Off); err != nil {
		return fmt.Sprintf("Offered failed reading %+v: %v", e, err)
	}
	for i, b := range buf {
		if want := st.held[e.Off+int64(i)].b; b != want {
			return fmt.Sprintf("Offered byte %d is %q, not %q", e.Off+int64(i), b, want)
		}
	}
	return ""
}

// Report applies a call of report during the offer of file id through share
// tag, naming reported, to which report returned err.
//
// A report naming an extent nothing of which was offered must fail with
// ErrNotOffered and mark nothing. Once EndOffer was called, report must fail
// with ErrOfferEnded. Otherwise it marks each offered extent where reported, at
// the extent's version, so content newer than the offer stays dirty and
// content removed meanwhile is not marked.
//
// decision: a reported extent only partly offered may be refused with
// ErrNotOffered, marking nothing, or accepted and intersected with the offer.
// The journal may intersect a report with what it offered, and must reject an
// extent that was not offered; an extent that was partly offered is both.
// Require one outcome once the journal's report documents which.
func (m *Model) Report(tag shareTag, id fileID, reported []extent, err error) error {
	const op = "report"
	o := m.offers[id]
	switch {
	case o == nil || o.tag != tag:
		return m.mismatch(op, id, "reported with no offer of the file; call Offer first")
	case o.ended:
		if !errors.Is(err, journalv2.ErrOfferEnded) {
			return m.mismatch(op, id, "report after the offer ended returned %v, not ErrOfferEnded", err)
		}
		return nil
	}

	var offeredExtents []extent
	for _, p := range o.pieces {
		offeredExtents = append(offeredExtents, p.Extent)
	}
	for _, n := range o.neighbours {
		offeredExtents = append(offeredExtents, n.Extent)
	}
	partly := false
	for _, r := range reported {
		if r.Len <= 0 || coveredBy(r, offeredExtents) {
			continue
		}
		if !slices.ContainsFunc(offeredExtents, func(e extent) bool { _, ok := intersect(e, r); return ok }) {
			if !errors.Is(err, journalv2.ErrNotOffered) {
				return m.mismatch(op, id, "report of %+v, not offered, returned %v, not ErrNotOffered", r, err)
			}
			return nil
		}
		partly = true
	}
	switch {
	case partly && errors.Is(err, journalv2.ErrNotOffered):
		return nil
	case err != nil:
		return unexpected(m, op, id, err)
	}

	f := m.files[id]
	if f == nil {
		return nil
	}
	for _, p := range o.pieces {
		for _, r := range reported {
			if e, ok := intersect(p.Extent, r); ok {
				m.append(f, &record{kind: kindMark, ext: e, ver: p.Version})
			}
		}
	}
	return nil
}

// EndOffer records that the callback of the offer of file id returned: a
// later report must fail.
func (m *Model) EndOffer(tag shareTag, id fileID) {
	if o := m.offers[id]; o != nil && o.tag == tag {
		o.ended = true
	}
}

// Release applies a release of extents of file id through share tag, to which
// the journal returned err. It must be refused with ErrNotOffloaded, releasing
// nothing, when any byte it names is held with its offloaded bit unset;
// otherwise every held byte it names is released at the version held there,
// and absent bytes are left as they are.
func (m *Model) Release(tag shareTag, id fileID, extents []extent, err error) error {
	const op = "release"
	f, proceed, mis := m.enter(op, tag, id, err)
	switch {
	case mis != nil:
		return mis
	case errors.Is(err, journalv2.ErrOtherShare):
		return nil
	}

	cur := m.current(id)
	dirtyAt := int64(-1)
	for off := range cur.held {
		if !cur.bit[off] && slices.ContainsFunc(extents, func(e extent) bool { return covers(e, off) }) {
			dirtyAt = off
			break
		}
	}
	switch {
	case errors.Is(err, journalv2.ErrNotOffloaded):
		if dirtyAt < 0 {
			return m.mismatch(op, id, "refused as not offloaded, but every byte named is offloaded or absent")
		}
		return nil
	case !proceed:
		return unexpected(m, op, id, err)
	case dirtyAt >= 0:
		return m.mismatch(op, id, "released byte %d, whose offloaded bit is unset", dirtyAt)
	}

	for _, r := range cur.runs() {
		for _, e := range extents {
			if piece, ok := intersect(r.ext, e); ok {
				f = m.claim(f, tag, id)
				m.append(f, &record{kind: kindRelease, ext: piece, ver: r.ver})
			}
		}
	}
	return nil
}
