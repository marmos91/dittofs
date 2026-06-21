package metadata_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckParentWriteAccess_ACLDenyReturnsAccessDenied asserts that
// CheckParentWriteAccess (the public wrapper SMB CREATE calls) returns an
// ErrAccessDenied StoreError when the parent directory's ACL denies WRITE
// for the requester's SID, even on a writable share.
func TestCheckParentWriteAccess_ACLDenyReturnsAccessDenied(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Create a directory under root with an explicit ACL that denies write to
	// the requester's SID. Note: the directory itself, not a child file,
	// carries the ACL — that's the parent-of-CREATE we want to block.
	denyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "denied-dir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  1001,
			ACL:  denyACL,
		})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	sid := requesterSID
	authCtx.Identity.SID = &sid
	authCtx.ShareReadOnly = false

	err = f.service.CheckParentWriteAccess(authCtx, dirHandle)
	if err == nil {
		t.Fatalf("CheckParentWriteAccess returned nil, want ErrAccessDenied")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAccessDenied {
		t.Fatalf("CheckParentWriteAccess err = %v, want StoreError{Code: ErrAccessDenied}", err)
	}
}

// TestCheckParentWriteAccess_NoACLAllowsAdd asserts the POSIX-only happy path
// is unchanged: a parent with no explicit ACL falls through to the POSIX write
// check (the root dir is mode 0o777, so a non-owner gets write via "other").
func TestCheckParentWriteAccess_NoACLAllowsAdd(t *testing.T) {
	f := newTestFixture(t)
	authCtx := f.authContext(1001, 1001)
	if err := f.service.CheckParentWriteAccess(authCtx, f.rootHandle); err != nil {
		t.Fatalf("CheckParentWriteAccess on POSIX-writable parent err = %v, want nil", err)
	}
}

// TestCheckParentCreateAccess_DenyAddFileBlocksFileAllowsDir verifies that
// a parent DACL denying only SEC_DIR_ADD_FILE (ACE4_ADD_FILE, 0x02) blocks
// file creates but still permits subdirectory creates. Regresses the
// smb2.create.mkdir-visible fix: PermissionWrite lumps ADD_FILE and
// ADD_SUBDIRECTORY together (mask 0x06), so the old combined check
// incorrectly denied directory creates whenever ADD_FILE was denied.
func TestCheckParentCreateAccess_DenyAddFileBlocksFileAllowsDir(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	denyAddFile := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: acl.ACE4_ADD_FILE,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "deny-add-file",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  1001,
			ACL:  denyAddFile,
		})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	sid := requesterSID
	authCtx.Identity.SID = &sid

	// File create: must be denied by the ADD_FILE deny ACE.
	err = f.service.CheckParentCreateAccess(authCtx, dirHandle, false)
	if err == nil {
		t.Fatalf("CheckParentCreateAccess(file) returned nil, want ErrAccessDenied")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAccessDenied {
		t.Fatalf("CheckParentCreateAccess(file) err = %v, want StoreError{Code: ErrAccessDenied}", err)
	}

	// Directory create: ADD_SUBDIRECTORY is not denied, must succeed.
	if err := f.service.CheckParentCreateAccess(authCtx, dirHandle, true); err != nil {
		t.Fatalf("CheckParentCreateAccess(dir) err = %v, want nil (ADD_SUBDIRECTORY not denied)", err)
	}
}

// TestCheckParentCreateAccess_DenyAddSubdirBlocksDirAllowsFile is the
// symmetric case: deny ADD_SUBDIRECTORY only, expect file create to
// succeed and subdirectory create to be denied.
func TestCheckParentCreateAccess_DenyAddSubdirBlocksDirAllowsFile(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	denyAddSubdir := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: acl.ACE4_ADD_SUBDIRECTORY,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "deny-add-subdir",
		&metadata.FileAttr{
			Type: metadata.FileTypeDirectory,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  1001,
			ACL:  denyAddSubdir,
		})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	sid := requesterSID
	authCtx.Identity.SID = &sid

	// Directory create: must be denied by the ADD_SUBDIRECTORY deny ACE.
	err = f.service.CheckParentCreateAccess(authCtx, dirHandle, true)
	if err == nil {
		t.Fatalf("CheckParentCreateAccess(dir) returned nil, want ErrAccessDenied")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAccessDenied {
		t.Fatalf("CheckParentCreateAccess(dir) err = %v, want StoreError{Code: ErrAccessDenied}", err)
	}

	// File create: ADD_FILE is not denied, must succeed.
	if err := f.service.CheckParentCreateAccess(authCtx, dirHandle, false); err != nil {
		t.Fatalf("CheckParentCreateAccess(file) err = %v, want nil (ADD_FILE not denied)", err)
	}
}

// TestCheckParentCreateAccess_NoACLFallsBackToWriteCheck verifies the
// POSIX fallback path: when the parent has no ACL, evaluation falls
// through to the shared PermissionWrite check so mode bits govern (the root
// dir is mode 0o777, so a non-owner gets write via "other").
func TestCheckParentCreateAccess_NoACLFallsBackToWriteCheck(t *testing.T) {
	f := newTestFixture(t)
	authCtx := f.authContext(1001, 1001)

	if err := f.service.CheckParentCreateAccess(authCtx, f.rootHandle, false); err != nil {
		t.Fatalf("CheckParentCreateAccess(file, no-ACL) err = %v, want nil", err)
	}
	if err := f.service.CheckParentCreateAccess(authCtx, f.rootHandle, true); err != nil {
		t.Fatalf("CheckParentCreateAccess(dir, no-ACL) err = %v, want nil", err)
	}
}
