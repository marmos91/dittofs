package gencache

import (
	"strconv"
	"testing"
)

// TestCache_BoundedPrunesOnOverflow pins the soft cap: past Cap the map is
// trimmed back toward half, so a hot-path cache cannot grow without bound.
func TestCache_BoundedPrunesOnOverflow(t *testing.T) {
	c := Cache[int]{Cap: 64}
	for i := range 256 {
		c.Store(strconv.Itoa(i), i, c.Generation())
	}
	if n := c.n.Load(); n > c.Cap {
		t.Fatalf("entry count %d exceeds cap %d — the bound never pruned", n, c.Cap)
	}
}

// TestCache_UnboundedNeverPrunes pins the Cap==0 contract sharecache relies on:
// shares are few and every one of them must stay resident, because a miss there
// costs a backend read on the permission funnel.
func TestCache_UnboundedNeverPrunes(t *testing.T) {
	var c Cache[int] // zero value: unbounded
	const n = 1000
	for i := range n {
		c.Store(strconv.Itoa(i), i, c.Generation())
	}
	for i := range n {
		if _, ok := c.Get(strconv.Itoa(i)); !ok {
			t.Fatalf("key %d evicted from an unbounded cache", i)
		}
	}
}

// TestCache_StaleStoreRejected is the generation guard: a populate that
// snapshotted the generation before a racing write committed must be dropped,
// never pinned as a stale hit.
func TestCache_StaleStoreRejected(t *testing.T) {
	var c Cache[int]

	gen := c.Generation() // reader snapshots, then "reads" the backing store
	c.Invalidate("k")     // a concurrent write commits and invalidates
	c.Store("k", 1, gen)  // the stale populate must be dropped

	if _, ok := c.Get("k"); ok {
		t.Fatal("stale entry pinned despite a racing invalidation")
	}
}
