package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/controlplane/api/auth"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newRootACLTestRouter builds a router over a FULLY WIRED runtime: an in-memory
// metadata store plus a memory block store, so each named share has a real root
// inode and the root-ACL projection actually writes. newTestRouter's runtime has
// no metadata store, where ReconcileShareRootACL returns at its first guard and
// every projection difference is invisible.
//
// Shares are created with DefaultPermission=none so the root ACL carries only
// the always-on trustees plus whatever grants a test writes.
func newRootACLTestRouter(t *testing.T, shareNames ...string) (http.Handler, string, store.Store, *runtime.Runtime) {
	t.Helper()
	ctx := context.Background()

	cpStore, err := store.New(&store.Config{
		Type:   "sqlite",
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	jwtService, err := auth.NewJWTService(auth.JWTConfig{
		Secret:               "test-secret-key-for-testing-only-32chars",
		Issuer:               "dittofs",
		AccessTokenDuration:  15 * time.Minute,
		RefreshTokenDuration: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create jwt service: %v", err)
	}

	rt := runtime.New(cpStore)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: t.TempDir()})
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	metaCfg := &models.MetadataStoreConfig{Name: "test-meta", Type: "memory"}
	metaID, err := cpStore.CreateMetadataStore(ctx, metaCfg)
	if err != nil {
		t.Fatalf("create metadata store config: %v", err)
	}
	if err := rt.RegisterMetadataStore("test-meta", memory.NewMemoryMetadataStoreWithDefaults()); err != nil {
		t.Fatalf("register metadata store: %v", err)
	}
	blockID, err := cpStore.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "test-block", Type: "memory"})
	if err != nil {
		t.Fatalf("create block store config: %v", err)
	}

	for _, name := range shareNames {
		if _, err := cpStore.CreateShare(ctx, &models.Share{
			Name:              name,
			MetadataStoreID:   metaID,
			BlockStoreID:      blockID,
			DefaultPermission: string(models.PermissionNone),
		}); err != nil {
			t.Fatalf("create share %q: %v", name, err)
		}
	}
	if err := runtime.LoadSharesFromStore(ctx, rt, cpStore); err != nil {
		t.Fatalf("load shares: %v", err)
	}

	router := NewRouter(rt, jwtService, cpStore, false, Timeouts{Restore: 30 * time.Minute, DrainStall: 5 * time.Minute})
	return router, tokenFor(t, jwtService, models.RoleAdmin), cpStore, rt
}

// rootACL reads the ACL actually stored on a share's root inode — the state a
// permission check consults, not a recomputation of what it ought to be.
func rootACL(t *testing.T, rt *runtime.Runtime, shareName string) *acl.ACL {
	t.Helper()
	ctx := context.Background()
	ms := rt.GetMetadataService()
	handle, err := ms.GetRootHandle(ctx, shareName)
	if err != nil {
		t.Fatalf("root handle for %q: %v", shareName, err)
	}
	f, err := ms.GetFile(ctx, handle)
	if err != nil {
		t.Fatalf("read root inode for %q: %v", shareName, err)
	}
	return f.ACL
}

// TestUserRouteGrantProjectsSameRootACLAsShareRoute pins the two endpoints that
// write the same share grant to the same on-disk result.
//
// The share root directory is owned by uid 0 with mode 0755, so a grantee
// reaches it only through the ACL projected from the share's grants. A grant
// written through PUT /api/v1/users/{name} that never reprojects therefore
// persists a permission record the filesystem layer never honours, while the
// identical grant written through the share permission route works — the same
// administrative intent producing different access depending on the endpoint.
//
// Both shares are configured identically and receive the identical grant, so
// the two root ACLs must be equal ACE for ACE.
func TestUserRouteGrantProjectsSameRootACLAsShareRoute(t *testing.T) {
	router, token, cpStore, rt := newRootACLTestRouter(t, "/viashare", "/viauser")
	seedUser(t, cpStore, "target")

	// Both shares start from the same projection, or a later difference would
	// not be attributable to the grant.
	if before, after := rootACL(t, rt, "/viashare"), rootACL(t, rt, "/viauser"); !reflect.DeepEqual(before, after) {
		t.Fatalf("shares differ before any grant:\n /viashare = %+v\n /viauser  = %+v", before, after)
	}

	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/shares/viashare/permissions/users/target", `{"level":"read-write"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("share-route grant = %d, want %d (body=%q)", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	if rec := doAuthedRequest(t, router, token, http.MethodPut,
		"/api/v1/users/target", `{"share_permissions":{"/viauser":"read-write"}}`); rec.Code != http.StatusOK {
		t.Fatalf("user-route grant = %d, want %d (body=%q)", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Both grants must have landed in the control plane, or an equal pair of
	// ACLs would prove nothing.
	for _, shareName := range []string{"/viashare", "/viauser"} {
		perm, err := cpStore.GetUserSharePermission(context.Background(), "target", shareName)
		if err != nil {
			t.Fatalf("read back grant on %q: %v", shareName, err)
		}
		if perm == nil || perm.Permission != string(models.PermissionReadWrite) {
			t.Fatalf("grant on %q not persisted: %+v", shareName, perm)
		}
	}

	viaShare := rootACL(t, rt, "/viashare")
	viaUser := rootACL(t, rt, "/viauser")

	// Name the subject: the grantee's ACE must be present on both roots.
	const grantee = "4242@localdomain"
	hasGrantee := func(a *acl.ACL) bool {
		if a == nil {
			return false
		}
		for _, ace := range a.ACEs {
			if ace.Who == grantee {
				return true
			}
		}
		return false
	}
	if !hasGrantee(viaShare) {
		t.Fatalf("share route did not project the grantee ACE %q; got %+v", grantee, viaShare)
	}
	if !hasGrantee(viaUser) {
		t.Errorf("user route wrote the grant but never projected it: root ACL of /viauser has no %q ACE; got %+v\n"+
			"the grantee cannot traverse a share root owned by uid 0 mode 0755 without it", grantee, viaUser)
	}
	if !reflect.DeepEqual(viaShare, viaUser) {
		t.Errorf("same grant, different root ACL by endpoint:\n share route = %+v\n user route  = %+v", viaShare, viaUser)
	}
}
