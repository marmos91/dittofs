package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMknod_FIFO tests creating a named pipe (FIFO) via MKNOD.
func TestMknod_FIFO(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.MknodRequest{
		DirHandle: fx.RootHandle,
		Name:      "testfifo",
		Type:      types.NF3FIFO,
	}
	resp, err := fx.Handler.Mknod(fx.Context(), req)

	require.NoError(t, err)
	// MKNOD for FIFO may succeed or return NFS3ErrNotSupp depending on store
	assert.True(t,
		resp.Status == types.NFS3OK || resp.Status == types.NFS3ErrNotSupp,
		"MKNOD FIFO should return NFS3OK or NFS3ErrNotSupp, got %d", resp.Status)
}

// TestMknod_InvalidHandle tests MKNOD with an invalid parent directory handle.
func TestMknod_InvalidHandle(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	invalidHandle := make([]byte, 16)
	for i := range invalidHandle {
		invalidHandle[i] = byte(i)
	}

	req := &handlers.MknodRequest{
		DirHandle: invalidHandle,
		Name:      "testnode",
		Type:      types.NF3FIFO,
	}
	resp, err := fx.Handler.Mknod(fx.Context(), req)

	require.NoError(t, err)
	assert.NotEqualValues(t, types.NFS3OK, resp.Status,
		"Invalid handle should not return NFS3OK")
}
