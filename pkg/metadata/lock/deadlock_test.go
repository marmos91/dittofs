package lock

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Wait-For Graph Tests
// ============================================================================

// wouldCauseCycle answers the same question as TryAddWaiter without recording
// any edges, so a test can assert the predicate repeatedly against a fixed graph.
func (wfg *WaitForGraph) wouldCauseCycle(waiter string, owners []string) bool {
	wfg.mu.RLock()
	defer wfg.mu.RUnlock()
	return wfg.wouldCauseCycleLocked(waiter, owners)
}

func TestWaitForGraph_NewWaitForGraph(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	require.NotNil(t, wfg)
	assert.NotNil(t, wfg.edges)
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_SimpleCycle(t *testing.T) {
	t.Parallel()

	// A waits for B, then B wants to wait for A (deadlock)
	wfg := NewWaitForGraph()

	// A is waiting for B
	wfg.TryAddWaiter("A", []string{"B"})

	// B wants to wait for A - this would create cycle A -> B -> A
	cycle := wfg.wouldCauseCycle("B", []string{"A"})

	assert.True(t, cycle, "Should detect simple A->B->A cycle")
}

func TestWaitForGraph_Chain_NoCycle(t *testing.T) {
	t.Parallel()

	// A waits for B, B waits for C - no cycle
	wfg := NewWaitForGraph()

	// A is waiting for B
	wfg.TryAddWaiter("A", []string{"B"})

	// B wants to wait for C - no cycle (A -> B -> C)
	cycle := wfg.wouldCauseCycle("B", []string{"C"})
	assert.False(t, cycle, "Chain should not be a cycle")

	// Add B waiting for C
	wfg.TryAddWaiter("B", []string{"C"})
	assert.Equal(t, 2, wfg.Size())
}

func TestWaitForGraph_TriangleCycle(t *testing.T) {
	t.Parallel()

	// A -> B -> C, then C wants to wait for A (triangle cycle)
	wfg := NewWaitForGraph()

	wfg.TryAddWaiter("A", []string{"B"})
	wfg.TryAddWaiter("B", []string{"C"})

	// C wants to wait for A - creates A -> B -> C -> A
	cycle := wfg.wouldCauseCycle("C", []string{"A"})

	assert.True(t, cycle, "Should detect triangle cycle A->B->C->A")
}

func TestWaitForGraph_ComplexGraph_NoCycle(t *testing.T) {
	t.Parallel()

	// Complex DAG without cycles:
	//   A -> B
	//   A -> C
	//   B -> D
	//   C -> D
	wfg := NewWaitForGraph()

	wfg.TryAddWaiter("A", []string{"B", "C"})
	wfg.TryAddWaiter("B", []string{"D"})
	wfg.TryAddWaiter("C", []string{"D"})

	// E wants to wait for A - no cycle possible
	cycle := wfg.wouldCauseCycle("E", []string{"A"})
	assert.False(t, cycle, "DAG should not have cycle")

	// D wants to wait for E - still no cycle
	cycle = wfg.wouldCauseCycle("D", []string{"E"})
	assert.False(t, cycle, "Still no cycle")
}

func TestWaitForGraph_ComplexGraph_WithCycle(t *testing.T) {
	t.Parallel()

	// Same DAG, but now D wants to wait for A (creates cycle)
	//   A -> B -> D
	//   A -> C -> D
	// D -> A creates multiple cycles
	wfg := NewWaitForGraph()

	wfg.TryAddWaiter("A", []string{"B", "C"})
	wfg.TryAddWaiter("B", []string{"D"})
	wfg.TryAddWaiter("C", []string{"D"})

	// D wants to wait for A - cycle through B and C
	cycle := wfg.wouldCauseCycle("D", []string{"A"})
	assert.True(t, cycle, "Should detect cycle through complex graph")
}

func TestWaitForGraph_MultipleOwners(t *testing.T) {
	t.Parallel()

	// A wants to wait for both B and C
	wfg := NewWaitForGraph()

	wfg.TryAddWaiter("A", []string{"B", "C"})

	// B has no other waits
	// C wants to wait for A - creates cycle through C
	cycle := wfg.wouldCauseCycle("C", []string{"A"})
	assert.True(t, cycle, "Should detect cycle even with multiple owners")
}

func TestWaitForGraph_RemoveWaiter_BreaksCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Create potential deadlock
	wfg.TryAddWaiter("A", []string{"B"})

	// Check that B->A would cause cycle
	assert.True(t, wfg.wouldCauseCycle("B", []string{"A"}))

	// Remove A as waiter (e.g., A's request times out)
	wfg.RemoveWaiter("A")

	// Now B->A should not cause cycle
	assert.False(t, wfg.wouldCauseCycle("B", []string{"A"}))
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_RemoveOwner_BreaksCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A -> B -> C
	wfg.TryAddWaiter("A", []string{"B"})
	wfg.TryAddWaiter("B", []string{"C"})

	// C -> A would create cycle
	assert.True(t, wfg.wouldCauseCycle("C", []string{"A"}))

	// Remove B (e.g., B releases lock or disconnects)
	wfg.RemoveOwner("B")

	// C -> A should not cause cycle now (A is waiting for nothing)
	assert.False(t, wfg.wouldCauseCycle("C", []string{"A"}))

	// A's wait for B should also be gone
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_RemoveOwner_PartialRemoval(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A waits for B and C
	wfg.TryAddWaiter("A", []string{"B", "C"})

	// Remove B
	wfg.RemoveOwner("B")

	// A should still be waiting for C
	assert.Equal(t, 1, wfg.Size())

	// D -> A still no cycle (A -> C only, no path back)
	assert.False(t, wfg.wouldCauseCycle("D", []string{"A"}))

	// C -> A should cause cycle (A -> C -> A)
	assert.True(t, wfg.wouldCauseCycle("C", []string{"A"}))
}

func TestWaitForGraph_GetWaitersFor(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A, B, C all waiting for D
	wfg.TryAddWaiter("A", []string{"D"})
	wfg.TryAddWaiter("B", []string{"D"})
	wfg.TryAddWaiter("C", []string{"D", "E"}) // C also waits for E

	waiters := wfg.GetWaitersFor("D")

	assert.Len(t, waiters, 3)
	assert.Contains(t, waiters, "A")
	assert.Contains(t, waiters, "B")
	assert.Contains(t, waiters, "C")
}

func TestWaitForGraph_GetWaitersFor_NoWaiters(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	wfg.TryAddWaiter("A", []string{"B"})

	waiters := wfg.GetWaitersFor("C") // No one waiting for C

	assert.Nil(t, waiters)
}

func TestWaitForGraph_EmptyGraph(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// No cycles in empty graph
	assert.False(t, wfg.wouldCauseCycle("A", []string{"B"}))
	assert.False(t, wfg.wouldCauseCycle("A", []string{}))
	assert.False(t, wfg.wouldCauseCycle("", []string{"A"}))

	// Safe to remove non-existent entries
	wfg.RemoveWaiter("X")
	wfg.RemoveOwner("Y")

	assert.Nil(t, wfg.GetWaitersFor("Z"))
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_TryAddWaiter_EmptyOwners(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Adding with empty owners should be no-op
	wfg.TryAddWaiter("A", []string{})
	wfg.TryAddWaiter("A", nil)

	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_SelfCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A wants to wait for A (immediate cycle)
	cycle := wfg.wouldCauseCycle("A", []string{"A"})

	// This isn't detected by our DFS because A has no edges yet
	// This is a degenerate case - in practice, A shouldn't wait for itself
	// The lock manager should prevent this at a higher level
	assert.False(t, cycle, "Self-cycle check depends on existing edges")

	// But if A is already waiting for B...
	wfg.TryAddWaiter("A", []string{"B"})
	// ...and B tries to wait for A
	cycle = wfg.wouldCauseCycle("B", []string{"A"})
	assert.True(t, cycle)
}

func TestWaitForGraph_LongChain(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Create long chain: owner0 -> owner1 -> owner2 -> ... -> owner99
	// Use numeric IDs to avoid character overflow
	for i := 0; i < 99; i++ {
		wfg.TryAddWaiter(
			string(rune('0'+i/10))+string(rune('0'+i%10)),                     // "00", "01", ..., "98"
			[]string{string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10))}, // "01", "02", ..., "99"
		)
	}

	// Check that a non-participant doesn't create a cycle
	assert.False(t, wfg.wouldCauseCycle("XX", []string{"00"}))

	// Creating cycle at the end: "99" -> "00" would create cycle
	cycle := wfg.wouldCauseCycle("99", []string{"00"})
	assert.True(t, cycle, "Should detect cycle in long chain")
}

// ============================================================================
// Concurrency Tests
// ============================================================================

func TestWaitForGraph_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()
	const numGoroutines = 50
	const numOps = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()

			ownerID := string(rune('A' + (id % 26)))
			targetID := string(rune('A' + ((id + 1) % 26)))

			for j := 0; j < numOps; j++ {
				// Mix of operations
				switch j % 5 {
				case 0:
					wfg.wouldCauseCycle(ownerID, []string{targetID})
				case 1:
					wfg.TryAddWaiter(ownerID, []string{targetID})
				case 2:
					wfg.RemoveWaiter(ownerID)
				case 3:
					wfg.RemoveOwner(ownerID)
				case 4:
					wfg.GetWaitersFor(targetID)
				}
			}
		}(i)
	}

	wg.Wait()
	// If we get here without panic or deadlock, concurrency is working
}

func TestWaitForGraph_ConcurrentCycleDetection(t *testing.T) {
	t.Parallel()

	// Test that concurrent cycle detection is correct
	for iteration := 0; iteration < 100; iteration++ {
		wfg := NewWaitForGraph()

		// Set up A -> B
		wfg.TryAddWaiter("A", []string{"B"})

		var wg sync.WaitGroup
		results := make(chan bool, 10)

		// Multiple goroutines checking B -> A cycle
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- wfg.wouldCauseCycle("B", []string{"A"})
			}()
		}

		wg.Wait()
		close(results)

		// All should detect cycle
		for result := range results {
			assert.True(t, result, "All goroutines should detect cycle")
		}
	}
}

// TestWaitForGraph_TryAddWaiter_ConcurrentRingCannotCloseCycle fires a full ring
// of wait edges (owner i waits on owner i+1, the last on the first) at once.
// Every edge is cycle-free against the empty graph, so a separate check call
// followed by a separate add call could admit all of them and leave a closed
// ring behind. TryAddWaiter holds one write lock across check and insert, so
// whichever request the mutex orders last sees a path back to itself and is
// refused. The assertions are exact, so the test never flakes on correct code;
// the iterations only raise the odds of catching a regression.
func TestWaitForGraph_TryAddWaiter_ConcurrentRingCannotCloseCycle(t *testing.T) {
	t.Parallel()

	const ringSize = 8
	owners := make([]string, ringSize)
	for i := range owners {
		owners[i] = string(rune('A' + i))
	}

	for iteration := 0; iteration < 300; iteration++ {
		wfg := NewWaitForGraph()

		start := make(chan struct{})
		results := make([]bool, ringSize)
		var wg sync.WaitGroup
		for i := 0; i < ringSize; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i] = wfg.TryAddWaiter(owners[i], []string{owners[(i+1)%ringSize]})
			}(i)
		}
		close(start)
		wg.Wait()

		refused := 0
		for _, granted := range results {
			if !granted {
				refused++
			}
		}
		require.GreaterOrEqual(t, refused, 1,
			"a full ring of wait edges is a deadlock; at least one request must be refused")

		// The surviving graph must be acyclic: no owner can reach itself.
		for _, owner := range owners {
			assert.False(t, wfg.wouldCauseCycle(owner, []string{owner}),
				"owner %s is waiting on itself transitively — a cycle was committed", owner)
		}
	}
}

// The ring test above asserts acyclicity with wouldCauseCycle(x, []string{x}).
// That reads like it only catches a self-edge, because canReach has no
// from == to base case. It does catch a multi-node ring: canReach checks
// waitSet[to] directly at each hop, so the check fires when the DFS reaches
// the node whose edge points back at the origin. visited only stops a node
// being expanded twice, it does not stop that node matching as a target.
// Pinned here so the assertion is not later weakened into a self-edge check.
func TestWouldCauseCycleDetectsRingWithoutSelfEdge(t *testing.T) {
	wfg := NewWaitForGraph()

	// Build A->B->C->A directly, bypassing the entry point that refuses
	// cycles, so the graph genuinely holds one.
	wfg.mu.Lock()
	for _, e := range [][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}} {
		if wfg.edges[e[0]] == nil {
			wfg.edges[e[0]] = make(map[string]struct{})
		}
		wfg.edges[e[0]][e[1]] = struct{}{}
	}
	wfg.mu.Unlock()

	for _, owner := range []string{"A", "B", "C"} {
		assert.True(t, wfg.wouldCauseCycle(owner, []string{owner}),
			"ring member %s must be reported as reaching itself", owner)
	}

	// An acyclic chain must not report a cycle.
	chain := NewWaitForGraph()
	chain.mu.Lock()
	chain.edges["X"] = map[string]struct{}{"Y": {}}
	chain.edges["Y"] = map[string]struct{}{"Z": {}}
	chain.mu.Unlock()

	for _, owner := range []string{"X", "Y", "Z"} {
		assert.False(t, chain.wouldCauseCycle(owner, []string{owner}),
			"acyclic chain member %s must not be reported as cyclic", owner)
	}
}
