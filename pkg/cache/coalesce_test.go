package cache

import (
	"context"
	"testing"
	"time"
)

// ============================================================================
// GetBlockLastDirtied Tests
// ============================================================================

func TestGetBlockLastDirtied_NewBlock(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 1024)

	before := time.Now()
	if err := c.WriteAt(ctx, "file", 0, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	after := time.Now()

	lastDirtied := c.GetBlockLastDirtied(ctx, "file", 0, 0)
	if lastDirtied.Before(before) || lastDirtied.After(after) {
		t.Errorf("lastDirtied=%v, expected between %v and %v", lastDirtied, before, after)
	}
}

func TestGetBlockLastDirtied_NonexistentBlock(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	lastDirtied := c.GetBlockLastDirtied(ctx, "nonexistent", 0, 0)
	if !lastDirtied.IsZero() {
		t.Errorf("expected zero time for nonexistent block, got %v", lastDirtied)
	}
}

func TestGetBlockLastDirtied_ReDirty(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, BlockSize)

	// Write new block -> Pending
	if err := c.WriteAt(ctx, "file", 0, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	firstDirtied := c.GetBlockLastDirtied(ctx, "file", 0, 0)

	// Mark as uploaded
	c.MarkBlockUploaded(ctx, "file", 0, 0, 0)

	time.Sleep(5 * time.Millisecond) // ensure time difference

	// Re-dirty by writing again -> should update lastDirtied
	if err := c.WriteAt(ctx, "file", 0, data[:1024], 0); err != nil {
		t.Fatalf("WriteAt (re-dirty) failed: %v", err)
	}
	secondDirtied := c.GetBlockLastDirtied(ctx, "file", 0, 0)

	if !secondDirtied.After(firstDirtied) {
		t.Errorf("re-dirty should update lastDirtied: first=%v, second=%v", firstDirtied, secondDirtied)
	}
}

func TestGetBlockLastDirtied_UploadingToRevert(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, BlockSize)

	// Write -> Pending
	if err := c.WriteAt(ctx, "file", 0, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Mark as uploading
	c.MarkBlockUploading(ctx, "file", 0, 0)

	time.Sleep(5 * time.Millisecond)

	// Write again while uploading -> reverts to Pending, updates lastDirtied
	before := time.Now()
	if err := c.WriteAt(ctx, "file", 0, data[:1024], 0); err != nil {
		t.Fatalf("WriteAt (during upload) failed: %v", err)
	}

	lastDirtied := c.GetBlockLastDirtied(ctx, "file", 0, 0)
	if lastDirtied.Before(before) {
		t.Errorf("expected lastDirtied >= %v, got %v", before, lastDirtied)
	}
}

// ============================================================================
// Re-dirty Backpressure Tests
// ============================================================================

func TestReDirtyBackpressure_PendingSizeDoesNotOvershoot(t *testing.T) {
	// Use a small maxPendingSize to trigger backpressure quickly
	maxPending := uint64(3 * BlockSize) // 12MB limit
	c := New(0)
	c.SetMaxPendingSize(maxPending)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, BlockSize)

	// Write 3 blocks to reach maxPending
	for i := 0; i < 3; i++ {
		offset := uint32(i) * BlockSize
		if err := c.WriteAt(ctx, "file", 0, data, offset); err != nil {
			t.Fatalf("WriteAt block %d failed: %v", i, err)
		}
	}

	if c.pendingSize.Load() != maxPending {
		t.Fatalf("expected pendingSize=%d, got %d", maxPending, c.pendingSize.Load())
	}

	// Mark all as uploaded
	for i := 0; i < 3; i++ {
		c.MarkBlockUploaded(ctx, "file", 0, uint32(i), 0)
	}

	// Now re-dirty them in sequence. Without Fix 3, a burst of re-dirties
	// would all skip the pending check and overshoot maxPending.
	// With Fix 3, each re-dirty checks the limit.
	var maxObserved uint64
	for round := 0; round < 3; round++ {
		for i := 0; i < 3; i++ {
			offset := uint32(i) * BlockSize
			if err := c.WriteAt(ctx, "file", 0, data[:1024], offset); err != nil {
				t.Fatalf("re-dirty round %d block %d failed: %v", round, i, err)
			}
			pending := c.pendingSize.Load()
			if pending > maxObserved {
				maxObserved = pending
			}
		}
		// Upload all again
		for i := 0; i < 3; i++ {
			c.MarkBlockUploaded(ctx, "file", 0, uint32(i), 0)
		}
	}

	// maxObserved should not exceed maxPending + BlockSize (at most 1 block overshoot
	// due to atomic add before check)
	limit := maxPending + BlockSize
	if maxObserved > limit {
		t.Errorf("pendingSize overshot: max observed %d, limit %d", maxObserved, limit)
	}
}
