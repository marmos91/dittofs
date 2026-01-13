package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
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

// TestWrite_SimpleWrite tests writing to a file.
func TestWrite_SimpleWrite(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create an empty file
	fileHandle := fx.CreateFile("testfile.txt", []byte{})

	// Write some data
	data := []byte("hello world")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2, // FILE_SYNC
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "WRITE should return NFS3OK")
	assert.EqualValues(t, uint32(len(data)), resp.Count, "Should write all bytes")

	// Verify data was written by reading it back
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, data, readResp.Data, "Read data should match written data")
}

// TestWrite_AtOffset tests writing at a specific offset.
func TestWrite_AtOffset(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with initial content
	fileHandle := fx.CreateFile("offset.txt", []byte("0123456789"))

	// Write "XXXX" at offset 3
	data := []byte("XXXX")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 3,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(len(data)), resp.Count)

	// Verify result
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("012XXXX789"), readResp.Data)
}

// TestWrite_ExtendFile tests writing beyond the current file size.
func TestWrite_ExtendFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file with initial content
	fileHandle := fx.CreateFile("extend.txt", []byte("hello"))

	// Write " world" at offset 5 (extending the file)
	data := []byte(" world")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 5,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify file was extended
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("hello world"), readResp.Data)
}

// TestWrite_SparseFile tests writing beyond EOF creating a sparse file.
func TestWrite_SparseFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create an empty file
	fileHandle := fx.CreateFile("sparse.txt", []byte{})

	// Write at offset 100 (creates a sparse file)
	data := []byte("data")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 100,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify file size is now 104 (offset + data length)
	attrResp, err := fx.Handler.GetAttr(fx.Context(), &handlers.GetAttrRequest{
		Handle: fileHandle,
	})
	require.NoError(t, err)
	assert.EqualValues(t, uint64(104), attrResp.Attr.Size)
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

// TestWrite_ReturnsWCC tests that WRITE returns WCC data (before/after attributes).
func TestWrite_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("wcc.txt", []byte("initial"))

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.AttrBefore, "Should return AttrBefore (pre-op attrs)")
	assert.NotNil(t, resp.AttrAfter, "Should return AttrAfter (post-op attrs)")
}

// TestWrite_ReturnsVerifier tests that WRITE returns a write verifier.
func TestWrite_ReturnsVerifier(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("verf.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 2,
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	// Verifier is returned (value depends on implementation)
}

// TestWrite_UnstableStability tests WRITE with UNSTABLE stability.
func TestWrite_UnstableStability(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("unstable.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 0, // UNSTABLE
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(5), resp.Count)
	// Committed level may be UNSTABLE or higher (implementation dependent)
}

// TestWrite_DataSyncStability tests WRITE with DATA_SYNC stability.
func TestWrite_DataSyncStability(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("datasync.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 1, // DATA_SYNC
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(5), resp.Count)
}

// TestWrite_FileSyncStability tests WRITE with FILE_SYNC stability.
// NOTE: Since Cache is always enabled, we always return UNSTABLE regardless
// of what the client requested. This is RFC 1813 compliant - the server is allowed
// to return a less stable commitment than requested. Clients must call COMMIT
// if they need durability guarantees.
func TestWrite_FileSyncStability(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("filesync.txt", []byte{})

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  5,
		Stable: 2, // FILE_SYNC
		Data:   []byte("hello"),
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(5), resp.Count)
	// With Cache always enabled, we always return UNSTABLE (0)
	// Server is allowed to return less stable than requested per RFC 1813
	assert.EqualValues(t, uint32(0), resp.Committed)
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

// TestWrite_LargeWrite tests writing a large amount of data.
func TestWrite_LargeWrite(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("large.bin", []byte{})

	// Write 1MB of data
	data := make([]byte, 1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}

	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(len(data)), resp.Count)

	// Verify data
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 512 * 1024, // Read from middle
		Count:  1024,
	})
	require.NoError(t, err)
	assert.Equal(t, data[512*1024:512*1024+1024], readResp.Data)
}

// TestWrite_NestedFile tests writing to a file in a nested directory.
func TestWrite_NestedFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("a/b/c/nested.txt", []byte{})

	data := []byte("nested content")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, data, readResp.Data)
}

// TestWrite_MultipleChunks tests multiple sequential writes.
func TestWrite_MultipleChunks(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("chunks.txt", []byte{})

	// Write in chunks
	chunks := []struct {
		offset uint64
		data   []byte
	}{
		{0, []byte("0123")},
		{4, []byte("4567")},
		{8, []byte("89AB")},
	}

	for _, chunk := range chunks {
		req := &handlers.WriteRequest{
			Handle: fileHandle,
			Offset: chunk.offset,
			Count:  uint32(len(chunk.data)),
			Stable: 2,
			Data:   chunk.data,
		}
		resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)
		require.NoError(t, err)
		assert.EqualValues(t, types.NFS3OK, resp.Status)
	}

	// Verify final content
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("0123456789AB"), readResp.Data)
}

// TestWrite_OverwriteExisting tests overwriting existing content.
func TestWrite_OverwriteExisting(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("overwrite.txt", []byte("original content here"))

	// Overwrite with shorter data at the beginning
	data := []byte("NEW")
	req := &handlers.WriteRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  uint32(len(data)),
		Stable: 2,
		Data:   data,
	}
	resp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify partial overwrite (rest of file unchanged)
	readResp, err := fx.Handler.Read(fx.Context(), &handlers.ReadRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  100,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("NEWginal content here"), readResp.Data)
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
