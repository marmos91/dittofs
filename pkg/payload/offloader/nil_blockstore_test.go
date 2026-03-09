package offloader

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/cache"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/store/memory"
)

// newNilBlockStoreEnv creates a test environment with nil blockStore (local-only mode).
func newNilBlockStoreEnv(t *testing.T) (*Offloader, *cache.BlockCache, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := cache.New(tmpDir, 0, 0, ms)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	// nil blockStore = local-only mode
	m := New(bc, nil, ms, DefaultConfig())
	return m, bc, func() {
		m.Close()
	}
}

func TestNilBlockStoreNew(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	if m == nil {
		t.Fatal("expected non-nil offloader with nil blockStore")
	}
}

func TestNilBlockStoreFlush(t *testing.T) {
	m, bc, cleanup := newNilBlockStoreEnv(t)
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
		t.Fatalf("Flush with nil blockStore should not error, got: %v", err)
	}
	if result.Finalized {
		t.Error("Flush with nil blockStore should return Finalized=false")
	}
}

func TestNilBlockStoreGetFileSize(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	size, err := m.GetFileSize(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("GetFileSize with nil blockStore should return nil error, got: %v", err)
	}
	if size != 0 {
		t.Errorf("GetFileSize with nil blockStore should return 0, got: %d", size)
	}
}

func TestNilBlockStoreExists(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	exists, err := m.Exists(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Exists with nil blockStore should return nil error, got: %v", err)
	}
	if exists {
		t.Error("Exists with nil blockStore should return false")
	}
}

func TestNilBlockStoreTruncate(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.Truncate(ctx, "test/file.bin", 100)
	if err != nil {
		t.Fatalf("Truncate with nil blockStore should return nil, got: %v", err)
	}
}

func TestNilBlockStoreDelete(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.Delete(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Delete with nil blockStore should return nil, got: %v", err)
	}
}

func TestNilBlockStoreHealthCheck(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck with nil blockStore should return nil, got: %v", err)
	}
}

func TestNilBlockStoreStart(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()

	// Start should not panic with nil blockStore
	m.Start(context.Background())

	// Give it a moment to verify no goroutine panics
	time.Sleep(50 * time.Millisecond)
}

func TestSetRemoteStoreSuccess(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	bs := memory.New()
	defer bs.Close()

	err := m.SetRemoteStore(ctx, bs)
	if err != nil {
		t.Fatalf("SetRemoteStore should succeed, got: %v", err)
	}

	// Verify blockStore is now set by checking that operations work
	exists, err := m.Exists(ctx, "test/file.bin")
	if err != nil {
		t.Fatalf("Exists after SetRemoteStore should work, got: %v", err)
	}
	if exists {
		t.Error("Exists for non-existent file should return false")
	}
}

func TestSetRemoteStoreOneShot(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	bs := memory.New()
	defer bs.Close()

	if err := m.SetRemoteStore(ctx, bs); err != nil {
		t.Fatalf("first SetRemoteStore should succeed, got: %v", err)
	}

	bs2 := memory.New()
	defer bs2.Close()

	err := m.SetRemoteStore(ctx, bs2)
	if err == nil {
		t.Fatal("second SetRemoteStore should return error (one-shot)")
	}
}

func TestSetRemoteStoreOnClosed(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	ctx := context.Background()
	m.Start(ctx)

	// Close the offloader first
	cleanup()

	bs := memory.New()
	defer bs.Close()

	err := m.SetRemoteStore(ctx, bs)
	if err == nil {
		t.Fatal("SetRemoteStore on closed offloader should return error")
	}
}

func TestSetRemoteStoreNilArg(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()
	m.Start(ctx)

	err := m.SetRemoteStore(ctx, nil)
	if err == nil {
		t.Fatal("SetRemoteStore with nil blockStore should return error")
	}
}

func TestNilBlockStoreUploadPending(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	// uploadPendingBlocks should be a no-op with nil blockStore
	m.uploadPendingBlocks(ctx)
	// If we reach here without panic, test passes
}

func TestNilBlockStoreDownload(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	data, err := m.downloadBlock(ctx, "test/file.bin", 0)
	if err != nil {
		t.Fatalf("downloadBlock with nil blockStore should not error, got: %v", err)
	}
	if data != nil {
		t.Error("downloadBlock with nil blockStore should return nil data")
	}
}

func TestNilBlockStoreEnsureAvailable(t *testing.T) {
	m, _, cleanup := newNilBlockStoreEnv(t)
	defer cleanup()
	ctx := context.Background()

	err := m.EnsureAvailable(ctx, "test/file.bin", 0, 1024)
	if err != nil {
		t.Fatalf("EnsureAvailable with nil blockStore should return nil, got: %v", err)
	}
}
