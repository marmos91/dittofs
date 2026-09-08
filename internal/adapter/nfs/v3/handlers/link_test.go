package handlers_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLink_Success tests creating a hard link to an existing file.
func TestLink_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create source file
	fileHandle := fx.CreateFile("original.txt", []byte("hello"))

	// Get initial attributes to check nlink
	getResp, err := fx.Handler.GetAttr(fx.Context(), &handlers.GetAttrRequest{Handle: fileHandle})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, getResp.Status)
	initialNlink := getResp.Attr.Nlink

	// Create hard link in root directory
	req := &handlers.LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  fx.RootHandle,
		Name:       "hardlink.txt",
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "LINK should succeed")

	// Verify link count increased
	getResp2, err := fx.Handler.GetAttr(fx.Context(), &handlers.GetAttrRequest{Handle: fileHandle})
	require.NoError(t, err)
	assert.EqualValues(t, initialNlink+1, getResp2.Attr.Nlink, "Link count should increase by 1")
}

// TestLink_DirectoryFails tests that linking a directory returns an error.
func TestLink_DirectoryFails(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("adir")

	req := &handlers.LinkRequest{
		FileHandle: dirHandle,
		DirHandle:  fx.RootHandle,
		Name:       "dirlink",
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	// Hard links to directories are not allowed
	assert.NotEqualValues(t, types.NFS3OK, resp.Status,
		"Linking a directory should fail")
}

// TestLink_InvalidHandle tests LINK with an invalid file handle.
func TestLink_InvalidHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	invalidHandle := make([]byte, 16)
	for i := range invalidHandle {
		invalidHandle[i] = byte(i)
	}

	req := &handlers.LinkRequest{
		FileHandle: invalidHandle,
		DirHandle:  fx.RootHandle,
		Name:       "link.txt",
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	assert.NotEqualValues(t, types.NFS3OK, resp.Status,
		"Invalid handle should not return NFS3OK")
}

// TestLink_NameExistsReturnsWCC drives the "name already exists" branch and
// verifies it returns NFS3ErrExist with non-nil WCC fields without a nil-pointer
// dereference (regression guard for the discarded-error GetFile paths).
func TestLink_NameExistsReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create source file
	fileHandle := fx.CreateFile("src.txt", []byte("hello"))
	// Create an existing entry at the target name
	fx.CreateFile("existing-link.txt", []byte("other"))

	req := &handlers.LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  fx.RootHandle,
		Name:       "existing-link.txt", // already exists -> drives name-exists path
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp.Status)
	assert.NotNil(t, resp.DirWccBefore, "WCC before must be set even on error")
	assert.NotNil(t, resp.DirWccAfter, "WCC after must be set even on error")
}

// TestLink_CrossShareRejected pins that LINK refuses to join a file in one
// share to a directory in another, and reports it as NFS3ErrXDev so clients
// see EXDEV and can fall back to copying.
//
// Both share names must come from the handles themselves: ctx.Share is derived
// from the first handle on the wire, which for LINK3args is the file handle,
// so a check against it compares the file handle to itself and never fires.
func TestLink_CrossShareRejected(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("original.txt", []byte("hello"))

	foreignDir, err := metadata.EncodeShareHandle("/other-share", uuid.New())
	require.NoError(t, err)

	req := &handlers.LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  foreignDir,
		Name:       "stolen.txt",
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrXDev, resp.Status,
		"LINK across shares should return NFS3ErrXDev")
}

// TestLink_UndecodableDirHandleRejected pins that a directory handle that is
// not a share handle is reported as a bad handle rather than reaching the
// store.
func TestLink_UndecodableDirHandleRejected(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("original.txt", []byte("hello"))

	req := &handlers.LinkRequest{
		FileHandle: fileHandle,
		DirHandle:  []byte("not-a-share-handle"),
		Name:       "link.txt",
	}
	resp, err := fx.Handler.Link(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status,
		"LINK with an undecodable directory handle should return NFS3ErrBadHandle")
}
