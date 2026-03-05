package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadDirPlus_EmptyDirectory tests READDIRPLUS on an empty directory.
func TestReadDirPlus_EmptyDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("emptydir")

	req := &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   4096,
		MaxCount:   8192,
	}
	resp, err := fx.Handler.ReadDirPlus(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.True(t, resp.Eof, "Empty directory should indicate EOF")
	assert.Empty(t, resp.Entries, "Empty directory should have no entries")
}

// TestReadDirPlus_WithFiles tests READDIRPLUS returns entries with attributes.
func TestReadDirPlus_WithFiles(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("plusdir/file1.txt", []byte("content1"))
	fx.CreateFile("plusdir/file2.txt", []byte("content2"))
	dirHandle := fx.MustGetHandle("plusdir")

	req := &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   4096,
		MaxCount:   65536,
	}
	resp, err := fx.Handler.ReadDirPlus(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.Len(t, resp.Entries, 2, "Should have 2 entries")

	// READDIRPLUS entries include attributes
	names := make([]string, len(resp.Entries))
	for i, entry := range resp.Entries {
		names[i] = entry.Name
		assert.NotNil(t, entry.Attr, "Entry %q should have attributes", entry.Name)
	}
	assert.Contains(t, names, "file1.txt")
	assert.Contains(t, names, "file2.txt")
}

// TestReadDirPlus_InvalidHandle tests READDIRPLUS with an invalid handle.
func TestReadDirPlus_InvalidHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	invalidHandle := make([]byte, 16)
	for i := range invalidHandle {
		invalidHandle[i] = byte(i)
	}

	req := &handlers.ReadDirPlusRequest{
		DirHandle:  invalidHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   4096,
		MaxCount:   8192,
	}
	resp, err := fx.Handler.ReadDirPlus(fx.Context(), req)

	require.NoError(t, err)
	assert.NotEqualValues(t, types.NFS3OK, resp.Status,
		"Invalid handle should not return NFS3OK")
}

// TestReadDirPlus_StaleVerifierContinues tests that READDIRPLUS continues serving entries
// when the cookie verifier is stale (directory modified between paginated reads).
// This prevents macOS Finder error -8062 during concurrent directory operations.
func TestReadDirPlus_StaleVerifierContinues(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory with files
	fx.CreateFile("stale/file1.txt", []byte("1"))
	fx.CreateFile("stale/file2.txt", []byte("2"))
	dirHandle := fx.MustGetHandle("stale")

	// First read: get the cookie verifier
	resp1, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp1.Status)
	savedVerifier := resp1.CookieVerf

	// Modify the directory (changes mtime, invalidates verifier)
	fx.CreateFile("stale/file3.txt", []byte("3"))

	// Second read with old verifier and non-zero cookie — should succeed, not BAD_COOKIE
	resp2, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     1, // non-zero cookie
		CookieVerf: savedVerifier,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp2.Status, "Stale verifier should not return BAD_COOKIE")
}
