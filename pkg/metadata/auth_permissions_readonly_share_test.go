package metadata_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckPermissions_StoreReadOnlyShareBlocksHandleBypass asserts the
// store-level read-only ceiling (ShareOptions.ReadOnly) also beats the SMB
// handle-based write bypass: if a share is toggled read-only while a client
// still holds a previously write-authorized handle (WriteAuthorizedByHandle),
// the per-op check must not let that handle keep writing, even though the
// per-user ctx.ShareReadOnly is false.
func TestCheckPermissions_StoreReadOnlyShareBlocksHandleBypass(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1000), uint32(1000)
	// Create the file BEFORE the share is read-only (a read-only share denies
	// creation).
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "rw.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: uid, GID: gid})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Register the share options entry (the fixture's CreateRootDirectory builds
	// the file tree but not the share-options record) and toggle it read-only at
	// the store level. CreateShare is a no-op-on-exists guard so the test is
	// robust to either fixture shape.
	_ = f.store.CreateShare(context.Background(), &metadata.Share{Name: f.shareName})
	require.NoError(t, f.store.UpdateShareOptions(context.Background(), f.shareName,
		&metadata.ShareOptions{ReadOnly: true}))

	// SMB handle granted write at open; per-user ShareReadOnly is false.
	authCtx := f.authContext(uid, gid)
	authCtx.WriteAuthorizedByHandle = true
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if got&metadata.PermissionWrite != 0 {
		t.Errorf("store-level read-only share must block the handle write bypass, got 0x%x", got)
	}
}

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
// parent DACL on an otherwise read-write share. The denial is an ordinary
// permission denial — the share itself is writable — so it must surface as
// ErrAccessDenied (NFS3ERR_ACCES / EACCES), NOT ErrReadOnly. A squashed/unknown
// uid given the share's default "read" permission lands here; the kernel client
// reports "permission denied", not "read-only file system".
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

	// Per-user read-only on a writable share → ErrAccessDenied (EACCES).
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

// TestCheckParentCreateAccess_StoreReadOnlyShareReturnsErrReadOnly asserts the
// other direction of the same discriminator: when the SHARE itself is read-only
// (ShareOptions.ReadOnly), a create denial surfaces as ErrReadOnly so NFSv3
// returns NFS3ERR_ROFS (EROFS) per RFC 1813 — preserving #1508's intent. This
// guards against a regression that would collapse both cases back to one code.
func TestCheckParentCreateAccess_StoreReadOnlyShareReturnsErrReadOnly(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1003), uint32(1003)
	// ALLOW-everyone DACL so the read-only-share ceiling is the ONLY thing that
	// can deny: the test would not distinguish a regression (read-only check
	// removed) from a missing grant without an explicit ALLOW to override.
	allowAll := &acl.ACL{
		ACEs: []acl.ACE{
			{Type: acl.ACE4_ACCESS_ALLOWED_ACE_TYPE, Who: acl.SpecialEveryone, AccessMask: 0xFFFFFFFF},
		},
	}
	dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "rodir2",
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o777, UID: uid, GID: gid, ACL: allowAll})
	require.NoError(t, err)
	dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
	require.NoError(t, err)

	// Toggle the share read-only at the STORE level; per-user ShareReadOnly stays
	// false so only the store-level flag is in play.
	_ = f.store.CreateShare(context.Background(), &metadata.Share{Name: f.shareName})
	require.NoError(t, f.store.UpdateShareOptions(context.Background(), f.shareName,
		&metadata.ShareOptions{ReadOnly: true}))

	rw := f.authContext(uid, gid)
	rw.ShareReadOnly = false

	err = f.service.CheckParentCreateAccess(rw, dirHandle, false)
	if err == nil {
		t.Fatal("CheckParentCreateAccess returned nil on read-only share, want ErrReadOnly")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrReadOnly {
		t.Fatalf("CheckParentCreateAccess err = %v, want StoreError{Code: ErrReadOnly}", err)
	}
}

// TestCheckParentCreateAccess_NoACLParent_ReadOnlyDiscriminator locks the
// EROFS-vs-EACCES discriminator for the CREATE path when the parent directory
// has NO ACL (the generic POSIX-write fallback in CheckParentCreateAccess).
// TestCheckParentCreateAccess_StoreReadOnlyShareReturnsErrReadOnly already
// covers the ACL branch; this fills the non-ACL sub-path so both directions of
// the fork are pinned:
//   - store-level read-only share (ShareOptions.ReadOnly) → ErrReadOnly (EROFS);
//   - per-user read-only level (ctx.ShareReadOnly on a writable share) → EACCES.
//
// f.rootHandle is a mode-0777 directory with no ACL, so a create there takes the
// file.ACL == nil fallback → checkWritePermission → checkPermission, exercising
// the shareIsReadOnly store-level discriminator rather than the ACL block.
func TestCheckParentCreateAccess_NoACLParent_ReadOnlyDiscriminator(t *testing.T) {
	asStoreErr := func(t *testing.T, err error) *metadata.StoreError {
		t.Helper()
		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.True(t, errors.As(err, &storeErr), "err %v is not a *StoreError", err)
		return storeErr
	}

	t.Run("store-level read-only share is EROFS", func(t *testing.T) {
		f := newTestFixture(t)
		// The fixture already registered the share (via CreateRootDirectory), so
		// CreateShare returns ErrExist here — intentionally ignored; the call only
		// guarantees a share entry for UpdateShareOptions to target.
		_ = f.store.CreateShare(context.Background(), &metadata.Share{Name: f.shareName})
		require.NoError(t, f.store.UpdateShareOptions(context.Background(), f.shareName,
			&metadata.ShareOptions{ReadOnly: true}))
		// Owner, per-user ShareReadOnly explicitly false: only the store-level
		// flag is in play, so a non-EROFS result would mean the discriminator
		// failed to consult ShareOptions.
		owner := f.authContext(1700, 1700)
		owner.ShareReadOnly = false
		got := asStoreErr(t, f.service.CheckParentCreateAccess(owner, f.rootHandle, false))
		require.Equal(t, metadata.ErrReadOnly, got.Code)
	})

	t.Run("per-user read-only on writable share is EACCES", func(t *testing.T) {
		f := newTestFixture(t)
		// Store NOT read-only; only the per-user ceiling denies. This must stay
		// EACCES so a squashed/unknown-uid read level (which sets ctx.ShareReadOnly
		// on a writable share) does not masquerade as a read-only filesystem —
		// TestNFSRootSquash depends on this.
		ro := f.authContext(1701, 1701)
		ro.ShareReadOnly = true
		got := asStoreErr(t, f.service.CheckParentCreateAccess(ro, f.rootHandle, false))
		require.Equal(t, metadata.ErrAccessDenied, got.Code)
	})
}

// TestCheckParentWriteAccess_ReadOnlyDiscriminator exercises the data-write
// permission path (checkPermission via CheckParentWriteAccess) and asserts the
// EROFS-vs-EACCES discriminator directly:
//   - store-level read-only share → ErrReadOnly (EROFS), preserving #1508;
//   - per-user read-only on a writable share → ErrAccessDenied (EACCES);
//   - an ordinary POSIX permission denial (no read-only ceiling) → ErrAccessDenied.
func TestCheckParentWriteAccess_ReadOnlyDiscriminator(t *testing.T) {
	asStoreErr := func(t *testing.T, err error) *metadata.StoreError {
		t.Helper()
		require.Error(t, err)
		var storeErr *metadata.StoreError
		require.True(t, errors.As(err, &storeErr), "err %v is not a *StoreError", err)
		return storeErr
	}

	t.Run("per-user read-only on writable share is EACCES", func(t *testing.T) {
		f := newTestFixture(t)
		ro := f.authContext(1100, 1100)
		ro.ShareReadOnly = true
		got := asStoreErr(t, f.service.CheckParentWriteAccess(ro, f.rootHandle))
		require.Equal(t, metadata.ErrAccessDenied, got.Code)
	})

	t.Run("store-level read-only share is EROFS", func(t *testing.T) {
		f := newTestFixture(t)
		_ = f.store.CreateShare(context.Background(), &metadata.Share{Name: f.shareName})
		require.NoError(t, f.store.UpdateShareOptions(context.Background(), f.shareName,
			&metadata.ShareOptions{ReadOnly: true}))
		owner := f.authContext(1101, 1101)
		owner.ShareReadOnly = false
		got := asStoreErr(t, f.service.CheckParentWriteAccess(owner, f.rootHandle))
		require.Equal(t, metadata.ErrReadOnly, got.Code)
	})

	t.Run("ordinary POSIX denial on writable share is EACCES", func(t *testing.T) {
		f := newTestFixture(t)
		// Directory owned by uid 2000, mode 0o755: "other" users get no write.
		dir, _, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "owned-dir",
			&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0o755, UID: 2000, GID: 2000})
		require.NoError(t, err)
		dirHandle, err := metadata.EncodeShareHandle(f.shareName, dir.ID)
		require.NoError(t, err)
		other := f.authContext(3000, 3000) // not owner, not in group
		got := asStoreErr(t, f.service.CheckParentWriteAccess(other, dirHandle))
		require.Equal(t, metadata.ErrAccessDenied, got.Code)
	})
}

// TestSetFileAttributes_PerUserReadOnlyDeniesOwnerMutation asserts the SETATTR
// ceiling: a read-only user may not mutate even a file they OWN — chmod, chown,
// and (most importantly) installing a permissive ACL must all be denied. Without
// the ceiling the owner-bypass in SetFileAttributes would let a read-only owner
// escalate access on their own file.
//
// The denial code here is ErrReadOnly (EROFS) even for the per-user ceiling.
// This is a KNOWN, DELIBERATE deferral: unlike the data WRITE/CREATE/DELETE path
// (which now distinguishes a per-user read-only level → EACCES from a genuinely
// read-only share → EROFS), SETATTR has not yet been aligned. The security
// property asserted here — that the mutation is DENIED — is unaffected.
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
		if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrReadOnly {
			t.Errorf("%s: err = %v, want StoreError{Code: ErrReadOnly}", name, err)
		}
	}

	// Sanity: the same owner on a read-write share CAN chmod.
	rw := f.authContext(uid, gid)
	if _, err := f.service.SetFileAttributes(rw, handle, &metadata.SetAttrs{Mode: &newMode}); err != nil {
		t.Fatalf("precondition: owner chmod on read-write share should succeed, got %v", err)
	}
}

// TestSetFileAttributes_StoreReadOnlyDeniesOwnerMutation asserts the SETATTR
// ceiling also fires for the STORE-level read-only flag (ShareOptions.ReadOnly),
// not just the per-user ctx.ShareReadOnly: an owner may not chmod their own file
// on a share configured read-only. The owner-bypass path skips
// checkWritePermission, so without consulting both ceilings the store-level flag
// would be silently ignored for SETATTR.
func TestSetFileAttributes_StoreReadOnlyDeniesOwnerMutation(t *testing.T) {
	f := newTestFixture(t)

	uid, gid := uint32(1600), uint32(1600)
	// Create the file BEFORE toggling the share read-only (creation is denied
	// on a read-only share).
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "store-ro.txt",
		&metadata.FileAttr{Type: metadata.FileTypeRegular, Mode: 0o644, UID: uid, GID: gid})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	_ = f.store.CreateShare(context.Background(), &metadata.Share{Name: f.shareName})
	require.NoError(t, f.store.UpdateShareOptions(context.Background(), f.shareName,
		&metadata.ShareOptions{ReadOnly: true}))

	// The OWNER, with per-user ShareReadOnly explicitly false — only the
	// store-level flag is in play.
	owner := f.authContext(uid, gid)
	owner.ShareReadOnly = false

	newMode := uint32(0o777)
	_, err = f.service.SetFileAttributes(owner, handle, &metadata.SetAttrs{Mode: &newMode})
	if err == nil {
		t.Fatal("SetFileAttributes returned nil for owner on store-level read-only share, want ErrReadOnly")
	}
	var storeErr *metadata.StoreError
	if !errors.As(err, &storeErr) || storeErr.Code != metadata.ErrReadOnly {
		t.Fatalf("SetFileAttributes err = %v, want StoreError{Code: ErrReadOnly}", err)
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
