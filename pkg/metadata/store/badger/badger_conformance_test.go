//go:build integration

package badger_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

func TestConformance(t *testing.T) {
	storetest.RunConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
		})
		return store
	})
}

func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, func(t *testing.T) metadata.MetadataStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
		})
		return store
	})
}

func TestResetThenRestoreConformance(t *testing.T) {
	storetest.ResetThenRestoreConformance(t, func(t *testing.T) metadata.MetadataStore {
		dbPath := filepath.Join(t.TempDir(), "metadata.db")
		store, err := badger.NewBadgerMetadataStoreWithDefaults(context.Background(), dbPath)
		if err != nil {
			t.Fatalf("NewBadgerMetadataStoreWithDefaults() failed: %v", err)
		}
		t.Cleanup(func() {
			store.Close()
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
			store.Close()
		})
		return store
	})
}

func TestBadgerStore_PutGetFile_BlocksRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { store.Close() })

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
	var h1, h2 blockstore.ContentHash
	for i := range h1 {
		h1[i] = byte(i)
		h2[i] = byte(0xff - i)
	}
	want := []blockstore.BlockRef{
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
