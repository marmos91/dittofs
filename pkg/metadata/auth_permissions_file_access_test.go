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

// TestCheckFileAccess_NilACLPosixFallbackOwner covers the documented divergence
// from buildDACL: when file.ACL == nil, enforcement uses POSIX mode bits (not
// the synthesized Windows-default SD). Owner gets GENERIC_ALL.
func TestCheckFileAccess_NilACLPosixFallbackOwner(t *testing.T) {
	f := newTestFixture(t)

	ownerUID := uint32(1001)
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "posix.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o600,
			UID:  ownerUID,
			GID:  1001,
			// ACL: nil — POSIX fallback path.
		})
	require.NoError(t, err)

	authCtx := f.authContext(ownerUID, 1001)

	granted, err := f.service.CheckFileAccess(created, authCtx, rightReadData|rightWriteData)
	require.NoError(t, err)
	require.Equal(t, rightReadData|rightWriteData, granted&(rightReadData|rightWriteData))
}

// TestCheckFileAccess_NilACLPosixFallbackOtherDeniesWrite covers the
// non-owner POSIX path: a file with mode 0o600 (rw owner only) must deny a
// WRITE_DATA request from a non-owner non-group user.
func TestCheckFileAccess_NilACLPosixFallbackOtherDeniesWrite(t *testing.T) {
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

	_, err = f.service.CheckFileAccess(created, authCtx, rightWriteData|rightAppendData)
	require.Error(t, err)
	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	require.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
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
