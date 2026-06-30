package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

// TestStore_BlockStoreConformance runs the unified
// BlockStoreConformance suite against the in-memory remote backend.
// Per the remote backends implement only BlockStore (no
// BlockStoreAppend); only BlockStoreConformance runs here.
//
// The inline TestStore_* tests below remain in place as a fine-grained
// per-method baseline (data-isolation defensive copies, closed-store
// rejection paths, ReadBlockVerified mismatch case) — they exercise
// backend-specific behaviors that are not part of the unified contract.
//
// -07 lands the missing Has() method on the remote-memory
// *Store; until then the factory return type does not type-check.
func TestStore_BlockStoreConformance(t *testing.T) {
	factory := func(t *testing.T) (block.Store, func()) {
		t.Helper()
		s := New()
		cleanup := func() { _ = s.Close() }
		return s, cleanup
	}
	blockstoretest.BlockStoreConformance(t, factory)
}

// TestMemory_RemoteBlockStoreConformance runs the unified
// RemoteBlockStoreConformance suite against the in-memory backend. All
// subtests run in-process with no I/O; they cover the block-keyed (non-CAS)
// surface: PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks.
func TestMemory_RemoteBlockStoreConformance(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		s := New()
		return s, func() { _ = s.Close() }
	})
}

// hashOf returns the BLAKE3-256 hash of data as a block.ContentHash.
func hashOf(t *testing.T, data []byte) block.ContentHash {
	t.Helper()
	sum := blake3.Sum256(data)
	var h block.ContentHash
	copy(h[:], sum[:])
	return h
}

func TestStore_PutAndGet(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	read, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if string(read) != string(data) {
		t.Errorf("Get returned %q, want %q", read, data)
	}
}

func TestStore_GetNotFound(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	hash := hashOf(t, []byte("nonexistent"))
	_, err := s.Get(ctx, hash)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Errorf("Get returned error %v, want %v", err, block.ErrChunkNotFound)
	}
}

func TestStore_GetRange(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	read, err := s.GetRange(ctx, hash, 0, 5)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "hello" {
		t.Errorf("GetRange returned %q, want %q", read, "hello")
	}

	read, err = s.GetRange(ctx, hash, 6, 5)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("GetRange returned %q, want %q", read, "world")
	}

	// Read range that exceeds length (should truncate)
	read, err = s.GetRange(ctx, hash, 6, 100)
	if err != nil {
		t.Fatalf("GetRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Errorf("GetRange returned %q, want %q", read, "world")
	}
}

// TestStore_GetRange_InvalidBounds pins the malformed-bounds sentinels:
// an offset at or beyond the chunk size is ErrInvalidOffset (not
// ErrChunkNotFound, the historical regression) and a non-positive length
// is ErrInvalidSize.
func TestStore_GetRange_InvalidBounds(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world") // 11 bytes
	hash := hashOf(t, data)
	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	cases := []struct {
		name           string
		offset, length int64
		want           error
	}{
		{"negative offset", -1, 4, block.ErrInvalidOffset},
		{"offset at size", 11, 4, block.ErrInvalidOffset},
		{"offset beyond size", 12, 4, block.ErrInvalidOffset},
		{"zero length", 0, 0, block.ErrInvalidSize},
		{"negative length", 0, -1, block.ErrInvalidSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.GetRange(ctx, hash, tc.offset, tc.length)
			if !errors.Is(err, tc.want) {
				t.Fatalf("GetRange(offset=%d,length=%d): want %v, got %v", tc.offset, tc.length, tc.want, err)
			}
		})
	}

	// A valid in-range offset with a length so large that offset+length would
	// overflow int64 must clamp to the remaining bytes, not panic or wrap.
	t.Run("length overflow clamps", func(t *testing.T) {
		got, err := s.GetRange(ctx, hash, 6, math.MaxInt64)
		if err != nil {
			t.Fatalf("GetRange(6, MaxInt64): unexpected error %v", err)
		}
		if string(got) != "world" {
			t.Fatalf("GetRange(6, MaxInt64): want clamped %q, got %q", "world", got)
		}
	})
}

func TestStore_Delete(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if err := s.Delete(ctx, hash); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	_, err := s.Get(ctx, hash)
	if !errors.Is(err, block.ErrChunkNotFound) {
		t.Errorf("Get after delete returned %v, want %v", err, block.ErrChunkNotFound)
	}

	// Delete on absent hash is idempotent.
	if err := s.Delete(ctx, hash); err != nil {
		t.Errorf("Delete on absent hash returned %v, want nil (idempotent)", err)
	}
}

func TestStore_Head(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("head fixture")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	meta, err := s.Head(ctx, hash)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if meta.Size != int64(len(data)) {
		t.Errorf("Head Size = %d, want %d", meta.Size, len(data))
	}
	if meta.LastModified.IsZero() {
		t.Error("Head LastModified is zero — WR-4-02 contract violation")
	}

	// Missing hash
	missing := hashOf(t, []byte("does-not-exist"))
	if _, err := s.Head(ctx, missing); !errors.Is(err, block.ErrChunkNotFound) {
		t.Errorf("Head on missing hash = %v, want %v", err, block.ErrChunkNotFound)
	}
}

// TestStore_Walk asserts the Walk contract: every stored
// CAS object is visited; the callback receives a non-zero Meta; ordering
// is unspecified so we collect into a set.
func TestStore_Walk(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	want := map[block.ContentHash][]byte{}
	for i := 0; i < 5; i++ {
		data := []byte(fmt.Sprintf("walk fixture %d", i))
		h := hashOf(t, data)
		want[h] = data
		if err := s.Put(ctx, h, data); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := map[block.ContentHash]bool{}
	err := s.Walk(ctx, func(hash block.ContentHash, meta block.Meta) error {
		seen[hash] = true
		exp, ok := want[hash]
		if !ok {
			return fmt.Errorf("Walk surfaced unknown hash %s", hash)
		}
		if meta.Size != int64(len(exp)) {
			return fmt.Errorf("Walk meta.Size = %d, want %d", meta.Size, len(exp))
		}
		if meta.LastModified.IsZero() {
			return fmt.Errorf("Walk meta.LastModified is zero")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for h := range want {
		if !seen[h] {
			t.Errorf("Walk did not surface hash %s", h)
		}
	}
}

// TestStore_Walk_ErrStopWalk pins the early-exit contract
// returning block.ErrStopWalk from the callback exits cleanly with
// nil from Walk.
func TestStore_Walk_ErrStopWalk(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	for i := 0; i < 3; i++ {
		data := []byte(fmt.Sprintf("stopwalk %d", i))
		if err := s.Put(ctx, hashOf(t, data), data); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	seen := 0
	err := s.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error {
		seen++
		if seen == 1 {
			return block.ErrStopWalk
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk should return nil on ErrStopWalk, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("Walk should stop after first ErrStopWalk, saw %d objects", seen)
	}
}

// TestStore_Walk_CallbackErrorWrapped pins the contract that
// non-ErrStopWalk callback errors are wrapped as "walk halted at %s: %w".
func TestStore_Walk_CallbackErrorWrapped(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("wrap me")
	if err := s.Put(ctx, hashOf(t, data), data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	sentinel := errors.New("callback boom")
	err := s.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error {
		return sentinel
	})
	if err == nil {
		t.Fatal("Walk should propagate callback error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk err = %v, want wrapped %v", err, sentinel)
	}
}

// TestStore_ReadBlockVerified covers the happy path and the
// body-mismatch failure mode.
// TestStore_ReadChunk covers the base-store block range read used by the
// #1414 locator read path: a chunk staged inside a block object is returned
// verbatim for its [offset, length) slice, with GetRange-style bounds checks.
func TestStore_ReadChunk(t *testing.T) {
	ctx := context.Background()
	s := New()

	a := bytes.Repeat([]byte{0xA1}, 64)
	b := bytes.Repeat([]byte{0xB2}, 128)
	blockData := append(append([]byte{}, a...), b...)
	const blockID = "block-mem-1"
	if err := s.PutBlock(ctx, blockID, bytes.NewReader(blockData)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got, err := s.ReadChunk(ctx, blockID, int64(len(a)), int64(len(b)), block.ContentHash{})
	if err != nil {
		t.Fatalf("ReadChunk: %v", err)
	}
	if !bytes.Equal(got, b) {
		t.Fatalf("ReadChunk returned wrong slice")
	}

	if _, err := s.ReadChunk(ctx, "missing", 0, 1, block.ContentHash{}); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("missing block: got %v, want ErrChunkNotFound", err)
	}
	if _, err := s.ReadChunk(ctx, blockID, -1, 1, block.ContentHash{}); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("negative offset: got %v, want ErrInvalidOffset", err)
	}
	if _, err := s.ReadChunk(ctx, blockID, 0, 0, block.ContentHash{}); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("zero length: got %v, want ErrInvalidSize", err)
	}
	// Past-EOF length clamps to remaining bytes (no error), mirroring GetRange.
	clamped, err := s.ReadChunk(ctx, blockID, int64(len(a)), 1<<20, block.ContentHash{})
	if err != nil {
		t.Fatalf("clamped read: %v", err)
	}
	if !bytes.Equal(clamped, b) {
		t.Fatalf("clamped read returned wrong bytes")
	}
}

func TestStore_ReadBlockVerified(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("verified read")
	hash := hashOf(t, data)
	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.ReadBlockVerified(ctx, hash, hash)
	if err != nil {
		t.Fatalf("ReadBlockVerified happy path: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("ReadBlockVerified bytes mismatch: got %q, want %q", got, data)
	}

	// Mismatched expected => ErrCASContentMismatch
	wrong := hash
	wrong[0] ^= 0xFF
	if _, err := s.ReadBlockVerified(ctx, hash, wrong); !errors.Is(err, block.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified mismatch err = %v, want wrapped ErrCASContentMismatch", err)
	}

	// Not found
	missing := hashOf(t, []byte("missing"))
	if _, err := s.ReadBlockVerified(ctx, missing, missing); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("ReadBlockVerified on missing hash = %v, want wrapped ErrChunkNotFound", err)
	}
}

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := New()

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	hash := hashOf(t, []byte("data"))

	if _, err := s.Get(ctx, hash); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Get on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}

	if err := s.Put(ctx, hash, []byte("data")); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Put on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}

	if err := s.Delete(ctx, hash); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Delete on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}

	if err := s.Walk(ctx, func(_ block.ContentHash, _ block.Meta) error { return nil }); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("Walk on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
}

func TestStore_DataIsolation(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	hash := hashOf(t, data)

	if err := s.Put(ctx, hash, data); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Modify original data
	data[0] = 'X'

	read, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if read[0] != 'h' {
		t.Errorf("Put did not copy data: got %c, want 'h'", read[0])
	}

	// Modify read data
	read[0] = 'Y'

	read2, err := s.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if read2[0] != 'h' {
		t.Errorf("Get did not copy data: got %c, want 'h'", read2[0])
	}
}

func TestStore_BlockCount(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if s.BlockCount() != 0 {
		t.Errorf("BlockCount on empty store returned %d, want 0", s.BlockCount())
	}

	if err := s.Put(ctx, hashOf(t, []byte("data1")), []byte("data1")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := s.Put(ctx, hashOf(t, []byte("data2")), []byte("data2")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if s.BlockCount() != 2 {
		t.Errorf("BlockCount returned %d, want 2", s.BlockCount())
	}
}

func TestStore_TotalSize(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if s.TotalSize() != 0 {
		t.Errorf("TotalSize on empty store returned %d, want 0", s.TotalSize())
	}

	if err := s.Put(ctx, hashOf(t, []byte("hello")), []byte("hello")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}
	if err := s.Put(ctx, hashOf(t, []byte("world")), []byte("world")); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if s.TotalSize() != 10 {
		t.Errorf("TotalSize returned %d, want 10", s.TotalSize())
	}
}

// ---- RemoteBlockStore method tests ----

// TestStore_PutBlock_GetBlock_RoundTrip verifies a PutBlock followed by
// GetBlock returns the exact bytes that were written.
func TestStore_PutBlock_GetBlock_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("block round-trip payload")
	if err := s.PutBlock(ctx, "blk-1", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, err := s.GetBlock(ctx, "blk-1")
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("GetBlock = %q, want %q", got, data)
	}
}

// TestStore_GetBlock_NotFound pins the ErrChunkNotFound mapping.
func TestStore_GetBlock_NotFound(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if _, err := s.GetBlock(ctx, "absent"); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetBlock absent: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_GetBlockRange_Bounds exercises mid, past-EOF clamping, and the
// invalid-offset / invalid-size sentinels.
func TestStore_GetBlockRange_Bounds(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("0123456789abcdef") // 16 bytes
	const id = "blk-range"
	if err := s.PutBlock(ctx, id, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// Mid-block.
	got, err := s.GetBlockRange(ctx, id, 4, 8)
	if err != nil {
		t.Fatalf("GetBlockRange mid: %v", err)
	}
	if string(got) != "456789ab" {
		t.Fatalf("GetBlockRange mid = %q, want %q", got, "456789ab")
	}

	// Past-EOF length: clamped to remaining bytes.
	got, err = s.GetBlockRange(ctx, id, 8, 100)
	if err != nil {
		t.Fatalf("GetBlockRange past-EOF length: %v", err)
	}
	if string(got) != "89abcdef" {
		t.Fatalf("GetBlockRange clamped = %q, want %q", got, "89abcdef")
	}

	// Offset at EOF: ErrInvalidOffset.
	if _, err := s.GetBlockRange(ctx, id, 16, 4); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("offset=EOF: want ErrInvalidOffset, got %v", err)
	}

	// Negative offset: ErrInvalidOffset.
	if _, err := s.GetBlockRange(ctx, id, -1, 4); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("negative offset: want ErrInvalidOffset, got %v", err)
	}

	// Zero length: ErrInvalidSize.
	if _, err := s.GetBlockRange(ctx, id, 0, 0); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("zero length: want ErrInvalidSize, got %v", err)
	}

	// Negative length: ErrInvalidSize.
	if _, err := s.GetBlockRange(ctx, id, 0, -1); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("negative length: want ErrInvalidSize, got %v", err)
	}

	// MaxInt64 length clamps without overflow.
	got, err = s.GetBlockRange(ctx, id, 8, math.MaxInt64)
	if err != nil {
		t.Fatalf("GetBlockRange MaxInt64 length: %v", err)
	}
	if string(got) != "89abcdef" {
		t.Fatalf("GetBlockRange MaxInt64 clamp = %q, want %q", got, "89abcdef")
	}

	// Missing block.
	if _, err := s.GetBlockRange(ctx, "missing", 0, 4); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("missing block: want ErrChunkNotFound, got %v", err)
	}
}

// TestStore_DeleteBlock confirms idempotent delete and that GetBlock misses
// after deletion.
func TestStore_DeleteBlock(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("to be deleted")
	if err := s.PutBlock(ctx, "blk-del", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := s.DeleteBlock(ctx, "blk-del"); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}
	if _, err := s.GetBlock(ctx, "blk-del"); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetBlock after delete: want ErrChunkNotFound, got %v", err)
	}
	// Idempotent: deleting absent block returns nil.
	if err := s.DeleteBlock(ctx, "blk-del"); err != nil {
		t.Fatalf("DeleteBlock idempotent: %v", err)
	}
}

// TestStore_WalkBlocks_EnumeratesAll verifies WalkBlocks visits every block
// exactly once with a non-zero LastModified and correct size.
func TestStore_WalkBlocks_EnumeratesAll(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	want := map[string]int{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("blk-%d", i)
		data := []byte(fmt.Sprintf("block-data-%d", i))
		want[id] = len(data)
		if err := s.PutBlock(ctx, id, bytes.NewReader(data)); err != nil {
			t.Fatalf("PutBlock %s: %v", id, err)
		}
	}

	seen := map[string]int{}
	err := s.WalkBlocks(ctx, func(blockID string, meta block.Meta) error {
		seen[blockID]++
		if meta.LastModified.IsZero() {
			t.Errorf("WalkBlocks: LastModified zero for %s", blockID)
		}
		if wantSize, ok := want[blockID]; ok && meta.Size != int64(wantSize) {
			t.Errorf("WalkBlocks size for %s = %d, want %d", blockID, meta.Size, wantSize)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}
	if len(seen) != len(want) {
		t.Fatalf("WalkBlocks visited %d blocks, want %d", len(seen), len(want))
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("WalkBlocks visited %s %d times, want 1", id, n)
		}
	}
}

// TestStore_WalkBlocks_StopWalk pins the ErrStopWalk clean-exit contract.
func TestStore_WalkBlocks_StopWalk(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("blk-stop-%d", i)
		if err := s.PutBlock(ctx, id, bytes.NewReader([]byte(id))); err != nil {
			t.Fatalf("PutBlock: %v", err)
		}
	}
	seen := 0
	err := s.WalkBlocks(ctx, func(string, block.Meta) error {
		seen++
		return block.ErrStopWalk
	})
	if err != nil {
		t.Fatalf("WalkBlocks ErrStopWalk: want nil, got %v", err)
	}
	if seen != 1 {
		t.Fatalf("WalkBlocks should stop after first ErrStopWalk; saw %d", seen)
	}
}

// TestStore_WalkBlocks_ErrorWrap pins the "walk halted at <blockID>: %w"
// wrapping contract for a non-sentinel callback error.
func TestStore_WalkBlocks_ErrorWrap(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if err := s.PutBlock(ctx, "blk-wrap", bytes.NewReader([]byte("data"))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	sentinel := errors.New("boom")
	err := s.WalkBlocks(ctx, func(string, block.Meta) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("WalkBlocks err does not wrap sentinel: got %v", err)
	}
	if !strings.Contains(err.Error(), "walk halted at") {
		t.Errorf("WalkBlocks err missing 'walk halted at' prefix: %q", err.Error())
	}
}

// TestStore_PutBlock_Idempotent verifies a second PutBlock with the same
// blockID silently overwrites with the new content.
func TestStore_PutBlock_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	original := []byte("original-content")
	updated := []byte("updated-content-v2")
	if err := s.PutBlock(ctx, "blk-idem", bytes.NewReader(original)); err != nil {
		t.Fatalf("PutBlock original: %v", err)
	}
	if err := s.PutBlock(ctx, "blk-idem", bytes.NewReader(updated)); err != nil {
		t.Fatalf("PutBlock updated: %v", err)
	}
	got, err := s.GetBlock(ctx, "blk-idem")
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if !bytes.Equal(got, updated) {
		t.Fatalf("PutBlock idempotent: got %q, want %q", got, updated)
	}
}

// TestStore_PutBlock_ZeroBody verifies a zero-byte body is accepted and
// round-trips correctly.
func TestStore_PutBlock_ZeroBody(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	if err := s.PutBlock(ctx, "blk-zero", bytes.NewReader(nil)); err != nil {
		t.Fatalf("PutBlock zero-byte: %v", err)
	}
	got, err := s.GetBlock(ctx, "blk-zero")
	if err != nil {
		t.Fatalf("GetBlock zero-byte: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetBlock zero-byte: want 0 bytes, got %d", len(got))
	}
}

// TestStore_PutBlock_Concurrent_SameID verifies concurrent PutBlock calls for
// the same blockID do not race or corrupt the store. The final stored value must
// be one of the two payloads (not a blend), and GetBlock must return consistent
// bytes.
func TestStore_PutBlock_Concurrent_SameID(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	const id = "blk-concurrent"
	payloadA := bytes.Repeat([]byte{0xAA}, 1024)
	payloadB := bytes.Repeat([]byte{0xBB}, 1024)

	done := make(chan error, 2)
	go func() { done <- s.PutBlock(ctx, id, bytes.NewReader(payloadA)) }()
	go func() { done <- s.PutBlock(ctx, id, bytes.NewReader(payloadB)) }()
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent PutBlock: %v", err)
		}
	}

	got, err := s.GetBlock(ctx, id)
	if err != nil {
		t.Fatalf("GetBlock after concurrent put: %v", err)
	}
	// Must be one of the two payloads, all-same-byte.
	if len(got) != 1024 {
		t.Fatalf("GetBlock length = %d, want 1024", len(got))
	}
	for _, b := range got {
		if b != 0xAA && b != 0xBB {
			t.Fatalf("GetBlock returned unexpected byte 0x%02X (not 0xAA or 0xBB)", b)
		}
	}
	// All bytes must be the same value (no interleaving).
	first := got[0]
	for i, b := range got {
		if b != first {
			t.Fatalf("GetBlock[%d] = 0x%02X, want all 0x%02X (concurrent blend)", i, b, first)
		}
	}
}
