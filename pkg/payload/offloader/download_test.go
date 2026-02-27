package offloader

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/cache"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/payload/store"
	"github.com/marmos91/dittofs/pkg/payload/store/memory"
)

// mockBlockStore wraps a memory store and allows injecting errors for specific keys.
type mockBlockStore struct {
	store.BlockStore
	inner     *memory.Store
	errorKeys map[string]error
	mu        sync.RWMutex
}

func newMockBlockStore() *mockBlockStore {
	return &mockBlockStore{
		inner:     memory.New(),
		errorKeys: make(map[string]error),
	}
}

func (m *mockBlockStore) ReadBlock(ctx context.Context, blockKey string) ([]byte, error) {
	m.mu.RLock()
	if err, ok := m.errorKeys[blockKey]; ok {
		m.mu.RUnlock()
		return nil, err
	}
	m.mu.RUnlock()
	return m.inner.ReadBlock(ctx, blockKey)
}

func (m *mockBlockStore) WriteBlock(ctx context.Context, blockKey string, data []byte) error {
	return m.inner.WriteBlock(ctx, blockKey, data)
}

func (m *mockBlockStore) ReadBlockRange(ctx context.Context, blockKey string, offset, length int64) ([]byte, error) {
	return m.inner.ReadBlockRange(ctx, blockKey, offset, length)
}

func (m *mockBlockStore) DeleteBlock(ctx context.Context, blockKey string) error {
	return m.inner.DeleteBlock(ctx, blockKey)
}

func (m *mockBlockStore) DeleteByPrefix(ctx context.Context, prefix string) error {
	return m.inner.DeleteByPrefix(ctx, prefix)
}

func (m *mockBlockStore) ListByPrefix(ctx context.Context, prefix string) ([]string, error) {
	return m.inner.ListByPrefix(ctx, prefix)
}

func (m *mockBlockStore) Close() error {
	return m.inner.Close()
}

func (m *mockBlockStore) HealthCheck(ctx context.Context) error {
	return m.inner.HealthCheck(ctx)
}

func (m *mockBlockStore) setError(blockKey string, err error) {
	m.mu.Lock()
	m.errorKeys[blockKey] = err
	m.mu.Unlock()
}

// newTestOffloader creates an offloader with mock block store for unit testing.
func newTestOffloader(bs store.BlockStore) (*Offloader, *cache.Cache) {
	c := cache.New(0)
	os := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	cfg := DefaultConfig()
	cfg.PrefetchBlocks = 0 // Disable prefetch for predictable test behavior
	m := New(c, bs, os, cfg)
	m.Start(context.Background())
	return m, c
}

// ============================================================================
// downloadBlock sparse tests
// ============================================================================

func TestDownloadBlock_SparseBlock_SkipsCache(t *testing.T) {
	// When a block does not exist in the block store (ErrBlockNotFound),
	// downloadBlock should return nil without caching a zero block,
	// avoiding unbounded memory growth from sparse file reads.
	bs := newMockBlockStore()
	offloader, c := newTestOffloader(bs)
	defer func() { _ = offloader.Close() }()

	ctx := context.Background()
	payloadID := "export/sparse-file.bin"
	chunkIdx := uint32(0)
	blockIdx := uint32(0)

	// Do NOT write any block to the store — this simulates a sparse block.
	err := offloader.downloadBlock(ctx, payloadID, chunkIdx, blockIdx)
	if err != nil {
		t.Fatalf("downloadBlock should not error on sparse block, got: %v", err)
	}

	// Verify sparse block was NOT cached (avoids memory bloat)
	dest := make([]byte, BlockSize)
	found, err := c.ReadAt(ctx, payloadID, chunkIdx, 0, BlockSize, dest)
	if err != nil {
		t.Fatalf("ReadAt from cache failed: %v", err)
	}
	if found {
		t.Fatal("Sparse block should NOT be cached to avoid memory growth")
	}
}

func TestDownloadBlock_RealError_Propagates(t *testing.T) {
	// When the block store returns a non-ErrBlockNotFound error,
	// downloadBlock should propagate the error.
	bs := newMockBlockStore()
	offloader, _ := newTestOffloader(bs)
	defer func() { _ = offloader.Close() }()

	ctx := context.Background()
	payloadID := "export/error-file.bin"
	chunkIdx := uint32(0)
	blockIdx := uint32(0)

	// Inject a network-like error for this block
	blockKey := FormatBlockKey(payloadID, chunkIdx, blockIdx)
	networkErr := fmt.Errorf("connection refused")
	bs.setError(blockKey, networkErr)

	err := offloader.downloadBlock(ctx, payloadID, chunkIdx, blockIdx)
	if err == nil {
		t.Fatal("downloadBlock should return error for real block store failures")
	}

	// The error should be wrapped
	expected := fmt.Sprintf("download block %s: connection refused", blockKey)
	if err.Error() != expected {
		t.Fatalf("Expected error %q, got %q", expected, err.Error())
	}
}

func TestDownloadBlock_NormalBlock_CachesData(t *testing.T) {
	// When a block exists in the block store, downloadBlock should cache it as-is.
	bs := newMockBlockStore()
	offloader, c := newTestOffloader(bs)
	defer func() { _ = offloader.Close() }()

	ctx := context.Background()
	payloadID := "export/normal-file.bin"
	chunkIdx := uint32(0)
	blockIdx := uint32(0)

	// Write real data to the block store
	blockKey := FormatBlockKey(payloadID, chunkIdx, blockIdx)
	data := make([]byte, BlockSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bs.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	err := offloader.downloadBlock(ctx, payloadID, chunkIdx, blockIdx)
	if err != nil {
		t.Fatalf("downloadBlock failed: %v", err)
	}

	// Verify cached data matches original
	dest := make([]byte, BlockSize)
	found, err := c.ReadAt(ctx, payloadID, chunkIdx, 0, BlockSize, dest)
	if err != nil {
		t.Fatalf("ReadAt from cache failed: %v", err)
	}
	if !found {
		t.Fatal("Data should be in cache after downloadBlock")
	}

	for i := 0; i < len(data); i++ {
		if dest[i] != data[i] {
			t.Fatalf("Data mismatch at byte %d: got %d, want %d", i, dest[i], data[i])
		}
	}
}

// ============================================================================
// EnsureAvailable sparse tests
// ============================================================================

func TestEnsureAvailable_SparseBlock_ReturnsNil(t *testing.T) {
	// EnsureAvailable should succeed (return nil) when the block store
	// does not have the requested block (sparse scenario).
	bs := newMockBlockStore()
	offloader, _ := newTestOffloader(bs)
	defer func() { _ = offloader.Close() }()

	ctx := context.Background()
	payloadID := "export/sparse-ensure.bin"

	// No block written — sparse file
	err := offloader.EnsureAvailable(ctx, payloadID, 0, 0, BlockSize)
	if err != nil {
		t.Fatalf("EnsureAvailable should not error on sparse block, got: %v", err)
	}
}

func TestEnsureAvailable_SparseBlock_MultipleBlocks(t *testing.T) {
	// When requesting a range that spans multiple blocks and some are sparse,
	// EnsureAvailable should handle them all without error.
	bs := newMockBlockStore()
	offloader, c := newTestOffloader(bs)
	defer func() { _ = offloader.Close() }()

	ctx := context.Background()
	payloadID := "export/multi-sparse.bin"

	// Write block 0 but leave block 1 sparse
	blockKey0 := FormatBlockKey(payloadID, 0, 0)
	data := make([]byte, BlockSize)
	for i := range data {
		data[i] = 0xAB
	}
	if err := bs.WriteBlock(ctx, blockKey0, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Request two blocks (0 and 1)
	err := offloader.EnsureAvailable(ctx, payloadID, 0, 0, 2*BlockSize)
	if err != nil {
		t.Fatalf("EnsureAvailable with mixed sparse/real blocks failed: %v", err)
	}

	// Verify block 0 has real data
	dest0 := make([]byte, BlockSize)
	found, _ := c.ReadAt(ctx, payloadID, 0, 0, BlockSize, dest0)
	if !found {
		t.Fatal("Block 0 should be in cache")
	}
	if dest0[0] != 0xAB {
		t.Fatalf("Block 0 data mismatch: got %d, want %d", dest0[0], 0xAB)
	}

	// Verify block 1 (sparse) was NOT cached
	dest1 := make([]byte, BlockSize)
	found, _ = c.ReadAt(ctx, payloadID, 0, BlockSize, BlockSize, dest1)
	if found {
		t.Fatal("Block 1 (sparse) should NOT be cached to avoid memory growth")
	}
}
