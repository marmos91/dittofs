package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRead_RFC1813 tests READ handler behaviors per RFC 1813 Section 3.3.6.
//
// READ is used to retrieve file content from the server. It supports:
// - Partial reads (offset + count)
// - EOF detection
// - Post-operation attributes for cache consistency

// TestRead_EntireFile tests reading an entire file.
func TestRead_EntireFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with known content
	content := []byte("hello world")
	fileHandle := fx.CreateFile("testfile.txt", content)

	// Execute READ from offset 0 with count >= file size
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(content) + 100), // Request more than available
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "READ should return NFS3OK")
	assert.EqualValues(t, uint32(len(content)), resp.Count, "Count should equal content length")
	assert.True(t, resp.Eof, "Should indicate EOF")
	assert.Equal(t, content, resp.Data, "Data should match original content")
}

// TestRead_PartialRead tests reading part of a file.
func TestRead_PartialRead(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with known content
	content := []byte("hello world, this is a longer file")
	fileHandle := fx.CreateFile("partial.txt", content)

	// Execute READ for first 5 bytes only
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 5, resp.Count, "Should return requested count")
	assert.False(t, resp.Eof, "Should NOT indicate EOF - more data available")
	assert.Equal(t, []byte("hello"), resp.Data)
}

// TestRead_FromOffset tests reading from a specific offset.
func TestRead_FromOffset(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with known content
	content := []byte("hello world")
	fileHandle := fx.CreateFile("offset.txt", content)

	// Execute READ from offset 6
	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 6,
		Count:  100, // Request more than remaining
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 5, resp.Count, "Should return remaining bytes")
	assert.True(t, resp.Eof, "Should indicate EOF")
	assert.Equal(t, []byte("world"), resp.Data)
}

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

// TestRead_ReturnsPostOpAttr tests that READ returns post-operation attributes.
func TestRead_ReturnsPostOpAttr(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	content := []byte("hello world")
	fileHandle := fx.CreateFile("attrs.txt", content)

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(content)),
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.Attr, "Should return post-operation attributes")
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type)
	assert.EqualValues(t, uint64(len(content)), resp.Attr.Size)
}

// TestRead_ZeroCount tests reading with count=0.
func TestRead_ZeroCount(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	content := []byte("hello world")
	fileHandle := fx.CreateFile("zero.txt", content)

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  0,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	// Zero count should return success with 0 bytes
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 0, resp.Count)
}

// TestRead_ExactFileSize tests reading exactly the file size.
func TestRead_ExactFileSize(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	content := []byte("exact size content")
	fileHandle := fx.CreateFile("exact.txt", content)

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(content)), // Exactly file size
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(len(content)), resp.Count)
	assert.True(t, resp.Eof, "Should indicate EOF when reading exact file size")
	assert.Equal(t, content, resp.Data)
}

// TestRead_MultipleChunks tests sequential reads to simulate chunked reading.
func TestRead_MultipleChunks(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	content := []byte("0123456789abcdef")
	fileHandle := fx.CreateFile("chunks.txt", content)

	// Read first chunk
	req1 := &handlers.ReadRequest{Handle: fileHandle, Offset: 0, Count: 4}
	resp1, err := fx.Handler.Read(fx.Context(), req1)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp1.Status)
	assert.Equal(t, []byte("0123"), resp1.Data)
	assert.False(t, resp1.Eof)

	// Read second chunk
	req2 := &handlers.ReadRequest{Handle: fileHandle, Offset: 4, Count: 4}
	resp2, err := fx.Handler.Read(fx.Context(), req2)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp2.Status)
	assert.Equal(t, []byte("4567"), resp2.Data)
	assert.False(t, resp2.Eof)

	// Read last chunk
	req3 := &handlers.ReadRequest{Handle: fileHandle, Offset: 12, Count: 100}
	resp3, err := fx.Handler.Read(fx.Context(), req3)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp3.Status)
	assert.Equal(t, []byte("cdef"), resp3.Data)
	assert.True(t, resp3.Eof)
}

// TestRead_NestedFile tests reading a file in a nested directory.
func TestRead_NestedFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create nested file
	content := []byte("nested content here")
	fileHandle := fx.CreateFile("a/b/c/nested.txt", content)

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(content)),
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.Equal(t, content, resp.Data)
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

// TestRead_LargeFile tests reading from a large file.
func TestRead_LargeFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a 1MB file
	content := make([]byte, 1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}
	fileHandle := fx.CreateFile("large.bin", content)

	// Read a chunk from the middle
	offset := uint64(512 * 1024)
	count := uint32(1024)

	req := &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: offset,
		Count:  count,
	}
	resp, err := fx.Handler.Read(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, count, resp.Count)
	assert.Equal(t, content[offset:offset+uint64(count)], resp.Data)
	assert.False(t, resp.Eof)
}
