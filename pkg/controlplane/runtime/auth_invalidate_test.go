package runtime

import (
	"context"
	"sync/atomic"
	"testing"

	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestAddShare_InvalidatesAuthCache covers the one caller of
// ReconcileShareRootACL outside the API's grant helper. A share created under a
// name that was just removed can find trees left over from the old one: the
// sweep a removal fires cannot re-resolve a tree whose share is gone, so it
// leaves that tree carrying the removed share's permission. Creating the share
// has to raise the invalidation that re-decides them, and it must not depend on
// the root-ACL projection, which is best-effort.
func TestAddShare_InvalidatesAuthCache(t *testing.T) {
	rt, bsID := newRuntimeWithBlockStore(t)
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	if err := rt.RegisterMetadataStore("meta", metaStore); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}

	var calls atomic.Int32
	rt.OnAuthCacheInvalidate(func() { calls.Add(1) })

	cfg := &ShareConfig{Name: "/export", MetadataStore: "meta", Enabled: true, BlockStoreID: bsID}
	if err := rt.AddShare(context.Background(), cfg); err != nil {
		t.Fatalf("AddShare: %v", err)
	}
	if got := calls.Load(); got == 0 {
		t.Fatal("AddShare did not fire the auth-cache invalidation event; a tree left over " +
			"from a share of the same name keeps the removed share's permission")
	}
}

// TestIdentityChangesInvalidateAuthCache covers the two notifications that
// carried only their own narrow callback. An identity or SID mapping decides
// which local identity a principal authenticates as, and a directory config
// change rewrites how group membership and foreign SIDs resolve — both move the
// answer share grants are evaluated against, and neither reached an established
// SMB session, whose subscriber only cleared a name-resolver cache.
func TestIdentityChangesInvalidateAuthCache(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify func(*Runtime)
	}{
		{"identity mapping", (*Runtime).NotifyIdentityMappingChange},
		{"identity provider config", (*Runtime).NotifyIdentityProviderConfigChange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newTestRuntime(t)
			var calls atomic.Int32
			rt.OnAuthCacheInvalidate(func() { calls.Add(1) })

			tc.notify(rt)

			if got := calls.Load(); got != 1 {
				t.Fatalf("auth-cache invalidations = %d, want 1", got)
			}
		})
	}
}
