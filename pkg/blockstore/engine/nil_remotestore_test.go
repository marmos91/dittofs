package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/local"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newNilRemoteStoreEnv creates a test environment with nil remoteStore (local-only mode).
func newNilRemoteStoreEnv(t *testing.T) (*Syncer, local.LocalStore, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := fs.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("fs.New() error = %v", err)
	}
	// nil remoteStore = local-only mode
	m := NewSyncer(bc, nil, ms, DefaultConfig())
	return m, bc, func() {
		_ = m.Close()
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
	if err := bc.WriteAt(ctx, payloadID, data, 0); err != nil {
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

func TestSetRemoteStoreSuccess(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	bs := remotememory.New()
	defer func() { _ = bs.Close() }()

	err := m.SetRemoteStore(ctx, bs)
	if err != nil {
		t.Fatalf("SetRemoteStore should succeed, got: %v", err)
	}

	// Verify remoteStore is now set by checking that operations work
	exists, err := m.Exists(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Exists after SetRemoteStore should work, got: %v", err)
	}
	if exists {
		t.Error("Exists for non-existent file should return false")
	}
}

func TestSetRemoteStoreOneShot(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	bs := remotememory.New()
	defer func() { _ = bs.Close() }()

	if err := m.SetRemoteStore(ctx, bs); err != nil {
		t.Fatalf("first SetRemoteStore should succeed, got: %v", err)
	}

	bs2 := remotememory.New()
	defer func() { _ = bs2.Close() }()

	err := m.SetRemoteStore(ctx, bs2)
	if err == nil {
		t.Fatal("second SetRemoteStore should return error (one-shot)")
	}
}

func TestSetRemoteStoreOnClosed(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	ctx := context.Background()
	m.Start(ctx)

	// Close the syncer first
	cleanup()

	bs := remotememory.New()
	defer func() { _ = bs.Close() }()

	err := m.SetRemoteStore(ctx, bs)
	if err == nil {
		t.Fatal("SetRemoteStore on closed syncer should return error")
	}
}

func TestSetRemoteStoreNilArg(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	err := m.SetRemoteStore(ctx, nil)
	if err == nil {
		t.Fatal("SetRemoteStore with nil remoteStore should return error")
	}
}

func TestNilRemoteStoreSyncLocalBlocks(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	// syncLocalBlocks should be a no-op with nil remoteStore
	m.syncLocalBlocks(ctx)
	// If we reach here without panic, test passes
}

func TestNilRemoteStoreFetchBlock(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	data, err := m.fetchBlock(ctx, "test/file.bin", 0)
	if err != nil {
		t.Fatalf("fetchBlock with nil remoteStore should not error, got: %v", err)
	}
	if data != nil {
		t.Error("fetchBlock with nil remoteStore should return nil data")
	}
}

func TestNilRemoteStoreEnsureAvailable(t *testing.T) {
	m, _, cleanup := newNilRemoteStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.EnsureAvailable(ctx, "test/file.bin", 0, 1024)
	if err != nil {
		t.Fatalf("EnsureAvailable with nil remoteStore should return nil, got: %v", err)
	}
}
