package nfs

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Buffer Allocation Tests
// ============================================================================

func TestBufferAllocation(t *testing.T) {
	t.Run("AllocatesSmallBuffer", func(t *testing.T) {
		buf := GetBuffer(100)
		defer PutBuffer(buf)

		assert.GreaterOrEqual(t, len(buf), 100)
		assert.Equal(t, 4*1024, cap(buf))
	})

	t.Run("AllocatesMediumBuffer", func(t *testing.T) {
		buf := GetBuffer(10 * 1024)
		defer PutBuffer(buf)

		assert.GreaterOrEqual(t, len(buf), 10*1024)
		assert.Equal(t, 64*1024, cap(buf))
	})

	t.Run("AllocatesLargeBuffer", func(t *testing.T) {
		buf := GetBuffer(100 * 1024)
		defer PutBuffer(buf)

		assert.GreaterOrEqual(t, len(buf), 100*1024)
		assert.Equal(t, 1024*1024, cap(buf))
	})

	t.Run("AllocatesOversizedBuffer", func(t *testing.T) {
		buf := GetBuffer(2 * 1024 * 1024)
		defer PutBuffer(buf)

		assert.GreaterOrEqual(t, len(buf), 2*1024*1024)
		assert.Equal(t, len(buf), cap(buf))
	})

	t.Run("AllocatesZeroSizeBuffer", func(t *testing.T) {
		buf := GetBuffer(0)
		defer PutBuffer(buf)

		assert.NotNil(t, buf)
		assert.Equal(t, 4*1024, cap(buf))
	})
}

// ============================================================================
// Size Class Tests
// ============================================================================

func TestBufferSizeClasses(t *testing.T) {
	t.Run("BoundarySmallToMedium", func(t *testing.T) {
		buf := GetBuffer(4 * 1024)
		defer PutBuffer(buf)

		assert.Equal(t, 4*1024, len(buf))
		assert.Equal(t, 4*1024, cap(buf))
	})

	t.Run("BoundaryMediumToLarge", func(t *testing.T) {
		buf := GetBuffer(64 * 1024)
		defer PutBuffer(buf)

		assert.Equal(t, 64*1024, len(buf))
		assert.Equal(t, 64*1024, cap(buf))
	})

	t.Run("BoundaryLargeToOversized", func(t *testing.T) {
		buf := GetBuffer(1024 * 1024)
		defer PutBuffer(buf)

		assert.Equal(t, 1024*1024, len(buf))
		assert.Equal(t, 1024*1024, cap(buf))
	})

	t.Run("JustAboveSmall", func(t *testing.T) {
		buf := GetBuffer(4*1024 + 1)
		defer PutBuffer(buf)

		assert.Equal(t, 64*1024, cap(buf))
	})

	t.Run("JustAboveMedium", func(t *testing.T) {
		buf := GetBuffer(64*1024 + 1)
		defer PutBuffer(buf)

		assert.Equal(t, 1024*1024, cap(buf))
	})

	t.Run("JustAboveLarge", func(t *testing.T) {
		buf := GetBuffer(1024*1024 + 1)
		defer PutBuffer(buf)

		assert.GreaterOrEqual(t, len(buf), 1024*1024+1)
	})
}

// ============================================================================
// Put and Reuse Tests
// ============================================================================

func TestBufferPutAndReuse(t *testing.T) {
	t.Run("ReusesReturnedSmallBuffer", func(t *testing.T) {
		buf1 := GetBuffer(1024)
		PutBuffer(buf1)

		buf2 := GetBuffer(1024)
		PutBuffer(buf2)

		assert.Equal(t, cap(buf1), cap(buf2))
	})

	t.Run("HandlesNilPut", func(t *testing.T) {
		require.NotPanics(t, func() {
			PutBuffer(nil)
		})
	})

	t.Run("HandlesEmptySlicePut", func(t *testing.T) {
		require.NotPanics(t, func() {
			PutBuffer([]byte{})
		})
	})

	t.Run("DoesNotPoolOversizedBuffers", func(t *testing.T) {
		buf := GetBuffer(2 * 1024 * 1024)
		originalCap := cap(buf)
		PutBuffer(buf)

		buf2 := GetBuffer(2 * 1024 * 1024)
		defer PutBuffer(buf2)

		assert.Equal(t, len(buf2), cap(buf2))
		assert.Equal(t, originalCap, len(buf))
	})
}

// ============================================================================
// Edge Cases Tests
// ============================================================================

func TestBufferPoolEdgeCases(t *testing.T) {
	t.Run("MultipleGetWithoutPut", func(t *testing.T) {
		buffers := make([][]byte, 10)
		for i := range buffers {
			buffers[i] = GetBuffer(1024)
			assert.NotNil(t, buffers[i])
		}

		for _, buf := range buffers {
			PutBuffer(buf)
		}
	})

	t.Run("PutWithoutGet", func(t *testing.T) {
		buf := make([]byte, 4*1024)

		require.NotPanics(t, func() {
			PutBuffer(buf)
		})
	})

	t.Run("GetPutGetSequence", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			buf := GetBuffer(1024)
			assert.NotNil(t, buf)
			assert.GreaterOrEqual(t, len(buf), 1024)
			PutBuffer(buf)
		}
	})

	t.Run("DifferentSizesInterleaved", func(t *testing.T) {
		small := GetBuffer(1024)
		medium := GetBuffer(10 * 1024)
		large := GetBuffer(100 * 1024)

		assert.Equal(t, 4*1024, cap(small))
		assert.Equal(t, 64*1024, cap(medium))
		assert.Equal(t, 1024*1024, cap(large))

		PutBuffer(medium)
		PutBuffer(small)
		PutBuffer(large)
	})
}

// ============================================================================
// Concurrency Tests
// ============================================================================

func TestBufferPoolConcurrency(t *testing.T) {
	t.Run("ConcurrentGetAndPut", func(t *testing.T) {
		const numGoroutines = 10
		const iterations = 100

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func(id int) {
				defer wg.Done()

				for j := 0; j < iterations; j++ {
					size := uint32((id*100 + j) % (500 * 1024))
					buf := GetBuffer(size)

					if len(buf) > 0 {
						buf[0] = byte(id)
					}

					PutBuffer(buf)
				}
			}(i)
		}

		wg.Wait()
	})

	t.Run("ConcurrentSameSizeClass", func(t *testing.T) {
		const numGoroutines = 20
		const iterations = 50

		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()

				for j := 0; j < iterations; j++ {
					buf := GetBuffer(1024)
					assert.NotNil(t, buf)
					PutBuffer(buf)
				}
			}()
		}

		wg.Wait()
	})

	t.Run("NoDataRaces", func(t *testing.T) {
		const numGoroutines = 5
		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				buf := GetBuffer(1024)
				for j := range buf {
					buf[j] = byte(j % 256)
				}
				PutBuffer(buf)
			}()
		}

		wg.Wait()
	})
}
