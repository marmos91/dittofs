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
	dir, err := f.service.CreateDirectory(f.rootContext(), f.rootHandle, "denied-dir",
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
	authCtx.ShareWritable = true
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
// is unchanged (parent has no explicit ACL; ShareWritable bypass still
// applies because the P1-1 gate only fires when an ACL is present).
func TestCheckParentWriteAccess_NoACLAllowsAdd(t *testing.T) {
	f := newTestFixture(t)
	authCtx := f.authContext(1001, 1001)
	authCtx.ShareWritable = true
	if err := f.service.CheckParentWriteAccess(authCtx, f.rootHandle); err != nil {
		t.Fatalf("CheckParentWriteAccess on POSIX-only writable parent err = %v, want nil", err)
	}
}
