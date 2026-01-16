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

	if err := c.WriteSlice(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Mark as flushed
	slices, _ := c.GetDirtySlices(ctx, payloadID)
	for _, slice := range slices {
		if err := c.MarkSliceFlushed(ctx, payloadID, slice.ID, nil); err != nil {
			t.Fatalf("MarkSliceFlushed failed: %v", err)
		}
	}

	// Evict
	evicted, err := c.Evict(ctx, payloadID)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}
	if evicted != 10*1024 {
		t.Errorf("expected 10KB evicted, got %d", evicted)
	}

	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after evict, got %d", c.GetTotalSize())
	}
}

func TestEvict_DirtyDataProtected(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 10*1024)

	if err := c.WriteSlice(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
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
	found, _ := c.ReadSlice(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if !found {
		t.Error("dirty data should still be present")
	}
}

func TestEvictAll(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write and flush multiple files
	for i := 0; i < 3; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 5*1024)
		if err := c.WriteSlice(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}

		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			if err := c.MarkSliceFlushed(ctx, file, slice.ID, nil); err != nil {
				t.Fatalf("MarkSliceFlushed failed: %v", err)
			}
		}
	}

	evicted, err := c.EvictAll(ctx)
	if err != nil {
		t.Fatalf("EvictAll failed: %v", err)
	}
	if evicted != 15*1024 {
		t.Errorf("expected 15KB evicted, got %d", evicted)
	}

	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after EvictAll, got %d", c.GetTotalSize())
	}
}

func TestEvictLRU(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write and flush files
	for i := 0; i < 5; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 10*1024)
		if err := c.WriteSlice(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}

		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			if err := c.MarkSliceFlushed(ctx, file, slice.ID, nil); err != nil {
				t.Fatalf("MarkSliceFlushed failed: %v", err)
			}
		}
	}

	if c.GetTotalSize() != 50*1024 {
		t.Errorf("expected 50KB, got %d", c.GetTotalSize())
	}

	// Evict 30KB
	evicted, err := c.EvictLRU(ctx, 30*1024)
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}

	if evicted < 30*1024 {
		t.Errorf("expected to evict at least 30KB, got %d", evicted)
	}

	if c.GetTotalSize() > 20*1024 {
		t.Errorf("expected at most 20KB remaining, got %d", c.GetTotalSize())
	}
}

func TestLRUEviction_OnlyEvictsFlushed(t *testing.T) {
	// Cache with 10KB limit
	c := New(10 * 1024)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write 5KB dirty data
	file1 := "dirty-file"
	if err := c.WriteSlice(ctx, file1, 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Write 5KB and flush
	file2 := "flushed-file"
	if err := c.WriteSlice(ctx, file2, 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}
	slices, _ := c.GetDirtySlices(ctx, file2)
	for _, slice := range slices {
		if err := c.MarkSliceFlushed(ctx, file2, slice.ID, nil); err != nil {
			t.Fatalf("MarkSliceFlushed failed: %v", err)
		}
	}

	// Write 5KB more - triggers eviction
	file3 := "new-file"
	if err := c.WriteSlice(ctx, file3, 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Dirty file should still exist
	result := make([]byte, 5*1024)
	found, _ := c.ReadSlice(ctx, file1, 0, 0, 5*1024, result)
	if !found {
		t.Error("dirty file should not be evicted")
	}

	// Flushed data should be gone
	stats := c.Stats()
	if stats.FlushedBytes > 0 {
		t.Errorf("flushed data should be evicted, got %d bytes", stats.FlushedBytes)
	}
}

func TestLRUEviction_EvictsOldestFirst(t *testing.T) {
	c := New(15 * 1024)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	data := make([]byte, 5*1024)

	// Write 3 files in sequence
	files := []string{"oldest", "middle", "newest"}
	for _, file := range files {
		if err := c.WriteSlice(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}
		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			if err := c.MarkSliceFlushed(ctx, file, slice.ID, nil); err != nil {
				t.Fatalf("MarkSliceFlushed failed: %v", err)
			}
		}
	}

	// Write new file to trigger eviction
	if err := c.WriteSlice(ctx, "trigger", 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	stats := c.Stats()
	if stats.TotalSize > 15*1024 {
		t.Errorf("cache size %d exceeds max %d", stats.TotalSize, 15*1024)
	}
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
	c := New(100 * 1024 * 1024) // 100MB cache
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Fill cache with flushed data
	data := make([]byte, 32*1024)
	for i := 0; i < 1000; i++ {
		payloadID := fmt.Sprintf("file-%d", i)
		_ = c.WriteSlice(ctx, payloadID, 0, data, 0)

		// Mark as flushed so it can be evicted
		slices, _ := c.GetDirtySlices(ctx, payloadID)
		for _, s := range slices {
			_ = c.MarkSliceFlushed(ctx, payloadID, s.ID, nil)
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
