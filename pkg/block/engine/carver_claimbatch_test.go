package engine

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// carveHash returns a distinct, non-zero ContentHash seeded from n.
func carveHash(n byte) block.ContentHash {
	var h block.ContentHash
	for i := range h {
		h[i] = n ^ byte(i)
	}
	return h
}

// TestClaimCarveBatch_UnderTargetLeavesQueueUntouched pins the de-quadratic
// contract: when the queued bytes are below target and we are not draining,
// claimCarveBatch must return nil WITHOUT detaching or re-copying carveQ (the
// old pop-then-prepend went O(n) on every carveWake).
func TestClaimCarveBatch_UnderTargetLeavesQueueUntouched(t *testing.T) {
	m := &Syncer{pendingCarveHashes: map[block.ContentHash]int64{}}
	h1, h2 := carveHash(1), carveHash(2)
	m.carveQ = []block.ContentHash{h1, h2}
	m.pendingCarveHashes[h1] = 10
	m.pendingCarveHashes[h2] = 10
	before := &m.carveQ[0] // backing-array identity

	if got := m.claimCarveBatch(25, false); got != nil {
		t.Fatalf("under-target claim = %v, want nil", got)
	}
	if len(m.carveQ) != 2 || m.carveQ[0] != h1 || m.carveQ[1] != h2 {
		t.Fatalf("carveQ mutated on under-target peek: %v", m.carveQ)
	}
	if &m.carveQ[0] != before {
		t.Fatal("carveQ backing array was reallocated on an under-target peek (quadratic churn)")
	}
}

// TestClaimCarveBatch_ReachesTargetClaimsPrefixKeepsSuffix verifies that once
// the accumulated size reaches target the FIFO prefix is claimed and detached,
// leaving the untouched suffix for the next pass.
func TestClaimCarveBatch_ReachesTargetClaimsPrefixKeepsSuffix(t *testing.T) {
	m := &Syncer{pendingCarveHashes: map[block.ContentHash]int64{}}
	h1, h2, h3 := carveHash(1), carveHash(2), carveHash(3)
	m.carveQ = []block.ContentHash{h1, h2, h3}
	for _, h := range []block.ContentHash{h1, h2, h3} {
		m.pendingCarveHashes[h] = 10
	}

	// target 15: h1(10) then h2(20>=15) -> claim [h1,h2], leave [h3].
	got := m.claimCarveBatch(15, false)
	if len(got) != 2 || got[0] != h1 || got[1] != h2 {
		t.Fatalf("claimed = %v, want [h1 h2] (FIFO prefix)", got)
	}
	if len(m.carveQ) != 1 || m.carveQ[0] != h3 {
		t.Fatalf("suffix = %v, want [h3]", m.carveQ)
	}
}

// TestClaimCarveBatch_DrainAllClaimsRemainderSkippingStale verifies drainAll
// claims the whole remaining queue even when under target, releases the backing
// array, and drops stale (already-committed) hashes from the batch.
func TestClaimCarveBatch_DrainAllClaimsRemainderSkippingStale(t *testing.T) {
	m := &Syncer{pendingCarveHashes: map[block.ContentHash]int64{}}
	h1, h2, h3 := carveHash(1), carveHash(2), carveHash(3)
	m.carveQ = []block.ContentHash{h1, h2, h3}
	// h2 is stale: present in the FIFO but not in the pending map.
	m.pendingCarveHashes[h1] = 10
	m.pendingCarveHashes[h3] = 10

	got := m.claimCarveBatch(1000, true) // target far above total, but drainAll
	if len(got) != 2 || got[0] != h1 || got[1] != h3 {
		t.Fatalf("claimed = %v, want [h1 h3] (stale h2 dropped, FIFO)", got)
	}
	if m.carveQ != nil {
		t.Fatalf("carveQ = %v, want nil (backing array released after full drain)", m.carveQ)
	}
}
