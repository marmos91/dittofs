package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSequenceWindow_NewCreatesWithZeroAvailable(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	assert.Equal(t, uint64(1), w.Size(), "new window should have size 1 (sequence {0})")
}

func TestSequenceWindow_ConsumeZeroSucceeds(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	ok := w.Consume(0, 1)
	assert.True(t, ok, "consuming sequence 0 should succeed")
}

func TestSequenceWindow_ConsumeDuplicateRejected(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	ok := w.Consume(0, 1)
	require.True(t, ok)

	ok = w.Consume(0, 1)
	assert.False(t, ok, "duplicate sequence 0 should be rejected")
}

func TestSequenceWindow_ConsumeOutOfRangeRejected(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	ok := w.Consume(5, 1)
	assert.False(t, ok, "out-of-range sequence 5 should be rejected (window only has {0})")
}

func TestSequenceWindow_GrantExpandsWindow(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	// Consume sequence 0 first
	ok := w.Consume(0, 1)
	require.True(t, ok)

	// Grant 10 more sequences (1-10 become available)
	w.Grant(10)

	// All sequences 1-10 should be consumable
	for i := uint64(1); i <= 10; i++ {
		ok = w.Consume(i, 1)
		assert.True(t, ok, "sequence %d should be consumable after grant", i)
	}
}

func TestSequenceWindow_MultiCreditConsume(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	// Consume 0, then grant 10 more
	w.Consume(0, 1)
	w.Grant(10)

	// Multi-credit: consume sequences 5, 6, 7
	ok := w.Consume(5, 3)
	assert.True(t, ok, "multi-credit consume of 5,6,7 should succeed")

	// Each of those sequences should now be unavailable
	ok = w.Consume(5, 1)
	assert.False(t, ok, "sequence 5 should be unavailable after multi-credit consume")
	ok = w.Consume(6, 1)
	assert.False(t, ok, "sequence 6 should be unavailable after multi-credit consume")
	ok = w.Consume(7, 1)
	assert.False(t, ok, "sequence 7 should be unavailable after multi-credit consume")

	// Sequences outside the range should still be available
	ok = w.Consume(4, 1)
	assert.True(t, ok, "sequence 4 should still be available")
	ok = w.Consume(8, 1)
	assert.True(t, ok, "sequence 8 should still be available")
}

func TestSequenceWindow_MultiCreditConsumeFailsIfAnyUnavailable(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	w.Consume(0, 1)
	w.Grant(10)

	// Consume sequence 6 individually
	ok := w.Consume(6, 1)
	require.True(t, ok)

	// Multi-credit consume of 5,6,7 should fail because 6 is already consumed
	ok = w.Consume(5, 3)
	assert.False(t, ok, "multi-credit should fail when any sequence is unavailable")

	// Verify 5 and 7 are still available (atomic failure — no partial consume)
	ok = w.Consume(5, 1)
	assert.True(t, ok, "sequence 5 should still be available after failed multi-credit")
	ok = w.Consume(7, 1)
	assert.True(t, ok, "sequence 7 should still be available after failed multi-credit")
}

func TestSequenceWindow_CreditChargeZeroTreatedAsOne(t *testing.T) {
	w := NewCommandSequenceWindow(131070)

	// CreditCharge=0 should be treated as CreditCharge=1
	ok := w.Consume(0, 0)
	assert.True(t, ok, "creditCharge=0 should be treated as 1 and succeed for sequence 0")

	// Sequence 0 should now be consumed
	ok = w.Consume(0, 1)
	assert.False(t, ok, "sequence 0 should be consumed after creditCharge=0 consume")
}

func TestSequenceWindow_LowWatermarkAdvances(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	// Consume 0
	w.Consume(0, 1)
	// Grant a large block
	w.Grant(200)

	// Consume first 128 sequences (1-128) — this fills two bitmap words
	for i := uint64(1); i <= 128; i++ {
		ok := w.Consume(i, 1)
		require.True(t, ok, "sequence %d should be consumable", i)
	}

	// Window should have compacted — low watermark should advance
	// Sequence 129 should still be available
	ok := w.Consume(129, 1)
	assert.True(t, ok, "sequence 129 should be consumable after watermark advance")
}

func TestSequenceWindow_MaxSizeCap(t *testing.T) {
	maxSize := uint64(100)
	w := NewCommandSequenceWindow(maxSize)
	w.Consume(0, 1)

	// Grant way more than maxSize
	w.Grant(200)

	// Size should be capped at maxSize
	assert.LessOrEqual(t, w.Size(), maxSize, "window size should not exceed maxSize")
}

func TestSequenceWindow_GrantAfterHeavyConsumption(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	w.Consume(0, 1)

	// Grant and consume in rounds
	for round := 0; round < 5; round++ {
		w.Grant(50)
		for i := uint64(0); i < 50; i++ {
			seq := uint64(round)*50 + i + 1
			ok := w.Consume(seq, 1)
			assert.True(t, ok, "round %d seq %d should be consumable", round, seq)
		}
	}

	// Grant one more batch
	w.Grant(10)
	// The new batch should be consumable
	baseSeq := uint64(5*50 + 1)
	for i := uint64(0); i < 10; i++ {
		ok := w.Consume(baseSeq+i, 1)
		assert.True(t, ok, "post-heavy-consumption seq %d should be consumable", baseSeq+i)
	}
}

func TestSequenceWindow_ConcurrentConsumeAndGrant(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	w.Consume(0, 1)

	const goroutines = 50
	const grantsPerGoroutine = 100

	// Pre-grant enough sequences for all goroutines
	w.Grant(uint16(goroutines * grantsPerGoroutine))

	var wg sync.WaitGroup
	consumed := make([]bool, goroutines*grantsPerGoroutine)
	var mu sync.Mutex

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < grantsPerGoroutine; i++ {
				seq := uint64(gid*grantsPerGoroutine + i + 1) // +1 because 0 is consumed
				ok := w.Consume(seq, 1)
				if ok {
					mu.Lock()
					consumed[seq-1] = true
					mu.Unlock()
				}
			}
		}(g)
	}

	wg.Wait()

	// Each sequence should have been consumed exactly once
	successCount := 0
	for _, c := range consumed {
		if c {
			successCount++
		}
	}
	assert.Equal(t, goroutines*grantsPerGoroutine, successCount,
		"all %d sequences should be consumed exactly once", goroutines*grantsPerGoroutine)
}

func TestSequenceWindow_SizeReturnsCorrectValue(t *testing.T) {
	w := NewCommandSequenceWindow(131070)
	assert.Equal(t, uint64(1), w.Size(), "initial window should have size 1")

	w.Grant(10)
	assert.Equal(t, uint64(11), w.Size(), "after granting 10, size should be 11")

	w.Consume(0, 1)
	// Size reflects the full window range, not just available slots
	// After consuming, size may reduce if low watermark advances
	sz := w.Size()
	assert.Greater(t, sz, uint64(0), "size should still be > 0 after consuming one")
}
