package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// TestSharePermissionMutationsInvalidateAuthCache pins the invalidation to the
// grant mutation rather than to the root-ACL projection that used to carry it.
//
// The test runtime has no metadata service, so ReconcileShareRootACL returns at
// its first guard without writing anything — the same shape as the four early
// returns and the swallowed SetFileAttributes error a live server hits when the
// metadata store is unavailable. An invalidation hung off the projection's tail
// is skipped on every one of those paths, so a revoke returns 204 while every
// SMB connection keeps the grant for its lifetime and every NFSv3 client keeps
// it until the TTL expires.
func TestSharePermissionMutationsInvalidateAuthCache(t *testing.T) {
	const testSID = "S-1-5-21-1111111111-2222222222-3333333333-1105"

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"grant user", http.MethodPut, "/api/v1/shares/export/permissions/users/target", `{"level":"read-write"}`, http.StatusNoContent},
		{"revoke user", http.MethodDelete, "/api/v1/shares/export/permissions/users/target", "", http.StatusNoContent},
		{"grant group", http.MethodPut, "/api/v1/shares/export/permissions/groups/staff", `{"level":"read"}`, http.StatusNoContent},
		{"revoke group", http.MethodDelete, "/api/v1/shares/export/permissions/groups/staff", "", http.StatusNoContent},
		{"grant sid", http.MethodPut, "/api/v1/shares/export/permissions/sids/" + testSID, `{"level":"read"}`, http.StatusNoContent},
		{"revoke sid", http.MethodDelete, "/api/v1/shares/export/permissions/sids/" + testSID, "", http.StatusNoContent},
		{"delete share", http.MethodDelete, "/api/v1/shares/export", "", http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router, token, cpStore, calls := newInvalidateTestRouter(t)
			seedUser(t, cpStore, "target")
			seedGroup(t, cpStore, "staff")
			seedShare(t, cpStore, "/export")

			// Seed the grant the revoke cases remove, so a revoke that fires
			// only because nothing was there cannot pass by accident.
			seedSharePermissions(t, cpStore, testSID)

			before := calls.Load()
			rec := doAuthedRequest(t, router, token, tc.method, tc.path, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("%s %s = %d, want %d (body=%q)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
			}
			if calls.Load() == before {
				t.Errorf("%s %s did not fire the auth-cache invalidation event; "+
					"an established SMB tree keeps the old share access for the life of the connection",
					tc.method, tc.path)
			}
		})
	}
}

// seedSharePermissions writes one grant of each kind on /export so the revoke
// cases have something to remove.
func seedSharePermissions(t *testing.T, cpStore interface {
	SetUserSharePermission(context.Context, *models.UserSharePermission) error
	SetGroupSharePermission(context.Context, *models.GroupSharePermission) error
	SetSIDSharePermission(context.Context, *models.SIDSharePermission) error
	GetUser(context.Context, string) (*models.User, error)
	GetGroup(context.Context, string) (*models.Group, error)
}, testSID string) {
	t.Helper()
	ctx := context.Background()

	user, err := cpStore.GetUser(ctx, "target")
	if err != nil {
		t.Fatalf("get seeded user: %v", err)
	}
	if err := cpStore.SetUserSharePermission(ctx, &models.UserSharePermission{
		UserID: user.ID, ShareID: "share-/export", ShareName: "/export",
		Permission: string(models.PermissionRead),
	}); err != nil {
		t.Fatalf("seed user grant: %v", err)
	}

	group, err := cpStore.GetGroup(ctx, "staff")
	if err != nil {
		t.Fatalf("get seeded group: %v", err)
	}
	if err := cpStore.SetGroupSharePermission(ctx, &models.GroupSharePermission{
		GroupID: group.ID, ShareID: "share-/export", ShareName: "/export",
		Permission: string(models.PermissionRead),
	}); err != nil {
		t.Fatalf("seed group grant: %v", err)
	}

	if err := cpStore.SetSIDSharePermission(ctx, &models.SIDSharePermission{
		SID: testSID, ShareID: "share-/export", ShareName: "/export",
		Permission: string(models.PermissionRead),
	}); err != nil {
		t.Fatalf("seed sid grant: %v", err)
	}
}
