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
