package handlers_test

import (
	"fmt"
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
// TestReadDirPlus_StaleVerifierBadCookie verifies that a non-zero-cookie
// READDIRPLUS carrying a stale cookie verifier returns NFS3ERR_BAD_COOKIE per
// RFC 1813 Section 3.3.17, mirroring the NFSv4 READDIR behavior.
func TestReadDirPlus_StaleVerifierBadCookie(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a directory with files
	fx.CreateFile("stale/file1.txt", []byte("1"))
	fx.CreateFile("stale/file2.txt", []byte("2"))
	dirHandle := fx.MustGetHandle("stale")

	// First read: get the cookie verifier and a real resume cookie
	resp1, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp1.Status)
	require.NotEmpty(t, resp1.Entries, "expected at least one directory entry to obtain a resume cookie")
	resumeCookie := resp1.Entries[len(resp1.Entries)-1].Cookie
	savedVerifier := resp1.CookieVerf
	require.NotZero(t, resumeCookie, "resume cookie must be non-zero to exercise the verifier check path")
	require.NotZero(t, savedVerifier, "saved verifier must be non-zero to exercise the verifier check path")

	// Modify the directory (changes mtime, invalidates verifier)
	fx.CreateFile("stale/file3.txt", []byte("3"))

	// Second read with old verifier and a real non-zero cookie — must return BAD_COOKIE.
	resp2, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     resumeCookie,
		CookieVerf: savedVerifier,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadCookie, resp2.Status, "stale verifier must return NFS3ERR_BAD_COOKIE")
}

// TestReadDirPlus_ZeroCookieIgnoresVerifierMismatch verifies the verifier check
// is bypassed for the initial request (cookie=0) and for verifier-less clients
// (verifier=0), even when the supplied verifier is garbage.
func TestReadDirPlus_ZeroCookieIgnoresVerifierMismatch(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fx.CreateFile("zc/aaa.txt", []byte("1"))
	dirHandle := fx.MustGetHandle("zc")

	resp, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0xDEADBEEFCAFEBABE,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "initial READDIRPLUS (cookie=0) must ignore verifier mismatch")
	require.NotEmpty(t, resp.Entries, "expected entries from initial READDIRPLUS")
	resumeCookie := resp.Entries[len(resp.Entries)-1].Cookie

	resp2, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     resumeCookie,
		CookieVerf: 0,
		DirCount:   8192,
		MaxCount:   65536,
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp2.Status, "zero verifier must bypass the cookie check")
}

// TestReadDirPlus_MaxCountTruncation verifies H11: when the client's maxcount
// byte budget is smaller than the encoded reply for all entries, READDIRPLUS
// returns a partial list with eof=false so the client resumes from the last
// emitted cookie (RFC 1813 Section 3.3.17).
func TestReadDirPlus_MaxCountTruncation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	const numFiles = 40
	for i := 0; i < numFiles; i++ {
		fx.CreateFile(fmt.Sprintf("budgetdir/file%02d.txt", i), []byte("x"))
	}
	dirHandle := fx.MustGetHandle("budgetdir")

	// Each READDIRPLUS entry costs ~140+ bytes (value_follows + fileid + name +
	// cookie + fattr3 + handle). A tiny maxcount must force truncation. DirCount
	// is kept large so only the maxcount (total reply) budget bites.
	// maxcount must be >= dircount per RFC 1813 / request validation. Keep both
	// small and equal so the total-reply (maxcount) budget bites first: each
	// entry's total cost (~140 B incl. fattr3+handle) far exceeds its dir-info
	// cost (~32 B), so maxcount truncates before dircount does.
	req := &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   512,
		MaxCount:   512, // far smaller than the full reply
	}
	resp, err := fx.Handler.ReadDirPlus(fx.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)

	// Must be a partial reply.
	assert.Greater(t, len(resp.Entries), 0, "should emit at least one entry")
	assert.Less(t, len(resp.Entries), numFiles, "should not emit all entries under a tiny maxcount")
	assert.False(t, resp.Eof, "eof must be false when the reply was truncated by maxcount")

	// The encoded reply must actually fit within the advertised maxcount.
	encoded, err := resp.Encode()
	require.NoError(t, err)
	assert.LessOrEqual(t, len(encoded), int(req.MaxCount),
		"encoded reply (%d bytes) must not exceed maxcount (%d)", len(encoded), req.MaxCount)

	// Resuming from the last cookie must make forward progress (no infinite loop).
	lastCookie := resp.Entries[len(resp.Entries)-1].Cookie
	resp2, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     lastCookie,
		CookieVerf: resp.CookieVerf,
		DirCount:   512,
		MaxCount:   512,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp2.Status)
	assert.Greater(t, len(resp2.Entries), 0, "second page should make progress")
}

// TestReadDirPlus_DirCountTruncation verifies the dircount (directory-info)
// budget is honored independently of maxcount.
func TestReadDirPlus_DirCountTruncation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	const numFiles = 40
	for i := 0; i < numFiles; i++ {
		fx.CreateFile(fmt.Sprintf("dircountdir/file%02d.txt", i), []byte("x"))
	}
	dirHandle := fx.MustGetHandle("dircountdir")

	// Large maxcount but tiny dircount: each entry's dir-info (~32 bytes) must
	// bound the result.
	req := &handlers.ReadDirPlusRequest{
		DirHandle:  dirHandle,
		Cookie:     0,
		CookieVerf: 0,
		DirCount:   128,
		MaxCount:   1 << 20,
	}
	resp, err := fx.Handler.ReadDirPlus(fx.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)
	assert.Greater(t, len(resp.Entries), 0, "should emit at least one entry")
	assert.Less(t, len(resp.Entries), numFiles, "dircount should bound the entry count")
	assert.False(t, resp.Eof, "eof must be false when truncated by dircount")
}
