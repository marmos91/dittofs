package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadLink_Success tests reading the target of a symbolic link.
func TestReadLink_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	target := "/some/target/path"
	symlinkHandle := fx.CreateSymlink("mylink", target)

	req := &handlers.ReadLinkRequest{
		Handle: symlinkHandle,
	}
	resp, err := fx.Handler.ReadLink(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "READLINK should succeed")
	assert.Equal(t, target, resp.Target, "Should return the symlink target path")
}

// TestReadLink_NotSymlink tests that reading a non-symlink returns an error.
func TestReadLink_NotSymlink(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("regular.txt", []byte("content"))

	req := &handlers.ReadLinkRequest{
		Handle: fileHandle,
	}
	resp, err := fx.Handler.ReadLink(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrInval, resp.Status,
		"READLINK on non-symlink should return NFS3ErrInval")
}
