package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
	"github.com/stretchr/testify/require"
)

// newApplierFixture builds a Service over an in-memory Badger store.
func newApplierFixture(t *testing.T) (*metadata.Service, metadata.Store, metadata.FileHandle, string) {
	t.Helper()
	ctx := context.Background()
	store := badgertest.NewInMemory(t)

	const share = "/applier"
	root, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0777})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	return svc, store, rootHandle, share
}

// TestDeferredFlushAppliesWrite covers the default write path: deferred
// commits are on by default, so a WRITE buffers into the pending-write tracker
// and only the flush touches the store. It asserts the flush persists the size
// grown, times stamped, and no shrink when a later write lands at a lower offset.
func TestDeferredFlushAppliesWrite(t *testing.T) {
	svc, store, rootHandle, _ := newApplierFixture(t)
	ctx := &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}

	_, _, err := svc.CreateFile(ctx, rootHandle, "w.bin", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := store.GetChild(ctx.Context, rootHandle, "w.bin")
	require.NoError(t, err)

	write := func(size uint64) {
		intent, err := svc.PrepareWrite(ctx, handle, size)
		require.NoError(t, err)
		_, err = svc.CommitWrite(ctx, intent)
		require.NoError(t, err)
	}

	before := time.Now().Add(-time.Second)
	write(4096)
	flushed, err := svc.FlushPendingWriteForFile(ctx, handle, true)
	require.NoError(t, err)
	require.True(t, flushed, "expected a pending write to flush")

	f, err := store.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(4096), f.Size, "flush must persist the grown size")
	require.False(t, f.Mtime.Before(before), "flush must stamp mtime, got %v", f.Mtime)
	require.False(t, f.Ctime.Before(before), "flush must stamp ctime, got %v", f.Ctime)

	// A later, smaller write must not shrink the file.
	write(1024)
	if _, err := svc.FlushPendingWriteForFile(ctx, handle, true); err != nil {
		require.NoError(t, err)
	}
	f, err = store.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(4096), f.Size, "an out-of-order smaller write must not shrink the file")
}
