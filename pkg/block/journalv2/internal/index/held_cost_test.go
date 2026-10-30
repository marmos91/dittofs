package index

import (
	"math"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

const pieceLen = 4096

// sequentialFile holds n extents written front to back, each its own record,
// so that none merge: the shape of a file written in small sequential writes.
func sequentialFile(n int) *heldSet {
	s := &heldSet{}
	s.tree.cmps = new(atomic.Uint64)
	for i := range n {
		s.insert(write(int64(i)*pieceLen, pieceLen, rec(core.SegmentID(1+i/60_000), uint32(i%60_000)*pieceLen), uint64(i+1)))
	}
	if s.len() != n {
		panic("sequentialFile: extents merged")
	}
	return s
}

// perOp averages the comparisons fn makes over samples calls.
func perOp(s *heldSet, samples int, fn func()) float64 {
	before := s.tree.comparisons()
	for range samples {
		fn()
	}
	return float64(s.tree.comparisons()-before) / float64(samples)
}

type heldCosts struct{ lookup, span, insert, report float64 }

func measureHeldCosts(n int) heldCosts {
	const samples = 2_000
	s := sequentialFile(n)
	rng := rand.New(rand.NewPCG(uint64(n), 3))
	size := int64(n) * pieceLen
	var c heldCosts
	c.lookup = perOp(s, samples, func() { s.lookup(rng.Int64N(size)) })
	var held []Placed
	var missing []core.Extent
	c.span = perOp(s, samples, func() {
		held, missing = s.span(ext(rng.Int64N(size-8*pieceLen), 8*pieceLen), held[:0], missing[:0])
	})
	next := uint32(0)
	c.insert = perOp(s, samples, func() {
		// A three-way split inside one extent, then the same bytes written
		// back whole, so every measured insert sees n extents.
		i := rng.Int64N(int64(n))
		next++
		s.insert(write(i*pieceLen+100, 200, rec(1<<20, next), 1<<40))
		s.insert(write(i*pieceLen, pieceLen, rec(1<<21, next), 1<<40))
	}) / 2
	// The report: one block of four extents at a time, front to back, as an
	// offload pass commits them.
	var marked int
	before := s.tree.comparisons()
	for b := range min(samples, n/4) {
		marked += s.markOffloaded(ext(int64(b)*4*pieceLen, 4*pieceLen), ver(0), ver(math.MaxUint64))
	}
	c.report = float64(s.tree.comparisons()-before) / float64(marked)
	return c
}

// TestHeldCostGrowsLogarithmically is the RFC's "bounds" and "report cost"
// checks: a file of 10^6 extents written sequentially, so none merge, costs
// about the comparisons of one of 10^3 times log(10^6) / log(10^3), per point
// lookup, per span query of a few extents, per insert that splits, and per
// extent a report marks. A linear walk of the extents, or a report that scans
// the file to decide anything, multiplies its count by about a thousand.
//
// decision: the ratio is held at 2.5, not at the 2 the RFC states. A file
// written front to back fills the tree's leaves about 94%, so 10^3 extents sit
// two levels deep and 10^6 five, and binary search costs about five
// comparisons a level: a correct set measures about 2.0 to 2.1 here, from the
// rounding of the tree's height alone. The tree itself is held to the RFC's 2
// in TestComparisonsGrowLogarithmically, on random keys. Every operation is
// also held to an absolute bound, 1.25 log2(n) a descent, as the tree's
// lookups are. A lookup is one descent and a span query two. An insert as
// measured here, a split averaged with an overwrite, makes about ten, cutting,
// setting and checking both ends for a merge, and a report about nine per
// extent it marks; a change that adds descents raises those counts. Return to
// 2 if the RFC is restated with headroom, or if the tree's fill stops
// depending on the order extents are written in.
//
// Each count is held from below too, or a set that stopped going through the
// counted tree would pass every bound at zero.
func TestHeldCostGrowsLogarithmically(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a file of 10^6 extents")
	}
	small, large := measureHeldCosts(1_000), measureHeldCosts(1_000_000)
	t.Logf("comparisons at 10^3: %+v", small)
	t.Logf("comparisons at 10^6: %+v", large)
	const ratio = 2.5
	for _, c := range []struct {
		name         string
		small, large float64
		descents     float64
	}{
		{"lookup", small.lookup, large.lookup, 1},
		{"span", small.span, large.span, 2},
		{"insert", small.insert, large.insert, 10},
		{"report", small.report, large.report, 10},
	} {
		if c.large > ratio*c.small {
			t.Errorf("%s: %.1f comparisons at 10^6, more than %.1f times %.1f at 10^3", c.name, c.large, ratio, c.small)
		}
		if limit := c.descents * 1.25 * math.Log2(1_000_000); c.large > limit {
			t.Errorf("%s: %.1f comparisons at 10^6, above %.1f for %.0f descents", c.name, c.large, limit, c.descents)
		}
		if least := 0.5 * math.Log2(1_000); c.small < least {
			t.Errorf("%s: %.1f comparisons at 10^3, below %.1f: comparisons go uncounted", c.name, c.small, least)
		}
	}
}

// TestHeldAllocations checks what lookups, reads and inserts allocate,
// outside -race. A lookup allocates nothing, and nor does a read into buffers
// the caller reuses. Overwriting a held extent, a write that splits one and
// is written back, a mark and a re-append allocate nothing once the set's
// scratch space has grown, beyond the nodes the tree splits: a mark that ends
// inside an extent adds an entry, and now and then a node. AllocsPerRun
// rounds those down, so the check holds every call to less than one. It
// catches a closure that escapes, a piece boxed into an interface, and scratch
// space allocated per call.
func TestHeldAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	const n = 100_000
	s := sequentialFile(n)
	rng := rand.New(rand.NewPCG(5, 5))
	size := int64(n) * pieceLen
	held := make([]Placed, 0, 64)
	missing := make([]core.Extent, 0, 64)
	next := uint32(0)
	ops := map[string]func(){
		"lookup": func() { s.lookup(rng.Int64N(size)) },
		"span": func() {
			held, missing = s.span(ext(rng.Int64N(size-8*pieceLen)+10, 8*pieceLen), held[:0], missing[:0])
		},
		"overwrite": func() {
			s.insert(write(5*pieceLen, pieceLen, rec(1<<20, 1), 1<<40))
		},
		"split and rejoin": func() {
			i := rng.Int64N(n/2) * 2
			next++
			s.insert(write(i*pieceLen+100, 200, rec(1<<20, next), 1<<40))
			s.insert(write(i*pieceLen, pieceLen, rec(1<<21, next), 1<<40))
		},
		"mark": func() {
			s.markOffloaded(ext(rng.Int64N(size-4*pieceLen), 4*pieceLen), ver(0), ver(math.MaxUint64))
		},
	}
	for name, op := range ops {
		op() // grow the set's scratch space
		if got := testing.AllocsPerRun(500, op); got != 0 {
			t.Errorf("%s: %v allocations per call, want 0", name, got)
		}
	}
	from, to := rec(1<<23, 0), rec(1<<23, 1)
	s.insert(write(9*pieceLen, pieceLen, from, 1<<41))
	if got := testing.AllocsPerRun(500, func() {
		s.repoint(ext(9*pieceLen, pieceLen), from, to, 0)
		from, to = to, from
	}); got != 0 {
		t.Errorf("repoint: %v allocations per call, want 0", got)
	}
}
