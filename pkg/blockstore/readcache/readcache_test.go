package readcache

import (
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
