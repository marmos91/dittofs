package handlers_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nilOnNthParentFetchStore wraps a memory store and returns (nil, nil) from
// GetFile for a specific handle once a configured number of GetFile calls for
// that handle have occurred. It reproduces the production condition the RMDIR
// nil-deref finding describes: the post-operation parent re-fetch returning nil
// after RemoveDirectory has run.
type nilOnNthParentFetchStore struct {
	*metadatamemory.MemoryMetadataStore

	targetHandle metadata.FileHandle
	failAfter    int64 // start returning nil once the call count exceeds this
	calls        atomic.Int64
	triggered    atomic.Bool // set true the first time the nil branch fires
}

func (s *nilOnNthParentFetchStore) GetFile(ctx context.Context, h metadata.FileHandle) (*metadata.File, error) {
	if bytes.Equal([]byte(h), []byte(s.targetHandle)) {
		if s.calls.Add(1) > s.failAfter {
			// Simulate a store that can no longer resolve the parent (e.g. it
			// was removed/raced, or the context is cancelled): the real store
			// returns (nil, err); a blank-discarded error then leaves a nil
			// *File. Return (nil, nil) to make the nil dereference unambiguous.
			s.triggered.Store(true)
			return nil, nil
		}
	}
	return s.MemoryMetadataStore.GetFile(ctx, h)
}

// TestRmdir_StoreErrorReFetchNil_NoPanic is a negative control for the RMDIR
// nil-dereference finding (rmdir.go:167). When RemoveDirectory fails and the
// post-op parent re-fetch returns a nil *File, the handler must NOT dereference
// it. Before the fix the handler did `&parentFile.FileAttr` on a nil pointer
// and panicked the server goroutine; after the fix it returns a clean status
// with no post-op WCC attributes.
func TestRmdir_StoreErrorReFetchNil_NoPanic(t *testing.T) {
	var wrapped *nilOnNthParentFetchStore

	fx := handlertesting.NewHandlerFixtureWithStore(t, func(inner *metadatamemory.MemoryMetadataStore) metadata.Store {
		wrapped = &nilOnNthParentFetchStore{
			MemoryMetadataStore: inner,
			// The parent handle is fetched several times before the handler's
			// post-failure re-fetch (handler pre-op fetch, the service's parent
			// lookup, and the delete-permission check). Only the final re-fetch
			// at rmdir.go:167 returns nil; the `triggered` assertion below
			// guards against this count drifting and making the test vacuous.
			failAfter: 3,
		}
		return wrapped
	})

	// A non-empty directory makes Service.RemoveDirectory fail with NotEmpty,
	// reaching the post-op re-fetch branch in the handler.
	fx.CreateFile("notempty/keep.txt", []byte("x"))
	wrapped.targetHandle = fx.RootHandle

	req := &handlers.RmdirRequest{
		DirHandle: fx.RootHandle,
		Name:      "notempty",
	}

	// Must not panic on the nil re-fetch.
	resp, err := fx.Handler.Rmdir(fx.ContextWithUID(0, 0), req)

	require.NoError(t, err)
	require.True(t, wrapped.triggered.Load(),
		"test did not exercise the nil post-op re-fetch path (call count drifted); adjust failAfter")
	assert.EqualValues(t, types.NFS3ErrNotEmpty, resp.Status,
		"RMDIR of a non-empty directory should return NFS3ErrNotEmpty")
	// The handler must survive the nil re-fetch (the regression was a panic).
	// Post-op WCC falls back to the pre-op parent attributes via
	// wccAfterOrFallback, so it is non-nil; the load-bearing assertion is
	// simply that we got here without a nil-pointer panic.
	assert.NotNil(t, resp.DirWccAfter, "post-op WCC should fall back to pre-op attrs, not panic on nil")
}
