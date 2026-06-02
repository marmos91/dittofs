package metadata_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/acl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLookup_BypassTraverseChecking_GrantsAccess regresses GitHub issue #562.
//
// Reproduces the parent-DACL restriction from smbtorture's
// `smb2.acls.DYNAMIC` (source4/torture/smb2/acls.c::test_inheritance_dynamic):
// a parent directory whose DACL grants only WRITE_DATA|DELETE|READ_ATTRIBUTE
// to the owner (no FILE_EXECUTE / FILE_TRAVERSE) must still permit name
// resolution of a child when the caller holds Windows
// SeChangeNotifyPrivilege ("Bypass traverse checking"). DittoFS encodes that
// privilege as AuthContext.BypassTraverseChecking, set by every SMB session.
//
// Before the fix, the Lookup on the restricted parent failed with
// ErrAccessDenied (mapped to STATUS_ACCESS_DENIED) or the upstream walkPath
// remapped it to STATUS_OBJECT_NAME_NOT_FOUND, neither of which matches the
// MS-FSA §2.1.5.1.1 behavior.
func TestLookup_BypassTraverseChecking_GrantsAccess(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	// Root creates a subdirectory and a child file inside it. We then
	// restrict the parent's DACL so callers without bypass cannot traverse.
	rootCtx := fx.rootContext()
	rootCtx.BypassTraverseChecking = true
	_, _, err := fx.service.CreateDirectory(rootCtx, fx.rootHandle, "restricted", &metadata.FileAttr{
		Mode: 0700,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)

	subHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "restricted")
	require.NoError(t, err)

	_, _, err = fx.service.CreateFile(rootCtx, subHandle, "child.txt", &metadata.FileAttr{
		Mode: 0644,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)

	// Install an owner-only DACL granting WRITE_DATA|READ_ATTRIBUTES but
	// NOT FILE_EXECUTE / FILE_TRAVERSE — matches the
	// security_descriptor_dacl_create(...) call at acls.c:1916.
	restrictedACL := &acl.ACL{
		ACEs: []acl.ACE{
			{
				Type:       acl.ACE4_ACCESS_ALLOWED_ACE_TYPE,
				AccessMask: acl.ACE4_WRITE_DATA | acl.ACE4_READ_ATTRIBUTES,
				Who:        acl.SpecialOwner,
			},
		},
	}
	_, err = fx.service.SetFileAttributes(rootCtx, subHandle, &metadata.SetAttrs{
		ACL: restrictedACL,
	})
	require.NoError(t, err)

	// Sanity check: without BypassTraverseChecking, a non-owner caller's
	// Lookup of "child.txt" through the restricted parent fails (strict
	// POSIX/NFS semantics — this is what NFS still gets after the fix).
	strictCtx := fx.authContext(2000, 2000)
	strictCtx.BypassTraverseChecking = false
	_, err = fx.service.Lookup(strictCtx, subHandle, "child.txt")
	require.Error(t, err, "without bypass, lookup must fail when parent denies traverse")

	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, metadata.ErrAccessDenied, storeErr.Code,
		"traverse denial must map to ErrAccessDenied, not ErrNoEntity (issue #562)")

	// With BypassTraverseChecking (the SMB default), the same Lookup succeeds.
	bypassCtx := fx.authContext(2000, 2000)
	bypassCtx.BypassTraverseChecking = true
	got, err := fx.service.Lookup(bypassCtx, subHandle, "child.txt")
	require.NoError(t, err,
		"with BypassTraverseChecking, lookup must succeed even when parent omits FILE_EXECUTE")
	assert.Equal(t, "/restricted/child.txt", got.Path)
}

// TestLookup_BypassTraverseChecking_NFSStrict pins the NFS-side contract:
// callers that leave BypassTraverseChecking false continue to enforce
// per-directory FILE_EXECUTE on Lookup. Guards against accidentally
// loosening POSIX traverse for the NFS adapter while flipping the SMB bit.
func TestLookup_BypassTraverseChecking_NFSStrict(t *testing.T) {
	t.Parallel()
	fx := newTestFixture(t)
	ctx := context.Background()

	rootCtx := fx.rootContext()
	rootCtx.BypassTraverseChecking = true
	_, _, err := fx.service.CreateDirectory(rootCtx, fx.rootHandle, "nfs-dir", &metadata.FileAttr{
		Mode: 0700,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)

	subHandle, err := fx.store.GetChild(ctx, fx.rootHandle, "nfs-dir")
	require.NoError(t, err)

	_, _, err = fx.service.CreateFile(rootCtx, subHandle, "file", &metadata.FileAttr{
		Mode: 0644,
		UID:  1000,
		GID:  1000,
	})
	require.NoError(t, err)

	// Non-owner caller, no group match, mode 0700 → no other-execute bit.
	nfsCtx := fx.authContext(2000, 2000) // BypassTraverseChecking left false
	_, err = fx.service.Lookup(nfsCtx, subHandle, "file")
	require.Error(t, err, "NFS-strict caller must still get traverse-denied")

	var storeErr *metadata.StoreError
	require.ErrorAs(t, err, &storeErr)
	assert.Equal(t, metadata.ErrAccessDenied, storeErr.Code)
}
