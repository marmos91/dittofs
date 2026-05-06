package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/require"
)

// TestCheckPermissions_ACLDenyOverridesShareWritable asserts that when a file
// carries an explicit ACL with a SID-form deny-write ACE for the requester,
// the share-level ShareWritable bypass does NOT grant write. Without this
// gate, smbtorture acls.DENY1 + delete-on-close-perms.* fail because the
// share short-circuit always wins over file-level intent.
func TestCheckPermissions_ACLDenyOverridesShareWritable(t *testing.T) {
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
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "denied.txt",
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

	// Build an AuthContext with ShareWritable = true (smbtorture's typical
	// session has a writable share permission). Identity carries the SID so
	// SID-form ACE matching can fire.
	authCtx := f.authContext(requesterUID, 1001)
	authCtx.Identity.SID = strPtr(requesterSID)
	authCtx.ShareWritable = true
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	if got&metadata.PermissionWrite != 0 || got&metadata.PermissionDelete != 0 {
		t.Errorf("expected write+delete denied via SID-form deny ACE on writable share, got 0x%x", got)
	}
}

// TestCheckPermissions_AllowOnlyACLKeepsShareWritableBypass asserts that when
// a file carries an explicit ACL containing only ALLOW ACEs (no DENY), the
// share-level ShareWritable bypass continues to grant write. This covers
// smbtorture stream-inherit-perms (where SET_INFO Security appends one ALLOW
// ACE to the synthesized DACL) and create.multi (allow-only SD on share root).
func TestCheckPermissions_AllowOnlyACLKeepsShareWritableBypass(t *testing.T) {
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
	created, err := f.service.CreateFile(f.rootContext(), f.rootHandle, "allow_only.txt",
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
	authCtx.ShareWritable = true
	authCtx.ShareReadOnly = false

	got, err := f.service.CheckPermissions(authCtx, handle, metadata.PermissionWrite|metadata.PermissionDelete)
	require.NoError(t, err)
	want := metadata.PermissionWrite | metadata.PermissionDelete
	if got&want != want {
		t.Errorf("expected share-level write+delete bypass on allow-only ACL, got 0x%x", got)
	}
}

func strPtr(s string) *string { return &s }
