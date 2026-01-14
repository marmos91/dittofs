package payload

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestPayloadService_New(t *testing.T) {
	svc := New()
	if svc == nil {
		t.Fatal("New() returned nil")
	}

	if svc.GetCache() != nil {
		t.Error("new service should have nil cache")
	}

	if svc.GetTransferManager() != nil {
		t.Error("new service should have nil transfer manager")
	}
}

func TestPayloadService_SetCache(t *testing.T) {
	svc := New()

	// Setting nil should fail
	if err := svc.SetCache(nil); err == nil {
		t.Error("SetCache(nil) should return error")
	}

	// Create a real cache
	c := cache.New(10 * 1024 * 1024) // 10MB

	if err := svc.SetCache(c); err != nil {
		t.Errorf("SetCache() error = %v", err)
	}

	if svc.GetCache() != c {
		t.Error("GetCache() returned wrong cache")
	}
}

func TestPayloadService_HasCache(t *testing.T) {
	svc := New()

	if svc.HasCache("any") {
		t.Error("HasCache() should return false without cache")
	}

	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	if !svc.HasCache("any") {
		t.Error("HasCache() should return true with cache")
	}
}

func TestPayloadService_NoCacheErrors(t *testing.T) {
	svc := New()
	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// All operations should return ErrNoCacheConfigured
	buf := make([]byte, 10)
	_, err := svc.ReadAt(ctx, "share", payloadID, buf, 0)
	if err != ErrNoCacheConfigured {
		t.Errorf("ReadAt() error = %v, want ErrNoCacheConfigured", err)
	}

	err = svc.WriteAt(ctx, "share", payloadID, []byte("data"), 0)
	if err != ErrNoCacheConfigured {
		t.Errorf("WriteAt() error = %v, want ErrNoCacheConfigured", err)
	}

	_, err = svc.GetContentSize(ctx, "share", payloadID)
	if err != ErrNoCacheConfigured {
		t.Errorf("GetContentSize() error = %v, want ErrNoCacheConfigured", err)
	}

	_, err = svc.ContentExists(ctx, "share", payloadID)
	if err != ErrNoCacheConfigured {
		t.Errorf("ContentExists() error = %v, want ErrNoCacheConfigured", err)
	}

	err = svc.Truncate(ctx, "share", payloadID, 0)
	if err != ErrNoCacheConfigured {
		t.Errorf("Truncate() error = %v, want ErrNoCacheConfigured", err)
	}

	err = svc.Delete(ctx, "share", payloadID)
	if err != ErrNoCacheConfigured {
		t.Errorf("Delete() error = %v, want ErrNoCacheConfigured", err)
	}

	_, err = svc.Flush(ctx, "share", payloadID)
	if err != ErrNoCacheConfigured {
		t.Errorf("Flush() error = %v, want ErrNoCacheConfigured", err)
	}

	_, err = svc.FlushAndFinalize(ctx, "share", payloadID)
	if err != ErrNoCacheConfigured {
		t.Errorf("FlushAndFinalize() error = %v, want ErrNoCacheConfigured", err)
	}

	_, err = svc.GetStorageStats(ctx, "share")
	if err != ErrNoCacheConfigured {
		t.Errorf("GetStorageStats() error = %v, want ErrNoCacheConfigured", err)
	}

	err = svc.Healthcheck(ctx, "share")
	if err != ErrNoCacheConfigured {
		t.Errorf("Healthcheck() error = %v, want ErrNoCacheConfigured", err)
	}
}

func TestPayloadService_WriteAndRead(t *testing.T) {
	svc := New()
	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")
	data := []byte("hello world")

	// Write data
	if err := svc.WriteAt(ctx, "share", payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt() error = %v", err)
	}

	// Read data back
	buf := make([]byte, len(data))
	n, err := svc.ReadAt(ctx, "share", payloadID, buf, 0)
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
	svc := New()
	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Writing empty data should be a no-op
	if err := svc.WriteAt(ctx, "share", payloadID, []byte{}, 0); err != nil {
		t.Errorf("WriteAt(empty) error = %v", err)
	}
}

func TestPayloadService_ReadEmpty(t *testing.T) {
	svc := New()
	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Reading with empty buffer should be a no-op
	n, err := svc.ReadAt(ctx, "share", payloadID, []byte{}, 0)
	if err != nil {
		t.Errorf("ReadAt(empty) error = %v", err)
	}
	if n != 0 {
		t.Errorf("ReadAt(empty) n = %d, want 0", n)
	}
}

func TestPayloadService_SupportsReadAt(t *testing.T) {
	svc := New()

	if svc.SupportsReadAt("share") {
		t.Error("SupportsReadAt() should return false without cache")
	}

	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	if !svc.SupportsReadAt("share") {
		t.Error("SupportsReadAt() should return true with cache")
	}
}

func TestPayloadService_FlushCacheOnly(t *testing.T) {
	svc := New()
	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Write some data
	_ = svc.WriteAt(ctx, "share", payloadID, []byte("test data"), 0)

	// Flush in cache-only mode
	result, err := svc.Flush(ctx, "share", payloadID)
	if err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if !result.AlreadyFlushed {
		t.Error("Flush() AlreadyFlushed = false, want true (cache-only mode)")
	}
	if !result.Finalized {
		t.Error("Flush() Finalized = false, want true")
	}
}

func TestPayloadService_FlushAndFinalizeCacheOnly(t *testing.T) {
	svc := New()
	c := cache.New(10 * 1024 * 1024)
	_ = svc.SetCache(c)

	ctx := context.Background()
	payloadID := metadata.PayloadID("test-file")

	// Write some data
	_ = svc.WriteAt(ctx, "share", payloadID, []byte("test data"), 0)

	// FlushAndFinalize in cache-only mode
	result, err := svc.FlushAndFinalize(ctx, "share", payloadID)
	if err != nil {
		t.Fatalf("FlushAndFinalize() error = %v", err)
	}
	if !result.Finalized {
		t.Error("FlushAndFinalize() Finalized = false, want true")
	}
}
