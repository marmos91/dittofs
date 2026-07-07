//go:build integration

package store

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// setupShareForSIDPerms creates the prerequisite stores + a share and returns
// the store, share name, and share ID.
func setupShareForSIDPerms(t *testing.T) (*GORMStore, string, string) {
	t.Helper()
	st := openSQLiteFileStore(t)
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	metaID, err := st.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "m", Type: "memory"})
	if err != nil {
		t.Fatalf("create metadata store: %v", err)
	}
	blkID, err := st.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "b", Kind: models.BlockStoreKindLocal, Type: "fs"})
	if err != nil {
		t.Fatalf("create block store: %v", err)
	}
	shareID, err := st.CreateShare(ctx, &models.Share{Name: "/export", MetadataStoreID: metaID, LocalBlockStoreID: blkID})
	if err != nil {
		t.Fatalf("create share: %v", err)
	}
	return st, "/export", shareID
}

func TestSIDSharePermission_CRUDAndResolve(t *testing.T) {
	st, share, shareID := setupShareForSIDPerms(t)
	ctx := context.Background()

	const groupSID = "S-1-5-21-111-222-333-1104"
	const userSID = "S-1-5-21-111-222-333-1200"

	// Grant a group SID read-write with an allocated GID, and a user SID read.
	if err := st.SetSIDSharePermission(ctx, &models.SIDSharePermission{
		SID: groupSID, ShareID: shareID, ShareName: share, Permission: string(models.PermissionReadWrite),
		IsGroup: true, UnixID: 1104, DisplayName: "CUBBIT\\Cubbit",
	}); err != nil {
		t.Fatalf("set group SID grant: %v", err)
	}
	if err := st.SetSIDSharePermission(ctx, &models.SIDSharePermission{
		SID: userSID, ShareID: shareID, ShareName: share, Permission: string(models.PermissionRead),
		IsGroup: false, UnixID: 1200, DisplayName: "alice",
	}); err != nil {
		t.Fatalf("set user SID grant: %v", err)
	}

	t.Run("list returns both grants", func(t *testing.T) {
		perms, err := st.GetShareSIDPermissions(ctx, share)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(perms) != 2 {
			t.Fatalf("got %d SID grants, want 2", len(perms))
		}
	})

	t.Run("resolve by SID takes the highest matching", func(t *testing.T) {
		got, err := st.ResolveSharePermissionForSIDs(ctx, []string{"S-1-0-0", groupSID}, share)
		if err != nil {
			t.Fatalf("resolve by SIDs: %v", err)
		}
		if got != models.PermissionReadWrite {
			t.Errorf("resolve [group] = %q, want read-write", got)
		}
	})

	t.Run("resolve by SID with no match is none", func(t *testing.T) {
		got, err := st.ResolveSharePermissionForSIDs(ctx, []string{"S-1-0-0"}, share)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != models.PermissionNone {
			t.Errorf("resolve [none] = %q, want none", got)
		}
	})

	t.Run("resolve by unix IDs: group GID matches", func(t *testing.T) {
		got, err := st.ResolveSharePermissionForUnixIDs(ctx, 9999, []uint32{1104}, share)
		if err != nil {
			t.Fatalf("resolve by unix ids: %v", err)
		}
		if got != models.PermissionReadWrite {
			t.Errorf("resolve gid 1104 = %q, want read-write", got)
		}
	})

	t.Run("resolve by unix IDs: user UID matches", func(t *testing.T) {
		got, err := st.ResolveSharePermissionForUnixIDs(ctx, 1200, nil, share)
		if err != nil {
			t.Fatalf("resolve by unix ids: %v", err)
		}
		if got != models.PermissionRead {
			t.Errorf("resolve uid 1200 = %q, want read", got)
		}
	})

	t.Run("resolve by unix IDs: a group grant does NOT match on UID", func(t *testing.T) {
		// The group SID's UnixID (1104) must not authorize a user whose UID is
		// 1104 — group grants match GIDs only.
		got, err := st.ResolveSharePermissionForUnixIDs(ctx, 1104, nil, share)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got != models.PermissionNone {
			t.Errorf("group GID leaked into UID match: got %q, want none", got)
		}
	})

	t.Run("re-grant upserts the level", func(t *testing.T) {
		if err := st.SetSIDSharePermission(ctx, &models.SIDSharePermission{
			SID: groupSID, ShareID: shareID, ShareName: share, Permission: string(models.PermissionAdmin), IsGroup: true, UnixID: 1104,
		}); err != nil {
			t.Fatalf("re-grant: %v", err)
		}
		got, _ := st.ResolveSharePermissionForSIDs(ctx, []string{groupSID}, share)
		if got != models.PermissionAdmin {
			t.Errorf("after re-grant = %q, want admin", got)
		}
		perms, _ := st.GetShareSIDPermissions(ctx, share)
		if len(perms) != 2 {
			t.Errorf("re-grant created a duplicate row: %d grants, want 2", len(perms))
		}
	})

	t.Run("delete removes the grant", func(t *testing.T) {
		if err := st.DeleteSIDSharePermission(ctx, groupSID, share); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got, _ := st.ResolveSharePermissionForSIDs(ctx, []string{groupSID}, share)
		if got != models.PermissionNone {
			t.Errorf("after delete = %q, want none", got)
		}
	})

	t.Run("delete by display name (name-based revoke, no LDAP)", func(t *testing.T) {
		// Re-grant the group with a DisplayName, then revoke by that name.
		if err := st.SetSIDSharePermission(ctx, &models.SIDSharePermission{
			SID: groupSID, ShareID: shareID, ShareName: share, Permission: string(models.PermissionRead),
			IsGroup: true, UnixID: 1104, DisplayName: "Cubbit",
		}); err != nil {
			t.Fatalf("re-grant: %v", err)
		}
		// A user-form grant of the same display name must NOT be removed by a
		// group revoke (is_group disambiguates).
		if err := st.SetSIDSharePermission(ctx, &models.SIDSharePermission{
			SID: "S-1-5-21-111-222-333-1300", ShareID: shareID, ShareName: share,
			Permission: string(models.PermissionRead), IsGroup: false, UnixID: 1300, DisplayName: "Cubbit",
		}); err != nil {
			t.Fatalf("grant user with same name: %v", err)
		}

		if err := st.DeleteSIDSharePermissionsByDisplayName(ctx, share, "Cubbit", true); err != nil {
			t.Fatalf("delete by display name: %v", err)
		}
		if got, _ := st.ResolveSharePermissionForSIDs(ctx, []string{groupSID}, share); got != models.PermissionNone {
			t.Errorf("group grant should be revoked by name, still = %q", got)
		}
		if got, _ := st.ResolveSharePermissionForSIDs(ctx, []string{"S-1-5-21-111-222-333-1300"}, share); got != models.PermissionRead {
			t.Errorf("user grant of same name must survive a group revoke, got %q", got)
		}
	})
}
