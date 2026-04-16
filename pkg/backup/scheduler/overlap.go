package scheduler

import "sync"

// OverlapGuard serializes work per repo ID. A cron tick that would produce
// a concurrent run for a repo with a still-running prior tick is skipped via
// TryLock returning (nil, false) — the caller logs and increments an
// overlap-skipped counter. On-demand backup APIs acquire the SAME mutex and
// can return 409 Conflict on contention.
//
// Implementation uses sync.Map keyed by repoID with *sync.Mutex values. Keys
// are created lazily via LoadOrStore; Forget removes them when a repo is
// unregistered so long-running servers don't accumulate dead entries.
type OverlapGuard struct {
	mu sync.Map // repoID -> *sync.Mutex
}

// NewOverlapGuard returns an empty guard ready for TryLock calls.
func NewOverlapGuard() *OverlapGuard { return &OverlapGuard{} }

// TryLock attempts to acquire the per-repo mutex. Returns (unlock, true) on
// success — caller defer-calls unlock() when the run completes. Returns
// (nil, false) if the mutex is currently held (another run is in flight for
// the same repoID).
//
// The returned unlock closure MUST be called exactly once. Calling it twice
// panics per the underlying sync.Mutex contract.
func (g *OverlapGuard) TryLock(repoID string) (unlock func(), acquired bool) {
	// Fast path: avoid allocating a mutex we throw away on the hot path.
	if m, ok := g.mu.Load(repoID); ok {
		mu := m.(*sync.Mutex)
		if !mu.TryLock() {
			return nil, false
		}
		return mu.Unlock, true
	}
	m, _ := g.mu.LoadOrStore(repoID, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// Forget drops the cached mutex for repoID. Safe to call when no holder is
// active; callers typically invoke this after UnregisterRepo so the guard
// doesn't retain mutexes for deleted repos.
func (g *OverlapGuard) Forget(repoID string) { g.mu.Delete(repoID) }
