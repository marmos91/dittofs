package scheduler

import "sync"

// OverlapGuard serializes work per repo ID. A cron tick that would
// produce a concurrent run for a repo with a still-running prior tick
// is skipped via TryLock returning (nil, false) — the caller logs and
// increments an overlap-skipped counter (D-07). Phase 6's on-demand
// backup API acquires the SAME mutex and returns 409 Conflict on
// contention (D-23).
//
// Implementation uses sync.Map keyed by repoID with *sync.Mutex
// values. This mirrors the per-connection mutex pattern used in
// pkg/adapter/nfs/connection.go. Keys are created on first contact
// via LoadOrStore and retained thereafter (the set is bounded by the
// number of registered repos — small enough that a manual GC path
// is not needed in v0.13.0).
type OverlapGuard struct {
	mu sync.Map // repoID -> *sync.Mutex
}

// NewOverlapGuard returns an empty guard ready for TryLock calls.
func NewOverlapGuard() *OverlapGuard { return &OverlapGuard{} }

// TryLock attempts to acquire the per-repo mutex. Returns (unlock, true)
// on success — caller defer-calls unlock() when the run completes.
// Returns (nil, false) if the mutex is currently held (another run is
// in flight for the same repoID).
//
// The returned unlock closure MUST be called exactly once. Calling it
// twice panics per the underlying sync.Mutex contract; that is the
// intended signal for double-unlock bugs.
func (g *OverlapGuard) TryLock(repoID string) (unlock func(), acquired bool) {
	m, _ := g.mu.LoadOrStore(repoID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}
