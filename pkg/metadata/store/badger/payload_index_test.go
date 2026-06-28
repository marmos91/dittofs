package badger

import (
	"context"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/stretchr/testify/require"
)

// mkPayloadShare creates a share and returns its root handle.
func mkPayloadShare(t *testing.T, store *BadgerMetadataStore, shareName string) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.CreateShare(ctx, &metadata.Share{Name: shareName}))
	rootFile, err := store.CreateRootDirectory(ctx, shareName, &metadata.FileAttr{
		Type: metadata.FileTypeDirectory, Mode: 0o755,
	})
	require.NoError(t, err)
	h, err := metadata.EncodeFileHandle(rootFile)
	require.NoError(t, err)
	return h
}

// mkPayloadFile creates a regular file with the given PayloadID and wires its
// parent/child links, returning its handle.
func mkPayloadFile(t *testing.T, store *BadgerMetadataStore, shareName string, dir metadata.FileHandle, name, fullPath string, pid metadata.PayloadID) metadata.FileHandle {
	t.Helper()
	ctx := context.Background()
	handle, err := store.GenerateHandle(ctx, shareName, fullPath)
	require.NoError(t, err)
	_, id, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	file := &metadata.File{
		ShareName: shareName,
		Path:      fullPath,
		FileAttr: metadata.FileAttr{
			Type: metadata.FileTypeRegular, Mode: 0o600, UID: 1000, GID: 1000,
			PayloadID: pid,
		},
	}
	file.ID = id
	require.NoError(t, store.PutFile(ctx, file))
	require.NoError(t, store.SetParent(ctx, handle, dir))
	require.NoError(t, store.SetChild(ctx, dir, name, handle))
	return handle
}

// payloadIndexValue reads the pl:<payloadID> secondary-index entry directly and
// returns the file UUID it maps to (and whether it exists). It lets the tests
// assert index maintenance independently of the GetFileByPayloadID read path.
func payloadIndexValue(t *testing.T, s *BadgerMetadataStore, pid metadata.PayloadID) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	found := false
	require.NoError(t, s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyPayloadID(pid))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		raw, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		if err := id.UnmarshalBinary(raw); err != nil {
			return err
		}
		found = true
		return nil
	}))
	return id, found
}

// TestPayloadIndex_PointLookupAndTeardown covers the #1435 secondary index: it
// is written on PutFile, resolves GetFileByPayloadID, still works via the legacy
// scan fallback when the entry is absent, and is torn down on DeleteFile.
func TestPayloadIndex_PointLookupAndTeardown(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const shareName = "s1"
	root := mkPayloadShare(t, store, shareName)
	pid := metadata.PayloadID(shareName + "/" + uuid.NewString())

	handle := mkPayloadFile(t, store, shareName, root, "f.bin", "/f.bin", pid)
	_, fileID, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	// Index written and points at the file.
	idxID, ok := payloadIndexValue(t, store, pid)
	require.True(t, ok, "pl: index entry must exist after PutFile")
	require.Equal(t, fileID, idxID)

	// Lookup resolves (via the index fast path).
	got, err := store.GetFileByPayloadID(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, fileID, got.ID)
	require.Equal(t, pid, got.PayloadID)

	// Legacy fallback: drop the index entry and confirm the scan still finds it.
	require.NoError(t, store.db.Update(func(txn *badgerdb.Txn) error {
		return txn.Delete(keyPayloadID(pid))
	}))
	got, err = store.GetFileByPayloadID(ctx, pid)
	require.NoError(t, err)
	require.Equal(t, fileID, got.ID, "scan fallback must resolve an unindexed file")

	// Teardown: deleting the file removes the index entry and the lookup 404s.
	require.NoError(t, store.DeleteFile(ctx, handle))
	if _, ok := payloadIndexValue(t, store, pid); ok {
		t.Fatal("pl: index entry must be gone after DeleteFile")
	}
	_, err = store.GetFileByPayloadID(ctx, pid)
	require.Error(t, err)
	require.True(t, metadata.IsNotFoundError(err), "want not-found, got %v", err)
}

// TestPayloadIndex_MovesOnPayloadIDChange asserts that re-persisting a file with
// a changed PayloadID drops the stale index entry and creates a fresh one, so a
// single file never leaves a dangling pl: key behind.
func TestPayloadIndex_MovesOnPayloadIDChange(t *testing.T) {
	ctx := context.Background()
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const shareName = "s1"
	root := mkPayloadShare(t, store, shareName)
	oldPID := metadata.PayloadID(shareName + "/" + uuid.NewString())

	handle := mkPayloadFile(t, store, shareName, root, "f.bin", "/f.bin", oldPID)
	_, fileID, err := metadata.DecodeFileHandle(handle)
	require.NoError(t, err)

	// Re-persist the same inode with a new PayloadID.
	file, err := store.GetFile(ctx, handle)
	require.NoError(t, err)
	newPID := metadata.PayloadID(shareName + "/" + uuid.NewString())
	file.PayloadID = newPID
	require.NoError(t, store.PutFile(ctx, file))

	// Old entry gone, new entry points at the same file.
	if _, ok := payloadIndexValue(t, store, oldPID); ok {
		t.Fatal("stale pl: entry for old PayloadID must be removed")
	}
	idxID, ok := payloadIndexValue(t, store, newPID)
	require.True(t, ok, "pl: entry for new PayloadID must exist")
	require.Equal(t, fileID, idxID)

	got, err := store.GetFileByPayloadID(ctx, newPID)
	require.NoError(t, err)
	require.Equal(t, fileID, got.ID)
}
