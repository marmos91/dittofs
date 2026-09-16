package handlers_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
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

// cancelDuringFetchStore cancels the request context from inside GetFile, once
// armed, and then fails the way a real store fails under cancellation. It
// reaches the branch of getFileOrError that a pre-call cancellation check
// cannot: the context going away while the store call is in flight.
type cancelDuringFetchStore struct {
	*metadatamemory.MemoryMetadataStore

	armed  atomic.Bool
	fired  atomic.Bool
	cancel context.CancelFunc
}

func (s *cancelDuringFetchStore) GetFile(ctx context.Context, h metadata.FileHandle) (*metadata.File, error) {
	if s.armed.Load() {
		s.fired.Store(true)
		s.cancel()
		return nil, context.Canceled
	}
	return s.MemoryMetadataStore.GetFile(ctx, h)
}

// TestLookup_CancelledDuringHandleResolutionReportsIO pins that LOOKUP reports
// a cancellation as NFS3ERR_IO and hands the RPC dispatcher a nil Go error.
//
// The dispatcher throws away a handler's response whenever the handler also
// returns an error, and answers with that procedure's fallback status instead.
// LOOKUP's fallback is NFS3ErrAccess, so propagating the cancellation error
// would report a timed-out lookup to the client as "permission denied". Every
// sibling handler that does propagate has NFS3ErrIO as its fallback, which is
// the status the cancellation carries anyway.
func TestLookup_CancelledDuringHandleResolutionReportsIO(t *testing.T) {
	reqCtx, cancel := context.WithCancel(context.Background())

	var wrapped *cancelDuringFetchStore
	fx := handlertesting.NewHandlerFixtureWithStore(t, func(inner *metadatamemory.MemoryMetadataStore) metadata.Store {
		wrapped = &cancelDuringFetchStore{MemoryMetadataStore: inner, cancel: cancel}
		return wrapped
	})

	hctx := fx.Context()
	hctx.Context = reqCtx
	wrapped.armed.Store(true)

	resp, err := fx.Handler.Lookup(hctx, &handlers.LookupRequest{
		DirHandle: fx.RootHandle,
		Filename:  "anything.txt",
	})

	require.True(t, wrapped.fired.Load(),
		"the store call never ran, so the cancellation branch was not exercised")
	require.NoError(t, err,
		"LOOKUP must not return a Go error: the dispatcher would discard this response and answer NFS3ErrAccess")
	assert.EqualValues(t, types.NFS3ErrIO, resp.Status,
		"a cancelled LOOKUP should be reported as NFS3ErrIO")
}
