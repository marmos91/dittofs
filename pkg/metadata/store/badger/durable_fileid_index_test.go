package badger

import (
	"context"
	"encoding/json"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

func newDurableStoreForTest(t *testing.T) *badgerDurableStore {
	t.Helper()

	db, err := badgerdb.Open(badgerdb.DefaultOptions(t.TempDir()).WithLogger(nil))
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() {
		if cErr := db.Close(); cErr != nil {
			t.Fatalf("close badger: %v", cErr)
		}
	})

	return newBadgerDurableStore(db)
}

func durableHandleForFile(id string, fileID [16]byte) *lock.PersistedDurableHandle {
	var createGuid [16]byte
	copy(createGuid[:], "cguid-"+id)

	return &lock.PersistedDurableHandle{
		ID:             id,
		FileID:         fileID,
		Path:           "/" + id,
		ShareName:      "/export",
		MetadataHandle: []byte("mh-" + id),
		CreateGuid:     createGuid,
	}
}

// seedLegacyFileIDIndex writes the unsuffixed FileID index entry an older store
// would have written, alongside a primary record the store itself wrote.
func seedLegacyFileIDIndex(t *testing.T, s *badgerDurableStore, handle *lock.PersistedDurableHandle) {
	t.Helper()

	data, err := json.Marshal(handle)
	if err != nil {
		t.Fatalf("marshal handle: %v", err)
	}

	err = s.db.Update(func(txn *badgerdb.Txn) error {
		if err := txn.Set([]byte(prefixDHID+handle.ID), data); err != nil {
			return err
		}
		return txn.Set(fileIDIndexScanPrefix(handle.FileID), []byte(handle.ID))
	})
	if err != nil {
		t.Fatalf("seed legacy index: %v", err)
	}
}

func legacyFileIDIndexOwner(t *testing.T, s *badgerDurableStore, fileID [16]byte) (string, bool) {
	t.Helper()

	var owner string
	var present bool
	err := s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(fileIDIndexScanPrefix(fileID))
		if err == badgerdb.ErrKeyNotFound {
			return nil
		}
		if err != nil {
			return err
		}
		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		owner, present = string(val), true
		return nil
	})
	if err != nil {
		t.Fatalf("read legacy index: %v", err)
	}
	return owner, present
}

// TestLegacyFileIDIndexIsReadable checks that a FileID entry written before the
// per-handle suffix existed still resolves through the current lookup path.
func TestLegacyFileIDIndexIsReadable(t *testing.T) {
	s := newDurableStoreForTest(t)
	ctx := context.Background()

	var fileID [16]byte
	copy(fileID[:], "legacy-file-0001")
	legacy := durableHandleForFile("legacy-handle", fileID)
	seedLegacyFileIDIndex(t, s, legacy)

	got, err := s.GetDurableHandleByFileID(ctx, fileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID() error: %v", err)
	}
	if got == nil || got.ID != legacy.ID {
		t.Fatalf("legacy FileID entry did not resolve, got %+v", got)
	}
}

// TestLegacyFileIDIndexClearedWithItsOwner checks that deleting the handle a
// legacy entry names removes the entry, while deleting an unrelated handle on
// the same file leaves it in place.
func TestLegacyFileIDIndexClearedWithItsOwner(t *testing.T) {
	s := newDurableStoreForTest(t)
	ctx := context.Background()

	var fileID [16]byte
	copy(fileID[:], "legacy-file-0002")

	legacy := durableHandleForFile("legacy-owner", fileID)
	seedLegacyFileIDIndex(t, s, legacy)

	current := durableHandleForFile("current-handle", fileID)
	if err := s.PutDurableHandle(ctx, current); err != nil {
		t.Fatalf("PutDurableHandle() error: %v", err)
	}

	if err := s.DeleteDurableHandle(ctx, current.ID); err != nil {
		t.Fatalf("DeleteDurableHandle(current) error: %v", err)
	}
	owner, present := legacyFileIDIndexOwner(t, s, fileID)
	if !present || owner != legacy.ID {
		t.Fatalf("legacy entry lost when an unrelated handle was deleted (present=%v owner=%q)", present, owner)
	}

	if err := s.DeleteDurableHandle(ctx, legacy.ID); err != nil {
		t.Fatalf("DeleteDurableHandle(legacy) error: %v", err)
	}
	if _, present := legacyFileIDIndexOwner(t, s, fileID); present {
		t.Fatal("legacy entry survived deletion of the handle it names")
	}

	got, err := s.GetDurableHandleByFileID(ctx, fileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID() error: %v", err)
	}
	if got != nil {
		t.Fatalf("FileID lookup resolves after both handles were deleted: %+v", got)
	}
}
