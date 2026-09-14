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

// newHoldbackFixture builds a Service whose durable-extent resolver reports that
// nothing has reached stable storage yet, the state a journal-backed block store
// is in between fsyncs. Every pending flush therefore holds the size back and
// leaves the write pending, which is the condition both tests below exercise.
func newHoldbackFixture(t *testing.T) (*metadata.Service, metadata.Store, metadata.FileHandle, *metadata.AuthContext) {
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

	const share = "/holdback"
	root, err := store.CreateRootDirectory(ctx, share,
		&metadata.FileAttr{Type: metadata.FileTypeDirectory, Mode: 0777})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeShareHandle(share, root.ID)
	require.NoError(t, err)

	svc := metadata.New()
	require.NoError(t, svc.RegisterStoreForShare(share, store))
	svc.SetDurableExtentResolver(func(string, metadata.PayloadID) (int64, bool) { return 0, true })

	authCtx := &metadata.AuthContext{
		Context:    ctx,
		AuthMethod: "unix",
		Identity: &metadata.Identity{
			UID: metadata.Uint32Ptr(0), GID: metadata.Uint32Ptr(0), GIDs: []uint32{0},
		},
		ClientAddr: "127.0.0.1",
	}
	return svc, store, rootHandle, authCtx
}

// TestHeldBackSizeDoesNotFreezeTheWriteTime covers a second write to a file
// whose first write is still held back for durability. The mtime a write is
// given is frozen for the lifetime of one pending-write session so every reply
// in that session reports the same value, but a flush that commits the mtime
// ends the session: the value is published, and the next write must take a fresh
// one. Holding bytes back withholds the size, never the already-committed write
// time, so the second write must not inherit the first's timestamp.
func TestHeldBackSizeDoesNotFreezeTheWriteTime(t *testing.T) {
	svc, store, rootHandle, ctx := newHoldbackFixture(t)

	_, _, err := svc.CreateFile(ctx, rootHandle, "w.bin", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := store.GetChild(ctx.Context, rootHandle, "w.bin")
	require.NoError(t, err)

	write := func(size uint64) time.Time {
		intent, err := svc.PrepareWrite(ctx, handle, size)
		require.NoError(t, err)
		_, err = svc.CommitWrite(ctx, intent)
		require.NoError(t, err)
		_, err = svc.FlushPendingWriteForFile(ctx, handle, false)
		require.NoError(t, err)
		return intent.NewMtime
	}

	first := write(1)
	time.Sleep(20 * time.Millisecond)
	second := write(1)

	require.True(t, second.After(first),
		"the second write must take a fresh mtime, got %v for both", first)

	f, err := store.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.False(t, f.Mtime.Before(second),
		"the store must carry the second write's mtime, got %v want >= %v", f.Mtime, second)
}

// TestReadDirectoryReportsTheAckedSize covers a directory listing of a file
// whose size is held back from the store for durability. Every other read path
// (GETATTR, QUERY_INFO, READ) reports the acknowledged size by merging the
// pending write, so a listing that reports the stored size instead contradicts
// them: the same file measures 0 bytes in the directory and its real length
// through a handle.
func TestReadDirectoryReportsTheAckedSize(t *testing.T) {
	svc, store, rootHandle, ctx := newHoldbackFixture(t)

	_, _, err := svc.CreateFile(ctx, rootHandle, "w.bin", &metadata.FileAttr{Mode: 0644})
	require.NoError(t, err)
	handle, err := store.GetChild(ctx.Context, rootHandle, "w.bin")
	require.NoError(t, err)

	intent, err := svc.PrepareWrite(ctx, handle, 7)
	require.NoError(t, err)
	_, err = svc.CommitWrite(ctx, intent)
	require.NoError(t, err)
	_, err = svc.FlushPendingWriteForFile(ctx, handle, false)
	require.NoError(t, err)

	byHandle, err := svc.GetFile(ctx.Context, handle)
	require.NoError(t, err)
	require.Equal(t, uint64(7), byHandle.Size, "GETATTR must report the acknowledged size")

	page, err := svc.ReadDirectory(ctx, rootHandle, 0, 0)
	require.NoError(t, err)

	var found *metadata.DirEntry
	for i := range page.Entries {
		if page.Entries[i].Name == "w.bin" {
			found = &page.Entries[i]
		}
	}
	require.NotNil(t, found, "listing must contain the written file")
	require.NotNil(t, found.Attr, "listing must carry attributes")
	require.Equal(t, byHandle.Size, found.Attr.Size,
		"the listing must report the same size a handle read does")
	require.False(t, found.Attr.Mtime.Before(byHandle.Mtime),
		"the listing must report the same write time a handle read does")
}
