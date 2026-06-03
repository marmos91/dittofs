package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// These tests cover Service.ComputeMaximalAccess — the single source of truth
// for the MS-SMB2 §2.2.13.2 MxAc maximal-access reply. ComputeMaximalAccess is
// a pure evaluation over (file, authCtx) and touches no Service store fields,
// so a zero-valued Service is sufficient.

func mkCtx(uid, gid uint32) *metadata.AuthContext {
	u, g := uid, gid
	return &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: &u, GID: &g},
	}
}

const genericAll = uint32(0x001F01FF)

func TestComputeMaximalAccess_POSIX(t *testing.T) {
	svc := &metadata.Service{}

	t.Run("OwnerGetsGenericAll", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o755}}
		require.Equal(t, genericAll, svc.ComputeMaximalAccess(file, mkCtx(1000, 1000)))
	})

	t.Run("GroupReadExecute", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o750}}
		// Group bits r-x => read + execute bundles.
		want := uint32(0x00120089) | uint32(0x001200A0)
		require.Equal(t, want, svc.ComputeMaximalAccess(file, mkCtx(2000, 1000)))
	})

	t.Run("SupplementaryGroupMatch", func(t *testing.T) {
		// Requester's primary GID differs, but a supplementary GID matches the
		// file's owning group → group bits apply.
		u, g := uint32(2000), uint32(9999)
		authCtx := &metadata.AuthContext{
			Context:  context.Background(),
			Identity: &metadata.Identity{UID: &u, GID: &g, GIDs: []uint32{9999, 1000}},
		}
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o740}}
		// Group bits r-- => read bundle only.
		require.Equal(t, uint32(0x00120089), svc.ComputeMaximalAccess(file, authCtx))
	})

	t.Run("OtherNoAccessGetsMinimal", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o770}}
		// Other bits --- => minimal READ_CONTROL | SYNCHRONIZE.
		require.Equal(t, uint32(0x00120000), svc.ComputeMaximalAccess(file, mkCtx(3000, 3000)))
	})

	t.Run("OtherWriteOnly", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o002}}
		require.Equal(t, uint32(0x00120116), svc.ComputeMaximalAccess(file, mkCtx(3000, 3000)))
	})
}

func TestComputeMaximalAccess_RootBypass(t *testing.T) {
	svc := &metadata.Service{}

	t.Run("NilACLRootIsNotSpecialCased", func(t *testing.T) {
		// On the nil-ACL POSIX path, the only short-circuit is owner == file.UID.
		// Root (UID 0) over a non-owned mode-000 file gets the minimal set, NOT
		// GENERIC_ALL — this preserves the legacy handler behavior verbatim
		// (the ACL path is where root short-circuits to GENERIC_ALL).
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o000}}
		require.Equal(t, uint32(0x00120000), svc.ComputeMaximalAccess(file, mkCtx(0, 0)))
	})

	t.Run("NilACLRootOwnsFile", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 0, GID: 0, Mode: 0o000}}
		require.Equal(t, genericAll, svc.ComputeMaximalAccess(file, mkCtx(0, 0)))
	})

	t.Run("WithACL", func(t *testing.T) {
		// Even a deny-everyone DACL must not restrict root — UID 0 short-circuits
		// to GENERIC_ALL, mirroring CheckFileAccessWithParentGeneric.
		file := &metadata.File{FileAttr: metadata.FileAttr{
			UID: 1000, GID: 1000, Mode: 0o000,
			ACL: &acl.ACL{ACEs: []acl.ACE{{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: genericAll,
			}}},
		}}
		require.Equal(t, genericAll, svc.ComputeMaximalAccess(file, mkCtx(0, 0)))
	})
}

func TestComputeMaximalAccess_ACL(t *testing.T) {
	svc := &metadata.Service{}

	t.Run("DenyACEClearsWriteBitsNoOwnerShortCircuit", func(t *testing.T) {
		// Owner would get GENERIC_ALL via POSIX, but the DACL denies WRITE before
		// the allow-all ACE. DENY-first ordering must clear those bits — the
		// owner short-circuit must NOT fire on the ACL path (MS-SMB2 §2.2.13.2).
		file := &metadata.File{FileAttr: metadata.FileAttr{
			UID: 1000, GID: 1000, Mode: 0o700,
			ACL: &acl.ACL{ACEs: []acl.ACE{
				{Type: acl.ACE4_ACCESS_DENIED_ACE_TYPE, Who: acl.SpecialOwner, AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA},
				{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialOwner, AccessMask: genericAll},
			}},
		}}
		access := svc.ComputeMaximalAccess(file, mkCtx(1000, 1000))
		require.Zero(t, access&acl.ACE4_WRITE_DATA, "WRITE_DATA must be cleared")
		require.Zero(t, access&acl.ACE4_APPEND_DATA, "APPEND_DATA must be cleared")
		require.NotZero(t, access&acl.ACE4_READ_DATA, "READ_DATA must be granted by allow-all ACE")
		require.NotEqual(t, genericAll, access, "owner short-circuit must not leak onto ACL path")
	})

	t.Run("AllowACEPlusOwnerImplicitBits", func(t *testing.T) {
		// MS-DTYP §2.5.3.2 layers RC|WRITE_DAC on top of explicit owner grants;
		// WRITE_OWNER is admin-only and excluded for a non-admin owner (#563).
		explicit := uint32(acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_SYNCHRONIZE)
		want := explicit | uint32(acl.ACE4_READ_ACL|acl.ACE4_WRITE_ACL)
		file := &metadata.File{FileAttr: metadata.FileAttr{
			UID: 1000, GID: 1000, Mode: 0o700,
			ACL: &acl.ACL{ACEs: []acl.ACE{{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialOwner, AccessMask: explicit}}},
		}}
		access := svc.ComputeMaximalAccess(file, mkCtx(1000, 1000))
		require.Equal(t, want, access)
		require.Zero(t, access&acl.ACE4_WRITE_OWNER, "non-admin owner must not get WRITE_OWNER (#563)")
	})

	t.Run("AdminOwnerGetsWriteOwnerImplicit", func(t *testing.T) {
		u, g := uint32(1000), uint32(1000)
		adminSID := "S-1-5-32-544" // BUILTIN\Administrators
		authCtx := &metadata.AuthContext{
			Context:  context.Background(),
			Identity: &metadata.Identity{UID: &u, GID: &g, SID: &adminSID},
		}
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o755, ACL: &acl.ACL{}}}
		want := uint32(acl.ACE4_READ_ACL | acl.ACE4_WRITE_ACL | acl.ACE4_WRITE_OWNER)
		require.Equal(t, want, svc.ComputeMaximalAccess(file, authCtx))
	})

	t.Run("EmptyACLGrantsOnlyOwnerImplicitBits", func(t *testing.T) {
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 1000, GID: 1000, Mode: 0o755, ACL: &acl.ACL{}}}
		want := uint32(acl.ACE4_READ_ACL | acl.ACE4_WRITE_ACL)
		require.Equal(t, want, svc.ComputeMaximalAccess(file, mkCtx(1000, 1000)))
	})

	t.Run("AnonymousDoesNotInheritRootOwnerImplicitBits", func(t *testing.T) {
		// Anonymous (no Identity) against a root-owned file must not collapse
		// onto OWNER@ and pick up the implicit RC|WRITE_DAC grant (#540).
		file := &metadata.File{FileAttr: metadata.FileAttr{UID: 0, GID: 0, Mode: 0o700, ACL: &acl.ACL{}}}
		anon := &metadata.AuthContext{Context: context.Background()}
		require.Zero(t, svc.ComputeMaximalAccess(file, anon))
	})
}
