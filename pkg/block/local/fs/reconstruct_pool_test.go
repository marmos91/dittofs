package fs

import (
	"testing"
)

// TestReconstructBuf_CheckoutZeroed proves getReconstructBuf returns a
// zeroed buffer even when a dirtied buffer of the same size class was
// previously returned to the pool. reconstructStream relies on zero-fill
// for sparse gaps (FIX-3), so a recycled buffer MUST NOT leak stale bytes.
func TestReconstructBuf_CheckoutZeroed(t *testing.T) {
	const size = 4 << 20 // 4 MiB, lands in the small bucket

	buf := getReconstructBuf(size)
	for i := range buf {
		buf[i] = 0xAB
	}
	putReconstructBuf(buf)

	reused := getReconstructBuf(size)
	for i, b := range reused {
		if b != 0 {
			t.Fatalf("recycled buffer not zeroed at index %d: got 0x%02x", i, b)
		}
	}
	putReconstructBuf(reused)
}

// TestReconstructBuf_SizeExact proves the returned buffer length equals the
// requested size (not the bucket capacity), so callers can index [0,size).
func TestReconstructBuf_SizeExact(t *testing.T) {
	for _, size := range []uint64{1, 1 << 20, 64 << 20, 200 << 20} {
		buf := getReconstructBuf(size)
		if uint64(len(buf)) != size {
			t.Fatalf("size %d: got len %d", size, len(buf))
		}
		putReconstructBuf(buf)
	}
}

// TestReconstructBuf_ReuseNoAlloc proves a pool round-trip serves the next
// checkout of the same size class without a heap allocation.
func TestReconstructBuf_ReuseNoAlloc(t *testing.T) {
	const size = 8 << 20 // small bucket

	// Prime the pool with one buffer.
	putReconstructBuf(getReconstructBuf(size))

	allocs := testing.AllocsPerRun(100, func() {
		buf := getReconstructBuf(size)
		putReconstructBuf(buf)
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs on pool hit, got %v", allocs)
	}
}
