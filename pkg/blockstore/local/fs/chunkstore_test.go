package fs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// hashFromHex builds a ContentHash from a 64-char hex string for tests.
func hashFromHex(t *testing.T, s string) blockstore.ContentHash {
	t.Helper()
	var h blockstore.ContentHash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != blockstore.HashSize {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	copy(h[:], b)
	return h
}

func TestChunkStore_RoundTrip(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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

func TestChunkStore_Idempotent(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
	h := hashFromHex(t, strings.Repeat("ef", 32))
	_, err := bc.ReadChunk(context.Background(), h)
	if !errors.Is(err, blockstore.ErrChunkNotFound) {
		t.Fatalf("want ErrChunkNotFound, got %v", err)
	}
}

func TestChunkStore_DeleteMissing_NoError(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
	h := hashFromHex(t, strings.Repeat("12", 32))
	if err := bc.DeleteChunk(context.Background(), h); err != nil {
		t.Fatalf("DeleteChunk on missing: want nil, got %v", err)
	}
}

func TestChunkStore_TornTmp_NotVisible(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
	full := "deadbeef" + strings.Repeat("00", 28)
	h := hashFromHex(t, full)
	got := bc.chunkPath(h)
	wantSuffix := filepath.Join("blocks", "de", "ad", full)
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("sharded path wrong: got %s, want suffix %s", got, wantSuffix)
	}
}

// TestChunkPathFormat closes the FIX-9 audit gap: TestContentHash_CASKey_Format
// (in pkg/blockstore) only validates the hex string helper, not the on-disk
// path layout that StoreChunk actually uses. This test calls StoreChunk with a
// known-hex hash and asserts the resulting file lives at the documented
// blocks/{hex[0:2]}/{hex[2:4]}/{hex} layout under baseDir — a regression
// guard if the sharding scheme is ever changed without updating callers
// (recovery's sharding inverse + Phase 11 GC mark-sweep both rely on this
// shape).
func TestChunkPathFormat(t *testing.T) {
	bc := newFSStoreForTest(t, FSStoreOptions{UseAppendLog: true})
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
