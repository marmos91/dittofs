package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccess_RootFile tests ACCESS check on a regular file.
func TestAccess_RootFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("accessfile.txt", []byte("content"))

	// Request all access bits
	req := &handlers.AccessRequest{
		Handle: fileHandle,
		Access: types.AccessRead | types.AccessModify | types.AccessExecute,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "ACCESS should succeed")
	// At minimum, read access should be granted for a file owned by the test user
	assert.True(t, resp.Access&types.AccessRead != 0, "Read access should be granted")
}

// TestAccess_Directory tests ACCESS check on a directory.
func TestAccess_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("accessdir")

	req := &handlers.AccessRequest{
		Handle: dirHandle,
		Access: types.AccessRead | types.AccessLookup | types.AccessModify,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "ACCESS should succeed for directory")
	assert.NotNil(t, resp.Attr, "Should return attributes")
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Should be a directory")
}

// TestAccess_NarrowsToRequest pins that the reply carries only the rights the
// client asked about. CheckPermissions already returns a subset of the generic
// permissions it was handed, so the widening comes entirely from the back
// translation, which is not bijective: one PermissionWrite becomes both
// AccessModify and AccessExtend, and one PermissionTraverse becomes both
// AccessLookup and AccessExecute. The Linux client caches this mask and answers
// later, different access checks from it without returning to the server, so a
// bit it never asked about becomes a grant nothing here evaluated.
func TestAccess_NarrowsToRequest(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("narrow.txt", []byte("content"))
	dirHandle := fx.CreateDirectory("narrowdir")

	tests := []struct {
		name      string
		handle    metadata.FileHandle
		requested uint32
	}{
		{"file/modify does not imply extend", fileHandle, types.AccessModify},
		{"file/extend does not imply modify", fileHandle, types.AccessExtend},
		{"dir/lookup does not imply execute", dirHandle, types.AccessLookup},
		{"dir/execute does not imply lookup", dirHandle, types.AccessExecute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &handlers.AccessRequest{Handle: tt.handle, Access: tt.requested}
			resp, err := fx.Handler.Access(fx.Context(), req)

			require.NoError(t, err)
			require.EqualValues(t, types.NFS3OK, resp.Status)
			assert.EqualValues(t, tt.requested, resp.Access,
				"reply must be the requested bits, not every right the object could answer for")
		})
	}
}

// TestAccess_NoDeleteOnRegularFile pins RFC 1813 Section 3.3.4: ACCESS3_DELETE
// is the right to remove a directory entry, so a non-directory object reports it
// as 0 even when the client asks and the object is writable by the caller.
func TestAccess_NoDeleteOnRegularFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("deletable.txt", []byte("content"))

	req := &handlers.AccessRequest{
		Handle: fileHandle,
		Access: types.AccessRead | types.AccessModify | types.AccessDelete,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)
	assert.Zero(t, resp.Access&types.AccessDelete,
		"AccessDelete has no meaning for a regular file")
	assert.NotZero(t, resp.Access&types.AccessModify,
		"the writable file should still report AccessModify")
}

// TestAccess_DeleteOnDirectory is the other half of the rule: a directory the
// caller may write does report AccessDelete, so dropping it for files did not
// drop it everywhere.
func TestAccess_DeleteOnDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("deletabledir")

	req := &handlers.AccessRequest{
		Handle: dirHandle,
		Access: types.AccessDelete,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, types.AccessDelete, resp.Access,
		"a writable directory grants AccessDelete")
}
