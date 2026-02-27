package io

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/payload/chunk"
)

// ============================================================================
// Mock implementations
// ============================================================================

// mockCacheReader implements CacheReader for testing.
type mockCacheReader struct {
	// readAtFunc allows per-test customization of ReadAt behavior.
	readAtFunc func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error)
	// fileSizes maps payloadID -> (size, found)
	fileSizes map[string]uint64
	fileFound map[string]bool
}

func newMockCacheReader() *mockCacheReader {
	return &mockCacheReader{
		fileSizes: make(map[string]uint64),
		fileFound: make(map[string]bool),
	}
}

func (m *mockCacheReader) ReadAt(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
	if m.readAtFunc != nil {
		return m.readAtFunc(ctx, payloadID, chunkIdx, offset, length, dest)
	}
	return false, nil
}

func (m *mockCacheReader) GetFileSize(ctx context.Context, payloadID string) (uint64, bool) {
	if found, ok := m.fileFound[payloadID]; ok && found {
		return m.fileSizes[payloadID], true
	}
	return 0, false
}

// mockCacheWriter implements CacheWriter for testing.
type mockCacheWriter struct {
	written map[string][]byte
}

func newMockCacheWriter() *mockCacheWriter {
	return &mockCacheWriter{
		written: make(map[string][]byte),
	}
}

func (m *mockCacheWriter) WriteAt(ctx context.Context, payloadID string, chunkIdx uint32, data []byte, offset uint32) error {
	key := payloadID
	copied := make([]byte, len(data))
	copy(copied, data)
	m.written[key] = copied
	return nil
}

// mockCacheStateManager implements CacheStateManager for testing.
type mockCacheStateManager struct{}

func (m *mockCacheStateManager) Remove(ctx context.Context, payloadID string) error { return nil }
func (m *mockCacheStateManager) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	return nil
}

// mockBlockDownloader implements BlockDownloader for testing.
type mockBlockDownloader struct {
	ensureAvailableFunc func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error
}

func (m *mockBlockDownloader) EnsureAvailable(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
	if m.ensureAvailableFunc != nil {
		return m.ensureAvailableFunc(ctx, payloadID, chunkIdx, offset, length)
	}
	return nil
}

func (m *mockBlockDownloader) GetFileSize(ctx context.Context, payloadID string) (uint64, error) {
	return 0, nil
}

func (m *mockBlockDownloader) Exists(ctx context.Context, payloadID string) (bool, error) {
	return false, nil
}

// mockBlockUploader implements BlockUploader for testing.
type mockBlockUploader struct{}

func (m *mockBlockUploader) OnWriteComplete(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) {
}
func (m *mockBlockUploader) Truncate(ctx context.Context, payloadID string, newSize uint64) error {
	return nil
}
func (m *mockBlockUploader) Delete(ctx context.Context, payloadID string) error {
	return nil
}

// ============================================================================
// ensureAndReadFromCache sparse tests
// ============================================================================

func TestEnsureAndReadFromCache_SparseBlock_ReturnsNil(t *testing.T) {
	// When EnsureAvailable succeeds but cache ReadAt returns found=false (sparse),
	// ensureAndReadFromCache should return nil (not error) and dest should be zeros.
	cr := newMockCacheReader()
	// Always return not found (sparse)
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		return false, nil
	}

	bd := &mockBlockDownloader{}

	svc := New(cr, newMockCacheWriter(), &mockCacheStateManager{}, bd, &mockBlockUploader{})

	blockRange := chunk.BlockRange{
		ChunkIndex: 0,
		BlockIndex: 0,
		Offset:     0,
		Length:     1024,
		BufOffset:  0,
	}

	dest := make([]byte, 1024)
	// Pre-fill with non-zero bytes to verify sparse handling explicitly clears the buffer
	for i := range dest {
		dest[i] = 0xFF
	}
	err := svc.ensureAndReadFromCache(context.Background(), "test-payload", blockRange, 0, dest)
	if err != nil {
		t.Fatalf("ensureAndReadFromCache should not error on sparse block, got: %v", err)
	}

	// Verify dest is all zeros (sparse data)
	for i := range dest {
		if dest[i] != 0 {
			t.Fatalf("Expected zero at byte %d, got %d", i, dest[i])
		}
	}
}

func TestEnsureAndReadFromCache_NormalBlock_ReturnsData(t *testing.T) {
	// When EnsureAvailable succeeds and cache ReadAt returns found=true with data,
	// ensureAndReadFromCache should return nil with data in dest.
	cr := newMockCacheReader()
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		for i := range dest {
			dest[i] = 0xAB
		}
		return true, nil
	}

	bd := &mockBlockDownloader{}
	svc := New(cr, newMockCacheWriter(), &mockCacheStateManager{}, bd, &mockBlockUploader{})

	blockRange := chunk.BlockRange{
		ChunkIndex: 0,
		BlockIndex: 0,
		Offset:     0,
		Length:     1024,
		BufOffset:  0,
	}

	dest := make([]byte, 1024)
	err := svc.ensureAndReadFromCache(context.Background(), "test-payload", blockRange, 0, dest)
	if err != nil {
		t.Fatalf("ensureAndReadFromCache failed: %v", err)
	}

	if dest[0] != 0xAB {
		t.Fatalf("Expected 0xAB at byte 0, got %d", dest[0])
	}
}

func TestEnsureAndReadFromCache_CacheError_Propagates(t *testing.T) {
	// When cache ReadAt returns an error, ensureAndReadFromCache should propagate it.
	cr := newMockCacheReader()
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		return false, context.DeadlineExceeded
	}

	bd := &mockBlockDownloader{}
	svc := New(cr, newMockCacheWriter(), &mockCacheStateManager{}, bd, &mockBlockUploader{})

	blockRange := chunk.BlockRange{
		ChunkIndex: 0,
		BlockIndex: 0,
		Offset:     0,
		Length:     1024,
		BufOffset:  0,
	}

	dest := make([]byte, 1024)
	err := svc.ensureAndReadFromCache(context.Background(), "test-payload", blockRange, 0, dest)
	if err == nil {
		t.Fatal("ensureAndReadFromCache should propagate cache read errors")
	}
}

// ============================================================================
// ReadAt end-to-end sparse tests
// ============================================================================

func TestReadAt_SparseBlock_ReturnsZeros(t *testing.T) {
	// End-to-end test: ReadAt with a cache miss on first read, downloader succeeds,
	// but cache still returns not-found (sparse). Result should be zeros, not error.
	callCount := 0
	cr := newMockCacheReader()
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		callCount++
		// Always return not found - simulates sparse block where cache never stores data
		return false, nil
	}

	bd := &mockBlockDownloader{
		ensureAvailableFunc: func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
			return nil // Sparse-aware: succeeds without error
		},
	}

	svc := New(cr, newMockCacheWriter(), &mockCacheStateManager{}, bd, &mockBlockUploader{})

	data := make([]byte, 1024)
	// Pre-fill with non-zero bytes to verify sparse handling explicitly clears the buffer
	for i := range data {
		data[i] = 0xFF
	}
	n, err := svc.ReadAt(context.Background(), metadata.PayloadID("sparse-payload"), data, 0)
	if err != nil {
		t.Fatalf("ReadAt should not error on sparse file, got: %v", err)
	}
	if n != 1024 {
		t.Fatalf("Expected 1024 bytes read, got %d", n)
	}

	// Verify all zeros
	for i := range data {
		if data[i] != 0 {
			t.Fatalf("Expected zero at byte %d, got %d", i, data[i])
		}
	}
}

func TestReadAt_NormalBlock_CacheHit(t *testing.T) {
	// When cache has the data, ReadAt should return it directly without calling downloader.
	cr := newMockCacheReader()
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		for i := range dest {
			dest[i] = 0xCD
		}
		return true, nil
	}

	downloaderCalled := false
	bd := &mockBlockDownloader{
		ensureAvailableFunc: func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
			downloaderCalled = true
			return nil
		},
	}

	svc := New(cr, newMockCacheWriter(), &mockCacheStateManager{}, bd, &mockBlockUploader{})

	data := make([]byte, 512)
	n, err := svc.ReadAt(context.Background(), metadata.PayloadID("cached-payload"), data, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 512 {
		t.Fatalf("Expected 512 bytes, got %d", n)
	}
	if data[0] != 0xCD {
		t.Fatalf("Expected 0xCD, got %d", data[0])
	}
	if downloaderCalled {
		t.Fatal("Downloader should not be called on cache hit")
	}
}

func TestReadAt_EmptyBuffer_ReturnsZero(t *testing.T) {
	// ReadAt with empty buffer should return 0 bytes immediately.
	svc := New(newMockCacheReader(), newMockCacheWriter(), &mockCacheStateManager{}, &mockBlockDownloader{}, &mockBlockUploader{})

	data := make([]byte, 0)
	n, err := svc.ReadAt(context.Background(), metadata.PayloadID("any"), data, 0)
	if err != nil {
		t.Fatalf("ReadAt with empty buffer should not error, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("Expected 0 bytes, got %d", n)
	}
}

// ============================================================================
// readFromCOWSource sparse tests
// ============================================================================

func TestReadAtWithCOWSource_SparseBlock_ReturnsZeros(t *testing.T) {
	// COW source reads with sparse blocks should return zeros, not error.
	cr := newMockCacheReader()
	cr.readAtFunc = func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32, dest []byte) (bool, error) {
		return false, nil // Always not found (sparse in both primary and COW)
	}

	bd := &mockBlockDownloader{
		ensureAvailableFunc: func(ctx context.Context, payloadID string, chunkIdx uint32, offset, length uint32) error {
			return nil // Sparse-aware
		},
	}

	cw := newMockCacheWriter()
	svc := New(cr, cw, &mockCacheStateManager{}, bd, &mockBlockUploader{})

	data := make([]byte, 1024)
	// Pre-fill with non-zero bytes to verify sparse handling explicitly clears the buffer
	for i := range data {
		data[i] = 0xFF
	}
	n, err := svc.ReadAtWithCOWSource(
		context.Background(),
		metadata.PayloadID("primary"),
		metadata.PayloadID("cow-source"),
		data,
		0,
	)
	if err != nil {
		t.Fatalf("ReadAtWithCOWSource should not error on sparse COW source, got: %v", err)
	}
	if n != 1024 {
		t.Fatalf("Expected 1024 bytes, got %d", n)
	}

	for i := range data {
		if data[i] != 0 {
			t.Fatalf("Expected zero at byte %d, got %d", i, data[i])
		}
	}
}
