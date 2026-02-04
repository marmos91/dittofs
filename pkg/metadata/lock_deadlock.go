package metadata

import "sync"

// ============================================================================
// Wait-For Graph Deadlock Detection
// ============================================================================

// WaitForGraph implements deadlock detection using a Wait-For Graph (WFG).
//
// In a WFG, nodes represent lock owners (by OwnerID) and directed edges
// represent "is waiting for" relationships. A cycle in this graph indicates
// a deadlock condition.
//
// Example deadlock:
//   - Owner A holds lock L1, waits for lock L2 (held by B)
//   - Owner B holds lock L2, waits for lock L1 (held by A)
//   - Graph: A -> B -> A (cycle = deadlock)
//
// Usage:
//  1. Before blocking on a lock, call WouldCauseCycle() to check for deadlock
//  2. If no cycle, call AddWaiter() to record the wait relationship
//  3. When lock is granted or request times out, call RemoveWaiter()
//  4. When lock is released, call RemoveOwner() to clear all wait relationships
//
// Thread Safety:
// WaitForGraph is safe for concurrent use by multiple goroutines.
type WaitForGraph struct {
	mu sync.RWMutex

	// edges maps waiter -> set of owners being waited on
	// waiter is waiting for all owners in the set
	edges map[string]map[string]struct{}
}

// NewWaitForGraph creates a new empty Wait-For Graph.
func NewWaitForGraph() *WaitForGraph {
	return &WaitForGraph{
		edges: make(map[string]map[string]struct{}),
	}
}

// WouldCauseCycle checks if adding edges from waiter to owners would create a cycle.
//
// This MUST be called before blocking on a lock request. If it returns true,
// the lock request should be denied with ErrDeadlock instead of blocking.
//
// Parameters:
//   - waiter: The owner ID that wants to wait
//   - owners: The owner IDs currently holding the conflicting lock(s)
//
// Returns:
//   - true if adding these wait relationships would create a cycle (deadlock)
//   - false if it's safe to proceed with waiting
//
// Algorithm:
// For each owner in owners, perform DFS from that owner to see if we can
// reach the waiter. If any path exists, adding waiter -> owner would create
// a cycle.
func (wfg *WaitForGraph) WouldCauseCycle(waiter string, owners []string) bool {
	wfg.mu.RLock()
	defer wfg.mu.RUnlock()

	// For each owner, check if we can reach waiter from owner
	for _, owner := range owners {
		if wfg.canReach(owner, waiter, make(map[string]bool)) {
			return true // Would create cycle
		}
	}

	return false
}

// AddWaiter records that waiter is waiting for all specified owners.
//
// IMPORTANT: Caller MUST call WouldCauseCycle first to ensure this won't
// create a deadlock. This method does NOT check for cycles.
//
// Parameters:
//   - waiter: The owner ID that is now waiting
//   - owners: The owner IDs being waited on
func (wfg *WaitForGraph) AddWaiter(waiter string, owners []string) {
	if len(owners) == 0 {
		return
	}

	wfg.mu.Lock()
	defer wfg.mu.Unlock()

	// Get or create the wait set for this waiter
	waitSet, exists := wfg.edges[waiter]
	if !exists {
		waitSet = make(map[string]struct{})
		wfg.edges[waiter] = waitSet
	}

	// Add all owners to the wait set
	for _, owner := range owners {
		waitSet[owner] = struct{}{}
	}
}

// RemoveWaiter removes all wait relationships where waiter is the source.
//
// Call this when:
//   - Lock is granted to the waiter
//   - Lock request is cancelled
//   - Lock request times out
//
// Parameters:
//   - waiter: The owner ID that is no longer waiting
func (wfg *WaitForGraph) RemoveWaiter(waiter string) {
	wfg.mu.Lock()
	defer wfg.mu.Unlock()

	delete(wfg.edges, waiter)
}

// RemoveOwner removes all wait relationships where owner is the target.
//
// Call this when:
//   - Owner releases a lock
//   - Owner's session disconnects
//
// This also removes the owner as a waiter (if they were waiting for something).
//
// Parameters:
//   - owner: The owner ID to remove from the graph
func (wfg *WaitForGraph) RemoveOwner(owner string) {
	wfg.mu.Lock()
	defer wfg.mu.Unlock()

	// Remove owner from all wait sets (edges where owner is target)
	for waiter, waitSet := range wfg.edges {
		delete(waitSet, owner)

		// Clean up empty wait sets
		if len(waitSet) == 0 {
			delete(wfg.edges, waiter)
		}
	}

	// Also remove owner as a waiter
	delete(wfg.edges, owner)
}

// GetWaitersFor returns all owners that are waiting for the specified owner.
//
// This is useful when a lock is released - we can notify these waiters
// that they may be able to proceed.
//
// Parameters:
//   - owner: The owner ID to find waiters for
//
// Returns:
//   - Slice of owner IDs that are waiting for this owner
func (wfg *WaitForGraph) GetWaitersFor(owner string) []string {
	wfg.mu.RLock()
	defer wfg.mu.RUnlock()

	var waiters []string
	for waiter, waitSet := range wfg.edges {
		if _, waiting := waitSet[owner]; waiting {
			waiters = append(waiters, waiter)
		}
	}
	return waiters
}

// Size returns the number of owners currently waiting (for testing/monitoring).
func (wfg *WaitForGraph) Size() int {
	wfg.mu.RLock()
	defer wfg.mu.RUnlock()
	return len(wfg.edges)
}

// canReach performs DFS to check if we can reach 'to' from 'from'.
// Must be called with at least RLock held.
func (wfg *WaitForGraph) canReach(from, to string, visited map[string]bool) bool {
	// Avoid infinite loops
	if visited[from] {
		return false
	}
	visited[from] = true

	// Check if 'from' is waiting for anything
	waitSet, exists := wfg.edges[from]
	if !exists {
		return false
	}

	// Check direct edge
	if _, waiting := waitSet[to]; waiting {
		return true
	}

	// Check transitive edges (DFS)
	for waitingFor := range waitSet {
		if wfg.canReach(waitingFor, to, visited) {
			return true
		}
	}

	return false
}
