package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/blockstoretest"
)

// TestMemory_RemoteBlockStoreConformance runs the unified
// RemoteBlockStoreConformance suite against the in-memory backend. All
// subtests run in-process with no I/O; they cover the block-keyed surface:
// PutBlock/GetBlock/GetBlockRange/DeleteBlock/WalkBlocks.
//
// The inline TestStore_* tests below remain in place as a fine-grained
// per-method baseline (data-isolation defensive copies, closed-store
// rejection paths) - they exercise backend-specific behaviors that are not
// part of the unified contract.
func TestMemory_RemoteBlockStoreConformance(t *testing.T) {
	blockstoretest.RemoteBlockStoreConformance(t, func(t *testing.T) (blockstoretest.RemoteBlockStore, func()) {
		t.Helper()
		s := New()
		return s, func() { _ = s.Close() }
	})
}

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

func TestStore_ClosedOperations(t *testing.T) {
	ctx := context.Background()
	s := New()

	if err := s.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := s.PutBlock(ctx, "blk", bytes.NewReader([]byte("data"))); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("PutBlock on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if _, err := s.GetBlock(ctx, "blk"); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("GetBlock on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if _, err := s.GetBlockRange(ctx, "blk", 0, 4); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("GetBlockRange on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if _, err := s.ReadChunk(ctx, "blk", 0, 4, block.ContentHash{}); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("ReadChunk on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if err := s.DeleteBlock(ctx, "blk"); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("DeleteBlock on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if err := s.WalkBlocks(ctx, func(string, block.Meta) error { return nil }); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("WalkBlocks on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
	if err := s.HealthCheck(ctx); !errors.Is(err, block.ErrStoreClosed) {
		t.Errorf("HealthCheck on closed store returned %v, want %v", err, block.ErrStoreClosed)
	}
}

// TestStore_DataIsolation pins the defensive-copy contract: the store must
// not alias the caller's buffer on the way in, nor hand out a slice that
// aliases its own storage on the way out.
func TestStore_DataIsolation(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("hello world")
	if err := s.PutBlock(ctx, "blk-iso", bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock failed: %v", err)
	}

	// Mutating the source buffer must not reach the stored bytes.
	data[0] = 'X'

	read, err := s.GetBlock(ctx, "blk-iso")
	if err != nil {
		t.Fatalf("GetBlock failed: %v", err)
	}
	if read[0] != 'h' {
		t.Errorf("PutBlock did not copy data: got %c, want 'h'", read[0])
	}

	// Mutating a returned slice must not reach the stored bytes either.
	read[0] = 'Y'

	read2, err := s.GetBlock(ctx, "blk-iso")
	if err != nil {
		t.Fatalf("GetBlock failed: %v", err)
	}
	if read2[0] != 'h' {
		t.Errorf("GetBlock did not copy data: got %c, want 'h'", read2[0])
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
