package index

import (
	"fmt"
	"math"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

// oracle is the sorted-slice reference the tree is checked against: the
// simplest structure with the same contract, whose correctness is evident.
type oracle struct {
	keys []uint64
	vals []int
}

func (o *oracle) set(k uint64, v int) (int, bool) {
	i, found := slices.BinarySearch(o.keys, k)
	if found {
		old := o.vals[i]
		o.vals[i] = v
		return old, true
	}
	o.keys = slices.Insert(o.keys, i, k)
	o.vals = slices.Insert(o.vals, i, v)
	return 0, false
}

func (o *oracle) delete(k uint64) (int, bool) {
	i, found := slices.BinarySearch(o.keys, k)
	if !found {
		return 0, false
	}
	v := o.vals[i]
	o.keys = slices.Delete(o.keys, i, i+1)
	o.vals = slices.Delete(o.vals, i, i+1)
	return v, true
}

func (o *oracle) floor(k uint64) (uint64, int, bool) {
	i, found := slices.BinarySearch(o.keys, k)
	if found {
		return o.keys[i], o.vals[i], true
	}
	if i == 0 {
		return 0, 0, false
	}
	return o.keys[i-1], o.vals[i-1], true
}

type pair struct {
	k uint64
	v int
}

func (o *oracle) ascend(from uint64, limit int) []pair {
	i, _ := slices.BinarySearch(o.keys, from)
	var out []pair
	for ; i < len(o.keys) && len(out) < limit; i++ {
		out = append(out, pair{o.keys[i], o.vals[i]})
	}
	return out
}

func collect(tr *btree[int], from uint64, limit int) []pair {
	var out []pair
	tr.ascend(from, func(k uint64, v int) bool {
		if len(out) == limit {
			return false
		}
		out = append(out, pair{k, v})
		return len(out) < limit
	})
	return out
}

// checkInvariants walks the whole tree and fails on any broken structural
// rule. A tree can answer every query right and still be malformed — an
// overfull node, uneven leaf depth, a stale value left in a vacated slot — and
// the malformation only shows as a wrong answer, or a held reference, later.
func checkInvariants(t *testing.T, tr *btree[int]) {
	t.Helper()
	if tr.root == nil {
		if tr.n != 0 {
			t.Fatalf("nil root but len %d", tr.n)
		}
		return
	}
	leafDepth := -1
	count := 0
	var walk func(n *node[int], lo, hi uint64, hasLo, hasHi bool, depth int, isRoot, rightSpine bool)
	walk = func(n *node[int], lo, hi uint64, hasLo, hasHi bool, depth int, isRoot, rightSpine bool) {
		switch {
		case n.count > maxItems:
			t.Fatalf("node at depth %d holds %d keys, above %d", depth, n.count, maxItems)
		case isRoot && n.count == 0:
			t.Fatalf("non-empty tree has a root with no keys")
		case !isRoot && !rightSpine && n.count < minItems:
			t.Fatalf("node at depth %d holds %d keys, below %d", depth, n.count, minItems)
		case !isRoot && n.count == 0:
			t.Fatalf("node at depth %d on the right spine holds no keys", depth)
		}
		for j := 0; j < n.count; j++ {
			k := n.keys[j]
			if j > 0 && n.keys[j-1] >= k {
				t.Fatalf("keys out of order at depth %d: %d then %d", depth, n.keys[j-1], k)
			}
			if (hasLo && k <= lo) || (hasHi && k >= hi) {
				t.Fatalf("key %d at depth %d outside its parent's separators", k, depth)
			}
		}
		for j := n.count; j < maxItems; j++ {
			if n.vals[j] != 0 {
				t.Fatalf("vacated value slot %d at depth %d not cleared", j, depth)
			}
		}
		count += n.count
		if n.children == nil {
			if leafDepth == -1 {
				leafDepth = depth
			} else if leafDepth != depth {
				t.Fatalf("leaves at depths %d and %d", leafDepth, depth)
			}
			return
		}
		for j := 0; j <= n.count; j++ {
			c := n.children[j]
			if c == nil {
				t.Fatalf("inner node at depth %d missing child %d of %d", depth, j, n.count+1)
			}
			clo, chi, cHasLo, cHasHi := lo, hi, hasLo, hasHi
			if j > 0 {
				clo, cHasLo = n.keys[j-1], true
			}
			if j < n.count {
				chi, cHasHi = n.keys[j], true
			}
			walk(c, clo, chi, cHasLo, cHasHi, depth+1, false, rightSpine && j == n.count)
		}
		for j := n.count + 1; j <= maxItems; j++ {
			if n.children[j] != nil {
				t.Fatalf("inner node at depth %d keeps a stale child in slot %d", depth, j)
			}
		}
	}
	walk(tr.root, 0, 0, false, false, 0, true, true)
	if count != tr.n {
		t.Fatalf("len %d, but the tree holds %d keys", tr.n, count)
	}
	if tr.len() != tr.n {
		t.Fatalf("len() %d, field %d", tr.len(), tr.n)
	}
}

func TestEmptyTree(t *testing.T) {
	var tr btree[int]
	if _, _, ok := tr.floor(math.MaxUint64); ok {
		t.Error("floor on an empty tree found a key")
	}
	if _, ok := tr.delete(0); ok {
		t.Error("delete on an empty tree removed a key")
	}
	if got := collect(&tr, 0, 10); len(got) != 0 {
		t.Errorf("ascend on an empty tree yielded %v", got)
	}
	if tr.len() != 0 {
		t.Errorf("len %d", tr.len())
	}
}

// TestFloorAcrossNodeBoundaries checks floor for every offset over a tree deep
// enough to have inner nodes. It catches a predecessor search that stays in the
// leaf it lands in: when an offset falls between a parent's separator and the
// first key of the leaf to its right, the answer is the separator, one level
// up.
func TestFloorAcrossNodeBoundaries(t *testing.T) {
	var tr btree[int]
	const keys, step = 2000, 10
	for i := range keys {
		tr.set(uint64(i*step+step), i)
	}
	checkInvariants(t, &tr)
	if tr.root == nil || tr.root.children == nil {
		t.Fatal("tree has no inner node; the test does not reach a node boundary")
	}
	for off := uint64(0); off <= keys*step+2*step; off++ {
		k, v, ok := tr.floor(off)
		if off < step {
			if ok {
				t.Fatalf("floor(%d) = %d, want none below the first key", off, k)
			}
			continue
		}
		want := min(off/step*step, keys*step)
		if !ok || k != want || v != int(want/step)-1 {
			t.Fatalf("floor(%d) = (%d, %d, %v), want (%d, %d, true)", off, k, v, ok, want, want/step-1)
		}
	}
}

// TestKeyExtremes covers the two ends of the key space, where an
// implementation that uses a sentinel key or computes k+1 or k-1 wraps.
func TestKeyExtremes(t *testing.T) {
	var tr btree[int]
	tr.set(0, 1)
	tr.set(math.MaxUint64, 2)
	if k, v, ok := tr.floor(0); !ok || k != 0 || v != 1 {
		t.Errorf("floor(0) = (%d, %d, %v)", k, v, ok)
	}
	if k, v, ok := tr.floor(math.MaxUint64 - 1); !ok || k != 0 || v != 1 {
		t.Errorf("floor(max-1) = (%d, %d, %v)", k, v, ok)
	}
	if k, v, ok := tr.floor(math.MaxUint64); !ok || k != math.MaxUint64 || v != 2 {
		t.Errorf("floor(max) = (%d, %d, %v)", k, v, ok)
	}
	if got := collect(&tr, math.MaxUint64, 10); !slices.Equal(got, []pair{{math.MaxUint64, 2}}) {
		t.Errorf("ascend(max) = %v", got)
	}
}

// TestSetReplaces catches a replace counted as an insert, which leaves len
// wrong and, in a tree, a duplicate key.
func TestSetReplaces(t *testing.T) {
	var tr btree[int]
	for i := range 100 {
		tr.set(uint64(i), i)
	}
	old, replaced := tr.set(42, -1)
	if !replaced || old != 42 {
		t.Errorf("set over an existing key returned (%d, %v), want (42, true)", old, replaced)
	}
	if tr.len() != 100 {
		t.Errorf("len %d after a replace, want 100", tr.len())
	}
	if _, v, _ := tr.floor(42); v != -1 {
		t.Errorf("floor(42) value %d, want the replacement -1", v)
	}
	checkInvariants(t, &tr)
}

// TestAscendFrom catches an ascend that starts at the predecessor of an absent
// start key instead of its successor, and one that ignores an early stop.
func TestAscendFrom(t *testing.T) {
	var tr btree[int]
	for i := range 500 {
		tr.set(uint64(i*2), i)
	}
	got := collect(&tr, 301, 3)
	want := []pair{{302, 151}, {304, 152}, {306, 153}}
	if !slices.Equal(got, want) {
		t.Errorf("ascend(301) = %v, want %v", got, want)
	}
	calls := 0
	tr.ascend(0, func(uint64, int) bool { calls++; return false })
	if calls != 1 {
		t.Errorf("ascend called fn %d times after it returned false, want 1", calls)
	}
	if got := collect(&tr, 999, 10); len(got) != 0 {
		t.Errorf("ascend past the last key yielded %v", got)
	}
}

// runOps drives the tree and the oracle with the same operations and fails at
// the first divergence or broken invariant.
func runOps(t *testing.T, tr *btree[int], o *oracle, ops int, key func() uint64, rng *rand.Rand) {
	t.Helper()
	for i := range ops {
		k := key()
		switch op := rng.IntN(10); {
		case op < 5:
			gotOld, gotRep := tr.set(k, i)
			wantOld, wantRep := o.set(k, i)
			if gotOld != wantOld || gotRep != wantRep {
				t.Fatalf("op %d set(%d) = (%d, %v), want (%d, %v)", i, k, gotOld, gotRep, wantOld, wantRep)
			}
		case op < 8:
			gotV, gotOK := tr.delete(k)
			wantV, wantOK := o.delete(k)
			if gotV != wantV || gotOK != wantOK {
				t.Fatalf("op %d delete(%d) = (%d, %v), want (%d, %v)", i, k, gotV, gotOK, wantV, wantOK)
			}
		case op < 9:
			gk, gv, gok := tr.floor(k)
			wk, wv, wok := o.floor(k)
			if gk != wk || gv != wv || gok != wok {
				t.Fatalf("op %d floor(%d) = (%d, %d, %v), want (%d, %d, %v)", i, k, gk, gv, gok, wk, wv, wok)
			}
		default:
			if got, want := collect(tr, k, 20), o.ascend(k, 20); !slices.Equal(got, want) {
				t.Fatalf("op %d ascend(%d) = %v, want %v", i, k, got, want)
			}
		}
		checkInvariants(t, tr)
	}
	if got, want := collect(tr, 0, math.MaxInt), o.ascend(0, math.MaxInt); !slices.Equal(got, want) {
		t.Fatalf("final contents differ: %d keys, want %d", len(got), len(want))
	}
}

// TestAgainstOracle runs random operation sequences over a narrow key range,
// where sets collide with existing keys and deletes hit, so nodes split, borrow
// and merge constantly; and over the full range, where they mostly do not. It
// catches keys lost or duplicated on a split or merge, a borrow from the wrong
// sibling, and a separator left stale after a delete.
func TestAgainstOracle(t *testing.T) {
	for _, keySpace := range []uint64{64, 1000, math.MaxUint64} {
		for seed := range uint64(40) {
			t.Run(fmt.Sprintf("keys=%d/seed=%d", keySpace, seed), func(t *testing.T) {
				rng := rand.New(rand.NewPCG(seed, keySpace))
				var tr btree[int]
				var o oracle
				runOps(t, &tr, &o, 3000, func() uint64 { return rng.Uint64N(keySpace) }, rng)
			})
		}
	}
}

// TestSequentialThenChurn fills the tree in ascending and in descending key
// order — the shapes a file written front to back, or back to front, gives —
// and then churns it at random. Ascending order takes the append path through
// the right spine; descending order the leftmost path.
func TestSequentialThenChurn(t *testing.T) {
	for _, ascending := range []bool{true, false} {
		t.Run(fmt.Sprintf("ascending=%v", ascending), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(7, 7))
			var tr btree[int]
			var o oracle
			const n = 5000
			for i := range n {
				k := uint64(i)
				if !ascending {
					k = n - uint64(i)
				}
				tr.set(k, i)
				o.set(k, i)
			}
			checkInvariants(t, &tr)
			runOps(t, &tr, &o, 4000, func() uint64 { return rng.Uint64N(n + 100) }, rng)
		})
	}
}

// TestDeleteAll drains a deep tree in random order. It catches a root that is
// not shrunk when it loses its last key, and a merge that leaves an empty node.
func TestDeleteAll(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	var tr btree[int]
	keys := rng.Perm(5000)
	for _, k := range keys {
		tr.set(uint64(k), k)
	}
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	for _, k := range keys {
		if v, ok := tr.delete(uint64(k)); !ok || v != k {
			t.Fatalf("delete(%d) = (%d, %v)", k, v, ok)
		}
		checkInvariants(t, &tr)
	}
	if tr.root != nil || tr.len() != 0 {
		t.Fatalf("drained tree keeps root %v, len %d", tr.root, tr.len())
	}
}

// TestConcurrentReaders runs lookups and ascends on one tree from several
// goroutines at once, as readers of one file do under a shared lock. Reads
// change nothing but the comparison counter, so it catches, under -race, a
// counter updated without synchronisation.
func TestConcurrentReaders(t *testing.T) {
	tr := btree[int]{cmps: new(atomic.Uint64)}
	for i := range 10_000 {
		tr.set(uint64(i), i)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for k := range uint64(2_000) {
				tr.floor(k)
				tr.ascend(k, func(uint64, int) bool { return false })
			}
		})
	}
	wg.Wait()
	if tr.comparisons() == 0 {
		t.Fatal("readers made no comparisons")
	}
}

// TestAppendBiasStaysOnTheRightEdge builds a path that follows the right edge
// down to a full inner node and turns left only at its last step, into a full
// leaf, then inserts a key above every key of that leaf. Every node above the
// leaf is on the right edge and the key looks like an append, but the leaf is
// not on the edge, so it must split evenly; its separator then overflows the
// inner node, which is, and that one splits biased. It catches an insert that
// keeps treating the path as rightmost after it turned left, which would leave
// a node of two keys inside the tree, where it never fills again.
func TestAppendBiasStaysOnTheRightEdge(t *testing.T) {
	var tr btree[int]
	next := uint64(0)
	appendKey := func() { tr.set(next, 0); next += 10 }
	lastChild := func() *node[int] { return tr.root.children[tr.root.count] }

	// Append until the root's last child is an inner node with a leaf in slot
	// target, one of its last children but not the last, so that later
	// appends leave it alone.
	const target = maxItems - 2
	for tr.root == nil || tr.root.children == nil || lastChild().children == nil || lastChild().count <= target {
		appendKey()
	}
	inner := lastChild()
	leaf := inner.children[target]
	for k := leaf.keys[0] + 1; leaf.count < maxItems; k++ {
		tr.set(k, 0)
	}
	for lastChild() == inner && inner.count < maxItems {
		appendKey()
	}
	if lastChild() != inner || inner.count != maxItems || inner.children[target] != leaf || leaf.count != maxItems {
		t.Fatal("setup did not build a full inner node on the right edge over a full leaf left of its median")
	}
	k := inner.keys[target] - 5
	if k <= leaf.keys[maxItems-1] {
		t.Fatal("setup left no gap above the leaf")
	}

	tr.set(k, 0)
	checkInvariants(t, &tr)
}

// countNodes returns how many leaves and inner nodes the tree holds.
func countNodes(tr *btree[int]) (leaves, inner int) {
	var walk func(n *node[int])
	walk = func(n *node[int]) {
		if n.children == nil {
			leaves++
			return
		}
		inner++
		for _, c := range n.children[:n.count+1] {
			walk(c)
		}
	}
	if tr.root != nil {
		walk(tr.root)
	}
	return leaves, inner
}

// leafFill returns the share of leaf slots that hold a key. Separator keys in
// inner nodes do not count: they are not what leaves hold.
func leafFill(tr *btree[int]) float64 {
	keys, leaves := 0, 0
	var walk func(n *node[int])
	walk = func(n *node[int]) {
		if n.children == nil {
			keys += n.count
			leaves++
			return
		}
		for _, c := range n.children[:n.count+1] {
			walk(c)
		}
	}
	if tr.root != nil {
		walk(tr.root)
	}
	return float64(keys) / float64(leaves*maxItems)
}

// TestReplaceKeepsTheShape replaces a key held in a full root leaf, the path
// on which an insert must split, and checks that the tree's nodes are the
// ones it had before and that nothing was allocated. It catches an insert
// that splits full nodes on its way down, before it knows whether the key is
// already held: a replace then restructures the tree, allocates three times
// and leaves two half-empty leaves where one full one was. Narrowing an extent
// and marking it offloaded are both replaces, so they would pay for it.
func TestReplaceKeepsTheShape(t *testing.T) {
	var tr btree[int]
	for i := range uint64(maxItems) {
		tr.set(i*2, 0)
	}
	root := tr.root
	if root.children != nil || root.count != maxItems {
		t.Fatalf("setup: root holds %d keys, inner %v; want one full leaf", root.count, root.children != nil)
	}

	k := uint64(maxItems / 2 * 2)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	old, replaced := tr.set(k, 1)
	runtime.ReadMemStats(&after)

	if !replaced || old != 0 {
		t.Fatalf("set(%d) = (%d, %v), want a replace of 0", k, old, replaced)
	}
	if tr.root != root || root.children != nil || root.count != maxItems {
		t.Errorf("replace restructured the tree: root %p was %p, holds %d keys", tr.root, root, tr.root.count)
	}
	if !raceEnabled && after.Mallocs != before.Mallocs {
		t.Errorf("replace allocated %d times, want 0", after.Mallocs-before.Mallocs)
	}
	checkInvariants(t, &tr)
}

// TestReplaceKeepsTheShapeBelowAFullChild replaces keys held inside full
// leaves that have roomy right siblings, in a tree deep enough that the
// descent passes an inner node: the exact shape conditions a rotation runs
// on, which a replace must not take. It catches an insert that rotates full
// children on its way down, before it knows whether the key is held: each
// such replace moves a key between two leaves and rewrites a separator,
// where the tree it answers from should not have moved at all.
func TestReplaceKeepsTheShapeBelowAFullChild(t *testing.T) {
	var tr btree[int]
	rng := rand.New(rand.NewPCG(7, 7))
	for tr.len() < 100_000 {
		tr.set(rng.Uint64(), 0)
	}
	if tr.root.children == nil {
		t.Fatal("setup: the tree has no inner nodes")
	}

	// The shape is the keys of every node, in walk order: node identities
	// alone cannot catch a rotation, which rewrites two children and their
	// separator in place.
	shape := func() [][]uint64 {
		var out [][]uint64
		var walk func(n *node[int])
		walk = func(n *node[int]) {
			out = append(out, slices.Clone(n.keys[:n.count]))
			if n.children != nil {
				for _, c := range n.children[:n.count+1] {
					walk(c)
				}
			}
		}
		if tr.root != nil {
			walk(tr.root)
		}
		return out
	}

	// Replace held keys drawn across the tree; most sit in ordinary leaves,
	// and some — on a random tree, many — under a full child with a roomy
	// right sibling, which a replace must leave standing.
	var victims []uint64
	tr.ascend(0, func(k uint64, _ int) bool { victims = append(victims, k); return len(victims) < 2_000 })
	before := shape()
	for _, k := range victims {
		if _, replaced := tr.set(k, 1); !replaced {
			t.Fatalf("set(%d) did not replace a held key", k)
		}
	}
	if after := shape(); !slices.EqualFunc(before, after, func(a, b []uint64) bool { return slices.Equal(a, b) }) {
		t.Fatal("a replace moved a key: the tree's shape changed while only values were set")
	}
	checkInvariants(t, &tr)
}
