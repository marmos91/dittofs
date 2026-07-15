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
func TestAuthCacheTTLExpiry(t *testing.T) {
	const key = "/export:1:1000:1000"
	h := &Handler{authCacheTTL: 20 * time.Millisecond}

	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now(),
	})

	if got := h.loadLiveAuthCtx(key); got == nil {
		t.Fatal("fresh entry should be served from cache")
	}

	time.Sleep(30 * time.Millisecond)

	if got := h.loadLiveAuthCtx(key); got != nil {
		t.Fatal("entry past its TTL should be treated as a miss")
	}

	// A backdated entry is expired regardless of sleep timing.
	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now().Add(-time.Hour),
	})
	if got := h.loadLiveAuthCtx(key); got != nil {
		t.Fatal("backdated entry should be expired")
	}
}

// TestAuthCacheClearOnIdentityChange verifies the effect of the
// OnIdentityMappingChange hook wired in the NFS adapter: ClearAuthCache drops a
// still-fresh entry so a user/group-membership edit takes effect immediately.
func TestAuthCacheClearOnIdentityChange(t *testing.T) {
	const key = "/export:1:1000:1000"
	h := &Handler{} // default (long) TTL — entry would otherwise stay live

	h.authCache.Store(key, &authCacheEntry{
		authCtx:  &metadata.AuthContext{AuthMethod: "unix"},
		cachedAt: time.Now(),
	})
	if got := h.loadLiveAuthCtx(key); got == nil {
		t.Fatal("entry should be live before the identity-change event")
	}

	// This is what the OnIdentityMappingChange callback invokes.
	h.ClearAuthCache()

	if got := h.loadLiveAuthCtx(key); got != nil {
		t.Fatal("identity-change invalidation should drop the cached entry")
	}
}
