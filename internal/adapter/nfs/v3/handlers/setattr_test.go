package handlers_test

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetAttr_RFC1813 tests SETATTR handler behaviors per RFC 1813 Section 3.3.2.
//
// SETATTR is used to modify file attributes including:
// - Mode (permissions)
// - UID/GID (ownership)
// - Size (truncate)
// - Access and modification times
// - Guard (conditional update based on ctime)

// TestSetAttr_SetMode tests setting file mode (permissions).
func TestSetAttr_SetMode(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("modefile.txt", []byte("content"))

	newMode := uint32(0755)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "SETATTR should return NFS3OK")
	assert.NotNil(t, resp.AttrAfter)
	assert.EqualValues(t, uint32(0755), resp.AttrAfter.Mode&0777, "Mode should be updated")
}

// TestSetAttr_SetUID tests setting file UID.
func TestSetAttr_SetUID(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("uidfile.txt", []byte("content"))

	newUID := uint32(1001)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			UID: &newUID,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(1001), resp.AttrAfter.UID)
}

// TestSetAttr_SetGID tests setting file GID.
func TestSetAttr_SetGID(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("gidfile.txt", []byte("content"))

	newGID := uint32(2002)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			GID: &newGID,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(2002), resp.AttrAfter.GID)
}

// TestSetAttr_Truncate tests truncating a file via SETATTR.
func TestSetAttr_Truncate(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create file with content
	fileHandle := fx.CreateFile("truncate.txt", []byte("hello world"))

	// Truncate to 5 bytes
	newSize := uint64(5)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Size: &newSize,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint64(5), resp.AttrAfter.Size, "Reported size should be truncated")

	// Verify GETATTR also reports truncated size
	attrResp, err := fx.Handler.GetAttr(fx.Context(), &handlers.GetAttrRequest{
		Handle: fileHandle,
	})
	require.NoError(t, err)
	assert.EqualValues(t, uint64(5), attrResp.Attr.Size, "GETATTR should also report truncated size")
}

// TestSetAttr_TruncateToZero tests truncating a file to zero bytes.
func TestSetAttr_TruncateToZero(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("trunczero.txt", []byte("hello world"))

	newSize := uint64(0)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Size: &newSize,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint64(0), resp.AttrAfter.Size)
}

// TestSetAttr_ExtendFile tests extending a file via SETATTR.
func TestSetAttr_ExtendFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("extend.txt", []byte("hello"))

	// Extend to 100 bytes
	newSize := uint64(100)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Size: &newSize,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint64(100), resp.AttrAfter.Size)
}

// TestSetAttr_SetMtime tests setting modification time.
func TestSetAttr_SetMtime(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("mtime.txt", []byte("content"))

	newMtime := time.Unix(1704067200, 0) // 2024-01-01 00:00:00 UTC
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mtime: &newMtime,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 1704067200, resp.AttrAfter.Mtime.Seconds)
}

// TestSetAttr_SetAtime tests setting access time.
func TestSetAttr_SetAtime(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("atime.txt", []byte("content"))

	newAtime := time.Unix(1704067200, 0) // 2024-01-01 00:00:00 UTC
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Atime: &newAtime,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, 1704067200, resp.AttrAfter.Atime.Seconds)
}

// TestSetAttr_MultipleAttributes tests setting multiple attributes at once.
func TestSetAttr_MultipleAttributes(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("multi.txt", []byte("content"))

	newMode := uint32(0700)
	newUID := uint32(1000)
	newGID := uint32(1000)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
			UID:  &newUID,
			GID:  &newGID,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
	assert.EqualValues(t, uint32(0700), resp.AttrAfter.Mode&0777)
	assert.EqualValues(t, uint32(1000), resp.AttrAfter.UID)
	assert.EqualValues(t, uint32(1000), resp.AttrAfter.GID)
}

// TestSetAttr_Directory tests SETATTR on a directory.
func TestSetAttr_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("testdir")

	newMode := uint32(0755)
	req := &handlers.SetAttrRequest{
		Handle: dirHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
}

// TestSetAttr_EmptyHandle tests SETATTR with empty handle.
func TestSetAttr_EmptyHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	newMode := uint32(0644)
	req := &handlers.SetAttrRequest{
		Handle: []byte{},
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Empty handle should return NFS3ErrBadHandle")
}

// TestSetAttr_HandleTooShort tests SETATTR with handle shorter than minimum.
func TestSetAttr_HandleTooShort(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	newMode := uint32(0644)
	req := &handlers.SetAttrRequest{
		Handle: []byte{1, 2, 3, 4, 5, 6, 7}, // 7 bytes, min is 8
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too short should return NFS3ErrBadHandle")
}

// TestSetAttr_HandleTooLong tests SETATTR with handle longer than maximum.
func TestSetAttr_HandleTooLong(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	newMode := uint32(0644)
	req := &handlers.SetAttrRequest{
		Handle: make([]byte, 65), // 65 bytes, max is 64
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrBadHandle, resp.Status, "Handle too long should return NFS3ErrBadHandle")
}

// TestSetAttr_ContextCancellation tests SETATTR respects context cancellation.
func TestSetAttr_ContextCancellation(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("cancel.txt", []byte("content"))

	newMode := uint32(0644)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithCancellation(), req)

	// Context cancellation is indicated via error or status
	if err != nil {
		return // Context cancellation detected via error
	}
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status, "Cancelled context should return NFS3ErrIO")
}

// TestSetAttr_ReturnsWCC tests that SETATTR returns WCC data.
func TestSetAttr_ReturnsWCC(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("wcc.txt", []byte("content"))

	newMode := uint32(0600)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	// WCC data should be present
	assert.NotNil(t, resp.AttrBefore, "Should return AttrBefore (pre-op attrs)")
	assert.NotNil(t, resp.AttrAfter, "Should return AttrAfter (post-op attrs)")
}

// TestSetAttr_NoChanges tests SETATTR with no attribute changes.
func TestSetAttr_NoChanges(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("nochange.txt", []byte("content"))

	// Empty SetAttrs - no changes
	req := &handlers.SetAttrRequest{
		Handle:  fileHandle,
		NewAttr: metadata.SetAttrs{},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "SETATTR with no changes should succeed")
}

// TestSetAttr_NestedFile tests SETATTR on a file in a nested directory.
func TestSetAttr_NestedFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("a/b/c/nested.txt", []byte("content"))

	newMode := uint32(0600)
	req := &handlers.SetAttrRequest{
		Handle: fileHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
}

// TestSetAttr_Symlink tests SETATTR on a symlink.
func TestSetAttr_Symlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	symlinkHandle := fx.CreateSymlink("link", "/target")

	newMode := uint32(0777)
	req := &handlers.SetAttrRequest{
		Handle: symlinkHandle,
		NewAttr: metadata.SetAttrs{
			Mode: &newMode,
		},
	}
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), req)

	// SETATTR on symlink may succeed or fail depending on implementation
	require.NoError(t, err)
	// Just verify we get a valid response with some status
	assert.NotNil(t, resp)
}
