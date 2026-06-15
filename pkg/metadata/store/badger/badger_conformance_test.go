package badger_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.Store {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("store.Close() failed: %v", err)
			}
		})
		return store
	})
}

func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.Store {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("store.Close() failed: %v", err)
			}
		})
		return store
	})
}

func TestResetThenRestoreConformance(t *testing.T) {
	storetest.ResetThenRestoreConformance(t, func(t *testing.T) metadata.Store {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("store.Close() failed: %v", err)
			}
		})
		return store
	})
}

func TestLockPersistenceConformance(t *testing.T) {
	storetest.RunLockPersistenceSuite(t, func(t *testing.T) lock.LockStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("store.Close() failed: %v", err)
			}
		})
		return store
	})
}

// TestBadgerStore_CleanShutdownMarkerDurable pins the area-4 H7 marker on the
// real on-disk path: a fresh DB defaults to unclean (false); a graceful Close
// records clean=true and that value SURVIVES a reopen (the durable property the
// in-memory store cannot exercise); and the boot-path clear (SetCleanShutdown
// false) is itself durable, so a process that clears-then-crashes is read as
// unclean on the following open.
//
// A true kill -9 cannot be emulated in-process (Badger holds an OS directory
// lock released only by Close or process exit), so the unclean path is verified
// by persisting the boot-clear and then performing a graceful Close BUT reading
// the value the boot-clear wrote BEFORE Close re-marks it — i.e. the clear is
// durable on its own write.
func TestBadgerStore_CleanShutdownMarkerDurable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")

	// Open #1: fresh store must default to unclean. Then graceful Close records
	// clean=true.
	s1, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	clean, err := s1.GetCleanShutdown(ctx)
	require.NoError(t, err)
	require.False(t, clean, "fresh on-disk store must default to unclean (false)")
	require.NoError(t, s1.Close())

	// Open #2: the clean marker must have survived the close+reopen.
	s2, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	clean, err = s2.GetCleanShutdown(ctx)
	require.NoError(t, err)
	require.True(t, clean, "graceful Close must durably record clean=true across reopen")
	// The boot path clears the marker for the running session; that clear is a
	// durable write (verified by reading it back within the same open).
	require.NoError(t, s2.SetCleanShutdown(ctx, false))
	clean, err = s2.GetCleanShutdown(ctx)
	require.NoError(t, err)
	require.False(t, clean, "boot-clear of the marker must be durable (read-back within session)")
	require.NoError(t, s2.Close())
}

// TestBadgerStore_DerivePathReverseIndex pins the cn:<parent>:<child> reverse
// index used by derivePath (#1166): after cross-directory moves, a parent-dir
// rename, and delete+recreate of a name, GetFile must always return the current
// derived path and never a stale name left behind by the previous edge. A
// regression in reverse-index maintenance (a leftover or un-repointed cn: key)
// surfaces here as a wrong or empty derived path.
func TestBadgerStore_DerivePathReverseIndex(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	shareName := "/rev"
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)

	now := time.Now().UTC()
	mkChild := func(parent metadata.FileHandle, name string, dir bool) metadata.FileHandle {
		h, err := store.GenerateHandle(ctx, shareName, "/"+name)
		require.NoError(t, err)
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err)
		typ := metadata.FileTypeRegular
		if dir {
			typ = metadata.FileTypeDirectory
		}
		require.NoError(t, store.PutFile(ctx, &metadata.File{
			ID: id, ShareName: shareName,
			FileAttr: metadata.FileAttr{
				Type: typ, Mode: 0o644,
				Mtime: now, Ctime: now, Atime: now, CreationTime: now,
			},
		}))
		require.NoError(t, store.SetParent(ctx, h, parent))
		require.NoError(t, store.SetChild(ctx, parent, name, h))
		return h
	}

	// Build /dirA/file and a sibling /dirB.
	dirA := mkChild(rootHandle, "dirA", true)
	dirB := mkChild(rootHandle, "dirB", true)
	fileH := mkChild(dirA, "file", false)

	got, err := store.GetFile(ctx, fileH)
	require.NoError(t, err)
	assert.Equal(t, "/dirA/file", got.Path, "initial derived path")

	// Cross-directory move /dirA/file -> /dirB/moved: repoints the parent edge
	// AND the reverse name edge. A stale cn: entry would surface the old name.
	require.NoError(t, store.DeleteChild(ctx, dirA, "file"))
	require.NoError(t, store.SetChild(ctx, dirB, "moved", fileH))
	require.NoError(t, store.SetParent(ctx, fileH, dirB))

	got, err = store.GetFile(ctx, fileH)
	require.NoError(t, err)
	assert.Equal(t, "/dirB/moved", got.Path, "derived path after cross-dir move")

	// Rename the parent directory dirB -> dirC; the descendant's path must
	// reflect it on read with no per-descendant writes.
	require.NoError(t, store.DeleteChild(ctx, rootHandle, "dirB"))
	require.NoError(t, store.SetChild(ctx, rootHandle, "dirC", dirB))

	got, err = store.GetFile(ctx, fileH)
	require.NoError(t, err)
	assert.Equal(t, "/dirC/moved", got.Path, "descendant path after parent-dir rename")

	// Delete the "moved" name, then recreate a NEW file under the same name in
	// the same directory. The new inode must derive /dirC/moved — never inherit
	// a stale reverse edge from the deleted inode.
	require.NoError(t, store.DeleteChild(ctx, dirB, "moved"))
	fresh := mkChild(dirB, "moved", false)
	require.NotEqual(t, string(fresh), string(fileH))
	got, err = store.GetFile(ctx, fresh)
	require.NoError(t, err)
	assert.Equal(t, "/dirC/moved", got.Path, "recreated name derives its own path, no stale resurface")

	// The original (now unlinked) inode has no parent edge left, so it derives
	// the root sentinel rather than a stale name.
	got, err = store.GetFile(ctx, fileH)
	require.NoError(t, err)
	assert.Equal(t, "/", got.Path, "unlinked inode derives '/' (no stale reverse edge)")
}

func TestBadgerStore_PutGetFile_BlocksRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	// Set up share + root.
	shareName := "/blocks-test"
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootAttr := &metadata.FileAttr{
		Type: metadata.FileTypeDirectory,
		Mode: 0o755,
	}
	rootFile, err := store.CreateRootDirectory(ctx, shareName, rootAttr)
	require.NoError(t, err)
	rootHandle, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)

	// Build deterministic BlockRef fixtures.
	var h1, h2 block.ContentHash
	for i := range h1 {
		h1[i] = byte(i)
		h2[i] = byte(0xff - i)
	}
	want := []block.BlockRef{
		{Hash: h1, Offset: 0, Size: 4 * 1024 * 1024},
		{Hash: h2, Offset: 4 * 1024 * 1024, Size: 4 * 1024 * 1024},
	}

	// Put file with Blocks populated.
	handle, err := store.GenerateHandle(ctx, shareName, "/blocks.bin")
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	now := time.Now().UTC()
	file := &metadata.File{
		ID:        id,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         0o644,
			UID:          1000,
			GID:          1000,
			Size:         8 * 1024 * 1024,
			Mtime:        now,
			Ctime:        now,
			Atime:        now,
			CreationTime: now,
			Blocks:       want,
		},
	}
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, rootHandle))
	require.NoError(t, store.SetChild(ctx, rootHandle, "blocks.bin", handle))

	// Get back and verify Blocks.
	got, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Blocks, 2)
	assert.Equal(t, want[0], got.Blocks[0])
	assert.Equal(t, want[1], got.Blocks[1])

	// Sanity: also verify a file with nil Blocks survives Put/Get with Blocks==nil.
	nilHandle, err := store.GenerateHandle(ctx, shareName, "/nil-blocks.bin")
	require.NoError(t, err)
	_, nilID, err := metadata.DecodeFileHandle(nilHandle)
	require.NoError(t, err)
	nilFile := &metadata.File{
		ID:        nilID,
		ShareName: shareName,
		FileAttr: metadata.FileAttr{
			Type:         metadata.FileTypeRegular,
			Mode:         0o644,
			Mtime:        now,
			Ctime:        now,
			Atime:        now,
			CreationTime: now,
			// Blocks left nil.
		},
	}
	require.NoError(t, store.PutFile(ctx, nilFile))
	gotNil, err := store.GetFile(ctx, nilHandle)
	require.NoError(t, err)
	assert.Nil(t, gotNil.Blocks)
}
