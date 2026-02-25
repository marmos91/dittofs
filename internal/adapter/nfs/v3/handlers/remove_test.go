package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemove_RFC1813 tests REMOVE handler behaviors per RFC 1813 Section 3.3.12.
//
// REMOVE removes a file from a directory. It must NOT be used to remove
// directories (use RMDIR for that).

// TestRemove_ExistingFile tests removing an existing file.
func TestRemove_ExistingFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file to remove
	fx.CreateFile("removeme.txt", []byte("content"))

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "removeme.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "REMOVE should return NFS3OK")

	// Verify file is gone
	assert.Nil(t, fx.GetHandle("removeme.txt"), "File should no longer exist")
}

// TestRemove_NonExistentFile tests removing a file that doesn't exist.
func TestRemove_NonExistentFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "nonexistent.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNoEnt, resp.Status, "REMOVE should return NFS3ErrNoEnt for non-existent file")
}

// TestRemove_Directory tests that REMOVE fails on a directory.
func TestRemove_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a directory
	fx.CreateDirectory("testdir")

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "testdir",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrIsDir, resp.Status, "REMOVE on directory should return NFS3ErrIsDir")
}

// TestRemove_NotADirectory tests REMOVE when parent is not a directory.
func TestRemove_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file
	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	req := &handlers.RemoveRequest{
		DirHandle: fileHandle, // Use file handle as parent
		Filename:  "child.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "REMOVE with file as parent should return NFS3ErrNotDir")
}

// TestRemove_EmptyFilename tests REMOVE with empty filename.
func TestRemove_EmptyFilename(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty filename should return NFS3ErrInval")
}

// TestRemove_FilenameTooLong tests REMOVE with filename longer than 255 bytes.
func TestRemove_FilenameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  string(longName),
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "Filename > 255 bytes should return NFS3ErrNameTooLong")
}

// TestRemove_FilenameWithNullByte tests REMOVE rejects filenames with null bytes.
func TestRemove_FilenameWithNullByte(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file\x00name.txt",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Filename with null byte should return NFS3ErrInval")
}

// TestRemove_FilenameWithPathSeparator tests REMOVE rejects filenames with path separators.
func TestRemove_FilenameWithPathSeparator(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "path/to/file",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Filename with path separator should return NFS3ErrInval")
}

// TestRemove_EmptyHandle tests REMOVE with empty handle.
func TestRemove_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: []byte{},
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestRemove_HandleTooShort tests REMOVE with handle shorter than minimum.
func TestRemove_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestRemove_HandleTooLong tests REMOVE with handle longer than maximum.
func TestRemove_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: make([]byte, 65), // 65 bytes, max is 64
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Remove(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestRemove_ContextCancellation tests REMOVE respects context cancellation.
func TestRemove_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("cancel.txt", []byte("content"))

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "cancel.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithCancellation(), req)

	require.Error(t, err, "Should return error for cancelled context")
	if resp != nil {
		assert.EqualValues(t, types.NFS3ErrIO, resp.Status)
	}
}

// TestRemove_ReturnsWCC tests that REMOVE returns WCC data.
func TestRemove_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("wcc.txt", []byte("content"))

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "wcc.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.DirWccBefore, "Should return DirWccBefore (pre-op attrs)")
	assert.NotNil(t, resp.DirWccAfter, "Should return DirWccAfter (post-op attrs)")
}

// TestRemove_InNestedDirectory tests REMOVE in a nested directory.
func TestRemove_InNestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create nested file
	fx.CreateFile("a/b/c/nested.txt", []byte("content"))
	parentHandle := fx.MustGetHandle("a/b/c")

	req := &handlers.RemoveRequest{
		DirHandle: parentHandle,
		Filename:  "nested.txt",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify file is gone
	assert.Nil(t, fx.GetHandle("a/b/c/nested.txt"))
}

// TestRemove_Symlink tests removing a symlink.
func TestRemove_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a symlink
	fx.CreateSymlink("link", "/target")

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "link",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "REMOVE should succeed for symlink")

	// Verify symlink is gone
	assert.Nil(t, fx.GetHandle("link"))
}

// TestRemove_DotEntry tests that REMOVE fails for "." entry.
func TestRemove_DotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  ".",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrInval for "." and ".."
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "REMOVE '.' should return error")
}

// TestRemove_DotDotEntry tests that REMOVE fails for ".." entry.
func TestRemove_DotDotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RemoveRequest{
		DirHandle: fx.RootHandle,
		Filename:  "..",
	}
	resp, err := fx.Handler.Remove(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrInval for "." and ".."
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "REMOVE '..' should return error")
}
