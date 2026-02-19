package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSymlink_Success tests creating a symbolic link.
func TestSymlink_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.SymlinkRequest{
		DirHandle: fx.RootHandle,
		Name:      "mylink",
		Target:    "/some/target/path",
	}
	resp, err := fx.Handler.Symlink(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "SYMLINK should succeed")
	assert.NotEmpty(t, resp.FileHandle, "Should return handle for new symlink")

	// Verify symlink exists via GETATTR
	getResp, err := fx.Handler.GetAttr(fx.Context(), &handlers.GetAttrRequest{Handle: resp.FileHandle})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, getResp.Status)
	assert.EqualValues(t, types.NF3LNK, getResp.Attr.Type, "Should be a symlink")
}

// TestSymlink_DuplicateName tests that creating a symlink with an existing name fails.
func TestSymlink_DuplicateName(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	// Create first symlink
	req := &handlers.SymlinkRequest{
		DirHandle: fx.RootHandle,
		Name:      "duplink",
		Target:    "/target1",
	}
	resp, err := fx.Handler.Symlink(fx.Context(), req)
	require.NoError(t, err)
	require.EqualValues(t, types.NFS3OK, resp.Status)

	// Try to create another symlink with the same name
	req2 := &handlers.SymlinkRequest{
		DirHandle: fx.RootHandle,
		Name:      "duplink",
		Target:    "/target2",
	}
	resp2, err := fx.Handler.Symlink(fx.Context(), req2)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrExist, resp2.Status,
		"Creating symlink with duplicate name should return NFS3ErrExist")
}
