package handlers

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

// ============================================================================
// Normal Addition Tests
// ============================================================================

func TestSafeAddNormalCases(t *testing.T) {
	t.Run("AddZeroToZero", func(t *testing.T) {
		sum, overflow := safeAdd(0, 0)
		assert.Equal(t, uint64(0), sum)
		assert.False(t, overflow)
	})

	t.Run("AddSmallNumbers", func(t *testing.T) {
		sum, overflow := safeAdd(42, 58)
		assert.Equal(t, uint64(100), sum)
		assert.False(t, overflow)
	})

	t.Run("AddLargeNumbersNoOverflow", func(t *testing.T) {
		sum, overflow := safeAdd(1000000000, 2000000000)
		assert.Equal(t, uint64(3000000000), sum)
		assert.False(t, overflow)
	})

	t.Run("AddZeroToNumber", func(t *testing.T) {
		sum, overflow := safeAdd(12345, 0)
		assert.Equal(t, uint64(12345), sum)
		assert.False(t, overflow)
	})

	t.Run("AddNumberToZero", func(t *testing.T) {
		sum, overflow := safeAdd(0, 67890)
		assert.Equal(t, uint64(67890), sum)
		assert.False(t, overflow)
	})

	t.Run("AddOneToOne", func(t *testing.T) {
		sum, overflow := safeAdd(1, 1)
		assert.Equal(t, uint64(2), sum)
		assert.False(t, overflow)
	})
}

// ============================================================================
// Overflow Detection Tests
// ============================================================================

func TestSafeAddOverflow(t *testing.T) {
	t.Run("OverflowWithMaxUint64", func(t *testing.T) {
		sum, overflow := safeAdd(math.MaxUint64, 1)
		assert.Equal(t, uint64(0), sum) // Wraps to 0
		assert.True(t, overflow)
	})

	t.Run("OverflowWithTwoLargeNumbers", func(t *testing.T) {
		// Two numbers that sum beyond MaxUint64
		a := uint64(math.MaxUint64 - 100)
		b := uint64(200)
		sum, overflow := safeAdd(a, b)

		// Should wrap around
		assert.Equal(t, uint64(99), sum)
		assert.True(t, overflow)
	})

	t.Run("OverflowWithMaxPlusMax", func(t *testing.T) {
		sum, overflow := safeAdd(math.MaxUint64, math.MaxUint64)
		// MaxUint64 + MaxUint64 = 2*MaxUint64 = MaxUint64 - 1 (wrapped)
		assert.Equal(t, uint64(math.MaxUint64-1), sum)
		assert.True(t, overflow)
	})

	t.Run("ExactlyAtBoundaryNoOverflow", func(t *testing.T) {
		// Sum exactly equals MaxUint64
		a := uint64(math.MaxUint64 - 1000)
		b := uint64(1000)
		sum, overflow := safeAdd(a, b)

		assert.Equal(t, uint64(math.MaxUint64), sum)
		assert.False(t, overflow) // Exactly at max, no overflow
	})

	t.Run("JustOverBoundary", func(t *testing.T) {
		// Sum is one over MaxUint64
		a := uint64(math.MaxUint64)
		b := uint64(1)
		sum, overflow := safeAdd(a, b)

		assert.Equal(t, uint64(0), sum) // Wraps to 0
		assert.True(t, overflow)
	})
}

// ============================================================================
// Boundary Value Tests
// ============================================================================

func TestSafeAddBoundaries(t *testing.T) {
	t.Run("AddToMaxUint64ZeroNoOverflow", func(t *testing.T) {
		sum, overflow := safeAdd(math.MaxUint64, 0)
		assert.Equal(t, uint64(math.MaxUint64), sum)
		assert.False(t, overflow)
	})

	t.Run("AddZeroToMaxUint64NoOverflow", func(t *testing.T) {
		sum, overflow := safeAdd(0, math.MaxUint64)
		assert.Equal(t, uint64(math.MaxUint64), sum)
		assert.False(t, overflow)
	})

	t.Run("HalfMaxPlusHalfMaxNoOverflow", func(t *testing.T) {
		half := uint64(math.MaxUint64 / 2)
		sum, overflow := safeAdd(half, half)

		// Two halves should be just under MaxUint64 (due to odd number)
		assert.Equal(t, uint64(math.MaxUint64-1), sum)
		assert.False(t, overflow)
	})

	t.Run("HalfMaxPlusHalfMaxPlusOneOverflow", func(t *testing.T) {
		half := uint64(math.MaxUint64 / 2)
		_, overflow := safeAdd(half+1, half+1)

		// This should overflow
		assert.True(t, overflow)
	})
}

// ============================================================================
// Symmetry Tests
// ============================================================================

func TestSafeAddSymmetry(t *testing.T) {
	t.Run("AdditionIsCommutative", func(t *testing.T) {
		a := uint64(12345)
		b := uint64(67890)

		sum1, overflow1 := safeAdd(a, b)
		sum2, overflow2 := safeAdd(b, a)

		assert.Equal(t, sum1, sum2)
		assert.Equal(t, overflow1, overflow2)
	})

	t.Run("OverflowIsCommutative", func(t *testing.T) {
		a := uint64(math.MaxUint64 - 100)
		b := uint64(200)

		sum1, overflow1 := safeAdd(a, b)
		sum2, overflow2 := safeAdd(b, a)

		assert.Equal(t, sum1, sum2)
		assert.Equal(t, overflow1, overflow2)
		assert.True(t, overflow1)
		assert.True(t, overflow2)
	})
}

// ============================================================================
// Real-World Scenario Tests
// ============================================================================

func TestSafeAddRealWorldScenarios(t *testing.T) {
	t.Run("FileSystemBlockCalculation", func(t *testing.T) {
		// Simulating: file size in bytes + block size
		fileSize := uint64(1024 * 1024 * 1024) // 1 GB
		blockSize := uint64(4096)               // 4 KB

		sum, overflow := safeAdd(fileSize, blockSize)
		assert.False(t, overflow)
		assert.Equal(t, uint64(1073745920), sum)
	})

	t.Run("LargeDiskSpaceCalculation", func(t *testing.T) {
		// Simulating: adding large disk usage values
		disk1 := uint64(5 * 1024 * 1024 * 1024 * 1024) // 5 TB
		disk2 := uint64(3 * 1024 * 1024 * 1024 * 1024) // 3 TB

		sum, overflow := safeAdd(disk1, disk2)
		assert.False(t, overflow)
		assert.Equal(t, uint64(8*1024*1024*1024*1024), sum) // 8 TB
	})

	t.Run("NetworkByteCounterOverflow", func(t *testing.T) {
		// Simulating: network byte counter near limit
		currentCount := uint64(math.MaxUint64 - 1000)
		bytesReceived := uint64(2000)

		_, overflow := safeAdd(currentCount, bytesReceived)
		assert.True(t, overflow)
		// Application should detect this and handle counter wrap
	})

	t.Run("TimestampMillisecondsAddition", func(t *testing.T) {
		// Simulating: adding milliseconds to timestamp
		timestamp := uint64(1700000000000) // Unix timestamp in ms
		delay := uint64(5000)              // 5 seconds

		sum, overflow := safeAdd(timestamp, delay)
		assert.False(t, overflow)
		assert.Equal(t, uint64(1700000005000), sum)
	})
}

// ============================================================================
// Multiple Operation Tests
// ============================================================================

func TestSafeAddChaining(t *testing.T) {
	t.Run("ChainedAdditionsWithoutOverflow", func(t *testing.T) {
		result := uint64(0)
		var overflow bool

		// Add 100 ten times
		for i := 0; i < 10; i++ {
			result, overflow = safeAdd(result, 100)
			assert.False(t, overflow)
		}

		assert.Equal(t, uint64(1000), result)
	})

	t.Run("ChainedAdditionsDetectsOverflow", func(t *testing.T) {
		result := uint64(math.MaxUint64 - 50)
		var overflow bool

		// First addition: OK
		result, overflow = safeAdd(result, 25)
		assert.False(t, overflow)
		assert.Equal(t, uint64(math.MaxUint64-25), result)

		// Second addition: OK
		result, overflow = safeAdd(result, 25)
		assert.False(t, overflow)
		assert.Equal(t, uint64(math.MaxUint64), result)

		// Third addition: OVERFLOW
		result, overflow = safeAdd(result, 1)
		assert.True(t, overflow)
		assert.Equal(t, uint64(0), result)
	})
}

// ============================================================================
// Edge Case Pattern Tests
// ============================================================================

func TestSafeAddPatterns(t *testing.T) {
	t.Run("PowersOfTwo", func(t *testing.T) {
		sum, overflow := safeAdd(1<<32, 1<<32)
		assert.False(t, overflow)
		assert.Equal(t, uint64(1<<33), sum)
	})

	t.Run("LargePowerOfTwoOverflow", func(t *testing.T) {
		// 1 << 63 is half of MaxUint64
		// Adding two of them should exactly equal MaxUint64 + 1 (overflow)
		largeVal := uint64(1 << 63)
		sum, overflow := safeAdd(largeVal, largeVal)
		assert.True(t, overflow)
		assert.Equal(t, uint64(0), sum)
	})

	t.Run("OneBeforePowerOfTwo", func(t *testing.T) {
		sum, overflow := safeAdd((1<<32)-1, 1)
		assert.False(t, overflow)
		assert.Equal(t, uint64(1<<32), sum)
	})
}
