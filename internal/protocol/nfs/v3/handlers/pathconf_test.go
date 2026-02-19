package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nfs/types"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/protocol/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPathConf_Success tests that PATHCONF returns valid path configuration.
func TestPathConf_Success(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	req := &handlers.PathConfRequest{
		Handle: fx.RootHandle,
	}
	resp, err := fx.Handler.PathConf(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "PATHCONF should succeed")

	// Linkmax should be positive (max hard links per file)
	assert.True(t, resp.Linkmax > 0, "Linkmax should be positive")

	// NameMax should be positive (max filename length)
	assert.True(t, resp.NameMax > 0, "NameMax should be positive")
}
