package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================================
// Basic Cache Tests
// ============================================================================

func TestCache_WriteAndRead(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("hello world")

	// Write
	err := c.WriteSlice(ctx, fileHandle, 0, data, 0)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Read
	result := make([]byte, len(data))
	found, err := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find data")
	}
	if string(result) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", result, data)
	}
}

func TestCache_SequentialWriteOptimization(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	// Write sequential chunks
	for i := 0; i < 10; i++ {
		data := make([]byte, 1024)
		offset := uint32(i * 1024)
		if err := c.WriteSlice(ctx, fileHandle, 0, data, offset); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}
	}

	// Get dirty slices - should be coalesced into 1
	slices, err := c.GetDirtySlices(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice, got %d", len(slices))
	}
	if slices[0].Length != 10*1024 {
		t.Errorf("expected length 10240, got %d", slices[0].Length)
	}
}

func TestCache_Remove(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("test data")

	// Write
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Remove
	if err := c.Remove(ctx, fileHandle); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Read should return not found
	result := make([]byte, len(data))
	found, err := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if found {
		t.Error("expected data to be removed")
	}
}

func TestCache_Truncate(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := make([]byte, 10*1024) // 10KB

	// Write
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Truncate to 5KB
	if err := c.Truncate(ctx, fileHandle, 5*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Check size
	size := c.GetFileSize(fileHandle)
	if size != 5*1024 {
		t.Errorf("expected size 5120, got %d", size)
	}
}

// ============================================================================
// mmap Tests
// ============================================================================

func TestCache_MmapPersistence(t *testing.T) {
	dir := t.TempDir()

	// Create cache with mmap
	c, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap failed: %v", err)
	}

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("persistent data")

	// Write
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Verify mmap file exists
	mmapFile := filepath.Join(dir, "cache.dat")
	if _, err := os.Stat(mmapFile); os.IsNotExist(err) {
		t.Fatal("mmap file should exist")
	}

	// Close
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen
	c2, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap (reopen) failed: %v", err)
	}
	defer c2.Close()

	// Read - should recover the data
	result := make([]byte, len(data))
	found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if !found {
		t.Fatal("expected to find recovered data")
	}
	if string(result) != string(data) {
		t.Errorf("data mismatch: got %q, want %q", result, data)
	}
}

func TestCache_MmapRemovePersistence(t *testing.T) {
	dir := t.TempDir()

	// Create cache with mmap
	c, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap failed: %v", err)
	}

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("to be removed")

	// Write
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Remove
	if err := c.Remove(ctx, fileHandle); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Close
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen
	c2, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap (reopen) failed: %v", err)
	}
	defer c2.Close()

	// Read - should not find data
	result := make([]byte, len(data))
	found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
	if err != nil {
		t.Fatalf("ReadSlice failed: %v", err)
	}
	if found {
		t.Error("expected data to be removed after recovery")
	}
}

func TestCache_MmapSync(t *testing.T) {
	dir := t.TempDir()

	c, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("sync test")

	// Write
	if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Sync
	if err := c.Sync(); err != nil {
		t.Fatalf("Sync failed: %v", err)
	}

	// No assertion here, just verify it doesn't panic
}

func TestCache_MmapMultipleFiles(t *testing.T) {
	dir := t.TempDir()

	c, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap failed: %v", err)
	}

	ctx := context.Background()

	// Write to multiple files
	for i := 0; i < 5; i++ {
		fileHandle := "file-" + string(rune('0'+i))
		data := []byte("data for file " + string(rune('0'+i)))
		if err := c.WriteSlice(ctx, fileHandle, 0, data, 0); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}
	}

	// Close
	if err := c.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen
	c2, err := NewWithMmap(dir, 0)
	if err != nil {
		t.Fatalf("NewWithMmap (reopen) failed: %v", err)
	}
	defer c2.Close()

	// Verify all files recovered
	files := c2.ListFiles()
	if len(files) != 5 {
		t.Errorf("expected 5 files, got %d", len(files))
	}

	// Verify data
	for i := 0; i < 5; i++ {
		fileHandle := "file-" + string(rune('0'+i))
		expected := "data for file " + string(rune('0'+i))
		result := make([]byte, len(expected))
		found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(expected)), result)
		if err != nil {
			t.Fatalf("ReadSlice failed: %v", err)
		}
		if !found {
			t.Errorf("file-%d: expected to find data", i)
			continue
		}
		if string(result) != expected {
			t.Errorf("file-%d: data mismatch: got %q, want %q", i, result, expected)
		}
	}
}

// ============================================================================
// Benchmark comparison: In-memory vs mmap
// ============================================================================

func BenchmarkCache_InMemory_Write(b *testing.B) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "benchmark-file"
	data := make([]byte, 32*1024)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCache_Mmap_Write(b *testing.B) {
	dir := b.TempDir()
	c, err := NewWithMmap(dir, 0)
	if err != nil {
		b.Fatalf("NewWithMmap failed: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	fileHandle := "benchmark-file"
	data := make([]byte, 32*1024)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		offset := uint32(i * len(data))
		chunkIdx := offset / ChunkSize
		offsetInChunk := offset % ChunkSize
		if err := c.WriteSlice(ctx, fileHandle, chunkIdx, data, offsetInChunk); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================================
// LRU Eviction Tests
// ============================================================================

func TestCache_LRUEviction_OnlyEvictsFlushed(t *testing.T) {
	// Create cache with 10KB max size
	c := New(10 * 1024)
	defer c.Close()

	ctx := context.Background()

	// Write 5KB to file1 (pending/dirty)
	file1 := "file1"
	data1 := make([]byte, 5*1024)
	if err := c.WriteSlice(ctx, file1, 0, data1, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Write 5KB to file2 (will be flushed)
	file2 := "file2"
	data2 := make([]byte, 5*1024)
	if err := c.WriteSlice(ctx, file2, 0, data2, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Mark file2's slices as flushed
	slices, _ := c.GetDirtySlices(ctx, file2)
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, file2, slice.ID, nil)
	}

	// Try to write 5KB more - should trigger eviction
	file3 := "file3"
	data3 := make([]byte, 5*1024)
	if err := c.WriteSlice(ctx, file3, 0, data3, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// file1 should still have data (dirty, protected)
	result1 := make([]byte, len(data1))
	found1, _ := c.ReadSlice(ctx, file1, 0, 0, uint32(len(data1)), result1)
	if !found1 {
		t.Error("file1 (dirty) should not be evicted")
	}

	// file2's flushed data should be evicted
	stats := c.Stats()
	if stats.FlushedBytes > 0 {
		t.Errorf("flushed data should be evicted, got %d bytes", stats.FlushedBytes)
	}

	// file3 should have data
	result3 := make([]byte, len(data3))
	found3, _ := c.ReadSlice(ctx, file3, 0, 0, uint32(len(data3)), result3)
	if !found3 {
		t.Error("file3 should have data")
	}
}

func TestCache_LRUEviction_EvictsOldestFirst(t *testing.T) {
	// Create cache with 15KB max size
	c := New(15 * 1024)
	defer c.Close()

	ctx := context.Background()

	// Write to 3 files in sequence (oldest to newest)
	files := []string{"old", "mid", "new"}
	data := make([]byte, 5*1024)

	for _, file := range files {
		if err := c.WriteSlice(ctx, file, 0, data, 0); err != nil {
			t.Fatalf("WriteSlice failed: %v", err)
		}
		// Mark as flushed immediately
		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			c.MarkSliceFlushed(ctx, file, slice.ID, nil)
		}
	}

	// Access "mid" to make it more recent
	resultMid := make([]byte, len(data))
	c.ReadSlice(ctx, files[1], 0, 0, uint32(len(data)), resultMid)

	// Note: Read doesn't update lastAccess in current impl (would need lock upgrade)
	// So LRU is based on write time, not access time

	// Write a new file to trigger eviction
	newFile := "newest"
	if err := c.WriteSlice(ctx, newFile, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// "old" should be evicted first (oldest write time)
	stats := c.Stats()
	// With 15KB limit and 20KB written (4 files * 5KB), some eviction should happen
	if stats.TotalSize > 15*1024 {
		t.Errorf("cache size %d exceeds max %d", stats.TotalSize, 15*1024)
	}
}

func TestCache_Stats(t *testing.T) {
	c := New(100 * 1024)
	defer c.Close()

	ctx := context.Background()

	// Write some data
	file1 := "file1"
	data1 := make([]byte, 10*1024)
	c.WriteSlice(ctx, file1, 0, data1, 0)

	file2 := "file2"
	data2 := make([]byte, 5*1024)
	c.WriteSlice(ctx, file2, 0, data2, 0)

	// Mark file2 as flushed
	slices, _ := c.GetDirtySlices(ctx, file2)
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, file2, slice.ID, nil)
	}

	stats := c.Stats()

	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MaxSize != 100*1024 {
		t.Errorf("expected maxSize 102400, got %d", stats.MaxSize)
	}
	if stats.DirtyBytes != 10*1024 {
		t.Errorf("expected 10KB dirty, got %d", stats.DirtyBytes)
	}
	if stats.FlushedBytes != 5*1024 {
		t.Errorf("expected 5KB flushed, got %d", stats.FlushedBytes)
	}
	if stats.TotalSize != 15*1024 {
		t.Errorf("expected 15KB total, got %d", stats.TotalSize)
	}
}

func TestCache_EvictLRU(t *testing.T) {
	c := New(0) // unlimited
	defer c.Close()

	ctx := context.Background()

	// Write and flush some files
	for i := 0; i < 5; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 10*1024)
		c.WriteSlice(ctx, file, 0, data, 0)

		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			c.MarkSliceFlushed(ctx, file, slice.ID, nil)
		}
	}

	initialSize := c.GetTotalSize()
	if initialSize != 50*1024 {
		t.Errorf("expected 50KB, got %d", initialSize)
	}

	// Manually trigger LRU eviction
	evicted, err := c.EvictLRU(ctx, 30*1024) // Try to free 30KB
	if err != nil {
		t.Fatalf("EvictLRU failed: %v", err)
	}

	if evicted < 30*1024 {
		t.Errorf("expected to evict at least 30KB, evicted %d", evicted)
	}

	finalSize := c.GetTotalSize()
	if finalSize > 20*1024 {
		t.Errorf("expected at most 20KB remaining, got %d", finalSize)
	}
}

// ============================================================================
// Additional Coverage Tests
// ============================================================================

func TestCache_ChunkRange(t *testing.T) {
	tests := []struct {
		offset, length       uint64
		wantStart, wantEnd   uint32
	}{
		{0, 0, 0, 0},                     // Zero length
		{0, 1024, 0, 0},                  // Within first chunk
		{0, ChunkSize, 0, 0},             // Exactly one chunk
		{0, ChunkSize + 1, 0, 1},         // Spans two chunks
		{ChunkSize - 1, 2, 0, 1},         // Cross chunk boundary
		{ChunkSize * 2, ChunkSize, 2, 2}, // Third chunk only
	}

	for _, tt := range tests {
		start, end := ChunkRange(tt.offset, tt.length)
		if start != tt.wantStart || end != tt.wantEnd {
			t.Errorf("ChunkRange(%d, %d) = (%d, %d), want (%d, %d)",
				tt.offset, tt.length, start, end, tt.wantStart, tt.wantEnd)
		}
	}
}

func TestCache_Evict(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := make([]byte, 10*1024)

	// Write
	c.WriteSlice(ctx, fileHandle, 0, data, 0)

	// Mark as flushed
	slices, _ := c.GetDirtySlices(ctx, fileHandle)
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, fileHandle, slice.ID, nil)
	}

	// Evict
	evicted, err := c.Evict(ctx, fileHandle)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}
	if evicted != 10*1024 {
		t.Errorf("expected 10KB evicted, got %d", evicted)
	}

	// Verify size reduced
	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after evict, got %d", c.GetTotalSize())
	}
}

func TestCache_EvictAll(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()

	// Write to multiple files and flush them
	for i := 0; i < 3; i++ {
		file := "file" + string(rune('0'+i))
		data := make([]byte, 5*1024)
		c.WriteSlice(ctx, file, 0, data, 0)

		slices, _ := c.GetDirtySlices(ctx, file)
		for _, slice := range slices {
			c.MarkSliceFlushed(ctx, file, slice.ID, nil)
		}
	}

	// Evict all
	evicted, err := c.EvictAll(ctx)
	if err != nil {
		t.Fatalf("EvictAll failed: %v", err)
	}
	if evicted != 15*1024 {
		t.Errorf("expected 15KB evicted, got %d", evicted)
	}

	// Verify size is 0
	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after EvictAll, got %d", c.GetTotalSize())
	}
}

func TestCache_HasDirtyData(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("test data")

	// Initially no dirty data
	if c.HasDirtyData(fileHandle) {
		t.Error("expected no dirty data initially")
	}

	// Write creates dirty data
	c.WriteSlice(ctx, fileHandle, 0, data, 0)
	if !c.HasDirtyData(fileHandle) {
		t.Error("expected dirty data after write")
	}

	// Flush clears dirty flag
	slices, _ := c.GetDirtySlices(ctx, fileHandle)
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, fileHandle, slice.ID, nil)
	}
	if c.HasDirtyData(fileHandle) {
		t.Error("expected no dirty data after flush")
	}
}

func TestCache_CoalesceWrites_NonAdjacent(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	// Write non-adjacent slices (with gap)
	c.WriteSlice(ctx, fileHandle, 0, []byte("AAA"), 0)
	c.WriteSlice(ctx, fileHandle, 0, []byte("BBB"), 100) // Gap at 3-99

	// Coalesce should create 2 slices (not merge due to gap)
	slices, err := c.GetDirtySlices(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}
	if len(slices) != 2 {
		t.Errorf("expected 2 non-adjacent slices, got %d", len(slices))
	}
}

func TestCache_CoalesceWrites_Overlapping(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	// Write overlapping slices
	c.WriteSlice(ctx, fileHandle, 0, make([]byte, 100), 0)   // 0-99
	c.WriteSlice(ctx, fileHandle, 0, make([]byte, 100), 50)  // 50-149 (overlaps)

	// Coalesce should merge into 1 slice
	slices, err := c.GetDirtySlices(ctx, fileHandle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice, got %d", len(slices))
	}
	if slices[0].Length != 150 {
		t.Errorf("expected length 150, got %d", slices[0].Length)
	}
}

func TestCache_PrependWrite(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	// Write at offset 100 first
	data1 := []byte("WORLD")
	c.WriteSlice(ctx, fileHandle, 0, data1, 100)

	// Prepend at offset 95 (ends where previous starts)
	data2 := []byte("HELLO")
	c.WriteSlice(ctx, fileHandle, 0, data2, 95)

	// Should be coalesced into one slice
	slices, _ := c.GetDirtySlices(ctx, fileHandle)
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice after prepend, got %d", len(slices))
	}
	if slices[0].Length != 10 {
		t.Errorf("expected length 10, got %d", slices[0].Length)
	}
}

func TestCache_EvictDoesNotRemoveDirty(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := make([]byte, 10*1024)

	// Write (creates dirty data)
	c.WriteSlice(ctx, fileHandle, 0, data, 0)

	// Try to evict (should not evict dirty data)
	evicted, err := c.Evict(ctx, fileHandle)
	if err != nil {
		t.Fatalf("Evict failed: %v", err)
	}
	if evicted != 0 {
		t.Errorf("should not evict dirty data, but evicted %d bytes", evicted)
	}

	// Data should still be there
	result := make([]byte, len(data))
	found, _ := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
	if !found {
		t.Error("dirty data should still be present after evict attempt")
	}
}
