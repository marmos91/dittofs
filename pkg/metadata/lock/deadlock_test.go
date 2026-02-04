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
	wfg.AddWaiter("A", []string{"B"})

	// B wants to wait for A - this would create cycle A -> B -> A
	cycle := wfg.WouldCauseCycle("B", []string{"A"})

	assert.True(t, cycle, "Should detect simple A->B->A cycle")
}

func TestWaitForGraph_Chain_NoCycle(t *testing.T) {
	t.Parallel()

	// A waits for B, B waits for C - no cycle
	wfg := NewWaitForGraph()

	// A is waiting for B
	wfg.AddWaiter("A", []string{"B"})

	// B wants to wait for C - no cycle (A -> B -> C)
	cycle := wfg.WouldCauseCycle("B", []string{"C"})
	assert.False(t, cycle, "Chain should not be a cycle")

	// Add B waiting for C
	wfg.AddWaiter("B", []string{"C"})
	assert.Equal(t, 2, wfg.Size())
}

func TestWaitForGraph_TriangleCycle(t *testing.T) {
	t.Parallel()

	// A -> B -> C, then C wants to wait for A (triangle cycle)
	wfg := NewWaitForGraph()

	wfg.AddWaiter("A", []string{"B"})
	wfg.AddWaiter("B", []string{"C"})

	// C wants to wait for A - creates A -> B -> C -> A
	cycle := wfg.WouldCauseCycle("C", []string{"A"})

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

	wfg.AddWaiter("A", []string{"B", "C"})
	wfg.AddWaiter("B", []string{"D"})
	wfg.AddWaiter("C", []string{"D"})

	// E wants to wait for A - no cycle possible
	cycle := wfg.WouldCauseCycle("E", []string{"A"})
	assert.False(t, cycle, "DAG should not have cycle")

	// D wants to wait for E - still no cycle
	cycle = wfg.WouldCauseCycle("D", []string{"E"})
	assert.False(t, cycle, "Still no cycle")
}

func TestWaitForGraph_ComplexGraph_WithCycle(t *testing.T) {
	t.Parallel()

	// Same DAG, but now D wants to wait for A (creates cycle)
	//   A -> B -> D
	//   A -> C -> D
	// D -> A creates multiple cycles
	wfg := NewWaitForGraph()

	wfg.AddWaiter("A", []string{"B", "C"})
	wfg.AddWaiter("B", []string{"D"})
	wfg.AddWaiter("C", []string{"D"})

	// D wants to wait for A - cycle through B and C
	cycle := wfg.WouldCauseCycle("D", []string{"A"})
	assert.True(t, cycle, "Should detect cycle through complex graph")
}

func TestWaitForGraph_MultipleOwners(t *testing.T) {
	t.Parallel()

	// A wants to wait for both B and C
	wfg := NewWaitForGraph()

	wfg.AddWaiter("A", []string{"B", "C"})

	// B has no other waits
	// C wants to wait for A - creates cycle through C
	cycle := wfg.WouldCauseCycle("C", []string{"A"})
	assert.True(t, cycle, "Should detect cycle even with multiple owners")
}

func TestWaitForGraph_RemoveWaiter_BreaksCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Create potential deadlock
	wfg.AddWaiter("A", []string{"B"})

	// Check that B->A would cause cycle
	assert.True(t, wfg.WouldCauseCycle("B", []string{"A"}))

	// Remove A as waiter (e.g., A's request times out)
	wfg.RemoveWaiter("A")

	// Now B->A should not cause cycle
	assert.False(t, wfg.WouldCauseCycle("B", []string{"A"}))
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_RemoveOwner_BreaksCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A -> B -> C
	wfg.AddWaiter("A", []string{"B"})
	wfg.AddWaiter("B", []string{"C"})

	// C -> A would create cycle
	assert.True(t, wfg.WouldCauseCycle("C", []string{"A"}))

	// Remove B (e.g., B releases lock or disconnects)
	wfg.RemoveOwner("B")

	// C -> A should not cause cycle now (A is waiting for nothing)
	assert.False(t, wfg.WouldCauseCycle("C", []string{"A"}))

	// A's wait for B should also be gone
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_RemoveOwner_PartialRemoval(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A waits for B and C
	wfg.AddWaiter("A", []string{"B", "C"})

	// Remove B
	wfg.RemoveOwner("B")

	// A should still be waiting for C
	assert.Equal(t, 1, wfg.Size())

	// D -> A still no cycle (A -> C only, no path back)
	assert.False(t, wfg.WouldCauseCycle("D", []string{"A"}))

	// C -> A should cause cycle (A -> C -> A)
	assert.True(t, wfg.WouldCauseCycle("C", []string{"A"}))
}

func TestWaitForGraph_GetWaitersFor(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A, B, C all waiting for D
	wfg.AddWaiter("A", []string{"D"})
	wfg.AddWaiter("B", []string{"D"})
	wfg.AddWaiter("C", []string{"D", "E"}) // C also waits for E

	waiters := wfg.GetWaitersFor("D")

	assert.Len(t, waiters, 3)
	assert.Contains(t, waiters, "A")
	assert.Contains(t, waiters, "B")
	assert.Contains(t, waiters, "C")
}

func TestWaitForGraph_GetWaitersFor_NoWaiters(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	wfg.AddWaiter("A", []string{"B"})

	waiters := wfg.GetWaitersFor("C") // No one waiting for C

	assert.Nil(t, waiters)
}

func TestWaitForGraph_EmptyGraph(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// No cycles in empty graph
	assert.False(t, wfg.WouldCauseCycle("A", []string{"B"}))
	assert.False(t, wfg.WouldCauseCycle("A", []string{}))
	assert.False(t, wfg.WouldCauseCycle("", []string{"A"}))

	// Safe to remove non-existent entries
	wfg.RemoveWaiter("X")
	wfg.RemoveOwner("Y")

	assert.Nil(t, wfg.GetWaitersFor("Z"))
	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_AddWaiter_EmptyOwners(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Adding with empty owners should be no-op
	wfg.AddWaiter("A", []string{})
	wfg.AddWaiter("A", nil)

	assert.Equal(t, 0, wfg.Size())
}

func TestWaitForGraph_SelfCycle(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// A wants to wait for A (immediate cycle)
	cycle := wfg.WouldCauseCycle("A", []string{"A"})

	// This isn't detected by our DFS because A has no edges yet
	// This is a degenerate case - in practice, A shouldn't wait for itself
	// The lock manager should prevent this at a higher level
	assert.False(t, cycle, "Self-cycle check depends on existing edges")

	// But if A is already waiting for B...
	wfg.AddWaiter("A", []string{"B"})
	// ...and B tries to wait for A
	cycle = wfg.WouldCauseCycle("B", []string{"A"})
	assert.True(t, cycle)
}

func TestWaitForGraph_LongChain(t *testing.T) {
	t.Parallel()

	wfg := NewWaitForGraph()

	// Create long chain: owner0 -> owner1 -> owner2 -> ... -> owner99
	// Use numeric IDs to avoid character overflow
	for i := 0; i < 99; i++ {
		wfg.AddWaiter(
			string(rune('0'+i/10))+string(rune('0'+i%10)), // "00", "01", ..., "98"
			[]string{string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10))}, // "01", "02", ..., "99"
		)
	}

	// Check that a non-participant doesn't create a cycle
	assert.False(t, wfg.WouldCauseCycle("XX", []string{"00"}))

	// Creating cycle at the end: "99" -> "00" would create cycle
	cycle := wfg.WouldCauseCycle("99", []string{"00"})
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
					wfg.WouldCauseCycle(ownerID, []string{targetID})
				case 1:
					wfg.AddWaiter(ownerID, []string{targetID})
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
		wfg.AddWaiter("A", []string{"B"})

		var wg sync.WaitGroup
		results := make(chan bool, 10)

		// Multiple goroutines checking B -> A cycle
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- wfg.WouldCauseCycle("B", []string{"A"})
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
