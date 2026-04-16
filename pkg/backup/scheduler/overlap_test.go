package scheduler

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOverlapGuard_TryLock — T1: first TryLock on a key succeeds, a second TryLock
// while the first is still held fails, and after unlock the key is free again (T3).
func TestOverlapGuard_TryLock(t *testing.T) {
	g := NewOverlapGuard()

	unlock, ok := g.TryLock("r1")
	require.True(t, ok, "first TryLock should succeed")
	require.NotNil(t, unlock, "unlock func must not be nil on success")

	// Second attempt while locked returns (nil, false).
	unlock2, ok2 := g.TryLock("r1")
	require.False(t, ok2, "second TryLock on same key while held should fail")
	require.Nil(t, unlock2, "unlock func must be nil on failure")

	// After unlock, next TryLock succeeds again (T3).
	unlock()
	unlock3, ok3 := g.TryLock("r1")
	require.True(t, ok3, "TryLock should succeed after unlock")
	require.NotNil(t, unlock3)
	unlock3()
}

// TestOverlapGuard_ExclusivePerKey — T2: different keys are independent;
// both can hold the lock simultaneously.
func TestOverlapGuard_ExclusivePerKey(t *testing.T) {
	tests := []struct {
		name         string
		repoA, repoB string
		wantSecondOK bool
	}{
		{"same key blocks", "r1", "r1", false},
		{"different keys independent", "r1", "r2", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewOverlapGuard()

			unlockA, okA := g.TryLock(tc.repoA)
			require.True(t, okA)
			t.Cleanup(unlockA)

			unlockB, okB := g.TryLock(tc.repoB)
			require.Equal(t, tc.wantSecondOK, okB)
			if okB {
				t.Cleanup(unlockB)
			} else {
				require.Nil(t, unlockB)
			}
		})
	}
}

// TestOverlapGuard_Concurrent100 — T4: 100 parallel TryLock calls on the same
// key produce exactly one winner while that winner is still holding the lock.
// Exercises the race detector.
//
// The winner must hold the lock until all goroutines have attempted to acquire,
// otherwise a goroutine that started late could still legitimately succeed
// after an earlier winner released — which would violate the "exactly one
// winner" invariant we're asserting.
func TestOverlapGuard_Concurrent100(t *testing.T) {
	const n = 100
	g := NewOverlapGuard()

	var wg sync.WaitGroup
	var successes atomic.Int64
	var attempts atomic.Int64
	start := make(chan struct{})
	holdUntilAllTried := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unlock, ok := g.TryLock("same")
			attempts.Add(1)
			if ok {
				successes.Add(1)
				// Hold the lock until every goroutine has attempted acquisition,
				// guaranteeing the invariant "exactly one winner while contended".
				<-holdUntilAllTried
				unlock()
			}
		}()
	}
	close(start) // fire the starting gun

	// Wait until all goroutines have completed their TryLock attempt.
	// The winner is spinning on <-holdUntilAllTried; the losers returned
	// immediately with (nil, false). All of them increment attempts.
	deadline := time.Now().Add(2 * time.Second)
	for attempts.Load() < int64(n) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, int64(n), attempts.Load(), "all goroutines should have attempted TryLock")

	close(holdUntilAllTried)
	wg.Wait()

	require.Equal(t, int64(1), successes.Load(), "expected exactly 1 winner out of %d", n)
}

// TestOverlapGuard_Concurrent_DifferentKeys — every goroutine gets its own key;
// all must succeed (independent per-key mutexes).
func TestOverlapGuard_Concurrent_DifferentKeys(t *testing.T) {
	const n = 50
	g := NewOverlapGuard()

	var wg sync.WaitGroup
	var successes atomic.Int64
	start := make(chan struct{})

	keys := make([]string, n)
	for i := 0; i < n; i++ {
		// Produce n genuinely unique keys, not collisions modulo 26 or 10.
		keys[i] = "repo-unique-" + time.Duration(i).String() + "-" + string(rune('a'+i%26))
	}

	for i := 0; i < n; i++ {
		key := keys[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			unlock, ok := g.TryLock(key)
			if ok {
				successes.Add(1)
				unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, int64(n), successes.Load(), "each unique key should acquire independently")
}

// TestOverlapGuard_ReacquireAfterUnlock — acquire, unlock, reacquire several
// times to verify per-key state cleanup. Documents that the returned unlock
// func is safe to call exactly once per acquisition (sync.Mutex contract).
func TestOverlapGuard_ReacquireAfterUnlock(t *testing.T) {
	g := NewOverlapGuard()
	for i := 0; i < 5; i++ {
		unlock, ok := g.TryLock("cycle")
		require.True(t, ok, "iteration %d: TryLock should succeed", i)
		unlock()
	}
}
