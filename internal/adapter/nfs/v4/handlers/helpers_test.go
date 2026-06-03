package handlers

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/auth"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

func TestBuildV4AuthContext_ValidHandle(t *testing.T) {
	pfs := pseudofs.New()
	pfs.Rebuild([]string{"/export"})
	h := NewHandler(nil, pfs) // nil registry -- basic auth context

	uid := uint32(1000)
	gid := uint32(1000)
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
		GIDs:       []uint32{100, 200},
	}

	// Use a valid file handle format: "shareName:uuid"
	handle := []byte("/export:00000000-0000-0000-0000-000000000001")

	authCtx, shareName, err := h.buildV4AuthContext(ctx, handle)
	if err != nil {
		t.Fatalf("buildV4AuthContext error: %v", err)
	}

	if shareName != "/export" {
		t.Errorf("shareName = %q, want %q", shareName, "/export")
	}

	if authCtx == nil {
		t.Fatal("authCtx is nil")
	}

	if authCtx.ClientAddr != "192.168.1.100:9999" {
		t.Errorf("ClientAddr = %q, want %q", authCtx.ClientAddr, "192.168.1.100:9999")
	}

	if authCtx.AuthMethod != "unix" {
		t.Errorf("AuthMethod = %q, want %q", authCtx.AuthMethod, "unix")
	}

	if authCtx.Identity == nil {
		t.Fatal("Identity is nil")
	}

	if authCtx.Identity.UID == nil || *authCtx.Identity.UID != 1000 {
		t.Errorf("UID = %v, want 1000", authCtx.Identity.UID)
	}

	if authCtx.Identity.Username != "uid:1000" {
		t.Errorf("Username = %q, want %q", authCtx.Identity.Username, "uid:1000")
	}
}

func TestBuildV4AuthContext_InvalidHandle(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs)

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
	}

	// Invalid handle (no colon separator)
	handle := []byte("invalid-handle-no-colon")

	_, _, err := h.buildV4AuthContext(ctx, handle)
	if err == nil {
		t.Fatal("expected error for invalid handle, got nil")
	}
}

func TestBuildV4AuthContext_NilRegistry(t *testing.T) {
	pfs := pseudofs.New()
	h := NewHandler(nil, pfs) // nil registry

	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:9999",
		AuthFlavor: 0, // AUTH_NULL
	}

	handle := []byte("/export:00000000-0000-0000-0000-000000000001")

	authCtx, shareName, err := h.buildV4AuthContext(ctx, handle)
	if err != nil {
		t.Fatalf("expected no error with nil registry, got: %v", err)
	}

	if shareName != "/export" {
		t.Errorf("shareName = %q, want %q", shareName, "/export")
	}

	if authCtx.AuthMethod != "anonymous" {
		t.Errorf("AuthMethod = %q, want %q for AUTH_NULL", authCtx.AuthMethod, "anonymous")
	}

	// With nil registry, ShareReadOnly defaults to false
	if authCtx.ShareReadOnly {
		t.Error("ShareReadOnly should be false with nil registry")
	}
}

// TestBuildV4AuthContext_FailsClosedOnMappingFailure verifies the G1 security
// fix: when a registry is present but identity mapping fails (e.g. the handle
// decodes to a share that is not registered), buildV4AuthContext must fail
// closed with an error rather than silently falling back to the original,
// UNMAPPED identity. This mirrors the v3 BuildAuthContextWithMapping behaviour
// and prevents a stale handle for a deleted/renamed share from bypassing
// squash rules (a root client must not remain root).
func TestBuildV4AuthContext_FailsClosedOnMappingFailure(t *testing.T) {
	// The fixture registers share "/export". We hand the handler a valid-format
	// handle for a DIFFERENT share that is NOT registered (e.g. a stale handle
	// for a deleted/renamed share), so ApplyIdentityMapping fails for it.
	fixture := newRealFSTestFixture(t, "/export")

	staleHandle := []byte("/deleted-share:00000000-0000-0000-0000-000000000001")

	uid := uint32(0) // root -- squash is meant to map this away
	gid := uint32(0)
	ctx := &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &uid,
		GID:        &gid,
	}

	authCtx, _, err := fixture.handler.buildV4AuthContext(ctx, staleHandle)
	if err == nil {
		t.Fatal("expected fail-closed error on identity mapping failure, got nil")
	}
	if authCtx != nil {
		t.Errorf("expected nil authCtx on mapping failure, got %+v", authCtx)
	}
}

// v4PermIdentityStore is a minimal IdentityStore for the buildV4AuthContext
// export-squash policy tests. It resolves a single UID→user and a fixed
// permission for that user.
type v4PermIdentityStore struct {
	knownUID uint32
	user     *models.User
	perm     models.SharePermission
}

func (s *v4PermIdentityStore) GetUser(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (s *v4PermIdentityStore) ValidateCredentials(context.Context, string, string) (*models.User, error) {
	return nil, models.ErrInvalidCredentials
}
func (s *v4PermIdentityStore) ListUsers(context.Context) ([]*models.User, error) { return nil, nil }
func (s *v4PermIdentityStore) GetGuestUser(context.Context, string) (*models.User, error) {
	return nil, errors.New("guest disabled")
}
func (s *v4PermIdentityStore) GetGroup(context.Context, string) (*models.Group, error) {
	return nil, models.ErrGroupNotFound
}
func (s *v4PermIdentityStore) ListGroups(context.Context) ([]*models.Group, error) { return nil, nil }
func (s *v4PermIdentityStore) GetUserGroups(context.Context, string) ([]*models.Group, error) {
	return nil, nil
}
func (s *v4PermIdentityStore) GetUserByID(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (s *v4PermIdentityStore) IsGuestEnabled(context.Context, string) bool { return false }
func (s *v4PermIdentityStore) ResolveSharePermission(context.Context, *models.User, string) (models.SharePermission, error) {
	return s.perm, nil
}
func (s *v4PermIdentityStore) GetUserByUID(_ context.Context, uid uint32) (*models.User, error) {
	if s.user != nil && uid == s.knownUID {
		return s.user, nil
	}
	return nil, models.ErrUserNotFound
}

func v4PolicyCtx(uid uint32) *types.CompoundContext {
	u, g := uid, uid
	return &types.CompoundContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:9999",
		AuthFlavor: 1, // AUTH_UNIX
		UID:        &u,
		GID:        &g,
	}
}

// TestBuildV4AuthContext_AppliesExportSquashPolicy verifies the #1047 fix: the
// NFSv4 auth-context builder applies the SAME export-squash permission policy as
// NFSv3, via the shared auth.ResolveSharePermission. Three behaviors that v4
// previously skipped:
//   - default_permission=none denies an unknown UID
//   - SquashRootToAdmin promotes root (UID 0) — no denial under none-default
//   - permission=read coerces ShareReadOnly=true
func TestBuildV4AuthContext_AppliesExportSquashPolicy(t *testing.T) {
	handle := []byte("/export:00000000-0000-0000-0000-000000000001")

	t.Run("default_permission=none denies unknown UID", func(t *testing.T) {
		fx := newRealFSTestFixture(t, "/export")
		fx.rt.SetIdentityStoreForTesting(&v4PermIdentityStore{})
		if err := fx.rt.SetSharePolicyForTesting("/export", "none", models.SquashNone); err != nil {
			t.Fatalf("set policy: %v", err)
		}

		_, _, err := fx.handler.buildV4AuthContext(v4PolicyCtx(1234), handle)
		if !errors.Is(err, auth.ErrShareAccessDenied) {
			t.Fatalf("expected ErrShareAccessDenied for unknown UID under default_permission=none, got %v", err)
		}
	})

	t.Run("SquashRootToAdmin promotes root", func(t *testing.T) {
		fx := newRealFSTestFixture(t, "/export")
		fx.rt.SetIdentityStoreForTesting(&v4PermIdentityStore{})
		// default_permission=none would deny a non-root unknown UID, but root
		// must be promoted, not denied.
		if err := fx.rt.SetSharePolicyForTesting("/export", "none", models.SquashRootToAdmin); err != nil {
			t.Fatalf("set policy: %v", err)
		}

		authCtx, _, err := fx.handler.buildV4AuthContext(v4PolicyCtx(0), handle)
		if err != nil {
			t.Fatalf("root must not be denied under root_to_admin: %v", err)
		}
		if authCtx == nil {
			t.Fatal("authCtx is nil for promoted root")
		}
	})

	t.Run("permission=read coerces read-only", func(t *testing.T) {
		fx := newRealFSTestFixture(t, "/export")
		uid := uint32(1000)
		fx.rt.SetIdentityStoreForTesting(&v4PermIdentityStore{
			knownUID: uid,
			user:     &models.User{ID: "u1", Username: "alice", UID: &uid},
			perm:     models.PermissionRead,
		})
		if err := fx.rt.SetSharePolicyForTesting("/export", "read-write", models.SquashNone); err != nil {
			t.Fatalf("set policy: %v", err)
		}

		authCtx, _, err := fx.handler.buildV4AuthContext(v4PolicyCtx(uid), handle)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !authCtx.ShareReadOnly {
			t.Errorf("ShareReadOnly = false, want true for permission=read")
		}
	})
}

func TestEncodeChangeInfo4(t *testing.T) {
	t.Run("atomic true", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, true, 100, 200)

		reader := bytes.NewReader(buf.Bytes())

		atomic, err := xdr.DecodeUint32(reader)
		if err != nil {
			t.Fatalf("decode atomic: %v", err)
		}
		if atomic != 1 {
			t.Errorf("atomic = %d, want 1", atomic)
		}

		before, err := xdr.DecodeUint64(reader)
		if err != nil {
			t.Fatalf("decode before: %v", err)
		}
		if before != 100 {
			t.Errorf("before = %d, want 100", before)
		}

		after, err := xdr.DecodeUint64(reader)
		if err != nil {
			t.Fatalf("decode after: %v", err)
		}
		if after != 200 {
			t.Errorf("after = %d, want 200", after)
		}
	})

	t.Run("atomic false", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, false, 50, 75)

		reader := bytes.NewReader(buf.Bytes())

		atomic, _ := xdr.DecodeUint32(reader)
		if atomic != 0 {
			t.Errorf("atomic = %d, want 0", atomic)
		}

		before, _ := xdr.DecodeUint64(reader)
		if before != 50 {
			t.Errorf("before = %d, want 50", before)
		}

		after, _ := xdr.DecodeUint64(reader)
		if after != 75 {
			t.Errorf("after = %d, want 75", after)
		}
	})

	t.Run("encoding size", func(t *testing.T) {
		var buf bytes.Buffer
		encodeChangeInfo4(&buf, true, 0, 0)

		// Expected: 4 bytes (bool) + 8 bytes (before) + 8 bytes (after) = 20 bytes
		if buf.Len() != 20 {
			t.Errorf("encoded size = %d, want 20", buf.Len())
		}
	})
}
