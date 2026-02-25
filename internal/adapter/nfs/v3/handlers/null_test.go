package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNull_Success tests that the NULL procedure returns successfully.
// NULL is a no-op ping used by clients to verify server liveness.
func TestNull_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.NullRequest{}
	resp, err := fx.Handler.Null(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "NULL should return NFS3OK")
}
