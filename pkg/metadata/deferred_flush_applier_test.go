package metadata_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/sqlite"
	"github.com/stretchr/testify/require"
)

// newApplierFixture builds a Service over a sqlite store. sqlite implements
// DataWriteApplier, so the deferred pending-write flush takes the narrow
// single-statement path rather than the GetFile+UpdateAttrs fallback. The memory
// store used by the other service tests does not implement it, so this is the
// only place the fast path is exercised end to end through the Service.
func newApplierFixture(t *testing.T) (*metadata.Service, metadata.Store, metadata.FileHandle, string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.NewSQLiteMetadataStore(ctx,
		&sqlite.SQLiteMetadataStoreConfig{Path: filepath.Join(t.TempDir(), "m.db"), AutoMigrate: true},
		metadata.FilesystemCapabilities{
			MaxReadSize: 1048576, PreferredReadSize: 1048576,
			MaxWriteSize: 1048576, PreferredWriteSize: 1048576,
			MaxFileSize: 1 << 62, MaxFilenameLen: 255,
			MaxPathLen: 4096, MaxHardLinkCount: 32767,
			SupportsHardLinks: true, SupportsSymlinks: true,
			CaseSensitive: true, CasePreserving: true, TimestampResolution: 1,
		})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

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

// TestDeferredFlushAppliesNarrowWrite covers the default write path: deferred
// commits are on by default, so a WRITE buffers into the pending-write tracker
// and only the flush touches the store. It asserts the flush persists what the
// GetFile+UpdateAttrs fallback would have persisted — size grown, times stamped,
// and no shrink when a later write lands at a lower offset.
func TestDeferredFlushAppliesNarrowWrite(t *testing.T) {
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
