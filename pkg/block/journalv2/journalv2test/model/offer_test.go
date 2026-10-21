package model

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// offered is an Offered reader over the bytes a test says the journal froze,
// failing outside them as the journal's must.
type offered map[int64]byte

func (o offered) ReadAt(p []byte, off int64) (int, error) {
	for i := range p {
		b, ok := o[off+int64(i)]
		if !ok {
			return i, journalv2.ErrNotOffered
		}
		p[i] = b
	}
	return len(p), nil
}

// frozen returns the Offered reader of an offer whose pieces read as the
// model's file reads now at each piece's version.
func frozen(m *Model, id journalv2.FileID, pieces ...journalv2.OfferedExtent) offered {
	o := offered{}
	f := m.files[id]
	for _, p := range pieces {
		for _, r := range f.recs {
			if r.ver != p.Version || r.data == nil {
				continue
			}
			for i, b := range r.data {
				if off := r.ext.Off + int64(i); covers(p.Extent, off) {
					o[off] = b
				}
			}
		}
	}
	return o
}

func dirty(off, n int64, v uint64) journalv2.OfferedExtent {
	return journalv2.OfferedExtent{Extent: ext(off, n), Version: ver(v)}
}

func offerOf(m *Model, id journalv2.FileID, oldest, newest uint64, pieces ...journalv2.OfferedExtent) journalv2.Offer {
	return journalv2.Offer{ID: id, Dirty: pieces, Oldest: ver(oldest), Newest: ver(newest), Offered: frozen(m, id, pieces...)}
}

// The offer covers [0, 40) at v10, and a client writes [10, 20) at v11 while
// it runs. The report of [0, 40) marks [0, 10) and [20, 40) only: the newer
// write's bit stays unset, and the next pass offers it.
func TestReportNeverMarksAWriteNewerThanTheOffer(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 40), 0, ver(10), nil))
	o := offerOf(m, fid(1), 10, 10, dirty(0, 40, 10))
	ok(t, m.Offer(shareA, fid(1), ver(10), 0, 0, o))
	ok(t, m.WriteAt(shareA, fid(1), 10, fill('B', 10), 0, ver(11), nil))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 40)}, nil))
	m.EndOffer(shareA, fid(1))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 10, 10, true},
		{10, 10, 11, false},
		{20, 20, 10, true},
	})
	ok(t, m.Offer(shareA, fid(1), ver(11), 0, 0, offerOf(m, fid(1), 11, 11, dirty(10, 10, 11))))
}

// An extent above through is not offered: the newer version waits for a later
// call. Where it overwrote one at or below through, the older one may still be
// offered as the file's live content.
func TestAnOfferStopsAtThrough(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 40), 0, ver(10), nil))
	ok(t, m.WriteAt(shareA, fid(1), 10, fill('B', 10), 0, ver(12), nil))

	ok(t, m.Offer(shareA, fid(1), ver(10), 0, 0, offerOf(m, fid(1), 10, 10, dirty(0, 40, 10))))
	ok(t, m.Offer(shareA, fid(1), ver(10), 0, 0, offerOf(m, fid(1), 10, 10, dirty(0, 10, 10), dirty(20, 20, 10))))
	refused(t, m.Offer(shareA, fid(1), ver(10), 0, 0, offerOf(m, fid(1), 10, 12, dirty(0, 10, 10), dirty(10, 10, 12))))
}

// The first extent always fits, so an offer is never empty while dirty
// content at or below through remains.
func TestAnOfferIsNeverEmptyWhileDirtyContentRemains(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 40), 0, ver(10), nil))
	refused(t, m.Offer(shareA, fid(1), ver(10), 0, 0, journalv2.Offer{ID: fid(1), Offered: offered{}}))
	ok(t, m.Offer(shareA, fid(1), ver(9), 0, 0, journalv2.Offer{ID: fid(1), Offered: offered{}}))
}

// Offers the RFC does not allow are reported.
func TestOffersTheRFCForbids(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 20), 0, ver(1), nil))
		ok(t, m.WriteAt(shareA, fid(1), 30, fill('B', 10), 0, ver(2), nil))
		return m
	}
	cases := []struct {
		name  string
		limit int64
		offer func(m *Model) journalv2.Offer
	}{
		{"more than limit", 25, func(m *Model) journalv2.Offer {
			return offerOf(m, fid(1), 1, 2, dirty(0, 20, 1), dirty(30, 10, 2))
		}},
		{"out of offset order", 0, func(m *Model) journalv2.Offer {
			return offerOf(m, fid(1), 1, 2, dirty(30, 10, 2), dirty(0, 20, 1))
		}},
		{"wrong version", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))
			o.Dirty[0].Version = ver(2)
			o.Oldest, o.Newest = ver(2), ver(2)
			return o
		}},
		{"bytes not the ones written", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))
			o.Offered.(offered)[5] = 'X'
			return o
		}},
		{"stamped with no stamp given", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))
			o.Dirty[0].Stamped = true
			return o
		}},
		{"oldest and newest not the offer's bounds", 0, func(m *Model) journalv2.Offer {
			return offerOf(m, fid(1), 1, 3, dirty(0, 20, 1), dirty(30, 10, 2))
		}},
		{"content never written", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 1, 1, dirty(0, 25, 1))
			for i := int64(20); i < 25; i++ {
				o.Offered.(offered)[i] = 0
			}
			return o
		}},
		{"neighbours without widen", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 1, 2, dirty(0, 20, 1))
			o.Neighbours = []journalv2.Held{{Extent: ext(30, 10), Version: ver(2)}}
			return o
		}},
		{"a readable byte outside the offer", 0, func(m *Model) journalv2.Offer {
			o := offerOf(m, fid(1), 2, 2, dirty(30, 10, 2))
			o.Offered.(offered)[29] = 'A'
			return o
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := setup(t)
			refused(t, m.Offer(shareA, fid(1), ver(2), c.limit, 0, c.offer(m)))
		})
	}
}

// Offloaded neighbours contiguous with the dirty extents are offered up to
// widen bytes on each side; a dirty one or one past widen is not a neighbour.
func TestNeighboursAreOffloadedAndWithinWiden(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 20), 0, ver(1), nil))
		ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))))
		ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 20)}, nil))
		m.EndOffer(shareA, fid(1))
		ok(t, m.WriteAt(shareA, fid(1), 20, fill('B', 10), 0, ver(2), nil))
		return m
	}
	withNeighbour := func(m *Model, n journalv2.Held) journalv2.Offer {
		o := offerOf(m, fid(1), 1, 2, dirty(20, 10, 2))
		o.Neighbours = []journalv2.Held{n}
		for k, b := range frozen(m, fid(1), journalv2.OfferedExtent{Extent: n.Extent, Version: n.Version}) {
			o.Offered.(offered)[k] = b
		}
		return o
	}

	m := setup(t)
	ok(t, m.Offer(shareA, fid(1), ver(2), 0, 8, withNeighbour(m, journalv2.Held{Extent: ext(12, 8), Version: ver(1)})))
	m = setup(t)
	refused(t, m.Offer(shareA, fid(1), ver(2), 0, 4, withNeighbour(m, journalv2.Held{Extent: ext(12, 8), Version: ver(1)})))
	m = setup(t)
	refused(t, m.Offer(shareA, fid(1), ver(2), 0, 8, withNeighbour(m, journalv2.Held{Extent: ext(10, 8), Version: ver(1)})))
}

// A reported extent nothing of which was offered is rejected with
// ErrNotOffered and marks nothing; a partial report marks exactly what it names; once the offer
// is over, report fails with ErrOfferEnded.
func TestReportMarksExactlyWhatWasOfferedAndReported(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 40), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 20, 0, offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))))

	refused(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(25, 5)}, nil))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(25, 5)}, journalv2.ErrNotOffered))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	m.EndOffer(shareA, fid(1))
	refused(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(10, 10)}, nil))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(10, 10)}, journalv2.ErrOfferEnded))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 10, 1, true},
		{10, 30, 1, false},
	})
}

// A removal does not interrupt an offer, and a report naming an extent removed
// meanwhile marks nothing, because nothing is held there.
func TestReportAfterARemovalMarksNothingThere(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 40), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 40, 1))))
	ok(t, m.Deallocate(shareA, fid(1), ext(0, 10), ver(2), nil))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 40)}, nil))
	m.EndOffer(shareA, fid(1))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 10), 0, ver(3), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 10, 3, false},
		{10, 30, 1, true},
	})
}

// Release refuses an extent whose offloaded bit is unset and makes no partial
// progress; an offloaded extent it releases is absent afterwards.
func TestReleaseRefusesDirtyBytesWithNoPartialProgress(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 20), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 20, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	m.EndOffer(shareA, fid(1))

	both := []journalv2.Extent{ext(0, 10), ext(10, 10)}
	refused(t, m.Release(shareA, fid(1), both, nil))
	ok(t, m.Release(shareA, fid(1), both, journalv2.ErrNotOffloaded))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, true}, {10, 10, 1, false}})

	refused(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, journalv2.ErrNotOffloaded))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{10, 10, 1, false}})
}

// Every record of one file, decided by precedence: the higher version wins
// and, between equal versions, the later record. A release covers the content
// it released, and a fill of the same version after it is held again. The
// held extents are (0, 4K) v7 offloaded, (4K, 8K) v8 dirty and (20K, 4K) v9
// dirty, with 12K–20K not held.
func TestRebuildByPrecedence(t *testing.T) {
	const k = 1 << 10
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 8*k), 0, ver(7), nil))
	ok(t, m.WriteAt(shareA, fid(1), 4*k, fill('B', 8*k), 0, ver(8), nil))
	ok(t, m.Offer(shareA, fid(1), ver(7), 0, 0, offerOf(m, fid(1), 7, 7, dirty(0, 8*k, 7))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 8*k)}, nil))
	m.EndOffer(shareA, fid(1))
	ok(t, m.Release(shareA, fid(1), []journalv2.Extent{ext(0, 4*k)}, nil))
	ok(t, m.WriteAt(shareA, fid(1), 20*k, fill('C', 4*k), 0, ver(9), nil))
	// Another file's record between them changes nothing of this one.
	ok(t, m.WriteAt(shareA, fid(2), 0, fill('x', k), 0, ver(10), nil))
	ok(t, m.Fill(shareA, fid(1), 0, fill('A', 4*k), ver(10), ver(7), nil))

	equalSpans(t, heldSpans(t, m, fid(1)), []span{
		{0, 4 * k, 7, true},
		{4 * k, 8 * k, 8, false},
		{20 * k, 4 * k, 9, false},
	})
}

// An offer captures its records synced: they are made durable before the
// callback is called, so a crash after it cannot lose them.
func TestAnOfferSyncsWhatItCaptures(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
	m.EndOffer(shareA, fid(1))
	ok(t, m.Crash())
	ok(t, m.Reopen(ver(10)))
	refused(t, m.Compare(shareA, modelView{New(), shareA}))
}

// Offloaded content is not dirty and is never offered as such.
func TestAnOfferNeverOffersOffloadedContentAsDirty(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
	m.EndOffer(shareA, fid(1))
	ok(t, m.WriteAt(shareA, fid(1), 20, fill('B', 10), 0, ver(2), nil))
	refused(t, m.Offer(shareA, fid(1), ver(2), 0, 0, offerOf(m, fid(1), 1, 2, dirty(0, 10, 1), dirty(20, 10, 2))))
}

// A dirty extent is never a neighbour, and a neighbour after the dirty
// extents lies within widen of them too.
func TestNeighboursAreNeitherDirtyNorPastWidenOnEitherSide(t *testing.T) {
	m := New()
	ok(t, m.WriteAt(shareA, fid(1), 10, fill('A', 20), 0, ver(1), nil))
	ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(10, 20, 1))))
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(10, 20)}, nil))
	m.EndOffer(shareA, fid(1))
	ok(t, m.WriteAt(shareA, fid(1), 0, fill('B', 10), 0, ver(2), nil))
	ok(t, m.WriteAt(shareA, fid(1), 30, fill('C', 5), 0, ver(3), nil))

	withNeighbour := func(n journalv2.Held, newest uint64) journalv2.Offer {
		o := offerOf(m, fid(1), 1, newest, dirty(0, 10, 2))
		o.Neighbours = []journalv2.Held{n}
		for k, b := range frozen(m, fid(1), journalv2.OfferedExtent{Extent: n.Extent, Version: n.Version}) {
			o.Offered.(offered)[k] = b
		}
		return o
	}
	ok(t, m.Offer(shareA, fid(1), ver(2), 0, 8, withNeighbour(journalv2.Held{Extent: ext(10, 8), Version: ver(1)}, 2)))
	refused(t, m.Offer(shareA, fid(1), ver(2), 0, 8, withNeighbour(journalv2.Held{Extent: ext(10, 20), Version: ver(1)}, 2)))
	o := withNeighbour(journalv2.Held{Extent: ext(10, 20), Version: ver(1)}, 3)
	o.Neighbours = append(o.Neighbours, journalv2.Held{Extent: ext(30, 5), Version: ver(3)})
	for k, b := range frozen(m, fid(1), dirty(30, 5, 3)) {
		o.Offered.(offered)[k] = b
	}
	refused(t, m.Offer(shareA, fid(1), ver(3), 0, 64, o))
}

// A report naming a neighbour is accepted and changes nothing, since the
// neighbour is already marked; one only partly offered may be refused with
// ErrNotOffered or intersected with the offer.
func TestReportOfNeighboursAndOfPartlyOfferedExtents(t *testing.T) {
	setup := func(t *testing.T) *Model {
		m := New()
		ok(t, m.WriteAt(shareA, fid(1), 0, fill('A', 10), 0, ver(1), nil))
		ok(t, m.Offer(shareA, fid(1), ver(1), 0, 0, offerOf(m, fid(1), 1, 1, dirty(0, 10, 1))))
		ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(0, 10)}, nil))
		m.EndOffer(shareA, fid(1))
		ok(t, m.WriteAt(shareA, fid(1), 10, fill('B', 10), 0, ver(2), nil))
		o := offerOf(m, fid(1), 1, 2, dirty(10, 10, 2))
		o.Neighbours = []journalv2.Held{{Extent: ext(5, 5), Version: ver(1)}}
		for k, b := range frozen(m, fid(1), dirty(5, 5, 1)) {
			o.Offered.(offered)[k] = b
		}
		ok(t, m.Offer(shareA, fid(1), ver(2), 0, 5, o))
		return m
	}

	m := setup(t)
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(5, 5)}, nil))

	m = setup(t)
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(15, 10)}, journalv2.ErrNotOffered))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, true}, {10, 10, 2, false}})

	m = setup(t)
	ok(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(15, 10)}, nil))
	equalSpans(t, heldSpans(t, m, fid(1)), []span{{0, 10, 1, true}, {10, 5, 2, false}, {15, 5, 2, true}})

	m = setup(t)
	refused(t, m.Report(shareA, fid(1), []journalv2.Extent{ext(30, 5)}, nil))
}
