package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetAttr_RFC1813 tests GETATTR handler behaviors per RFC 1813 Section 3.3.1.
//
// GETATTR is the fundamental operation for retrieving file metadata.
// It's one of the most frequently called NFS procedures.

// TestGetAttr_RegularFile tests that GETATTR returns correct attributes for a regular file.
func TestGetAttr_RegularFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with known content
	content := []byte("hello world")
	fileHandle := fx.CreateFile("testfile.txt", content)

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "GETATTR should return NFS3OK for existing file")
	assert.NotNil(t, resp.Attr, "Should return file attributes")
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type, "Type should be regular file")
	assert.EqualValues(t, uint64(len(content)), resp.Attr.Size, "Size should match content length")
}

// TestGetAttr_Directory tests that GETATTR returns correct attributes for a directory.
func TestGetAttr_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory
	dirHandle := fx.CreateDirectory("testdir")

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: dirHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.Attr)
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Type should be directory")
}

// TestGetAttr_Symlink tests that GETATTR returns correct attributes for a symlink.
func TestGetAttr_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a symlink
	target := "/some/target/path"
	symlinkHandle := fx.CreateSymlink("mylink", target)

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: symlinkHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.Attr)
	assert.EqualValues(t, types.NF3LNK, resp.Attr.Type, "Type should be symlink")
	assert.EqualValues(t, uint64(len(target)), resp.Attr.Size, "Symlink size should be target length")
}

// TestGetAttr_RootDirectory tests that GETATTR returns attributes for root directory.
func TestGetAttr_RootDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Execute GETATTR on root handle
	req := &handlers.GetAttrRequest{
		Handle: fx.RootHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.Attr)
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Root should be a directory")
}

// TestGetAttr_EmptyHandle tests that GETATTR returns NFS3ErrBadHandle for empty handle.
func TestGetAttr_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.GetAttrRequest{
		Handle: []byte{},
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestGetAttr_HandleTooShort tests that GETATTR returns NFS3ErrBadHandle
// for handles shorter than minimum length (8 bytes per RFC 1813).
func TestGetAttr_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.GetAttrRequest{
		Handle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestGetAttr_HandleTooLong tests that GETATTR returns NFS3ErrBadHandle
// for handles longer than maximum length (64 bytes per RFC 1813).
func TestGetAttr_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.GetAttrRequest{
		Handle: make([]byte, 65), // 65 bytes, max is 64
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestGetAttr_ContextCancellation tests that GETATTR respects context cancellation.
func TestGetAttr_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.GetAttrRequest{
		Handle: fx.RootHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.ContextWithCancellation(), req)

	// Should return error for context cancellation
	require.Error(t, err, "Should return error for cancelled context")
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestGetAttr_InvalidHandle tests that GETATTR returns NFS3ErrStale
// for a well-formed but non-existent handle.
func TestGetAttr_InvalidHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a well-formed but invalid handle (16 bytes)
	invalidHandle := make([]byte, 16)
	for i := range invalidHandle {
		invalidHandle[i] = byte(i)
	}

	req := &handlers.GetAttrRequest{
		Handle: invalidHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	require.NoError(t, err)
	// Non-existent handle returns NFS3ErrStale or NFS3ErrBadHandle depending on implementation
	assert.True(t,
		resp.Status == types.NFS3ErrStale || resp.Status == types.NFS3ErrBadHandle,
		"Invalid handle should return NFS3ErrStale or NFS3ErrBadHandle, got %d", resp.Status)
}

// TestGetAttr_FileAttributes tests that GETATTR returns all expected attributes.
func TestGetAttr_FileAttributes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	content := []byte("test content")
	fileHandle := fx.CreateFile("attrs.txt", content)

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify all attributes are present
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	attr := resp.Attr
	assert.NotNil(t, attr)

	// Type should be regular file
	assert.EqualValues(t, types.NF3REG, attr.Type)

	// Mode should include file type bits
	assert.True(t, attr.Mode > 0, "Mode should be non-zero")

	// Size should match content
	assert.EqualValues(t, uint64(len(content)), attr.Size)

	// UID and GID should be set
	// Note: specific values depend on test fixture configuration

	// Timestamps should be set
	assert.NotNil(t, attr.Atime, "Access time should be set")
	assert.NotNil(t, attr.Mtime, "Modification time should be set")
	assert.NotNil(t, attr.Ctime, "Change time should be set")
}

// TestGetAttr_NestedFile tests that GETATTR works for files in nested directories.
func TestGetAttr_NestedFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a deeply nested file
	content := []byte("nested content")
	fileHandle := fx.CreateFile("a/b/c/nested.txt", content)

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type)
	assert.EqualValues(t, uint64(len(content)), resp.Attr.Size)
}

// TestGetAttr_EmptyFile tests that GETATTR returns size 0 for empty files.
func TestGetAttr_EmptyFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create an empty file
	fileHandle := fx.CreateFile("empty.txt", []byte{})

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type)
	assert.EqualValues(t, uint64(0), resp.Attr.Size, "Empty file should have size 0")
}

// TestGetAttr_LargeFile tests that GETATTR returns correct size for large files.
func TestGetAttr_LargeFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with known large content
	content := make([]byte, 1024*1024) // 1MB
	for i := range content {
		content[i] = byte(i % 256)
	}
	fileHandle := fx.CreateFile("large.bin", content)

	// Execute GETATTR
	req := &handlers.GetAttrRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.GetAttr(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint64(len(content)), resp.Attr.Size, "Size should match content length")
}
