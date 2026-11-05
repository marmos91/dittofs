package handlers_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/types"
	"github.com/cubbit/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/cubbit/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRead_RFC1813 tests READ handler behaviors per RFC 1813 Section 3.3.6.
//
// READ is used to retrieve file content from the server. It supports:
// - Partial reads (offset + count)
// - EOF detection
// - Post-operation attributes for cache consistency

// TestRead_BeyondEOF tests reading past the end of file.
func TestRead_BeyondEOF(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	content := []byte("hello")
	fileHandle := fx.CreateFile("eof.txt", content)

	// Execute READ starting beyond file end
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 1000, // Way beyond file size
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify - should return 0 bytes with EOF
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 0, resp.Count, "Should return 0 bytes")
	assert.True(t, resp.Eof, "Should indicate EOF")
	assert.Empty(t, resp.Data)
}

// TestRead_EmptyFile tests reading an empty file.
func TestRead_EmptyFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create empty file
	fileHandle := fx.CreateFile("empty.txt", []byte{})

	// Execute READ
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 0, resp.Count)
	assert.True(t, resp.Eof, "Empty file read should indicate EOF")
}

// TestRead_Directory tests that READ returns NFS3ErrIsDir for directories.
func TestRead_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory
	dirHandle := fx.CreateDirectory("testdir")

	// Execute READ on directory
	req := &handlers.ReadRequest{
		Handle: dirHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrIsDir, resp.Status, "READ on directory should return NFS3ErrIsDir")
}

// TestRead_EmptyHandle tests that READ returns NFS3ErrBadHandle for empty handle.
func TestRead_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadRequest{
		Handle: []byte{},
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestRead_HandleTooShort tests that READ returns NFS3ErrBadHandle
// for handles shorter than minimum length.
func TestRead_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadRequest{
		Handle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestRead_HandleTooLong tests that READ returns NFS3ErrBadHandle
// for handles longer than maximum length.
func TestRead_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadRequest{
		Handle: make([]byte, 65), // 65 bytes, max is 64
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestRead_ContextCancellation tests that READ respects context cancellation.
func TestRead_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a file
	fileHandle := fx.CreateFile("cancel.txt", []byte("content"))

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.ContextWithCancellation(), req)

	// Should return error for context cancellation
	require.Error(t, err, "Should return error for cancelled context")
	if resp != nil {
		assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
	}
}

// TestRead_Symlink tests that READ on a symlink fails appropriately.
// Note: NFS clients typically resolve symlinks before reading.
func TestRead_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a symlink
	symlinkHandle := fx.CreateSymlink("link", "/target")

	req := &handlers.ReadRequest{
		Handle: symlinkHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// READ on symlink should fail - it's not a regular file
	require.NoError(t, err)
	// Implementation may return NFS3ErrInval, NFS3ErrIsDir, or similar
	assert.NotEqual(t, types.NFS3OK, resp.Status,
		"READ on symlink should not return NFS3OK")
}

// TestRead_PermissionDenied verifies that READ on a mode-000 file owned by root
// returns NFS3ErrAccess when the caller is a non-root user, proving the
// PrepareRead permission gate is active.
func TestRead_PermissionDenied(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	rootCtx := fx.ContextWithUID(0, 0)

	// Create a file as root (UID 0).
	createResp, err := fx.Handler.Create(rootCtx, &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "secret.txt",
		Mode:      types.CreateUnchecked,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, createResp.Status)
	fileHandle := createResp.FileHandle

	// chmod 000 via SETATTR so no one but root may read.
	mode := uint32(0000)
	setattrResp, err := fx.Handler.SetAttr(rootCtx, &handlers.SetAttrRequest{
		Handle:  fileHandle,
		NewAttr: metadata.SetAttrs{Mode: &mode},
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, setattrResp.Status)

	// Read as non-root user (uid=1000) -> must be denied.
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	}
	resp, err := fx.Handler.Read(fx.Context(), req) // fx.Context() uses uid=1000

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrAccess, resp.Status,
		"READ on mode-000 root-owned file by non-root must return NFS3ErrAccess")
}
