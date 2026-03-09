package payload

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/offloader"
	storemem "github.com/marmos91/dittofs/pkg/payload/store/memory"
)

// newTestService creates a PayloadService for testing with in-memory stores.
func newTestService(t *testing.T) *PayloadService {
	t.Helper()

	tmpDir := t.TempDir()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := cache.New(tmpDir, 0, 0, metaStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	blockStore := storemem.New()
	tm := offloader.New(bc, blockStore, metaStore, offloader.DefaultConfig())

	svc, err := New(bc, tm)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() {
		_ = tm.Close()
		_ = bc.Close()
	})

	return svc
}

func TestPayloadService_New(t *testing.T) {
	tmpDir := t.TempDir()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := cache.New(tmpDir, 0, 0, metaStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	blockStore := storemem.New()
	tm := offloader.New(bc, blockStore, metaStore, offloader.DefaultConfig())

	svc, err := New(bc, tm)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if svc == nil {
		t.Fatal("New() returned nil")
	}
}

func TestPayloadService_New_NilCache(t *testing.T) {
	tmpDir := t.TempDir()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := cache.New(tmpDir, 0, 0, metaStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}
	blockStore := storemem.New()
	tm := offloader.New(bc, blockStore, metaStore, offloader.DefaultConfig())

	_, err = New(nil, tm)
	if err == nil {
		t.Error("New(nil, tm) should return error")
	}
}

func TestPayloadService_New_NilTransferManager(t *testing.T) {
	tmpDir := t.TempDir()
	metaStore := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	bc, err := cache.New(tmpDir, 0, 0, metaStore)
	if err != nil {
		t.Fatalf("cache.New() error = %v", err)
	}

	_, err = New(bc, nil)
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
	// Flush is decoupled from S3 upload — it only flushes memory to disk.
	// Finalized will be false for non-blocking flush.
	if result.Finalized {
		t.Error("Flush() Finalized = true, want false (decoupled from upload)")
	}
}

func TestPayloadService_GetStorageStats(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// Empty cache: no data written yet.
	stats, err := svc.GetStorageStats(ctx)
	if err != nil {
		t.Fatalf("GetStorageStats() error = %v", err)
	}
	if stats.UsedSize != 0 {
		t.Errorf("GetStorageStats() UsedSize = %d, want 0", stats.UsedSize)
	}
	if stats.ContentCount != 0 {
		t.Errorf("GetStorageStats() ContentCount = %d, want 0", stats.ContentCount)
	}

	// Write data to two separate files.
	file1 := metadata.PayloadID("file-1")
	data1 := []byte("hello world")
	if err := svc.WriteAt(ctx, file1, data1, 0); err != nil {
		t.Fatalf("WriteAt(file1) error = %v", err)
	}

	file2 := metadata.PayloadID("file-2")
	data2 := []byte("goodbye")
	if err := svc.WriteAt(ctx, file2, data2, 0); err != nil {
		t.Fatalf("WriteAt(file2) error = %v", err)
	}

	stats, err = svc.GetStorageStats(ctx)
	if err != nil {
		t.Fatalf("GetStorageStats() error = %v", err)
	}

	// UsedSize tracking is not yet implemented in the new BlockCache architecture.
	// ContentCount tracks the number of files currently cached.
	if stats.ContentCount != 2 {
		t.Errorf("GetStorageStats() ContentCount = %d, want 2", stats.ContentCount)
	}
}

func TestPayloadService_HealthCheck(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	if err := svc.HealthCheck(ctx); err != nil {
		t.Errorf("HealthCheck() error = %v", err)
	}
}

func TestPayloadService_ReadAt_SparseReturnsZeros(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("sparse-file")

	// Write at offset 1024, leaving 0..1023 sparse
	_ = svc.WriteAt(ctx, payloadID, []byte("data at offset"), 1024)

	// Read from the sparse region (offset 0)
	buf := make([]byte, 16)
	for i := range buf {
		buf[i] = 0xFF // pre-fill to verify zeroing
	}
	n, err := svc.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt() sparse region error = %v", err)
	}
	if n != 16 {
		t.Errorf("ReadAt() n = %d, want 16", n)
	}
	for i, b := range buf {
		if b != 0 {
			t.Fatalf("Expected zero at byte %d, got %d", i, b)
		}
	}
}

func TestPayloadService_ReadAtWithCOWSource(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	cowSource := metadata.PayloadID("cow-source")
	primary := metadata.PayloadID("primary")

	// Write data only to the COW source
	data := []byte("cow data here!")
	_ = svc.WriteAt(ctx, cowSource, data, 0)

	// Read from primary with COW fallback
	buf := make([]byte, len(data))
	n, err := svc.ReadAtWithCOWSource(ctx, primary, cowSource, buf, 0)
	if err != nil {
		t.Fatalf("ReadAtWithCOWSource() error = %v", err)
	}
	if n != len(data) {
		t.Errorf("ReadAtWithCOWSource() n = %d, want %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Errorf("ReadAtWithCOWSource() = %q, want %q", buf, data)
	}

	// Subsequent read from primary should hit cache (COW copied it)
	buf2 := make([]byte, len(data))
	_, err = svc.ReadAt(ctx, primary, buf2, 0)
	if err != nil {
		t.Fatalf("ReadAt() after COW error = %v", err)
	}
	if string(buf2) != string(data) {
		t.Errorf("ReadAt() after COW = %q, want %q", buf2, data)
	}
}

func TestPayloadService_Truncate(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("truncate-file")

	// Write data
	_ = svc.WriteAt(ctx, payloadID, []byte("hello world"), 0)

	// Truncate to 5 bytes
	if err := svc.Truncate(ctx, payloadID, 5); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}

	size, err := svc.GetSize(ctx, payloadID)
	if err != nil {
		t.Fatalf("GetSize() after truncate error = %v", err)
	}
	if size != 5 {
		t.Errorf("GetSize() = %d, want 5", size)
	}
}

func TestPayloadService_Delete(t *testing.T) {
	svc := newTestService(t)

	ctx := context.Background()
	payloadID := metadata.PayloadID("delete-file")

	// Write data
	_ = svc.WriteAt(ctx, payloadID, []byte("hello"), 0)

	// Delete
	if err := svc.Delete(ctx, payloadID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	// Should no longer exist in cache
	exists, err := svc.Exists(ctx, payloadID)
	if err != nil {
		t.Fatalf("Exists() after delete error = %v", err)
	}
	if exists {
		t.Error("Exists() = true after delete, want false")
	}
}
