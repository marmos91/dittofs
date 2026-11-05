package handlers_test

import (
	"testing"

	"github.com/cubbit/dittofs/internal/adapter/nfs/types"
	"github.com/cubbit/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/cubbit/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWrite_RFC1813 tests WRITE handler behaviors per RFC 1813 Section 3.3.7.
//
// WRITE is used to write data to a regular file. It supports:
// - Writing at any offset
// - Extending files beyond their current size
// - Different stability levels (UNSTABLE, DATA_SYNC, FILE_SYNC)
// - WCC data for cache consistency

// TestWrite_Directory tests that WRITE returns NFS3ErrIsDir for directories.
func TestWrite_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create a directory
	dirHandle := fx.CreateDirectory("testdir")

	req := &handlers.WriteRequest{
		Handle: dirHandle,
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrIsDir, resp.Status, "WRITE on directory should return NFS3ErrIsDir")
}

// TestWrite_EmptyHandle tests that WRITE returns error for empty handle.
func TestWrite_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.WriteRequest{
		Handle: []byte{},
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.Context(), req)

	require.NoError(t, err)
	// RFC 1813: NFS3ErrBadHandle for invalid file handles
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestWrite_HandleTooShort tests that WRITE returns error for short handles.
func TestWrite_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.WriteRequest{
		Handle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.Context(), req)

	require.NoError(t, err)
	// RFC 1813: NFS3ErrBadHandle for invalid file handles
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestWrite_HandleTooLong tests that WRITE returns error for long handles.
func TestWrite_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.WriteRequest{
		Handle: make([]byte, 65), // 65 bytes, max is 64
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.Context(), req)

	require.NoError(t, err)
	// RFC 1813: NFS3ErrBadHandle for invalid file handles
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestWrite_ContextCancellation tests that WRITE respects context cancellation.
func TestWrite_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("cancel.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithCancellation(), req)

	// WRITE returns response with status (may or may not return error)
	if err != nil {
		return // Context cancellation detected via error
	}
	// If no error, status should indicate IO error
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestWrite_InvalidStability tests WRITE with invalid stability level.
func TestWrite_InvalidStability(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("invalid.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 99, // Invalid
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Invalid stability should return NFS3ErrInval")
}

// TestWrite_Symlink tests that WRITE on symlink fails.
func TestWrite_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	symlinkHandle := fx.CreateSymlink("link", "/target")

	req := &handlers.WriteRequest{
		Handle: symlinkHandle,
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	// WRITE on symlink should fail (not a regular file)
	assert.NotEqual(t, types.NFS3OK, resp.Status, "WRITE on symlink should not return NFS3OK")
}

// TestWrite_CountMismatch tests when Count differs from len(Data).
func TestWrite_CountMismatch(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("mismatch.txt", []byte{})

	// Count says 10 but data is only 5 bytes
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  10,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	// Implementation should handle gracefully (write actual data length)
	require.NoError(t, err)
	// Either success with actual bytes written, or error for mismatch
	if resp.Status == types.NFS3OK {
		// Should only write actual data length (5 bytes)
		assert.EqualValues(t, 5, resp.Count)
	}
}

// TestWrite_ZeroBytes tests writing zero bytes.
func TestWrite_ZeroBytes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("zero.txt", []byte("content"))

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  0,
		Stable: 2,
		Data:   []byte{},
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 0, resp.Count)
}
