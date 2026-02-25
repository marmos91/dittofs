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

// TestMkdir_RFC1813 tests MKDIR handler behaviors per RFC 1813 Section 3.3.9.
//
// MKDIR creates a new directory. It requires:
// - A valid parent directory handle
// - A valid directory name
// - Appropriate permissions

// TestMkdir_SimpleCreate tests creating a new directory.
func TestMkdir_SimpleCreate(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "MKDIR should return NFS3OK")
	assert.NotNil(t, resp.Handle, "Should return directory handle")
	assert.NotNil(t, resp.Attr, "Should return directory attributes")
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Should be a directory")
}

// TestMkdir_WithMode tests that MKDIR applies the specified mode.
func TestMkdir_WithMode(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0700)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "modedir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(0700), resp.Attr.Mode&0777, "Mode should match requested")
}

// TestMkdir_ExistingDirectory tests that MKDIR fails if directory exists.
func TestMkdir_ExistingDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create directory first
	fx.CreateDirectory("existing")

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "existing",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp.Status, "MKDIR should fail with NFS3ErrExist if directory exists")
}

// TestMkdir_ExistingFile tests that MKDIR fails if a file with that name exists.
func TestMkdir_ExistingFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create file first
	fx.CreateFile("existingfile", []byte("content"))

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "existingfile",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp.Status, "MKDIR should fail with NFS3ErrExist if file exists")
}

// TestMkdir_NotADirectory tests that MKDIR fails if parent is not a directory.
func TestMkdir_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file
	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fileHandle, // Use file handle as parent
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "MKDIR in file should return NFS3ErrNotDir")
}

// TestMkdir_EmptyName tests that MKDIR fails with empty name.
func TestMkdir_EmptyName(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty name should return NFS3ErrInval")
}

// TestMkdir_NameTooLong tests that MKDIR fails with name longer than 255 bytes.
func TestMkdir_NameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      string(longName),
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "Name > 255 bytes should return NFS3ErrNameTooLong")
}

// TestMkdir_NameWithNullByte tests that MKDIR rejects names with null bytes.
func TestMkdir_NameWithNullByte(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "dir\x00name",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Name with null byte should return NFS3ErrInval")
}

// TestMkdir_NameWithPathSeparator tests that MKDIR rejects names with path separators.
func TestMkdir_NameWithPathSeparator(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "path/to/dir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Name with path separator should return NFS3ErrInval")
}

// TestMkdir_EmptyHandle tests that MKDIR returns error for empty handle.
func TestMkdir_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: []byte{},
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrBadHandle for empty handle
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return error")
}

// TestMkdir_HandleTooShort tests that MKDIR returns error for short handles.
func TestMkdir_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrBadHandle for invalid handles
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return error")
}

// TestMkdir_HandleTooLong tests that MKDIR returns error for long handles.
func TestMkdir_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: make([]byte, 65), // 65 bytes, max is 64
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrBadHandle for handles exceeding max length
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return error")
}

// TestMkdir_ContextCancellation tests that MKDIR respects context cancellation.
func TestMkdir_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "newdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithCancellation(), req)

	require.Error(t, err, "Should return error for cancelled context")
	if resp != nil {
		assert.EqualValues(t, types.NFS3ErrIO, resp.Status)
	}
}

// TestMkdir_ReturnsWCC tests that MKDIR returns WCC data.
func TestMkdir_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "wccdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.WccBefore, "Should return WccBefore (pre-op attrs)")
	assert.NotNil(t, resp.WccAfter, "Should return WccAfter (post-op attrs)")
}

// TestMkdir_InNestedDirectory tests MKDIR in a nested directory.
func TestMkdir_InNestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create nested parent directory
	parentHandle := fx.CreateDirectory("a/b/c")

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: parentHandle,
		Name:      "deepdir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify directory exists
	assert.NotNil(t, fx.GetHandle("a/b/c/deepdir"))
}

// TestMkdir_DirectoryCanBeUsed tests that created directory can be used for operations.
func TestMkdir_DirectoryCanBeUsed(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0755)
	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "usabledir",
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Use the returned handle to lookup self
	lookupResp, err := fx.Handler.Lookup(fx.Context(), &handlers.LookupRequest{
		DirHandle: resp.Handle,
		Filename:  ".",
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, lookupResp.Status)
	assert.EqualValues(t, types.NF3DIR, lookupResp.Attr.Type)
}

// TestMkdir_NilAttr tests MKDIR with nil attributes.
func TestMkdir_NilAttr(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.MkdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "noattrdir",
		Attr:      nil, // No attributes specified
	}
	resp, err := fx.Handler.Mkdir(fx.ContextWithUID(0, 0), req)

	// Should still succeed with default attributes
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
}
