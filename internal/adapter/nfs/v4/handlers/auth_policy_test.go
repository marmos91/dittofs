package handlers

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/attrs"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// emptyIdentityStore is a models.IdentityStore with no users: GetUserByUID
// returns no match so every UID resolves to guest, exercising the
// default_permission policy in auth.ResolveSharePermission. ResolveSharePermission
// short-circuits to "allow with defaults" when the identity store is nil, so a
// non-nil (but empty) store is required to drive the guest-denial path.
type emptyIdentityStore struct{}

func (emptyIdentityStore) GetUser(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (emptyIdentityStore) ValidateCredentials(context.Context, string, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (emptyIdentityStore) ListUsers(context.Context) ([]*models.User, error) { return nil, nil }
func (emptyIdentityStore) GetGuestUser(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (emptyIdentityStore) GetGroup(context.Context, string) (*models.Group, error) {
	return nil, models.ErrGroupNotFound
}
func (emptyIdentityStore) ListGroups(context.Context) ([]*models.Group, error) { return nil, nil }
func (emptyIdentityStore) GetUserGroups(context.Context, string) ([]*models.Group, error) {
	return nil, nil
}
func (emptyIdentityStore) ResolveSharePermission(context.Context, *models.User, string) (models.SharePermission, error) {
	return models.PermissionNone, nil
}
func (emptyIdentityStore) GetUserByUID(context.Context, uint32) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (emptyIdentityStore) GetUserByID(context.Context, string) (*models.User, error) {
	return nil, models.ErrUserNotFound
}
func (emptyIdentityStore) IsGuestEnabled(context.Context, string) bool { return false }

// ============================================================================
// Export auth-flavor policy enforcement (#1253)
// ============================================================================
//
// NFSv4.1 has no MOUNT call, so the per-share RequireKerberos / AllowAuthSys
// checks the v3 MOUNT handler applies never ran on v4 — a share that required
// Kerberos (or disallowed AUTH_SYS) was mountable over AUTH_SYS on v4.1,
// silently bypassing the export policy. buildV4AuthContext now mirrors the v3
// logic at the first real-FS op and surfaces the refusal as NFS4ERR_WRONGSEC
// (not NFS4ERR_SERVERFAULT) so the client retries with the correct flavor.
//
// These tests drive the policy through a real op handler (GETATTR over an
// AUTH_UNIX context) and assert the status the client actually sees.

// getAttrStatusForFile builds an AUTH_UNIX GETATTR for the given file handle
// and returns the resulting CompoundResult status.
func getAttrStatusForFile(fx *realFSTestFixture, fileHandle metadata.FileHandle) uint32 {
	ctx := newRealFSContext(1000, 1000) // AUTH_UNIX
	ctx.CurrentFH = make([]byte, len(fileHandle))
	copy(ctx.CurrentFH, fileHandle)

	var requested []uint32
	attrs.SetBit(&requested, attrs.FATTR4_TYPE)

	return fx.handler.getAttrRealFS(ctx, requested).Status
}

// TestExportAuthPolicy_DefaultAllowsAuthSys is the sanity case: the default
// fixture share permits AUTH_SYS, so an AUTH_UNIX GETATTR succeeds.
func TestExportAuthPolicy_DefaultAllowsAuthSys(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4_OK {
		t.Fatalf("default share AUTH_UNIX GETATTR status = %d, want NFS4_OK (%d)", status, types.NFS4_OK)
	}
}

// TestExportAuthPolicy_RequireKerberosRejectsAuthSys verifies that a share
// requiring Kerberos rejects an AUTH_SYS request with NFS4ERR_WRONGSEC (not
// NFS4ERR_SERVERFAULT, which would mask the policy refusal).
func TestExportAuthPolicy_RequireKerberosRejectsAuthSys(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// allowAuthSys=true but requireKerberos=true: an AUTH_SYS request must be
	// refused because the share mandates Kerberos.
	if err := fx.rt.SetExportAuthPolicyForTesting("/export", true, true); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("RequireKerberos share AUTH_UNIX GETATTR status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

// TestExportAuthPolicy_DisallowAuthSysRejectsAuthSys verifies that a share that
// disallows AUTH_SYS rejects an AUTH_UNIX request with NFS4ERR_WRONGSEC.
func TestExportAuthPolicy_DisallowAuthSysRejectsAuthSys(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// allowAuthSys=false: an AUTH_SYS request must be refused.
	if err := fx.rt.SetExportAuthPolicyForTesting("/export", false, false); err != nil {
		t.Fatalf("SetExportAuthPolicyForTesting: %v", err)
	}

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4ERR_WRONGSEC {
		t.Fatalf("AllowAuthSys=false share AUTH_UNIX GETATTR status = %d, want NFS4ERR_WRONGSEC (%d)",
			status, types.NFS4ERR_WRONGSEC)
	}
}

// TestExportAuthPolicy_DefaultPermissionNoneMapsToAccess pins the other half of
// the status-mapping fix: a share-permission denial (default_permission=none for
// an unmapped principal — the krb5 machine-principal-maps-to-nobody case) must
// surface as NFS4ERR_ACCESS, not NFS4ERR_SERVERFAULT. The fixture identity store
// has no user for UID 1000, so it resolves to guest and default_permission=none
// denies it.
func TestExportAuthPolicy_DefaultPermissionNoneMapsToAccess(t *testing.T) {
	fx := newRealFSTestFixture(t, "/export")
	fileHandle := fx.createTestFile(t, fx.rootHandle, "f.txt", metadata.FileTypeRegular, 0o644, 1000, 1000)

	// A non-nil identity store with no users is required: ResolveSharePermission
	// allows-with-defaults when the store is nil, so without this the guest
	// default_permission=none branch never runs.
	fx.rt.SetIdentityStoreForTesting(emptyIdentityStore{})
	if err := fx.rt.SetSharePolicyForTesting("/export", "none", models.SquashNone); err != nil {
		t.Fatalf("SetSharePolicyForTesting: %v", err)
	}

	if status := getAttrStatusForFile(fx, fileHandle); status != types.NFS4ERR_ACCESS {
		t.Fatalf("default_permission=none AUTH_UNIX GETATTR status = %d, want NFS4ERR_ACCESS (%d)",
			status, types.NFS4ERR_ACCESS)
	}
}
