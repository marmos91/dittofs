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
	// The order slice is allocated before the first measurement and kept
	// alive past the second, so its 8n bytes count on neither side of the
	// difference: a slice the build loop outlives would be freed by the
	// second read's collection and come off every entry's figure.
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
	runtime.KeepAlive(perm)
	// The subtraction is signed, so a shrinking heap shows as negative and
	// fails the lower bound.
	return float64(int64(after)-int64(before)) / float64(n)
}

// randomTreeBytesPerEntry measures a bare tree of n entries, written in the
// order perm gives — front to back when perm is nil. The order slice is
// allocated before the first measurement and kept alive past the second, so
// its 8n bytes count on neither side of the difference, exactly as the
// index-level measurement holds its own order slice alive.
func randomTreeBytesPerEntry[V any](n int, perm []int, val func(i int) V) float64 {
	if perm == nil {
		perm = make([]int, n)
		for i := range perm {
			perm[i] = i
		}
	}
	before := prof.LiveHeap()
	t := &btree[V]{}
	for _, j := range perm {
		t.set(uint64(j)*pieceLen, val(j))
	}
	after := prof.LiveHeap()
	if t.len() != n {
		panic("randomTreeBytesPerEntry: entries lost")
	}
	runtime.KeepAlive(t)
	runtime.KeepAlive(perm)
	return float64(int64(after)-int64(before)) / float64(n)
}

// TestIndexBytesPerEntry is the index memory check, measured through the
// whole index: one file of 10^6 extents, each its own record, written through
// admission and commit as the journal writes, must hold at most 56 bytes an
// entry written sequentially and at most 72 written in random order. The
// bounds are the RFC's own, not a decision. Catches entry or node growth,
// and an unmeasured heap: the check holds from below too, at an entry's own
// width plus its key.
//
// decision: the check measures one file, the population the per-entry
// figures describe. A file's own cost does not shrink with its extents: its
// first leaf is a full 1,536-byte node, so a file of one extent costs about
// 1,960 bytes and 10^6 extents over 10^4 files about 86 an entry. Overturned
// if the extent bound must cap memory for journals of many small files,
// which needs the bound to count files too, or a first leaf sized to its
// keys.
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
	least := float64(unsafe.Sizeof(held{}) + 8)
	if sequential < least || random < least {
		t.Errorf("bytes per entry below the %.0f an entry itself takes: the heap goes unmeasured (%.1f, %.1f)", least, sequential, random)
	}
}

// TestBytesPerEntryFailsWithOneMoreField is the committed proof that the
// memory check fails when one 8-byte field is added to the entry: an index
// that grew an entry would pass nothing here. Both bounds are proved, in both
// orders: the sequential leg holds the held tree within the 56 the RFC holds
// sequential order to and the fielded one above it, and the random leg the
// same against the 72. The proof measures the tree the index holds through
// the honest measurement the RFC check itself uses — same helper, same
// liveness rules, only the value type grown — so a bias in one shows in the
// other.
func TestBytesPerEntryFailsWithOneMoreField(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("builds a tree of 10^6 entries and measures its heap")
	}
	const n = 1_000_000
	heldVal := func(i int) held {
		return held{version: ver(uint64(i + 1)), segment: core.SegmentID(1 + i/60_000), recordOff: uint32(i%60_000) * pieceLen, len: pieceLen}
	}
	fieldedVal := func(i int) heldPlusField {
		return heldPlusField{held: heldVal(i), extra: uint64(i)}
	}

	sequential := randomTreeBytesPerEntry(n, nil, heldVal)
	t.Logf("tree of held entries, front to back: %.1f bytes per entry", sequential)
	if sequential > 56 {
		t.Errorf("%.1f bytes per entry for the held tree front to back, above the 56 the RFC bounds sequential order to: the calibration fails", sequential)
	}
	sequentialFielded := randomTreeBytesPerEntry(n, nil, fieldedVal)
	t.Logf("fielded tree of %d-byte entries, front to back: %.1f bytes per entry", unsafe.Sizeof(heldPlusField{}), sequentialFielded)
	if sequentialFielded <= 56 {
		t.Errorf("%.1f bytes per entry for the fielded tree front to back, at or below 56: a grown entry passes the check", sequentialFielded)
	}

	perm := rand.New(rand.NewPCG(6, 6)).Perm(n)
	randomCalibration := randomTreeBytesPerEntry(n, perm, heldVal)
	t.Logf("tree of held entries, random order: %.1f bytes per entry", randomCalibration)
	if randomCalibration > 72 {
		t.Errorf("%.1f bytes per entry for the held tree in random order, above the 72 the RFC bounds it to: the calibration fails", randomCalibration)
	}
	randomFielded := randomTreeBytesPerEntry(n, perm, fieldedVal)
	t.Logf("fielded tree of %d-byte entries, random order: %.1f bytes per entry", unsafe.Sizeof(heldPlusField{}), randomFielded)
	if randomFielded <= 72 {
		t.Errorf("%.1f bytes per entry for the fielded tree in random order, at or below 72: a grown entry passes the check", randomFielded)
	}
}
