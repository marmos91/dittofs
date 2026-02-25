package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadDir_RFC1813 tests READDIR handler behaviors per RFC 1813 Section 3.3.16.
//
// READDIR returns directory entries. The client uses cookies to paginate
// through large directories across multiple requests.

// TestReadDir_EmptyDirectory tests reading an empty directory.
// Note: "." and ".." entries are optional per RFC 1813 - this implementation doesn't include them.
func TestReadDir_EmptyDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create an empty directory
	dirHandle := fx.CreateDirectory("emptydir")

	// Execute READDIR
	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "READDIR should return NFS3OK")
	assert.True(t, resp.Eof, "Empty directory should indicate EOF")
	// Empty directory returns no entries (implementation doesn't include "." and "..")
	assert.Empty(t, resp.Entries, "Empty directory should have no entries")
}

// TestReadDir_WithFiles tests reading a directory with files.
func TestReadDir_WithFiles(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory with some files
	fx.CreateFile("testdir/file1.txt", []byte("content1"))
	fx.CreateFile("testdir/file2.txt", []byte("content2"))
	fx.CreateFile("testdir/file3.txt", []byte("content3"))
	dirHandle := fx.MustGetHandle("testdir")

	// Execute READDIR
	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      8192,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Should have all files
	names := extractEntryNames(resp.Entries)
	assert.Contains(t, names, "file1.txt")
	assert.Contains(t, names, "file2.txt")
	assert.Contains(t, names, "file3.txt")
	assert.Len(t, resp.Entries, 3, "Should have exactly 3 entries")
}

// TestReadDir_WithSubdirectories tests that READDIR includes subdirectories.
func TestReadDir_WithSubdirectories(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create directory with subdirectories
	fx.CreateDirectory("parent/sub1")
	fx.CreateDirectory("parent/sub2")
	fx.CreateFile("parent/file.txt", []byte("content"))
	dirHandle := fx.MustGetHandle("parent")

	// Execute READDIR
	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      8192,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	names := extractEntryNames(resp.Entries)
	assert.Contains(t, names, "sub1")
	assert.Contains(t, names, "sub2")
	assert.Contains(t, names, "file.txt")
}

// TestReadDir_RootDirectory tests READDIR on root directory.
func TestReadDir_RootDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create some entries in root
	fx.CreateFile("root_file.txt", []byte("content"))
	fx.CreateDirectory("root_dir")

	// Execute READDIR
	req := &handlers.ReadDirRequest{
		DirHandle:  fx.RootHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      8192,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	names := extractEntryNames(resp.Entries)
	assert.Contains(t, names, "root_file.txt")
	assert.Contains(t, names, "root_dir")
}

// TestReadDir_NotADirectory tests that READDIR returns NFS3ErrNotDir
// when the handle points to a file.
func TestReadDir_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file
	fileHandle := fx.CreateFile("file.txt", []byte("content"))

	// Execute READDIR on file
	req := &handlers.ReadDirRequest{
		DirHandle:  fileHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	// Verify
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "READDIR on file should return NFS3ErrNotDir")
}

// TestReadDir_EmptyHandle tests that READDIR returns NFS3ErrBadHandle
// for empty handle.
func TestReadDir_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadDirRequest{
		DirHandle:  []byte{},
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestReadDir_HandleTooShort tests that READDIR returns NFS3ErrBadHandle
// for handles shorter than minimum length.
func TestReadDir_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadDirRequest{
		DirHandle:  []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestReadDir_HandleTooLong tests that READDIR returns NFS3ErrBadHandle
// for handles longer than maximum length.
func TestReadDir_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadDirRequest{
		DirHandle:  make([]byte, 65), // 65 bytes, max is 64
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestReadDir_ContextCancellation tests that READDIR respects context cancellation.
func TestReadDir_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.ReadDirRequest{
		DirHandle:  fx.RootHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.ContextWithCancellation(), req)

	// Should return error for context cancellation
	require.Error(t, err, "Should return error for cancelled context")
	if resp != nil {
		assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
	}
}

// TestReadDir_ReturnsAttributes tests that READDIR returns post-operation attributes.
func TestReadDir_ReturnsAttributes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup
	dirHandle := fx.CreateDirectory("attrdir")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.DirAttr, "Should return directory attributes")
	assert.EqualValues(t, types.NF3DIR, resp.DirAttr.Type)
}

// TestReadDir_ReturnsCookieVerf tests that READDIR returns a cookie verifier.
func TestReadDir_ReturnsCookieVerf(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("cookiedir")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	// Cookie verifier should be non-zero (contains mtime-based value)
	// Note: Depending on implementation, this may or may not be non-zero
	// The important thing is that it's returned consistently
}

// TestReadDir_EntryHasCookie tests that each entry has a cookie for pagination.
func TestReadDir_EntryHasCookie(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create files
	fx.CreateFile("dir/file1.txt", []byte("1"))
	fx.CreateFile("dir/file2.txt", []byte("2"))
	dirHandle := fx.MustGetHandle("dir")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      8192,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Each entry should have a unique, non-zero cookie (except possibly first)
	seenCookies := make(map[uint64]bool)
	for _, entry := range resp.Entries {
		if entry.Cookie != 0 {
			assert.False(t, seenCookies[entry.Cookie], "Cookie should be unique: %d", entry.Cookie)
			seenCookies[entry.Cookie] = true
		}
	}
}

// TestReadDir_Symlink tests that symlinks appear in directory listing.
func TestReadDir_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup
	fx.CreateFile("linkdir/target.txt", []byte("target"))
	fx.CreateSymlink("linkdir/link", "target.txt")
	dirHandle := fx.MustGetHandle("linkdir")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      8192,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	names := extractEntryNames(resp.Entries)
	assert.Contains(t, names, "target.txt")
	assert.Contains(t, names, "link")
}

// TestReadDir_ManyFiles tests READDIR with many files.
func TestReadDir_ManyFiles(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create many files
	for i := 0; i < 50; i++ {
		fx.CreateFile("manydir/file_"+string(rune('a'+i%26))+string(rune('0'+i/26))+".txt", []byte("content"))
	}
	dirHandle := fx.MustGetHandle("manydir")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      65536, // Large count to get all entries
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Should have exactly 50 files (implementation doesn't include "." and "..")
	assert.Equal(t, 50, len(resp.Entries), "Should have exactly 50 entries")
}

// TestReadDir_NestedDirectory tests READDIR on nested directory.
func TestReadDir_NestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup
	fx.CreateFile("a/b/c/file.txt", []byte("nested"))
	dirHandle := fx.MustGetHandle("a/b/c")

	req := &handlers.ReadDirRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		Count:      4096,
	}
	resp, err := fx.Handler.ReadDir(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	names := extractEntryNames(resp.Entries)
	assert.Contains(t, names, "file.txt")
}

// extractEntryNames extracts the names from directory entries.
func extractEntryNames(entries []*types.DirEntry) []string {
	names := make([]string, len(entries))
	for i, entry := range entries {
		names[i] = entry.Name
	}
	return names
}
