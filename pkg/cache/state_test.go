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
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := []byte("test data")

	_ = c.WriteAt(ctx, payloadID, 0, data, 0)

	if err := c.Remove(ctx, payloadID); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Should not find data
	result := make([]byte, len(data))
	found, _ := c.ReadAt(ctx, payloadID, 0, 0, uint32(len(data)), result)
	if found {
		t.Error("expected data to be removed")
	}
}

func TestRemove_Idempotent(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Remove nonexistent file - should not error
	if err := c.Remove(ctx, "nonexistent"); err != nil {
		t.Errorf("Remove nonexistent should be idempotent, got %v", err)
	}
}

func TestRemove_UpdatesTotalSize(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 1024)

	if err := c.WriteAt(ctx, payloadID, 0, data, 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	// TotalSize tracks memory allocation (BlockSize per block buffer), not bytes written
	if c.GetTotalSize() != BlockSize {
		t.Errorf("expected size %d (BlockSize), got %d", BlockSize, c.GetTotalSize())
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
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Remove(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRemove_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

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
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 10*1024)

	_ = c.WriteAt(ctx, payloadID, 0, data, 0)

	if err := c.Truncate(ctx, payloadID, 5*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Fatal("expected file to be found in cache after truncate")
	}
	if size != 5*1024 {
		t.Errorf("expected size 5120, got %d", size)
	}
}

func TestTruncate_ToZero(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 1024), 0)

	if err := c.Truncate(ctx, payloadID, 0); err != nil {
		t.Fatalf("Truncate to 0 failed: %v", err)
	}

	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Fatal("expected file to be found in cache after truncate to 0")
	}
	if size != 0 {
		t.Errorf("expected size 0, got %d", size)
	}
}

func TestTruncate_ExtendNoOp(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	data := make([]byte, 1024)

	_ = c.WriteAt(ctx, payloadID, 0, data, 0)

	// Try to extend - should be no-op
	if err := c.Truncate(ctx, payloadID, 2048); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Size should remain 1024 (truncate doesn't extend)
	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Fatal("expected file to be found in cache")
	}
	if size != 1024 {
		t.Errorf("expected size 1024, got %d", size)
	}
}

func TestTruncate_NonexistentFile(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	// Should not error for nonexistent file
	if err := c.Truncate(context.Background(), "nonexistent", 100); err != nil {
		t.Errorf("Truncate nonexistent should not error, got %v", err)
	}
}

func TestTruncate_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Truncate(ctx, "test", 100)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestTruncate_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

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
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	if c.HasDirtyData(ctx, "nonexistent") {
		t.Error("expected no dirty data for nonexistent file")
	}
}

func TestHasDirtyData_TrueAfterWrite(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.WriteAt(ctx, payloadID, 0, []byte("data"), 0)

	if !c.HasDirtyData(ctx, payloadID) {
		t.Error("expected dirty data after write")
	}
}

func TestHasDirtyData_FalseAfterUpload(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	if err := c.WriteAt(ctx, payloadID, 0, []byte("data"), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	for _, blk := range blocks {
		c.MarkBlockUploaded(ctx, payloadID, blk.ChunkIndex, blk.BlockIndex)
	}

	if c.HasDirtyData(ctx, payloadID) {
		t.Error("expected no dirty data after upload")
	}
}

// ============================================================================
// GetFileSize Tests
// ============================================================================

func TestGetFileSize_Basic(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 1024), 0)

	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Fatal("expected file to be found in cache")
	}
	if size != 1024 {
		t.Errorf("expected size 1024, got %d", size)
	}
}

func TestGetFileSize_NonexistentFile(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	size, found := c.GetFileSize(ctx, "nonexistent")
	if found {
		t.Error("expected file not to be found in cache")
	}
	if size != 0 {
		t.Errorf("expected size 0 for nonexistent, got %d", size)
	}
}

func TestGetFileSize_ZeroLengthFile(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write some data then truncate to 0
	_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 1024), 0)
	_ = c.Truncate(ctx, payloadID, 0)

	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Error("expected zero-length file to be found in cache")
	}
	if size != 0 {
		t.Errorf("expected size 0 for truncated file, got %d", size)
	}
}

func TestGetFileSize_MultipleChunks(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write to chunk 0 and chunk 1
	_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 1000), 0)
	_ = c.WriteAt(ctx, payloadID, 1, make([]byte, 500), 0)

	// Size should be: chunk_1_offset + 500 = ChunkSize + 500
	expected := uint64(ChunkSize) + 500
	size, found := c.GetFileSize(ctx, payloadID)
	if !found {
		t.Fatal("expected file to be found in cache")
	}
	if size != expected {
		t.Errorf("expected size %d, got %d", expected, size)
	}
}

// ============================================================================
// ListFiles Tests
// ============================================================================

func TestListFiles_Empty(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	files := c.ListFiles()
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestListFiles_MultipleFiles(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	_ = c.WriteAt(ctx, "file1", 0, []byte("data1"), 0)
	_ = c.WriteAt(ctx, "file2", 0, []byte("data2"), 0)
	_ = c.WriteAt(ctx, "file3", 0, []byte("data3"), 0)

	files := c.ListFiles()
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d", len(files))
	}
}

func TestListFiles_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteAt(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

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
	defer func() { _ = c.Close() }()

	sizes := c.ListFilesWithSizes()
	if len(sizes) != 0 {
		t.Errorf("expected empty map, got %d entries", len(sizes))
	}
}

func TestListFilesWithSizes_SingleFile(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// Write 32KB to file
	_ = c.WriteAt(ctx, "file1", 0, make([]byte, 32*1024), 0)

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
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// Write different sizes to different files
	_ = c.WriteAt(ctx, "small", 0, make([]byte, 1024), 0)
	_ = c.WriteAt(ctx, "medium", 0, make([]byte, 10*1024), 0)
	_ = c.WriteAt(ctx, "large", 0, make([]byte, 100*1024), 0)

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
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// Write at offset 0 and at offset 1MB (sparse file)
	_ = c.WriteAt(ctx, "sparse", 0, make([]byte, 1024), 0)
	_ = c.WriteAt(ctx, "sparse", 0, make([]byte, 1024), 1024*1024) // 1MB offset

	sizes := c.ListFilesWithSizes()
	// Size should be max offset + length = 1MB + 1KB
	expectedSize := uint64(1024*1024 + 1024)
	if sizes["sparse"] != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, sizes["sparse"])
	}
}

func TestListFilesWithSizes_MultipleChunks(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	// Write to chunk 0 and chunk 1 (each chunk is 64MB)
	// Offset parameter is offset within the chunk, not global offset
	_ = c.WriteAt(ctx, "multiChunk", 0, make([]byte, 1024), 0) // Chunk 0, offset 0
	_ = c.WriteAt(ctx, "multiChunk", 1, make([]byte, 1024), 0) // Chunk 1, offset 0 within chunk

	sizes := c.ListFilesWithSizes()
	// Size should be: chunk1_base + offset + length = 64MB + 0 + 1024
	expectedSize := uint64(ChunkSize + 1024)
	if sizes["multiChunk"] != expectedSize {
		t.Errorf("expected size %d, got %d", expectedSize, sizes["multiChunk"])
	}
}

func TestListFilesWithSizes_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteAt(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	sizes := c.ListFilesWithSizes()
	if len(sizes) != 0 {
		t.Errorf("expected empty map when closed, got %v", sizes)
	}
}

// ============================================================================
// Stats Tests
// ============================================================================

func TestStats_Basic(t *testing.T) {
	// Use unlimited cache size to avoid cache full errors
	// (each write creates a 4MB block buffer)
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Write dirty data
	if err := c.WriteAt(ctx, "file1", 0, make([]byte, 10*1024), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Write and mark as uploaded
	if err := c.WriteAt(ctx, "file2", 0, make([]byte, 5*1024), 0); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	blocks, _ := c.GetDirtyBlocks(ctx, "file2")
	for _, blk := range blocks {
		c.MarkBlockUploaded(ctx, "file2", blk.ChunkIndex, blk.BlockIndex)
	}

	stats := c.Stats()

	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MaxSize != 0 {
		t.Errorf("expected maxSize 0 (unlimited), got %d", stats.MaxSize)
	}
	// DirtyBytes/UploadedBytes track actual data written (dataSize)
	if stats.DirtyBytes != 10*1024 {
		t.Errorf("expected 10KB dirty, got %d", stats.DirtyBytes)
	}
	if stats.UploadedBytes != 5*1024 {
		t.Errorf("expected 5KB uploaded, got %d", stats.UploadedBytes)
	}
	// TotalSize tracks memory allocation (2 blocks * BlockSize)
	if stats.TotalSize != 2*BlockSize {
		t.Errorf("expected %d (2 blocks), got %d", 2*BlockSize, stats.TotalSize)
	}
	// BlockCount should be 2 (one per file)
	if stats.BlockCount != 2 {
		t.Errorf("expected 2 blocks, got %d", stats.BlockCount)
	}
}

func TestStats_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteAt(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

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
	_ = c.WriteAt(ctx, "test", 0, make([]byte, 1024), 0)

	_ = c.Close()

	if c.GetTotalSize() != 0 {
		t.Errorf("expected 0 size after close, got %d", c.GetTotalSize())
	}
}

// ============================================================================
// Sync Tests
// ============================================================================

func TestSync_NoWal(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	// Sync without WAL should not error
	if err := c.Sync(); err != nil {
		t.Errorf("Sync without WAL should not error, got %v", err)
	}
}

func TestSync_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Close()

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
				// Each file has 3 chunks with varying writes
				payloadID := fmt.Sprintf("file-%d", i)
				_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 32*1024), 0)
				_ = c.WriteAt(ctx, payloadID, 0, make([]byte, 16*1024), 32*1024)
				_ = c.WriteAt(ctx, payloadID, 1, make([]byte, 64*1024), 0)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_ = c.ListFilesWithSizes()
			}

			b.StopTimer()
			_ = c.Close()
		})
	}
}

// BenchmarkGetFileSize measures single file size calculation.
func BenchmarkGetFileSize(b *testing.B) {
	writeCounts := []struct {
		name   string
		writes int
	}{
		{"1write", 1},
		{"10writes", 10},
		{"100writes", 100},
	}

	for _, size := range writeCounts {
		b.Run(size.name, func(b *testing.B) {
			c := New(0)
			ctx := context.Background()

			// Create a file with many writes (simulates sequential writes)
			for i := 0; i < size.writes; i++ {
				offset := uint32(i * 32 * 1024)
				_ = c.WriteAt(ctx, "test-file", uint32(offset/ChunkSize), make([]byte, 32*1024), offset%ChunkSize)
			}

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_, _ = c.GetFileSize(ctx, "test-file")
			}

			b.StopTimer()
			_ = c.Close()
		})
	}
}
