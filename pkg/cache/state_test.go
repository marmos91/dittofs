package cache

import (
	"context"
	"testing"
)

// ============================================================================
// Remove Tests
// ============================================================================

func TestRemove_ExistingFile(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := []byte("test data")

	c.WriteSlice(ctx, fileHandle, 0, data, 0)

	if err := c.Remove(ctx, fileHandle); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	// Should not find data
	result := make([]byte, len(data))
	found, _ := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)), result)
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
	fileHandle := "test-file"
	data := make([]byte, 1024)

	c.WriteSlice(ctx, fileHandle, 0, data, 0)
	if c.GetTotalSize() != 1024 {
		t.Errorf("expected size 1024, got %d", c.GetTotalSize())
	}

	c.Remove(ctx, fileHandle)
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
	fileHandle := "test-file"
	data := make([]byte, 10*1024)

	c.WriteSlice(ctx, fileHandle, 0, data, 0)

	if err := c.Truncate(ctx, fileHandle, 5*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	size := c.GetFileSize(fileHandle)
	if size != 5*1024 {
		t.Errorf("expected size 5120, got %d", size)
	}
}

func TestTruncate_ToZero(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	c.WriteSlice(ctx, fileHandle, 0, make([]byte, 1024), 0)

	if err := c.Truncate(ctx, fileHandle, 0); err != nil {
		t.Fatalf("Truncate to 0 failed: %v", err)
	}

	size := c.GetFileSize(fileHandle)
	if size != 0 {
		t.Errorf("expected size 0, got %d", size)
	}
}

func TestTruncate_ExtendNoOp(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"
	data := make([]byte, 1024)

	c.WriteSlice(ctx, fileHandle, 0, data, 0)

	// Try to extend - should be no-op
	if err := c.Truncate(ctx, fileHandle, 2048); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Size should remain 1024 (truncate doesn't extend)
	size := c.GetFileSize(fileHandle)
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
	fileHandle := "test-file"

	c.WriteSlice(ctx, fileHandle, 0, []byte("data"), 0)

	if !c.HasDirtyData(fileHandle) {
		t.Error("expected dirty data after write")
	}
}

func TestHasDirtyData_FalseAfterFlush(t *testing.T) {
	c := New(0)
	defer c.Close()

	ctx := context.Background()
	fileHandle := "test-file"

	c.WriteSlice(ctx, fileHandle, 0, []byte("data"), 0)

	slices, _ := c.GetDirtySlices(ctx, fileHandle)
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, fileHandle, slice.ID, nil)
	}

	if c.HasDirtyData(fileHandle) {
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
	fileHandle := "test-file"

	c.WriteSlice(ctx, fileHandle, 0, make([]byte, 1024), 0)

	if size := c.GetFileSize(fileHandle); size != 1024 {
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
	fileHandle := "test-file"

	// Write to chunk 0 and chunk 1
	c.WriteSlice(ctx, fileHandle, 0, make([]byte, 1000), 0)
	c.WriteSlice(ctx, fileHandle, 1, make([]byte, 500), 0)

	// Size should be: chunk_1_offset + 500 = ChunkSize + 500
	expected := uint64(ChunkSize) + 500
	if size := c.GetFileSize(fileHandle); size != expected {
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
// Stats Tests
// ============================================================================

func TestStats_Basic(t *testing.T) {
	c := New(100 * 1024)
	defer c.Close()

	ctx := context.Background()

	// Write dirty data
	c.WriteSlice(ctx, "file1", 0, make([]byte, 10*1024), 0)

	// Write and flush
	c.WriteSlice(ctx, "file2", 0, make([]byte, 5*1024), 0)
	slices, _ := c.GetDirtySlices(ctx, "file2")
	for _, slice := range slices {
		c.MarkSliceFlushed(ctx, "file2", slice.ID, nil)
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
