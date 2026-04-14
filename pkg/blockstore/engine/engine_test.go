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
// optional read buffer and prefetch settings.
func newTestEngine(t *testing.T, readBufferBytes int64, prefetchWorkers int) *BlockStore {
	t.Helper()
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := blocksync.New(localStore, nil, fbs, blocksync.DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadBufferBytes: readBufferBytes,
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

// TestReadAt_ReadBufferHit verifies that ReadAt returns data from read buffer without hitting local store.
func TestReadAt_ReadBufferHit(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0) // 64MB read buffer, no prefetch

	ctx := context.Background()
	payloadID := "test-file-1"
	data := []byte("hello world, this is a test of read buffer hit path")

	// Write data to the engine (goes to local store).
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// First read: should miss read buffer and read from local, filling read buffer.
	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt (first) failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt (first) returned %d bytes, expected %d", n, len(data))
	}

	// Verify read buffer was filled (block 0 should be buffered).
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("expected block 0 to be in read buffer after first read")
	}

	// Second read: should hit read buffer directly.
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

// TestReadAt_ReadBufferMiss_FillsBuffer verifies ReadAt fills read buffer on miss and subsequent read hits.
func TestReadAt_ReadBufferMiss_FillsBuffer(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "fill-test"
	data := []byte("read buffer fill test data")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Verify read buffer is empty (write should have invalidated).
	if bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should be empty before first read")
	}

	// First read fills read buffer.
	buf := make([]byte, len(data))
	_, err := bs.ReadAt(ctx, payloadID, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}

	// Read buffer should now contain block 0.
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0 after read")
	}
}

// TestReadAt_ReadBufferDisabled verifies ReadAt works normally when read buffer is disabled (nil readBuffer).
func TestReadAt_ReadBufferDisabled(t *testing.T) {
	bs := newTestEngine(t, 0, 0) // read buffer disabled

	ctx := context.Background()
	payloadID := "no-cache-test"
	data := []byte("works without read buffer")

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

	// readBuffer should be nil.
	if bs.readBuffer != nil {
		t.Fatal("readBuffer should be nil when disabled")
	}
}

// TestReadAt_PrefetcherNotified verifies ReadAt calls prefetcher.OnRead after successful read.
func TestReadAt_PrefetcherNotified(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 2) // read buffer enabled, 2 prefetch workers

	ctx := context.Background()
	payloadID := "prefetch-notify-test"
	data := []byte("prefetch notification test")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Prefetcher should be non-nil.
	if bs.prefetcher == nil {
		t.Fatal("prefetcher should be non-nil when workers > 0 and read buffer enabled")
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

// TestWriteAt_InvalidatesReadBuffer verifies WriteAt invalidates read buffer entries for affected blocks.
func TestWriteAt_InvalidatesReadBuffer(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "write-invalidate"
	data := []byte("original data for invalidation test")

	// Write then read to populate read buffer.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0 after read")
	}

	// Write new data - should invalidate read buffer.
	newData := []byte("modified data")
	if err := bs.WriteAt(ctx, payloadID, newData, 0); err != nil {
		t.Fatalf("WriteAt (new) failed: %v", err)
	}

	if bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should NOT contain block 0 after write (invalidated)")
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

	// Write data that spans at least 1 block and read to fill read buffer.
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
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0 after read")
	}

	// Truncate to 0 should invalidate all blocks.
	if err := bs.Truncate(ctx, payloadID, 0); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	if bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should NOT contain block 0 after truncate to 0")
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

	// Write then read to populate read buffer.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	buf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, buf, 0); err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0 after read")
	}

	// Delete should invalidate all read buffer entries for this file.
	if err := bs.Delete(ctx, payloadID); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	if bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should NOT contain block 0 after delete")
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

// TestFlush_AutoPromote verifies that after Flush, flushed block data is readable from read buffer.
func TestFlush_AutoPromote(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "flush-promote"
	data := []byte("flush auto promote test data")

	// Write data.
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read buffer should be empty (write invalidates).
	if bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should be empty before flush")
	}

	// Flush should auto-promote data into read buffer.
	_, err := bs.Flush(ctx, payloadID)
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Read buffer should now contain block 0 (auto-promoted).
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0 after flush (auto-promote)")
	}

	// Read should come from read buffer now.
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

// TestClose_ClosesReadBufferAndPrefetcher verifies Close calls readBuffer.Close() and prefetcher.Close().
func TestClose_ClosesReadBufferAndPrefetcher(t *testing.T) {
	localStore := memory.New()
	fbs := &stubFileBlockStore{}
	syncer := blocksync.New(localStore, nil, fbs, blocksync.DefaultConfig())

	bs, err := New(Config{
		Local:           localStore,
		Remote:          nil,
		Syncer:          syncer,
		ReadBufferBytes: 64 * 1024 * 1024,
		PrefetchWorkers: 2,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Write and read to populate read buffer.
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

	// After close, read buffer should be cleared (Contains returns false).
	if bs.readBuffer.Contains("close-test", 0) {
		t.Fatal("read buffer should be empty after Close")
	}
}

// TestMultiBlockRead_PartialReadBuffer tests ReadAt spanning multiple blocks with partial read buffer hits.
func TestMultiBlockRead_PartialReadBuffer(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "multi-block"

	// Write data that fits in a single block (we won't actually span multiple blocks
	// in the memory store since BlockSize is 8MB, but we can at least test that
	// the read buffer integration code works for single-block reads).
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read to fill read buffer.
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

	// Read buffer should contain block 0.
	if !bs.readBuffer.Contains(payloadID, 0) {
		t.Fatal("read buffer should contain block 0")
	}

	// Read again - should hit read buffer.
	buf2 := make([]byte, 512)
	n, err = bs.ReadAt(ctx, payloadID, buf2, 0)
	if err != nil {
		t.Fatalf("ReadAt (read buffer hit) failed: %v", err)
	}
	if n != 512 {
		t.Fatalf("ReadAt returned %d bytes, expected 512", n)
	}
	for i := range buf2 {
		if buf2[i] != byte(i%256) {
			t.Fatalf("read buffer data mismatch at offset %d: got %d, want %d", i, buf2[i], byte(i%256))
		}
	}
}

// TestNewWithReadBufferDisabled verifies New works with ReadBufferBytes=0 (disabled).
func TestNewWithReadBufferDisabled(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	if bs.readBuffer != nil {
		t.Fatal("readBuffer should be nil when ReadBufferBytes=0")
	}
	if bs.prefetcher != nil {
		t.Fatal("prefetcher should be nil when ReadBufferBytes=0")
	}
}

// TestNewWithPrefetchDisabled verifies prefetcher is nil when PrefetchWorkers=0.
func TestNewWithPrefetchDisabled(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)
	if bs.readBuffer == nil {
		t.Fatal("readBuffer should be non-nil when ReadBufferBytes > 0")
	}
	if bs.prefetcher != nil {
		t.Fatal("prefetcher should be nil when PrefetchWorkers=0")
	}
}

// TestReadBufferAndPrefetchIndependent verifies read buffer and prefetch can be configured independently.
func TestReadBufferAndPrefetchIndependent(t *testing.T) {
	// Read buffer enabled, prefetch disabled.
	bs1 := newTestEngine(t, 64*1024*1024, 0)
	if bs1.readBuffer == nil {
		t.Fatal("readBuffer should be non-nil")
	}
	if bs1.prefetcher != nil {
		t.Fatal("prefetcher should be nil when workers=0")
	}

	// Read buffer disabled, prefetch configured but should be nil (no buffer target).
	bs2 := newTestEngine(t, 0, 4)
	if bs2.readBuffer != nil {
		t.Fatal("readBuffer should be nil when bytes=0")
	}
	if bs2.prefetcher != nil {
		t.Fatal("prefetcher should be nil when readBuffer is nil (no buffer target)")
	}

	// Both enabled.
	bs3 := newTestEngine(t, 64*1024*1024, 4)
	if bs3.readBuffer == nil {
		t.Fatal("readBuffer should be non-nil")
	}
	if bs3.prefetcher == nil {
		t.Fatal("prefetcher should be non-nil when both enabled")
	}
}

// TestReadAtPrefetcherWithReadBuffer is a light integration test showing prefetcher + read buffer work together.
func TestReadAtPrefetcherWithReadBuffer(t *testing.T) {
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

// TestReadAtSubBlockOffset verifies reading from non-zero offset within a block works with read buffer.
func TestReadAtSubBlockOffset(t *testing.T) {
	bs := newTestEngine(t, 64*1024*1024, 0)

	ctx := context.Background()
	payloadID := "sub-offset"
	data := []byte("0123456789abcdef")

	if err := bs.WriteAt(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read entire data to fill read buffer.
	fullBuf := make([]byte, len(data))
	if _, err := bs.ReadAt(ctx, payloadID, fullBuf, 0); err != nil {
		t.Fatalf("ReadAt (full) failed: %v", err)
	}

	// Read subset at offset 4 from read buffer.
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

// TestCopyPayload_LocalOnly verifies CopyPayload duplicates data between payloads.
func TestCopyPayload_LocalOnly(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	srcPayload := "src-file"
	dstPayload := "dst-file"
	data := []byte("hello world, this is test data for copy payload")

	// Write source data
	if err := bs.WriteAt(ctx, srcPayload, data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Copy payload
	copied, err := bs.CopyPayload(ctx, srcPayload, dstPayload)
	if err != nil {
		t.Fatalf("CopyPayload failed: %v", err)
	}
	if copied != 1 {
		t.Fatalf("CopyPayload returned %d blocks, expected 1", copied)
	}

	// Read back from destination
	buf := make([]byte, len(data))
	n, err := bs.ReadAt(ctx, dstPayload, buf, 0)
	if err != nil {
		t.Fatalf("ReadAt on dest failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("ReadAt returned %d bytes, expected %d", n, len(data))
	}
	if string(buf) != string(data) {
		t.Fatalf("dest data = %q, want %q", buf, data)
	}

	// Verify source is unchanged
	srcBuf := make([]byte, len(data))
	_, err = bs.ReadAt(ctx, srcPayload, srcBuf, 0)
	if err != nil {
		t.Fatalf("ReadAt on src failed: %v", err)
	}
	if string(srcBuf) != string(data) {
		t.Fatalf("source data changed: got %q, want %q", srcBuf, data)
	}
}

// TestCopyPayload_EmptySource verifies CopyPayload handles empty source gracefully.
func TestCopyPayload_EmptySource(t *testing.T) {
	bs := newTestEngine(t, 0, 0)
	ctx := context.Background()

	copied, err := bs.CopyPayload(ctx, "nonexistent", "dst")
	if err != nil {
		t.Fatalf("CopyPayload should succeed for empty source, got: %v", err)
	}
	if copied != 0 {
		t.Fatalf("CopyPayload returned %d blocks, expected 0", copied)
	}
}
