package metadata_test

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckPermissions_PerUserReadOnlyStripsWrite asserts the #1276 fix: when a
// user's PER-USER share permission is read-only (AuthContext.ShareReadOnly), the
// metadata permission funnel strips write+delete even on a share whose stored
// ShareOptions.ReadOnly is false and whose POSIX mode would grant write. This is
// the NFS path (no handle flag); SMB shares the same funnel.
func TestCheckPermissions_PerUserReadOnlyStripsWrite(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1000), uint32(1000)

	// mode 0o666 owned by the requester — POSIX grants read+write.
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "rw.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666, UID: uid, GID: gid})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Baseline (read-write user): write granted.
	rw := f.authContext(uid, gid)
	got, err := f.service.CheckPermissions(rw, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	require.NotZero(t, got&metadata.PermissionWrite, "precondition: write should be granted for read-write user")

	// Read-only user: read granted, write+delete denied.
	ro := f.authContext(uid, gid)
	ro.ShareReadOnly = true

	gotRead, err := f.service.CheckPermissions(ro, handle, metadata.PermissionRead)
	require.NoError(t, err)
	require.NotZero(t, gotRead&metadata.PermissionRead, "read must stay allowed on a read-only share")

	gotWrite, err := f.service.CheckPermissions(ro, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	if gotWrite&(metadata.PermissionWrite|metadata.PermissionDelete) != 0 {
		t.Errorf("per-user read-only must strip write+delete, got 0x%x", gotWrite)
	}
}

// TestCheckPermissions_PerUserReadOnlyStripsWriteWithACL asserts the ceiling
// also beats an ALLOW ACL that would grant write — the per-user read-only flag
// is enforced after ACL evaluation.
func TestCheckPermissions_PerUserReadOnlyStripsWriteWithACL(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1001), uint32(1001)
	allowAll := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "acl.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o600, UID: uid, GID: gid, ACL: allowAll})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	ro := f.authContext(uid, gid)
	ro.ShareReadOnly = true

	gotWrite, err := f.service.CheckPermissions(ro, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if gotWrite&metadata.PermissionWrite != 0 {
		t.Errorf("per-user read-only must beat an ALLOW ACL write grant, got 0x%x", gotWrite)
	}
}

// TestCheckParentCreateAccess_PerUserReadOnlyDenies asserts the create-path
// ceiling: a read-only user cannot create an entry under an ALLOW-granting
// parent DACL on an otherwise read-write share.
func TestCheckParentCreateAccess_PerUserReadOnlyDenies(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1002), uint32(1002)
	allowAll := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "rodir",
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777, UID: uid, GID: gid, ACL: allowAll})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
	require.NoError(t, err)

	ro := f.authContext(uid, gid)
	ro.ShareReadOnly = true

	err = f.service.CheckParentCreateAccess(ro, dirHandle, false)
	if err == nil {
		t.Fatal("CheckParentCreateAccess returned nil for read-only user, want ErrAccessDenied")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAccessDenied {
		t.Fatalf("CheckParentCreateAccess err = %v, want StoreError{Code: ErrAccessDenied}", err)
	}

	// And the POSIX-only parent-write path (no ACL) must deny too.
	if err := f.service.CheckParentWriteAccess(ro, f.rootHandle); err == nil {
		t.Fatal("CheckParentWriteAccess returned nil for read-only user on POSIX-writable parent, want denied")
	}
}
