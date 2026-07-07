package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// permMockStore is a configurable IdentityStore for ResolveSharePermission
// tests: it maps UIDs to users and users to a fixed resolved permission.
type permMockStore struct {
	usersByUID map[uint32]*models.User
	perm       models.SharePermission
	permErr    error

	// sidPerm is returned by ResolveSharePermissionForUnixIDs when the login's
	// UID or one of its GIDs is present in sidMatchIDs (a direct AD/SID grant,
	// #1528). Left zero, the SID path resolves to none — existing tests unchanged.
	sidPerm     models.SharePermission
	sidMatchIDs map[uint32]bool
}

func newPermMockStore() *permMockStore {
	return &permMockStore{usersByUID: make(map[uint32]*models.User)}
}

func (m *permMockStore) GetUser(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (m *permMockStore) ValidateCredentials(context.Context, string, string) (*models.User, error) {
	return nil, models.ErrInvalidCredentials
}
func (m *permMockStore) ListUsers(context.Context) ([]*models.User, error) { return nil, nil }
func (m *permMockStore) GetGuestUser(context.Context, string) (*models.User, error) {
	return nil, errors.New("guest disabled")
}
func (m *permMockStore) GetGroup(context.Context, string) (*models.Group, error) {
	return nil, models.ErrGroupNotFound
}
func (m *permMockStore) ListGroups(context.Context) ([]*models.Group, error) { return nil, nil }
func (m *permMockStore) GetUserGroups(context.Context, string) ([]*models.Group, error) {
	return nil, nil
}
func (m *permMockStore) GetUserByID(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (m *permMockStore) IsGuestEnabled(context.Context, string) bool { return false }

func (m *permMockStore) ResolveSharePermission(context.Context, *models.User, string) (models.SharePermission, error) {
	return m.perm, m.permErr
}

func (m *permMockStore) GetUserByUID(_ context.Context, uid uint32) (*models.User, error) {
	user, ok := m.usersByUID[uid]
	if !ok {
		return nil, models.ErrUserNotFound
	}
	return user, nil
}

func (m *permMockStore) ResolveSharePermissionForUnixIDs(_ context.Context, uid uint32, gids []uint32, _ string) (models.SharePermission, error) {
	if m.sidMatchIDs == nil {
		return models.PermissionNone, nil
	}
	if m.sidMatchIDs[uid] {
		return m.sidPerm, nil
	}
	for _, g := range gids {
		if m.sidMatchIDs[g] {
			return m.sidPerm, nil
		}
	}
	return models.PermissionNone, nil
}

func ptrUID(v uint32) *uint32 { return &v }

// Behavior 1: default_permission=none denies an unknown UID. This is the v4
// regression the fix closes — v4 previously never applied this policy.
func TestResolveSharePermission_DefaultPermissionNoneDeniesUnknownUID(t *testing.T) {
	store := newPermMockStore() // no user registered for the UID
	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "none",
		Squash:            models.SquashNone,
	}

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1234), nil)
	if !errors.Is(err, ErrShareAccessDenied) {
		t.Fatalf("expected ErrShareAccessDenied for unknown UID under default_permission=none, got %v", err)
	}
}

func TestResolveSharePermission_DefaultPermissionReadWriteAllowsUnknownUID(t *testing.T) {
	store := newPermMockStore()
	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "read-write",
		Squash:            models.SquashNone,
	}

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1234), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ReadOnly {
		t.Errorf("ReadOnly = true, want false for read-write default")
	}
}

// Behavior 2: SquashRootToAdmin (and none / all_to_admin) promote root (UID 0)
// to admin with username "root". Empty squash is NOT in this set — it defaults
// to root_to_guest (see TestResolveSharePermission_EmptySquashDoesNotPromoteRoot).
func TestResolveSharePermission_RootPromotedToAdmin(t *testing.T) {
	for _, sq := range []models.SquashMode{models.SquashNone, models.SquashRootToAdmin, models.SquashAllToAdmin} {
		store := newPermMockStore() // no user mapping; root path must not need one
		share := &runtime.Share{
			Name:              "/export",
			DefaultPermission: "none", // would deny a non-root unknown UID
			Squash:            sq,
		}

		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(0), nil)
		if err != nil {
			t.Fatalf("squash=%q: unexpected error: %v", sq, err)
		}
		if res.Username != "root" {
			t.Errorf("squash=%q: Username = %q, want \"root\"", sq, res.Username)
		}
		if res.ReadOnly {
			t.Errorf("squash=%q: ReadOnly = true, want false (admin)", sq)
		}
	}
}

// Behavior 2b: an empty/unset squash defaults to root_to_guest, so root (UID 0)
// is NOT promoted to admin — it is gated by default_permission like any guest.
// With default_permission=none this denies root, matching conventional NFS
// root_squash (the new default).
func TestResolveSharePermission_EmptySquashDoesNotPromoteRoot(t *testing.T) {
	store := newPermMockStore() // no UID 0 mapping → root falls through to guest
	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "none",
		Squash:            "", // unset → DefaultSquashMode (root_to_guest)
	}

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(0), nil)
	if !errors.Is(err, ErrShareAccessDenied) {
		t.Fatalf("empty squash + default none: err = %v, want ErrShareAccessDenied (root not promoted)", err)
	}
}

// Behavior 3: a known user resolved to permission "read" is coerced read-only.
func TestResolveSharePermission_ReadPermissionCoercesReadOnly(t *testing.T) {
	store := newPermMockStore()
	store.usersByUID[1000] = &models.User{ID: "u1", Username: "alice", UID: ptrUID(1000)}
	store.perm = models.PermissionRead

	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "read-write",
		Squash:            models.SquashNone,
	}

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ReadOnly {
		t.Errorf("ReadOnly = false, want true for permission=read")
	}
	if res.Username != "alice" {
		t.Errorf("Username = %q, want \"alice\"", res.Username)
	}
}

func TestResolveSharePermission_KnownUserPermissionNoneDenied(t *testing.T) {
	store := newPermMockStore()
	store.usersByUID[1000] = &models.User{ID: "u1", Username: "alice", UID: ptrUID(1000)}
	store.perm = models.PermissionNone

	share := &runtime.Share{Name: "/export", DefaultPermission: "read-write", Squash: models.SquashNone}

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000), nil)
	if !errors.Is(err, ErrShareAccessDenied) {
		t.Fatalf("expected ErrShareAccessDenied for known user with permission=none, got %v", err)
	}
}

// Guard rails: a nil identity store or share means no policy information is
// available, so resolution must allow with defaults (never deny). A nil UID
// (AUTH_NULL) is NOT in this set: an anonymous caller is gated by the share's
// default_permission policy (see the AnonymousAuthNull tests below).
func TestResolveSharePermission_NilInputsAllow(t *testing.T) {
	cases := []struct {
		name  string
		store models.IdentityStore
		share *runtime.Share
		uid   *uint32
	}{
		{"nil store", nil, &runtime.Share{}, ptrUID(0)},
		{"nil share", newPermMockStore(), nil, ptrUID(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ResolveSharePermission(context.Background(), tc.store, tc.share, "/export", "127.0.0.1:1", tc.uid, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ReadOnly || res.Username != "" {
				t.Errorf("expected zero result, got %+v", res)
			}
		})
	}
}

// When no identity store is available but the share is read-only, the result
// must still be read-only — an unavailable/unconfigured identity store must not
// silently make a read-only export writable.
func TestResolveSharePermission_NilStoreHonoursShareReadOnly(t *testing.T) {
	share := &runtime.Share{Name: "/export", ReadOnly: true}

	res, err := ResolveSharePermission(context.Background(), nil, share, "/export", "127.0.0.1:1", ptrUID(1000), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ReadOnly {
		t.Errorf("nil identity store with read-only share: ReadOnly = false, want true")
	}
}

// Negative control for the AUTH_NULL share-permission bypass: an anonymous
// caller (nil UID, e.g. AUTH_NULL credentials) must be DENIED on a share with
// default_permission=none. Before the fix the nil-UID early return granted
// full read-write access, nullifying the export permission model.
func TestResolveSharePermission_AnonymousAuthNullDeniedWhenDefaultNone(t *testing.T) {
	store := newPermMockStore()
	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "none",
		Squash:            models.SquashNone,
	}

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil, nil)
	if !errors.Is(err, ErrShareAccessDenied) {
		t.Fatalf("anonymous AUTH_NULL caller on default_permission=none: expected ErrShareAccessDenied, got %v", err)
	}
}

// An anonymous AUTH_NULL caller on a read-only export must be coerced to
// read-only, not granted read-write. Before the fix the nil-UID early return
// returned ReadOnly=false even on read-only exports.
func TestResolveSharePermission_AnonymousAuthNullCoercedReadOnly(t *testing.T) {
	store := newPermMockStore()

	t.Run("share read-only flag", func(t *testing.T) {
		share := &runtime.Share{Name: "/export", DefaultPermission: "read-write", ReadOnly: true}
		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.ReadOnly {
			t.Errorf("AUTH_NULL on read-only export: ReadOnly = false, want true")
		}
	})

	t.Run("default_permission=read", func(t *testing.T) {
		share := &runtime.Share{Name: "/export", DefaultPermission: "read"}
		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.ReadOnly {
			t.Errorf("AUTH_NULL on default_permission=read: ReadOnly = false, want true")
		}
	})
}

// A read-write share with no read-only flag still allows the anonymous caller —
// the fix must not break legitimate anon-allowed shares.
func TestResolveSharePermission_AnonymousAuthNullAllowedReadWrite(t *testing.T) {
	store := newPermMockStore()
	share := &runtime.Share{Name: "/export", DefaultPermission: "read-write"}

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ReadOnly {
		t.Errorf("AUTH_NULL on read-write export: ReadOnly = true, want false")
	}
}

// share.ReadOnly forces read-only even when the resolved permission would allow
// writes — verifies the share-level flag is ORed in.
func TestResolveSharePermission_ShareReadOnlyForcesReadOnly(t *testing.T) {
	store := newPermMockStore()
	store.usersByUID[1000] = &models.User{ID: "u1", Username: "alice", UID: ptrUID(1000)}
	store.perm = models.PermissionReadWrite

	share := &runtime.Share{Name: "/export", DefaultPermission: "read-write", Squash: models.SquashNone, ReadOnly: true}

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ReadOnly {
		t.Errorf("ReadOnly = false, want true (share.ReadOnly forces it)")
	}
}

// Issue #1419: a share created by uid=501 with the new default_permission=none
// must deny a different uid (2001) that has no identity mapping. Previously the
// CLI defaulted to "read-write", allowing any uid to mount.
func TestResolveSharePermission_DefaultNoneDeniesUnmappedUID(t *testing.T) {
	store := newPermMockStore() // uid 2001 has no mapping

	share := &runtime.Share{
		Name:              "/export",
		DefaultPermission: "none",
		Squash:            models.SquashNone,
	}

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(2001), nil)
	if !errors.Is(err, ErrShareAccessDenied) {
		t.Fatalf("uid=2001 on default_permission=none: expected ErrShareAccessDenied, got %v", err)
	}
}
