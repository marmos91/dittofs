package health

import (
	"context"
	"sync"
	"time"
)

// CachedChecker wraps a [Checker] with a time-based cache and single-flight
// behavior. At most one underlying probe runs at a time; concurrent callers
// during an in-flight probe wait for it and share the result. Results are
// reused for the configured TTL before the next probe.
//
// Intended usage: API handlers that serve /status routes wrap each
// entity's real Checker once at construction and store the wrapped
// instance. Per-request handlers then call Healthcheck on the cache;
// bursty traffic (10 browser tabs, a dashboard auto-refresh, a CLI
// status loop) collapses onto a single underlying probe per TTL window.
//
// A zero TTL disables caching (every call probes). Negative TTLs are
// treated as zero.
type CachedChecker struct {
	inner Checker
	ttl   time.Duration

	mu     sync.Mutex // guards last, lastAt
	last   Report
	lastAt time.Time // zero means "no probe has run yet"
}

// NewCachedChecker wraps inner with a TTL cache. A TTL of zero or less
// disables caching entirely, which is useful for tests that want every
// call to hit the underlying probe.
func NewCachedChecker(inner Checker, ttl time.Duration) *CachedChecker {
	if ttl < 0 {
		ttl = 0
	}
	return &CachedChecker{inner: inner, ttl: ttl}
}

// Healthcheck returns a Report, serving from cache when possible.
//
// The implementation uses a single mutex held across the underlying
// probe. This serialises concurrent callers, which is the desired
// single-flight behavior: while one goroutine probes, others block on
// the mutex and then see the freshly cached result when they acquire it.
// For typical probe durations (milliseconds to a few seconds) this is
// cheaper and simpler than a separate singleflight.Group, and keeps the
// package dependency-free.
//
// The context is forwarded unchanged to the underlying probe; cancellation
// is the inner Checker's responsibility.
func (c *CachedChecker) Healthcheck(ctx context.Context) Report {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Fast path: within TTL, return the cached report unchanged.
	// Using the package-level Now function so tests can fake the clock.
	if c.ttl > 0 && !c.lastAt.IsZero() && Now().Sub(c.lastAt) < c.ttl {
		return c.last
	}

	// Slow path: probe and cache.
	rep := c.inner.Healthcheck(ctx)
	c.last = rep
	c.lastAt = Now()
	return rep
}

// Invalidate drops any cached result. The next Healthcheck call will
// always run the underlying probe. Useful when the caller knows the
// underlying state has changed (e.g. an adapter was just restarted) and
// wants to force a fresh read rather than waiting out the TTL.
func (c *CachedChecker) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastAt = time.Time{}
	c.last = Report{}
}

// Ensure the type satisfies Checker at compile time.
var _ Checker = (*CachedChecker)(nil)
