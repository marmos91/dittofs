package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFsInfo_Success tests that FSINFO returns valid filesystem information.
func TestFsInfo_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.FsInfoRequest{
		Handle: fx.RootHandle,
	}
	resp, err := fx.Handler.FsInfo(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "FSINFO should succeed")

	// Verify key filesystem capabilities are returned
	assert.True(t, resp.Rtmax > 0, "Maximum read size (rtmax) should be positive")
	assert.True(t, resp.Wtmax > 0, "Maximum write size (wtmax) should be positive")
	assert.True(t, resp.Dtpref > 0, "Preferred directory transfer size (dtpref) should be positive")
}
