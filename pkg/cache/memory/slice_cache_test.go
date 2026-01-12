package memory

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
)

// Helper to create a file handle for testing.
func testFileHandle(name string) []byte {
	return []byte("/test:" + name)
}

func TestMemorySliceCache_WriteAndRead(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write some data
	data := []byte("hello world")
	err := c.WriteSlice(ctx, handle, 0, data, 0)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Read it back
	result, found, err := c.ReadSlice(ctx, handle, 0, 0, uint32(len(data)))
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("ReadSlice: expected found=true")
	}
	if !bytes.Equal(result, data) {
		t.Errorf("ReadSlice: got %q, want %q", result, data)
	}
}

func TestMemorySliceCache_ReadNonExistent(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("nonexistent")

	// Read from non-existent file
	_, found, err := c.ReadSlice(ctx, handle, 0, 0, 100)
	if err != nil && err != cache.ErrFileNotInCache {
		t.Fatalf("ReadSlice: unexpected error: %v", err)
	}
	if found {
		t.Error("ReadSlice: expected found=false for non-existent file")
	}
}

func TestMemorySliceCache_NewestWins(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write initial data
	err := c.WriteSlice(ctx, handle, 0, []byte("AAAA"), 0)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Small delay to ensure different timestamps
	time.Sleep(time.Millisecond)

	// Overwrite part of it
	err = c.WriteSlice(ctx, handle, 0, []byte("BB"), 1)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Read merged result - newest wins
	result, found, err := c.ReadSlice(ctx, handle, 0, 0, 4)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("ReadSlice: expected found=true")
	}

	expected := []byte("ABBA") // A[BB]A - BB overwrites middle
	if !bytes.Equal(result, expected) {
		t.Errorf("ReadSlice: got %q, want %q", result, expected)
	}
}

func TestMemorySliceCache_SparseRead(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write at offset 10
	err := c.WriteSlice(ctx, handle, 0, []byte("hello"), 10)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Read from 0 to 20 - should have zeros before and after
	result, found, err := c.ReadSlice(ctx, handle, 0, 0, 20)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("ReadSlice: expected found=true")
	}

	// First 10 bytes should be zeros
	for i := 0; i < 10; i++ {
		if result[i] != 0 {
			t.Errorf("result[%d] = %d, want 0", i, result[i])
		}
	}

	// Bytes 10-14 should be "hello"
	if !bytes.Equal(result[10:15], []byte("hello")) {
		t.Errorf("result[10:15] = %q, want %q", result[10:15], "hello")
	}

	// Bytes 15-19 should be zeros
	for i := 15; i < 20; i++ {
		if result[i] != 0 {
			t.Errorf("result[%d] = %d, want 0", i, result[i])
		}
	}
}

func TestMemorySliceCache_MultipleChunks(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write to chunk 0
	err := c.WriteSlice(ctx, handle, 0, []byte("chunk0"), 0)
	if err != nil {
		t.Fatalf("WriteSlice chunk 0 failed: %v", err)
	}

	// Write to chunk 1
	err = c.WriteSlice(ctx, handle, 1, []byte("chunk1"), 0)
	if err != nil {
		t.Fatalf("WriteSlice chunk 1 failed: %v", err)
	}

	// Read chunk 0
	result0, found, _ := c.ReadSlice(ctx, handle, 0, 0, 6)
	if !found || !bytes.Equal(result0, []byte("chunk0")) {
		t.Errorf("Chunk 0: got %q, want %q", result0, "chunk0")
	}

	// Read chunk 1
	result1, found, _ := c.ReadSlice(ctx, handle, 1, 0, 6)
	if !found || !bytes.Equal(result1, []byte("chunk1")) {
		t.Errorf("Chunk 1: got %q, want %q", result1, "chunk1")
	}
}

func TestMemorySliceCache_GetDirtySlices(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write multiple slices
	_ = c.WriteSlice(ctx, handle, 0, []byte("AAA"), 0)
	_ = c.WriteSlice(ctx, handle, 0, []byte("BBB"), 10)
	_ = c.WriteSlice(ctx, handle, 1, []byte("CCC"), 0)

	// Get dirty slices
	dirty, err := c.GetDirtySlices(ctx, handle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}

	// Should have slices from both chunks
	if len(dirty) != 3 {
		t.Errorf("GetDirtySlices: got %d slices, want 3", len(dirty))
	}

	// Should be sorted by chunk index, then offset
	if len(dirty) >= 2 && dirty[0].ChunkIndex > dirty[len(dirty)-1].ChunkIndex {
		t.Error("GetDirtySlices: slices not sorted by chunk index")
	}
}

func TestMemorySliceCache_CoalesceWrites(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write adjacent slices
	_ = c.WriteSlice(ctx, handle, 0, []byte("AAA"), 0)   // 0-2
	_ = c.WriteSlice(ctx, handle, 0, []byte("BBB"), 3)   // 3-5
	_ = c.WriteSlice(ctx, handle, 0, []byte("CCC"), 6)   // 6-8
	_ = c.WriteSlice(ctx, handle, 0, []byte("DDD"), 100) // Non-adjacent

	// Coalesce
	err := c.CoalesceWrites(ctx, handle)
	if err != nil {
		t.Fatalf("CoalesceWrites failed: %v", err)
	}

	// Get dirty slices - should have merged AAA+BBB+CCC and kept DDD separate
	dirty, _ := c.GetDirtySlices(ctx, handle)

	// Should have 2 slices: one merged (0-8) and one separate (100-102)
	if len(dirty) != 2 {
		t.Errorf("After coalesce: got %d slices, want 2", len(dirty))
	}

	// Verify data integrity after coalesce
	result, _, _ := c.ReadSlice(ctx, handle, 0, 0, 9)
	expected := []byte("AAABBBCCC")
	if !bytes.Equal(result, expected) {
		t.Errorf("After coalesce: got %q, want %q", result, expected)
	}
}

func TestMemorySliceCache_MarkSliceFlushed(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write a slice
	_ = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)

	// Get the slice ID
	dirty, _ := c.GetDirtySlices(ctx, handle)
	if len(dirty) != 1 {
		t.Fatalf("Expected 1 dirty slice, got %d", len(dirty))
	}
	sliceID := dirty[0].ID

	// Mark as flushed
	blockRefs := []cache.BlockRef{{ID: "block-1", Size: 4}}
	err := c.MarkSliceFlushed(ctx, handle, sliceID, blockRefs)
	if err != nil {
		t.Fatalf("MarkSliceFlushed failed: %v", err)
	}

	// Should have no dirty data now
	if c.HasDirtyData(handle) {
		t.Error("Expected no dirty data after MarkSliceFlushed")
	}
}

func TestMemorySliceCache_Evict(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write and mark flushed
	_ = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)
	dirty, _ := c.GetDirtySlices(ctx, handle)
	_ = c.MarkSliceFlushed(ctx, handle, dirty[0].ID, nil)

	// Write pending data
	_ = c.WriteSlice(ctx, handle, 0, []byte("pending"), 10)

	// Evict - should only remove flushed
	evicted, err := c.Evict(ctx, handle)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}

	if evicted != 4 {
		t.Errorf("Evict: got %d bytes evicted, want 4", evicted)
	}

	// Pending data should still be there
	if !c.HasDirtyData(handle) {
		t.Error("Expected dirty data to remain after Evict")
	}
}

func TestMemorySliceCache_Remove(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write some data
	_ = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)

	// Remove completely
	err := c.Remove(ctx, handle)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// File should no longer exist
	_, found, err := c.ReadSlice(ctx, handle, 0, 0, 4)
	if found {
		t.Error("Expected file to be removed")
	}
	if err != nil && err != cache.ErrFileNotInCache {
		t.Errorf("Unexpected error: %v", err)
	}
}

func TestMemorySliceCache_GetFileSize(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Initially zero
	if size := c.GetFileSize(handle); size != 0 {
		t.Errorf("Initial size: got %d, want 0", size)
	}

	// Write at offset 100, length 50
	data := make([]byte, 50)
	_ = c.WriteSlice(ctx, handle, 0, data, 100)

	// Size should be 150 (0-149)
	if size := c.GetFileSize(handle); size != 150 {
		t.Errorf("After write: got %d, want 150", size)
	}
}

func TestMemorySliceCache_ListFiles(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()

	// Write to multiple files
	_ = c.WriteSlice(ctx, testFileHandle("file1"), 0, []byte("a"), 0)
	_ = c.WriteSlice(ctx, testFileHandle("file2"), 0, []byte("b"), 0)
	_ = c.WriteSlice(ctx, testFileHandle("file3"), 0, []byte("c"), 0)

	files := c.ListFiles()
	if len(files) != 3 {
		t.Errorf("ListFiles: got %d files, want 3", len(files))
	}
}

func TestMemorySliceCache_Stats(t *testing.T) {
	c := NewMemorySliceCache(1024*1024) // 1MB max
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write some data - adjacent writes get merged into 1 slice
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 100), 0)
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 200), 100) // Adjacent, merged

	// Write with a gap to create second slice
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 50), 500) // Gap, new slice

	stats := c.GetStats()

	if stats.FileCount != 1 {
		t.Errorf("Stats.FileCount: got %d, want 1", stats.FileCount)
	}
	if stats.ChunkCount != 1 {
		t.Errorf("Stats.ChunkCount: got %d, want 1", stats.ChunkCount)
	}
	// 2 slices: one merged (0-299) and one separate (500-549)
	if stats.PendingSlices != 2 {
		t.Errorf("Stats.PendingSlices: got %d, want 2", stats.PendingSlices)
	}
	if stats.TotalSize != 350 {
		t.Errorf("Stats.TotalSize: got %d, want 350", stats.TotalSize)
	}
	if stats.DirtySize != 350 {
		t.Errorf("Stats.DirtySize: got %d, want 350", stats.DirtySize)
	}
	if stats.MaxSize != 1024*1024 {
		t.Errorf("Stats.MaxSize: got %d, want %d", stats.MaxSize, 1024*1024)
	}
}

func TestMemorySliceCache_Close(t *testing.T) {
	c := NewMemorySliceCache(0)

	ctx := context.Background()
	handle := testFileHandle("file1")

	_ = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)

	// Close
	err := c.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// All operations should fail
	err = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)
	if err != cache.ErrSliceCacheClosed {
		t.Errorf("WriteSlice after close: got %v, want ErrSliceCacheClosed", err)
	}

	_, _, err = c.ReadSlice(ctx, handle, 0, 0, 4)
	if err != cache.ErrSliceCacheClosed {
		t.Errorf("ReadSlice after close: got %v, want ErrSliceCacheClosed", err)
	}
}

func TestMemorySliceCache_ContextCancellation(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	handle := testFileHandle("file1")

	// All operations should return context error
	err := c.WriteSlice(ctx, handle, 0, []byte("test"), 0)
	if err != context.Canceled {
		t.Errorf("WriteSlice with cancelled context: got %v, want context.Canceled", err)
	}

	_, _, err = c.ReadSlice(ctx, handle, 0, 0, 4)
	if err != context.Canceled {
		t.Errorf("ReadSlice with cancelled context: got %v, want context.Canceled", err)
	}
}

func TestMemorySliceCache_InvalidOffset(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Try to write past chunk boundary
	data := make([]byte, 100)
	err := c.WriteSlice(ctx, handle, 0, data, cache.ChunkSize-50)
	if err != cache.ErrInvalidOffset {
		t.Errorf("WriteSlice past chunk boundary: got %v, want ErrInvalidOffset", err)
	}
}

func TestMemorySliceCache_HitRate(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write some data
	_ = c.WriteSlice(ctx, handle, 0, []byte("test"), 0)

	// Read (hit)
	_, _, _ = c.ReadSlice(ctx, handle, 0, 0, 4)

	// Read non-existent (miss)
	_, _, _ = c.ReadSlice(ctx, testFileHandle("nonexistent"), 0, 0, 4)

	stats := c.GetStats()
	if stats.Hits != 1 {
		t.Errorf("Stats.Hits: got %d, want 1", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Errorf("Stats.Misses: got %d, want 1", stats.Misses)
	}

	hitRate := stats.HitRate()
	if hitRate != 0.5 {
		t.Errorf("HitRate: got %f, want 0.5", hitRate)
	}
}

func TestMemorySliceCache_SequentialWriteOptimization(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Simulate NFS-style sequential writes (32KB chunks for a 320KB file = 10 writes)
	chunkSize := 32 * 1024
	numWrites := 10
	totalSize := chunkSize * numWrites

	for i := range numWrites {
		data := make([]byte, chunkSize)
		// Fill with identifiable pattern
		for j := range data {
			data[j] = byte(i)
		}
		offset := uint32(i * chunkSize)
		err := c.WriteSlice(ctx, handle, 0, data, offset)
		if err != nil {
			t.Fatalf("WriteSlice %d failed: %v", i, err)
		}
	}

	// Key assertion: Should have only 1 slice, not 10!
	// Because sequential writes should extend the same slice
	dirty, err := c.GetDirtySlices(ctx, handle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}

	if len(dirty) != 1 {
		t.Errorf("Sequential writes created %d slices, want 1 (extension optimization failed)", len(dirty))
	}

	// Verify the slice has correct size
	if len(dirty) > 0 && dirty[0].Length != uint32(totalSize) {
		t.Errorf("Slice length: got %d, want %d", dirty[0].Length, totalSize)
	}

	// Verify data integrity
	result, found, err := c.ReadSlice(ctx, handle, 0, 0, uint32(totalSize))
	if err != nil || !found {
		t.Fatalf("ReadSlice failed: err=%v, found=%v", err, found)
	}

	// Check pattern for each chunk
	for i := range numWrites {
		start := i * chunkSize
		for j := range chunkSize {
			if result[start+j] != byte(i) {
				t.Errorf("Data mismatch at offset %d: got %d, want %d", start+j, result[start+j], i)
				break
			}
		}
	}
}

func TestMemorySliceCache_SequentialWriteWithGap(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write at offset 0
	_ = c.WriteSlice(ctx, handle, 0, []byte("AAA"), 0)

	// Write at offset 100 (gap of 97 bytes) - should create new slice
	_ = c.WriteSlice(ctx, handle, 0, []byte("BBB"), 100)

	// Write at offset 103 (adjacent to previous) - should extend
	_ = c.WriteSlice(ctx, handle, 0, []byte("CCC"), 103)

	dirty, _ := c.GetDirtySlices(ctx, handle)

	// Should have 2 slices: one at 0-2 and one at 100-105
	if len(dirty) != 2 {
		t.Errorf("Got %d slices, want 2 (gap should create separate slice)", len(dirty))
	}
}

func TestMemorySliceCache_PrependOptimization(t *testing.T) {
	c := NewMemorySliceCache(0)
	defer c.Close()

	ctx := context.Background()
	handle := testFileHandle("file1")

	// Write at offset 100 first
	_ = c.WriteSlice(ctx, handle, 0, []byte("BBB"), 100)

	// Write at offset 97 (adjacent, prepending) - should extend
	_ = c.WriteSlice(ctx, handle, 0, []byte("AAA"), 97)

	dirty, _ := c.GetDirtySlices(ctx, handle)

	// Should have 1 slice covering 97-102
	if len(dirty) != 1 {
		t.Errorf("Got %d slices, want 1 (prepend should extend)", len(dirty))
	}

	if len(dirty) > 0 {
		if dirty[0].Offset != 97 {
			t.Errorf("Slice offset: got %d, want 97", dirty[0].Offset)
		}
		if dirty[0].Length != 6 {
			t.Errorf("Slice length: got %d, want 6", dirty[0].Length)
		}
	}

	// Verify data
	result, _, _ := c.ReadSlice(ctx, handle, 0, 97, 6)
	expected := []byte("AAABBB")
	if !bytes.Equal(result, expected) {
		t.Errorf("Data: got %q, want %q", result, expected)
	}
}
