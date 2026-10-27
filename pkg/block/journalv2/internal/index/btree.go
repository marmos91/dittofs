// Package index is the journal's placement index: for each file, which
// records hold which byte ranges of it. It is pure in-memory state, rebuilt
// from records at open and never stored, and it does no I/O.
package index

import "sync/atomic"

// A node holds up to maxItems keys. Every node but the root and those on the
// rightmost path from it holds at least minItems, so a merge of a node at
// minItems with a sibling at minItems, plus their separator, always fits.
//
// ponytail: leaves are only as full as the insert order leaves them: about 94%
// for ascending or nearly ascending keys, 69% for random ones, 59% after heavy
// churn and 50% for descending ones. A held extent's entry is 48 bytes, its
// 8-byte key beside a 40-byte value, so a node of 32 is 1,552 bytes, which the
// allocator rounds up to 1,792: ascending keys cost about 60 bytes an entry
// and random ones about 82, and the index's budget is 56, so no insert order
// meets it. Upgrade before bytes per entry are checked against that budget.
// 31 slots fit the 1,536-byte size class and bring ascending keys to about
// 53; splitting into a sibling with room, as a B*-tree does, raises the fill
// of the other orders.
const (
	maxItems = 32
	minItems = (maxItems - 1) / 2
)

// node is one packed B-tree node. Keys and values sit inline in fixed arrays,
// so a split costs one allocation and a lookup touches one contiguous block
// per level. Only inner nodes allocate the child array, so leaves, which are
// nearly all nodes, carry one nil pointer in its place rather than 33 child
// pointers. A vacated value slot is cleared, so a removed value keeps nothing
// reachable; a vacated key slot is left as it is.
type node[V any] struct {
	count    int
	keys     [maxItems]uint64
	vals     [maxItems]V
	children *[maxItems + 1]*node[V]
}

// btree is an ordered map from uint64 keys to values, kept as a B-tree.
// Lookup, insert and delete cost O(log n) comparisons; ascend costs
// O(log n + k) for k keys visited. It is not safe for concurrent mutation:
// the caller serialises writers, and readers against writers.
//
// decision: keys are unsigned, and the tree trusts them. The journal's API
// takes offsets as int64, so a negative offset converted to a key becomes one
// of the largest, and floor would answer with a file's last extent instead of
// nothing. Refusing a negative offset, or an offset and length whose sum
// overflows, is the API's job, before anything reaches the index. Check here
// as well if a caller is ever found converting without that refusal.
type btree[V any] struct {
	root *node[V]
	n    int

	// cmps, when set, counts key comparisons, so a check can tell an O(log n)
	// tree from one that has degraded without timing anything. It is nil
	// outside such checks.
	//
	// decision: counting is opt-in. Readers of one tree run concurrently, and
	// one shared atomic add per lookup made eight concurrent readers about five
	// times slower, 58 ns a lookup against 12 ns, as the cache line holding the
	// counter moved between cores. Each operation counts in a local variable,
	// which costs nothing measurable, and adds the total once at its end only
	// when counting is on.
	cmps *atomic.Uint64
}

// len returns the number of keys held.
func (t *btree[V]) len() int { return t.n }

// comparisons returns the key comparisons counted so far, or zero when the
// tree does not count.
func (t *btree[V]) comparisons() uint64 {
	if t.cmps == nil {
		return 0
	}
	return t.cmps.Load()
}

// counted adds one operation's comparisons to the counter, if there is one.
func (t *btree[V]) counted(c uint64) {
	if t.cmps != nil {
		t.cmps.Add(c)
	}
}

// floor returns the greatest key at or below k, and its value. A tree keyed on
// the start offsets of disjoint extents answers "which extent covers k" with
// it.
func (t *btree[V]) floor(k uint64) (key uint64, v V, ok bool) {
	var cmps uint64
	for n := t.root; n != nil; {
		i, c := n.upperBound(k)
		cmps += c
		if i > 0 {
			key, v, ok = n.keys[i-1], n.vals[i-1], true
		}
		if n.children == nil {
			break
		}
		n = n.children[i]
	}
	t.counted(cmps)
	return key, v, ok
}

// ascend calls fn on every key at or above from, in ascending order, until fn
// returns false. fn may replace a visited key's value with set, which never
// changes the tree's shape, but must not insert or delete: either moves keys
// between nodes under the walk, and the walk then skips keys without failing.
func (t *btree[V]) ascend(from uint64, fn func(k uint64, v V) bool) {
	if t.root == nil {
		return
	}
	_, cmps := ascendNode(t.root, from, true, fn)
	t.counted(cmps)
}

// ascendNode visits n's subtree in order. Only the leftmost path it descends
// is bounded by from: every subtree to the right of it holds greater keys.
func ascendNode[V any](n *node[V], from uint64, bounded bool, fn func(uint64, V) bool) (more bool, cmps uint64) {
	start := 0
	if bounded {
		start, cmps = n.lowerBound(from)
	}
	for i := start; i <= n.count; i++ {
		if n.children != nil {
			more, c := ascendNode(n.children[i], from, bounded && i == start, fn)
			cmps += c
			if !more {
				return false, cmps
			}
		}
		if i < n.count && !fn(n.keys[i], n.vals[i]) {
			return false, cmps
		}
	}
	return true, cmps
}

// set inserts k with value v, or replaces the value of k if it is held,
// returning the value it replaced.
//
// The descent changes nothing, so a replace never touches the tree's shape and
// never allocates. Only an insert splits, on the way back up, and only the
// nodes it overflows: the leaf, then each parent its separator fills.
//
// A split on the rightmost path is biased: it leaves the left node nearly full
// instead of half full. Content written front to back appends at the right
// edge, and an even split there would leave every node it passes half empty
// for good. The bias holds wherever the key falls in the node, because writes
// arrive nearly in order rather than in order: a key a few places late lands
// inside the last leaf, and an even split for it would strand that leaf half
// full just the same.
//
// decision: nothing biases the leftmost path, so keys inserted in descending
// order, a file written back to front, leave the leaves about half full. A
// journal's writers rarely write in that order; mirror the bias on the left
// edge if a workload that does shows up in the index's bytes per entry.
func (t *btree[V]) set(k uint64, v V) (old V, replaced bool) {
	if t.root == nil {
		t.root = &node[V]{}
	}
	old, replaced, up, cmps := t.root.insert(k, v, true)
	if up.right != nil {
		r := &node[V]{children: new([maxItems + 1]*node[V])}
		r.keys[0], r.vals[0], r.count = up.key, up.val, 1
		r.children[0], r.children[1] = t.root, up.right
		t.root = r
	}
	if !replaced {
		t.n++
	}
	t.counted(cmps)
	return old, replaced
}

// promoted is what a split hands up to its parent: the median, which moves
// up, and the new node to the median's right. right is nil when nothing split.
type promoted[V any] struct {
	key   uint64
	val   V
	right *node[V]
}

// insert sets k to v in n's subtree, replacing the value if k is held. It
// returns what n's split hands up when the insert overflowed n. rightmost says
// whether n is on the tree's rightmost path.
func (n *node[V]) insert(k uint64, v V, rightmost bool) (old V, replaced bool, up promoted[V], cmps uint64) {
	i, found, cmps := n.search(k)
	if found {
		old, n.vals[i] = n.vals[i], v
		return old, true, up, cmps
	}
	if n.children == nil {
		return old, false, n.insertOrSplit(i, k, v, nil, rightmost), cmps
	}
	old, replaced, below, c := n.children[i].insert(k, v, rightmost && i == n.count)
	cmps += c
	if below.right != nil {
		up = n.insertOrSplit(i, below.key, below.val, below.right, rightmost)
	}
	return old, replaced, up, cmps
}

// insertOrSplit puts k and v at slot i, and right after them in an inner
// node, as insertAt does. A full node is split first, around a median that
// moves up, and the slot goes to whichever half it now falls in. When the
// node is on the rightmost path the split is biased: the median is its
// second-to-last key, so the left half stays nearly full.
func (n *node[V]) insertOrSplit(i int, k uint64, v V, right *node[V], rightmost bool) promoted[V] {
	if n.count < maxItems {
		n.insertAt(i, k, v, right)
		return promoted[V]{}
	}
	mid := maxItems / 2
	if rightmost {
		mid = maxItems - 2
	}
	r := &node[V]{}
	r.count = copy(r.keys[:], n.keys[mid+1:n.count])
	copy(r.vals[:], n.vals[mid+1:n.count])
	if n.children != nil {
		r.children = new([maxItems + 1]*node[V])
		copy(r.children[:], n.children[mid+1:n.count+1])
		clear(n.children[mid+1 : n.count+1])
	}
	up := promoted[V]{key: n.keys[mid], val: n.vals[mid], right: r}
	clear(n.vals[mid:n.count])
	n.count = mid
	if i <= mid {
		n.insertAt(i, k, v, right)
	} else {
		r.insertAt(i-mid-1, k, v, right)
	}
	return up
}

// delete removes k and returns its value, if k is held.
//
// Every child is grown before the delete descends into it, above minItems or,
// on the rightmost path, by at least one key, so removing one key from it
// never needs a pass back up to rebalance.
func (t *btree[V]) delete(k uint64) (v V, ok bool) {
	if t.root == nil {
		return v, false
	}
	_, v, ok, cmps := t.root.remove(k, false)
	if t.root.count == 0 {
		if t.root.children == nil {
			t.root = nil
		} else {
			t.root = t.root.children[0]
		}
	}
	if ok {
		t.n--
	}
	t.counted(cmps)
	return v, ok
}

// remove deletes k from n's subtree, or its greatest key when greatest is set.
func (n *node[V]) remove(k uint64, greatest bool) (uint64, V, bool, uint64) {
	var cmps uint64
	for {
		i, found, c := n.locate(k, greatest)
		cmps += c
		if n.children == nil {
			if !found {
				var zero V
				return 0, zero, false, cmps
			}
			rk, rv, _ := n.removeAt(i)
			return rk, rv, true, cmps
		}
		if n.children[i].count <= minItems {
			// Growing the child may move k: down into the child, by a merge
			// or by a rotation from the right. So locate it again.
			n.growChild(i)
			i, found, c = n.locate(k, greatest)
			cmps += c
		}
		if found {
			// k separates two children: its predecessor, the greatest key
			// of the left one, takes its place.
			rk, rv := n.keys[i], n.vals[i]
			pk, pv, _, c := n.children[i].remove(0, true)
			n.keys[i], n.vals[i] = pk, pv
			return rk, rv, true, cmps + c
		}
		n = n.children[i]
	}
}

// locate finds where a remove continues in n: the slot of k, or the child it
// would be under; for the greatest key, the last slot of a leaf or the last
// child of an inner node.
func (n *node[V]) locate(k uint64, greatest bool) (i int, found bool, cmps uint64) {
	if !greatest {
		return n.search(k)
	}
	if n.children == nil {
		return n.count - 1, true, 0
	}
	return n.count, false, 0
}

// growChild lifts child i above minItems: by rotating a key through n from a
// sibling that can spare one, or else by merging the child with a sibling.
func (n *node[V]) growChild(i int) {
	c := n.children[i]
	if i > 0 && n.children[i-1].count > minItems {
		lk, lv, lc := n.children[i-1].popLast()
		c.pushFirst(n.keys[i-1], n.vals[i-1], lc)
		n.keys[i-1], n.vals[i-1] = lk, lv
		return
	}
	if i < n.count && n.children[i+1].count > minItems {
		rk, rv, rc := n.children[i+1].popFirst()
		c.pushLast(n.keys[i], n.vals[i], rc)
		n.keys[i], n.vals[i] = rk, rv
		return
	}
	if i == n.count {
		i--
	}
	l := n.children[i]
	k, v, r := n.removeAt(i)
	l.keys[l.count], l.vals[l.count] = k, v
	l.count++
	copy(l.keys[l.count:], r.keys[:r.count])
	copy(l.vals[l.count:], r.vals[:r.count])
	if l.children != nil {
		copy(l.children[l.count:], r.children[:r.count+1])
	}
	l.count += r.count
}

// lowerBound returns the index of the first key at or above k.
func (n *node[V]) lowerBound(k uint64) (int, uint64) {
	lo, hi, cmps := 0, n.count, uint64(0)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmps++
		if n.keys[mid] < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, cmps
}

// upperBound returns the index of the first key above k.
func (n *node[V]) upperBound(k uint64) (int, uint64) {
	lo, hi, cmps := 0, n.count, uint64(0)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		cmps++
		if n.keys[mid] <= k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, cmps
}

// search returns the index of the first key at or above k, and whether it is
// k.
func (n *node[V]) search(k uint64) (i int, found bool, cmps uint64) {
	i, cmps = n.lowerBound(k)
	if i < n.count {
		cmps++
		found = n.keys[i] == k
	}
	return i, found, cmps
}

// insertAt puts k and v at slot i and, in an inner node, right as the child
// after them.
func (n *node[V]) insertAt(i int, k uint64, v V, right *node[V]) {
	copy(n.keys[i+1:n.count+1], n.keys[i:n.count])
	copy(n.vals[i+1:n.count+1], n.vals[i:n.count])
	n.keys[i], n.vals[i] = k, v
	if n.children != nil {
		copy(n.children[i+2:n.count+2], n.children[i+1:n.count+1])
		n.children[i+1] = right
	}
	n.count++
}

// removeAt takes out slot i and, in an inner node, the child after it.
func (n *node[V]) removeAt(i int) (k uint64, v V, right *node[V]) {
	k, v = n.keys[i], n.vals[i]
	copy(n.keys[i:], n.keys[i+1:n.count])
	copy(n.vals[i:], n.vals[i+1:n.count])
	n.count--
	clear(n.vals[n.count : n.count+1])
	if n.children != nil {
		right = n.children[i+1]
		copy(n.children[i+1:], n.children[i+2:n.count+2])
		n.children[n.count+1] = nil
	}
	return k, v, right
}

// popLast takes out the last key and, in an inner node, the last child.
func (n *node[V]) popLast() (k uint64, v V, child *node[V]) {
	if n.children != nil {
		child = n.children[n.count]
		n.children[n.count] = nil
	}
	n.count--
	k, v = n.keys[n.count], n.vals[n.count]
	clear(n.vals[n.count : n.count+1])
	return k, v, child
}

// popFirst takes out the first key and, in an inner node, the first child.
func (n *node[V]) popFirst() (k uint64, v V, child *node[V]) {
	k, v = n.keys[0], n.vals[0]
	if n.children != nil {
		child = n.children[0]
		copy(n.children[:], n.children[1:n.count+1])
		n.children[n.count] = nil
	}
	copy(n.keys[:], n.keys[1:n.count])
	copy(n.vals[:], n.vals[1:n.count])
	n.count--
	clear(n.vals[n.count : n.count+1])
	return k, v, child
}

// pushFirst puts k and v before every key and, in an inner node, child before
// every child.
func (n *node[V]) pushFirst(k uint64, v V, child *node[V]) {
	copy(n.keys[1:n.count+1], n.keys[:n.count])
	copy(n.vals[1:n.count+1], n.vals[:n.count])
	n.keys[0], n.vals[0] = k, v
	if n.children != nil {
		copy(n.children[1:n.count+2], n.children[:n.count+1])
		n.children[0] = child
	}
	n.count++
}

// pushLast puts k and v after every key and, in an inner node, child after
// every child.
func (n *node[V]) pushLast(k uint64, v V, child *node[V]) {
	n.keys[n.count], n.vals[n.count] = k, v
	n.count++
	if n.children != nil {
		n.children[n.count] = child
	}
}
