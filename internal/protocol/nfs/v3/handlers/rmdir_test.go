package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRmdir_RFC1813 tests RMDIR handler behaviors per RFC 1813 Section 3.3.13.
//
// RMDIR removes an empty directory. The directory must be empty
// (no entries other than "." and "..").

// TestRmdir_EmptyDirectory tests removing an empty directory.
func TestRmdir_EmptyDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create an empty directory
	fx.CreateDirectory("emptydir")

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "emptydir",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "RMDIR should return NFS3OK")

	// Verify directory is gone
	assert.Nil(t, fx.GetHandle("emptydir"), "Directory should no longer exist")
}

// TestRmdir_NonEmptyDirectory tests that RMDIR fails on non-empty directory.
func TestRmdir_NonEmptyDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a directory with a file inside
	fx.CreateFile("notempty/file.txt", []byte("content"))

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "notempty",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotEmpty, resp.Status, "RMDIR should return NFS3ErrNotEmpty for non-empty directory")
}

// TestRmdir_NonExistentDirectory tests removing a directory that doesn't exist.
func TestRmdir_NonExistentDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "nonexistent",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNoEnt, resp.Status, "RMDIR should return NFS3ErrNoEnt for non-existent directory")
}

// TestRmdir_File tests that RMDIR fails on a file.
func TestRmdir_File(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file
	fx.CreateFile("file.txt", []byte("content"))

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "file.txt",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "RMDIR on file should return NFS3ErrNotDir")
}

// TestRmdir_NotADirectory tests RMDIR when parent is not a directory.
func TestRmdir_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file
	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	req := &handlers.RmdirRequest{
		DirHandle: fileHandle, // Use file handle as parent
		Name:      "child",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "RMDIR with file as parent should return NFS3ErrNotDir")
}

// TestRmdir_EmptyName tests RMDIR with empty name.
func TestRmdir_EmptyName(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty name should return NFS3ErrInval")
}

// TestRmdir_NameTooLong tests RMDIR with name longer than 255 bytes.
func TestRmdir_NameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      string(longName),
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "Name > 255 bytes should return NFS3ErrNameTooLong")
}

// TestRmdir_NameWithNullByte tests RMDIR rejects names with null bytes.
func TestRmdir_NameWithNullByte(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "dir\x00name",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Name with null byte should return NFS3ErrInval")
}

// TestRmdir_NameWithPathSeparator tests RMDIR rejects names with path separators.
func TestRmdir_NameWithPathSeparator(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "path/to/dir",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Name with path separator should return NFS3ErrInval")
}

// TestRmdir_DotEntry tests that RMDIR fails for "." entry.
func TestRmdir_DotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      ".",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "RMDIR '.' should return NFS3ErrInval")
}

// TestRmdir_DotDotEntry tests that RMDIR fails for ".." entry.
func TestRmdir_DotDotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "..",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "RMDIR '..' should return NFS3ErrInval")
}

// TestRmdir_EmptyHandle tests RMDIR with empty handle.
func TestRmdir_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: []byte{},
		Name:      "dir",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestRmdir_HandleTooShort tests RMDIR with handle shorter than minimum.
func TestRmdir_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Name:      "dir",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestRmdir_HandleTooLong tests RMDIR with handle longer than maximum.
func TestRmdir_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RmdirRequest{
		DirHandle: make([]byte, 65), // 65 bytes, max is 64
		Name:      "dir",
	}
	resp, err := fx.Handler.Rmdir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestRmdir_ContextCancellation tests RMDIR respects context cancellation.
func TestRmdir_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateDirectory("canceldir")

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "canceldir",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithCancellation(), req)

	// RMDIR may return response with status or error
	if err != nil {
		return // Context cancellation detected via error
	}
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestRmdir_ReturnsWCC tests that RMDIR returns WCC data.
func TestRmdir_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateDirectory("wccdir")

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "wccdir",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.DirWccBefore, "Should return DirWccBefore (pre-op attrs)")
	assert.NotNil(t, resp.DirWccAfter, "Should return DirWccAfter (post-op attrs)")
}

// TestRmdir_InNestedDirectory tests RMDIR in a nested directory.
func TestRmdir_InNestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create nested directory structure
	fx.CreateDirectory("a/b/c/nested")
	parentHandle := fx.MustGetHandle("a/b/c")

	req := &handlers.RmdirRequest{
		DirHandle: parentHandle,
		Name:      "nested",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify directory is gone
	assert.Nil(t, fx.GetHandle("a/b/c/nested"))
}

// TestRmdir_DirectoryWithSubdirectory tests RMDIR fails with subdirectory.
func TestRmdir_DirectoryWithSubdirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a directory with a subdirectory
	fx.CreateDirectory("parent/child")

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "parent",
	}
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotEmpty, resp.Status, "RMDIR should return NFS3ErrNotEmpty for directory with subdirectory")
}
