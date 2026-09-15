package shares

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The share-level policy changes below decide who may access a share, and
// adapters resolve that once per connection: SMB at TREE_CONNECT, NFSv3 into a
// TTL cache. Each therefore has to raise an auth-cache invalidation from the
// place the live value is written, or an established connection keeps enforcing
// the previous policy for its whole lifetime.

// TestUpdateShare_ReadOnlyInvalidatesAuthCache covers the read_only half: a
// share flipped to read-only must retire the write access every established
// tree is still carrying.
func TestUpdateShare_ReadOnlyInvalidatesAuthCache(t *testing.T) {
	svc, _ := makeService(t, &Share{Name: "/export", MetadataStore: "meta-a", Enabled: true})

	var calls atomic.Int32
	svc.OnAuthCacheInvalidate(func() { calls.Add(1) })

	readOnly := true
	if err := svc.UpdateShare("/export", &readOnly, nil, nil, nil); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("read_only change: auth-cache invalidations = %d, want 1", got)
	}

	perm := "read"
	if err := svc.UpdateShare("/export", nil, &perm, nil, nil); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("default-permission change: auth-cache invalidations = %d, want 2", got)
	}
}

// TestUpdateShare_RetentionOnlyDoesNotInvalidate is the other side of the same
// guard: the retention knobs carry no authorization, so an invalidation fired
// for them would be a sweep over every SMB session for nothing.
func TestUpdateShare_RetentionOnlyDoesNotInvalidate(t *testing.T) {
	svc, _ := makeService(t, &Share{Name: "/export", MetadataStore: "meta-a", Enabled: true})

	var calls atomic.Int32
	svc.OnAuthCacheInvalidate(func() { calls.Add(1) })

	ttl := time.Hour
	if err := svc.UpdateShare("/export", nil, nil, nil, &ttl); err != nil {
		t.Fatalf("UpdateShare: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("retention-only change: auth-cache invalidations = %d, want 0", got)
	}
}

// TestUpdateShare_InvalidatesOutsideRegistryLock pins the ordering constraint
// InvalidateAuthCache documents: a subscriber re-reads the registry, so firing
// under s.mu would deadlock.
func TestUpdateShare_InvalidatesOutsideRegistryLock(t *testing.T) {
	svc, _ := makeService(t, &Share{Name: "/export", MetadataStore: "meta-a", Enabled: true})

	svc.OnAuthCacheInvalidate(func() {
		if _, err := svc.GetShare("/export"); err != nil {
			t.Errorf("GetShare from invalidate callback: %v", err)
		}
	})

	readOnly := true
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := svc.UpdateShare("/export", &readOnly, nil, nil, nil); err != nil {
			t.Errorf("UpdateShare: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateShare deadlocked: invalidation fired while holding the registry lock")
	}
}

// TestDisableShare_InvalidatesAuthCache: a disabled share admits nobody, but
// the flag reaches an established SMB tree through nothing on its own.
func TestDisableShare_InvalidatesAuthCache(t *testing.T) {
	svc, store := makeService(t, &Share{Name: "/export", MetadataStore: "meta-a", Enabled: true})

	var calls atomic.Int32
	svc.OnAuthCacheInvalidate(func() { calls.Add(1) })

	if err := svc.DisableShare(context.Background(), store, "/export"); err != nil {
		t.Fatalf("DisableShare: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("disable: auth-cache invalidations = %d, want 1", got)
	}

	// Re-enabling only widens access and restores no stored decision, so it
	// deliberately raises nothing.
	if err := svc.EnableShare(context.Background(), store, "/export"); err != nil {
		t.Fatalf("EnableShare: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("enable: auth-cache invalidations = %d, want 1", got)
	}
}

// TestRemoveShare_InvalidatesAuthCache: every decision resolved against a
// removed share is stale, and DELETE /shares/{n} previously evicted only the
// health-probe cache.
func TestRemoveShare_InvalidatesAuthCache(t *testing.T) {
	svc, _ := makeService(t, &Share{Name: "/export", MetadataStore: "meta-a", Enabled: true})

	var calls atomic.Int32
	svc.OnAuthCacheInvalidate(func() {
		// The registry entry must already be gone: a subscriber told to forget
		// the share cannot be handed it back by a concurrent lookup.
		if _, err := svc.GetShare("/export"); err == nil {
			t.Error("share still registered when auth-cache invalidation fired")
		}
		calls.Add(1)
	})

	if err := svc.RemoveShare("/export"); err != nil {
		t.Fatalf("RemoveShare: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("remove: auth-cache invalidations = %d, want 1", got)
	}
}
