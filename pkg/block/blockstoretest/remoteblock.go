package blockstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/remote"
)

// RemoteBlockStore is re-exported from remote.RemoteBlockStore so that
// test files only need to import blockstoretest (not pkg/block/remote).
type RemoteBlockStore = remote.RemoteBlockStore

// RemoteBlockStoreFactory creates a fresh RemoteBlockStore per subtest.
// The returned cleanup function must release all resources. Implementations
// must not share state across factory calls.
type RemoteBlockStoreFactory func(t *testing.T) (RemoteBlockStore, func())

// RemoteBlockStoreConformance runs the unified contract suite for
// remote.RemoteBlockStore. Every method is covered by at least one subtest.
// Wire one backend at a time; if an assertion fails against a correct backend,
// fix the suite — only stop and report a concern if you find a genuine backend
// bug.
//
// Subtests:
//
//   - PutBlock_GetBlock_RoundTrip — happy-path idempotent PutBlock then GetBlock.
//   - GetBlock_NoAliasing — mutating a returned slice must not change the stored bytes.
//   - GetBlock_NotFound — absent blockID returns ErrChunkNotFound.
//   - GetBlockRange_Mid — range read from the middle of a stored block.
//   - GetBlockRange_PastEOFClamped — length past EOF is clamped without error.
//   - GetBlockRange_InvalidOffset — negative offset returns ErrInvalidOffset (a past-EOF
//     offset cannot be detected without a HEAD, so backends may surface a native error).
//   - GetBlockRange_InvalidSize — zero / negative length returns ErrInvalidSize.
//   - GetBlockRange_ZeroLength — explicit zero-length returns ErrInvalidSize.
//   - GetBlockRange_NotFound — absent blockID returns ErrChunkNotFound.
//   - DeleteBlock_Durable — delete removes the block durably.
//   - DeleteBlock_Idempotent — deleting an absent blockID returns nil.
//   - WalkBlocks_EnumeratesAll — every PutBlock'd object is visited exactly once.
//   - WalkBlocks_ErrStopWalk — ErrStopWalk halts enumeration cleanly (returns nil).
//   - PutBlock_Idempotent — second PutBlock for same ID overwrites content.
//   - PutBlock_ZeroBody — zero-byte block is accepted; GetBlock returns empty slice.
//   - PutBlock_Concurrent_SameID — concurrent same-ID PutBlocks produce a coherent result.
func RemoteBlockStoreConformance(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()

	t.Run("PutBlock_GetBlock_RoundTrip", func(t *testing.T) {
		testRBSPutGetRoundTrip(t, factory)
	})
	t.Run("GetBlock_NoAliasing", func(t *testing.T) {
		testRBSGetNoAliasing(t, factory)
	})
	t.Run("GetBlock_NotFound", func(t *testing.T) {
		testRBSGetNotFound(t, factory)
	})
	t.Run("GetBlockRange_Mid", func(t *testing.T) {
		testRBSGetBlockRangeMid(t, factory)
	})
	t.Run("GetBlockRange_PastEOFClamped", func(t *testing.T) {
		testRBSGetBlockRangePastEOFClamped(t, factory)
	})
	t.Run("GetBlockRange_InvalidOffset", func(t *testing.T) {
		testRBSGetBlockRangeInvalidOffset(t, factory)
	})
	t.Run("GetBlockRange_InvalidSize", func(t *testing.T) {
		testRBSGetBlockRangeInvalidSize(t, factory)
	})
	t.Run("GetBlockRange_ZeroLength", func(t *testing.T) {
		testRBSGetBlockRangeZeroLength(t, factory)
	})
	t.Run("GetBlockRange_NotFound", func(t *testing.T) {
		testRBSGetBlockRangeNotFound(t, factory)
	})
	t.Run("DeleteBlock_Durable", func(t *testing.T) {
		testRBSDeleteBlockDurable(t, factory)
	})
	t.Run("DeleteBlock_Idempotent", func(t *testing.T) {
		testRBSDeleteBlockIdempotent(t, factory)
	})
	t.Run("WalkBlocks_EnumeratesAll", func(t *testing.T) {
		testRBSWalkBlocksEnumeratesAll(t, factory)
	})
	t.Run("WalkBlocks_ErrStopWalk", func(t *testing.T) {
		testRBSWalkBlocksStopWalk(t, factory)
	})
	t.Run("PutBlock_Idempotent", func(t *testing.T) {
		testRBSPutBlockIdempotent(t, factory)
	})
	t.Run("PutBlock_ZeroBody", func(t *testing.T) {
		testRBSPutBlockZeroBody(t, factory)
	})
	t.Run("PutBlock_Concurrent_SameID", func(t *testing.T) {
		testRBSPutBlockConcurrentSameID(t, factory)
	})
}

// ---- subtest implementations ----

func testRBSPutGetRoundTrip(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("conformance: RemoteBlockStore round-trip payload")
	if err := s.PutBlock(ctx, "blk-rtrip", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	got, err := s.GetBlock(ctx, "blk-rtrip")
	if err != nil {
		t.Fatalf("GetBlock: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("GetBlock = %q, want %q", got, data)
	}
}

func testRBSGetNoAliasing(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	original := []byte("aliasing-guard payload for RemoteBlockStore")
	if err := s.PutBlock(ctx, "blk-alias", strings.NewReader(string(original))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got1, err := s.GetBlock(ctx, "blk-alias")
	if err != nil {
		t.Fatalf("GetBlock #1: %v", err)
	}
	// Mutate the returned slice — the store must not share its internal buffer.
	got1[0] ^= 0xFF

	got2, err := s.GetBlock(ctx, "blk-alias")
	if err != nil {
		t.Fatalf("GetBlock #2: %v", err)
	}
	if !bytes.Equal(got2, original) {
		t.Fatalf("GetBlock aliasing: mutating first-Get slice changed second-Get result")
	}
}

func testRBSGetNotFound(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	if _, err := s.GetBlock(ctx, "absent-block-id"); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetBlock absent: want ErrChunkNotFound, got %v", err)
	}
}

func testRBSGetBlockRangeMid(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("0123456789abcdef") // 16 bytes
	if err := s.PutBlock(ctx, "blk-range", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	got, err := s.GetBlockRange(ctx, "blk-range", 4, 8)
	if err != nil {
		t.Fatalf("GetBlockRange mid: %v", err)
	}
	want := data[4:12] // "456789ab"
	if !bytes.Equal(got, want) {
		t.Fatalf("GetBlockRange mid = %q, want %q", got, want)
	}
}

func testRBSGetBlockRangePastEOFClamped(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("0123456789abcdef") // 16 bytes
	if err := s.PutBlock(ctx, "blk-clamp", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// offset=8, length=100: runs past the 16-byte EOF; expect the remaining 8 bytes.
	got, err := s.GetBlockRange(ctx, "blk-clamp", 8, 100)
	if err != nil {
		t.Fatalf("GetBlockRange past-EOF length: %v", err)
	}
	want := data[8:] // "89abcdef"
	if !bytes.Equal(got, want) {
		t.Fatalf("GetBlockRange clamped = %q, want %q", got, want)
	}
}

func testRBSGetBlockRangeInvalidOffset(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("0123456789abcdef")
	if err := s.PutBlock(ctx, "blk-invoff", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// Negative offset: must be caught before the wire call and return ErrInvalidOffset.
	if _, err := s.GetBlockRange(ctx, "blk-invoff", -1, 4); !errors.Is(err, block.ErrInvalidOffset) {
		t.Fatalf("GetBlockRange offset=-1: want ErrInvalidOffset, got %v", err)
	}
	// Offset strictly past EOF: some backends (S3) cannot detect this without
	// a pre-flight HEAD and instead surface a native 416. The contract only
	// guarantees some error — not necessarily ErrInvalidOffset — for this case.
	// See also pkg/block/remote/s3/store.go GetBlockRange comment.
	if _, err := s.GetBlockRange(ctx, "blk-invoff", int64(len(data)), 4); err == nil {
		t.Fatal("GetBlockRange offset=EOF: want error, got nil")
	}
}

func testRBSGetBlockRangeInvalidSize(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("0123456789abcdef")
	if err := s.PutBlock(ctx, "blk-invsize", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// Negative length.
	if _, err := s.GetBlockRange(ctx, "blk-invsize", 0, -4); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("GetBlockRange length=-4: want ErrInvalidSize, got %v", err)
	}
}

func testRBSGetBlockRangeZeroLength(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("0123456789abcdef")
	if err := s.PutBlock(ctx, "blk-zerolen", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	if _, err := s.GetBlockRange(ctx, "blk-zerolen", 0, 0); !errors.Is(err, block.ErrInvalidSize) {
		t.Fatalf("GetBlockRange length=0: want ErrInvalidSize, got %v", err)
	}
}

func testRBSGetBlockRangeNotFound(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	if _, err := s.GetBlockRange(ctx, "absent-block", 0, 4); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetBlockRange absent: want ErrChunkNotFound, got %v", err)
	}
}

func testRBSDeleteBlockDurable(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	data := []byte("to-be-deleted block content")
	if err := s.PutBlock(ctx, "blk-del", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}

	// Confirm presence before delete.
	if _, err := s.GetBlock(ctx, "blk-del"); err != nil {
		t.Fatalf("GetBlock before DeleteBlock: %v", err)
	}

	if err := s.DeleteBlock(ctx, "blk-del"); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}

	// Block must be gone.
	if _, err := s.GetBlock(ctx, "blk-del"); !errors.Is(err, block.ErrChunkNotFound) {
		t.Fatalf("GetBlock after DeleteBlock: want ErrChunkNotFound, got %v", err)
	}
}

func testRBSDeleteBlockIdempotent(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	// Delete absent block — must not error.
	if err := s.DeleteBlock(ctx, "never-existed"); err != nil {
		t.Fatalf("DeleteBlock on absent block: want nil, got %v", err)
	}

	// Put then delete twice.
	data := []byte("idempotent-delete payload")
	if err := s.PutBlock(ctx, "blk-idem-del", strings.NewReader(string(data))); err != nil {
		t.Fatalf("PutBlock: %v", err)
	}
	if err := s.DeleteBlock(ctx, "blk-idem-del"); err != nil {
		t.Fatalf("DeleteBlock first: %v", err)
	}
	if err := s.DeleteBlock(ctx, "blk-idem-del"); err != nil {
		t.Fatalf("DeleteBlock second (idempotent): %v", err)
	}
}

func testRBSWalkBlocksEnumeratesAll(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	want := make(map[string]int64)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("walk-blk-%d", i)
		data := []byte(fmt.Sprintf("walk-payload-%d-xxxx", i))
		want[id] = int64(len(data))
		if err := s.PutBlock(ctx, id, strings.NewReader(string(data))); err != nil {
			t.Fatalf("PutBlock %s: %v", id, err)
		}
	}

	seen := make(map[string]int)
	err := s.WalkBlocks(ctx, func(blockID string, m block.Meta) error {
		seen[blockID]++
		if m.LastModified.IsZero() {
			t.Errorf("WalkBlocks: LastModified is zero for %s", blockID)
		}
		if w, ok := want[blockID]; ok && m.Size != w {
			t.Errorf("WalkBlocks: Size for %s = %d, want %d", blockID, m.Size, w)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkBlocks: %v", err)
	}

	if len(seen) != len(want) {
		t.Fatalf("WalkBlocks visited %d blocks, want %d; seen=%v", len(seen), len(want), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("WalkBlocks: block %s visited %d times, want 1", id, n)
		}
	}
}

func testRBSWalkBlocksStopWalk(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("stop-blk-%d", i)
		if err := s.PutBlock(ctx, id, strings.NewReader(id)); err != nil {
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
		t.Errorf("WalkBlocks: expected to stop after first callback; saw %d", seen)
	}
}

func testRBSPutBlockIdempotent(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	original := []byte("original-block-v1")
	updated := []byte("updated-block-v2-xxxx")

	if err := s.PutBlock(ctx, "blk-idem", strings.NewReader(string(original))); err != nil {
		t.Fatalf("PutBlock original: %v", err)
	}
	if err := s.PutBlock(ctx, "blk-idem", strings.NewReader(string(updated))); err != nil {
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

func testRBSPutBlockZeroBody(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	if err := s.PutBlock(ctx, "blk-zero", strings.NewReader("")); err != nil {
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

func testRBSPutBlockConcurrentSameID(t *testing.T, factory RemoteBlockStoreFactory) {
	t.Helper()
	s, cleanup := factory(t)
	t.Cleanup(cleanup)
	ctx := context.Background()

	const id = "blk-concurrent"
	payloadA := strings.Repeat("A", 512)
	payloadB := strings.Repeat("B", 512)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = s.PutBlock(ctx, id, strings.NewReader(payloadA))
	}()
	go func() {
		defer wg.Done()
		errs[1] = s.PutBlock(ctx, id, strings.NewReader(payloadB))
	}()
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent PutBlock[%d]: %v", i, err)
		}
	}

	got, err := s.GetBlock(ctx, id)
	if err != nil {
		t.Fatalf("GetBlock after concurrent put: %v", err)
	}
	// Must be entirely A or entirely B — no interleaved bytes.
	if len(got) != 512 {
		t.Fatalf("GetBlock length = %d, want 512", len(got))
	}
	first := got[0]
	if first != 'A' && first != 'B' {
		t.Fatalf("unexpected first byte 0x%02X", first)
	}
	for i, b := range got {
		if b != first {
			t.Fatalf("GetBlock[%d] = 0x%02X, want all 0x%02X (concurrent blend detected)", i, b, first)
		}
	}
}
