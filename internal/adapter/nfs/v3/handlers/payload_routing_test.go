package handlers_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers"
	handlertesting "github.com/marmos91/dittofs/internal/adapter/nfs/v3/handlers/testing"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/stretchr/testify/require"
)

type payloadRoutingStore struct {
	*badger.BadgerMetadataStore
	reads atomic.Int64
}

func (s *payloadRoutingStore) GetFileForRead(ctx context.Context, handle metadata.FileHandle) (*metadata.File, error) {
	s.reads.Add(1)
	return s.BadgerMetadataStore.GetFileForRead(ctx, handle)
}

func TestV3WarmWriteReusesPayloadIdentity(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	handle := fx.CreateFile("routing.bin", []byte("original"))
	fx.MetadataService.SetDeferredCommit(true)
	fx.MetadataService.InvalidateWriteCache(handle)
	store := &payloadRoutingStore{BadgerMetadataStore: fx.MetaStore}
	require.NoError(t, fx.MetadataService.RegisterStoreForShare(fx.ShareName, store))
	write := func() *handlers.WriteResponse {
		resp, err := fx.Handler.Write(fx.ContextWithUID(1000, 1000), &handlers.WriteRequest{Handle: handle, Count: 1, Data: []byte("x")})
		require.NoError(t, err)
		return resp
	}
	require.Equal(t, uint32(types.NFS3OK), write().Status)
	require.Equal(t, int64(1), store.reads.Load(), "a cold write must resolve its payload identity")
	for range 4 {
		require.Equal(t, uint32(types.NFS3OK), write().Status)
	}
	require.Equal(t, int64(1), store.reads.Load(), "warm writes must not re-read the inode just to route payload admission")

	// A routing cache hit must still check current permissions inside admission.
	file, err := fx.MetaStore.GetFile(t.Context(), handle)
	require.NoError(t, err)
	file.Mode = 0o444
	require.NoError(t, fx.MetaStore.UpdateAttrs(t.Context(), file))
	require.Equal(t, uint32(types.NFS3ErrAccess), write().Status)
}

type setattrDeadlineStore struct {
	metadata.Store
	deadline chan time.Time
}

func (s *setattrDeadlineStore) WithTransaction(ctx context.Context, fn func(metadata.Transaction) error) error {
	d, _ := ctx.Deadline()
	select {
	case s.deadline <- d:
	default:
	}
	return s.Store.WithTransaction(ctx, fn)
}

func TestV3SetAttrPayloadScopeHasDeadline(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	handle := fx.CreateFile("deadline.bin", []byte("original"))
	store := &setattrDeadlineStore{Store: fx.MetaStore, deadline: make(chan time.Time, 1)}
	require.NoError(t, fx.MetadataService.RegisterStoreForShare(fx.ShareName, store))
	size := uint64(4)
	start := time.Now()
	resp, err := fx.Handler.SetAttr(fx.ContextWithUID(0, 0), &handlers.SetAttrRequest{Handle: handle, NewAttr: metadata.SetAttrs{Size: &size}})
	require.NoError(t, err)
	require.Equal(t, uint32(types.NFS3OK), resp.Status)
	select {
	case deadline := <-store.deadline:
		require.False(t, deadline.IsZero(), "size mutation must carry the admission deadline")
		require.WithinDuration(t, start.Add(30*time.Second), deadline, time.Second)
	default:
		t.Fatal("SETATTR never reached its metadata transaction")
	}
}

func TestV3SetAttrAdmissionDeadlineMapsToIO(t *testing.T) {
	fx := handlertesting.NewHandlerFixture(t)
	handle := fx.CreateFile("blocked.bin", []byte("original"))
	file, err := fx.MetadataService.GetFile(t.Context(), handle)
	require.NoError(t, err)
	require.NoError(t, fx.BlockStore.WithPayloadScope(t.Context(), []string{string(file.PayloadID)}, true, func(context.Context) error {
		ctx := fx.ContextWithUID(0, 0)
		var cancel context.CancelFunc
		ctx.Context, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		size := uint64(4)
		resp, err := fx.Handler.SetAttr(ctx, &handlers.SetAttrRequest{Handle: handle, NewAttr: metadata.SetAttrs{Size: &size}})
		require.NoError(t, err)
		require.Equal(t, uint32(types.NFS3ErrIO), resp.Status)
		return nil
	}))
	after, err := fx.MetadataService.GetFile(t.Context(), handle)
	require.NoError(t, err)
	require.Equal(t, uint64(len("original")), after.Size)
}
