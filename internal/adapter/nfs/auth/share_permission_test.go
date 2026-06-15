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

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1234))
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

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1234))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ReadOnly {
		t.Errorf("ReadOnly = true, want false for read-write default")
	}
}

// Behavior 2: SquashRootToAdmin (and the default empty squash) promote root
// (UID 0) to admin with username "root".
func TestResolveSharePermission_RootPromotedToAdmin(t *testing.T) {
	for _, sq := range []models.SquashMode{"", models.SquashNone, models.SquashRootToAdmin, models.SquashAllToAdmin} {
		store := newPermMockStore() // no user mapping; root path must not need one
		share := &runtime.Share{
			Name:              "/export",
			DefaultPermission: "none", // would deny a non-root unknown UID
			Squash:            sq,
		}

		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(0))
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

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000))
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

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000))
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
			res, err := ResolveSharePermission(context.Background(), tc.store, tc.share, "/export", "127.0.0.1:1", tc.uid)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ReadOnly || res.Username != "" {
				t.Errorf("expected zero result, got %+v", res)
			}
		})
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

	_, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil)
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
		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.ReadOnly {
			t.Errorf("AUTH_NULL on read-only export: ReadOnly = false, want true")
		}
	})

	t.Run("default_permission=read", func(t *testing.T) {
		share := &runtime.Share{Name: "/export", DefaultPermission: "read"}
		res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil)
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

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", nil)
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

	res, err := ResolveSharePermission(context.Background(), store, share, "/export", "127.0.0.1:1", ptrUID(1000))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.ReadOnly {
		t.Errorf("ReadOnly = false, want true (share.ReadOnly forces it)")
	}
}
