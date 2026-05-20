package localtest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/local"
)

// chunkStorer is the optional capability used by RunGetSuite to populate
// a CAS-shaped chunk before exercising LocalStore.Get. Backends that
// store CAS chunks (e.g. *fs.FSStore) satisfy this interface; backends
// that do not (e.g. memory.MemoryStore) cause the round-trip assertions
// to be skipped while the missing-hash sentinel assertion still runs.
type chunkStorer interface {
	StoreChunk(ctx context.Context, h blockstore.ContentHash, data []byte) error
}

// Factory creates a new LocalStore instance for testing.
// Each test calls Factory to get a fresh, independent store.
type Factory func(t *testing.T) local.LocalStore

// RunSuite runs the full conformance test suite against a LocalStore
// implementation.
//
// For the append-log / rollup / recovery scenarios, see RunAppendLogSuite
// in appendlog_suite.go — it uses a separate factory type (*fs.FSStore)
// because the append-log methods are not on the LocalStore interface.
// Callers that want both suites can invoke them independently.
func RunSuite(t *testing.T, factory Factory) {
	t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
	t.Run("ReadMiss", func(t *testing.T) { testReadMiss(t, factory) })
	t.Run("WriteMultiBlock", func(t *testing.T) { testWriteMultiBlock(t, factory) })
	t.Run("Flush", func(t *testing.T) { testFlush(t, factory) })
	t.Run("Truncate", func(t *testing.T) { testTruncate(t, factory) })
	t.Run("EvictMemory", func(t *testing.T) { testEvictMemory(t, factory) })
	t.Run("DeleteBlockFile", func(t *testing.T) { testDeleteBlockFile(t, factory) })
	t.Run("DeleteAllBlockFiles", func(t *testing.T) { testDeleteAllBlockFiles(t, factory) })
	t.Run("GetFileSize", func(t *testing.T) { testGetFileSize(t, factory) })
	t.Run("ListFiles", func(t *testing.T) { testListFiles(t, factory) })
	t.Run("Stats", func(t *testing.T) { testStats(t, factory) })
	t.Run("WriteFromRemote", func(t *testing.T) { testWriteFromRemote(t, factory) })
	t.Run("GetBlockData", func(t *testing.T) { testGetBlockData(t, factory) })
	t.Run("IsBlockLocal", func(t *testing.T) { testIsBlockLocal(t, factory) })
	t.Run("CloseRejectsOps", func(t *testing.T) { testCloseRejectsOps(t, factory) })
}

func testWriteAndRead(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte("hello"), 100)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}
}

func testReadMiss(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	dest := make([]byte, 4096)
	found, err := store.ReadAt(ctx, "nonexistent", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt on missing file should not error: %v", err)
	}
	if found {
		t.Fatal("expected miss for nonexistent file")
	}
}

func testWriteMultiBlock(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Write data spanning 2+ blocks
	size := int(blockstore.BlockSize) + 4096
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	dest := make([]byte, size)
	found, err := store.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt returned miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt returned wrong data")
	}
}

func testFlush(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xAB}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	flushed, err := store.Flush(ctx, "file1")
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if len(flushed) == 0 {
		t.Fatal("expected at least one flushed block")
	}

	// Data should still be readable after flush
	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt after Flush failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt after Flush returned miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt after Flush returned wrong data")
	}
}

func testTruncate(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xAA}, 8192)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.Truncate(ctx, "file1", 4096); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	fileSize, ok := store.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found after Truncate")
	}
	if fileSize != 4096 {
		t.Fatalf("expected file size 4096, got %d", fileSize)
	}
}

func testEvictMemory(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xFF}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.EvictMemory(ctx, "file1"); err != nil {
		t.Fatalf("EvictMemory failed: %v", err)
	}

	_, ok := store.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after EvictMemory")
	}
}

func testDeleteBlockFile(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xBB}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.DeleteBlockFile(ctx, "file1", 0); err != nil {
		t.Fatalf("DeleteBlockFile failed: %v", err)
	}

	// Block should no longer be in local store
	if store.IsBlockLocal(ctx, "file1", 0) {
		t.Error("block should not be in local store after DeleteBlockFile")
	}
}

func testDeleteAllBlockFiles(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Write to two blocks
	data := make([]byte, int(blockstore.BlockSize)+4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if err := store.DeleteAllBlockFiles(ctx, "file1"); err != nil {
		t.Fatalf("DeleteAllBlockFiles failed: %v", err)
	}

	_, ok := store.GetFileSize(ctx, "file1")
	if ok {
		t.Error("file still tracked after DeleteAllBlockFiles")
	}
}

func testGetFileSize(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Not found case
	_, ok := store.GetFileSize(ctx, "nonexistent")
	if ok {
		t.Fatal("expected GetFileSize to return false for nonexistent file")
	}

	// Write and check
	data := bytes.Repeat([]byte{0xCC}, 8192)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	size, ok := store.GetFileSize(ctx, "file1")
	if !ok {
		t.Fatal("GetFileSize returned not found")
	}
	if size != 8192 {
		t.Fatalf("expected file size 8192, got %d", size)
	}
}

func testListFiles(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for _, id := range []string{"a", "b", "c"} {
		if err := store.WriteAt(ctx, id, []byte("data"), 0); err != nil {
			t.Fatalf("WriteAt %s failed: %v", id, err)
		}
	}

	files := store.ListFiles()
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

func testStats(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.WriteAt(ctx, "f1", bytes.Repeat([]byte{1}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if err := store.WriteAt(ctx, "f2", bytes.Repeat([]byte{2}, 4096), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	stats := store.Stats()
	if stats.FileCount != 2 {
		t.Errorf("expected 2 files, got %d", stats.FileCount)
	}
	if stats.MemBlockCount < 2 {
		t.Errorf("expected at least 2 memBlocks, got %d", stats.MemBlockCount)
	}
}

func testWriteFromRemote(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0xDD}, 4096)
	if err := store.WriteFromRemote(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteFromRemote failed: %v", err)
	}

	dest := make([]byte, len(data))
	found, err := store.ReadAt(ctx, "file1", dest, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if !found {
		t.Fatal("ReadAt miss")
	}
	if !bytes.Equal(dest, data) {
		t.Fatal("ReadAt wrong data")
	}
}

func testGetBlockData(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte{0x42}, 4096)
	if err := store.WriteAt(ctx, "file1", data, 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	blockData, dataSize, err := store.GetBlockData(ctx, "file1", 0)
	if err != nil {
		t.Fatalf("GetBlockData failed: %v", err)
	}
	if dataSize != 4096 {
		t.Fatalf("expected dataSize 4096, got %d", dataSize)
	}
	if !bytes.Equal(blockData[:4096], data) {
		t.Fatal("GetBlockData returned wrong data")
	}
}

func testIsBlockLocal(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Not in local store initially
	if store.IsBlockLocal(ctx, "file1", 0) {
		t.Fatal("block should not be in local store before write")
	}

	// Write and check
	if err := store.WriteAt(ctx, "file1", []byte("data"), 0); err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}

	if !store.IsBlockLocal(ctx, "file1", 0) {
		t.Fatal("block should be in local store after write")
	}
}

// RunGetSuite exercises LocalStore.Get. The scenario matrix is:
//
//   - missing-hash: Get(zero hash) → blockstore.ErrChunkNotFound.
//   - CAS-capable backends only (chunkStorer): StoreChunk a known
//     payload, Get it back, assert byte-identical via bytes.Equal.
//   - Fresh-allocation contract: two Get calls on the same hash return
//     independent backing arrays — mutating slice #1 must not be
//     observable through slice #2. This defends the buffer-ownership
//     contract by behavior, not by unsafe pointer comparison.
//
// Backends that do not store CAS chunks (memory.MemoryStore) skip the
// round-trip + aliasing assertions and exercise only the missing-hash
// sentinel — matching the documented stub behavior of MemoryStore.Get.
func RunGetSuite(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("Get_MissingHash_ReturnsErrChunkNotFound", func(t *testing.T) {
		store := factory(t)
		ctx := context.Background()
		var missing blockstore.ContentHash
		// One non-zero byte so this doesn't collide with any hypothetical
		// future "all-zero hash" sentinel a backend might special-case.
		missing[0] = 0xDE
		missing[31] = 0xAD
		if _, err := store.Get(ctx, missing); !errors.Is(err, blockstore.ErrChunkNotFound) {
			t.Fatalf("Get(missing): want ErrChunkNotFound, got %v", err)
		}
	})

	t.Run("Get_CASRoundTrip", func(t *testing.T) {
		store := factory(t)
		cs, ok := store.(chunkStorer)
		if !ok {
			t.Skip("backend does not implement StoreChunk; skipping CAS round-trip")
		}
		ctx := context.Background()

		// Deterministic payload + matching ContentHash.
		var h blockstore.ContentHash
		for i := range h {
			h[i] = byte(i + 1)
		}
		payload := bytes.Repeat([]byte{0x42}, 4096)
		if err := cs.StoreChunk(ctx, h, payload); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}

		got, err := store.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatal("Get returned bytes that differ from the stored payload")
		}
	})

	t.Run("Get_FreshAllocationPerCall", func(t *testing.T) {
		store := factory(t)
		cs, ok := store.(chunkStorer)
		if !ok {
			t.Skip("backend does not implement StoreChunk; skipping fresh-allocation defense")
		}
		ctx := context.Background()

		var h blockstore.ContentHash
		for i := range h {
			h[i] = byte(0xA0 ^ i)
		}
		payload := bytes.Repeat([]byte{0x77}, 1024)
		if err := cs.StoreChunk(ctx, h, payload); err != nil {
			t.Fatalf("StoreChunk: %v", err)
		}

		out1, err := store.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get #1: %v", err)
		}
		out2, err := store.Get(ctx, h)
		if err != nil {
			t.Fatalf("Get #2: %v", err)
		}
		if len(out1) == 0 || len(out2) == 0 {
			t.Fatalf("Get returned empty slice (len1=%d len2=%d)", len(out1), len(out2))
		}

		// Mutate the first slice; the second must remain unchanged.
		// Defends the fresh-allocation-per-call + no-aliasing contracts
		// by behavior — `&out1[0] != &out2[0]` is
		// fragile (compiler/runtime may legitimately reuse). Mutation
		// is the load-bearing assertion the engine actually depends on:
		// the Cache copies bytes into its LRU slot and a subsequent
		// loader pass must not observe a previous caller's writes.
		original := out2[0]
		out1[0] = ^out1[0]
		if out2[0] != original {
			t.Fatalf("Get is aliasing: mutating slice from first Get changed slice from second Get (out2[0] went %#x -> %#x)", original, out2[0])
		}
	})
}

func testCloseRejectsOps(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Operations after Close should fail
	if err := store.WriteAt(ctx, "file1", []byte("data"), 0); err == nil {
		t.Error("WriteAt should fail after Close")
	}

	_, err := store.ReadAt(ctx, "file1", make([]byte, 4), 0)
	if err == nil {
		t.Error("ReadAt should fail after Close")
	}
}
