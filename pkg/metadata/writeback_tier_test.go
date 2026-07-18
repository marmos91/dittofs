package metadata_test

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/stretchr/testify/require"
)

// transactorSpy wraps a memory store and records whether a flush committed
// through the durable (WithTransaction) or relaxed (WithTransactionRelaxed)
// path. The bare memory store does NOT implement RelaxedTransactor, so
// withRelaxedTransaction would otherwise fall back to WithTransaction and hide
// the split; the spy implements both to make the choice observable (#1757).
type transactorSpy struct {
	*memory.MemoryMetadataStore
	durable int
	relaxed int
}

func (s *transactorSpy) WithTransaction(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	s.durable++
	return s.MemoryMetadataStore.WithTransaction(ctx, fn)
}

func (s *transactorSpy) WithTransactionRelaxed(ctx context.Context, fn func(tx metadata.Transaction) error) error {
	s.relaxed++
	// Memory has no separate relaxed commit; delegate the actual write durably.
	return s.MemoryMetadataStore.WithTransaction(ctx, fn)
}

func (s *transactorSpy) reset() { s.durable, s.relaxed = 0, 0 }

// newWritebackFixture builds a Service backed by a transactorSpy for one share.
func newWritebackFixture(t *testing.T) (*metadata.Service, *transactorSpy, metadata.FileHandle, *metadata.AuthContext) {
	t.Helper()

	spy := &transactorSpy{MemoryMetadataStore: memory.NewMemoryMetadataStoreWithDefaults()}
	ctx := context.Background()
	const shareName = "/wb"

	rootFile, err := spy.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0777,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(shareName, rootFile.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(shareName, spy))

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID:  metadata.Uint32Ptr(0),
			GID:  metadata.Uint32Ptr(0),
			GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
	return svc, spy, rootHandle, authCtx
}

// bufferPendingWrite records a deferred WRITE that leaves a buffered MaxSize in
// the pending-writes tracker without committing it (an UNSTABLE NFS WRITE).
func bufferPendingWrite(t *testing.T, svc *metadata.Service, authCtx *metadata.AuthContext, handle metadata.FileHandle, size uint64) {
	t.Helper()
	intent, err := svc.PrepareWrite(authCtx, handle, size)
	require.NoError(t, err)
	_, err = svc.CommitWrite(authCtx, intent)
	require.NoError(t, err)
	got, ok := svc.GetPendingSize(handle)
	require.True(t, ok, "expected buffered pending write")
	require.Equal(t, size, got)
}

func createChild(t *testing.T, svc *metadata.Service, spy *transactorSpy, authCtx *metadata.AuthContext, root metadata.FileHandle, name string) metadata.FileHandle {
	t.Helper()
	_, _, err := svc.CreateFile(authCtx, root, name, &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := spy.GetChild(authCtx.Context, root, name)
	require.NoError(t, err)
	return handle
}

// TestWritebackTier_DurableFlushRelaxedWhenEnabled asserts that a FILE_SYNC
// (durable=true) flush for a writeback share commits through the relaxed
// deferred-fsync path, not the inline durable transaction.
func TestWritebackTier_DurableFlushRelaxedWhenEnabled(t *testing.T) {
	svc, spy, root, authCtx := newWritebackFixture(t)
	svc.SetShareWriteback("/wb", true)

	handle := createChild(t, svc, spy, authCtx, root, "f.txt")
	bufferPendingWrite(t, svc, authCtx, handle, 4096)

	spy.reset()
	flushed, err := svc.FlushPendingWriteForFile(authCtx, handle, true) // durable request
	require.NoError(t, err)
	require.True(t, flushed)

	require.Equal(t, 1, spy.relaxed, "writeback share must downgrade a durable flush to the relaxed path")
	require.Equal(t, 0, spy.durable, "writeback share must not take the inline durable path")
}

// TestWritebackTier_DurableFlushStaysDurableByDefault asserts the default
// (no writeback) keeps the inline durable transaction for a FILE_SYNC flush.
func TestWritebackTier_DurableFlushStaysDurableByDefault(t *testing.T) {
	svc, spy, root, authCtx := newWritebackFixture(t)
	// No SetShareWriteback -> default durable.

	handle := createChild(t, svc, spy, authCtx, root, "f.txt")
	bufferPendingWrite(t, svc, authCtx, handle, 4096)

	spy.reset()
	flushed, err := svc.FlushPendingWriteForFile(authCtx, handle, true)
	require.NoError(t, err)
	require.True(t, flushed)

	require.Equal(t, 1, spy.durable, "default share must keep the inline durable path for a durable flush")
	require.Equal(t, 0, spy.relaxed, "default share must not take the relaxed path")
}

// TestWritebackTier_ShutdownFlushStaysDurable asserts the shutdown flush stays
// durable even for a writeback share: FlushAllPendingWritesForShutdown calls
// flushPendingWrite directly with durable=true, bypassing the downgrade.
func TestWritebackTier_ShutdownFlushStaysDurable(t *testing.T) {
	svc, spy, root, authCtx := newWritebackFixture(t)
	svc.SetShareWriteback("/wb", true)

	handle := createChild(t, svc, spy, authCtx, root, "f.txt")
	bufferPendingWrite(t, svc, authCtx, handle, 4096)

	spy.reset()
	n, err := svc.FlushAllPendingWritesForShutdown(5 * time.Second)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	require.Equal(t, 1, spy.durable, "shutdown flush must stay durable even for a writeback share")
	require.Equal(t, 0, spy.relaxed, "shutdown flush must not be downgraded to the relaxed path")
}
