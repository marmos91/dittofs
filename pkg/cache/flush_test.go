package cache

import (
	"context"
	"fmt"
	"testing"
)

// ============================================================================
// Coverage Bitmap Helper Tests
// ============================================================================

func TestMarkCoverage(t *testing.T) {
	coverage := newCoverageBitmap()

	// Mark first 64 bytes (bit 0)
	markCoverage(coverage, 0, 64)
	if coverage[0]&1 == 0 {
		t.Error("bit 0 should be set")
	}

	// Mark bytes 128-192 (bit 2)
	markCoverage(coverage, 128, 64)
	if coverage[0]&(1<<2) == 0 {
		t.Error("bit 2 should be set")
	}

	// Mark a range spanning multiple bits (0-256 bytes = bits 0-3)
	coverage2 := newCoverageBitmap()
	markCoverage(coverage2, 0, 256)
	for bit := 0; bit < 4; bit++ {
		if coverage2[0]&(1<<bit) == 0 {
			t.Errorf("bit %d should be set", bit)
		}
	}
}

func TestMarkCoverage_Empty(t *testing.T) {
	coverage := newCoverageBitmap()

	// Zero length should not modify anything
	markCoverage(coverage, 0, 0)
	for _, word := range coverage {
		if word != 0 {
			t.Error("coverage should be empty for zero length")
		}
	}

	// Nil coverage should not panic
	markCoverage(nil, 0, 64)
}

func TestIsRangeCovered(t *testing.T) {
	coverage := newCoverageBitmap()

	// Empty coverage - nothing is covered
	if isRangeCovered(coverage, 0, 64) {
		t.Error("empty coverage should not cover any range")
	}

	// Mark first 64 bytes
	markCoverage(coverage, 0, 64)
	if !isRangeCovered(coverage, 0, 64) {
		t.Error("marked range should be covered")
	}

	// Check partial coverage
	if isRangeCovered(coverage, 0, 128) {
		t.Error("only partially marked range should not be fully covered")
	}

	// Zero length is always covered
	if !isRangeCovered(coverage, 0, 0) {
		t.Error("zero length should always be covered")
	}
}

func TestIsRangeCovered_Nil(t *testing.T) {
	if isRangeCovered(nil, 0, 64) {
		t.Error("nil coverage should not cover any range")
	}

	// Zero length is covered even with nil coverage
	if !isRangeCovered(nil, 0, 0) {
		t.Error("zero length should be covered even with nil coverage")
	}
}

func TestIsFullyCovered(t *testing.T) {
	// Empty coverage
	coverage := newCoverageBitmap()
	if isFullyCovered(coverage) {
		t.Error("empty coverage should not be fully covered")
	}

	// Partially covered
	markCoverage(coverage, 0, 1024*1024) // 1MB
	if isFullyCovered(coverage) {
		t.Error("partially covered block should not be fully covered")
	}

	// Fully covered (all bits set)
	fullCoverage := make([]uint64, CoverageWordsPerBlock)
	for i := range fullCoverage {
		fullCoverage[i] = ^uint64(0) // All bits set
	}
	if !isFullyCovered(fullCoverage) {
		t.Error("fully covered block should be fully covered")
	}

	// Nil coverage
	if isFullyCovered(nil) {
		t.Error("nil coverage should not be fully covered")
	}
}

func TestGetCoveredSize(t *testing.T) {
	coverage := newCoverageBitmap()

	// Empty coverage
	if size := getCoveredSize(coverage); size != 0 {
		t.Errorf("empty coverage should have size 0, got %d", size)
	}

	// Mark first 64 bytes
	markCoverage(coverage, 0, 64)
	if size := getCoveredSize(coverage); size != 64 {
		t.Errorf("expected covered size 64, got %d", size)
	}

	// Mark more bytes
	markCoverage(coverage, 64, 64) // Now 0-128 covered
	if size := getCoveredSize(coverage); size != 128 {
		t.Errorf("expected covered size 128, got %d", size)
	}

	// Nil coverage
	if size := getCoveredSize(nil); size != 0 {
		t.Errorf("nil coverage should have size 0, got %d", size)
	}
}

// ============================================================================
// GetDirtyBlocks Tests
// ============================================================================

func TestGetDirtyBlocks_Empty(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	_, err := c.GetDirtyBlocks(context.Background(), "nonexistent")
	if err != ErrFileNotInCache {
		t.Errorf("expected ErrFileNotInCache, got %v", err)
	}
}

func TestGetDirtyBlocks_SortedByChunkAndBlock(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write to different chunks and blocks in random order
	// Chunk 1, block 2
	_ = c.Write(ctx, payloadID, 1, []byte("chunk1-block2"), 2*BlockSize)
	// Chunk 0, block 1
	_ = c.Write(ctx, payloadID, 0, []byte("chunk0-block1"), 1*BlockSize)
	// Chunk 1, block 0
	_ = c.Write(ctx, payloadID, 1, []byte("chunk1-block0"), 0)
	// Chunk 0, block 0
	_ = c.Write(ctx, payloadID, 0, []byte("chunk0-block0"), 0)

	blocks, err := c.GetDirtyBlocks(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetDirtyBlocks failed: %v", err)
	}

	// Should be sorted: chunk0-block0, chunk0-block1, chunk1-block0, chunk1-block2
	expected := []struct {
		chunkIdx uint32
		blockIdx uint32
	}{
		{0, 0},
		{0, 1},
		{1, 0},
		{1, 2},
	}

	if len(blocks) != len(expected) {
		t.Fatalf("expected %d blocks, got %d", len(expected), len(blocks))
	}

	for i, exp := range expected {
		if blocks[i].ChunkIndex != exp.chunkIdx || blocks[i].BlockIndex != exp.blockIdx {
			t.Errorf("block[%d]: got chunk=%d block=%d, want chunk=%d block=%d",
				i, blocks[i].ChunkIndex, blocks[i].BlockIndex, exp.chunkIdx, exp.blockIdx)
		}
	}
}

func TestGetDirtyBlocks_OnlyReturnsPending(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write two blocks
	_ = c.Write(ctx, payloadID, 0, []byte("block0"), 0)
	_ = c.Write(ctx, payloadID, 0, []byte("block1"), BlockSize)

	// Mark second block as uploaded
	c.MarkBlockUploaded(ctx, payloadID, 0, 1)

	// Get dirty again - should only have the pending one
	blocks, err := c.GetDirtyBlocks(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetDirtyBlocks failed: %v", err)
	}

	if len(blocks) != 1 {
		t.Errorf("expected 1 pending block, got %d", len(blocks))
	}
	if blocks[0].BlockIndex != 0 {
		t.Errorf("expected pending block at index 0, got %d", blocks[0].BlockIndex)
	}
}

func TestGetDirtyBlocks_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetDirtyBlocks(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestGetDirtyBlocks_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	_, err := c.GetDirtyBlocks(context.Background(), "test")
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// MarkBlockUploaded Tests
// ============================================================================

func TestMarkBlockUploaded_Success(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Verify we have pending blocks
	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}

	// Mark as uploaded
	marked := c.MarkBlockUploaded(ctx, payloadID, 0, 0)
	if !marked {
		t.Error("MarkBlockUploaded should return true")
	}

	// Should have no more dirty blocks
	blocks, _ = c.GetDirtyBlocks(ctx, payloadID)
	if len(blocks) != 0 {
		t.Errorf("expected 0 dirty blocks after upload, got %d", len(blocks))
	}
}

func TestMarkBlockUploaded_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Try to mark a nonexistent block
	marked := c.MarkBlockUploaded(ctx, payloadID, 99, 99)
	if marked {
		t.Error("MarkBlockUploaded should return false for nonexistent block")
	}
}

func TestMarkBlockUploaded_AlreadyUploaded(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Mark as uploaded twice
	c.MarkBlockUploaded(ctx, payloadID, 0, 0)
	marked := c.MarkBlockUploaded(ctx, payloadID, 0, 0)

	// Second call should return false (already uploaded)
	if marked {
		t.Error("MarkBlockUploaded should return false for already uploaded block")
	}
}

func TestMarkBlockUploaded_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	marked := c.MarkBlockUploaded(ctx, "test", 0, 0)
	if marked {
		t.Error("MarkBlockUploaded should return false for cancelled context")
	}
}

func TestMarkBlockUploaded_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	marked := c.MarkBlockUploaded(context.Background(), "test", 0, 0)
	if marked {
		t.Error("MarkBlockUploaded should return false for closed cache")
	}
}

// ============================================================================
// MarkBlockUploading Tests
// ============================================================================

func TestMarkBlockUploading_Success(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Mark as uploading
	marked := c.MarkBlockUploading(ctx, payloadID, 0, 0)
	if !marked {
		t.Error("MarkBlockUploading should return true")
	}

	// Block should still be returned by GetDirtyBlocks? No - uploading is not pending
	// Actually let me check the implementation - it only returns BlockStatePending
	blocks, _ := c.GetDirtyBlocks(ctx, payloadID)
	if len(blocks) != 0 {
		t.Errorf("expected 0 dirty blocks (uploading is not pending), got %d", len(blocks))
	}
}

func TestMarkBlockUploading_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Try to mark a nonexistent block
	marked := c.MarkBlockUploading(ctx, payloadID, 99, 99)
	if marked {
		t.Error("MarkBlockUploading should return false for nonexistent block")
	}
}

func TestMarkBlockUploading_AlreadyUploading(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	_ = c.Write(ctx, payloadID, 0, []byte("data"), 0)

	// Mark as uploading twice
	c.MarkBlockUploading(ctx, payloadID, 0, 0)
	marked := c.MarkBlockUploading(ctx, payloadID, 0, 0)

	// Second call should return false (not pending anymore)
	if marked {
		t.Error("MarkBlockUploading should return false for already uploading block")
	}
}

// ============================================================================
// GetBlockData Tests
// ============================================================================

func TestGetBlockData_Success(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"
	testData := []byte("test data content")

	_ = c.Write(ctx, payloadID, 0, testData, 0)

	data, size, err := c.GetBlockData(ctx, payloadID, 0, 0)
	if err != nil {
		t.Fatalf("GetBlockData failed: %v", err)
	}

	if size != uint32(len(testData)) {
		t.Errorf("expected size %d, got %d", len(testData), size)
	}

	if string(data[:len(testData)]) != string(testData) {
		t.Errorf("data mismatch: got %q, want %q", data[:len(testData)], testData)
	}
}

func TestGetBlockData_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// File doesn't exist
	_, _, err := c.GetBlockData(ctx, "nonexistent", 0, 0)
	if err != ErrBlockNotFound {
		t.Errorf("expected ErrBlockNotFound, got %v", err)
	}

	// File exists but block doesn't
	_ = c.Write(ctx, "test", 0, []byte("data"), 0)
	_, _, err = c.GetBlockData(ctx, "test", 99, 99)
	if err != ErrBlockNotFound {
		t.Errorf("expected ErrBlockNotFound for nonexistent block, got %v", err)
	}
}

func TestGetBlockData_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := c.GetBlockData(ctx, "test", 0, 0)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestGetBlockData_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.Write(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	_, _, err := c.GetBlockData(context.Background(), "test", 0, 0)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// Backpressure (ErrCacheFull) Tests
// ============================================================================

func TestWrite_CacheFull_ReturnsError(t *testing.T) {
	// Create a cache with small max size
	// Block buffers are 4MB, so we need at least one block to fit
	// Use a size that allows one block but not two
	c := New(BlockSize + 1024) // Just over one block
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Write data that creates a block (all pending, can't be evicted)
	err := c.Write(ctx, payloadID, 0, make([]byte, 1024), 0)
	if err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}

	// Try to write to a different block that would exceed cache size
	// Since all data is pending (not uploaded), eviction can't free space
	err = c.Write(ctx, payloadID, 0, make([]byte, 1024), BlockSize)
	if err != ErrCacheFull {
		t.Errorf("expected ErrCacheFull when cache is full of pending data, got %v", err)
	}
}

func TestWrite_CacheFull_SucceedsAfterUpload(t *testing.T) {
	// Create a cache with small max size
	c := New(BlockSize + 1024) // Just over one block
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	payloadID := "test-file"

	// Fill cache with pending data
	_ = c.Write(ctx, payloadID, 0, make([]byte, 1024), 0)

	// Mark block as uploaded so it can be evicted
	c.MarkBlockUploaded(ctx, payloadID, 0, 0)

	// Now write should succeed because eviction can free space
	err := c.Write(ctx, payloadID, 0, make([]byte, 1024), BlockSize)
	if err != nil {
		t.Errorf("write should succeed after upload (eviction possible), got %v", err)
	}
}

// ============================================================================
// Flush Benchmarks
// ============================================================================

// BenchmarkGetDirtyBlocks measures dirty block retrieval performance.
func BenchmarkGetDirtyBlocks(b *testing.B) {
	chunkCounts := []int{1, 10, 100}

	for _, chunks := range chunkCounts {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"

			// Create dirty blocks across multiple chunks
			data := make([]byte, 32*1024)
			for i := 0; i < chunks; i++ {
				_ = c.Write(ctx, payloadID, uint32(i), data, 0)
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := c.GetDirtyBlocks(ctx, payloadID)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkMarkBlockUploaded measures block upload marking performance.
// Pre-creates many blocks, then marks them all as uploaded.
func BenchmarkMarkBlockUploaded(b *testing.B) {
	// Use a fixed number of blocks to avoid excessive memory usage
	// (each block allocates 4MB)
	const maxBlocks = 100

	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()

	// Pre-create blocks across multiple files
	for i := 0; i < maxBlocks; i++ {
		payloadID := fmt.Sprintf("file-%d", i)
		_ = c.Write(ctx, payloadID, 0, make([]byte, 1024), 0)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		payloadID := fmt.Sprintf("file-%d", i%maxBlocks)
		c.MarkBlockUploaded(ctx, payloadID, 0, 0)
	}
}
