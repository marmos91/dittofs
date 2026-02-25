package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
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
