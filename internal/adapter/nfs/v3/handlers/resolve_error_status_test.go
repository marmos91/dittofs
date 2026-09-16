package handlers_test

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// faultOnFetchStore fails every GetFile with a fixed error, so a handler's
// handle-resolution branch can be exercised with an error the store would not
// produce on its own.
type faultOnFetchStore struct {
	*metadatamemory.MemoryMetadataStore
	err error
}

func (s *faultOnFetchStore) GetFile(_ context.Context, _ metadata.FileHandle) (*metadata.File, error) {
	return nil, s.err
}

// TestHandleResolutionPreservesNonStaleStatus pins that only a typed
// not-found / stale-handle result is reported to the client as NFS3ERR_STALE.
//
// RFC 1813 defines NFS3ERR_STALE as "the file referred to by that file handle
// no longer exists or access to it has been revoked". A backend I/O or decode
// fault is neither: telling the client STALE makes it discard a handle that is
// still valid and revalidate the whole path, and it hides the BADHANDLE / IO
// status the wire owes. A malformed handle is NFS3ERR_BADHANDLE, not STALE.
//
// Each case runs the same injected store fault through every handler that
// resolves a caller-supplied handle before doing name work, because they must
// not disagree about the status purely by procedure.
func TestHandleResolutionPreservesNonStaleStatus(t *testing.T) {
	handlersUnderTest := []struct {
		name string
		call func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32
	}{
		{
			name: "LOOKUP directory handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.Lookup(fx.Context(), &handlers.LookupRequest{
					DirHandle: fx.RootHandle,
					Filename:  "somefile.txt",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "RMDIR parent handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.Rmdir(fx.Context(), &handlers.RmdirRequest{
					DirHandle: fx.RootHandle,
					Name:      "somedir",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "LINK source handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.Link(fx.Context(), &handlers.LinkRequest{
					FileHandle: fx.RootHandle,
					DirHandle:  fx.RootHandle,
					Name:       "newlink.txt",
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "COMMIT file handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.Commit(fx.Context(), &handlers.CommitRequest{Handle: fx.RootHandle})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "READDIRPLUS directory handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.ReadDirPlus(fx.Context(), &handlers.ReadDirPlusRequest{
					DirHandle: fx.RootHandle,
					DirCount:  4096,
					MaxCount:  8192,
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
		{
			name: "ACCESS file handle",
			call: func(t *testing.T, fx *handlertesting.HandlerTestFixture) uint32 {
				resp, err := fx.Handler.Access(fx.Context(), &handlers.AccessRequest{
					Handle: fx.RootHandle,
					Access: 0x1,
				})
				require.NoError(t, err)
				return resp.Status
			},
		},
	}

	cases := []struct {
		name     string
		injected error
		want     uint32
	}{
		{
			name:     "backend I/O fault is not a stale handle",
			injected: &metadata.StoreError{Code: metadata.ErrIOError, Message: "backend read failed"},
			want:     types.NFS3ErrIO,
		},
		{
			name:     "unclassified error is not a stale handle",
			injected: errors.New("transport reset"),
			want:     types.NFS3ErrIO,
		},
		{
			name:     "malformed handle is bad handle, not stale",
			injected: metadata.NewInvalidHandleError(),
			want:     types.NFS3ErrBadHandle,
		},
		{
			name:     "unresolvable handle is stale",
			injected: &metadata.StoreError{Code: metadata.ErrNotFound, Message: "file not found"},
			want:     types.NFS3ErrStale,
		},
		{
			name:     "handle naming a removed share is stale",
			injected: metadata.NewStaleHandleError(handlertesting.DefaultShareName),
			want:     types.NFS3ErrStale,
		},
	}

	for _, tc := range cases {
		for _, h := range handlersUnderTest {
			t.Run(tc.name+"/"+h.name, func(t *testing.T) {
				fx := handlertesting.NewHandlerFixtureWithStore(t, func(inner *metadatamemory.MemoryMetadataStore) metadata.Store {
					return &faultOnFetchStore{MemoryMetadataStore: inner, err: tc.injected}
				})

				assert.EqualValues(t, tc.want, h.call(t, fx),
					"a handle-resolution failure must keep the status its error class calls for")
			})
		}
	}
}
