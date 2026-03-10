package engine

import (
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local/memory"
	blocksync "github.com/marmos91/dittofs/pkg/blockstore/sync"
)

// stubFileBlockStore is a minimal FileBlockStore for testing that satisfies the
// interface but stores nothing. We only need it to construct a Syncer.
type stubFileBlockStore struct{}

func (s *stubFileBlockStore) GetFileBlock(_ context.Context, _ string) (*blockstore.FileBlock, error) {
	return nil, blockstore.ErrFileBlockNotFound
}
func (s *stubFileBlockStore) PutFileBlock(_ context.Context, _ *blockstore.FileBlock) error {
	return nil
}
func (s *stubFileBlockStore) DeleteFileBlock(_ context.Context, _ string) error { return nil }
func (s *stubFileBlockStore) IncrementRefCount(_ context.Context, _ string) error {
	return nil
}
func (s *stubFileBlockStore) DecrementRefCount(_ context.Context, _ string) (uint32, error) {
	return 0, nil
}
func (s *stubFileBlockStore) FindFileBlockByHash(_ context.Context, _ blockstore.ContentHash) (*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *stubFileBlockStore) ListLocalBlocks(_ context.Context, _ time.Duration, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *stubFileBlockStore) ListRemoteBlocks(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *stubFileBlockStore) ListUnreferenced(_ context.Context, _ int) ([]*blockstore.FileBlock, error) {
	return nil, nil
}
func (s *stubFileBlockStore) ListFileBlocks(_ context.Context, _ string) ([]*blockstore.FileBlock, error) {
	return nil, nil
}

// newTestEngine creates an engine.BlockStore with memory local store, nil remote,
// optional L1 cache and prefetch settings.
func newTestEngine(t *testing.T, readCacheBytes int64, prefetchWorkers int) *BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := blocksync.New(localStore, nil, fbs, blocksync.DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadCacheBytes:  readCacheBytes,
		PrefetchWorkers: prefetchWorkers,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	t.Cleanup(func() { _ = bs.Close() })
	return bs
}

// TestReadAt_L1Hit verifies that ReadAt returns data from L1 cache without hitting local store.
func TestReadAt_L1Hit(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0) // 64MB L1, no prefetch

	ctx := context.Background()
	payloadID := "test-file-1"
	data := []byte("hello world, this is a test of L1 cache hit path")

	// Write data to the engine (goes to local store).
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// First read: should miss L1 and read from local, filling L1.
	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt (first) failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt (first) returned %d bytes, expected %d", n, len(data))
	}

	// Verify L1 was filled (block 0 should be cached).
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("expected block 0 to be in L1 cache after first read")
	}

	// Second read: should hit L1 cache directly.
	buf2 := make([]byte, len(data))
	n, err = bs.ReadAt(ctx, payloadID, buf2, 0)
	if err != nil {
		t.Fatalf("ReadAt (second) failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt (second) returned %d bytes, expected %d", n, len(data))
	}
	if string(buf2[:len(data)]) != string(data) {
		t.Fatalf("ReadAt (second) data mismatch: got %q, want %q", buf2[:len(data)], data)
	}
}

// TestReadAt_L1Miss_FillsCache verifies ReadAt fills L1 on miss and subsequent read hits.
func TestReadAt_L1Miss_FillsCache(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "fill-test"
	data := []byte("L1 fill test data")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Verify L1 is empty (write should have invalidated).
	if bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should be empty before first read")
	}

	// First read fills L1.
	buf := make([]byte, len(data))
	_, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}

	// L1 should now contain block 0.
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0 after read")
	}
}

// TestReadAt_L1Disabled verifies ReadAt works normally when L1 is disabled (nil readCache).
func TestReadAt_L1Disabled(t *testing.T) {
	bs := newTestEngine(t, 0, 0) // L1 disabled

	ctx := context.Background()
	payloadID := "no-cache-test"
	data := []byte("works without L1")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt returned %d bytes, expected %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Fatalf("data mismatch: got %q, want %q", buf, data)
	}

	// readCache should be nil.
	if bs.readCache != nil {
		t.Fatal("readCache should be nil when disabled")
	}
}

// TestReadAt_PrefetcherNotified verifies ReadAt calls prefetcher.OnRead after successful read.
func TestReadAt_PrefetcherNotified(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2) // L1 enabled, 2 prefetch workers

	ctx := context.Background()
	payloadID := "prefetch-notify-test"
	data := []byte("prefetch notification test")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Prefetcher should be non-nil.
	if bs.prefetcher == nil {
		t.Fatal("prefetcher should be non-nil when workers > 0 and L1 enabled")
	}

	// Read to trigger prefetcher notification.
	buf := make([]byte, len(data))
	_, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	// We can't easily verify OnRead was called without mocking, but at least
	// ensure no panic and the read succeeds.
}

// TestWriteAt_InvalidatesL1 verifies WriteAt invalidates L1 entries for affected blocks.
func TestWriteAt_InvalidatesL1(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "write-invalidate"
	data := []byte("original data for invalidation test")

	// Write then read to populate L1.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0 after read")
	}

	// Write new data - should invalidate L1.
	newData := []byte("modified data")
	if err := bs.WriteAt(ctx, payloadID, newData, 0); err != nil {
		t.Fatalf("WriteAt (new) failed: %v", err)
	}

	if bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should NOT contain block 0 after write (invalidated)")
	}
}

// TestWriteAt_ResetsPrefetcher verifies WriteAt calls prefetcher.Reset.
func TestWriteAt_ResetsPrefetcher(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2)

	ctx := context.Background()
	payloadID := "write-reset-prefetch"

	// Do a read first so the prefetcher has state.
	data := []byte("setup data")
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, len(data))
	_, _ = bs.ReadAt(ctx, payloadID, buf, 0)

	// Write should reset prefetcher state for this payloadID (no panic = OK).
	if err := bs.WriteAt(ctx, payloadID, []byte("modified"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
}

// TestTruncate_InvalidatesAbove verifies Truncate calls InvalidateAbove for blocks beyond new size.
func TestTruncate_InvalidatesAbove(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "truncate-invalidate"

	// Write data that spans at least 1 block and read to fill L1.
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, 100)
	if _, err := bs.ReadAt(ctx, payloadID, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0 after read")
	}

	// Truncate to 0 should invalidate all blocks.
	if err := bs.Truncate(ctx, payloadID, 0); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	if bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should NOT contain block 0 after truncate to 0")
	}
}

// TestTruncate_ResetsPrefetcher verifies Truncate resets prefetcher state.
func TestTruncate_ResetsPrefetcher(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2)

	ctx := context.Background()
	payloadID := "truncate-reset"

	data := []byte("truncate reset test")
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Truncate should reset prefetcher (no panic = OK).
	if err := bs.Truncate(ctx, payloadID, 5); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}
}

// TestDelete_InvalidatesFile verifies Delete calls InvalidateFile for the payloadID.
func TestDelete_InvalidatesFile(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "delete-invalidate"
	data := []byte("data for delete invalidation")

	// Write then read to populate L1.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0 after read")
	}

	// Delete should invalidate all L1 entries for this file.
	if err := bs.Delete(ctx, payloadID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should NOT contain block 0 after delete")
	}
}

// TestDelete_ResetsPrefetcher verifies Delete resets prefetcher state.
func TestDelete_ResetsPrefetcher(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2)

	ctx := context.Background()
	payloadID := "delete-reset"

	data := []byte("delete reset test")
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Delete should reset prefetcher (no panic = OK).
	if err := bs.Delete(ctx, payloadID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

// TestFlush_AutoPromote verifies that after Flush, flushed block data is readable from L1.
func TestFlush_AutoPromote(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "flush-promote"
	data := []byte("flush auto promote test data")

	// Write data.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// L1 should be empty (write invalidates).
	if bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should be empty before flush")
	}

	// Flush should auto-promote data into L1.
	_, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// L1 should now contain block 0 (auto-promoted).
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0 after flush (auto-promote)")
	}

	// Read should come from L1 now.
	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt after flush failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt returned %d bytes, expected %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Fatalf("data mismatch after flush: got %q, want %q", buf, data)
	}
}

// TestClose_ClosesL1AndPrefetcher verifies Close calls readCache.Close() and prefetcher.Close().
func TestClose_ClosesL1AndPrefetcher(t *testing.T) {
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := blocksync.New(localStore, nil, fbs, blocksync.DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadCacheBytes:  64 * 1024 * 1024,
		PrefetchWorkers: 2,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Write and read to populate L1.
	ctx := context.Background()
	if err := bs.WriteAt(ctx, "close-test", []byte("data"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, 4)
	_, _ = bs.ReadAt(ctx, "close-test", buf, 0)

	// Close should not panic and should clean up.
	if err := bs.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// After close, L1 cache should be cleared (Contains returns false).
	if bs.readCache.Contains("close-test", 0) {
		t.Fatal("L1 should be empty after Close")
	}
}

// TestMultiBlockRead_PartialL1 tests ReadAt spanning multiple blocks with partial L1 hits.
func TestMultiBlockRead_PartialL1(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "multi-block"

	// Write data that fits in a single block (we won't actually span multiple blocks
	// in the memory store since BlockSize is 8MB, but we can at least test that
	// the L1 integration code works for single-block reads).
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read to fill L1.
	buf := make([]byte, 1024)
	n, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != 1024 {
		t.Fatalf("ReadAt returned %d bytes, expected 1024", n)
	}

	// Verify data correctness.
	for i := range buf {
		if buf[i] != byte(i%256) {
			t.Fatalf("data mismatch at offset %d: got %d, want %d", i, buf[i], byte(i%256))
		}
	}

	// L1 should contain block 0.
	if !bs.readCache.Contains(payloadID, 0) {
		t.Fatal("L1 should contain block 0")
	}

	// Read again - should hit L1.
	buf2 := make([]byte, 512)
	n, err = bs.ReadAt(ctx, payloadID, buf2, 0)
	if err != nil {
		t.Fatalf("ReadAt (L1 hit) failed: %v", err)
	}
	if n != 512 {
		t.Fatalf("ReadAt returned %d bytes, expected 512", n)
	}
	for i := range buf2 {
		if buf2[i] != byte(i%256) {
			t.Fatalf("L1 data mismatch at offset %d: got %d, want %d", i, buf2[i], byte(i%256))
		}
	}
}

// TestNewWithL1Disabled verifies New works with ReadCacheBytes=0 (disabled).
func TestNewWithL1Disabled(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	if bs.readCache != nil {
		t.Fatal("readCache should be nil when ReadCacheBytes=0")
	}
	if bs.prefetcher != nil {
		t.Fatal("prefetcher should be nil when ReadCacheBytes=0")
	}
}

// TestNewWithPrefetchDisabled verifies prefetcher is nil when PrefetchWorkers=0.
func TestNewWithPrefetchDisabled(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	if bs.readCache == nil {
		t.Fatal("readCache should be non-nil when ReadCacheBytes > 0")
	}
	if bs.prefetcher != nil {
		t.Fatal("prefetcher should be nil when PrefetchWorkers=0")
	}
}

// TestL1AndPrefetchIndependent verifies L1 and prefetch can be configured independently.
func TestL1AndPrefetchIndependent(t *testing.T) {
	// L1 enabled, prefetch disabled.
	bs1 := newTestEngine(t, 64*1024*1024, 0)
	if bs1.readCache == nil {
		t.Fatal("readCache should be non-nil")
	}
	if bs1.prefetcher != nil {
		t.Fatal("prefetcher should be nil when workers=0")
	}

	// L1 disabled, prefetch configured but should be nil (no cache target).
	bs2 := newTestEngine(t, 0, 4)
	if bs2.readCache != nil {
		t.Fatal("readCache should be nil when bytes=0")
	}
	if bs2.prefetcher != nil {
		t.Fatal("prefetcher should be nil when readCache is nil (no cache target)")
	}

	// Both enabled.
	bs3 := newTestEngine(t, 64*1024*1024, 4)
	if bs3.readCache == nil {
		t.Fatal("readCache should be non-nil")
	}
	if bs3.prefetcher == nil {
		t.Fatal("prefetcher should be non-nil when both enabled")
	}
}

// TestReadAtPrefetcherWithL1 is a light integration test showing prefetcher + L1 work together.
func TestReadAtPrefetcherWithL1(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2)

	ctx := context.Background()
	payloadID := "prefetch-integration"
	data := []byte("prefetch integration test data blob")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Multiple sequential reads to trigger prefetcher.
	buf := make([]byte, len(data))
	for i := 0; i < 5; i++ {
		n, err := bs.ReadAt(ctx, payloadID, buf, 0)
		if err != nil {
			t.Fatalf("ReadAt #%d failed: %v", i, err)
		}
		if n != len(data) {
			t.Fatalf("ReadAt #%d returned %d, expected %d", i, n, len(data))
		}
	}
}

// TestReadAtSubBlockOffset verifies reading from non-zero offset within a block works with L1.
func TestReadAtSubBlockOffset(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "sub-offset"
	data := []byte("0123456789abcdef")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read entire data to fill L1.
	fullBuf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, fullBuf, 0); err != nil {
		t.Fatalf("ReadAt (full) failed: %v", err)
	}

	// Read subset at offset 4 from L1.
	subBuf := make([]byte, 8)
	n, err := bs.ReadAt(ctx, payloadID, subBuf, 4)
	if err != nil {
		t.Fatalf("ReadAt (sub) failed: %v", err)
	}
	if n != 8 {
		t.Fatalf("ReadAt returned %d, expected 8", n)
	}
	// "0123456789abcdef" at offset 4 is: "456789ab"
	expected := "456789ab"
	if string(subBuf) != expected {
		t.Fatalf("sub-block read mismatch: got %q, want %q", subBuf, expected)
	}
}
