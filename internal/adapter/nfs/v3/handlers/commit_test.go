package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommit_Success tests that COMMIT succeeds on a valid file handle.
func TestCommit_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("commitfile.txt", []byte("data to commit"))

	req := &handlers.CommitRequest{
		Handle: fileHandle,
		Offset: 0,
		Count:  0, // 0 means commit entire file
	}
	resp, err := fx.Handler.Commit(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "COMMIT should succeed")
	assert.NotNil(t, resp.AttrAfter, "Should return post-operation attributes after commit")
}

// TestCommit_InvalidHandle tests that COMMIT returns an error for invalid handle.
func TestCommit_InvalidHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	invalidHandle := make([]byte, 16)
	for i := range invalidHandle {
		invalidHandle[i] = byte(i)
	}

	req := &handlers.CommitRequest{
		Handle: invalidHandle,
		Offset: 0,
		Count:  0,
	}
	resp, err := fx.Handler.Commit(fx.Context(), req)

	require.NoError(t, err)
	assert.NotEqualValues(t, types.NFS3OK, resp.Status,
		"Invalid handle should not return NFS3OK")
}

// TestCommit_PermissionDenied verifies COMMIT is gated on write permission:
// a non-root caller cannot force a flush of a mode-000 root-owned file.
// Without the gate any client holding a traversable handle could drive
// stable-storage flushes and uploads on files it may not modify.
func TestCommit_PermissionDenied(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	rootCtx := fx.ContextWithUID(0, 0)

	createResp, err := fx.Handler.Create(rootCtx, &handlers.CreateRequest{
		DirHandle: fx.RootHandle,
		Filename:  "rootonly.txt",
		Mode:      types.CreateUnchecked,
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, createResp.Status)

	mode := uint32(0000)
	setattrResp, err := fx.Handler.SetAttr(rootCtx, &handlers.SetAttrRequest{
		Handle:  createResp.FileHandle,
		NewAttr: metadata.SetAttrs{Mode: &mode},
	})
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, setattrResp.Status)

	resp, err := fx.Handler.Commit(fx.Context(), &handlers.CommitRequest{
		Handle: createResp.FileHandle,
	})

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrAccess, resp.Status,
		"COMMIT on a mode-000 root-owned file by non-root must return NFS3ErrAccess")
}

// TestCommit_PermissionGranted is the companion: the writer of a file it owns
// still commits successfully, so the gate costs the normal write-then-commit
// flow nothing.
func TestCommit_PermissionGranted(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// CreateFile makes the file mode 0644 owned by DefaultUID (1000).
	fileHandle := fx.CreateFile("mine.txt", []byte("committed content"))

	resp, err := fx.Handler.Commit(fx.Context(), &handlers.CommitRequest{
		Handle: fileHandle,
	})

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)
}
