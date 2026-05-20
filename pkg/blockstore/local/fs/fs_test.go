package fs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// countingFileBlockStore wraps a blockstore.EngineFileBlockStore and
// counts calls per method. Used by TestLocalWritePath_NoFileBlockStoreCall
// to assert that the local write hot path (WriteAt / flushBlock /
// tryDirectDiskWrite) and eviction no longer touch the FileBlockStore
// directly (TD-02d / D-19).
//
// Counters are atomic so tests may observe them without racing the background
// SyncFileBlocks goroutine that Start() launches.
//
// Phase 12 (META-03 / D-09): wrapped surface narrowed to the 6-method
// FileBlockStore plus the engine-internal GetFileBlock + ListFileBlocks
// kept on each backend struct (see blockstore.EngineFileBlockStore).
type countingFileBlockStore struct {
	inner blockstore.EngineFileBlockStore

	get               atomic.Int64 // GetFileBlock (engine-internal)
	put               atomic.Int64 // Put (was PutFileBlock)
	del               atomic.Int64 // Delete (was DeleteFileBlock)
	incrementRefCount atomic.Int64
	decrementRefCount atomic.Int64
	getByHash         atomic.Int64 // GetByHash (was FindFileBlockByHash)
	listPending       atomic.Int64 // ListPending (was ListLocalBlocks)
	listFileBlocks    atomic.Int64 // engine-internal
}

func newCountingFileBlockStore(inner blockstore.EngineFileBlockStore) *countingFileBlockStore {
	return &countingFileBlockStore{inner: inner}
}

func (c *countingFileBlockStore) GetFileBlock(ctx context.Context, id string) (*blockstore.FileBlock, error) {
	c.get.Add(1)
	return c.inner.GetFileBlock(ctx, id)
}

func (c *countingFileBlockStore) Put(ctx context.Context, block *blockstore.FileBlock) error {
	c.put.Add(1)
	return c.inner.Put(ctx, block)
}

func (c *countingFileBlockStore) Delete(ctx context.Context, id string) error {
	c.del.Add(1)
	return c.inner.Delete(ctx, id)
}

func (c *countingFileBlockStore) IncrementRefCount(ctx context.Context, id string) error {
	c.incrementRefCount.Add(1)
	return c.inner.IncrementRefCount(ctx, id)
}

func (c *countingFileBlockStore) DecrementRefCount(ctx context.Context, id string) (uint32, error) {
	c.decrementRefCount.Add(1)
	return c.inner.DecrementRefCount(ctx, id)
}

func (c *countingFileBlockStore) GetByHash(ctx context.Context, hash blockstore.ContentHash) (*blockstore.FileBlock, error) {
	c.getByHash.Add(1)
	return c.inner.GetByHash(ctx, hash)
}

func (c *countingFileBlockStore) ListPending(ctx context.Context, olderThan time.Duration, limit int) ([]*blockstore.FileBlock, error) {
	c.listPending.Add(1)
	return c.inner.ListPending(ctx, olderThan, limit)
}

func (c *countingFileBlockStore) ListFileBlocks(ctx context.Context, payloadID string) ([]*blockstore.FileBlock, error) {
	c.listFileBlocks.Add(1)
	return c.inner.ListFileBlocks(ctx, payloadID)
}

// snapshot captures the current call counts for comparison.
type fbsCallSnapshot struct {
	get, put, del, inc, dec, find, listPending, listFile int64
}

func (c *countingFileBlockStore) snapshot() fbsCallSnapshot {
	return fbsCallSnapshot{
		get:         c.get.Load(),
		put:         c.put.Load(),
		del:         c.del.Load(),
		inc:         c.incrementRefCount.Load(),
		dec:         c.decrementRefCount.Load(),
		find:        c.getByHash.Load(),
		listPending: c.listPending.Load(),
		listFile:    c.listFileBlocks.Load(),
	}
}

func diffSnapshot(before, after fbsCallSnapshot) fbsCallSnapshot {
	return fbsCallSnapshot{
		get:         after.get - before.get,
		put:         after.put - before.put,
		del:         after.del - before.del,
		inc:         after.inc - before.inc,
		dec:         after.dec - before.dec,
		find:        after.find - before.find,
		listPending: after.listPending - before.listPending,
		listFile:    after.listFile - before.listFile,
	}
}

// ResetCount and TotalCount satisfy the FBSCounter interface declared in
// test_hooks.go so the LSL-08 conformance suite can assert no
// FileBlockStore calls happen during ensureSpace.
func (c *countingFileBlockStore) ResetCount() {
	c.get.Store(0)
	c.put.Store(0)
	c.del.Store(0)
	c.incrementRefCount.Store(0)
	c.decrementRefCount.Store(0)
	c.getByHash.Store(0)
	c.listPending.Store(0)
	c.listFileBlocks.Store(0)
}

func (c *countingFileBlockStore) TotalCount() int {
	return int(c.get.Load() +
		c.put.Load() +
		c.del.Load() +
		c.incrementRefCount.Load() +
		c.decrementRefCount.Load() +
		c.getByHash.Load() +
		c.listPending.Load() +
		c.listFileBlocks.Load())
}

// newTestCache creates an FSStore with a temporary directory and in-memory block store.
func newTestCache(t *testing.T, maxMemory int64) *FSStore {
	t.Helper()
	dir := t.TempDir()
	blockStore := memory.NewMemoryMetadataStoreWithDefaults()
	bc, err := New(dir, 0, maxMemory, blockStore)
	if err != nil {
		t.Fatalf("failed to create local store: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bc.Start(ctx)
	t.Cleanup(func() {
		cancel()
		_ = bc.Close()
	})
	return bc
}

func TestWriteAndReadSimple(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte("hello"), 100)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}
}

func TestWriteFullBlock(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write exactly one full 8MB block
	data := bytes.Repeat([]byte{0xAB}, int(blockstore.BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Block should have been flushed to disk (memBlock stays but data=nil)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	mb := bc.getMemBlock(key)
	if mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Error("expected memBlock data to be nil after full block flush")
		}
	}

	// Should still be readable from disk
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt from disk failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt from disk returned local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt from disk returned wrong data")
	}
}

func TestMultiBlockWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write 20MB spanning 3 blocks (8MB each)
	size := 20 * 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Read back and verify
	dest := make([]byte, size)
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}

	// Check file size
	fileSize, ok := bc.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found")
	}
	if fileSize != uint64(size) {
		t.Fatalf("expected file size %d, got %d", size, fileSize)
	}
}

func TestFlushCallsFsync(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write partial block (won't auto-flush)
	data := bytes.Repeat([]byte{0xCD}, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// memBlock should exist before Flush
	key := blockKey{payloadID: "file1", blockIdx: 0}
	if mb := bc.getMemBlock(key); mb == nil {
		t.Error("expected memBlock to exist before Flush")
	}

	// Flush (NFS COMMIT path)
	if _, err := bc.Flush(ctx, "file1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// After flush, memBlock stays but data should be nil
	if mb := bc.getMemBlock(key); mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Error("expected memBlock data to be nil after Flush")
		}
	}

	// Data should still be readable from disk
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt after Flush failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after Flush returned local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt after Flush returned wrong data")
	}
}

func TestConcurrentWritesDifferentFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	const numFiles = 10
	const writeSize = 1024 * 1024 // 1MB per file

	var wg sync.WaitGroup
	errs := make([]error, numFiles)

	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payloadID := "file" + string(rune('A'+idx))
			data := bytes.Repeat([]byte{byte(idx)}, writeSize)
			errs[idx] = bc.WriteAt(ctx, payloadID, data, 0)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent write %d failed: %v", i, err)
		}
	}

	// Verify all files
	for i := 0; i < numFiles; i++ {
		payloadID := "file" + string(rune('A'+i))
		expected := bytes.Repeat([]byte{byte(i)}, writeSize)
		dest := make([]byte, writeSize)
		found, err := bc.ReadAt(ctx, payloadID, dest, 0)
		if err != nil {
			t.Fatalf("ReadAt file %d failed: %v", i, err)
		}
		if !found {
			t.Fatalf("ReadAt file %d local store miss", i)
		}
		if !bytes.Equal(dest, expected) {
			t.Fatalf("ReadAt file %d wrong data", i)
		}
	}
}

func TestConcurrentWritesSameFile(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	const numWriters = 8
	const writeSize = 4096 // 4KB each

	var wg sync.WaitGroup

	for i := 0; i < numWriters; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			offset := uint64(idx) * writeSize
			data := bytes.Repeat([]byte{byte(idx)}, writeSize)
			if err := bc.WriteAt(ctx, "file1", data, offset); err != nil {
				t.Errorf("concurrent write %d failed: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	// Each 4KB region should have the corresponding byte value
	for i := 0; i < numWriters; i++ {
		offset := uint64(i) * writeSize
		dest := make([]byte, writeSize)
		found, err := bc.ReadAt(ctx, "file1", dest, offset)
		if err != nil {
			t.Fatalf("ReadAt region %d failed: %v", i, err)
		}
		if !found {
			t.Fatalf("ReadAt region %d local store miss", i)
		}
		expected := bytes.Repeat([]byte{byte(i)}, writeSize)
		if !bytes.Equal(dest, expected) {
			t.Fatalf("ReadAt region %d wrong data (got %d, expected %d)", i, dest[0], byte(i))
		}
	}
}

func TestBackpressure(t *testing.T) {
	// Set very low memory budget (32MB = 4 blocks)
	bc := newTestCache(t, 32*1024*1024)
	ctx := context.Background()

	// Write 80MB (10 blocks) to trigger backpressure
	const totalSize = 80 * 1024 * 1024
	data := make([]byte, totalSize)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt with backpressure failed: %v", err)
	}

	// Memory should not exceed 2x budget (hard backpressure limit)
	if bc.memUsed.Load() > bc.maxMemory*2 {
		t.Fatalf("memory usage %d exceeds 2x budget %d", bc.memUsed.Load(), bc.maxMemory*2)
	}

	// Data should be fully readable
	dest := make([]byte, totalSize)
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt after backpressure failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after backpressure local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt after backpressure wrong data")
	}
}

func TestRemove(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xFF}, 4096)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := bc.EvictMemory(ctx, "file1"); err != nil {
		t.Fatalf("EvictMemory failed: %v", err)
	}

	_, ok := bc.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after Remove")
	}
}

func TestTruncate(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write 16MB (2 full blocks)
	data := bytes.Repeat([]byte{0xAA}, 16*1024*1024)
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Truncate to 4MB (block 1 should be purged)
	if err := bc.Truncate(ctx, "file1", 4*1024*1024); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	fileSize, ok := bc.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found after Truncate")
	}
	if fileSize != 4*1024*1024 {
		t.Fatalf("expected file size %d, got %d", 4*1024*1024, fileSize)
	}
}

func TestDirectDiskWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write a full block to get it flushed to disk
	data := bytes.Repeat([]byte{0xAB}, int(blockstore.BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// Verify the block was flushed (memBlock stays but data=nil)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	mb := bc.getMemBlock(key)
	if mb != nil {
		mb.mu.RLock()
		hasData := mb.data != nil
		mb.mu.RUnlock()
		if hasData {
			t.Fatal("expected memBlock data to be nil after full block write")
		}
	}

	// Now write a small partial update to the same block -- should go direct to disk
	patch := []byte("patched!")
	if err := bc.WriteAt(ctx, "file1", patch, 100); err != nil {
		t.Fatalf("direct disk write failed: %v", err)
	}

	// Verify the patch was written correctly
	dest := make([]byte, len(patch))
	found, err := bc.ReadAt(ctx, "file1", dest, 100)
	if err != nil {
		t.Fatalf("ReadAt after direct disk write failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after direct disk write returned local store miss")
	}
	if !bytes.Equal(dest, patch) {
		t.Fatalf("direct disk write wrong data: got %q, want %q", dest, patch)
	}
}

func TestListFiles(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := bc.WriteAt(ctx, id, []byte("data"), 0); err != nil {
			t.Fatalf("WriteAt %s failed: %v", id, err)
		}
	}

	files := bc.ListFiles()
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	got := make(map[string]bool)
	for _, f := range files {
		got[f] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !got[id] {
			t.Errorf("missing file %s", id)
		}
	}
}

func TestStats(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	if err := bc.WriteAt(ctx, "f1", bytes.Repeat([]byte{1}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := bc.WriteAt(ctx, "f2", bytes.Repeat([]byte{2}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats := bc.Stats()
	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MemBlockCount != 2 {
		t.Errorf("expected 2 memBlocks, got %d", stats.MemBlockCount)
	}
	if stats.MemUsed != 2*int64(blockstore.BlockSize) {
		t.Errorf("expected memUsed %d, got %d", 2*int64(blockstore.BlockSize), stats.MemUsed)
	}

	// After flushing, memBlocks should be 0
	if _, err := bc.Flush(ctx, "f1"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if _, err := bc.Flush(ctx, "f2"); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	stats = bc.Stats()
	if stats.MemBlockCount != 0 {
		t.Errorf("expected 0 memBlocks after flush, got %d", stats.MemBlockCount)
	}
}

func TestConcurrentFlushAndWrite(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Start writing in background
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			data := bytes.Repeat([]byte{byte(i)}, 4096)
			if err := bc.WriteAt(ctx, "file1", data, uint64(i)*4096); err != nil {
				t.Errorf("write %d failed: %v", i, err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := bc.Flush(ctx, "file1"); err != nil {
				t.Errorf("flush %d failed: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()
}

func TestNoFsyncOnBlockFill(t *testing.T) {
	// This test verifies that flushBlock (called when a block fills up during
	// writes) does NOT call fsync. The .blk file should exist but without
	// the durability guarantee of fsync (which is deferred to Flush/COMMIT).
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	// Write exactly one full block to trigger flushBlock
	data := bytes.Repeat([]byte{0xBB}, int(blockstore.BlockSize))
	if err := bc.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	// The .blk file should exist (written but not fsynced)
	key := blockKey{payloadID: "file1", blockIdx: 0}
	blockID := makeBlockID(key)
	path := bc.blockPath(blockID)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("expected .blk file to exist after block fill")
	}

	// Data should be correct
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data")
	}
}

func TestWriteFromRemote(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xCC}, 4096)
	if err := bc.WriteFromRemote(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteFromRemote failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt local store miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data")
	}
}

// TestWriteFromRemote_PreservesCASMetadata is the regression test for
// Pass-2 CR-2-01. WriteFromRemote MUST NOT clobber the canonical CAS
// metadata (Hash + BlockStoreKey) on the FileBlockStore row when the
// in-process diskIndex misses (the steady-state case after a server
// restart, or for any block this node never produced locally).
//
// Pre-fix bug: diskIndex miss -> NewFileBlock(blockID, "") with zero Hash,
// then BlockStoreKey was overwritten with the legacy "{payloadID}/block-N"
// format and queueFileBlockUpdate UPSERTed a row with Hash=zero. The next
// fetchBlock fell into the legacy path, hit the never-existing legacy key,
// and returned a sparse "zero" read. GC's mark phase then skipped the
// zero-hash row and reaped the live CAS object.
func TestWriteFromRemote_PreservesCASMetadata(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)
	ctx := context.Background()

	const payloadID = "file-cas"
	const blockIdx = uint64(0)

	// Seed the FileBlockStore with the canonical CAS row that the syncer
	// would have written at upload time: Hash=H, BlockStoreKey=cas/.../<hex>.
	data := bytes.Repeat([]byte{0xCC}, 4096)
	hash := hashBytes(data)
	casKey := blockstore.FormatCASKey(hash)
	blockID := makeBlockID(blockKey{payloadID: payloadID, blockIdx: blockIdx})

	row := blockstore.NewFileBlock(blockID, "")
	row.Hash = hash
	row.BlockStoreKey = casKey
	row.State = blockstore.BlockStateRemote
	row.DataSize = uint32(len(data))
	if err := bc.blockStore.Put(ctx, row); err != nil {
		t.Fatalf("seed PutFileBlock: %v", err)
	}

	// Simulate the post-restart state: the diskIndex is empty (no local
	// .blk file ever materialized for this block). WriteFromRemote MUST
	// fall back to the FileBlockStore lookup and preserve the CAS row.
	bc.diskIndex.Range(func(k, _ any) bool {
		bc.diskIndex.Delete(k)
		return true
	})

	if err := bc.WriteFromRemote(ctx, payloadID, data, 0); err != nil {
		t.Fatalf("WriteFromRemote failed: %v", err)
	}

	// Drain any queued FileBlock updates so the assertion below sees the
	// post-WriteFromRemote row, not the seeded one.
	bc.SyncFileBlocks(ctx)

	got, err := bc.blockStore.GetFileBlock(ctx, blockID)
	if err != nil {
		t.Fatalf("GetFileBlock after WriteFromRemote: %v", err)
	}
	if got.Hash != hash {
		t.Errorf("CR-2-01 regression: Hash clobbered\n  got:  %s\n  want: %s",
			got.Hash.String(), hash.String())
	}
	if got.BlockStoreKey != casKey {
		t.Errorf("CR-2-01 regression: BlockStoreKey clobbered\n  got:  %q\n  want: %q",
			got.BlockStoreKey, casKey)
	}
	if got.State != blockstore.BlockStateRemote {
		t.Errorf("State not Remote after WriteFromRemote: got %v", got.State)
	}
	if got.LocalPath == "" {
		t.Errorf("LocalPath empty after WriteFromRemote (cache file should be tracked)")
	}

	// Read-back: bytes must round-trip from the local cache.
	dest := make([]byte, len(data))
	found, err := bc.ReadAt(ctx, payloadID, dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt local store miss after WriteFromRemote")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data after WriteFromRemote")
	}
}

// TestFSStoreStartCloseNoGoroutineLeak verifies that FSStore.Close() joins the
// background goroutine launched by Start(), preventing a goroutine leak across
// repeated Start/Close cycles. Regression test for TD-02a.
//
// Uses a never-cancelled parent context so the ONLY termination signal
// available to the Start goroutine is Close() itself. Without the fix,
// goroutines accumulate linearly with the cycle count.
func TestFSStoreStartCloseNoGoroutineLeak(t *testing.T) {
	// Warm-up: allow any package-init goroutines to settle before measuring.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	const cycles = 20
	ctx := context.Background() // never cancelled — only Close may stop the goroutine
	for i := 0; i < cycles; i++ {
		dir := t.TempDir()
		blockStore := memory.NewMemoryMetadataStoreWithDefaults()
		bc, err := New(dir, 0, 256*1024*1024, blockStore)
		if err != nil {
			t.Fatalf("cycle %d: New failed: %v", i, err)
		}
		bc.Start(ctx)
		// Close must deterministically join the Start goroutine.
		if err := bc.Close(); err != nil {
			t.Fatalf("cycle %d: Close failed: %v", i, err)
		}
	}

	// Small settle window — Close() should have joined already; this accounts
	// only for scheduler jitter, not for goroutines still selecting on ticker.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()

	// A real leak accumulates linearly with cycles (20). A small tolerance
	// absorbs unrelated test-runner background goroutines.
	if delta := after - before; delta > 2 {
		t.Fatalf("goroutine leak: before=%d after=%d delta=%d (cycles=%d)", before, after, delta, cycles)
	}
}

func TestBlockPathSharding(t *testing.T) {
	bc := newTestCache(t, 256*1024*1024)

	// A blockID like "abc/0" should be sharded as "<baseDir>/ab/abc/0.blk"
	path := bc.blockPath("abc/0")
	expected := filepath.Join(bc.baseDir, "ab", "abc/0.blk")
	if path != expected {
		t.Errorf("blockPath wrong: got %s, want %s", path, expected)
	}
}

// TestLocalWritePath_NoFileBlockStoreCall enforces TD-02d / D-19: the local
// tier's write hot path and eviction must make zero calls into the
// FileBlockStore interface. Any lookup or list must come from on-disk state
// or an on-process index.
//
// The test wraps the backing FileBlockStore with a counter. Note: the Start()
// background goroutine periodically drains queued FileBlock metadata via
// PutFileBlock (SyncFileBlocks); that ASYNC drain is out of scope — the
// assertion is only about the synchronous hot-path/eviction call paths. We
// therefore disable Start() here and never invoke SyncFileBlocks, so any
// counter increment during the write or eviction section is a real hot-path
// regression rather than background noise.
func TestLocalWritePath_NoFileBlockStoreCall(t *testing.T) {
	t.Run("write_hot_path", func(t *testing.T) {
		dir := t.TempDir()
		inner := memory.NewMemoryMetadataStoreWithDefaults()
		counter := newCountingFileBlockStore(inner)

		bc, err := New(dir, 0, 256*1024*1024, counter)
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		// Intentionally do NOT call bc.Start: the async drain path legitimately
		// calls PutFileBlock. We want the assertion to cover only synchronous
		// hot-path behavior.
		t.Cleanup(func() { _ = bc.Close() })

		ctx := context.Background()
		before := counter.snapshot()

		// Representative writes exercising both the memBlock path and the
		// direct-disk (pwrite) path:
		//  1. Partial-block write -> fills memBlock, no disk.
		if err := bc.WriteAt(ctx, "file1", bytes.Repeat([]byte{0xAA}, 4096), 0); err != nil {
			t.Fatalf("WriteAt small failed: %v", err)
		}
		//  2. Full-block write -> triggers flushBlock (mem -> disk).
		full := bytes.Repeat([]byte{0xBB}, int(blockstore.BlockSize))
		if err := bc.WriteAt(ctx, "file2", full, 0); err != nil {
			t.Fatalf("WriteAt full failed: %v", err)
		}
		//  3. Partial write to the now-on-disk block -> tryDirectDiskWrite path
		//     (exercises the fd-pool pwrite branch).
		if err := bc.WriteAt(ctx, "file2", []byte("patch"), 100); err != nil {
			t.Fatalf("WriteAt direct-disk failed: %v", err)
		}
		//  4. Explicit Flush (NFS COMMIT) -> exercises flushBlock on file1.
		if _, err := bc.Flush(ctx, "file1"); err != nil {
			t.Fatalf("Flush failed: %v", err)
		}

		after := counter.snapshot()
		delta := diffSnapshot(before, after)
		if delta != (fbsCallSnapshot{}) {
			t.Errorf("write hot path called FileBlockStore: %+v", delta)
		}
	})

	t.Run("eviction_path", func(t *testing.T) {
		dir := t.TempDir()
		inner := memory.NewMemoryMetadataStoreWithDefaults()
		counter := newCountingFileBlockStore(inner)

		bc, err := New(dir, 1500, 256*1024*1024, counter)
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}
		t.Cleanup(func() { _ = bc.Close() })

		ctx := context.Background()

		// Seed two CAS chunks via StoreChunk (the canonical write path
		// post-LSL-08; eviction is now LRU-driven keyed by ContentHash).
		_ = storeChunk(t, bc, bytes.Repeat([]byte{0xA1}, 500))
		_ = storeChunk(t, bc, bytes.Repeat([]byte{0xA2}, 500))

		bc.SetEvictionEnabled(true)
		bc.SetRetentionPolicy(blockstore.RetentionLRU, 0)

		before := counter.snapshot()

		// diskUsed=1000, maxDisk=1500, needed=600 -> over limit by 100 bytes,
		// forcing eviction of at least one of the 500B chunks.
		if err := bc.ensureSpace(ctx, 600); err != nil {
			t.Fatalf("ensureSpace failed: %v", err)
		}

		after := counter.snapshot()
		delta := diffSnapshot(before, after)
		if delta != (fbsCallSnapshot{}) {
			t.Errorf("eviction called FileBlockStore: %+v", delta)
		}
	})
}

// writeSentinelForTest writes a minimal valid `.cas-migrated-v1` marker
// at the share-dir root. Mirrors pkg/blockstore/migrate.writeSentinel's
// contract (file content is opaque to the boot guard; presence is what
// matters).
func writeSentinelForTest(t *testing.T, shareDir string) {
	t.Helper()
	p := filepath.Join(shareDir, ".cas-migrated-v1")
	if err := os.WriteFile(p, []byte(`{"version":"v1"}`), 0644); err != nil {
		t.Fatalf("write sentinel %q: %v", p, err)
	}
}

// writeLegacyBlkForTest creates a non-empty `.blk` file at the legacy
// path-keyed layout depth that *fs.FSStore's flush path historically
// produced: <baseDir>/<shard>/<payloadID>/<idx>.blk. Phase 17's gate
// only cares that a `.blk` extension exists somewhere within the depth
// cap; the exact tree shape is conventional, not enforced.
func writeLegacyBlkForTest(t *testing.T, shareDir string) {
	t.Helper()
	payloadDir := filepath.Join(shareDir, "fi", "file-001")
	if err := os.MkdirAll(payloadDir, 0755); err != nil {
		t.Fatalf("mkdir %q: %v", payloadDir, err)
	}
	p := filepath.Join(payloadDir, "0.blk")
	if err := os.WriteFile(p, []byte("legacy bytes"), 0644); err != nil {
		t.Fatalf("write legacy blk %q: %v", p, err)
	}
}

// TestNewFSStore_SentinelDetection exercises the Phase 17 Plan 09 D-10
// four-state matrix:
//
//   - sentinel PRESENT, no .blk files     → success (post-migration steady state)
//   - sentinel PRESENT, .blk files PRESENT → success (operator footgun trust)
//   - sentinel MISSING, no .blk files     → success (fresh install)
//   - sentinel MISSING, .blk files PRESENT → ErrLegacyLayoutDetected
//
// The assertion path for the legacy state uses errors.Is per Plan 01
// D-11 contract (the sentinel is a `var = errors.New(...)`).
func TestNewFSStore_SentinelDetection(t *testing.T) {
	type matrixCase struct {
		name          string
		writeSentinel bool
		writeBlk      bool
		wantLegacy    bool // expect ErrLegacyLayoutDetected
	}
	cases := []matrixCase{
		{name: "sentinel_present_no_blk_files", writeSentinel: true, writeBlk: false, wantLegacy: false},
		{name: "sentinel_present_with_blk_files", writeSentinel: true, writeBlk: true, wantLegacy: false},
		{name: "no_sentinel_no_blk_files", writeSentinel: false, writeBlk: false, wantLegacy: false},
		{name: "no_sentinel_with_blk_files", writeSentinel: false, writeBlk: true, wantLegacy: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shareDir := t.TempDir()
			if tc.writeSentinel {
				writeSentinelForTest(t, shareDir)
			}
			if tc.writeBlk {
				writeLegacyBlkForTest(t, shareDir)
			}
			mds := memory.NewMemoryMetadataStoreWithDefaults()
			bc, err := New(shareDir, 0, 256*1024*1024, mds)
			if tc.wantLegacy {
				if err == nil {
					_ = bc.Close()
					t.Fatalf("expected ErrLegacyLayoutDetected, got nil")
				}
				if !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
					t.Fatalf("expected errors.Is(err, ErrLegacyLayoutDetected); got %v", err)
				}
				// Share path must appear in the wrapped message so the
				// boot directive can echo it back to the operator.
				if !bytes.Contains([]byte(err.Error()), []byte(shareDir)) {
					t.Errorf("err %q missing share path %q", err, shareDir)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			t.Cleanup(func() { _ = bc.Close() })
		})
	}
}

// TestNewFSStore_DeepBlkFile asserts the gate's depth cap of 3 directories
// under baseDir: legacy `.blk` at the canonical legacy depth (<= 3) is
// detected; a `.blk` planted past depth 3 is intentionally NOT detected.
// This is a perf optimization documented in the implementation — legacy
// `.blk` files always lived at <share>/<shard>/<payloadID>/<idx>.blk which
// is depth 3 under baseDir.
func TestNewFSStore_DeepBlkFile(t *testing.T) {
	t.Run("legacy_depth_detected", func(t *testing.T) {
		shareDir := t.TempDir()
		// depth-3 .blk file: shareDir/a/b/c/0.blk → 0.blk has depth 4? Let's
		// match the cap (≤3). Use depth-3 placement matching the legacy
		// writer: <share>/<shard=2chars>/<payloadID>/<idx>.blk.
		dir := filepath.Join(shareDir, "ab", "payload-001")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "0.blk"), []byte("x"), 0644); err != nil {
			t.Fatalf("write blk: %v", err)
		}
		mds := memory.NewMemoryMetadataStoreWithDefaults()
		_, err := New(shareDir, 0, 256*1024*1024, mds)
		if !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
			t.Fatalf("expected ErrLegacyLayoutDetected at legacy depth; got %v", err)
		}
	})

	t.Run("beyond_depth_cap_not_detected", func(t *testing.T) {
		shareDir := t.TempDir()
		// Plant a .blk file at depth 5 — past the legacy layout's depth=3.
		// Plan 09 explicitly documents this as a perf optimization
		// (legacy data was always at depth ≤3); this is a regression
		// guard against future unbounded WalkDir on huge stores.
		deep := filepath.Join(shareDir, "d1", "d2", "d3", "d4", "d5")
		if err := os.MkdirAll(deep, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(deep, "stray.blk"), []byte("x"), 0644); err != nil {
			t.Fatalf("write blk: %v", err)
		}
		mds := memory.NewMemoryMetadataStoreWithDefaults()
		bc, err := New(shareDir, 0, 256*1024*1024, mds)
		if err != nil {
			t.Fatalf("expected success (depth>3 .blk not detected); got %v", err)
		}
		t.Cleanup(func() { _ = bc.Close() })
	})
}

// TestNewFSStoreForMigration_BypassesSentinel asserts the bypass
// constructor opens an FSStore against the very state Plan 09's gate
// refuses (sentinel-missing, .blk-present). This is the entry point
// the `dfs migrate-to-cas` subcommand uses to process legacy data.
func TestNewFSStoreForMigration_BypassesSentinel(t *testing.T) {
	shareDir := t.TempDir()
	writeLegacyBlkForTest(t, shareDir)

	// Confirm the production constructor refuses, so we know the bypass
	// is actually being exercised by the next call.
	if _, err := New(shareDir, 0, 256*1024*1024, memory.NewMemoryMetadataStoreWithDefaults()); !errors.Is(err, blockstore.ErrLegacyLayoutDetected) {
		t.Fatalf("precondition: New should refuse legacy layout; got %v", err)
	}

	bc, err := NewFSStoreForMigration(shareDir, 0, 256*1024*1024,
		memory.NewMemoryMetadataStoreWithDefaults(), FSStoreOptions{})
	if err != nil {
		t.Fatalf("NewFSStoreForMigration: expected success on legacy layout; got %v", err)
	}
	t.Cleanup(func() { _ = bc.Close() })
}
