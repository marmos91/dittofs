package memory

import (
	"bytes"
	"context"
	"errors"
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
// The inline TestStore_* tests below cover only what the suite does not
// reach: the chunk-keyed ReadChunk surface, closed-store rejection, the
// defensive copy on the way IN to PutBlock, the two bounds cases the suite
// deliberately leaves loose, and the WalkBlocks error-wrapping format.
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

// TestStore_GetBlockRange_Bounds pins the two bounds behaviours the shared
// conformance suite cannot assert. The suite only requires SOME error for an
// offset at EOF, because a remote that reads the object without a pre-flight
// HEAD surfaces the backend's own status instead; this store holds the whole
// body, so it must return ErrInvalidOffset exactly. And the suite's past-EOF
// case uses a small length, which passes even if offset+length overflows.
func TestStore_GetBlockRange_Bounds(t *testing.T) {
	ctx := context.Background()
	s := New()
	defer func() { _ = s.Close() }()

	data := []byte("0123456789abcdef") // 16 bytes
	const id = "blk-range"
	if err := s.PutBlock(ctx, id, bytes.NewReader(data)); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// Offset at EOF: ErrInvalidOffset.
	if _, err := s.GetBlockRange(ctx, id, 16, 4); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("offset=EOF: want ErrInvalidOffset, got %v", err)
	}

	// MaxInt64 length clamps without overflow.
	got, err := s.GetBlockRange(ctx, id, 8, math.MaxInt64)
	if err != nil {
		t.Fatalf("GetBlockRange MaxInt64 length: %v", err)
	}
	if string(got) != "89abcdef" {
		t.Fatalf("GetBlockRange MaxInt64 clamp = %q, want %q", got, "89abcdef")
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
