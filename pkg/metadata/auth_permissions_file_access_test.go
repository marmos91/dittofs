package metadata_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// Issue #529: SMB CREATE must enforce DesiredAccess against the file's DACL.
//
// These tests cover MetadataService.CheckFileAccess — the helper SMB CREATE
// calls between the share-mode recheck and the actual file open to gate the
// caller's requested access bits against the stored security descriptor.
//
// MS-DTYP §2.4.3 access-right bits used here:
//   FILE_READ_DATA       0x00000001
//   FILE_WRITE_DATA      0x00000002
//   FILE_APPEND_DATA     0x00000004
//   DELETE               0x00010000
//   READ_CONTROL         0x00020000
//   SYNCHRONIZE          0x00100000
//   MAXIMUM_ALLOWED      0x02000000

const (
	rightReadData    uint32 = 0x00000001
	rightWriteData   uint32 = 0x00000002
	rightAppendData  uint32 = 0x00000004
	rightDelete      uint32 = 0x00010000
	rightReadControl uint32 = 0x00020000
	rightSynchronize uint32 = 0x00100000
	rightMaxAllowed  uint32 = 0x02000000
)

// TestCheckFileAccess_DenyACEOnOwnerDeniesWrite covers smbtorture acls.OWNER /
// acls.DENY1: an explicit DENY ACE targeting the owner's SID must override
// the POSIX owner-everything fallback and refuse write access.
func TestCheckFileAccess_DenyACEOnOwnerDeniesWrite(t *testing.T) {
	f := newTestFixture(t)

	ownerUID := uint32(1001)
	ownerSID := "S-1-5-21-1-2-3-2001"

	denyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        "sid:" + ownerSID,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "owner_denied.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  ownerUID,
			GID:  1001,
			ACL:  denyACL,
		})
	require.NoError(t, err)

	authCtx := f.authContext(ownerUID, 1001)
	authCtx.Identity.SID = strPtr(ownerSID)

	// Request write; expect access denied even though the requester owns the file.
	_, err = f.service.CheckFileAccess(created, authCtx, rightWriteData|rightReadData)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// TestCheckFileAccess_AllowOnlyReadGrantsReadDeniesWrite covers acls.OWNER's
// allow-readonly probe: a file whose DACL grants only READ_DATA must allow
// a CREATE that asks for READ_DATA and deny a CREATE that asks for WRITE_DATA.
func TestCheckFileAccess_AllowOnlyReadGrantsReadDeniesWrite(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "ro.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999, // not the requester
			GID:  9999,
			ACL:  readOnlyACL,
		})
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	// READ probe must succeed.
	granted, err := f.service.CheckFileAccess(created, authCtx, rightReadData|rightSynchronize)
	require.NoError(t, err)
	require.NotZero(t, granted&rightReadData, "expected READ_DATA granted, got 0x%x", granted)

	// WRITE probe must be denied.
	_, err = f.service.CheckFileAccess(created, authCtx, rightWriteData)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// TestCheckFileAccess_MaximumAllowedNeverDenies covers acls.MXAC-NOT-GRANTED:
// when the client requests MAXIMUM_ALLOWED, the server must return whatever
// the DACL allows as the granted mask (no STATUS_ACCESS_DENIED), regardless
// of how restrictive the DACL is.
func TestCheckFileAccess_MaximumAllowedNeverDenies(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// DACL: a single ALLOW ACE for READ_DATA to the requester. No WRITE.
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "max.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999,
			GID:  9999,
			ACL:  readOnlyACL,
		})
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	// MAXIMUM_ALLOWED alone must never deny — even though WRITE etc. aren't
	// in the DACL. Granted bits should reflect what is allowed.
	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed)
	require.NoError(t, err)
	require.NotZero(t, granted&acl.ACE4_READ_DATA, "expected READ_DATA in granted mask, got 0x%x", granted)
	require.Zero(t, granted&acl.ACE4_WRITE_DATA, "WRITE_DATA must not be granted, got 0x%x", granted)

	// MAXIMUM_ALLOWED | WRITE_DATA must also not deny: the MAXIMUM_ALLOWED bit
	// suppresses denial. The handle's effective access reflects the DACL.
	granted, err = f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightWriteData)
	require.NoError(t, err)
	require.Zero(t, granted&acl.ACE4_WRITE_DATA, "WRITE_DATA must not be granted even when requested with MAXIMUM_ALLOWED, got 0x%x", granted)
}

// TestCheckFileAccess_RootBypassGetsEverything documents the root bypass:
// UID 0 always gets the requested access, regardless of the DACL.
func TestCheckFileAccess_RootBypassGetsEverything(t *testing.T) {
	f := newTestFixture(t)

	denyEveryoneACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "root_only.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o000,
			UID:  1001,
			GID:  1001,
			ACL:  denyEveryoneACL,
		})
	require.NoError(t, err)

	authCtx := f.rootContext()

	granted, err := f.service.CheckFileAccess(created, authCtx, rightReadData|rightWriteData|rightDelete)
	require.NoError(t, err)
	require.Equal(t, rightReadData|rightWriteData|rightDelete, granted)
}

// TestCheckFileAccess_NilACLOwnerGrantsRequested covers the nil-ACL path:
// when file.ACL == nil there is no DACL to enforce at the open gate, so the
// requested access bits are granted as-is. Downstream metadata operations
// still apply POSIX-mode enforcement; CheckFileAccess gates only the SMB2
// open, not the per-op read/write/delete.
func TestCheckFileAccess_NilACLOwnerGrantsRequested(t *testing.T) {
	f := newTestFixture(t)

	ownerUID := uint32(1001)
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "posix.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600,
			UID:  ownerUID,
			GID:  1001,
			// ACL: nil — no DACL to enforce.
		})
	require.NoError(t, err)

	authCtx := f.authContext(ownerUID, 1001)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightReadData|rightWriteData)
	require.NoError(t, err)
	require.Equal(t, rightReadData|rightWriteData, granted)
}

// TestCheckFileAccess_NilACLNonOwnerGrantsRequested verifies the open-time
// gate does NOT deny a non-owner's request when file.ACL == nil — even when
// the POSIX mode would deny the same operation downstream. This is the WPTS
// BVT load-bearing case (and smb2.create.multi): CREATE asks for DELETE,
// WRITE_DAC, or higher-namespace rights that the rwx mapping cannot encode,
// and the pre-#529 server granted those opens. We must keep granting them at
// open time; per-op enforcement in read/write/delete catches abuse.
func TestCheckFileAccess_NilACLNonOwnerGrantsRequested(t *testing.T) {
	f := newTestFixture(t)

	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "posix_owner_only.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600,
			UID:  9999,
			GID:  9999,
		})
	require.NoError(t, err)

	authCtx := f.authContext(1001, 1001)

	// Non-owner requesting WRITE_DATA on a 0o600 file. Open succeeds
	// (no DACL to enforce); the actual WRITE op denies via POSIX mode.
	granted, err := f.service.CheckFileAccess(created, authCtx, rightWriteData|rightAppendData)
	require.NoError(t, err)
	require.Equal(t, rightWriteData|rightAppendData, granted)

	// DELETE / WRITE_DAC / WRITE_OWNER — bits the POSIX rwx mapping cannot
	// encode — must also pass the open gate when there is no DACL.
	const rightWriteDac uint32 = 0x00040000
	const rightWriteOwner uint32 = 0x00080000
	probe := rightDelete | rightWriteDac | rightWriteOwner
	granted, err = f.service.CheckFileAccess(created, authCtx, probe)
	require.NoError(t, err)
	require.Equal(t, probe, granted)
}

// TestCheckFileAccess_NilACLMaxAllowedReturnsGenericAll verifies that
// MAXIMUM_ALLOWED on a no-DACL file returns the full GENERIC_ALL set, mirroring
// the MxAc reply produced by computeMaximalAccess.
func TestCheckFileAccess_NilACLMaxAllowedReturnsGenericAll(t *testing.T) {
	f := newTestFixture(t)

	ownerUID := uint32(1001)
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "max_nil_acl.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600,
			UID:  ownerUID,
			GID:  1001,
		})
	require.NoError(t, err)

	authCtx := f.authContext(ownerUID, 1001)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed)
	require.NoError(t, err)
	// GENERIC_ALL = 0x001F01FF
	require.Equal(t, uint32(0x001F01FF), granted)
}

// TestCheckFileAccess_ZeroDesiredAccessIsNoop guards against returning
// ErrAccessDenied for an empty request — empty DesiredAccess is a valid
// shape (e.g., probing existence or paired solely with MAXIMUM_ALLOWED).
func TestCheckFileAccess_ZeroDesiredAccessIsNoop(t *testing.T) {
	f := newTestFixture(t)
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "empty.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  9999,
			GID:  9999,
		})
	require.NoError(t, err)
	authCtx := f.authContext(1001, 1001)
	granted, err := f.service.CheckFileAccess(created, authCtx, 0)
	require.NoError(t, err)
	require.Zero(t, granted)
}

// TestCheckFileAccess_DenyErrorIsStoreError ensures the returned error is a
// *StoreError, which is what common.MapToSMB depends on to produce the
// correct STATUS_ACCESS_DENIED on the SMB wire.
func TestCheckFileAccess_DenyErrorIsStoreError(t *testing.T) {
	f := newTestFixture(t)

	denyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "denied.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999,
			GID:  9999,
			ACL:  denyACL,
		})
	require.NoError(t, err)

	authCtx := f.authContext(1001, 1001)
	_, err = f.service.CheckFileAccess(created, authCtx, rightReadData|rightSynchronize)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("error %v is not *StoreError", err)
	}
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// TestCheckFileAccessWithParent_DeleteOverrideViaParentDeleteChild covers
// issue #547: when a file's own DACL denies DELETE but the parent directory
// grants FILE_DELETE_CHILD to the caller, DELETE on the open must be
// granted (MS-FSA §2.1.4.13, mirrors Samba parent_override_delete in
// source3/smbd/open.c).
//
// Without this override, smbtorture's smb2_deltree algorithm cannot recover
// from a prior subtest that left a restrictive DACL on a child file, and
// the entire acls.* cluster errors at setup_dir with "Unable to deltree".
func TestCheckFileAccessWithParent_DeleteOverrideViaParentDeleteChild(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Parent dir DACL grants FILE_DELETE_CHILD (and basic read) to the
	// requester. This is what synthesize.go::rwxToFullMask produces for the
	// owner of a dir with mode-bit `w`.
	parentACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_DELETE_CHILD | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	parentCreated, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "deldir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
			UID:  requesterUID,
			GID:  1001,
			ACL:  parentACL,
		})
	require.NoError(t, err)

	// Build the child File directly (avoiding CreateFile which would apply
	// parent ACL inheritance and replace the child's DACL). CheckFileAccess
	// only reads File fields, so an in-memory File suffices.
	childACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_READ_ATTRIBUTES | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	childCreated := &metadata.File{
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  requesterUID,
			GID:  1001,
			ACL:  childACL,
		},
	}

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	// Without parent: file's own DACL denies DELETE.
	_, err = f.service.CheckFileAccess(childCreated, authCtx, rightDelete)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)

	// With parent: FILE_DELETE_CHILD on parent grants DELETE on the open.
	granted, err := f.service.CheckFileAccessWithParent(childCreated, parentCreated, authCtx, rightDelete)
	require.NoError(t, err)
	require.Equal(t, rightDelete, granted)
}

// TestCheckFileAccessWithParent_OverrideDoesNotApplyToNonDeleteBits ensures
// the parent FILE_DELETE_CHILD override is scoped to DELETE only. Other
// rights denied by the file's DACL must remain denied even when the parent
// would otherwise grant broad access.
func TestCheckFileAccessWithParent_OverrideDoesNotApplyToNonDeleteBits(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Parent dir grants FULL access including FILE_DELETE_CHILD.
	parentACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	parentCreated, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "broadparent",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
			UID:  requesterUID,
			GID:  1001,
			ACL:  parentACL,
		})
	require.NoError(t, err)

	// Child file denies WRITE.
	childACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	childCreated := &metadata.File{
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  requesterUID,
			GID:  1001,
			ACL:  childACL,
		},
	}

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	// WRITE alone — parent override is delete-only; must still deny.
	_, err = f.service.CheckFileAccessWithParent(childCreated, parentCreated, authCtx, rightWriteData)
	require.Error(t, err)

	// DELETE + WRITE — DELETE is granted via parent, WRITE is not → still deny.
	_, err = f.service.CheckFileAccessWithParent(childCreated, parentCreated, authCtx, rightDelete|rightWriteData)
	require.Error(t, err)
}

// TestCheckFileAccessWithParent_NullParentDACLGrantsDelete pins MS-DTYP
// §2.5.3: a NULL DACL on the parent grants every right to every principal,
// including FILE_DELETE_CHILD, so the parent override grants DELETE on the
// child. This is the load-bearing case for smbtorture deltree recovery —
// new dirs in DittoFS are created without an explicit ACL, so parent.ACL
// is nil for most filesystem state.
func TestCheckFileAccessWithParent_NullParentDACLGrantsDelete(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Parent: created with no explicit ACL → parent.ACL is nil.
	parentCreated, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "nullacldir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
			UID:  requesterUID,
			GID:  1001,
		})
	require.NoError(t, err)
	require.Nil(t, parentCreated.ACL, "parent ACL must be nil for this scenario")

	// Child: restrictive DACL denying DELETE.
	childACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	childCreated := &metadata.File{
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  requesterUID,
			GID:  1001,
			ACL:  childACL,
		},
	}

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccessWithParent(childCreated, parentCreated, authCtx, rightDelete)
	require.NoError(t, err)
	require.Equal(t, rightDelete, granted)
}

// TestCheckFileAccessWithParent_NoOverrideWhenParentLacksDeleteChild verifies
// that when the parent's own DACL also lacks FILE_DELETE_CHILD for the
// caller, the override does NOT grant DELETE — the file remains undeletable.
// This pins the Windows "delete via parent" rule: parent permission, not just
// parent presence, is what overrides.
func TestCheckFileAccessWithParent_NoOverrideWhenParentLacksDeleteChild(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	parentACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	parentCreated, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "lockedparent",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o755,
			UID:  requesterUID,
			GID:  1001,
			ACL:  parentACL,
		})
	require.NoError(t, err)

	childACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA | acl.ACE4_SYNCHRONIZE,
			},
		},
	}
	childCreated := &metadata.File{
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  requesterUID,
			GID:  1001,
			ACL:  childACL,
		},
	}

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	_, err = f.service.CheckFileAccessWithParent(childCreated, parentCreated, authCtx, rightDelete)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}
