package readcache

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBlockSize is a small block size for tests to avoid allocating real 8MB blocks.
const testBlockSize = 1024

func makeData(size int, fill byte) []byte {
	d := make([]byte, size)
	for i := range d {
		d[i] = fill
	}
	return d
}

// --- New ---

func TestReadCache_New_ZeroDisabled(t *testing.T) {
	c := New(0)
	assert.Nil(t, c, "New(0) should return nil (disabled mode)")
}

func TestReadCache_New_NegativeDisabled(t *testing.T) {
	c := New(-1)
	assert.Nil(t, c, "New(-1) should return nil")
}

func TestReadCache_New_Positive(t *testing.T) {
	c := New(testBlockSize * 2)
	require.NotNil(t, c, "New with positive value should return non-nil")
	assert.Equal(t, int64(testBlockSize*2), c.maxBytes)
	c.Close()
}

// --- Put and Get ---

func TestReadCache_PutAndGet_Hit(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	data := makeData(testBlockSize, 0xAA)
	c.Put("file1", 0, data, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("file1", 0, dest, 0)
	assert.True(t, ok, "expected cache hit")
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, data, dest[:n])
}

func TestReadCache_Get_Miss(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("nonexistent", 0, dest, 0)
	assert.False(t, ok, "expected cache miss")
	assert.Equal(t, 0, n)
}

func TestReadCache_Get_CopyOnRead(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	data := makeData(testBlockSize, 0xBB)
	c.Put("file1", 0, data, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("file1", 0, dest, 0)
	require.True(t, ok)
	require.Equal(t, testBlockSize, n)

	// Modify returned data
	dest[0] = 0xFF

	// Re-read should still have original data
	dest2 := make([]byte, testBlockSize)
	n2, ok2 := c.Get("file1", 0, dest2, 0)
	assert.True(t, ok2)
	assert.Equal(t, testBlockSize, n2)
	assert.Equal(t, byte(0xBB), dest2[0], "cache data should not be affected by caller modification")
}

func TestReadCache_Get_WithOffset(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	data := makeData(testBlockSize, 0x00)
	data[512] = 0xCC
	data[513] = 0xDD
	c.Put("file1", 0, data, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("file1", 0, dest, 512)
	assert.True(t, ok)
	assert.Equal(t, testBlockSize-512, n)
	assert.Equal(t, byte(0xCC), dest[0])
	assert.Equal(t, byte(0xDD), dest[1])
}

func TestReadCache_Get_OffsetBeyondData(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	data := makeData(512, 0xAA)
	c.Put("file1", 0, data, 512)

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("file1", 0, dest, 512) // offset == dataSize
	assert.False(t, ok, "offset >= dataSize should return miss")
	assert.Equal(t, 0, n)

	n, ok = c.Get("file1", 0, dest, 1000) // offset > dataSize
	assert.False(t, ok)
	assert.Equal(t, 0, n)
}

// --- Put update existing ---

func TestReadCache_Put_UpdateExisting(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	data1 := makeData(testBlockSize, 0xAA)
	c.Put("file1", 0, data1, uint32(testBlockSize))

	data2 := makeData(testBlockSize, 0xBB)
	c.Put("file1", 0, data2, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	n, ok := c.Get("file1", 0, dest, 0)
	assert.True(t, ok)
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, byte(0xBB), dest[0], "should return updated data")
}

// --- Eviction ---

func TestReadCache_Put_EvictsLRU(t *testing.T) {
	// Budget for exactly 2 blocks
	c := New(int64(testBlockSize * 2))
	require.NotNil(t, c)
	defer c.Close()

	d0 := makeData(testBlockSize, 0xAA)
	d1 := makeData(testBlockSize, 0xBB)
	d2 := makeData(testBlockSize, 0xCC)

	c.Put("file1", 0, d0, uint32(testBlockSize)) // block 0
	c.Put("file1", 1, d1, uint32(testBlockSize)) // block 1

	// Insert block 2 -- should evict block 0 (LRU)
	c.Put("file1", 2, d2, uint32(testBlockSize))

	dest := make([]byte, testBlockSize)
	_, ok := c.Get("file1", 0, dest, 0)
	assert.False(t, ok, "block 0 should have been evicted")

	_, ok = c.Get("file1", 1, dest, 0)
	assert.True(t, ok, "block 1 should still be cached")

	_, ok = c.Get("file1", 2, dest, 0)
	assert.True(t, ok, "block 2 should be cached")
}

func TestReadCache_Put_EvictsMultiple(t *testing.T) {
	// Budget for exactly 2 small blocks (256 bytes each)
	smallSize := 256
	c := New(int64(smallSize * 2))
	require.NotNil(t, c)
	defer c.Close()

	d0 := makeData(smallSize, 0xAA)
	d1 := makeData(smallSize, 0xBB)
	c.Put("f", 0, d0, uint32(smallSize))
	c.Put("f", 1, d1, uint32(smallSize))

	// Insert a bigger block that requires evicting both
	bigData := makeData(smallSize*2, 0xCC)
	c.Put("f", 2, bigData, uint32(smallSize*2))

	dest := make([]byte, testBlockSize)
	_, ok := c.Get("f", 0, dest, 0)
	assert.False(t, ok, "block 0 should be evicted")
	_, ok = c.Get("f", 1, dest, 0)
	assert.False(t, ok, "block 1 should be evicted")
	_, ok = c.Get("f", 2, dest, 0)
	assert.True(t, ok, "block 2 should be cached")
}

func TestReadCache_Put_SkipsOversizedEntry(t *testing.T) {
	// Budget smaller than a single block
	c := New(100)
	require.NotNil(t, c)
	defer c.Close()

	// Put a block larger than maxBytes -- should be silently skipped
	bigData := makeData(200, 0xAA)
	c.Put("f", 0, bigData, 200)

	dest := make([]byte, 200)
	_, ok := c.Get("f", 0, dest, 0)
	assert.False(t, ok, "oversized entry should not be cached")
	assert.Equal(t, int64(0), c.curBytes, "curBytes should remain 0")
}

// --- LRU Promotion ---

func TestReadCache_LRU_Promotion(t *testing.T) {
	c := New(int64(testBlockSize * 2))
	require.NotNil(t, c)
	defer c.Close()

	d0 := makeData(testBlockSize, 0xAA)
	d1 := makeData(testBlockSize, 0xBB)
	d2 := makeData(testBlockSize, 0xCC)

	c.Put("f", 0, d0, uint32(testBlockSize))
	c.Put("f", 1, d1, uint32(testBlockSize))

	// Access block 0 to promote it
	dest := make([]byte, testBlockSize)
	_, ok := c.Get("f", 0, dest, 0)
	require.True(t, ok)

	// Insert block 2 -- should evict block 1 (now LRU), not block 0
	c.Put("f", 2, d2, uint32(testBlockSize))

	_, ok = c.Get("f", 0, dest, 0)
	assert.True(t, ok, "block 0 should still be cached (was promoted)")
	_, ok = c.Get("f", 1, dest, 0)
	assert.False(t, ok, "block 1 should have been evicted (was LRU)")
	_, ok = c.Get("f", 2, dest, 0)
	assert.True(t, ok, "block 2 should be cached")
}

// --- Invalidate ---

func TestReadCache_Invalidate_Existing(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	c.Put("f", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))
	c.Invalidate("f", 0)

	dest := make([]byte, testBlockSize)
	_, ok := c.Get("f", 0, dest, 0)
	assert.False(t, ok, "invalidated entry should be a miss")
}

func TestReadCache_Invalidate_Missing(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	// Should not panic
	c.Invalidate("nonexistent", 99)
}

// --- InvalidateFile ---

func TestReadCache_InvalidateFile_RemovesAll(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	c.Put("file1", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))
	c.Put("file1", 1, makeData(testBlockSize, 0xBB), uint32(testBlockSize))
	c.Put("file1", 2, makeData(testBlockSize, 0xCC), uint32(testBlockSize))

	c.InvalidateFile("file1")

	dest := make([]byte, testBlockSize)
	for _, idx := range []uint64{0, 1, 2} {
		_, ok := c.Get("file1", idx, dest, 0)
		assert.False(t, ok, "block %d should be invalidated", idx)
	}
}

func TestReadCache_InvalidateFile_EmptyIndex(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	// Should not panic
	c.InvalidateFile("unknown")
}

func TestReadCache_InvalidateFile_OnlyTargetFile(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	c.Put("file1", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))
	c.Put("file2", 0, makeData(testBlockSize, 0xBB), uint32(testBlockSize))

	c.InvalidateFile("file1")

	dest := make([]byte, testBlockSize)
	_, ok := c.Get("file1", 0, dest, 0)
	assert.False(t, ok, "file1 should be invalidated")

	_, ok = c.Get("file2", 0, dest, 0)
	assert.True(t, ok, "file2 should NOT be affected")
}

// --- InvalidateAbove ---

func TestReadCache_InvalidateAbove_RemovesHighBlocks(t *testing.T) {
	c := New(testBlockSize * 16)
	require.NotNil(t, c)
	defer c.Close()

	for i := uint64(0); i < 6; i++ {
		c.Put("file1", i, makeData(testBlockSize, byte(i)), uint32(testBlockSize))
	}

	c.InvalidateAbove("file1", 3)

	dest := make([]byte, testBlockSize)
	for _, idx := range []uint64{0, 1, 2} {
		_, ok := c.Get("file1", idx, dest, 0)
		assert.True(t, ok, "block %d should still be cached", idx)
	}
	for _, idx := range []uint64{3, 4, 5} {
		_, ok := c.Get("file1", idx, dest, 0)
		assert.False(t, ok, "block %d should be invalidated", idx)
	}
}

func TestReadCache_InvalidateAbove_NoMatch(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	// Should not panic
	c.InvalidateAbove("unknown", 0)
}

// --- Contains ---

func TestReadCache_Contains_Hit(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	c.Put("f", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))
	assert.True(t, c.Contains("f", 0))
}

func TestReadCache_Contains_Miss(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	assert.False(t, c.Contains("f", 99))
}

// --- Close ---

func TestReadCache_Close_ClearsAll(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)

	c.Put("f", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))
	c.Close()

	dest := make([]byte, testBlockSize)
	_, ok := c.Get("f", 0, dest, 0)
	assert.False(t, ok, "after Close, Get should return miss")
}

// --- Concurrency ---

func TestReadCache_Concurrency_ReadWrite(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	const goroutines = 16
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			pid := "file"
			for i := 0; i < iterations; i++ {
				idx := uint64(i % 4)
				data := makeData(testBlockSize, byte(id))
				c.Put(pid, idx, data, uint32(testBlockSize))

				dest := make([]byte, testBlockSize)
				c.Get(pid, idx, dest, 0)
				c.Contains(pid, idx)
			}
		}(g)
	}

	wg.Wait()
}

// --- InvalidateRange ---

func TestReadCache_InvalidateRange_SingleBlock(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	for i := uint64(0); i < 4; i++ {
		c.Put("f", i, makeData(testBlockSize, byte(i)), uint32(testBlockSize))
	}

	// Invalidate a range within block 1 only
	c.InvalidateRange("f", uint64(testBlockSize), testBlockSize, uint64(testBlockSize))

	dest := make([]byte, testBlockSize)
	assert.True(t, c.Contains("f", 0), "block 0 should remain")
	assert.False(t, c.Contains("f", 1), "block 1 should be invalidated")
	assert.True(t, c.Contains("f", 2), "block 2 should remain")
	_ = dest
}

func TestReadCache_InvalidateRange_SpansMultipleBlocks(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	for i := uint64(0); i < 4; i++ {
		c.Put("f", i, makeData(testBlockSize, byte(i)), uint32(testBlockSize))
	}

	// Invalidate range spanning blocks 1 and 2 (offset in middle of block 1, length into block 2)
	offset := uint64(testBlockSize) + 100
	length := testBlockSize // crosses into block 2
	c.InvalidateRange("f", offset, length, uint64(testBlockSize))

	assert.True(t, c.Contains("f", 0), "block 0 should remain")
	assert.False(t, c.Contains("f", 1), "block 1 should be invalidated")
	assert.False(t, c.Contains("f", 2), "block 2 should be invalidated")
	assert.True(t, c.Contains("f", 3), "block 3 should remain")
}

func TestReadCache_InvalidateRange_ZeroLength(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	c.Put("f", 0, makeData(testBlockSize, 0xAA), uint32(testBlockSize))

	// Zero length should be a no-op (no underflow)
	c.InvalidateRange("f", 0, 0, uint64(testBlockSize))

	assert.True(t, c.Contains("f", 0), "block should remain after zero-length invalidate")
}

func TestReadCache_InvalidateRange_NilSafe(t *testing.T) {
	var c *ReadCache
	// Should not panic
	c.InvalidateRange("f", 0, 100, uint64(testBlockSize))
}

// --- NotifyRead ---

func TestReadCache_NotifyRead_ZeroLength(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	// Should not panic or underflow
	c.NotifyRead("f", 0, 0, uint64(testBlockSize))
}

func TestReadCache_NotifyRead_NilSafe(t *testing.T) {
	var c *ReadCache
	// Should not panic
	c.NotifyRead("f", 0, 100, uint64(testBlockSize))
}

func TestReadCache_NotifyRead_NoPrefetcher(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	// No prefetcher set -- should not panic
	c.NotifyRead("f", 0, uint64(testBlockSize), uint64(testBlockSize))
}

// --- FillFromStore ---

func TestReadCache_FillFromStore_PopulatesCache(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	// Mock block data loader
	loader := func(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
		data := makeData(testBlockSize, byte(blockIdx))
		return data, uint32(testBlockSize), nil
	}

	// Fill blocks 0-2
	c.FillFromStore(context.Background(), "f", 0, uint64(testBlockSize*3), uint64(testBlockSize), loader)

	for i := uint64(0); i < 3; i++ {
		assert.True(t, c.Contains("f", i), "block %d should be in cache", i)
	}
}

func TestReadCache_FillFromStore_SkipsExisting(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	// Pre-populate block 1
	originalData := makeData(testBlockSize, 0xFF)
	c.Put("f", 1, originalData, uint32(testBlockSize))

	callCount := 0
	loader := func(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
		callCount++
		return makeData(testBlockSize, 0x00), uint32(testBlockSize), nil
	}

	// Fill blocks 0-2
	c.FillFromStore(context.Background(), "f", 0, uint64(testBlockSize*3), uint64(testBlockSize), loader)

	// Loader should have been called for blocks 0 and 2, but NOT block 1
	assert.Equal(t, 2, callCount, "loader should be called for blocks 0 and 2 only")

	// Block 1 should still have original data
	dest := make([]byte, testBlockSize)
	n, ok := c.Get("f", 1, dest, 0)
	require.True(t, ok)
	assert.Equal(t, testBlockSize, n)
	assert.Equal(t, byte(0xFF), dest[0], "block 1 should retain original data")
}

func TestReadCache_FillFromStore_HandlesLoaderError(t *testing.T) {
	c := New(testBlockSize * 8)
	require.NotNil(t, c)
	defer c.Close()

	loader := func(_ context.Context, payloadID string, blockIdx uint64) ([]byte, uint32, error) {
		if blockIdx == 1 {
			return nil, 0, fmt.Errorf("disk error")
		}
		return makeData(testBlockSize, byte(blockIdx)), uint32(testBlockSize), nil
	}

	c.FillFromStore(context.Background(), "f", 0, uint64(testBlockSize*3), uint64(testBlockSize), loader)

	assert.True(t, c.Contains("f", 0), "block 0 should be cached")
	assert.False(t, c.Contains("f", 1), "block 1 should NOT be cached (loader error)")
	assert.True(t, c.Contains("f", 2), "block 2 should be cached")
}

func TestReadCache_FillFromStore_ZeroLength(t *testing.T) {
	c := New(testBlockSize * 4)
	require.NotNil(t, c)
	defer c.Close()

	called := false
	loader := func(_ context.Context, _ string, _ uint64) ([]byte, uint32, error) {
		called = true
		return nil, 0, nil
	}

	// Zero length should be a no-op (no underflow)
	c.FillFromStore(context.Background(), "f", 0, 0, uint64(testBlockSize), loader)
	assert.False(t, called, "loader should not be called for zero-length fill")
}

func TestReadCache_FillFromStore_NilSafe(t *testing.T) {
	var c *ReadCache
	loader := func(_ context.Context, _ string, _ uint64) ([]byte, uint32, error) {
		return nil, 0, nil
	}
	// Should not panic
	c.FillFromStore(context.Background(), "f", 0, 100, uint64(testBlockSize), loader)
}
