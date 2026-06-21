package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckPermissions_ACLDenyOverridesHandleWrite asserts that when a file
// carries an explicit ACL with a SID-form deny-write ACE for the requester, the
// handle-based write authorization (WriteAuthorizedByHandle) does NOT grant
// write — the metadata layer keeps the explicit-DENY guard for defense-in-depth.
//
// In production this case cannot arise: the SMB open-time DACL gate strips
// FILE_WRITE_DATA when a DENY-write ACE is present, so a write-authorized handle
// is never minted and the WRITE handler never sets the flag. We set the flag
// anyway here to prove the metadata-layer DENY guard holds even if the flag were
// mis-set, preserving smbtorture acls.DENY1 + delete-on-close-perms.* behavior.
func TestCheckPermissions_ACLDenyOverridesHandleWrite(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Create a regular file under the root with an explicit ACL that denies
	// WriteData|AppendData for the requester's SID, then allows everything to
	// EVERYONE@. POSIX bits would grant writes; the deny ACE must win.
	deniedACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_DENIED_ACE_TYPE,
				Who:        "sid:" + requesterSID,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_APPEND_DATA | acl.ACE4_DELETE,
			},
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "denied.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o777,
			UID:  requesterUID,
			GID:  1001,
			ACL:  deniedACL,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Build an AuthContext with WriteAuthorizedByHandle = true (as an SMB WRITE
	// handler would set from a write-granted handle). Identity carries the SID so
	// SID-form ACE matching can fire.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)
	authCtx.WriteAuthorizedByHandle = true
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	if got&metadata.PermissionWrite != 0 || got&metadata.PermissionDelete != 0 {
		t.Errorf("expected write+delete denied via SID-form deny ACE despite WriteAuthorizedByHandle, got 0x%x", got)
	}
}

// TestCheckPermissions_HandleWriteGrantsOnReadonlyFile asserts the headline
// #1240 scenario: a handle granted write at open (WriteAuthorizedByHandle=true)
// writes through to a file the requester does NOT own and whose POSIX mode would
// deny write — and, separately, a DOS-READONLY file — yet the write is granted
// because the open already enforced the DACL ceiling.
//
// This is the smbtorture smb2.durable-open.read-only motivation: a
// FILE_ATTRIBUTE_READONLY file opened with full access (NULL DACL) must remain
// writable through the handle.
func TestCheckPermissions_HandleWriteGrantsOnReadonlyFile(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)

	// DOS-READONLY bit (mode 0x100000) plus a mode that grants nothing to the
	// requester (owned by a different UID, 0o400 owner-read-only). No ACL — so
	// the open-time gate was permissive and the handle carries write.
	const dosReadonly = uint32(0x100000)
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "readonly.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o400 | dosReadonly,
			UID:  2002, // not the requester
			GID:  2002,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Without the handle flag, the requester (non-owner, no write bit) is denied.
	denyCtx := f.authContext(requesterUID, 1001)
	denyCtx.ShareReadOnly = false
	gotDeny, err := f.service.CheckPermissions(denyCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if gotDeny&metadata.PermissionWrite != 0 {
		t.Fatalf("precondition: expected write denied without handle authorization, got 0x%x", gotDeny)
	}

	// With WriteAuthorizedByHandle=true, the write is granted despite POSIX mode
	// and the DOS-READONLY attribute — the open is the authorization boundary.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.WriteAuthorizedByHandle = true
	authCtx.ShareReadOnly = false
	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if got&metadata.PermissionWrite == 0 {
		t.Errorf("expected write granted via WriteAuthorizedByHandle on readonly/non-owner file, got 0x%x", got)
	}
}

// TestCheckPermissions_HandleWriteSuppressedByReadOnlyShare asserts the
// ShareReadOnly ceiling beats the handle authorization: when the share is
// read-only for the user, the handle-based write bypass is suppressed and the
// write falls through to the normal POSIX/ACL check, which denies a non-owner
// who has no write bit. (ShareReadOnly gates the bypass; the share's ReadOnly
// option is enforced inside calculatePermissions on the fallthrough.)
func TestCheckPermissions_HandleWriteSuppressedByReadOnlyShare(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	// Non-owner, owner-read-only: without the handle bypass the requester has no
	// write bit, so suppression of the bypass must surface as a denial.
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "ro_share.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o400,
			UID:  2002, // not the requester
			GID:  2002,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Sanity: with the handle bypass and a writable share, write is granted.
	rwCtx := f.authContext(requesterUID, 1001)
	rwCtx.WriteAuthorizedByHandle = true
	rwCtx.ShareReadOnly = false
	gotRW, err := f.service.CheckPermissions(rwCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if gotRW&metadata.PermissionWrite == 0 {
		t.Fatalf("precondition: expected write granted via handle on writable share, got 0x%x", gotRW)
	}

	// ShareReadOnly suppresses the bypass; the POSIX fallthrough then denies.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.WriteAuthorizedByHandle = true
	authCtx.ShareReadOnly = true

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite)
	require.NoError(t, err)
	if got&metadata.PermissionWrite != 0 {
		t.Errorf("expected write denied on read-only share despite WriteAuthorizedByHandle, got 0x%x", got)
	}
}

// TestCheckPermissions_AllowOnlyACLKeepsHandleWriteBypass asserts that when a
// file carries an explicit ACL containing only ALLOW ACEs (no DENY), the
// handle-based write authorization continues to grant write. This covers
// smbtorture stream-inherit-perms (where SET_INFO Security appends one ALLOW
// ACE to the synthesized DACL) and create.multi (allow-only SD on share root).
func TestCheckPermissions_AllowOnlyACLKeepsHandleWriteBypass(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Allow-only ACL: a single ALLOW ACE for EVERYONE@ with broad mask.
	// This mirrors what smbtorture stream-inherit-perms ends up with after
	// SET_INFO Security adds an explicit ALLOW ACE: a DACL flagged as
	// SMB-explicit but containing zero DENY ACEs.
	allowOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: 0xFFFFFFFF,
			},
		},
	}
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "allow_only.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o755,
			UID:  requesterUID,
			GID:  1001,
			ACL:  allowOnlyACL,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)
	authCtx.WriteAuthorizedByHandle = true
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	want := metadata.PermissionWrite | metadata.PermissionDelete
	if got&want != want {
		t.Errorf("expected handle-based write+delete bypass on allow-only ACL, got 0x%x", got)
	}
}

func strPtr(s string) *string { return &s }
