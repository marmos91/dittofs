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

// duringFlush makes fn run inside every pending-write flush, at the
// durable-extent lookup, which a flush makes before it commits.
func duringFlush(svc *metadata.Service, fn func()) {
	svc.SetDurableExtentResolver(func(string, metadata.PayloadID) (int64, bool) {
		fn()
		return 0, false
	})
}

func rootAuthContext() *metadata.AuthContext {
	return &metadata.AuthContext{
		Context:    context.Background(),
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
}

// TestReadDuringFlushSeesTheWrite covers a GETATTR that runs while a COMMIT or
// CLOSE flushes an acknowledged write. Until the store holds the write, the read
// must still see it through the pending state; otherwise it reports the file as
// it was before the write, with the old size and the old mtime, ctime and change
// attribute, which a client holding a write delegation takes as the file's times.
func TestReadDuringFlushSeesTheWrite(t *testing.T) {
	svc, store, rootHandle, _ := newApplierFixture(t)
	ctx := rootAuthContext()

	_, _, err := svc.CreateFile(ctx, rootHandle, "w.bin", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := store.GetChild(ctx.Context, rootHandle, "w.bin")
	require.NoError(t, err)

	intent, err := svc.PrepareWrite(ctx, handle, 4096)
	require.NoError(t, err)
	_, err = svc.CommitWrite(ctx, intent)
	require.NoError(t, err)

	var stored, seen *metadata.File
	duringFlush(svc, func() {
		stored, err = store.GetFile(ctx.Context, handle)
		require.NoError(t, err)
		seen, err = svc.GetFile(ctx.Context, handle)
		require.NoError(t, err)
	})
	flushed, err := svc.FlushPendingWriteForFile(ctx, handle, false)
	require.NoError(t, err)
	require.True(t, flushed, "expected a pending write to flush")

	require.NotNil(t, seen, "the flush must look up the durable extent")
	require.Zero(t, stored.Size, "the lookup must run before the flush commits, or this test checks nothing")
	require.True(t, seen.Mtime.Equal(intent.NewMtime), "mtime: got %v, the write's is %v", seen.Mtime, intent.NewMtime)
	require.True(t, seen.Ctime.Equal(intent.NewMtime), "ctime: got %v, the write's is %v", seen.Ctime, intent.NewMtime)
	require.Equal(t, uint64(4096), seen.Size, "a read during the flush must report the acknowledged size")
}

// TestWriteDuringFlushStaysPending covers a write acknowledged while a flush of
// an earlier one is committing. The flush commits what it read before that
// write, so the write must stay pending for the next flush rather than be
// dropped with the state the flush is done with.
func TestWriteDuringFlushStaysPending(t *testing.T) {
	svc, store, rootHandle, _ := newApplierFixture(t)
	ctx := rootAuthContext()

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

	write(4096)
	raced := false
	duringFlush(svc, func() {
		if !raced {
			raced = true
			write(8192)
		}
	})
	_, err = svc.FlushPendingWriteForFile(ctx, handle, false)
	require.NoError(t, err)
	require.True(t, raced)

	seen, err := svc.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(8192), seen.Size, "the write made during the flush must still be pending")

	flushed, err := svc.FlushPendingWriteForFile(ctx, handle, false)
	require.NoError(t, err)
	require.True(t, flushed, "the write made during the flush must be left for the next flush")
	f, err := store.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(8192), f.Size, "the next flush must commit the write made during the first")
}
