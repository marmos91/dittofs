package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckPermissions_ACLDenyBeatsPosixWrite asserts that when a file carries
// an explicit ACL with a SID-form deny-write ACE for the requester, write is
// denied even though the POSIX mode (0o777) would grant it and the share is
// read-write. The file's ACL is authoritative; the share grant is a ceiling,
// not a floor. Covers smbtorture acls.DENY1 + delete-on-close-perms.*.
func TestCheckPermissions_ACLDenyBeatsPosixWrite(t *testing.T) {
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

	// Read-write share (ShareReadOnly=false, the default). Identity carries the
	// SID so SID-form ACE matching can fire.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	if got&metadata.PermissionWrite != 0 || got&metadata.PermissionDelete != 0 {
		t.Errorf("expected write+delete denied via SID-form deny ACE on writable share, got 0x%x", got)
	}
}

// TestCheckPermissions_AllowOnlyACLGrantsWriteViaACL asserts that an allow-only
// DACL that itself grants write yields write, sourced from the ACL. This is the
// smbtorture stream-inherit-perms case (SET_INFO Security appends a broad ALLOW
// ACE) and create.multi (allow-only SD on the share root): the ACL grants the
// write directly.
func TestCheckPermissions_AllowOnlyACLGrantsWriteViaACL(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Allow-only ACL: a single ALLOW ACE for EVERYONE@ with broad mask, mirroring
	// what stream-inherit-perms ends up with — a DACL flagged SMB-explicit with
	// zero DENY ACEs that grants WRITE_DATA|APPEND_DATA among the broad mask.
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
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	want := metadata.PermissionWrite | metadata.PermissionDelete
	if got&want != want {
		t.Errorf("expected write+delete granted by the allow-only ACL, got 0x%x", got)
	}
}

// TestCheckPermissions_ShareDoesNotGrantBeyondACL guards the share-permission
// ceiling: an allow-only DACL that grants only READ, on a mode 0o000 file, must
// not yield write on a read-write share. The share permission is a ceiling, never
// a floor — a read-write share grants nothing the file's ACL/POSIX denies.
func TestCheckPermissions_ShareDoesNotGrantBeyondACL(t *testing.T) {
	f := newTestFixture(t)

	requesterUID := uint32(1001)
	requesterSID := "S-1-5-21-1-2-3-2001"

	// Allow-only DACL granting READ only; POSIX mode denies everyone.
	readOnlyACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				Who:        acl.SpecialEveryone,
				AccessMask: acl.ACE4_READ_DATA,
			},
		},
	}
	created, _, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "share_ceiling.txt",
		&metadata.FileAttr{
			Type: metadata.FileTypeRegular,
			Mode: 0o000,
			UID:  2000,
			GID:  2000,
			ACL:  readOnlyACL,
		})
	require.NoError(t, err)
	handle, err := metadata.EncodeShareHandle(f.shareName, created.ID)
	require.NoError(t, err)

	// Read-write share (ShareReadOnly=false). Requester is neither owner nor a
	// grantee of write in the DACL.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionRead|metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	if got&metadata.PermissionRead == 0 {
		t.Errorf("expected read granted by the allow-only ACL, got 0x%x", got)
	}
	if got&(metadata.PermissionWrite|metadata.PermissionDelete) != 0 {
		t.Errorf("expected write+delete DENIED (share is a ceiling, not a floor), got 0x%x", got)
	}
}

func strPtr(s string) *string { return &s }
