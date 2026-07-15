package handlers

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/identity"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestAuthCacheDefaultTTL confirms an unset TTL falls back to the pkg/identity
// positive cache TTL so the NFS auth cache and identity cache age out together.
func TestAuthCacheDefaultTTL(t *testing.T) {
	h := &Handler{}
	if got := h.authCacheEntryTTL(); got != identity.DefaultCacheTTL {
		t.Fatalf("default auth-cache TTL = %v, want %v (aligned with identity cache)", got, identity.DefaultCacheTTL)
	}
}

// TestAuthCacheTTLExpiry verifies a cached entry is served while fresh and
// treated as a miss once its TTL has elapsed, so an out-of-band UID/group
// change can't stay effective past the TTL even without an invalidation event.
// Expiry is forced by backdating cachedAt (no sleep) so the test is
// deterministic under load.
func TestAuthCacheTTLExpiry(t *testing.T) {
	const key = "/export:1:1000:1000"
	h := &Handler{authCacheTTL: time.Minute}

	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now(),
	})
	if got := h.loadLiveAuthCtx(key); got == nil {
		t.Fatal("fresh entry should be served from cache")
	}

	// Backdate past the TTL: the entry must now be treated as a miss.
	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now().Add(-2 * time.Minute),
	})
	if got := h.loadLiveAuthCtx(key); got != nil {
		t.Fatal("entry past its TTL should be treated as a miss")
	}
	// And the expired entry must have been evicted, not left in the map.
	if _, ok := h.authCache.Load(key); ok {
		t.Fatal("expired entry should be deleted from the cache")
	}
}

// TestClearAuthCacheDropsLiveEntry verifies ClearAuthCache drops a still-fresh
// entry. This is the effect the adapter's OnIdentityMappingChange callback
// relies on so a user/group-membership edit takes effect immediately; the
// adapter wiring itself is not exercised here.
func TestClearAuthCacheDropsLiveEntry(t *testing.T) {
	const key = "/export:1:1000:1000"
	h := &Handler{} // default (long) TTL — entry would otherwise stay live

	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now(),
	})
	if got := h.loadLiveAuthCtx(key); got == nil {
		t.Fatal("entry should be live before ClearAuthCache")
	}

	h.ClearAuthCache()

	if got := h.loadLiveAuthCtx(key); got != nil {
		t.Fatal("ClearAuthCache should drop the cached entry")
	}
}
