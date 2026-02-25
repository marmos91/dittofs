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
