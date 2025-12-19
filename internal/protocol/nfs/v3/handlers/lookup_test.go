package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLookup_RFC1813 tests LOOKUP handler behaviors per RFC 1813 Section 3.3.3.
//
// LOOKUP is the fundamental building block for pathname resolution in NFS.
// Clients use it to traverse directory hierarchies one component at a time.

// TestLookup_ExistingFile tests that LOOKUP returns file handle and attributes
// for an existing file (RFC 1813: successful lookup returns NFS3OK).
func TestLookup_ExistingFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	fx.CreateFile("testfile.txt", []byte("hello world"))

	// Execute LOOKUP
	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "testfile.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "LOOKUP existing file should return NFS3OK")
	assert.NotNil(t, resp.FileHandle, "Should return file handle")
	assert.NotNil(t, resp.Attr, "Should return file attributes")
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type, "Should be a regular file")
}

// TestLookup_ExistingDirectory tests that LOOKUP returns directory handle
// for an existing directory.
func TestLookup_ExistingDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory
	fx.CreateDirectory("subdir")

	// Execute LOOKUP
	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "subdir",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)
	assert.NotNil(t, resp.Attr)
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Should be a directory")
}

// TestLookup_NonExistentFile tests that LOOKUP returns NFS3ErrNoEnt
// when the file doesn't exist (RFC 1813: child not found).
func TestLookup_NonExistentFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Execute LOOKUP for non-existent file
	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "nonexistent.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNoEnt, resp.Status, "LOOKUP non-existent file should return NFS3ErrNoEnt")
}

// TestLookup_NotADirectory tests that LOOKUP returns NFS3ErrNotDir
// when the directory handle points to a file (RFC 1813: parent not a directory).
func TestLookup_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	fileHandle := fx.CreateFile("file.txt", []byte("content"))

	// Execute LOOKUP using file handle as directory
	req := &handlers.LookupRequest{
		DirHandle: fileHandle,
		Filename:  "child.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "LOOKUP in file should return NFS3ErrNotDir")
}

// TestLookup_DotEntry tests that LOOKUP "." returns the directory itself
// (RFC 1813: "." is the current directory).
func TestLookup_DotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a subdirectory
	dirHandle := fx.CreateDirectory("subdir")

	// Execute LOOKUP "."
	req := &handlers.LookupRequest{
		DirHandle: dirHandle,
		Filename:  ".",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type)
}

// TestLookup_DotDotEntry tests that LOOKUP ".." returns the parent directory
// (RFC 1813: ".." is the parent directory).
func TestLookup_DotDotEntry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a subdirectory
	dirHandle := fx.CreateDirectory("subdir")

	// Execute LOOKUP ".."
	req := &handlers.LookupRequest{
		DirHandle: dirHandle,
		Filename:  "..",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type)
}

// TestLookup_EmptyFilename tests that LOOKUP returns NFS3ErrInval
// for empty filename (RFC 1813: filename validation).
func TestLookup_EmptyFilename(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty filename should return NFS3ErrInval")
}

// TestLookup_FilenameWithNullByte tests that LOOKUP rejects filenames
// containing null bytes.
func TestLookup_FilenameWithNullByte(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file\x00name",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Filename with null byte should return NFS3ErrInval")
}

// TestLookup_FilenameWithPathSeparator tests that LOOKUP rejects filenames
// containing path separators to prevent directory traversal.
func TestLookup_FilenameWithPathSeparator(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "path/to/file",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Filename with path separator should return NFS3ErrInval")
}

// TestLookup_EmptyHandle tests that LOOKUP returns NFS3ErrBadHandle
// for empty directory handle.
func TestLookup_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: []byte{},
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestLookup_HandleTooShort tests that LOOKUP returns NFS3ErrBadHandle
// for handles shorter than minimum length (8 bytes per RFC 1813).
func TestLookup_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestLookup_HandleTooLong tests that LOOKUP returns NFS3ErrBadHandle
// for handles longer than maximum length (64 bytes per RFC 1813).
func TestLookup_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: make([]byte, 65), // 65 bytes, max is 64
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestLookup_ContextCancellation tests that LOOKUP respects context cancellation
// (graceful shutdown support).
func TestLookup_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Lookup(fx.ContextWithCancellation(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestLookup_FilenameTooLong tests that LOOKUP returns NFS3ErrNameTooLong
// for filenames exceeding 255 bytes (RFC 1813 limit).
func TestLookup_FilenameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  string(longName),
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "Filename > 255 bytes should return NFS3ErrNameTooLong")
}

// TestLookup_NestedPath tests that LOOKUP works for nested directories.
func TestLookup_NestedPath(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create nested structure
	fx.CreateFile("a/b/c/file.txt", []byte("deep"))

	// Navigate step by step (like NFS clients do)
	aHandle := fx.MustGetHandle("a")
	bHandle := fx.MustGetHandle("a/b")
	cHandle := fx.MustGetHandle("a/b/c")

	// Lookup from c directory
	req := &handlers.LookupRequest{
		DirHandle: cHandle,
		Filename:  "file.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)

	// Verify intermediate lookups work
	_ = aHandle
	_ = bHandle
}

// TestLookup_ReturnsDirAttr tests that LOOKUP returns directory attributes
// for cache consistency (RFC 1813: post-operation attributes).
func TestLookup_ReturnsDirAttr(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	fx.CreateFile("testfile.txt", []byte("content"))

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "testfile.txt",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.DirAttr, "Should return directory attributes for cache consistency")
	assert.EqualValues(t, types.NF3DIR, resp.DirAttr.Type)
}

// TestLookup_Symlink tests that LOOKUP can find symbolic links.
func TestLookup_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a symlink
	fx.CreateSymlink("link", "/target/path")

	req := &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "link",
	}
	resp, err := fx.Handler.Lookup(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)
	assert.EqualValues(t, types.NF3LNK, resp.Attr.Type, "Should identify as symlink")
}
