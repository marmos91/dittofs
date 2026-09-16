package handlers_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandleThatDoesNotResolveIsStale pins one wire status for one failure
// condition: a well-formed file handle that the metadata store cannot resolve.
//
// RFC 1813 defines NFS3ERR_STALE as "invalid file handle ... the file referred
// to by that file handle no longer exists or access to it has been revoked",
// while NFS3ERR_NOENT means the *name* looked up in a directory does not exist.
// A client that receives NOENT for a handle it still holds caches a negative
// name entry instead of discarding the handle, which is the wrong recovery.
//
// The handlers below each resolve a caller-supplied handle before doing any
// name work, so every one of them is reporting the same condition; they must
// not disagree about which status describes it purely because of which
// procedure the client happened to call.
func TestHandleThatDoesNotResolveIsStale(t *testing.T) {
	// A syntactically valid handle for the fixture's own share carrying a file
	// ID that was never created: it passes handle validation and share routing,
	// and fails only at the store lookup.
	staleHandle := func(t *testing.T) []byte {
		t.Helper()
		h, err := metadata.EncodeShareHandle(handlertesting.DefaultShareName, uuid.New())
		require.NoError(t, err)
		return h
	}

	tests := []struct {
		name string
		call func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32
	}{
		{
			name: "RMDIR parent handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				resp, err := fx.Handler.Rmdir(fx.Context(), &handlers.RmdirRequest{
					DirHandle: stale,
					Name:      "somedir",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "LOOKUP directory handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				resp, err := fx.Handler.Lookup(fx.Context(), &handlers.LookupRequest{
					DirHandle: stale,
					Filename:  "somefile.txt",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "LINK source file handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				resp, err := fx.Handler.Link(fx.Context(), &handlers.LinkRequest{
					FileHandle: stale,
					DirHandle:  fx.RootHandle,
					Name:       "newlink.txt",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "LINK target directory handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				source := fx.CreateFile("linksource.txt", []byte("content"))
				resp, err := fx.Handler.Link(fx.Context(), &handlers.LinkRequest{
					FileHandle: source,
					DirHandle:  stale,
					Name:       "newlink.txt",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "COMMIT file handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				resp, err := fx.Handler.Commit(fx.Context(), &handlers.CommitRequest{
					Handle: stale,
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "READDIRPLUS directory handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture, stale []byte) uint32 {
				resp, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
					DirHandle: stale,
					DirCount:  4096,
					MaxCount:  8192,
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := handlertesting.NewHandlerFixture(t)
			assert.EqualValues(t, types.NFS3ErrStale, tt.call(t, fx, staleHandle(t)),
				"an unresolvable file handle must be reported as NFS3ErrStale, not as a missing name")
		})
	}
}
