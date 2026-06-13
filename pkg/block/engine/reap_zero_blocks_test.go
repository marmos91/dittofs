package engine

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
)

// TestReapSupersededFileBlocks_ZeroNewBlocks_ReapsAllPrior asserts that when a
// rollup pass produces zero chunks (file truncated to zero bytes), every prior
// FileBlock row is unconditionally reaped. Before the fix the outer guard
// short-circuited on len(newBlocks) == 0, leaving the prior rows (and their
// CAS chunks) orphaned forever.
func TestReapSupersededFileBlocks_ZeroNewBlocks_ReapsAllPrior(t *testing.T) {
	ctx := context.Background()
	coord := newRefcountCoordinator()
	var h0, h1 block.ContentHash
	h0[0] = 0xA0
	h1[0] = 0xA1
	coord.seedBlock("pid-trunc", 0, h0, 1)
	coord.seedBlock("pid-trunc", 4096, h1, 1)

	bs := buildCascadeFixture(t, coord, nil)

	priorOffsets := []uint64{0, 4096}
	var newBlocks []block.BlockRef // empty — truncated to zero

	if err := bs.reapSupersededFileBlocks(ctx, "pid-trunc", priorOffsets, newBlocks); err != nil {
		t.Fatalf("reapSupersededFileBlocks: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	for _, h := range []block.ContentHash{h0, h1} {
		if c := coord.counts[h]; c != 0 {
			t.Errorf("hash %x refcount=%d after reap; want 0 (block not reaped)", h[:2], c)
		}
	}
}

// TestReapSupersededFileBlocks_EmptyPrior_IsNoOp guards that removing the
// len(newBlocks) == 0 short-circuit from the outer guard did not break the
// len(priorOffsets) == 0 no-op path. With no prior rows there is nothing to
// reap regardless of newBlocks.
func TestReapSupersededFileBlocks_EmptyPrior_IsNoOp(t *testing.T) {
	ctx := context.Background()
	coord := newRefcountCoordinator()
	var h0 block.ContentHash
	h0[0] = 0xB0
	coord.seedBlock("pid-empty-prior", 0, h0, 1)

	bs := buildCascadeFixture(t, coord, nil)

	newBlocks := []block.BlockRef{{Hash: h0, Offset: 0, Size: 4096}}

	if err := bs.reapSupersededFileBlocks(ctx, "pid-empty-prior", nil, newBlocks); err != nil {
		t.Fatalf("reapSupersededFileBlocks: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if c := coord.counts[h0]; c != 1 {
		t.Errorf("hash %x refcount=%d after no-op; want 1 (DecrementRefCountAndReap must not fire)", h0[:2], c)
	}
}

// TestReapSupersededFileBlocks_NilCoordinator_IsNoOp asserts the unwired
// coordinator guard still short-circuits even on the zero-blocks path (no
// panic, nil error).
func TestReapSupersededFileBlocks_NilCoordinator_IsNoOp(t *testing.T) {
	ctx := context.Background()
	bs := buildCascadeFixture(t, nil, nil)

	priorOffsets := []uint64{0, 4096}
	var newBlocks []block.BlockRef // empty — truncated to zero

	if err := bs.reapSupersededFileBlocks(ctx, "pid-nil-coord", priorOffsets, newBlocks); err != nil {
		t.Fatalf("reapSupersededFileBlocks with nil coordinator: %v", err)
	}
}

// TestReapSupersededFileBlocks_RegionFilter exercises the general (non-empty
// newBlocks) reap path: only prior offsets that fall inside the rewritten
// region [regionStart, regionEnd) and are NOT reused by a new chunk are
// reaped. Prior offsets outside the region are kept, and reused offsets are
// kept (overwritten in place — current generation). It also covers a
// duplicate prior offset to confirm the dedup guard fires DecrementRefCount
// at most once per offset.
func TestReapSupersededFileBlocks_RegionFilter(t *testing.T) {
	ctx := context.Background()
	coord := newRefcountCoordinator()

	const payloadID = "pid-region"

	// Three distinct prior rows at offsets 0, 8192, 16384.
	var hKept, hReaped, hOutside block.ContentHash
	hKept[0] = 0x10    // offset 0 — reused by a new chunk -> kept
	hReaped[0] = 0x12  // offset 8192 — inside region, not reused -> reaped
	hOutside[0] = 0x13 // offset 16384 — outside the rewritten region -> kept

	coord.seedBlock(payloadID, 0, hKept, 1)
	coord.seedBlock(payloadID, 8192, hReaped, 1)
	coord.seedBlock(payloadID, 16384, hOutside, 1)

	bs := buildCascadeFixture(t, coord, nil)

	// New chunks: one reuses offset 0, two fresh chunks extend regionEnd past
	// offset 8192. With chunks at 0, 4096, 6144 (each size 4096) the rewritten
	// region is [0, 10240). Prior offset 0 is reused (kept); prior offset 8192
	// is inside [0, 10240) and not reused (reaped); prior offset 16384 is
	// outside (kept).
	var nh0, nh1 block.ContentHash
	nh0[0] = 0x20
	nh1[0] = 0x21
	newBlocks := []block.BlockRef{
		{Hash: hKept, Offset: 0, Size: 4096},  // reuses prior offset 0
		{Hash: nh0, Offset: 4096, Size: 4096}, // fresh
		{Hash: nh1, Offset: 6144, Size: 4096}, // fresh, extends regionEnd to 10240
	}

	// Duplicate the reaped offset in priorOffsets to exercise the dedup guard.
	priorOffsets := []uint64{0, 8192, 8192, 16384}

	if err := bs.reapSupersededFileBlocks(ctx, payloadID, priorOffsets, newBlocks); err != nil {
		t.Fatalf("reapSupersededFileBlocks: %v", err)
	}

	coord.mu.Lock()
	defer coord.mu.Unlock()
	if c := coord.counts[hKept]; c != 1 {
		t.Errorf("reused offset 0 hash refcount=%d; want 1 (must not be reaped)", c)
	}
	if c := coord.counts[hOutside]; c != 1 {
		t.Errorf("out-of-region offset 16384 hash refcount=%d; want 1 (must not be reaped)", c)
	}
	if _, present := coord.counts[hReaped]; present {
		t.Errorf("superseded offset 8192 hash still present; want reaped to 0 (deleted)")
	}
}
