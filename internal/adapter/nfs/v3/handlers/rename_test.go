package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRename_RFC1813 tests RENAME handler behaviors per RFC 1813 Section 3.3.14.
//
// RENAME renames a file or directory. It supports:
// - Simple rename within same directory
// - Move to different directory
// - Atomic replacement of destination if it exists

// TestRename_SimpleRename tests renaming a file within the same directory.
func TestRename_SimpleRename(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("oldname.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "oldname.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "RENAME should return NFS3OK")

	// Verify old name is gone
	assert.Nil(t, fx.GetHandle("oldname.txt"))
	// Verify new name exists
	assert.NotNil(t, fx.GetHandle("newname.txt"))
}

// TestRename_MoveToAnotherDirectory tests moving a file to a different directory.
func TestRename_MoveToAnotherDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source/file.txt", []byte("content"))
	fx.CreateDirectory("dest")
	destHandle := fx.MustGetHandle("dest")
	sourceHandle := fx.MustGetHandle("source")

	req := &handlers.RenameRequest{
		FromDirHandle: sourceHandle,
		FromName:      "file.txt",
		ToDirHandle:   destHandle,
		ToName:        "moved.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify file moved
	assert.Nil(t, fx.GetHandle("source/file.txt"))
	assert.NotNil(t, fx.GetHandle("dest/moved.txt"))
}

// TestRename_ReplaceExisting tests that RENAME atomically replaces existing file.
func TestRename_ReplaceExisting(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("new content"))
	fx.CreateFile("target.txt", []byte("old content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "target.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify source is gone, target has new content
	assert.Nil(t, fx.GetHandle("source.txt"))
	// Read the target to verify content
	targetHandle := fx.MustGetHandle("target.txt")
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: targetHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("new content"), readResp.Data)
}

// TestRename_NonExistentSource tests RENAME with non-existent source.
func TestRename_NonExistentSource(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "nonexistent.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNoEnt, resp.Status, "RENAME should return NFS3ErrNoEnt for non-existent source")
}

// TestRename_Directory tests renaming a directory.
func TestRename_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateDirectory("olddir")

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "olddir",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newdir",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	assert.Nil(t, fx.GetHandle("olddir"))
	assert.NotNil(t, fx.GetHandle("newdir"))
}

// TestRename_FromNotADirectory tests RENAME when source parent is not a directory.
func TestRename_FromNotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fileHandle, // file, not directory
		FromName:      "child.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "RENAME should return NFS3ErrNotDir for non-directory source parent")
}

// TestRename_ToNotADirectory tests RENAME when dest parent is not a directory.
func TestRename_ToNotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("content"))
	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   fileHandle, // file, not directory
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "RENAME should return NFS3ErrNotDir for non-directory dest parent")
}

// TestRename_EmptyFromName tests RENAME with empty source name.
func TestRename_EmptyFromName(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty from name should return NFS3ErrInval")
}

// TestRename_EmptyToName tests RENAME with empty destination name.
func TestRename_EmptyToName(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "",
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty to name should return NFS3ErrInval")
}

// TestRename_FromNameTooLong tests RENAME with source name too long.
func TestRename_FromNameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      string(longName),
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "From name > 255 bytes should return NFS3ErrNameTooLong")
}

// TestRename_ToNameTooLong tests RENAME with destination name too long.
func TestRename_ToNameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("content"))

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        string(longName),
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "To name > 255 bytes should return NFS3ErrNameTooLong")
}

// TestRename_EmptyFromHandle tests RENAME with empty source handle.
func TestRename_EmptyFromHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RenameRequest{
		FromDirHandle: []byte{},
		FromName:      "source.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty from handle should return NFS3ErrBadHandle")
}

// TestRename_EmptyToHandle tests RENAME with empty destination handle.
func TestRename_EmptyToHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   []byte{},
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty to handle should return NFS3ErrBadHandle")
}

// TestRename_ContextCancellation tests RENAME respects context cancellation.
func TestRename_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("source.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "source.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithCancellation(), req)

	// RENAME may return response with status or error
	if err != nil {
		return // Context cancellation detected via error
	}
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestRename_ReturnsWCC tests that RENAME returns WCC data.
func TestRename_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("wcc.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "wcc.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "wccnew.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.FromDirWccBefore, "Should return FromDirWccBefore")
	assert.NotNil(t, resp.FromDirWccAfter, "Should return FromDirWccAfter")
	assert.NotNil(t, resp.ToDirWccBefore, "Should return ToDirWccBefore")
	assert.NotNil(t, resp.ToDirWccAfter, "Should return ToDirWccAfter")
}

// TestRename_SameFile tests renaming a file to itself.
func TestRename_SameFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("samefile.txt", []byte("content"))

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "samefile.txt",
		ToDirHandle:   fx.RootHandle,
		ToName:        "samefile.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	// Renaming to same name should succeed (no-op)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, fx.GetHandle("samefile.txt"))
}

// TestRename_Symlink tests renaming a symlink.
func TestRename_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateSymlink("oldlink", "/target")

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "oldlink",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newlink",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	assert.Nil(t, fx.GetHandle("oldlink"))
	assert.NotNil(t, fx.GetHandle("newlink"))
}

// TestRename_DotEntry tests that RENAME fails for "." entry.
func TestRename_DotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      ".",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "RENAME '.' should return NFS3ErrInval")
}

// TestRename_DotDotEntry tests that RENAME fails for ".." entry.
func TestRename_DotDotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.RenameRequest{
		FromDirHandle: fx.RootHandle,
		FromName:      "..",
		ToDirHandle:   fx.RootHandle,
		ToName:        "newname",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "RENAME '..' should return NFS3ErrInval")
}

// TestRename_NestedDirectory tests renaming in nested directories.
func TestRename_NestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("a/b/c/nested.txt", []byte("content"))
	parentHandle := fx.MustGetHandle("a/b/c")

	req := &handlers.RenameRequest{
		FromDirHandle: parentHandle,
		FromName:      "nested.txt",
		ToDirHandle:   parentHandle,
		ToName:        "renamed.txt",
	}
	resp, err := fx.Handler.Rename(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	assert.Nil(t, fx.GetHandle("a/b/c/nested.txt"))
	assert.NotNil(t, fx.GetHandle("a/b/c/renamed.txt"))
}
