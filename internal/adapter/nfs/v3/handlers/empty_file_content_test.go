package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCommit_EmptyFile succeeds without a content store: an empty file has
// nothing to make durable, though every regular file carries a PayloadID. A
// file with a size is the one that needs a content store.
func TestCommit_EmptyFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	resp, err := fx.Handler.Commit(fx.ContextWithUID(0, 0), &handlers.CommitRequest{Handle: fx.CreateFile("empty.txt", nil)})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status)

	resp, err = fx.Handler.Commit(fx.ContextWithUID(0, 0), &handlers.CommitRequest{Handle: fx.CreateFile("full.txt", []byte("data"))})
	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status)
}
