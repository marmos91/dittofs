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
