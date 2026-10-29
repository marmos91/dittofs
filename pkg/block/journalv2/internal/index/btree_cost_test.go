package index

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync/atomic"
	"testing"
)

// costs is what one tree size costs, in comparisons per operation.
type costs struct {
	lookup, insert float64
}

// measureCosts builds a tree of n random keys, then averages the comparisons
// of random floor lookups, and of inserting a fresh key and deleting it again
// so that every measured insert sees a tree of n keys.
func measureCosts(n int) costs {
	const samples = 10_000
	rng := rand.New(rand.NewPCG(uint64(n), 1))
	tr := btree[int]{cmps: new(atomic.Uint64)}
	for tr.len() < n {
		tr.set(rng.Uint64(), 0)
	}

	before := tr.comparisons()
	for range samples {
		tr.floor(rng.Uint64())
	}
	lookup := float64(tr.comparisons()-before) / samples

	var inserted uint64
	for range samples {
		k := rng.Uint64()
		before := tr.comparisons()
		if _, replaced := tr.set(k, 0); replaced {
			continue
		}
		inserted += tr.comparisons() - before
		tr.delete(k)
	}
	return costs{lookup: lookup, insert: float64(inserted) / samples}
}

// TestComparisonsGrowLogarithmically is the "index cost" check: going from
// 10^3 to 10^6 keys must at most double the comparisons per lookup and per
// insert, the ratio log(10^6) / log(10^3) an O(log n) structure gives. It
// catches a linear walk of the keys and a tree that has stopped balancing,
// without timing anything.
//
// The ratio has little headroom: with nodes of 31 keys a correct B-tree lands
// at about 1.88 for lookups and 1.99 for inserts, and with nodes of 16 just
// above 2, from rounding alone. Nor can it see a linear scan inside each node,
// which with nodes of bounded size is still logarithmic overall and lands near
// 2. So both are also held to absolute bounds: lookups to 1.25 log2(n), which
// binary search within nodes meets at about 1.03 and a linear in-node scan
// exceeds at about 2.5; inserts, which also test for an exact hit, to
// 1.5 log2(n), met at about 1.25 and exceeded by a linear scan at about 2.8.
//
// Both are held from below too, or a counter that stopped counting would pass
// every bound above at zero. No search by comparisons tells n keys apart in
// much under log2(n) on average; 0.9 log2(n) leaves room for the random
// spacing of the keys, which lets an optimal search do a little better.
func TestComparisonsGrowLogarithmically(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a tree of 10^6 keys")
	}
	small, large := measureCosts(1_000), measureCosts(1_000_000)
	t.Logf("comparisons at 10^3: lookup %.1f, insert %.1f", small.lookup, small.insert)
	t.Logf("comparisons at 10^6: lookup %.1f, insert %.1f", large.lookup, large.insert)
	if large.lookup > 2*small.lookup {
		t.Errorf("lookup: %.1f comparisons at 10^6, more than twice %.1f at 10^3", large.lookup, small.lookup)
	}
	if large.insert > 2*small.insert {
		t.Errorf("insert: %.1f comparisons at 10^6, more than twice %.1f at 10^3", large.insert, small.insert)
	}
	if limit := 1.25 * math.Log2(1_000_000); large.lookup > limit {
		t.Errorf("lookup: %.1f comparisons at 10^6, above %.1f: nodes are not binary searched", large.lookup, limit)
	}
	if limit := 1.5 * math.Log2(1_000_000); large.insert > limit {
		t.Errorf("insert: %.1f comparisons at 10^6, above %.1f: nodes are not binary searched", large.insert, limit)
	}
	for n, c := range map[int]costs{1_000: small, 1_000_000: large} {
		if least := 0.9 * math.Log2(float64(n)); c.lookup < least || c.insert < least {
			t.Errorf("at %d keys: lookup %.1f, insert %.1f comparisons, below %.1f: comparisons go uncounted", n, c.lookup, c.insert, least)
		}
	}
}

// TestAscendSearchesOnce checks that an ascend pays for one descent, not one
// search per node it visits: visiting 10^4 keys must cost no more comparisons
// than visiting one. It catches a walk that bounds every subtree by the start
// key, which returns the right keys but turns O(log n + k) into O(k log n).
func TestAscendSearchesOnce(t *testing.T) {
	tr := btree[int]{cmps: new(atomic.Uint64)}
	for i := range 100_000 {
		tr.set(uint64(i), i)
	}
	cost := func(visit int) uint64 {
		before := tr.comparisons()
		seen := 0
		tr.ascend(500, func(uint64, int) bool { seen++; return seen < visit })
		return tr.comparisons() - before
	}
	one, many := cost(1), cost(10_000)
	if one == 0 {
		t.Fatal("ascend made no comparisons: it goes uncounted")
	}
	if many > one {
		t.Errorf("ascend over 10^4 keys made %d comparisons, over one key %d", many, one)
	}
}

// TestAllocations asserts what the read and write paths allocate. Lookups,
// ascends, deletes and replaces allocate nothing;
// inserts allocate the nodes their splits create and nothing else: one
// allocation a leaf, two an inner node, for its child array. It catches a
// value boxed into an interface, a closure that escapes, a node that grows a
// slice instead of using its fixed arrays, and a split that allocates scratch
// space.
//
// The counter is process-wide, so the runtime or the testing package can add
// to it while the inserts run: usually nothing, once 64 allocations. So the
// bound allows an eighth over the nodes; a split that allocates anything
// doubles the count.
func TestAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector changes allocation counts")
	}
	rng := rand.New(rand.NewPCG(9, 9))
	var tr btree[int]
	for tr.len() < 100_000 {
		tr.set(rng.Uint64(), 1)
	}
	held, _, ok := tr.floor(rng.Uint64())
	if !ok {
		t.Fatal("no key to replace: the replace case would measure an insert")
	}
	var victims []uint64
	tr.ascend(0, func(k uint64, _ int) bool { victims = append(victims, k); return len(victims) < 2_000 })

	zero := map[string]func(){
		"floor": func() { tr.floor(rng.Uint64()) },
		"ascend": func() {
			sum := 0
			tr.ascend(rng.Uint64(), func(_ uint64, v int) bool { sum += v; return sum < 50 })
		},
		"replace": func() { tr.set(held, 2) },
		"delete": func() {
			if _, ok := tr.delete(victims[0]); !ok {
				t.Fatalf("delete(%d) found no key", victims[0])
			}
			victims = victims[1:]
		},
	}
	for name, op := range zero {
		if got := testing.AllocsPerRun(1000, op); got != 0 {
			t.Errorf("%s: %v allocations per call, want 0", name, got)
		}
	}

	const inserts = 100_000
	keys := make([]uint64, inserts)
	for i := range keys {
		keys[i] = rng.Uint64()
	}
	leavesBefore, innerBefore := countNodes(&tr)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for _, k := range keys {
		tr.set(k, 1)
	}
	runtime.ReadMemStats(&after)
	leavesAfter, innerAfter := countNodes(&tr)
	nodes := uint64(leavesAfter - leavesBefore + 2*(innerAfter-innerBefore))
	if got := after.Mallocs - before.Mallocs; got > nodes+nodes/8 {
		t.Errorf("%d inserts made %d allocations, for %d allocations of new nodes: something beyond node splits allocates", inserts, got, nodes)
	}
}

// TestFrontToBackInsertsFillLeaves checks that keys inserted in ascending
// order, and in nearly ascending order — each key up to a few places late, as
// a client with several writes in flight sends them — leave the leaves nearly
// full. Biased, the split leaves them about 94% full. It catches the loss of
// the bias at the right edge, a weaker bias, and a bias kept only for a key
// above every key of the node: an even split there leaves a leaf about half
// full for good.
//
// Lateness is drawn per key rather than by shuffling fixed blocks: blocks of a
// fixed size line up with the period at which the right edge splits, and for
// some sizes every split then strands keys below the median, whatever the
// node size, which a client's window of writes in flight does not do.
func TestFrontToBackInsertsFillLeaves(t *testing.T) {
	for _, late := range []float64{0, 2, 4} {
		t.Run(fmt.Sprintf("late=%v", late), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(uint64(late), 5))
			keys := make([]uint64, 100_000)
			arrival := make([]float64, len(keys))
			for i := range keys {
				keys[i], arrival[i] = uint64(i), float64(i)+late*rng.Float64()
			}
			slices.SortFunc(keys, func(a, b uint64) int { return cmp.Compare(arrival[a], arrival[b]) })
			var tr btree[int]
			for _, k := range keys {
				tr.set(k, 0)
			}
			if fill := leafFill(&tr); fill < 0.92 {
				t.Errorf("leaves %.1f%% full, want at least 92%%", 100*fill)
			}
		})
	}
}

func BenchmarkFloor(b *testing.B) {
	for _, n := range []int{1_000, 1_000_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(1, 1))
			var tr btree[int]
			for tr.len() < n {
				tr.set(rng.Uint64(), 0)
			}
			b.ReportAllocs()
			for b.Loop() {
				tr.floor(rng.Uint64())
			}
		})
	}
}

// BenchmarkFloorParallel measures lookups from concurrent readers, the case
// a shared counter or any other write on the read path would slow down.
func BenchmarkFloorParallel(b *testing.B) {
	rng := rand.New(rand.NewPCG(1, 1))
	var tr btree[int]
	for tr.len() < 100_000 {
		tr.set(rng.Uint64(), 0)
	}
	b.RunParallel(func(pb *testing.PB) {
		r := rand.New(rand.NewPCG(2, 2))
		for pb.Next() {
			tr.floor(r.Uint64())
		}
	})
}

func BenchmarkSetDelete(b *testing.B) {
	for _, n := range []int{1_000, 1_000_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(1, 1))
			var tr btree[int]
			for tr.len() < n {
				tr.set(rng.Uint64(), 0)
			}
			b.ReportAllocs()
			for b.Loop() {
				k := rng.Uint64()
				tr.set(k, 0)
				tr.delete(k)
			}
		})
	}
}
