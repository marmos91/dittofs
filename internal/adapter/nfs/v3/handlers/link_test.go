package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
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
