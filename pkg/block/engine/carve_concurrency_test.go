package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
)

// concCountingRemote wraps the memory block store and records the peak number
// of PutBlock calls in flight at once, so a test can assert the carver actually
// uploads blocks concurrently (#1432 — the serial carver capped this at 1).
type concCountingRemote struct {
	*remotememory.Store
	mu   sync.Mutex
	cur  int
	peak int
}

func (c *concCountingRemote) PutBlock(ctx context.Context, blockID string, r io.Reader) error {
	c.mu.Lock()
	c.cur++
	if c.cur > c.peak {
		c.peak = c.cur
	}
	c.mu.Unlock()
	// Hold the slot briefly so genuinely-concurrent PutBlocks overlap in time.
	time.Sleep(40 * time.Millisecond)
	c.mu.Lock()
	c.cur--
	c.mu.Unlock()
	return c.Store.PutBlock(ctx, blockID, r)
}

func (c *concCountingRemote) peakInFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// feedBlocks stores n distinct chunks each at least carveBytes, so each chunk
// carves into its own block and carveFlush has n independent blocks to upload.
func feedBlocks(t *testing.T, ctx context.Context, f *carveFixture, n int, carveBytes int64) {
	t.Helper()
	for i := 0; i < n; i++ {
		data := bytes.Repeat([]byte(fmt.Sprintf("blk%03d-", i)), int(carveBytes))
		f.storeChunk(t, ctx, data)
	}
}

// TestCarve_UploadsConcurrently proves the carve path PUTs blocks in parallel
// (the #1432 fix): with the default adaptive window the peak in-flight PutBlock
// count exceeds 1, and a pinned window of 1 serializes it back to 1.
func TestCarve_UploadsConcurrently(t *testing.T) {
	const carveBytes = 64
	const nBlocks = 8

	t.Run("adaptive window uploads concurrently", func(t *testing.T) {
		ctx := context.Background()
		rem := &concCountingRemote{Store: remotememory.New()}
		f := newCarveFixture(t, rem, carveBytes)
		feedBlocks(t, ctx, f, nBlocks, carveBytes)

		if err := f.syncer.carveFlush(ctx, true); err != nil {
			t.Fatalf("carveFlush: %v", err)
		}
		if got := rem.peakInFlight(); got < 2 {
			t.Fatalf("peak in-flight PutBlock = %d, want >= 2 (carver must upload concurrently)", got)
		}
		if got := countRemoteBlocks(t, ctx, rem.Store); got != nBlocks {
			t.Fatalf("remote blocks = %d, want %d", got, nBlocks)
		}
	})

	t.Run("pinned window of 1 serializes", func(t *testing.T) {
		ctx := context.Background()
		rem := &concCountingRemote{Store: remotememory.New()}
		f := newCarveFixture(t, rem, carveBytes)
		// Pin the upload window to 1: carveFlush must never run two PutBlocks at once.
		f.syncer.uploadLimiter = newDynamicSemaphore(1)
		feedBlocks(t, ctx, f, nBlocks, carveBytes)

		if err := f.syncer.carveFlush(ctx, true); err != nil {
			t.Fatalf("carveFlush: %v", err)
		}
		if got := rem.peakInFlight(); got != 1 {
			t.Fatalf("peak in-flight PutBlock = %d, want 1 (pinned window must serialize)", got)
		}
		if got := countRemoteBlocks(t, ctx, rem.Store); got != nBlocks {
			t.Fatalf("remote blocks = %d, want %d", got, nBlocks)
		}
	})
}
