package index

import (
	"math/rand/v2"
	"runtime"
	"testing"
	"unsafe"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// heldPlusField is a held entry with one more 8-byte field, the shape
// TestBytesPerEntryFailsWithOneMoreField measures. The field is 8 bytes
// because a smaller one that fits held's padding changes nothing.
type heldPlusField struct {
	held
	extra uint64
}

// indexBytesPerEntry builds one file of n extents, each its own record,
// written in the order order gives, through admission and commit as the
// journal writes, and returns the heap the whole index holds per entry.
func indexBytesPerEntry(n int, order func(i int) int) float64 {
	// The order slice is allocated before the first measurement, so it costs
	// the structure nothing.
	perm := make([]int, n)
	for i := range perm {
		perm[i] = order(i)
	}
	before := prof.LiveHeap()
	fs := &files{}
	fs.setExtentLimit(1 << 40)
	id := fid(1)
	for i, j := range perm {
		e := ext(int64(j)*pieceLen, pieceLen)
		r := rec(core.SegmentID(1+j/60_000), uint32(j%60_000)*pieceLen)
		v := ver(uint64(i + 1))
		adm, err := fs.admitWrite(id, e, false)
		if err != nil {
			panic(err)
		}
		err = fs.commit(id, adm, func(f *file) error {
			if err := f.recorded(r.Segment, e, Numbers{Version: v, Seq: core.Seq(i + 1)}, 1, id); err != nil {
				return err
			}
			f.write(Placed{Extent: e, Record: r, Version: v})
			return nil
		})
		if err != nil {
			panic(err)
		}
	}
	after := prof.LiveHeap()
	if fs.extentCount() != int64(n) {
		panic("indexBytesPerEntry: the count disagrees with the extents written")
	}
	runtime.KeepAlive(fs)
	// The subtraction is signed, so a shrinking heap shows as negative and
	// fails the lower bound.
	return float64(int64(after)-int64(before)) / float64(n)
}

// treeBytesPerEntry measures a bare tree of n entries, written front to back,
// the calibration the one-more-field check runs against.
func treeBytesPerEntry[V any](n int, val func(i int) V) float64 {
	before := prof.LiveHeap()
	t := &btree[V]{}
	for i := range n {
		t.set(uint64(i)*pieceLen, val(i))
	}
	after := prof.LiveHeap()
	if t.len() != n {
		panic("treeBytesPerEntry: entries lost")
	}
	runtime.KeepAlive(t)
	return float64(int64(after)-int64(before)) / float64(n)
}

// TestIndexBytesPerEntry is RFC 1 §9.7's index memory check, measured
// through the whole index: one file of 10^6 extents, each its own record,
// written through admission and commit as the journal writes, must hold
// "bytes per entry at 10^6 extents at or below 56 written sequentially and
// at or below 72 written in random order, the figures of §4.2 plus a tenth".
// The bounds are the RFC's own, not a decision. Catches entry or node growth,
// and an unmeasured heap: the check holds from below too, at the size an
// entry itself takes. The floor is the entry's own width rather than a wide
// margin over it, because the sequential figure sweeps a little under the
// bare tree's — the lazy sweep after the collection defers some of the
// difference — and a floor the honest measurement dips below would fail
// a correct index.
func TestIndexBytesPerEntry(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("builds a file of 10^6 extents and measures its heap")
	}
	const n = 1_000_000
	perm := rand.New(rand.NewPCG(6, 6)).Perm(n)
	sequential := indexBytesPerEntry(n, func(i int) int { return i })
	random := indexBytesPerEntry(n, func(i int) int { return perm[i] })
	t.Logf("bytes per entry at 10^6 through the index: %.1f written front to back, %.1f in random order", sequential, random)
	if sequential > 56 {
		t.Errorf("%.1f bytes per entry for 10^6 extents written front to back, above 56", sequential)
	}
	if random > 72 {
		t.Errorf("%.1f bytes per entry for 10^6 extents written in random order, above 72", random)
	}
	least := float64(unsafe.Sizeof(held{}))
	if sequential < least || random < least {
		t.Errorf("bytes per entry below the %.0f an entry itself takes: the heap goes unmeasured (%.1f, %.1f)", least, sequential, random)
	}
}

// TestBytesPerEntryFailsWithOneMoreField is the committed proof that the
// memory check fails when one 8-byte field is added to the entry: an index
// that grew an entry would pass nothing here. The held entry stays within the
// 56 the RFC holds sequential writes to; the fielded one leaves it.
func TestBytesPerEntryFailsWithOneMoreField(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("builds a tree of 10^6 entries and measures its heap")
	}
	const n = 1_000_000
	held58 := treeBytesPerEntry(n, func(i int) held {
		return held{version: ver(uint64(i + 1)), segment: core.SegmentID(1 + i/60_000), recordOff: uint32(i%60_000) * pieceLen, len: pieceLen}
	})
	t.Logf("byte tree of held entries: %.1f bytes per entry", held58)
	if held58 > 56 {
		t.Errorf("%.1f bytes per entry for the held tree, above the 56 the RFC bounds sequential order to: the calibration fails", held58)
	}
	withField := treeBytesPerEntry(n, func(i int) heldPlusField {
		return heldPlusField{held: held{version: ver(uint64(i + 1)), segment: core.SegmentID(1 + i/60_000), recordOff: uint32(i%60_000) * pieceLen, len: pieceLen}, extra: uint64(i)}
	})
	t.Logf("fielded tree of %d-byte entries: %.1f bytes per entry", unsafe.Sizeof(heldPlusField{}), withField)
	if withField <= 56 {
		t.Errorf("%.1f bytes per entry for the fielded tree, at or below 56: a grown entry passes the check", withField)
	}
}
