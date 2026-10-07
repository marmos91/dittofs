package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block/journal"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newNilRemoteStoreEnv creates a test environment with nil remoteStore (local-only mode).
func newNilRemoteStoreEnv(t *testing.T) (*RemoteSync, journal.LocalStore, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := journal.Open(tmpDir, journal.Config{})
	if err != nil {
		t.Fatalf("journal.Open() error = %v", err)
	}
	// nil remoteStore = local-only mode
	m := NewRemoteSync(bc, nil, ms, DefaultConfig())
	return m, bc, func() {
		_ = m.Close()
		_ = bc.Close()
	}
}

func TestNilRemoteStoreNew(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	if m == nil {
		t.Fatal("expected non-nil syncer with nil remoteStore")
	}
}

func TestNilRemoteStoreFlush(t *testing.T) {
	m, bc, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	payloadID := "test/flush-local.bin"
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, journal.FileID(payloadID), 0, data); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	result, err := m.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush with nil remoteStore should not error, got: %v", err)
	}
	if result.Finalized {
		t.Error("Flush with nil remoteStore should return Finalized=false")
	}
}

func TestNilRemoteStoreGetFileSize(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	size, err := m.GetFileSize(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("GetFileSize with nil remoteStore should return nil error, got: %v", err)
	}
	if size != 0 {
		t.Errorf("GetFileSize with nil remoteStore should return 0, got: %d", size)
	}
}

func TestNilRemoteStoreExists(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	exists, err := m.Exists(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Exists with nil remoteStore should return nil error, got: %v", err)
	}
	if exists {
		t.Error("Exists with nil remoteStore should return false")
	}
}

func TestNilRemoteStoreTruncate(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.Truncate(ctx, "test/file.bin", 100)
	if err != nil {
		t.Fatalf("Truncate with nil remoteStore should return nil, got: %v", err)
	}
}

func TestNilRemoteStoreDelete(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.Delete(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Delete with nil remoteStore should return nil, got: %v", err)
	}
}

func TestNilRemoteStoreHealthCheck(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck with nil remoteStore should return nil, got: %v", err)
	}
}

func TestNilRemoteStoreStart(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()

	// Start should not panic with nil remoteStore
	m.Start(context.Background())

	// Give it a moment to verify no goroutine panics
	time.Sleep(50 * time.Millisecond)
}

func TestNilRemoteStoreSyncNow(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	// The explicit drain path should be a no-op (and not error) with nil
	// remoteStore.
	if err := m.SyncNow(ctx); err != nil {
		t.Fatalf("SyncNow with nil remoteStore should not error, got: %v", err)
	}
}

func TestNilRemoteStoreFetchBlock(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	if err := m.fetchBlock(ctx, "test/file.bin", 0); err != nil {
		t.Fatalf("fetchBlock with nil remoteStore should not error, got: %v", err)
	}
}
