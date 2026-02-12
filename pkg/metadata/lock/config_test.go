package lock

import (
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Config Tests
// ============================================================================

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	assert.Equal(t, 1000, cfg.MaxLocksPerFile)
	assert.Equal(t, 10000, cfg.MaxLocksPerClient)
	assert.Equal(t, 100000, cfg.MaxTotalLocks)
	assert.Equal(t, 60*time.Second, cfg.BlockingTimeout)
	assert.Equal(t, 90*time.Second, cfg.GracePeriodDuration)
	assert.False(t, cfg.MandatoryLocking)
}

func TestConfig_CustomValues(t *testing.T) {
	t.Parallel()

	cfg := Config{
		MaxLocksPerFile:     500,
		MaxLocksPerClient:   5000,
		MaxTotalLocks:       50000,
		BlockingTimeout:     30 * time.Second,
		GracePeriodDuration: 120 * time.Second,
		MandatoryLocking:    true,
	}

	assert.Equal(t, 500, cfg.MaxLocksPerFile)
	assert.Equal(t, 5000, cfg.MaxLocksPerClient)
	assert.Equal(t, 50000, cfg.MaxTotalLocks)
	assert.Equal(t, 30*time.Second, cfg.BlockingTimeout)
	assert.Equal(t, 120*time.Second, cfg.GracePeriodDuration)
	assert.True(t, cfg.MandatoryLocking)
}

// ============================================================================
// Limits Tests
// ============================================================================

func TestNewLimits(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	require.NotNil(t, ll)
	assert.NotNil(t, ll.locksByFile)
	assert.NotNil(t, ll.locksByClient)
	assert.Equal(t, 0, ll.totalLocks)
}

func TestLimits_CheckLimits_UnderLimits(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := DefaultConfig()

	// Should pass when under all limits
	err := ll.CheckLimits(cfg, "file1", "client1")
	assert.NoError(t, err)
}

func TestLimits_CheckLimits_FileLimitExceeded(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := Config{
		MaxLocksPerFile:   3,
		MaxLocksPerClient: 100,
		MaxTotalLocks:     100,
	}

	// Acquire 3 locks on same file
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file1", "client2")
	ll.IncrementCounts("file1", "client3")

	// 4th lock should fail
	err := ll.CheckLimits(cfg, "file1", "client4")
	require.Error(t, err)

	var storeErr *errors.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, errors.ErrLockLimitExceeded, storeErr.Code)
	assert.Contains(t, storeErr.Message, "per-file")
}

func TestLimits_CheckLimits_ClientLimitExceeded(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := Config{
		MaxLocksPerFile:   100,
		MaxLocksPerClient: 3,
		MaxTotalLocks:     100,
	}

	// Acquire 3 locks for same client
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file2", "client1")
	ll.IncrementCounts("file3", "client1")

	// 4th lock should fail
	err := ll.CheckLimits(cfg, "file4", "client1")
	require.Error(t, err)

	var storeErr *errors.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, errors.ErrLockLimitExceeded, storeErr.Code)
	assert.Contains(t, storeErr.Message, "per-client")
}

func TestLimits_CheckLimits_TotalLimitExceeded(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := Config{
		MaxLocksPerFile:   100,
		MaxLocksPerClient: 100,
		MaxTotalLocks:     3,
	}

	// Acquire 3 total locks
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file2", "client2")
	ll.IncrementCounts("file3", "client3")

	// 4th lock should fail
	err := ll.CheckLimits(cfg, "file4", "client4")
	require.Error(t, err)

	var storeErr *errors.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, errors.ErrLockLimitExceeded, storeErr.Code)
	assert.Contains(t, storeErr.Message, "total")
}

func TestLimits_CheckLimits_DisabledLimits(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := Config{
		MaxLocksPerFile:   0, // Disabled
		MaxLocksPerClient: 0, // Disabled
		MaxTotalLocks:     0, // Disabled
	}

	// Acquire many locks
	for i := 0; i < 1000; i++ {
		ll.IncrementCounts("file1", "client1")
	}

	// Should still pass - limits are disabled
	err := ll.CheckLimits(cfg, "file1", "client1")
	assert.NoError(t, err)
}

func TestLimits_IncrementDecrement(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Increment
	ll.IncrementCounts("file1", "client1")
	assert.Equal(t, 1, ll.GetFileCount("file1"))
	assert.Equal(t, 1, ll.GetClientCount("client1"))
	assert.Equal(t, 1, ll.GetTotalCount())

	// Increment same file, different client
	ll.IncrementCounts("file1", "client2")
	assert.Equal(t, 2, ll.GetFileCount("file1"))
	assert.Equal(t, 1, ll.GetClientCount("client2"))
	assert.Equal(t, 2, ll.GetTotalCount())

	// Decrement
	ll.DecrementCounts("file1", "client1")
	assert.Equal(t, 1, ll.GetFileCount("file1"))
	assert.Equal(t, 0, ll.GetClientCount("client1"))
	assert.Equal(t, 1, ll.GetTotalCount())

	// Decrement last
	ll.DecrementCounts("file1", "client2")
	assert.Equal(t, 0, ll.GetFileCount("file1"))
	assert.Equal(t, 0, ll.GetClientCount("client2"))
	assert.Equal(t, 0, ll.GetTotalCount())
}

func TestLimits_DecrementBelowZero(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Decrement without any increments should not go negative
	ll.DecrementCounts("file1", "client1")
	ll.DecrementCounts("file1", "client1")

	assert.Equal(t, 0, ll.GetFileCount("file1"))
	assert.Equal(t, 0, ll.GetClientCount("client1"))
	assert.Equal(t, 0, ll.GetTotalCount())
}

func TestLimits_DecrementCountsN(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Setup: client has 5 locks on same file
	for i := 0; i < 5; i++ {
		ll.IncrementCounts("file1", "client1")
	}
	assert.Equal(t, 5, ll.GetFileCount("file1"))
	assert.Equal(t, 5, ll.GetClientCount("client1"))
	assert.Equal(t, 5, ll.GetTotalCount())

	// Decrement 3 at once
	ll.DecrementCountsN("file1", "client1", 3)
	assert.Equal(t, 2, ll.GetFileCount("file1"))
	assert.Equal(t, 2, ll.GetClientCount("client1"))
	assert.Equal(t, 2, ll.GetTotalCount())

	// Decrement more than remaining (should go to 0, not negative)
	ll.DecrementCountsN("file1", "client1", 10)
	assert.Equal(t, 0, ll.GetFileCount("file1"))
	assert.Equal(t, 0, ll.GetClientCount("client1"))
	assert.Equal(t, 0, ll.GetTotalCount())
}

func TestLimits_DecrementCountsN_ZeroOrNegative(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	ll.IncrementCounts("file1", "client1")

	// Zero count should be no-op
	ll.DecrementCountsN("file1", "client1", 0)
	assert.Equal(t, 1, ll.GetFileCount("file1"))

	// Negative count should be no-op
	ll.DecrementCountsN("file1", "client1", -5)
	assert.Equal(t, 1, ll.GetFileCount("file1"))
}

func TestLimits_DecrementCountsN_PartialKeys(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Setup
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file1", "client1")

	// Decrement only client (empty file handle)
	ll.DecrementCountsN("", "client1", 2)
	assert.Equal(t, 3, ll.GetFileCount("file1")) // Unchanged
	assert.Equal(t, 1, ll.GetClientCount("client1"))
	assert.Equal(t, 1, ll.GetTotalCount())
}

func TestLimits_GetStats(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Setup: various locks
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file1", "client2")
	ll.IncrementCounts("file2", "client1")
	ll.IncrementCounts("file3", "client3")

	stats := ll.GetStats()

	assert.Equal(t, 5, stats.TotalLocks)
	assert.Equal(t, 3, stats.UniqueFiles)
	assert.Equal(t, 3, stats.UniqueClients)
	assert.Equal(t, 3, stats.MaxLocksOnFile)    // file1 has 3 locks
	assert.Equal(t, 3, stats.MaxLocksForClient) // client1 has 3 locks
}

func TestLimits_Reset(t *testing.T) {
	t.Parallel()

	ll := NewLimits()

	// Setup
	ll.IncrementCounts("file1", "client1")
	ll.IncrementCounts("file2", "client2")

	// Reset
	ll.Reset()

	assert.Equal(t, 0, ll.GetFileCount("file1"))
	assert.Equal(t, 0, ll.GetClientCount("client1"))
	assert.Equal(t, 0, ll.GetTotalCount())

	stats := ll.GetStats()
	assert.Equal(t, 0, stats.TotalLocks)
	assert.Equal(t, 0, stats.UniqueFiles)
	assert.Equal(t, 0, stats.UniqueClients)
}

// ============================================================================
// Concurrency Tests
// ============================================================================

func TestLimits_ConcurrentIncrementDecrement(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	const numGoroutines = 100
	const numOpsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines * 2) // Half increment, half decrement

	// Start incrementers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			file := "file" + string(rune('A'+id%10))
			client := "client" + string(rune('A'+id%10))
			for j := 0; j < numOpsPerGoroutine; j++ {
				ll.IncrementCounts(file, client)
			}
		}(i)
	}

	// Start decrementers
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			file := "file" + string(rune('A'+id%10))
			client := "client" + string(rune('A'+id%10))
			for j := 0; j < numOpsPerGoroutine; j++ {
				ll.DecrementCounts(file, client)
			}
		}(i)
	}

	wg.Wait()
	// If we get here without panic or deadlock, concurrency is working
}

func TestLimits_ConcurrentCheckLimits(t *testing.T) {
	t.Parallel()

	ll := NewLimits()
	cfg := DefaultConfig()

	const numGoroutines = 100
	const numOpsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			file := "file" + string(rune('A'+id%10))
			client := "client" + string(rune('A'+id%10))
			for j := 0; j < numOpsPerGoroutine; j++ {
				_ = ll.CheckLimits(cfg, file, client)
				ll.IncrementCounts(file, client)
				ll.GetStats()
				ll.DecrementCounts(file, client)
			}
		}(i)
	}

	wg.Wait()
}
