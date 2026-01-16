package cache

import (
	"context"
	"fmt"
	"testing"
)

// ============================================================================
// Remove Tests
// ============================================================================

func TestRemove_ExistingFile(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("test data")

	c.WriteSlice(ctx, payloadID, 0, data, 0)

	if err := c.Remove(ctx, payloadID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Should not find data
	result := make([]byte, len(data))
	found, _ := c.ReadSlice(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if found {
		t.Error("expected data to be removed")
	}
}

func TestRemove_Idempotent(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()

	// Remove nonexistent file - should not error
	if err := c.Remove(ctx, "nonexistent"); err != nil {
		t.Errorf("Remove nonexistent should be idempotent, got %v", err)
	}
}

func TestRemove_UpdatesTotalSize(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 1024)

	if err := c.WriteSlice(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}
	if c.GetTotalSize() != 1024 {
		t.Errorf("expected size 1024, got %d", c.GetTotalSize())
	}

	if err := c.Remove(ctx, payloadID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	if c.GetTotalSize() != 0 {
		t.Errorf("expected size 0 after remove, got %d", c.GetTotalSize())
	}
}

func TestRemove_ContextCancelled(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Remove(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRemove_CacheClosed(t *testing.T) {
	c := New(0)
	c.Close()

	err := c.Remove(context.Background(), "test")
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// Truncate Tests
// ============================================================================

func TestTruncate_ReducesSize(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 10*1024)

	c.WriteSlice(ctx, payloadID, 0, data, 0)

	if err := c.Truncate(ctx, payloadID, 5*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	size := c.GetFileSize(payloadID)
	if size != 5*1024 {
		t.Errorf("expected size 5120, got %d", size)
	}
}

func TestTruncate_ToZero(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"

	c.WriteSlice(ctx, payloadID, 0, make([]byte, 1024), 0)

	if err := c.Truncate(ctx, payloadID, 0); err != nil {
		t.Fatalf("Truncate to 0 failed: %v", err)
	}

	size := c.GetFileSize(payloadID)
	if size != 0 {
		t.Errorf("expected size 0, got %d", size)
	}
}

func TestTruncate_ExtendNoOp(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 1024)

	c.WriteSlice(ctx, payloadID, 0, data, 0)

	// Try to extend - should be no-op
	if err := c.Truncate(ctx, payloadID, 2048); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Size should remain 1024 (truncate doesn't extend)
	size := c.GetFileSize(payloadID)
	if size != 1024 {
		t.Errorf("expected size 1024, got %d", size)
	}
}

func TestTruncate_NonexistentFile(t *testing.T) {
	c := New(0)
	defer c.Close()

	// Should not error for nonexistent file
	if err := c.Truncate(context.Background(), "nonexistent", 100); err != nil {
		t.Errorf("Truncate nonexistent should not error, got %v", err)
	}
}

func TestTruncate_ContextCancelled(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Truncate(ctx, "test", 100)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestTruncate_CacheClosed(t *testing.T) {
	c := New(0)
	c.Close()

	err := c.Truncate(context.Background(), "test", 100)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// HasDirtyData Tests
// ============================================================================

func TestHasDirtyData_InitiallyFalse(t *testing.T) {
	c := New(0)
	defer c.Close()

	if c.HasDirtyData("nonexistent") {
		t.Error("expected no dirty data for nonexistent file")
	}
}

func TestHasDirtyData_TrueAfterWrite(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"

	c.WriteSlice(ctx, payloadID, 0, []byte("data"), 0)

	if !c.HasDirtyData(payloadID) {
		t.Error("expected dirty data after write")
	}
}

func TestHasDirtyData_FalseAfterFlush(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"

	if err := c.WriteSlice(ctx, payloadID, 0, []byte("data"), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	slices, _ := c.GetDirtySlices(ctx, payloadID)
	for _, slice := range slices {
		if err := c.MarkSliceFlushed(ctx, payloadID, slice.ID, nil); err != nil {
			t.Fatalf("MarkSliceFlushed failed: %v", err)
		}
	}

	if c.HasDirtyData(payloadID) {
		t.Error("expected no dirty data after flush")
	}
}

// ============================================================================
// GetFileSize Tests
// ============================================================================

func TestGetFileSize_Basic(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"

	c.WriteSlice(ctx, payloadID, 0, make([]byte, 1024), 0)

	if size := c.GetFileSize(payloadID); size != 1024 {
		t.Errorf("expected size 1024, got %d", size)
	}
}

func TestGetFileSize_NonexistentFile(t *testing.T) {
	c := New(0)
	defer c.Close()

	if size := c.GetFileSize("nonexistent"); size != 0 {
		t.Errorf("expected size 0 for nonexistent, got %d", size)
	}
}

func TestGetFileSize_MultipleChunks(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	payloadID := "test-file"

	// Write to chunk 0 and chunk 1
	c.WriteSlice(ctx, payloadID, 0, make([]byte, 1000), 0)
	c.WriteSlice(ctx, payloadID, 1, make([]byte, 500), 0)

	// Size should be: chunk_1_offset + 500 = ChunkSize + 500
	expected := uint64(ChunkSize) + 500
	if size := c.GetFileSize(payloadID); size != expected {
		t.Errorf("expected size %d, got %d", expected, size)
	}
}

// ============================================================================
// ListFiles Tests
// ============================================================================

func TestListFiles_Empty(t *testing.T) {
	c := New(0)
	defer c.Close()

	files := c.ListFiles()
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestListFiles_MultipleFiles(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()

	c.WriteSlice(ctx, "file1", 0, []byte("data1"), 0)
	c.WriteSlice(ctx, "file2", 0, []byte("data2"), 0)
	c.WriteSlice(ctx, "file3", 0, []byte("data3"), 0)

	files := c.ListFiles()
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d", len(files))
	}
}

func TestListFiles_CacheClosed(t *testing.T) {
	c := New(0)
	c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	c.Close()

	files := c.ListFiles()
	if len(files) != 0 {
		t.Errorf("expected 0 files when closed, got %d", len(files))
	}
}

// ============================================================================
// ListFilesWithSizes Tests
// ============================================================================

func TestListFilesWithSizes_Empty(t *testing.T) {
	c := New(0)
	defer c.Close()

	sizes := c.ListFilesWithSizes()
	if len(sizes) != 0 {
		t.Errorf("expected empty map, got %d entries", len(sizes))
	}
}

func TestListFilesWithSizes_SingleFile(t *testing.T) {
	c := New(0)
	defer c.Close()
	ctx := context.Background()

	// Write 32KB to file
	c.WriteSlice(ctx, "file1", 0, make([]byte, 32*1024), 0)

	sizes := c.ListFilesWithSizes()
	if len(sizes) != 1 {
		t.Errorf("expected 1 file, got %d", len(sizes))
	}
	if sizes["file1"] != 32*1024 {
		t.Errorf("expected size 32768, got %d", sizes["file1"])
	}
}

func TestListFilesWithSizes_MultipleFiles(t *testing.T) {
	c := New(0)
	defer c.Close()
	ctx := context.Background()

	// Write different sizes to different files
	c.WriteSlice(ctx, "small", 0, make([]byte, 1024), 0)
	c.WriteSlice(ctx, "medium", 0, make([]byte, 10*1024), 0)
	c.WriteSlice(ctx, "large", 0, make([]byte, 100*1024), 0)

	sizes := c.ListFilesWithSizes()
	if len(sizes) != 3 {
		t.Errorf("expected 3 files, got %d", len(sizes))
	}
	if sizes["small"] != 1024 {
		t.Errorf("expected small=1024, got %d", sizes["small"])
	}
	if sizes["medium"] != 10*1024 {
		t.Errorf("expected medium=10240, got %d", sizes["medium"])
	}
	if sizes["large"] != 100*1024 {
		t.Errorf("expected large=102400, got %d", sizes["large"])
	}
}

func TestListFilesWithSizes_SparseFile(t *testing.T) {
	c := New(0)
	defer c.Close()
	ctx := context.Background()

	// Write at offset 0 and at offset 1MB (sparse file)
	c.WriteSlice(ctx, "sparse", 0, make([]byte, 1024), 0)
	c.WriteSlice(ctx, "sparse", 0, make([]byte, 1024), 1024*1024) // 1MB offset

	sizes := c.ListFilesWithSizes()
	// Size should be max offset + length = 1MB + 1KB
	expectedSize := uint64(1024*1024 + 1024)
	if sizes["sparse"] != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, sizes["sparse"])
	}
}

func TestListFilesWithSizes_MultipleChunks(t *testing.T) {
	c := New(0)
	defer c.Close()
	ctx := context.Background()

	// Write to chunk 0 and chunk 1 (each chunk is 64MB)
	// Offset parameter is offset within the chunk, not global offset
	c.WriteSlice(ctx, "multiChunk", 0, make([]byte, 1024), 0) // Chunk 0, offset 0
	c.WriteSlice(ctx, "multiChunk", 1, make([]byte, 1024), 0) // Chunk 1, offset 0 within chunk

	sizes := c.ListFilesWithSizes()
	// Size should be: chunk1_base + offset + length = 64MB + 0 + 1024
	expectedSize := uint64(ChunkSize + 1024)
	if sizes["multiChunk"] != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, sizes["multiChunk"])
	}
}

func TestListFilesWithSizes_CacheClosed(t *testing.T) {
	c := New(0)
	c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	c.Close()

	sizes := c.ListFilesWithSizes()
	if sizes != nil {
		t.Errorf("expected nil when closed, got %v", sizes)
	}
}

// ============================================================================
// Stats Tests
// ============================================================================

func TestStats_Basic(t *testing.T) {
	c := New(100 * 1024)
	defer c.Close()

	ctx := context.Background()

	// Write dirty data
	if err := c.WriteSlice(ctx, "file1", 0, make([]byte, 10*1024), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Write and flush
	if err := c.WriteSlice(ctx, "file2", 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}
	slices, _ := c.GetDirtySlices(ctx, "file2")
	for _, slice := range slices {
		if err := c.MarkSliceFlushed(ctx, "file2", slice.ID, nil); err != nil {
			t.Fatalf("MarkSliceFlushed failed: %v", err)
		}
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

func TestStats_CacheClosed(t *testing.T) {
	c := New(0)
	c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	c.Close()

	stats := c.Stats()
	if stats.FileCount != 0 || stats.TotalSize != 0 {
		t.Error("expected empty stats when closed")
	}
}

// ============================================================================
// Close Tests
// ============================================================================

func TestClose_Idempotent(t *testing.T) {
	c := New(0)

	if err := c.Close(); err != nil {
		t.Errorf("first Close failed: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Errorf("second Close should be idempotent, got %v", err)
	}
}

func TestClose_ReleasesResources(t *testing.T) {
	c := New(0)

	ctx := context.Background()
	c.WriteSlice(ctx, "test", 0, make([]byte, 1024), 0)

	c.Close()

	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after close, got %d", c.GetTotalSize())
	}
}

// ============================================================================
// Sync Tests
// ============================================================================

func TestSync_NoWal(t *testing.T) {
	c := New(0)
	defer c.Close()

	// Sync without WAL should not error
	if err := c.Sync(); err != nil {
		t.Errorf("Sync without WAL should not error, got %v", err)
	}
}

func TestSync_CacheClosed(t *testing.T) {
	c := New(0)
	c.Close()

	if err := c.Sync(); err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// Benchmarks for GC Recovery Support
// ============================================================================

// BenchmarkListFilesWithSizes measures the cost of getting file sizes for recovery.
// This is called during crash recovery to reconcile metadata.
func BenchmarkListFilesWithSizes(b *testing.B) {
	sizes := []struct {
		name      string
		fileCount int
	}{
		{"10files", 10},
		{"100files", 100},
		{"1000files", 1000},
	}

	for _, size := range sizes {
		b.Run(size.name, func(b *testing.B) {
			c := New(0)
			ctx := context.Background()

			// Create files with varying sizes
			for i := 0; i < size.fileCount; i++ {
				// Each file has 3 chunks with varying slices
				payloadID := fmt.Sprintf("file-%d", i)
				c.WriteSlice(ctx, payloadID, 0, make([]byte, 32*1024), 0)
				c.WriteSlice(ctx, payloadID, 0, make([]byte, 16*1024), 32*1024)
				c.WriteSlice(ctx, payloadID, 1, make([]byte, 64*1024), 0)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_ = c.ListFilesWithSizes()
			}

			b.StopTimer()
			c.Close()
		})
	}
}

// BenchmarkGetFileSize measures single file size calculation.
func BenchmarkGetFileSize(b *testing.B) {
	sliceCounts := []struct {
		name   string
		slices int
	}{
		{"1slice", 1},
		{"10slices", 10},
		{"100slices", 100},
	}

	for _, size := range sliceCounts {
		b.Run(size.name, func(b *testing.B) {
			c := New(0)
			ctx := context.Background()

			// Create a file with many slices (simulates sequential writes)
			for i := 0; i < size.slices; i++ {
				offset := uint32(i * 32 * 1024)
				c.WriteSlice(ctx, "test-file", uint32(offset/ChunkSize), make([]byte, 32*1024), offset%ChunkSize)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_ = c.GetFileSize("test-file")
			}

			b.StopTimer()
			c.Close()
		})
	}
}
