package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFsStat_Success tests that FSSTAT returns valid filesystem statistics.
func TestFsStat_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.FsStatRequest{
		Handle: fx.RootHandle,
	}
	resp, err := fx.Handler.FsStat(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "FSSTAT should succeed")

	// Total bytes should be reported
	assert.True(t, resp.Tbytes > 0, "Total bytes should be positive")
	// Free bytes should be less than or equal to total
	assert.True(t, resp.Fbytes <= resp.Tbytes, "Free bytes should not exceed total")
	// Available bytes should be less than or equal to free
	assert.True(t, resp.Abytes <= resp.Fbytes, "Available bytes should not exceed free")
}
