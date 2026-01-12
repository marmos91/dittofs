package content

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	"github.com/marmos91/dittofs/pkg/metadata"
)

func TestContentService_SliceCache_WriteAndRead(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	if err := svc.RegisterCacheForShare(shareName, sc); err != nil {
		t.Fatalf("RegisterSliceCacheForShare failed: %v", err)
	}

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-1")
	data := []byte("hello world")

	// Write data
	err := svc.WriteAt(ctx, shareName, contentID, data, 0)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read it back
	buf := make([]byte, len(data))
	n, err := svc.ReadAt(ctx, shareName, contentID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("ReadAt: got %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("ReadAt: got %q, want %q", buf, data)
	}
}

func TestContentService_SliceCache_WriteAtOffset(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-2")

	// Write at offset 100
	data := []byte("hello")
	err := svc.WriteAt(ctx, shareName, contentID, data, 100)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read from 0 to 110
	buf := make([]byte, 110)
	n, err := svc.ReadAt(ctx, shareName, contentID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 110 {
		t.Errorf("ReadAt: got %d bytes, want 110", n)
	}

	// First 100 bytes should be zeros
	for i := 0; i < 100; i++ {
		if buf[i] != 0 {
			t.Errorf("buf[%d] = %d, want 0", i, buf[i])
		}
	}

	// Bytes 100-104 should be "hello"
	if !bytes.Equal(buf[100:105], []byte("hello")) {
		t.Errorf("buf[100:105] = %q, want %q", buf[100:105], "hello")
	}
}

func TestContentService_SliceCache_MultipleWrites(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-3")

	// Write "AAAA" at offset 0
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("AAAA"), 0)

	// Overwrite "BB" at offset 1 (newest wins)
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("BB"), 1)

	// Read result
	buf := make([]byte, 4)
	_, _ = svc.ReadAt(ctx, shareName, contentID, buf, 0)

	expected := []byte("ABBA")
	if !bytes.Equal(buf, expected) {
		t.Errorf("got %q, want %q", buf, expected)
	}
}

func TestContentService_SliceCache_GetContentSize(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-4")

	// Initial size should be 0
	size, err := svc.GetContentSize(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("GetContentSize failed: %v", err)
	}
	if size != 0 {
		t.Errorf("Initial size: got %d, want 0", size)
	}

	// Write 100 bytes at offset 50
	data := make([]byte, 100)
	_ = svc.WriteAt(ctx, shareName, contentID, data, 50)

	// Size should be 150 (0-149)
	size, err = svc.GetContentSize(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("GetContentSize failed: %v", err)
	}
	if size != 150 {
		t.Errorf("After write: got %d, want 150", size)
	}
}

func TestContentService_SliceCache_ContentExists(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-5")

	// Should not exist initially
	exists, err := svc.ContentExists(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("ContentExists failed: %v", err)
	}
	if exists {
		t.Error("Expected file to not exist initially")
	}

	// Write some data
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("test"), 0)

	// Should exist now
	exists, err = svc.ContentExists(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("ContentExists failed: %v", err)
	}
	if !exists {
		t.Error("Expected file to exist after write")
	}
}

func TestContentService_SliceCache_Delete(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-6")

	// Write some data
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("test"), 0)

	// Verify it exists
	exists, _ := svc.ContentExists(ctx, shareName, contentID)
	if !exists {
		t.Fatal("Expected file to exist")
	}

	// Delete it
	err := svc.Delete(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Should not exist now
	exists, _ = svc.ContentExists(ctx, shareName, contentID)
	if exists {
		t.Error("Expected file to not exist after delete")
	}
}

func TestContentService_SliceCache_Flush(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-7")

	// Write some data
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("test"), 0)

	// Flush (Phase 1: no-op, but should succeed)
	result, err := svc.Flush(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if !result.AlreadyFlushed {
		t.Error("Expected AlreadyFlushed=true for Phase 1")
	}
	if !result.Finalized {
		t.Error("Expected Finalized=true for Phase 1")
	}

	// Data should still be readable
	buf := make([]byte, 4)
	_, err = svc.ReadAt(ctx, shareName, contentID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt after flush failed: %v", err)
	}
	if !bytes.Equal(buf, []byte("test")) {
		t.Errorf("Data after flush: got %q, want %q", buf, "test")
	}
}

func TestContentService_SliceCache_FlushAndFinalize(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-8")

	// Write some data
	_ = svc.WriteAt(ctx, shareName, contentID, []byte("test"), 0)

	// FlushAndFinalize (Phase 1: no-op, but should succeed)
	result, err := svc.FlushAndFinalize(ctx, shareName, contentID)
	if err != nil {
		t.Fatalf("FlushAndFinalize failed: %v", err)
	}
	if !result.Finalized {
		t.Error("Expected Finalized=true for Phase 1")
	}
}

func TestContentService_SliceCache_CrossChunkWrite(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"
	_ = svc.RegisterCacheForShare(shareName, sc)

	ctx := context.Background()
	contentID := metadata.ContentID("test-file-9")

	// Write data that spans chunk boundary (at 64MB - 5 bytes)
	offset := uint64(cache.ChunkSize - 5)
	data := []byte("0123456789") // 10 bytes: 5 in chunk 0, 5 in chunk 1

	err := svc.WriteAt(ctx, shareName, contentID, data, offset)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read it back
	buf := make([]byte, 10)
	n, err := svc.ReadAt(ctx, shareName, contentID, buf, offset)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 10 {
		t.Errorf("ReadAt: got %d bytes, want 10", n)
	}
	if !bytes.Equal(buf, data) {
		t.Errorf("ReadAt: got %q, want %q", buf, data)
	}

	// Verify file size
	size, _ := svc.GetContentSize(ctx, shareName, contentID)
	expectedSize := offset + uint64(len(data))
	if size != expectedSize {
		t.Errorf("Size: got %d, want %d", size, expectedSize)
	}
}

func TestContentService_SliceCache_HasCache(t *testing.T) {
	svc := New()
	sc := cache.New(0)
	defer sc.Close()

	shareName := "/export"

	// Initially no slice cache
	if svc.HasCache(shareName) {
		t.Error("Expected HasCache=false initially")
	}

	// Register slice cache
	_ = svc.RegisterCacheForShare(shareName, sc)

	// Now should have slice cache
	if !svc.HasCache(shareName) {
		t.Error("Expected HasCache=true after registration")
	}
}
