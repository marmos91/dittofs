package metadata_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// recordPendingWrite issues a deferred WRITE that leaves a buffered MaxSize in
// the pending-writes tracker WITHOUT committing it to the store, mirroring an
// UNSTABLE NFS WRITE that has not yet been flushed by COMMIT.
func recordPendingWrite(t *testing.T, fx *testFixture, handle metadata.FileHandle, size uint64) {
	t.Helper()
	intent, err := fx.service.PrepareWrite(fx.rootContext(), handle, size)
	require.NoError(t, err)
	_, err = fx.service.CommitWrite(fx.rootContext(), intent)
	require.NoError(t, err)
	got, ok := fx.service.GetPendingSize(handle)
	require.True(t, ok, "expected buffered pending write")
	require.Equal(t, size, got)
}

// TestTruncateDiscardsPendingWrite asserts a truncate-to-0 SETATTR that lands
// after an unflushed WRITE cannot be undone by a later pending-write flush that
// re-grows the file to the buffered MaxSize (#1753).
func TestTruncateDiscardsPendingWrite(t *testing.T) {
	fx := newTestFixture(t)

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "trunc.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(fx.rootContext().Context, fx.rootHandle, "trunc.txt")
	require.NoError(t, err)

	// A WRITE buffers MaxSize=4096 without flushing to the store.
	recordPendingWrite(t, fx, handle, 4096)

	// Truncate to 0 (size-changing SETATTR).
	_, err = fx.service.SetFileAttributes(fx.rootContext(), handle, &metadata.SetAttrs{Size: metadata.Uint64Ptr(0)})
	require.NoError(t, err)

	// The buffered pending state must be gone after truncate.
	_, ok := fx.service.GetPendingSize(handle)
	require.False(t, ok, "truncate should have discarded pending write state")

	// A flush must not resurrect the pre-truncate size.
	_, err = fx.service.FlushPendingWriteForFile(fx.rootContext(), handle, true)
	require.NoError(t, err)

	file, err := fx.store.GetFile(fx.rootContext().Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(0), file.Size, "truncated file must stay size 0, not resurrect to buffered MaxSize")
}

// TestRemoveFileDiscardsPendingWrite asserts RemoveFile drops buffered
// pending-write state for the unlinked file so a later flush cannot apply a
// stale size/mtime to the removed inode (#1753).
func TestRemoveFileDiscardsPendingWrite(t *testing.T) {
	fx := newTestFixture(t)

	_, _, err := fx.service.CreateFile(fx.rootContext(), fx.rootHandle, "gone.txt", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := fx.store.GetChild(fx.rootContext().Context, fx.rootHandle, "gone.txt")
	require.NoError(t, err)

	recordPendingWrite(t, fx, handle, 4096)

	_, _, err = fx.service.RemoveFile(fx.rootContext(), fx.rootHandle, "gone.txt")
	require.NoError(t, err)

	_, ok := fx.service.GetPendingSize(handle)
	require.False(t, ok, "RemoveFile should have discarded pending write state for the unlinked file")
}
