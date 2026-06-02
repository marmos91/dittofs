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

	// MS-DTYP §2.4.3 GENERIC_* bits (pre-expansion form a client may send).
	rightGenericRead    uint32 = 0x80000000
	rightGenericWrite   uint32 = 0x40000000
	rightGenericExecute uint32 = 0x20000000
	rightGenericAll     uint32 = 0x10000000
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

// TestCheckFileAccess_MaximumAllowedAloneNeverDenies covers the baseline
// MAXIMUM_ALLOWED contract: when the client requests MAXIMUM_ALLOWED with NO
// explicit bits, the server must return whatever the DACL allows as the
// granted mask (no STATUS_ACCESS_DENIED), regardless of how restrictive the
// DACL is. Per MS-SMB2 §3.3.5.9 paragraph 8, this never-deny guarantee only
// applies to MAX-alone; explicit non-MAX bits are validated separately (see
// TestCheckFileAccess_MaximumAllowedPlusExplicitDeniedBitDenies).
func TestCheckFileAccess_MaximumAllowedAloneNeverDenies(t *testing.T) {
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
}

// TestCheckFileAccess_MaximumAllowedPlusExplicitDeniedBitDenies covers
// smbtorture acls.MXAC-NOT-GRANTED (issue #564). When the requester combines
// MAXIMUM_ALLOWED with an EXPLICIT non-MAX access bit that the DACL does not
// grant, the open must fail with STATUS_ACCESS_DENIED. MAXIMUM_ALLOWED only
// suppresses denial for bits the caller did not name outright — it cannot
// rescue an explicitly requested bit that the DACL rejects.
//
// Per MS-SMB2 §3.3.5.9 paragraph 8 and Samba
// source3/smbd/open.c::smbd_calculate_maximum_allowed_access_fsp, which
// passes the requested mask (minus MAX) through se_file_access_check and
// returns NT_STATUS_ACCESS_DENIED when any explicit bit is rejected.
func TestCheckFileAccess_MaximumAllowedPlusExplicitDeniedBitDenies(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// DACL: ALLOW READ_DATA only. WRITE_DATA is not granted to anyone.
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_READ_DATA,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "mxac.txt",
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

	// MAXIMUM_ALLOWED | WRITE_DATA — WRITE_DATA is the explicit bit, the
	// DACL does not grant it, so the open MUST deny.
	_, err = f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightWriteData)
	require.Error(t, err, "MAX|WRITE_DATA against READ-only DACL must deny")
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
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

// TestCheckFileAccess_ReadAttributesAlwaysGrantedFromParent covers MS-FSA
// §2.1.4.13 "Algorithm to Check Access to an Existing File": FILE_READ_ATTRIBUTES
// is unconditionally granted from the containing directory once traverse access
// to the path succeeds. Even a DACL that explicitly omits READ_ATTRIBUTES must
// not block an open that asks for it.
//
// Covers smb2.acls.OWNER (acls.c::test_owner_bits, line 765 loop) where the
// test installs a DACL granting only FILE_WRITE_DATA to the owner and then
// expects an open with desired_access=0x80 (READ_ATTRIBUTES) to succeed.
// Mirrors Samba source3/smbd/open.c::smbd_check_access_rights_fsp setting
// `do_not_check_mask = FILE_READ_ATTRIBUTES`. Refs #559.
func TestCheckFileAccess_ReadAttributesAlwaysGrantedFromParent(t *testing.T) {
	f := newTestFixture(t)

	const rightReadAttributes uint32 = 0x00000080

	ownerUID := uint32(1001)
	ownerSID := "S-1-5-21-1-2-3-2001"

	// DACL grants the owner ONLY FILE_WRITE_DATA — no READ_ATTRIBUTES.
	writeOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + ownerSID,
				AccessMask: acl.ACE4_WRITE_DATA,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "raa.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o644,
			UID:  ownerUID,
			GID:  1001,
			ACL:  writeOnlyACL,
		})
	require.NoError(t, err)

	authCtx := f.authContext(ownerUID, 1001)
	authCtx.Identity.SID = strPtr(ownerSID)

	// Bit 0x80 alone — DACL omits it but MS-FSA always-grants it from parent.
	granted, err := f.service.CheckFileAccess(created, authCtx, rightReadAttributes)
	require.NoError(t, err, "READ_ATTRIBUTES must be granted even when DACL omits it")
	require.Equal(t, rightReadAttributes, granted&rightReadAttributes,
		"expected READ_ATTRIBUTES in granted mask, got 0x%x", granted)

	// READ_ATTRIBUTES combined with the DACL-granted WRITE_DATA — both must
	// be present. Mirrors smb2.acls.OWNER's expected_bits = WRITE_DATA|READ_ATTRIBUTE.
	probe := rightReadAttributes | rightWriteData
	granted, err = f.service.CheckFileAccess(created, authCtx, probe)
	require.NoError(t, err)
	require.Equal(t, probe, granted&probe,
		"expected WRITE_DATA|READ_ATTRIBUTES granted, got 0x%x", granted)

	// READ_DATA must still be denied — the always-grant rule covers only
	// READ_ATTRIBUTES, not the rest of the read namespace.
	_, err = f.service.CheckFileAccess(created, authCtx, rightReadData)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// createSpecificRightsFile is a helper for the GENERIC_* expansion tests: it
// creates a file whose DACL grants exactly `mask` to `sid` and nothing else,
// owned by an unrelated UID so only the DACL (not the POSIX owner fallback)
// governs access.
func createSpecificRightsFile(t *testing.T, f *testFixture, name, sid string, mask uint32) *metadata.File {
	t.Helper()
	a := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        "sid:" + sid,
				AccessMask: mask,
			},
		},
	}
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, name,
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  9999, // not the requester
			GID:  9999,
			ACL:  a,
		})
	require.NoError(t, err)
	return created
}

// TestCheckFileAccess_MaximumAllowedPlusGenericReadGranted covers smbtorture
// smb2.maximum_allowed (max_allowed.c:162). A request of
// MAXIMUM_ALLOWED | GENERIC_READ against a DACL that grants only the specific
// READ rights (FILE_READ_DATA | FILE_READ_EA | FILE_READ_ATTRIBUTES |
// READ_CONTROL | SYNCHRONIZE) must be GRANTED with STATUS_OK.
//
// Regression guard: before expanding GENERIC_* in `explicit`, the raw
// GENERIC_READ bit (0x80000000) survived into the subset check while the DACL
// evaluation produced only specific rights, so effective&explicit could never
// equal explicit and the open was wrongly denied STATUS_ACCESS_DENIED.
func TestCheckFileAccess_MaximumAllowedPlusGenericReadGranted(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// FILE_GENERIC_READ specific-rights set (MS-DTYP §2.4.3 file mapping).
	readMask := uint32(acl.ACE4_READ_DATA | acl.ACE4_READ_NAMED_ATTRS |
		acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE)
	created := createSpecificRightsFile(t, f, "max_generic_read.txt", requesterSID, readMask)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightGenericRead)
	require.NoError(t, err, "MAXIMUM_ALLOWED|GENERIC_READ against a READ-granting DACL must succeed")
	require.NotZero(t, granted&acl.ACE4_READ_DATA, "expected READ_DATA in granted mask, got 0x%x", granted)
	require.Zero(t, granted&uint32(0xF0000000), "granted mask must not retain raw GENERIC_* bits, got 0x%x", granted)
}

// TestCheckFileAccess_MaximumAllowedPlusGenericWriteGranted is the WRITE
// symmetric case of the smb2.maximum_allowed fix.
func TestCheckFileAccess_MaximumAllowedPlusGenericWriteGranted(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// FILE_GENERIC_WRITE specific-rights set.
	writeMask := uint32(acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA |
		acl.ACE4_WRITE_NAMED_ATTRS | acl.ACE4_WRITE_ATTRIBUTES |
		acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE)
	created := createSpecificRightsFile(t, f, "max_generic_write.txt", requesterSID, writeMask)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightGenericWrite)
	require.NoError(t, err, "MAXIMUM_ALLOWED|GENERIC_WRITE against a WRITE-granting DACL must succeed")
	require.NotZero(t, granted&acl.ACE4_WRITE_DATA, "expected WRITE_DATA in granted mask, got 0x%x", granted)
	require.Zero(t, granted&uint32(0xF0000000), "granted mask must not retain raw GENERIC_* bits, got 0x%x", granted)
}

// TestCheckFileAccess_MaximumAllowedPlusGenericExecuteGranted is the EXECUTE
// symmetric case of the smb2.maximum_allowed fix.
func TestCheckFileAccess_MaximumAllowedPlusGenericExecuteGranted(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// FILE_GENERIC_EXECUTE specific-rights set.
	execMask := uint32(acl.ACE4_READ_ATTRIBUTES | acl.ACE4_EXECUTE |
		acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE)
	created := createSpecificRightsFile(t, f, "max_generic_exec.txt", requesterSID, execMask)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightGenericExecute)
	require.NoError(t, err, "MAXIMUM_ALLOWED|GENERIC_EXECUTE against an EXECUTE-granting DACL must succeed")
	require.NotZero(t, granted&acl.ACE4_EXECUTE, "expected EXECUTE in granted mask, got 0x%x", granted)
	require.Zero(t, granted&uint32(0xF0000000), "granted mask must not retain raw GENERIC_* bits, got 0x%x", granted)
}

// TestCheckFileAccess_MaximumAllowedPlusGenericReadBestEffort proves that
// GENERIC_* bits are best-effort under MAXIMUM_ALLOWED: MAX|GENERIC_READ against
// a DACL that grants only WRITE_DATA must SUCCEED, granting only what the DACL
// allows rather than failing because the GENERIC_READ-expanded READ rights are
// absent. This matches Samba's se_access_check, where the MAXIMUM_ALLOWED branch
// computes the maximal grantable set and never strict-enforces generic-derived
// bits (smb2.maximum_allowed.maximum_allowed grants MAX|GENERIC_EXECUTE on a
// read-only DACL). The strict explicit-bit gate (smb2.acls.MXAC-NOT-GRANTED)
// applies only to DIRECTLY-named specific rights — see
// TestCheckFileAccess_MaximumAllowedPlusExplicitDeniedBitDenies.
func TestCheckFileAccess_MaximumAllowedPlusGenericReadBestEffort(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// DACL grants ONLY WRITE_DATA — none of the GENERIC_READ specific rights.
	created := createSpecificRightsFile(t, f, "max_generic_read_best_effort.txt", requesterSID, acl.ACE4_WRITE_DATA)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightGenericRead)
	require.NoError(t, err, "MAXIMUM_ALLOWED|GENERIC_READ is best-effort: generic-derived bits never force a denial")
	require.NotZero(t, granted&acl.ACE4_WRITE_DATA, "expected the DACL-granted WRITE_DATA in granted mask, got 0x%x", granted)
	require.Zero(t, granted&acl.ACE4_READ_DATA, "READ_DATA is not granted by the DACL and must not appear, got 0x%x", granted)
}

// TestCheckFileAccess_MaximumAllowedPlusGenericAllStrictlyEnforced proves the
// best-effort relaxation is scoped to GENERIC_READ / GENERIC_EXECUTE only.
// GENERIC_ALL (and GENERIC_WRITE) are NOT in Samba's max_allowed.c ok_mask, so a
// MAX open naming GENERIC_ALL against a read-only DACL must DENY when the mapped
// write/all-access rights aren't granted. Without this, the GENERIC_ALL open in
// smb2.maximum_allowed.maximum_allowed (i=28) would wrongly succeed, leave a
// share-conflicting handle open, and break the subsequent set_sd WRITE_DAC open
// with STATUS_SHARING_VIOLATION (max_allowed.c:197).
func TestCheckFileAccess_MaximumAllowedPlusGenericAllStrictlyEnforced(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// FILE_GENERIC_READ specific-rights set only — no WRITE/EXECUTE/owner rights.
	readMask := uint32(acl.ACE4_READ_DATA | acl.ACE4_READ_NAMED_ATTRS |
		acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE)
	created := createSpecificRightsFile(t, f, "max_generic_all_deny.txt", requesterSID, readMask)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	_, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|rightGenericAll)
	require.Error(t, err, "MAXIMUM_ALLOWED|GENERIC_ALL against a READ-only DACL must deny — GENERIC_ALL is not best-effort")
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// TestCheckFileAccess_MaximumAllowedPlusExplicitWriteDeniedStillDenies is the
// companion true-deny control: a DIRECTLY-named specific right (FILE_WRITE_DATA,
// no generic expansion involved) that the DACL denies must still fail under
// MAXIMUM_ALLOWED. Guards against the best-effort relaxation leaking into the
// strict explicit-bit gate (smb2.acls.MXAC-NOT-GRANTED, #564).
func TestCheckFileAccess_MaximumAllowedPlusExplicitWriteDeniedStillDenies(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	// DACL grants ONLY READ_DATA; the request names WRITE_DATA explicitly.
	created := createSpecificRightsFile(t, f, "max_explicit_write_deny.txt", requesterSID, acl.ACE4_READ_DATA)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	_, err := f.service.CheckFileAccess(created, authCtx, rightMaxAllowed|uint32(acl.ACE4_WRITE_DATA))
	require.Error(t, err, "MAXIMUM_ALLOWED|FILE_WRITE_DATA against a READ-only DACL must deny the directly-named bit")
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}

// TestCheckFileAccess_StrictGenericReadGranted exercises the strict (non-MAX)
// branch: a bare GENERIC_READ request (no MAXIMUM_ALLOWED) against a
// READ-granting DACL must succeed once the generic bit is expanded.
func TestCheckFileAccess_StrictGenericReadGranted(t *testing.T) {
	f := newTestFixture(t)

	requesterSID := "S-1-5-21-1-2-3-2001"
	readMask := uint32(acl.ACE4_READ_DATA | acl.ACE4_READ_NAMED_ATTRS |
		acl.ACE4_READ_ATTRIBUTES | acl.ACE4_READ_ACL | acl.ACE4_SYNCHRONIZE)
	created := createSpecificRightsFile(t, f, "strict_generic_read.txt", requesterSID, readMask)

	authCtx := f.authContext(1001, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightGenericRead)
	require.NoError(t, err, "GENERIC_READ against a READ-granting DACL must succeed in strict mode")
	require.NotZero(t, granted&acl.ACE4_READ_DATA, "expected READ_DATA in granted mask, got 0x%x", granted)
	require.Zero(t, granted&uint32(0xF0000000), "granted mask must not retain raw GENERIC_* bits, got 0x%x", granted)
}
