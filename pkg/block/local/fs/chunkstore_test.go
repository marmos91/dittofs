package fs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// hashFromHex builds a ContentHash from a 64-char hex string for tests.
func hashFromHex(t *testing.T, s string) block.ContentHash {
	t.Helper()
	var h block.ContentHash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != block.HashSize {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	copy(h[:], b)
	return h
}

func TestChunkStore_RoundTrip(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("ab", 32))
	data := bytes.Repeat([]byte{0xAB}, 4096)
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	exists, err := bc.HasChunk(ctx, h)
	if err != nil || !exists {
		t.Fatalf("HasChunk: exists=%v err=%v", exists, err)
	}
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip data mismatch")
	}
}

// TestChunkStore_RoundTrip_OddSize exercises the stat-sized read path with a
// chunk whose length is odd and not a power of two (so a single make()+ReadFull
// must size the buffer exactly from the on-disk stat). The in-RAM LRU index is
// cleared before the read to force a cold disk read and exercise the
// re-stat-after-read LRU promote. Bytes must be returned byte-identical.
func TestChunkStore_RoundTrip_OddSize(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("3e", 32))
	// 7919 is prime: odd, not a power of two, larger than any round buffer.
	data := make([]byte, 7919)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	// Drop the LRU index so the read can't be served from a warm entry and
	// the stat-sized cold-read path is exercised end to end.
	bc.lruMu.Lock()
	bc.lruList.Init()
	for k := range bc.lruIndex {
		delete(bc.lruIndex, k)
	}
	bc.lruMu.Unlock()

	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("odd-size read length: got %d want %d", len(got), len(data))
	}
	if !bytes.Equal(got, data) {
		t.Fatal("odd-size round-trip data mismatch")
	}
}

func TestChunkStore_Idempotent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("cd", 32))
	data := bytes.Repeat([]byte{0xCD}, 256)
	ctx := context.Background()

	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("first StoreChunk: %v", err)
	}
	if err := bc.StoreChunk(ctx, h, data); err != nil {
		t.Fatalf("second StoreChunk (idempotent): %v", err)
	}
	// Verify on-disk bytes still match.
	got, err := bc.ReadChunk(ctx, h)
	if err != nil {
		t.Fatalf("ReadChunk after idempotent store: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data mismatch after idempotent store")
	}
	// No .tmp leak in the shard directory.
	shardDir := filepath.Dir(bc.chunkPath(h))
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		t.Fatalf("ReadDir shardDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf(".tmp leaked in shard dir: %s", e.Name())
		}
	}
}

func TestChunkStore_ReadMissing_ErrChunkNotFound(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("ef", 32))
	_, err := bc.ReadChunk(context.Background(), h)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("want ErrChunkNotFound, got %v", err)
	}
}

func TestChunkStore_DeleteMissing_NoError(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("12", 32))
	if err := bc.DeleteChunk(context.Background(), h); err != nil {
		t.Fatalf("DeleteChunk on missing: want nil, got %v", err)
	}
}

func TestChunkStore_TornTmp_NotVisible(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	h := hashFromHex(t, strings.Repeat("34", 32))
	path := bc.chunkPath(h)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// Simulate a crash that left a torn .tmp sibling without the atomic
	// rename ever completing. HasChunk must ignore the .tmp.
	if err := os.WriteFile(path+".tmp", []byte("torn"), 0644); err != nil {
		t.Fatalf("WriteFile .tmp: %v", err)
	}
	exists, err := bc.HasChunk(context.Background(), h)
	if err != nil {
		t.Fatalf("HasChunk err: %v", err)
	}
	if exists {
		t.Fatal("HasChunk returned true for a torn .tmp — partial chunk was visible")
	}
}

func TestChunkStore_ShardPath_TwoLevel(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	full := "deadbeef" + strings.Repeat("00", 28)
	h := hashFromHex(t, full)
	got := bc.chunkPath(h)
	wantSuffix := filepath.Join("blocks", "de", "ad", full)
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("sharded path wrong: got %s, want suffix %s", got, wantSuffix)
	}
}

// TestChunkPathFormat closes the FIX-9 audit gap: TestContentHash_CASKey_Format
// (in pkg/block) only validates the hex string helper, not the on-disk
// path layout that StoreChunk actually uses. This test calls StoreChunk with a
// known-hex hash and asserts the resulting file lives at the documented
// blocks/{hex[0:2]}/{hex[2:4]}/{hex} layout under baseDir — a regression
// guard if the sharding scheme is ever changed without updating callers
// (recovery's sharding inverse + GC mark-sweep both rely on this
// shape).
func TestChunkPathFormat(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	full := "5a5a" + strings.Repeat("00", 30) // 64-char hex
	h := hashFromHex(t, full)
	data := bytes.Repeat([]byte{0x5A}, 64)

	if err := bc.StoreChunk(context.Background(), h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}

	wantPath := filepath.Join(bc.baseDir, "blocks", "5a", "5a", full)
	st, err := os.Stat(wantPath)
	if err != nil {
		t.Fatalf("expected chunk file at %s: stat err=%v", wantPath, err)
	}
	if st.Size() != int64(len(data)) {
		t.Fatalf("chunk file size: got %d want %d", st.Size(), len(data))
	}
	if st.IsDir() {
		t.Fatalf("expected file at %s, got directory", wantPath)
	}
}

// ---: OnChunkComplete wire-in in StoreChunk. ---
//
// landed the FSStoreOptions.OnChunkComplete slot + the storage
// field. fires the callback from StoreChunk AFTER lruTouch on
// every successful chunk write. The five tests below pin the producer-
// side contract end-to-end: exactly-once on success, never on error
// nil-safe, and outside the lruMu hot lock so consumers (engine.Cache.Put)
// can take their own locks without deadlock risk.

// TestChunkstore_OnChunkComplete_FiresAfterSuccessfulStoreChunk asserts a
// single StoreChunk fires OnChunkComplete exactly once with the canonical
// (hash, data, path) triple. Path matches the on-disk file the chunk
// resolved to.
func TestChunkstore_OnChunkComplete_FiresAfterSuccessfulStoreChunk(t *testing.T) {
	var calls atomic.Int64
	var gotHash block.ContentHash
	var gotData []byte
	var gotPath string
	cb := func(h block.ContentHash, data []byte, path string) {
		calls.Add(1)
		gotHash = h
		// Copy the slice so a later overwrite by the caller does not
		// invalidate the capture (matches Cache.Put's heap-copy semantics
		// — the test mirrors what a real consumer would see).
		gotData = append([]byte(nil), data...)
		gotPath = path
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	h := hashFromHex(t, strings.Repeat("7a", 32))
	data := bytes.Repeat([]byte{0x7A}, 1024)

	if err := bc.StoreChunk(context.Background(), h, data); err != nil {
		t.Fatalf("StoreChunk: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("OnChunkComplete fired %d times; want 1", got)
	}
	if gotHash != h {
		t.Fatalf("captured hash mismatch: got %s want %s", gotHash.String(), h.String())
	}
	if !bytes.Equal(gotData, data) {
		t.Fatalf("captured data byte-mismatch: len got %d want %d", len(gotData), len(data))
	}
	if wantPath := bc.chunkPath(h); gotPath != wantPath {
		t.Fatalf("captured path mismatch: got %s want %s", gotPath, wantPath)
	}
}

// TestChunkstore_NilOnChunkComplete_NoOp asserts StoreChunk succeeds and
// does NOT panic when no callback is installed (nil-safety contract
// gated at the firing site — chunkstore.go is the producer).
func TestChunkstore_NilOnChunkComplete_NoOp(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{})
	if cb := bc.onChunkComplete.Load(); cb == nil || cb.fn != nil {
		t.Fatalf("precondition: onChunkComplete.fn must start nil; holder=%v", cb)
	}
	h := hashFromHex(t, strings.Repeat("8b", 32))
	data := bytes.Repeat([]byte{0x8B}, 512)

	if err := bc.StoreChunk(context.Background(), h, data); err != nil {
		t.Fatalf("StoreChunk with nil OnChunkComplete: %v", err)
	}
	if _, err := os.Stat(bc.chunkPath(h)); err != nil {
		t.Fatalf("chunk on disk missing after StoreChunk: %v", err)
	}
}

// TestChunkstore_OnChunkComplete_FiresExactlyOnce_PerSuccessfulCall asserts
//   - Two StoreChunk calls with DIFFERENT hashes → counter == 2.
//   - A second StoreChunk for an already-stored hash short-circuits via
//     HasChunk (idempotent CAS) and does NOT fire again — the callback is
//     anchored to lruTouch, which itself is only called on rename success.
func TestChunkstore_OnChunkComplete_FiresExactlyOnce_PerSuccessfulCall(t *testing.T) {
	var calls atomic.Int64
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	ctx := context.Background()

	hA := hashFromHex(t, strings.Repeat("9c", 32))
	hB := hashFromHex(t, strings.Repeat("ad", 32))
	dataA := bytes.Repeat([]byte{0x9C}, 256)
	dataB := bytes.Repeat([]byte{0xAD}, 256)

	if err := bc.StoreChunk(ctx, hA, dataA); err != nil {
		t.Fatalf("StoreChunk A: %v", err)
	}
	if err := bc.StoreChunk(ctx, hB, dataB); err != nil {
		t.Fatalf("StoreChunk B: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("after 2 distinct StoreChunk: callback fired %d times; want 2", got)
	}
	// Second store of the same hash is the idempotent CAS path: HasChunk
	// returns true, StoreChunk returns nil before reaching the rename / LRU
	// touch / callback site. The counter must NOT advance.
	if err := bc.StoreChunk(ctx, hA, dataA); err != nil {
		t.Fatalf("StoreChunk A (idempotent): %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("after idempotent re-store: callback fired %d times; want still 2", got)
	}
}

// TestChunkstore_OnChunkComplete_DoesNotFireOnError asserts that error
// paths through StoreChunk leave the callback un-invoked (never
// fire on error). The setup pre-creates the blocks/<hh>/<hh> directory as
// a regular file so MkdirAll fails before the rename / LRU touch / callback
// can run.
func TestChunkstore_OnChunkComplete_DoesNotFireOnError(t *testing.T) {
	var calls atomic.Int64
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		calls.Add(1)
	}
	bc := newFSStoreForTest(t, FSStoreOptions{OnChunkComplete: cb})
	h := hashFromHex(t, strings.Repeat("be", 32))
	data := bytes.Repeat([]byte{0xBE}, 256)

	// Pre-create one of the shard parent directories AS A FILE so
	// MkdirAll on its child path fails with ENOTDIR. Layout
	//   <baseDir>/blocks/<hh>/<hh>/<hex>
	// With h = bebe...be the first shard is "be"; create
	// <baseDir>/blocks/be as a regular file before StoreChunk runs.
	blocksDir := filepath.Join(bc.baseDir, "blocks")
	if err := os.MkdirAll(blocksDir, 0755); err != nil {
		t.Fatalf("mkdir blocks/: %v", err)
	}
	occupier := filepath.Join(blocksDir, "be")
	if err := os.WriteFile(occupier, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write occupier: %v", err)
	}

	err := bc.StoreChunk(context.Background(), h, data)
	if err == nil {
		t.Fatal("StoreChunk: expected error from MkdirAll over file; got nil")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("OnChunkComplete fired %d times on error path; want 0", got)
	}
	// And the chunk must NOT exist on disk.
	if _, statErr := os.Stat(bc.chunkPath(h)); statErr == nil {
		t.Fatal("chunk file exists after failed StoreChunk")
	}
}

// TestChunkstore_OnChunkComplete_FiresOutsideLruMuLock asserts the
// callback runs OUTSIDE bc.lruMu. The callback re-enters bc.lruTouch for
// an unrelated hash; if the firing site held lruMu, the re-entrant
// acquisition would deadlock and the goroutine would block past the
// timeout. The test runs StoreChunk on a separate goroutine and asserts
// it completes within a generous budget.
func TestChunkstore_OnChunkComplete_FiresOutsideLruMuLock(t *testing.T) {
	otherHash := hashFromHex(t, strings.Repeat("cf", 32))
	otherPath := "/dev/null/touch-target" // never opened by lruTouch — index-only insert
	done := make(chan struct{})
	cb := func(_ block.ContentHash, _ []byte, _ string) {
		// Re-enter lruTouch on the SAME FSStore. If the producer holds
		// lruMu while firing the callback, sync.Mutex.Lock here blocks
		// forever (Go mutexes are not reentrant).
	}
	bc := newFSStoreForTest(t, FSStoreOptions{})
	// Install callback that closes over bc so we can probe lruTouch from
	// within the callback body.
	bc.SetOnChunkComplete(func(h block.ContentHash, data []byte, path string) {
		bc.lruTouch(otherHash, int64(len(data)), otherPath)
		cb(h, data, path)
	})

	h := hashFromHex(t, strings.Repeat("d0", 32))
	data := bytes.Repeat([]byte{0xD0}, 256)

	go func() {
		if err := bc.StoreChunk(context.Background(), h, data); err != nil {
			t.Errorf("StoreChunk: %v", err)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StoreChunk blocked > 5s: callback fired inside lruMu (deadlock)")
	}
	// Confirm the otherHash was indeed inserted into the LRU by the
	// callback (otherwise the test would pass trivially even if the
	// callback never ran).
	bc.lruMu.Lock()
	_, ok := bc.lruIndex[otherHash]
	bc.lruMu.Unlock()
	if !ok {
		t.Fatal("callback did not register otherHash in LRU; reentrancy probe ineffective")
	}
}
