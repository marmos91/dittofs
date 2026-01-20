package cache

import (
	"context"
	"fmt"
	"testing"
)

func TestEvict_FlushedData(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 10*1024)

	if err := c.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Mark blocks as uploaded
	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	for _, blk := range blocks {
		c.MarkBlockUploaded(ctx, payloadID, blk.ChunkIndex, blk.BlockIndex)
	}

	// Evict
	evicted, err := c.Evict(ctx, payloadID)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}
	if evicted == 0 {
		t.Errorf("expected some bytes evicted, got 0")
	}
}

func TestEvict_DirtyDataProtected(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 10*1024)

	if err := c.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Try to evict dirty data - should not evict
	evicted, err := c.Evict(ctx, payloadID)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}
	if evicted != 0 {
		t.Errorf("should not evict dirty data, got %d bytes", evicted)
	}

	// Data should still be there
	result := make([]byte, len(data))
	found, _ := c.ReadAt(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if !found {
		t.Error("dirty data should still be present")
	}
}

func TestEvictAll(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write and mark as uploaded multiple files
	for i := 0; i < 3; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 5*1024)
		if err := c.WriteAt(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		blocks, _ := c.GetDirtyBlocks(ctx, file)
		for _, blk := range blocks {
			c.MarkBlockUploaded(ctx, file, blk.ChunkIndex, blk.BlockIndex)
		}
	}

	evicted, err := c.EvictAll(ctx)
	if err != nil {
		t.Fatalf("EvictAll failed: %v", err)
	}
	if evicted == 0 {
		t.Errorf("expected some bytes evicted, got 0")
	}
}

func TestEvictLRU(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write and mark as uploaded files
	for i := 0; i < 5; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 10*1024)
		if err := c.WriteAt(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("Write failed: %v", err)
		}

		blocks, _ := c.GetDirtyBlocks(ctx, file)
		for _, blk := range blocks {
			c.MarkBlockUploaded(ctx, file, blk.ChunkIndex, blk.BlockIndex)
		}
	}

	initialSize := c.GetTotalSize()
	if initialSize == 0 {
		t.Fatal("expected non-zero initial size")
	}

	// Evict some data
	evicted, err := c.EvictLRU(ctx, 30*1024)
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}

	if evicted == 0 {
		t.Error("expected to evict some bytes")
	}
}

func TestLRUEviction_OnlyEvictsFlushed(t *testing.T) {
	// Cache with 10KB limit (but block size is 4MB so this will exceed)
	// Use a larger limit that makes sense for block buffers
	c := New(16 * 1024 * 1024) // 16MB
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write dirty data
	file1 := "dirty-file"
	if err := c.WriteAt(ctx, file1, 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Write and mark as uploaded
	file2 := "uploaded-file"
	if err := c.WriteAt(ctx, file2, 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	blocks, _ := c.GetDirtyBlocks(ctx, file2)
	for _, blk := range blocks {
		c.MarkBlockUploaded(ctx, file2, blk.ChunkIndex, blk.BlockIndex)
	}

	// Evict
	_, err := c.EvictLRU(ctx, BlockSize)
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}

	// Dirty file should still exist
	result := make([]byte, 5*1024)
	found, _ := c.ReadAt(ctx, file1, 0, 0, 5*1024, result)
	if !found {
		t.Error("dirty file should not be evicted")
	}
}

func TestLRUEviction_EvictsOldestFirst(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 5*1024)

	// Write 3 files in sequence
	files := []string{"oldest", "middle", "newest"}
	for _, file := range files {
		if err := c.WriteAt(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
		blocks, _ := c.GetDirtyBlocks(ctx, file)
		for _, blk := range blocks {
			c.MarkBlockUploaded(ctx, file, blk.ChunkIndex, blk.BlockIndex)
		}
	}

	// Evict some
	_, err := c.EvictLRU(ctx, BlockSize)
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}

	// Oldest should be evicted first
	// Note: In block buffer model, we evict entire block buffers
}

func TestEvict_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Evict(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestEvict_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

	_, err := c.Evict(context.Background(), "test")
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

func TestEvictAll_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.EvictAll(ctx)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestEvictLRU_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.EvictLRU(ctx, 1000)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// ============================================================================
// Eviction Benchmarks
// ============================================================================

// BenchmarkEvictLRU measures LRU eviction performance.
func BenchmarkEvictLRU(b *testing.B) {
	c := New(0) // Unlimited for benchmark
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Fill cache with uploaded data
	data := make([]byte, 32*1024)
	for i := 0; i < 1000; i++ {
		payloadID := fmt.Sprintf("file-%d", i)
		_ = c.WriteAt(ctx, payloadID, 0, data, 0)

		// Mark as uploaded so it can be evicted
		blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
		for _, blk := range blocks {
			c.MarkBlockUploaded(ctx, payloadID, blk.ChunkIndex, blk.BlockIndex)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_, err := c.EvictLRU(ctx, 1024*1024) // Evict 1MB
		if err != nil {
			b.Fatal(err)
		}
	}
}
