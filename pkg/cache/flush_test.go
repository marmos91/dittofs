package cache

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// ============================================================================
// Helper Function Tests
// ============================================================================

func TestPartitionByState(t *testing.T) {
	slices := []Slice{
		{ID: "1", State: SliceStatePending},
		{ID: "2", State: SliceStateFlushed},
		{ID: "3", State: SliceStatePending},
		{ID: "4", State: SliceStateUploading},
	}

	pending, other := partitionByState(slices, SliceStatePending)

	if len(pending) != 2 {
		t.Errorf("expected 2 pending, got %d", len(pending))
	}
	if len(other) != 2 {
		t.Errorf("expected 2 other, got %d", len(other))
	}

	for _, s := range pending {
		if s.State != SliceStatePending {
			t.Errorf("expected pending state, got %v", s.State)
		}
	}
}

func TestPartitionByState_Empty(t *testing.T) {
	pending, other := partitionByState(nil, SliceStatePending)

	if len(pending) != 0 || len(other) != 0 {
		t.Error("expected empty results for nil input")
	}
}

func TestPartitionByState_AllMatch(t *testing.T) {
	slices := []Slice{
		{ID: "1", State: SliceStatePending},
		{ID: "2", State: SliceStatePending},
	}

	pending, other := partitionByState(slices, SliceStatePending)

	if len(pending) != 2 {
		t.Errorf("expected 2 pending, got %d", len(pending))
	}
	if len(other) != 0 {
		t.Errorf("expected 0 other, got %d", len(other))
	}
}

func TestNewSliceFrom(t *testing.T) {
	original := Slice{
		ID:        "original-id",
		Offset:    100,
		Length:    50,
		Data:      []byte("test data"),
		State:     SliceStateFlushed,
		CreatedAt: time.Now().Add(-time.Hour),
	}

	copied := newSliceFrom(original)

	// Should have new ID
	if copied.ID == original.ID {
		t.Error("copied slice should have new ID")
	}

	// Should preserve offset and length
	if copied.Offset != original.Offset {
		t.Errorf("offset mismatch: got %d, want %d", copied.Offset, original.Offset)
	}
	if copied.Length != original.Length {
		t.Errorf("length mismatch: got %d, want %d", copied.Length, original.Length)
	}

	// Should deep copy data
	if string(copied.Data) != string(original.Data) {
		t.Error("data content should match")
	}
	copied.Data[0] = 'X'
	if original.Data[0] == 'X' {
		t.Error("data should be deep copied, not shared")
	}

	// Should reset state to pending
	if copied.State != SliceStatePending {
		t.Errorf("state should be pending, got %v", copied.State)
	}

	// Should have fresh timestamp
	if copied.CreatedAt.Before(original.CreatedAt) {
		t.Error("copied slice should have newer timestamp")
	}
}

func TestExtendSlice_NoGrowth(t *testing.T) {
	dst := Slice{
		Offset: 0,
		Length: 100,
		Data:   make([]byte, 100),
	}
	src := Slice{
		Offset: 50,
		Length: 30,
		Data:   []byte("inserted"),
	}

	extendSlice(&dst, &src)

	if dst.Length != 100 {
		t.Errorf("length should stay 100, got %d", dst.Length)
	}
	if string(dst.Data[50:58]) != "inserted" {
		t.Error("data not copied at correct offset")
	}
}

func TestExtendSlice_WithGrowth(t *testing.T) {
	dst := Slice{
		Offset: 0,
		Length: 50,
		Data:   make([]byte, 50),
	}
	for i := range dst.Data {
		dst.Data[i] = 'A'
	}

	src := Slice{
		Offset: 40,
		Length: 30,
		Data:   []byte("BBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"), // 30 B's
	}

	extendSlice(&dst, &src)

	if dst.Length != 70 {
		t.Errorf("length should be 70, got %d", dst.Length)
	}
	// First 40 bytes should be A's
	for i := 0; i < 40; i++ {
		if dst.Data[i] != 'A' {
			t.Errorf("byte %d should be A, got %c", i, dst.Data[i])
			break
		}
	}
	// Bytes 40-69 should be B's
	for i := 40; i < 70; i++ {
		if dst.Data[i] != 'B' {
			t.Errorf("byte %d should be B, got %c", i, dst.Data[i])
			break
		}
	}
}

func TestMergeAdjacent_Single(t *testing.T) {
	slices := []Slice{
		{Offset: 0, Length: 10, Data: make([]byte, 10)},
	}

	result := mergeAdjacent(slices)

	if len(result) != 1 {
		t.Errorf("expected 1 slice, got %d", len(result))
	}
}

func TestMergeAdjacent_NoOverlap(t *testing.T) {
	slices := []Slice{
		{Offset: 0, Length: 10, Data: make([]byte, 10)},
		{Offset: 100, Length: 10, Data: make([]byte, 10)},
	}

	result := mergeAdjacent(slices)

	if len(result) != 2 {
		t.Errorf("expected 2 slices (gap), got %d", len(result))
	}
}

func TestMergeAdjacent_Adjacent(t *testing.T) {
	slices := []Slice{
		{Offset: 0, Length: 10, Data: make([]byte, 10)},
		{Offset: 10, Length: 10, Data: make([]byte, 10)},
	}

	result := mergeAdjacent(slices)

	if len(result) != 1 {
		t.Errorf("expected 1 merged slice, got %d", len(result))
	}
	if result[0].Length != 20 {
		t.Errorf("expected length 20, got %d", result[0].Length)
	}
}

func TestMergeAdjacent_Overlapping(t *testing.T) {
	slices := []Slice{
		{Offset: 0, Length: 50, Data: make([]byte, 50)},
		{Offset: 30, Length: 50, Data: make([]byte, 50)},
	}

	result := mergeAdjacent(slices)

	if len(result) != 1 {
		t.Errorf("expected 1 merged slice, got %d", len(result))
	}
	if result[0].Length != 80 {
		t.Errorf("expected length 80, got %d", result[0].Length)
	}
}

func TestMergeAdjacent_MultipleGroups(t *testing.T) {
	slices := []Slice{
		{Offset: 0, Length: 10, Data: make([]byte, 10)},
		{Offset: 10, Length: 10, Data: make([]byte, 10)},
		{Offset: 100, Length: 10, Data: make([]byte, 10)},
		{Offset: 110, Length: 10, Data: make([]byte, 10)},
	}

	result := mergeAdjacent(slices)

	if len(result) != 2 {
		t.Errorf("expected 2 merged groups, got %d", len(result))
	}
	if result[0].Length != 20 {
		t.Errorf("first group length should be 20, got %d", result[0].Length)
	}
	if result[1].Length != 20 {
		t.Errorf("second group length should be 20, got %d", result[1].Length)
	}
}

// ============================================================================
// GetDirtySlices Tests
// ============================================================================

func TestGetDirtySlices_Empty(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	_, err := c.GetDirtySlices(context.Background(), "nonexistent")
	if err != ErrFileNotInCache {
		t.Errorf("expected ErrFileNotInCache, got %v", err)
	}
}

func TestGetDirtySlices_SortedByChunkAndOffset(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write in random order
	_ = c.WriteSlice(ctx, handle, 1, []byte("chunk1-offset100"), 100)
	_ = c.WriteSlice(ctx, handle, 0, []byte("chunk0-offset50"), 50)
	_ = c.WriteSlice(ctx, handle, 1, []byte("chunk1-offset0"), 0)
	_ = c.WriteSlice(ctx, handle, 0, []byte("chunk0-offset0"), 0)

	slices, err := c.GetDirtySlices(ctx, handle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}

	// Should be sorted: chunk0-offset0, chunk0-offset50, chunk1-offset0, chunk1-offset100
	if len(slices) != 4 {
		t.Fatalf("expected 4 slices, got %d", len(slices))
	}

	expected := []struct {
		chunk  uint32
		offset uint32
	}{
		{0, 0},
		{0, 50},
		{1, 0},
		{1, 100},
	}

	for i, exp := range expected {
		if slices[i].ChunkIndex != exp.chunk || slices[i].Offset != exp.offset {
			t.Errorf("slice[%d]: got chunk=%d offset=%d, want chunk=%d offset=%d",
				i, slices[i].ChunkIndex, slices[i].Offset, exp.chunk, exp.offset)
		}
	}
}

func TestGetDirtySlices_OnlyReturnsPending(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write two slices
	_ = c.WriteSlice(ctx, handle, 0, []byte("pending"), 0)
	_ = c.WriteSlice(ctx, handle, 0, []byte("will-be-flushed"), 100)

	// Mark second as flushed
	slices, _ := c.GetDirtySlices(ctx, handle)
	for _, s := range slices {
		if s.Offset == 100 {
			_ = c.MarkSliceFlushed(ctx, handle, s.ID, nil)
		}
	}

	// Get dirty again - should only have the pending one
	slices, err := c.GetDirtySlices(ctx, handle)
	if err != nil {
		t.Fatalf("GetDirtySlices failed: %v", err)
	}

	if len(slices) != 1 {
		t.Errorf("expected 1 pending slice, got %d", len(slices))
	}
	if slices[0].Offset != 0 {
		t.Errorf("expected pending slice at offset 0, got %d", slices[0].Offset)
	}
}

func TestGetDirtySlices_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.GetDirtySlices(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestGetDirtySlices_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	_, err := c.GetDirtySlices(context.Background(), "test")
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// MarkSliceFlushed Tests
// ============================================================================

func TestMarkSliceFlushed_Success(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	_ = c.WriteSlice(ctx, handle, 0, []byte("data"), 0)

	slices, _ := c.GetDirtySlices(ctx, handle)
	if len(slices) != 1 {
		t.Fatalf("expected 1 slice, got %d", len(slices))
	}

	blockRefs := []BlockRef{{ID: "block-1", Size: 4}}
	err := c.MarkSliceFlushed(ctx, handle, slices[0].ID, blockRefs)
	if err != nil {
		t.Fatalf("MarkSliceFlushed failed: %v", err)
	}

	// Should have no more dirty slices
	slices, _ = c.GetDirtySlices(ctx, handle)
	if len(slices) != 0 {
		t.Errorf("expected 0 dirty slices after flush, got %d", len(slices))
	}
}

func TestMarkSliceFlushed_NotFound(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	_ = c.WriteSlice(ctx, handle, 0, []byte("data"), 0)

	err := c.MarkSliceFlushed(ctx, handle, "nonexistent-id", nil)
	if err != ErrSliceNotFound {
		t.Errorf("expected ErrSliceNotFound, got %v", err)
	}
}

func TestMarkSliceFlushed_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.MarkSliceFlushed(ctx, "test", "id", nil)
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestMarkSliceFlushed_CacheClosed(t *testing.T) {
	c := New(0)
	_ = c.WriteSlice(context.Background(), "test", 0, []byte("data"), 0)
	_ = c.Close()

	err := c.MarkSliceFlushed(context.Background(), "test", "id", nil)
	if err != ErrCacheClosed {
		t.Errorf("expected ErrCacheClosed, got %v", err)
	}
}

// ============================================================================
// CoalesceWrites Tests
// ============================================================================

func TestCoalesceWrites_MergesAdjacentPending(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write sequential small chunks (simulating NFS writes)
	for i := 0; i < 10; i++ {
		_ = c.WriteSlice(ctx, handle, 0, make([]byte, 32*1024), uint32(i*32*1024))
	}

	slices, _ := c.GetDirtySlices(ctx, handle)

	// Sequential writes should be merged by tryExtendAdjacentSlice,
	// but even if not, CoalesceWrites should merge them
	if len(slices) != 1 {
		t.Errorf("expected 1 coalesced slice, got %d", len(slices))
	}
	if slices[0].Length != 320*1024 {
		t.Errorf("expected length %d, got %d", 320*1024, slices[0].Length)
	}
}

func TestCoalesceWrites_PreservesFlushed(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write and flush first slice
	_ = c.WriteSlice(ctx, handle, 0, []byte("first"), 0)
	slices, _ := c.GetDirtySlices(ctx, handle)
	_ = c.MarkSliceFlushed(ctx, handle, slices[0].ID, nil)

	// Write adjacent pending slice
	_ = c.WriteSlice(ctx, handle, 0, []byte("second"), 5)

	// Coalesce should not merge flushed with pending
	err := c.CoalesceWrites(ctx, handle)
	if err != nil {
		t.Fatalf("CoalesceWrites failed: %v", err)
	}

	// Should still have one pending
	slices, _ = c.GetDirtySlices(ctx, handle)
	if len(slices) != 1 {
		t.Errorf("expected 1 pending slice, got %d", len(slices))
	}
}

func TestCoalesceWrites_FileNotInCache(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	err := c.CoalesceWrites(context.Background(), "nonexistent")
	if err != ErrFileNotInCache {
		t.Errorf("expected ErrFileNotInCache, got %v", err)
	}
}

func TestCoalesceWrites_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.CoalesceWrites(ctx, "test")
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCoalesceWrites_DataIntegrity(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write overlapping data with specific patterns
	data1 := []byte("AAAAAAAAAA") // 10 bytes at offset 0
	data2 := []byte("BBBBB")      // 5 bytes at offset 5 (overlaps)
	data3 := []byte("CCC")        // 3 bytes at offset 10 (adjacent)

	_ = c.WriteSlice(ctx, handle, 0, data1, 0)
	_ = c.WriteSlice(ctx, handle, 0, data2, 5)
	_ = c.WriteSlice(ctx, handle, 0, data3, 10)

	slices, _ := c.GetDirtySlices(ctx, handle)
	if len(slices) != 1 {
		t.Fatalf("expected 1 coalesced slice, got %d", len(slices))
	}

	// Expected: AAAAABBBBBCCC (13 bytes)
	expected := "AAAAABBBBBCCC"
	if string(slices[0].Data) != expected {
		t.Errorf("data mismatch: got %q, want %q", slices[0].Data, expected)
	}
}

// ============================================================================
// Flush Benchmarks
// ============================================================================

// BenchmarkGetDirtySlices measures dirty slice retrieval performance.
func BenchmarkGetDirtySlices(b *testing.B) {
	chunkCounts := []int{1, 10, 100}

	for _, chunks := range chunkCounts {
		b.Run(fmt.Sprintf("chunks=%d", chunks), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()
			payloadID := "bench-file"

			// Create dirty slices across multiple chunks
			data := make([]byte, 32*1024)
			for i := 0; i < chunks; i++ {
				_ = c.WriteSlice(ctx, payloadID, uint32(i), data, 0)
			}

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				_, err := c.GetDirtySlices(ctx, payloadID)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCoalesceWrites measures write coalescing performance.
func BenchmarkCoalesceWrites(b *testing.B) {
	slicesPerChunk := []int{10, 50, 100}

	for _, slices := range slicesPerChunk {
		b.Run(fmt.Sprintf("slices=%d", slices), func(b *testing.B) {
			c := New(0)
			defer func() { _ = c.Close() }()

			ctx := context.Background()

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				payloadID := fmt.Sprintf("file-%d", i)

				// Create many small non-adjacent slices
				for j := 0; j < slices; j++ {
					data := make([]byte, 1024)
					offset := uint32(j * 2048) // 1KB gap between slices
					_ = c.WriteSlice(ctx, payloadID, 0, data, offset)
				}
				b.StartTimer()

				if err := c.CoalesceWrites(ctx, payloadID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ============================================================================
// MarkBlockRangeFlushed Tests
// ============================================================================

func TestMarkBlockRangeFlushed_SlicesFullyContained(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write slices that are fully contained within a 4MB block (offset 0-4MB)
	blockSize := uint32(4 * 1024 * 1024) // 4MB

	// Small slice at offset 0
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 1024), 0)
	// Small slice at offset 1MB
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 1024), 1024*1024)

	// Verify we have 2 pending slices (they may have been merged)
	slices, _ := c.GetDirtySlices(ctx, handle)
	initialCount := len(slices)
	if initialCount == 0 {
		t.Fatal("expected at least 1 slice")
	}

	// Mark block 0 (0 to 4MB) as flushed
	marked := c.MarkBlockRangeFlushed(ctx, handle, 0, 0, blockSize)

	// All slices should be marked as flushed since they're within the block
	if marked == 0 {
		t.Error("expected at least 1 slice to be marked as flushed")
	}

	// Should have no more dirty slices
	slices, _ = c.GetDirtySlices(ctx, handle)
	if len(slices) != 0 {
		t.Errorf("expected 0 dirty slices after marking block flushed, got %d", len(slices))
	}
}

func TestMarkBlockRangeFlushed_SliceExtendsBeyndBlock(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	blockSize := uint32(4 * 1024 * 1024) // 4MB

	// Write a slice that extends beyond block 0 (starts at 3MB, length 2MB)
	// This slice spans from 3MB to 5MB, so it extends beyond block 0 (0-4MB)
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 2*1024*1024), 3*1024*1024)

	// Mark block 0 (0 to 4MB) as flushed
	marked := c.MarkBlockRangeFlushed(ctx, handle, 0, 0, blockSize)

	// Slice should NOT be marked because it extends beyond the block
	if marked != 0 {
		t.Errorf("expected 0 slices marked (slice extends beyond block), got %d", marked)
	}

	// Should still have the dirty slice
	slices, _ := c.GetDirtySlices(ctx, handle)
	if len(slices) != 1 {
		t.Errorf("expected 1 dirty slice still present, got %d", len(slices))
	}
}

func TestMarkBlockRangeFlushed_OnlyPendingSlices(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write a slice and mark it as flushed manually
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 1024), 0)
	slices, _ := c.GetDirtySlices(ctx, handle)
	_ = c.MarkSliceFlushed(ctx, handle, slices[0].ID, nil)

	// MarkBlockRangeFlushed should not affect already-flushed slices
	marked := c.MarkBlockRangeFlushed(ctx, handle, 0, 0, 4*1024*1024)

	if marked != 0 {
		t.Errorf("expected 0 newly marked (slice already flushed), got %d", marked)
	}
}

func TestMarkBlockRangeFlushed_NonexistentFile(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	marked := c.MarkBlockRangeFlushed(context.Background(), "nonexistent", 0, 0, 4*1024*1024)

	if marked != 0 {
		t.Errorf("expected 0 for nonexistent file, got %d", marked)
	}
}

func TestMarkBlockRangeFlushed_ContextCancelled(t *testing.T) {
	c := New(0)
	defer func() { _ = c.Close() }()

	_ = c.WriteSlice(context.Background(), "test", 0, make([]byte, 1024), 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	marked := c.MarkBlockRangeFlushed(ctx, "test", 0, 0, 4*1024*1024)

	if marked != 0 {
		t.Errorf("expected 0 for cancelled context, got %d", marked)
	}
}

// ============================================================================
// Backpressure (ErrCacheFull) Tests
// ============================================================================

func TestWriteSlice_CacheFull_ReturnsError(t *testing.T) {
	// Create a cache with 1KB max size
	c := New(1024)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Write data that fills the cache (all pending, can't be evicted)
	err := c.WriteSlice(ctx, handle, 0, make([]byte, 512), 0)
	if err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}

	// Try to write more data that would exceed cache size
	// Since all data is pending (not flushed), eviction can't free space
	err = c.WriteSlice(ctx, handle, 0, make([]byte, 600), 512)
	if err != ErrCacheFull {
		t.Errorf("expected ErrCacheFull when cache is full of pending data, got %v", err)
	}
}

func TestWriteSlice_CacheFull_SucceedsAfterFlush(t *testing.T) {
	// Create a cache with 1KB max size
	c := New(1024)
	defer func() { _ = c.Close() }()

	ctx := context.Background()
	handle := "test-file"

	// Fill cache with pending data
	_ = c.WriteSlice(ctx, handle, 0, make([]byte, 512), 0)

	// Mark data as flushed so it can be evicted
	slices, _ := c.GetDirtySlices(ctx, handle)
	for _, s := range slices {
		_ = c.MarkSliceFlushed(ctx, handle, s.ID, nil)
	}

	// Now write should succeed because eviction can free space
	err := c.WriteSlice(ctx, handle, 0, make([]byte, 600), 512)
	if err != nil {
		t.Errorf("write should succeed after flushing (eviction possible), got %v", err)
	}
}
