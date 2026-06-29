package shares

import (
	"sync/atomic"
	"testing"
)

// TestOnAuthCacheInvalidate_FiresAndUnsubscribes verifies that registered
// auth-cache-invalidate callbacks fire on InvalidateAuthCache and that the
// returned unsubscribe function removes them.
func TestOnAuthCacheInvalidate_FiresAndUnsubscribes(t *testing.T) {
	svc := New()

	var calls atomic.Int32
	unsub := svc.OnAuthCacheInvalidate(func() { calls.Add(1) })

	svc.InvalidateAuthCache()
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected callback to fire once, got %d", got)
	}

	svc.InvalidateAuthCache()
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected callback to fire twice, got %d", got)
	}

	unsub()
	svc.InvalidateAuthCache()
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected no further calls after unsubscribe, got %d", got)
	}
}

// TestOnAuthCacheInvalidate_DoesNotFireShareChange verifies the two callback
// registries are independent: an auth-cache invalidation must not trigger the
// heavier share-set listeners (pseudo-fs rebuild, NFSv4 delegation recall).
func TestOnAuthCacheInvalidate_DoesNotFireShareChange(t *testing.T) {
	svc := New()

	var authCalls, shareCalls atomic.Int32
	svc.OnAuthCacheInvalidate(func() { authCalls.Add(1) })
	svc.OnShareChange(func(_ []string) { shareCalls.Add(1) })

	svc.InvalidateAuthCache()

	if got := authCalls.Load(); got != 1 {
		t.Fatalf("expected auth callback to fire once, got %d", got)
	}
	if got := shareCalls.Load(); got != 0 {
		t.Fatalf("expected share-change callback NOT to fire, got %d", got)
	}
}
