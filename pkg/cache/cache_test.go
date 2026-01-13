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
	fileHandle := []byte("test-file")
	data := []byte("hello world")

	// Write
	err := c.WriteSlice(ctx, fileHandle, 0, data, 0)
	if err != nil {
		t.Fatalf("WriteSlice failed: %v", err)
	}

	// Read
	result, found, err := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)))
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
	fileHandle := []byte("test-file")

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
	fileHandle := []byte("test-file")
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
	_, found, err := c.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)))
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
	fileHandle := []byte("test-file")
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
	fileHandle := []byte("test-file")
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
	result, found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)))
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
	fileHandle := []byte("test-file")
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
	_, found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(data)))
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
	fileHandle := []byte("test-file")
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
		fileHandle := []byte("file-" + string(rune('0'+i)))
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
		fileHandle := []byte("file-" + string(rune('0'+i)))
		expected := "data for file " + string(rune('0'+i))
		result, found, err := c2.ReadSlice(ctx, fileHandle, 0, 0, uint32(len(expected)))
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
	fileHandle := []byte("benchmark-file")
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
	fileHandle := []byte("benchmark-file")
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
