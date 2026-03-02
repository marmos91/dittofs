package storetest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// DurableHandleStoreProvider is an interface that metadata stores can implement
// to provide access to their DurableHandleStore implementation.
type DurableHandleStoreProvider interface {
	DurableHandleStore() lock.DurableHandleStore
}

// RunDurableHandleStoreTests runs the full conformance test suite for DurableHandleStore.
// The factory creates a fresh MetadataStore for each test. The store must implement
// the DurableHandleStoreProvider interface to be tested.
//
// If the store does not implement DurableHandleStoreProvider, the test is skipped.
func RunDurableHandleStoreTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	// Verify the store implements DurableHandleStoreProvider
	store := factory(t)
	provider, ok := store.(DurableHandleStoreProvider)
	if !ok {
		t.Skip("store does not implement DurableHandleStoreProvider")
		return
	}
	// Quick sanity check that accessor works
	_ = provider.DurableHandleStore()

	t.Run("PutAndGet", func(t *testing.T) {
		testDurablePutAndGet(t, factory)
	})

	t.Run("GetNonExistent", func(t *testing.T) {
		testDurableGetNonExistent(t, factory)
	})

	t.Run("PutOverwrite", func(t *testing.T) {
		testDurablePutOverwrite(t, factory)
	})

	t.Run("Delete", func(t *testing.T) {
		testDurableDelete(t, factory)
	})

	t.Run("DeleteNonExistent", func(t *testing.T) {
		testDurableDeleteNonExistent(t, factory)
	})

	t.Run("GetByFileID", func(t *testing.T) {
		testDurableGetByFileID(t, factory)
	})

	t.Run("GetByCreateGuid", func(t *testing.T) {
		testDurableGetByCreateGuid(t, factory)
	})

	t.Run("GetByAppInstanceId", func(t *testing.T) {
		testDurableGetByAppInstanceId(t, factory)
	})

	t.Run("GetByFileHandle", func(t *testing.T) {
		testDurableGetByFileHandle(t, factory)
	})

	t.Run("ListAll", func(t *testing.T) {
		testDurableListAll(t, factory)
	})

	t.Run("ListByShare", func(t *testing.T) {
		testDurableListByShare(t, factory)
	})

	t.Run("DeleteExpired", func(t *testing.T) {
		testDurableDeleteExpired(t, factory)
	})

	t.Run("DeleteExpiredKeepsActive", func(t *testing.T) {
		testDurableDeleteExpiredKeepsActive(t, factory)
	})
}

// getDurableStore extracts the DurableHandleStore from a factory-created store.
func getDurableStore(t *testing.T, factory StoreFactory) lock.DurableHandleStore {
	t.Helper()
	store := factory(t)
	provider := store.(DurableHandleStoreProvider)
	return provider.DurableHandleStore()
}

// makeDurableHandle creates a test PersistedDurableHandle with distinct values.
func makeDurableHandle(id string, shareName string) *lock.PersistedDurableHandle {
	now := time.Now().UTC().Truncate(time.Microsecond)

	var fileID [16]byte
	copy(fileID[:], []byte("fileid-"+id))

	var createGuid [16]byte
	copy(createGuid[:], []byte("cguid-"+id))

	var appInstanceId [16]byte
	copy(appInstanceId[:], []byte("appid-"+id))

	var leaseKey [16]byte
	copy(leaseKey[:], []byte("lease-"+id))

	var sessionKeyHash [32]byte
	copy(sessionKeyHash[:], []byte("sessionhash-"+id))

	return &lock.PersistedDurableHandle{
		ID:              id,
		FileID:          fileID,
		Path:            "/test/" + id + ".txt",
		ShareName:       shareName,
		DesiredAccess:   0x12019F,
		ShareAccess:     0x07,
		CreateOptions:   0x40,
		MetadataHandle:  []byte("handle-" + id),
		PayloadID:       "payload-" + id,
		OplockLevel:     0x09,
		LeaseKey:        leaseKey,
		LeaseState:      0x07,
		CreateGuid:      createGuid,
		AppInstanceId:   appInstanceId,
		Username:        "user-" + id,
		SessionKeyHash:  sessionKeyHash,
		IsV2:            true,
		CreatedAt:       now.Add(-10 * time.Minute),
		DisconnectedAt:  now,
		TimeoutMs:       60000,
		ServerStartTime: now.Add(-1 * time.Hour),
	}
}

// assertDurableHandleEqual compares two PersistedDurableHandle structs field by field.
func assertDurableHandleEqual(t *testing.T, expected, actual *lock.PersistedDurableHandle) {
	t.Helper()

	if actual == nil {
		t.Fatal("actual handle is nil")
	}
	if expected.ID != actual.ID {
		t.Errorf("ID: got %q, want %q", actual.ID, expected.ID)
	}
	if expected.FileID != actual.FileID {
		t.Errorf("FileID: got %x, want %x", actual.FileID, expected.FileID)
	}
	if expected.Path != actual.Path {
		t.Errorf("Path: got %q, want %q", actual.Path, expected.Path)
	}
	if expected.ShareName != actual.ShareName {
		t.Errorf("ShareName: got %q, want %q", actual.ShareName, expected.ShareName)
	}
	if expected.DesiredAccess != actual.DesiredAccess {
		t.Errorf("DesiredAccess: got %d, want %d", actual.DesiredAccess, expected.DesiredAccess)
	}
	if expected.ShareAccess != actual.ShareAccess {
		t.Errorf("ShareAccess: got %d, want %d", actual.ShareAccess, expected.ShareAccess)
	}
	if expected.CreateOptions != actual.CreateOptions {
		t.Errorf("CreateOptions: got %d, want %d", actual.CreateOptions, expected.CreateOptions)
	}
	if !bytes.Equal(expected.MetadataHandle, actual.MetadataHandle) {
		t.Errorf("MetadataHandle: got %x, want %x", actual.MetadataHandle, expected.MetadataHandle)
	}
	if expected.PayloadID != actual.PayloadID {
		t.Errorf("PayloadID: got %q, want %q", actual.PayloadID, expected.PayloadID)
	}
	if expected.OplockLevel != actual.OplockLevel {
		t.Errorf("OplockLevel: got %d, want %d", actual.OplockLevel, expected.OplockLevel)
	}
	if expected.LeaseKey != actual.LeaseKey {
		t.Errorf("LeaseKey: got %x, want %x", actual.LeaseKey, expected.LeaseKey)
	}
	if expected.LeaseState != actual.LeaseState {
		t.Errorf("LeaseState: got %d, want %d", actual.LeaseState, expected.LeaseState)
	}
	if expected.CreateGuid != actual.CreateGuid {
		t.Errorf("CreateGuid: got %x, want %x", actual.CreateGuid, expected.CreateGuid)
	}
	if expected.AppInstanceId != actual.AppInstanceId {
		t.Errorf("AppInstanceId: got %x, want %x", actual.AppInstanceId, expected.AppInstanceId)
	}
	if expected.Username != actual.Username {
		t.Errorf("Username: got %q, want %q", actual.Username, expected.Username)
	}
	if expected.SessionKeyHash != actual.SessionKeyHash {
		t.Errorf("SessionKeyHash: got %x, want %x", actual.SessionKeyHash, expected.SessionKeyHash)
	}
	if expected.IsV2 != actual.IsV2 {
		t.Errorf("IsV2: got %v, want %v", actual.IsV2, expected.IsV2)
	}
	if !expected.CreatedAt.Equal(actual.CreatedAt) {
		t.Errorf("CreatedAt: got %v, want %v", actual.CreatedAt, expected.CreatedAt)
	}
	if !expected.DisconnectedAt.Equal(actual.DisconnectedAt) {
		t.Errorf("DisconnectedAt: got %v, want %v", actual.DisconnectedAt, expected.DisconnectedAt)
	}
	if expected.TimeoutMs != actual.TimeoutMs {
		t.Errorf("TimeoutMs: got %d, want %d", actual.TimeoutMs, expected.TimeoutMs)
	}
	if !expected.ServerStartTime.Equal(actual.ServerStartTime) {
		t.Errorf("ServerStartTime: got %v, want %v", actual.ServerStartTime, expected.ServerStartTime)
	}
}

// testDurablePutAndGet tests that Put then Get returns an identical struct.
func testDurablePutAndGet(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle := makeDurableHandle("test-1", "/export")

	if err := ds.PutDurableHandle(ctx, handle); err != nil {
		t.Fatalf("PutDurableHandle() error: %v", err)
	}

	got, err := ds.GetDurableHandle(ctx, "test-1")
	if err != nil {
		t.Fatalf("GetDurableHandle() error: %v", err)
	}

	assertDurableHandleEqual(t, handle, got)
}

// testDurableGetNonExistent tests that Get for non-existent returns nil, nil.
func testDurableGetNonExistent(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	got, err := ds.GetDurableHandle(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("GetDurableHandle() error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for non-existent handle, got: %+v", got)
	}
}

// testDurablePutOverwrite tests that Put overwrites an existing handle.
func testDurablePutOverwrite(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle1 := makeDurableHandle("overwrite-1", "/export")
	handle2 := makeDurableHandle("overwrite-1", "/export") // same ID
	handle2.Path = "/updated/path.txt"
	handle2.Username = "updated-user"

	if err := ds.PutDurableHandle(ctx, handle1); err != nil {
		t.Fatalf("PutDurableHandle(1) error: %v", err)
	}

	if err := ds.PutDurableHandle(ctx, handle2); err != nil {
		t.Fatalf("PutDurableHandle(2) error: %v", err)
	}

	got, err := ds.GetDurableHandle(ctx, "overwrite-1")
	if err != nil {
		t.Fatalf("GetDurableHandle() error: %v", err)
	}

	assertDurableHandleEqual(t, handle2, got)
}

// testDurableDelete tests that Delete removes a handle.
func testDurableDelete(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle := makeDurableHandle("delete-1", "/export")

	if err := ds.PutDurableHandle(ctx, handle); err != nil {
		t.Fatalf("PutDurableHandle() error: %v", err)
	}

	if err := ds.DeleteDurableHandle(ctx, "delete-1"); err != nil {
		t.Fatalf("DeleteDurableHandle() error: %v", err)
	}

	got, err := ds.GetDurableHandle(ctx, "delete-1")
	if err != nil {
		t.Fatalf("GetDurableHandle() error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got: %+v", got)
	}
}

// testDurableDeleteNonExistent tests that deleting a non-existent handle returns nil.
func testDurableDeleteNonExistent(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	if err := ds.DeleteDurableHandle(ctx, "never-existed"); err != nil {
		t.Fatalf("DeleteDurableHandle() should return nil for non-existent, got: %v", err)
	}
}

// testDurableGetByFileID tests lookup by FileID.
func testDurableGetByFileID(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle := makeDurableHandle("fid-1", "/export")
	if err := ds.PutDurableHandle(ctx, handle); err != nil {
		t.Fatalf("PutDurableHandle() error: %v", err)
	}

	got, err := ds.GetDurableHandleByFileID(ctx, handle.FileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID() error: %v", err)
	}
	assertDurableHandleEqual(t, handle, got)

	// Non-existent FileID
	var noSuchFileID [16]byte
	copy(noSuchFileID[:], []byte("nosuchfileid1234"))
	got, err = ds.GetDurableHandleByFileID(ctx, noSuchFileID)
	if err != nil {
		t.Fatalf("GetDurableHandleByFileID() error for non-existent: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for non-existent FileID, got: %+v", got)
	}
}

// testDurableGetByCreateGuid tests lookup by CreateGuid.
func testDurableGetByCreateGuid(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle := makeDurableHandle("cguid-1", "/export")
	if err := ds.PutDurableHandle(ctx, handle); err != nil {
		t.Fatalf("PutDurableHandle() error: %v", err)
	}

	got, err := ds.GetDurableHandleByCreateGuid(ctx, handle.CreateGuid)
	if err != nil {
		t.Fatalf("GetDurableHandleByCreateGuid() error: %v", err)
	}
	assertDurableHandleEqual(t, handle, got)

	// Non-existent CreateGuid
	var noSuchGuid [16]byte
	copy(noSuchGuid[:], []byte("nosuchguid123456"))
	got, err = ds.GetDurableHandleByCreateGuid(ctx, noSuchGuid)
	if err != nil {
		t.Fatalf("GetDurableHandleByCreateGuid() error for non-existent: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for non-existent CreateGuid, got: %+v", got)
	}
}

// testDurableGetByAppInstanceId tests lookup by AppInstanceId.
func testDurableGetByAppInstanceId(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	// Create two handles with the same AppInstanceId (Hyper-V failover scenario)
	handle1 := makeDurableHandle("appid-1", "/export")
	handle2 := makeDurableHandle("appid-2", "/export")
	handle2.AppInstanceId = handle1.AppInstanceId // Same AppInstanceId

	if err := ds.PutDurableHandle(ctx, handle1); err != nil {
		t.Fatalf("PutDurableHandle(1) error: %v", err)
	}
	if err := ds.PutDurableHandle(ctx, handle2); err != nil {
		t.Fatalf("PutDurableHandle(2) error: %v", err)
	}

	results, err := ds.GetDurableHandlesByAppInstanceId(ctx, handle1.AppInstanceId)
	if err != nil {
		t.Fatalf("GetDurableHandlesByAppInstanceId() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Non-existent AppInstanceId
	var noSuchAppId [16]byte
	copy(noSuchAppId[:], []byte("nosuchappid12345"))
	results, err = ds.GetDurableHandlesByAppInstanceId(ctx, noSuchAppId)
	if err != nil {
		t.Fatalf("GetDurableHandlesByAppInstanceId() error for non-existent: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-existent AppInstanceId, got %d", len(results))
	}
}

// testDurableGetByFileHandle tests lookup by metadata file handle.
func testDurableGetByFileHandle(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	handle1 := makeDurableHandle("fh-1", "/export")
	handle2 := makeDurableHandle("fh-2", "/export")
	handle2.MetadataHandle = handle1.MetadataHandle // Same file handle

	if err := ds.PutDurableHandle(ctx, handle1); err != nil {
		t.Fatalf("PutDurableHandle(1) error: %v", err)
	}
	if err := ds.PutDurableHandle(ctx, handle2); err != nil {
		t.Fatalf("PutDurableHandle(2) error: %v", err)
	}

	results, err := ds.GetDurableHandlesByFileHandle(ctx, handle1.MetadataHandle)
	if err != nil {
		t.Fatalf("GetDurableHandlesByFileHandle() error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// Non-existent file handle
	results, err = ds.GetDurableHandlesByFileHandle(ctx, []byte("nonexistent-handle"))
	if err != nil {
		t.Fatalf("GetDurableHandlesByFileHandle() error for non-existent: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-existent file handle, got %d", len(results))
	}
}

// testDurableListAll tests listing all durable handles.
func testDurableListAll(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	// Empty store
	results, err := ds.ListDurableHandles(ctx)
	if err != nil {
		t.Fatalf("ListDurableHandles() error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results from empty store, got %d", len(results))
	}

	// Add handles
	for i := 0; i < 3; i++ {
		h := makeDurableHandle("list-"+string(rune('a'+i)), "/export")
		if err := ds.PutDurableHandle(ctx, h); err != nil {
			t.Fatalf("PutDurableHandle(%d) error: %v", i, err)
		}
	}

	results, err = ds.ListDurableHandles(ctx)
	if err != nil {
		t.Fatalf("ListDurableHandles() error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
}

// testDurableListByShare tests filtering by share name.
func testDurableListByShare(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	h1 := makeDurableHandle("share-1", "/export1")
	h2 := makeDurableHandle("share-2", "/export1")
	h3 := makeDurableHandle("share-3", "/export2")

	for _, h := range []*lock.PersistedDurableHandle{h1, h2, h3} {
		if err := ds.PutDurableHandle(ctx, h); err != nil {
			t.Fatalf("PutDurableHandle(%s) error: %v", h.ID, err)
		}
	}

	// List for /export1
	results, err := ds.ListDurableHandlesByShare(ctx, "/export1")
	if err != nil {
		t.Fatalf("ListDurableHandlesByShare(/export1) error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results for /export1, got %d", len(results))
	}

	// List for /export2
	results, err = ds.ListDurableHandlesByShare(ctx, "/export2")
	if err != nil {
		t.Fatalf("ListDurableHandlesByShare(/export2) error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for /export2, got %d", len(results))
	}

	// List for non-existent share
	results, err = ds.ListDurableHandlesByShare(ctx, "/nonexistent")
	if err != nil {
		t.Fatalf("ListDurableHandlesByShare(/nonexistent) error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for non-existent share, got %d", len(results))
	}
}

// testDurableDeleteExpired tests that expired handles are removed based on DisconnectedAt + TimeoutMs.
func testDurableDeleteExpired(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)

	// Create an expired handle (disconnected 2 minutes ago, 60s timeout)
	expired := makeDurableHandle("expired-1", "/export")
	expired.DisconnectedAt = now.Add(-2 * time.Minute)
	expired.TimeoutMs = 60000 // 60 seconds

	// Create a non-expired handle (disconnected 30 seconds ago, 60s timeout)
	active := makeDurableHandle("active-1", "/export")
	active.DisconnectedAt = now.Add(-30 * time.Second)
	active.TimeoutMs = 60000 // 60 seconds

	if err := ds.PutDurableHandle(ctx, expired); err != nil {
		t.Fatalf("PutDurableHandle(expired) error: %v", err)
	}
	if err := ds.PutDurableHandle(ctx, active); err != nil {
		t.Fatalf("PutDurableHandle(active) error: %v", err)
	}

	// Delete expired
	count, err := ds.DeleteExpiredDurableHandles(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredDurableHandles() error: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 expired handle deleted, got %d", count)
	}

	// Verify expired is gone
	got, err := ds.GetDurableHandle(ctx, "expired-1")
	if err != nil {
		t.Fatalf("GetDurableHandle(expired) error: %v", err)
	}
	if got != nil {
		t.Fatalf("expired handle should be deleted, but found: %+v", got)
	}

	// Verify active is still there
	got, err = ds.GetDurableHandle(ctx, "active-1")
	if err != nil {
		t.Fatalf("GetDurableHandle(active) error: %v", err)
	}
	if got == nil {
		t.Fatalf("active handle should still exist")
	}
}

// testDurableDeleteExpiredKeepsActive tests that handles exactly at the boundary are handled correctly.
func testDurableDeleteExpiredKeepsActive(t *testing.T, factory StoreFactory) {
	ds := getDurableStore(t, factory)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)

	// No expired handles
	count, err := ds.DeleteExpiredDurableHandles(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredDurableHandles() error: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 expired handles on empty store, got %d", count)
	}

	// Create a handle that expires exactly at 'now' (boundary case)
	boundary := makeDurableHandle("boundary-1", "/export")
	boundary.DisconnectedAt = now.Add(-60 * time.Second)
	boundary.TimeoutMs = 60000 // exactly 60 seconds

	if err := ds.PutDurableHandle(ctx, boundary); err != nil {
		t.Fatalf("PutDurableHandle(boundary) error: %v", err)
	}

	// At exactly the boundary, the handle should be considered expired
	// (disconnected_at + timeout_ms <= now)
	count, err = ds.DeleteExpiredDurableHandles(ctx, now)
	if err != nil {
		t.Fatalf("DeleteExpiredDurableHandles() error: %v", err)
	}
	// Boundary handles should be expired (DisconnectedAt + TimeoutMs == now means expired)
	if count != 1 {
		t.Fatalf("expected boundary handle to be expired, got count %d", count)
	}
}
