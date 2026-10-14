package handlers_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreate_RFC1813 tests CREATE handler behaviors per RFC 1813 Section 3.3.8.
//
// CREATE creates a new regular file. It supports three modes:
// - UNCHECKED: Create, or keep an existing file (resized only when a size is given)
// - GUARDED: Fail if file exists
// - EXCLUSIVE: Use verifier for idempotent creation

// TestCreate_UncheckedNewFile tests creating a new file with UNCHECKED mode.
func TestCreate_UncheckedNewFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "newfile.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	// Use root context for write operations
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "CREATE should return NFS3OK")
	assert.NotNil(t, resp.FileHandle, "Should return file handle")
	assert.NotNil(t, resp.Attr, "Should return file attributes")
	assert.EqualValues(t, types.NF3REG, resp.Attr.Type, "Should be regular file")

	// Verify file exists
	assert.NotNil(t, fx.GetHandle("newfile.txt"), "File should exist after CREATE")
}

// uncheckedCreate issues an UNCHECKED CREATE of name with attr, as root.
func uncheckedCreate(t *testing.T, fx *handlertesting.HandlerTestFixture, name string, attr *metadata.SetAttrs) *handlers.CreateResponse {
	t.Helper()
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  name,
		Mode:      types.CreateUnchecked,
		Attr:      attr,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status, "UNCHECKED CREATE of an existing entry should succeed")
	return resp
}

// readAll reads the whole file through the READ handler.
func readAll(t *testing.T, fx *handlertesting.HandlerTestFixture, handle []byte) []byte {
	t.Helper()
	resp, err := fx.Handler.Read(fx.ContextWithUID(0, 0), &handlers.ReadRequest{Handle: handle, Count: 1 << 16})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)
	return resp.Data[:resp.Count]
}

// TestCreate_UncheckedExistingFile_NoSizeKeepsFile tests that an UNCHECKED
// CREATE naming an existing file, with no size in its attributes, leaves the
// file as it is. That is what a client sends for open(O_CREAT) without
// O_TRUNC, as for an append: the attributes it carries must not reset the
// file, and the size returned must be the real one, because an O_APPEND write
// is placed at it.
func TestCreate_UncheckedExistingFile_NoSizeKeepsFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	fx.CreateFile("existing.txt", []byte("ABCDE"))

	mode := uint32(0600)
	resp := uncheckedCreate(t, fx, "existing.txt", &metadata.SetAttrs{Mode: &mode})

	assert.EqualValues(t, 5, resp.Attr.Size, "the reply must carry the existing size")
	assert.EqualValues(t, 0644, resp.Attr.Mode&0o7777, "the mode of an existing file must not change")
	assert.Equal(t, []byte("ABCDE"), readAll(t, fx, resp.FileHandle), "the existing data must survive")

	// The client appends at the size the reply gave it.
	wresp, err := fx.Handler.Write(fx.ContextWithUID(0, 0), &handlers.WriteRequest{
		Handle: resp.FileHandle,
		Offset: resp.Attr.Size,
		Count:  5,
		Stable: 2, // FILE_SYNC
		Data:   []byte("abcde"),
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, wresp.Status)
	assert.Equal(t, []byte("ABCDEabcde"), readAll(t, fx, resp.FileHandle), "an append must land after the existing data")
}

// TestCreate_UncheckedExistingDirectory_NoSize tests that an UNCHECKED CREATE
// naming an existing directory, with no size, answers with the directory's
// own handle and attributes rather than an error, and leaves it in place.
func TestCreate_UncheckedExistingDirectory_NoSize(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	dir := fx.CreateDirectory("existing-dir")

	resp := uncheckedCreate(t, fx, "existing-dir", &metadata.SetAttrs{})

	assert.Equal(t, []byte(dir), []byte(resp.FileHandle), "the reply must carry the directory's own handle")
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type)
	assert.Equal(t, metadata.FileTypeDirectory, fx.GetFile("existing-dir").Type, "the directory must stay a directory")
}

// TestCreate_UncheckedExistingSymlink_NoSize tests the same for an existing
// symlink: its own handle and attributes, and its target unchanged.
func TestCreate_UncheckedExistingSymlink_NoSize(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	link := fx.CreateSymlink("existing-link", "some/target")

	resp := uncheckedCreate(t, fx, "existing-link", &metadata.SetAttrs{})

	assert.Equal(t, []byte(link), []byte(resp.FileHandle), "the reply must carry the symlink's own handle")
	assert.EqualValues(t, types.NF3LNK, resp.Attr.Type)
	assert.Equal(t, "some/target", fx.GetFile("existing-link").LinkTarget, "the symlink's target must not change")
}

// TestCreate_UncheckedExistingFile_ReadOnlyShare tests that an UNCHECKED
// CREATE naming an existing file on a read-only share fails with ROFS even
// without a size, so a client opening the file for writing learns it at open.
func TestCreate_UncheckedExistingFile_ReadOnlyShare(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	fx.CreateFile("existing.txt", []byte("ABCDE"))

	store, err := fx.MetadataService.GetStoreForShare(fx.ShareName)
	require.NoError(t, err)
	opts, err := store.GetShareOptions(context.Background(), fx.ShareName)
	require.NoError(t, err)
	readOnly := *opts
	readOnly.ReadOnly = true
	require.NoError(t, store.UpdateShareOptions(context.Background(), fx.ShareName, &readOnly))

	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "existing.txt",
		Mode:      types.CreateUnchecked,
		Attr:      &metadata.SetAttrs{},
	})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrRofs, resp.Status)
	assert.Equal(t, []byte("ABCDE"), fx.ReadContent("existing.txt"), "the existing data must survive")
}

// TestCreate_UncheckedExistingFile_NilAttrKeepsFile tests the same with no
// attributes at all.
func TestCreate_UncheckedExistingFile_NilAttrKeepsFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	fx.CreateFile("existing.txt", []byte("ABCDE"))

	resp := uncheckedCreate(t, fx, "existing.txt", nil)

	assert.EqualValues(t, 5, resp.Attr.Size)
	assert.Equal(t, []byte("ABCDE"), readAll(t, fx, resp.FileHandle))
}

// TestCreate_UncheckedExistingFile_ZeroSizeTruncates tests that a size of
// zero, which a client sends for O_TRUNC, empties the existing file.
func TestCreate_UncheckedExistingFile_ZeroSizeTruncates(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	fx.CreateFile("existing.txt", []byte("original content"))

	size := uint64(0)
	resp := uncheckedCreate(t, fx, "existing.txt", &metadata.SetAttrs{Size: &size})

	assert.EqualValues(t, 0, resp.Attr.Size, "a size of zero must truncate")
	assert.Empty(t, readAll(t, fx, resp.FileHandle))
}

// TestCreate_UncheckedExistingFile_SizeResizes tests that a non-zero size
// shortens or extends the existing file to it, and that the request's other
// attributes are still ignored.
func TestCreate_UncheckedExistingFile_SizeResizes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	fx.CreateFile("existing.txt", []byte("ABCDE"))

	short := uint64(3)
	mode := uint32(0600)
	resp := uncheckedCreate(t, fx, "existing.txt", &metadata.SetAttrs{Size: &short, Mode: &mode})
	assert.EqualValues(t, 3, resp.Attr.Size)
	assert.EqualValues(t, 0644, resp.Attr.Mode&0o7777, "only the size is applied to an existing file")
	assert.Equal(t, []byte("ABC"), readAll(t, fx, resp.FileHandle))

	long := uint64(6)
	resp = uncheckedCreate(t, fx, "existing.txt", &metadata.SetAttrs{Size: &long})
	assert.EqualValues(t, 6, resp.Attr.Size)
	assert.Equal(t, []byte("ABC\x00\x00\x00"), readAll(t, fx, resp.FileHandle), "an extension reads back as zeros")
}

// TestCreate_GuardedNewFile tests creating a new file with GUARDED mode.
func TestCreate_GuardedNewFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "guarded.txt",
		Mode:      types.CreateGuarded,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "GUARDED CREATE of new file should succeed")
	assert.NotNil(t, resp.FileHandle)
}

// TestCreate_GuardedExistingFile tests GUARDED mode fails if file exists.
func TestCreate_GuardedExistingFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create existing file
	fx.CreateFile("existing.txt", []byte("content"))

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "existing.txt",
		Mode:      types.CreateGuarded,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp.Status, "GUARDED should fail if file exists")
}

// TestCreate_ExclusiveNewFile tests creating a new file with EXCLUSIVE mode.
func TestCreate_ExclusiveNewFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "exclusive.txt",
		Mode:      types.CreateExclusive,
		Verf:      12345, // Non-zero verifier
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "EXCLUSIVE CREATE of new file should succeed")
	assert.NotNil(t, resp.FileHandle)
}

// TestCreate_ExclusiveIdempotent tests EXCLUSIVE mode behavior.
// Note: Idempotency in this implementation requires verifier stored in file metadata.
// If file exists with same verifier, it returns success with existing handle.
func TestCreate_ExclusiveIdempotent(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	verifier := uint64(12345)

	// First create
	req1 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "exclusive.txt",
		Mode:      types.CreateExclusive,
		Verf:      verifier,
	}
	resp1, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req1)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp1.Status)

	// Second create with same verifier - idempotent retry
	// RFC 1813 Section 3.3.8: If the file exists and verifiers match, return success
	req2 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "exclusive.txt",
		Mode:      types.CreateExclusive,
		Verf:      verifier,
	}
	resp2, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req2)
	require.NoError(t, err)
	// Matching verifier indicates idempotent retry - return success with existing file
	assert.EqualValues(t, types.NFS3OK, resp2.Status)
	assert.NotNil(t, resp2.FileHandle, "Should return existing file handle")
	assert.NotNil(t, resp2.Attr, "Should return file attributes")
}

// TestCreate_ExclusiveDifferentVerifier tests EXCLUSIVE fails with different verifier.
func TestCreate_ExclusiveDifferentVerifier(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// First create
	req1 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "exclusive.txt",
		Mode:      types.CreateExclusive,
		Verf:      12345,
	}
	resp1, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req1)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp1.Status)

	// Second create with different verifier - should fail
	req2 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "exclusive.txt",
		Mode:      types.CreateExclusive,
		Verf:      99999, // Different verifier
	}
	resp2, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req2)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp2.Status,
		"EXCLUSIVE with different verifier should fail with EXIST")
}

// TestCreate_NotADirectory tests CREATE returns NFS3ErrNotDir when parent is not a directory.
func TestCreate_NotADirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create a file to use as "parent"
	fileHandle := fx.CreateFile("notadir.txt", []byte("content"))

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fileHandle,
		Filename:  "child.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNotDir, resp.Status, "CREATE in file should return NFS3ErrNotDir")
}

// TestCreate_EmptyFilename tests CREATE returns NFS3ErrInval for empty filename.
func TestCreate_EmptyFilename(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty filename should return NFS3ErrInval")
}

// TestCreate_FilenameTooLong tests CREATE returns NFS3ErrNameTooLong for long filenames.
func TestCreate_FilenameTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	longName := make([]byte, 256)
	for i := range longName {
		longName[i] = 'a'
	}

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  string(longName),
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrNameTooLong, resp.Status, "Long filename should return NFS3ErrNameTooLong")
}

// TestCreate_FilenameWithNullByte tests CREATE rejects filenames with null bytes.
func TestCreate_FilenameWithNullByte(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file\x00name.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Filename with null byte should return NFS3ErrInval")
}

// TestCreate_EmptyHandle tests CREATE returns error for empty handle.
func TestCreate_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: []byte{},
		Filename:  "file.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrInval for empty handle
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Empty handle should return error")
}

// TestCreate_HandleTooShort tests CREATE returns error for short handles.
func TestCreate_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes
		Filename:  "file.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrStale for invalid/unrecognized handles
	assert.EqualValues(t, types.NFS3ErrStale, resp.Status, "Invalid handle should return error")
}

// TestCreate_HandleTooLong tests CREATE returns error for long handles.
func TestCreate_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: make([]byte, 65), // 65 bytes, max is 64
		Filename:  "file.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	// Implementation returns NFS3ErrInval for handles exceeding max length
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Handle too long should return error")
}

// TestCreate_ContextCancellation tests CREATE respects context cancellation.
func TestCreate_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithCancellation(), req)

	require.Error(t, err, "Should return error for cancelled context")
	if resp != nil {
		assert.EqualValues(t, types.NFS3ErrIO, resp.Status)
	}
}

// TestCreate_ReturnsWCC tests that CREATE returns WCC data (before/after attributes).
func TestCreate_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "wcctest.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.DirBefore, "Should return DirBefore (pre-op attrs)")
	assert.NotNil(t, resp.DirAfter, "Should return DirAfter (post-op attrs)")
}

// TestCreate_InNestedDirectory tests CREATE in a nested directory.
func TestCreate_InNestedDirectory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Setup: Create nested directory
	dirHandle := fx.CreateDirectory("a/b/c")

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: dirHandle,
		Filename:  "nested.txt",
		Mode:      types.CreateUnchecked,
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// Verify file exists at expected path
	assert.NotNil(t, fx.GetHandle("a/b/c/nested.txt"))
}

// TestCreate_InvalidMode tests CREATE returns error for invalid mode.
func TestCreate_InvalidMode(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	mode := uint32(0644)
	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "file.txt",
		Mode:      99, // Invalid mode
		Attr: &metadata.SetAttrs{
			Mode: &mode,
		},
	}
	resp, err := fx.Handler.Create(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status, "Invalid mode should return NFS3ErrInval")
}

// TestCreate_ExclusiveZeroVerifierRetry verifies that a zero verifier is not
// falsely classified as a conflict when the file was created with Verf=0.
func TestCreate_ExclusiveZeroVerifierRetry(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "zero-verf.txt",
		Mode:      types.CreateExclusive,
		Verf:      0, // zero verifier
	}
	// First create must succeed
	resp1, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp1.Status, "initial zero-verifier EXCLUSIVE create must succeed")

	// Retry with same (zero) verifier: must be recognised as a retry (NFS3OK).
	resp2, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp2.Status,
		"zero-verifier retry must return NFS3OK, not NFS3ErrExist")
	assert.NotNil(t, resp2.FileHandle, "retry must return the existing file handle")
}

// TestCreate_ExclusiveZeroVerifierNewFile verifies a first create with Verf=0 succeeds.
func TestCreate_ExclusiveZeroVerifierNewFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "zero-new.txt",
		Mode:      types.CreateExclusive,
		Verf:      0,
	}
	resp, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.NotNil(t, resp.FileHandle)
}

// TestCreate_ExclusiveZeroVsNonZeroVerifierConflict tests that Verf=0 sent
// against a file created with Verf!=0 is still a conflict.
func TestCreate_ExclusiveZeroVsNonZeroVerifierConflict(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req1 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "nonzero.txt",
		Mode:      types.CreateExclusive,
		Verf:      42,
	}
	resp1, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req1)
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp1.Status)

	req2 := &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "nonzero.txt",
		Mode:      types.CreateExclusive,
		Verf:      0, // different verifier
	}
	resp2, err := fx.Handler.Create(fx.ContextWithUID(0, 0), req2)
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp2.Status,
		"zero verifier vs stored non-zero verifier must conflict")
}
