package bufpool

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
		buf := Get(100)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 100)
		assert.Equal(t, DefaultSmallSize, cap(buf))
	})

	t.Run("AllocatesMediumBuffer", func(t *testing.T) {
		buf := Get(10 * 1024)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 10*1024)
		assert.Equal(t, DefaultMediumSize, cap(buf))
	})

	t.Run("AllocatesLargeBuffer", func(t *testing.T) {
		buf := Get(100 * 1024)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 100*1024)
		assert.Equal(t, DefaultLargeSize, cap(buf))
	})

	t.Run("AllocatesOversizedBuffer", func(t *testing.T) {
		buf := Get(2 * 1024 * 1024)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 2*1024*1024)
		assert.Equal(t, len(buf), cap(buf))
	})

	t.Run("AllocatesZeroSizeBuffer", func(t *testing.T) {
		buf := Get(0)
		defer Put(buf)

		assert.NotNil(t, buf)
		assert.Equal(t, DefaultSmallSize, cap(buf))
	})
}

// ============================================================================
// Size Class Tests
// ============================================================================

func TestBufferSizeClasses(t *testing.T) {
	t.Run("BoundarySmallToMedium", func(t *testing.T) {
		buf := Get(DefaultSmallSize)
		defer Put(buf)

		assert.Equal(t, DefaultSmallSize, len(buf))
		assert.Equal(t, DefaultSmallSize, cap(buf))
	})

	t.Run("BoundaryMediumToLarge", func(t *testing.T) {
		buf := Get(DefaultMediumSize)
		defer Put(buf)

		assert.Equal(t, DefaultMediumSize, len(buf))
		assert.Equal(t, DefaultMediumSize, cap(buf))
	})

	t.Run("BoundaryLargeToOversized", func(t *testing.T) {
		buf := Get(DefaultLargeSize)
		defer Put(buf)

		assert.Equal(t, DefaultLargeSize, len(buf))
		assert.Equal(t, DefaultLargeSize, cap(buf))
	})

	t.Run("JustAboveSmall", func(t *testing.T) {
		buf := Get(DefaultSmallSize + 1)
		defer Put(buf)

		assert.Equal(t, DefaultMediumSize, cap(buf))
	})

	t.Run("JustAboveMedium", func(t *testing.T) {
		buf := Get(DefaultMediumSize + 1)
		defer Put(buf)

		assert.Equal(t, DefaultLargeSize, cap(buf))
	})

	t.Run("JustAboveLarge", func(t *testing.T) {
		buf := Get(DefaultLargeSize + 1)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), DefaultLargeSize+1)
	})
}

// ============================================================================
// Put and Reuse Tests
// ============================================================================

func TestBufferPutAndReuse(t *testing.T) {
	t.Run("ReusesReturnedSmallBuffer", func(t *testing.T) {
		buf1 := Get(1024)
		Put(buf1)

		buf2 := Get(1024)
		Put(buf2)

		assert.Equal(t, cap(buf1), cap(buf2))
	})

	t.Run("HandlesNilPut", func(t *testing.T) {
		require.NotPanics(t, func() {
			Put(nil)
		})
	})

	t.Run("HandlesEmptySlicePut", func(t *testing.T) {
		require.NotPanics(t, func() {
			Put([]byte{})
		})
	})

	t.Run("DoesNotPoolOversizedBuffers", func(t *testing.T) {
		buf := Get(2 * 1024 * 1024)
		originalCap := cap(buf)
		Put(buf)

		buf2 := Get(2 * 1024 * 1024)
		defer Put(buf2)

		assert.Equal(t, len(buf2), cap(buf2))
		assert.Equal(t, originalCap, len(buf))
	})
}

// ============================================================================
// Custom Pool Tests
// ============================================================================

func TestCustomPool(t *testing.T) {
	t.Run("CustomSizes", func(t *testing.T) {
		pool := NewPool(&Config{
			SmallSize:  1024,
			MediumSize: 8192,
			LargeSize:  65536,
		})

		small := pool.Get(500)
		assert.Equal(t, 1024, cap(small))
		pool.Put(small)

		medium := pool.Get(2000)
		assert.Equal(t, 8192, cap(medium))
		pool.Put(medium)

		large := pool.Get(10000)
		assert.Equal(t, 65536, cap(large))
		pool.Put(large)
	})

	t.Run("NilConfig", func(t *testing.T) {
		pool := NewPool(nil)

		buf := pool.Get(100)
		assert.Equal(t, DefaultSmallSize, cap(buf))
		pool.Put(buf)
	})

	t.Run("ZeroConfigValues", func(t *testing.T) {
		pool := NewPool(&Config{})

		buf := pool.Get(100)
		assert.Equal(t, DefaultSmallSize, cap(buf))
		pool.Put(buf)
	})
}

// ============================================================================
// GetUint32 Tests
// ============================================================================

func TestGetUint32(t *testing.T) {
	t.Run("WorksWithUint32", func(t *testing.T) {
		buf := GetUint32(1024)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 1024)
		assert.Equal(t, DefaultSmallSize, cap(buf))
	})

	t.Run("LargeUint32Value", func(t *testing.T) {
		buf := GetUint32(100 * 1024)
		defer Put(buf)

		assert.GreaterOrEqual(t, len(buf), 100*1024)
		assert.Equal(t, DefaultLargeSize, cap(buf))
	})
}

// ============================================================================
// Edge Cases Tests
// ============================================================================

func TestBufferPoolEdgeCases(t *testing.T) {
	t.Run("MultipleGetWithoutPut", func(t *testing.T) {
		buffers := make([][]byte, 10)
		for i := range buffers {
			buffers[i] = Get(1024)
			assert.NotNil(t, buffers[i])
		}

		for _, buf := range buffers {
			Put(buf)
		}
	})

	t.Run("PutWithoutGet", func(t *testing.T) {
		buf := make([]byte, DefaultSmallSize)

		require.NotPanics(t, func() {
			Put(buf)
		})
	})

	t.Run("GetPutGetSequence", func(t *testing.T) {
		for i := 0; i < 5; i++ {
			buf := Get(1024)
			assert.NotNil(t, buf)
			assert.GreaterOrEqual(t, len(buf), 1024)
			Put(buf)
		}
	})

	t.Run("DifferentSizesInterleaved", func(t *testing.T) {
		small := Get(1024)
		medium := Get(10 * 1024)
		large := Get(100 * 1024)

		assert.Equal(t, DefaultSmallSize, cap(small))
		assert.Equal(t, DefaultMediumSize, cap(medium))
		assert.Equal(t, DefaultLargeSize, cap(large))

		Put(medium)
		Put(small)
		Put(large)
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
					size := (id*100 + j) % (500 * 1024)
					buf := Get(size)

					if len(buf) > 0 {
						buf[0] = byte(id)
					}

					Put(buf)
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
					buf := Get(1024)
					assert.NotNil(t, buf)
					Put(buf)
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
				buf := Get(1024)
				for j := range buf {
					buf[j] = byte(j % 256)
				}
				Put(buf)
			}()
		}

		wg.Wait()
	})

	t.Run("CustomPoolConcurrent", func(t *testing.T) {
		pool := NewPool(&Config{
			SmallSize:  512,
			MediumSize: 4096,
			LargeSize:  32768,
		})

		const numGoroutines = 10
		var wg sync.WaitGroup
		wg.Add(numGoroutines)

		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < 50; j++ {
					buf := pool.Get(256)
					pool.Put(buf)
				}
			}()
		}

		wg.Wait()
	})
}

// ============================================================================
// Benchmark Tests
// ============================================================================

func BenchmarkGet(b *testing.B) {
	b.Run("Small", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			buf := Get(1024)
			Put(buf)
		}
	})

	b.Run("Medium", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			buf := Get(32 * 1024)
			Put(buf)
		}
	})

	b.Run("Large", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			buf := Get(512 * 1024)
			Put(buf)
		}
	})
}

func BenchmarkGetParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := Get(1024)
			Put(buf)
		}
	})
}
