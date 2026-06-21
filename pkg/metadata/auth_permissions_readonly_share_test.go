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

// TestSetFileAttributes_PerUserReadOnlyDeniesOwnerMutation asserts the SETATTR
// ceiling: a read-only user may not mutate even a file they OWN — chmod, chown,
// and (most importantly) installing a permissive ACL must all be denied. Without
// the ceiling the owner-bypass in SetFileAttributes would let a read-only owner
// escalate access on their own file.
func TestSetFileAttributes_PerUserReadOnlyDeniesOwnerMutation(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1500), uint32(1500)
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "owned.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: uid, GID: gid})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	ro := f.authContext(uid, gid) // the OWNER
	ro.ShareReadOnly = true

	newMode := uint32(0o777)
	openACL := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	cases := map[string]*metadata.SetAttrs{
		"chmod": {Mode: &newMode},
		"acl":   {ACL: openACL},
		"chown": {UID: metadata.Uint32Ptr(2000)},
		"utime": {MtimeNow: true},
	}
	for name, attrs := range cases {
		_, err := f.service.SetFileAttributes(ro, handle, attrs)
		if err == nil {
			t.Errorf("%s: SetFileAttributes returned nil for read-only owner, want denied", name)
			continue
		}
		var storeErr *metadata.StoreError
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrAccessDenied {
			t.Errorf("%s: err = %v, want StoreError{Code: ErrAccessDenied}", name, err)
		}
	}

	// Sanity: the same owner on a read-write share CAN chmod.
	rw := f.authContext(uid, gid)
	if _, err := f.service.SetFileAttributes(rw, handle, &metadata.SetAttrs{Mode: &newMode}); err != nil {
		t.Fatalf("precondition: owner chmod on read-write share should succeed, got %v", err)
	}
}

// TestLockFile_PerUserReadOnlyDeniesExclusiveLock asserts a read-only user
// cannot acquire an exclusive (write) byte-range lock, while a shared (read)
// lock is still allowed.
func TestLockFile_PerUserReadOnlyDeniesExclusiveLock(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1600), uint32(1600)
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "lockme.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o666, UID: uid, GID: gid})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	ro := f.authContext(uid, gid)
	ro.ShareReadOnly = true

	// Exclusive (write) lock: denied.
	err = f.service.LockFile(ro, handle, metadata.FileLock{SessionID: 1, Offset: 0, Length: 100, Exclusive: true})
	if err == nil {
		t.Error("LockFile(exclusive) returned nil for read-only user, want denied")
	}

	// Shared (read) lock: allowed.
	if err := f.service.LockFile(ro, handle, metadata.FileLock{SessionID: 2, Offset: 0, Length: 100, Exclusive: false}); err != nil {
		t.Errorf("LockFile(shared) for read-only user err = %v, want nil", err)
	}
}
