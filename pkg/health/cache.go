package health

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DefaultProbeTimeout is the timeout applied to the detached context used
// for the underlying probe. It exists so a runaway backend probe (e.g. an
// S3 call that never returns) can't tie up the cache's leader slot
// forever. Callers that need a different value can use
// [NewCachedCheckerWithTimeout].
const DefaultProbeTimeout = 30 * time.Second

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
//
// # Context handling
//
// The cache is careful with caller contexts. Two rules:
//
//  1. The underlying probe runs with a **detached** context (a fresh
//     [context.Background] with [DefaultProbeTimeout]), not with the
//     caller's context. This prevents one caller with a tight deadline
//     from canceling a probe whose result would have been used by other
//     concurrent callers, poisoning the cache with [StatusUnknown].
//
//  2. The caller's context only gates the caller's **wait**. If the
//     caller's context is canceled while they are blocked waiting for an
//     in-flight probe to finish, Healthcheck returns a [StatusUnknown]
//     report describing the caller's own cancellation — it does not
//     abort the probe, which keeps running for the benefit of other
//     waiters.
//
// This matches the [Checker] contract: Healthcheck respects ctx
// cancellation (rule 2) but doesn't let one impatient caller degrade the
// service for everyone else (rule 1).
//
// # Panic safety
//
// If the underlying probe panics, CachedChecker recovers the panic,
// synthesises a [StatusUnhealthy] [Report] with the panic value as the
// message, publishes it (so concurrent waiters are released instead of
// blocking forever on a never-closed done channel), and then re-panics
// in the leader's goroutine so the caller that triggered the probe
// still sees the panic. This guarantees the cache cannot wedge even if
// an upstream implementation misbehaves.
type CachedChecker struct {
	inner        Checker
	ttl          time.Duration
	probeTimeout time.Duration

	// clock is the time source used for TTL calculations. Defaults to
	// [time.Now]; tests in this package assign it before any concurrent
	// callers start. Keeping it as an instance field (rather than a
	// package-level variable) means tests running in parallel or
	// mutating clocks on different CachedChecker instances don't race
	// against each other.
	clock func() time.Time

	mu       sync.Mutex // guards last, lastAt, inflight
	last     Report
	lastAt   time.Time     // zero means "no probe has run yet"
	inflight *inflightCall // non-nil while a probe is running
}

// inflightCall is the rendezvous for a single in-flight probe. The leader
// goroutine creates one, runs the probe, writes the result to rep, and
// closes done. Waiters selecting on <-done will see a closed channel and
// can then read rep safely (the channel close is a happens-before edge).
type inflightCall struct {
	done chan struct{}
	rep  Report
}

// NewCachedChecker wraps inner with a TTL cache using [DefaultProbeTimeout]
// for the detached probe context. A TTL of zero or less disables caching
// entirely, which is useful for tests that want every call to hit the
// underlying probe.
//
// NewCachedChecker panics if inner is nil — a nil Checker would cause an
// opaque runtime panic on the first Healthcheck call, so we fail fast at
// construction time with a clear message.
func NewCachedChecker(inner Checker, ttl time.Duration) *CachedChecker {
	return NewCachedCheckerWithTimeout(inner, ttl, DefaultProbeTimeout)
}

// NewCachedCheckerWithTimeout is like [NewCachedChecker] but lets the
// caller override the per-probe timeout applied to the detached context.
// A non-positive probeTimeout falls back to [DefaultProbeTimeout].
func NewCachedCheckerWithTimeout(inner Checker, ttl, probeTimeout time.Duration) *CachedChecker {
	if inner == nil {
		panic("health.NewCachedChecker: inner Checker must not be nil")
	}
	if ttl < 0 {
		ttl = 0
	}
	if probeTimeout <= 0 {
		probeTimeout = DefaultProbeTimeout
	}
	return &CachedChecker{
		inner:        inner,
		ttl:          ttl,
		probeTimeout: probeTimeout,
		clock:        time.Now,
	}
}

// Healthcheck returns a Report, serving from cache when possible.
//
// See the type-level doc comment for the context-handling and
// panic-safety contracts.
func (c *CachedChecker) Healthcheck(ctx context.Context) Report {
	// Fast path: check cache under the lock.
	c.mu.Lock()

	if c.ttl > 0 && !c.lastAt.IsZero() && c.clock().Sub(c.lastAt) < c.ttl {
		rep := c.last
		c.mu.Unlock()
		return rep
	}

	// Cache miss or expired. Is another goroutine already probing?
	if c.inflight != nil {
		call := c.inflight
		c.mu.Unlock()
		// Wait for the probe to finish or the caller to cancel.
		select {
		case <-call.done:
			return call.rep
		case <-ctx.Done():
			return Report{
				Status:    StatusUnknown,
				Message:   "health check canceled: " + ctx.Err().Error(),
				CheckedAt: c.clock().UTC(),
			}
		}
	}

	// We are the leader: register an inflight slot and run the probe
	// with a detached context so that our own caller's cancellation
	// doesn't kill the probe for everyone else who may be waiting.
	call := &inflightCall{done: make(chan struct{})}
	c.inflight = call
	c.mu.Unlock()

	// Deferred cancel ensures we don't leak the timeout context's
	// internal goroutine even if the probe panics on the way out.
	probeCtx, cancel := context.WithTimeout(context.Background(), c.probeTimeout)
	defer cancel()

	// Run the probe inside a recovery closure so a panic in the inner
	// Checker cannot leave c.inflight set or call.done unclosed —
	// either of which would wedge every subsequent caller forever.
	var (
		rep        Report
		probePanic any
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				probePanic = r
				rep = Report{
					Status:    StatusUnhealthy,
					Message:   fmt.Sprintf("health probe panicked: %v", r),
					CheckedAt: c.clock().UTC(),
				}
			}
		}()
		rep = c.inner.Healthcheck(probeCtx)
	}()

	// Publish the result and release waiters, regardless of whether the
	// probe returned normally or panicked. Writing call.rep after the
	// unlock and before close(call.done) is race-free: the close is the
	// happens-before edge that waiters read through.
	c.mu.Lock()
	c.last = rep
	c.lastAt = c.clock()
	c.inflight = nil
	c.mu.Unlock()

	call.rep = rep
	close(call.done)

	// Re-panic AFTER releasing waiters so the caller that triggered
	// the panic still sees it (surfacing bugs rather than silently
	// swallowing them) while no other goroutine is left blocked.
	if probePanic != nil {
		panic(probePanic)
	}

	return rep
}

// Invalidate drops any cached result. The next Healthcheck call will
// always run the underlying probe. Useful when the caller knows the
// underlying state has changed (e.g. an adapter was just restarted) and
// wants to force a fresh read rather than waiting out the TTL.
//
// An in-flight probe is not canceled — it will finish and release its
// waiters — but its result will not be persisted: the next caller will
// see the cleared cache and trigger a fresh probe.
func (c *CachedChecker) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastAt = time.Time{}
	c.last = Report{}
}

// Ensure the type satisfies Checker at compile time.
var _ Checker = (*CachedChecker)(nil)
