package flusher

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/cache/memory"
	contentMemory "github.com/marmos91/dittofs/pkg/store/content/memory"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestNew_DefaultConfig(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	if f == nil {
		t.Fatal("New() returned nil")
	}

	if f.sweepInterval != defaultSweepInterval {
		t.Errorf("sweepInterval = %v, want %v", f.sweepInterval, defaultSweepInterval)
	}
	if f.flushTimeout != defaultFlushTimeout {
		t.Errorf("flushTimeout = %v, want %v", f.flushTimeout, defaultFlushTimeout)
	}
}

func TestNew_CustomConfig(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	config := &Config{
		SweepInterval: 5 * time.Second,
		FlushTimeout:  60 * time.Second,
	}

	f := New(c, store, config)
	if f.sweepInterval != 5*time.Second {
		t.Errorf("sweepInterval = %v, want 5s", f.sweepInterval)
	}
	if f.flushTimeout != 60*time.Second {
		t.Errorf("flushTimeout = %v, want 60s", f.flushTimeout)
	}
}

func TestNew_PartialConfig(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	// Only set SweepInterval, FlushTimeout should use default
	config := &Config{
		SweepInterval: 5 * time.Second,
	}

	f := New(c, store, config)
	if f.sweepInterval != 5*time.Second {
		t.Errorf("sweepInterval = %v, want 5s", f.sweepInterval)
	}
	if f.flushTimeout != defaultFlushTimeout {
		t.Errorf("flushTimeout = %v, want default %v", f.flushTimeout, defaultFlushTimeout)
	}
}

func TestStartStop(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		SweepInterval: 100 * time.Millisecond,
		FlushTimeout:  50 * time.Millisecond,
	})

	ctx := context.Background()
	f.Start(ctx)

	// Give it time to run a few sweeps
	time.Sleep(250 * time.Millisecond)

	// Stop should not hang
	done := make(chan struct{})
	go func() {
		f.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() timed out")
	}
}

func TestStop_MultipleCallsSafe(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	f.Start(context.Background())

	// Multiple Stop calls should not panic
	f.Stop()
	f.Stop()
	f.Stop()
}

func TestShouldFlush_StateNone(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	contentID := metadata.ContentID("test-file")

	// Entry doesn't exist
	threshold := time.Now().Add(-f.flushTimeout)
	if f.shouldFlush(contentID, threshold) {
		t.Error("shouldFlush() should return false for non-existent entry")
	}
}

func TestShouldFlush_StateBuffering(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	contentID := metadata.ContentID("test-file")
	ctx := context.Background()

	// Create entry in StateBuffering
	_ = c.WriteAt(ctx, contentID, []byte("test data"), 0)
	// State should be StateBuffering by default

	threshold := time.Now().Add(-f.flushTimeout)
	if f.shouldFlush(contentID, threshold) {
		t.Error("shouldFlush() should return false for StateBuffering entry")
	}
}

func TestShouldFlush_StateUploading_Idle(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		FlushTimeout: 50 * time.Millisecond,
	})
	contentID := metadata.ContentID("test-file")
	ctx := context.Background()

	// Create entry and transition to StateUploading
	_ = c.WriteAt(ctx, contentID, []byte("test data"), 0)
	c.SetState(contentID, cache.StateUploading)
	c.SetFlushedOffset(contentID, 9) // All data flushed

	// Wait for idle timeout
	time.Sleep(100 * time.Millisecond)

	threshold := time.Now().Add(-f.flushTimeout)
	if !f.shouldFlush(contentID, threshold) {
		t.Error("shouldFlush() should return true for idle StateUploading entry")
	}
}

func TestShouldFlush_StateUploading_NotIdle(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		FlushTimeout: 1 * time.Hour, // Very long timeout
	})
	contentID := metadata.ContentID("test-file")
	ctx := context.Background()

	// Create entry and transition to StateUploading
	_ = c.WriteAt(ctx, contentID, []byte("test data"), 0)
	c.SetState(contentID, cache.StateUploading)
	c.SetFlushedOffset(contentID, 9) // All data flushed

	// Entry just written, not idle yet
	threshold := time.Now().Add(-f.flushTimeout)
	if f.shouldFlush(contentID, threshold) {
		t.Error("shouldFlush() should return false for recently-written entry")
	}
}

func TestShouldFlush_StateCached(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	contentID := metadata.ContentID("test-file")
	ctx := context.Background()

	// Create entry and transition to StateCached
	_ = c.WriteAt(ctx, contentID, []byte("test data"), 0)
	c.SetState(contentID, cache.StateCached)

	threshold := time.Now().Add(-f.flushTimeout)
	if f.shouldFlush(contentID, threshold) {
		t.Error("shouldFlush() should return false for StateCached entry")
	}
}

func TestFlush_NonIncrementalStore(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, nil)
	f.ctx = context.Background()

	contentID := metadata.ContentID("test-file")
	testData := []byte("test data for flush")
	ctx := context.Background()

	// Write data to cache
	_ = c.WriteAt(ctx, contentID, testData, 0)
	c.SetState(contentID, cache.StateUploading)
	c.SetFlushedOffset(contentID, uint64(len(testData))) // Mark as flushed

	// Flush should succeed
	err := f.flush(contentID)
	if err != nil {
		t.Fatalf("flush() error = %v", err)
	}

	// State should be StateCached after flush
	if c.GetState(contentID) != cache.StateCached {
		t.Errorf("State after flush = %v, want StateCached", c.GetState(contentID))
	}
}

func TestSweep_FlushesIdleEntries(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		SweepInterval: 100 * time.Millisecond,
		FlushTimeout:  50 * time.Millisecond,
	})
	f.ctx = context.Background()

	contentID := metadata.ContentID("test-file")
	testData := []byte("test data")
	ctx := context.Background()

	// Write data to cache and mark as uploading
	_ = c.WriteAt(ctx, contentID, testData, 0)
	c.SetState(contentID, cache.StateUploading)
	c.SetFlushedOffset(contentID, uint64(len(testData)))

	// Wait for idle timeout
	time.Sleep(100 * time.Millisecond)

	// Run sweep
	f.sweep()

	// Entry should be in StateCached now
	if c.GetState(contentID) != cache.StateCached {
		t.Errorf("State after sweep = %v, want StateCached", c.GetState(contentID))
	}
}

func TestSweep_SkipsActiveEntries(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		SweepInterval: 100 * time.Millisecond,
		FlushTimeout:  1 * time.Hour, // Very long timeout
	})
	f.ctx = context.Background()

	contentID := metadata.ContentID("test-file")
	testData := []byte("test data")
	ctx := context.Background()

	// Write data to cache and mark as uploading
	_ = c.WriteAt(ctx, contentID, testData, 0)
	c.SetState(contentID, cache.StateUploading)
	c.SetFlushedOffset(contentID, uint64(len(testData)))

	// Run sweep immediately (entry not idle yet)
	f.sweep()

	// Entry should still be in StateUploading
	if c.GetState(contentID) != cache.StateUploading {
		t.Errorf("State after sweep = %v, want StateUploading (not flushed)", c.GetState(contentID))
	}
}

func TestContextCancellation(t *testing.T) {
	c := memory.NewMemoryCache(100*1024*1024, nil)
	store, _ := contentMemory.NewMemoryContentStore(context.Background())

	f := New(c, store, &Config{
		SweepInterval: 50 * time.Millisecond,
		FlushTimeout:  10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	f.Start(ctx)

	// Cancel context
	cancel()

	// Wait for flusher to stop
	done := make(chan struct{})
	go func() {
		f.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Flusher did not stop after context cancellation")
	}
}
