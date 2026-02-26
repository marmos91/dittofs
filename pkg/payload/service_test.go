package payload

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	storemem "github.com/marmos91/dittofs/pkg/payload/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/offloader"
)

// newTestService creates a PayloadService for testing with in-memory stores.
func newTestService(t *testing.T) *PayloadService {
	t.Helper()

	c := cache.New(10 * 1024 * 1024) // 10MB cache
	blockStore := storemem.New()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	tm := offloader.New(c, blockStore, metaStore, offloader.DefaultConfig())

	svc, err := New(c, tm)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return svc
}

func TestPayloadService_New(t *testing.T) {
	c := cache.New(10 * 1024 * 1024)
	blockStore := storemem.New()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	tm := offloader.New(c, blockStore, metaStore, offloader.DefaultConfig())

	svc, err := New(c, tm)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if svc == nil {
		t.Fatal("New() returned nil")
	}
}

func TestPayloadService_New_NilCache(t *testing.T) {
	blockStore := storemem.New()
	c := cache.New(10 * 1024 * 1024)
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	tm := offloader.New(c, blockStore, metaStore, offloader.DefaultConfig())

	_, err := New(nil, tm)
	if err == nil {
		t.Error("New(nil, tm) should return error")
	}
}

func TestPayloadService_New_NilTransferManager(t *testing.T) {
	c := cache.New(10 * 1024 * 1024)

	_, err := New(c, nil)
	if err == nil {
		t.Error("New(c, nil) should return error")
	}
}

func TestPayloadService_WriteAndRead(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")
	data := []byte("hello world")

	// Write data
	if err := svc.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt() error = %v", err)
	}

	// Read data back
	buf := make([]byte, len(data))
	n, err := svc.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt() error = %v", err)
	}
	if n != len(data) {
		t.Errorf("ReadAt() n = %d, want %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Errorf("ReadAt() = %q, want %q", buf, data)
	}
}

func TestPayloadService_WriteEmpty(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Writing empty data should be a no-op
	if err := svc.WriteAt(ctx, payloadID, []byte{}, 0); err != nil {
		t.Errorf("WriteAt(empty) error = %v", err)
	}
}

func TestPayloadService_ReadEmpty(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Reading with empty buffer should be a no-op
	n, err := svc.ReadAt(ctx, payloadID, []byte{}, 0)
	if err != nil {
		t.Errorf("ReadAt(empty) error = %v", err)
	}
	if n != 0 {
		t.Errorf("ReadAt(empty) n = %d, want 0", n)
	}
}

func TestPayloadService_GetSize(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Initially size should be 0
	size, err := svc.GetSize(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetSize() error = %v", err)
	}
	if size != 0 {
		t.Errorf("GetSize() = %d, want 0", size)
	}

	// Write some data
	data := []byte("hello world")
	_ = svc.WriteAt(ctx, payloadID, data, 0)

	// Size should now be data length
	size, err = svc.GetSize(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetSize() error = %v", err)
	}
	if size != uint64(len(data)) {
		t.Errorf("GetSize() = %d, want %d", size, len(data))
	}
}

func TestPayloadService_Exists(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Initially should not exist (no data written)
	exists, err := svc.Exists(ctx, payloadID)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if exists {
		t.Error("Exists() = true, want false for new file")
	}

	// Write some data
	_ = svc.WriteAt(ctx, payloadID, []byte("data"), 0)

	// Now should exist
	exists, err = svc.Exists(ctx, payloadID)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !exists {
		t.Error("Exists() = false, want true after write")
	}
}

func TestPayloadService_Flush(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Write some data
	_ = svc.WriteAt(ctx, payloadID, []byte("test data"), 0)

	// Flush (non-blocking - enqueues for background upload)
	result, err := svc.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if !result.Finalized {
		t.Error("Flush() Finalized = false, want true")
	}
}

func TestPayloadService_HealthCheck(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	if err := svc.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}
