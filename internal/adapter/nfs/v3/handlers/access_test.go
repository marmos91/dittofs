package handlers_test

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAccess_RootFile tests ACCESS check on a regular file.
func TestAccess_RootFile(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	fileHandle := fx.CreateFile("accessfile.txt", []byte("content"))

	// Request all access bits
	req := &handlers.AccessRequest{
		Handle: fileHandle,
		Access: types.AccessRead | types.AccessModify | types.AccessExecute,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "ACCESS should succeed")
	// At minimum, read access should be granted for a file owned by the test user
	assert.True(t, resp.Access&types.AccessRead != 0, "Read access should be granted")
}

// TestAccess_Directory tests ACCESS check on a directory.
func TestAccess_Directory(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)

	dirHandle := fx.CreateDirectory("accessdir")

	req := &handlers.AccessRequest{
		Handle: dirHandle,
		Access: types.AccessRead | types.AccessLookup | types.AccessModify,
	}
	resp, err := fx.Handler.Access(fx.Context(), req)

	require.NoError(t, err)
	assert.EqualValues(t, types.NFS3OK, resp.Status, "ACCESS should succeed for directory")
	assert.NotNil(t, resp.Attr, "Should return attributes")
	assert.EqualValues(t, types.NF3DIR, resp.Attr.Type, "Should be a directory")
}
