package chunker

import (
	"math/rand"
	"testing"
)

// BenchmarkChunker_Throughput_64MiB chunks a 64 MiB pseudo-random buffer
// per iteration and reports throughput via SetBytes.
func BenchmarkChunker_Throughput_64MiB(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 64*1024*1024)
	_, _ = rng.Read(data)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		c := NewChunker()
		pos := 0
		for pos < len(data) {
			bnd, _ := c.Next(data[pos:], true)
			if bnd == 0 {
				b.Fatalf("zero boundary at pos %d", pos)
			}
			pos += bnd
		}
	}
}

// TestChunker_ConstantMemory pins the chunker's memory invariant: scanning a
// buffer for boundaries allocates nothing on the heap, independent of input
// size. Next reports offsets into the caller's slice — it must never copy chunk
// bytes. A regression here (e.g. Next starting to buffer or return copies)
// turns per-block streaming into per-file RAM — exactly the silent memory
// blow-up the #1555 per-stage profiling exists to catch.
func TestChunker_ConstantMemory(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	data := make([]byte, 16*1024*1024) // 16 MiB → many boundaries
	_, _ = rng.Read(data)

	chunkAll := func() {
		c := NewChunker()
		pos := 0
		for pos < len(data) {
			bnd, _ := c.Next(data[pos:], true)
			if bnd == 0 {
				t.Fatalf("zero boundary at pos %d", pos)
			}
			pos += bnd
		}
	}

	if allocs := testing.AllocsPerRun(5, chunkAll); allocs != 0 {
		t.Fatalf("chunking allocated %.0f objects/run; want 0 (Next must scan in place, not copy)", allocs)
	}
}
