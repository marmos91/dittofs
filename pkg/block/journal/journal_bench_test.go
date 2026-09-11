package journal

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func benchStore(b *testing.B) *Store {
	b.Helper()
	s, _ := benchStoreDir(b, Config{})
	return s
}

// BenchmarkWriteAt measures the dirty-write append path with a 64 KiB payload.
func BenchmarkWriteAt(b *testing.B) {
	s, dir := benchStoreDir(b, Config{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 64<<10)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.WriteAt(ctx, "bench-file", int64(i)*int64(len(data)), data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
	}
	b.StopTimer()
	reportWriteAmp(b, s, dir, "bench-file", int64(b.N)*int64(len(data)))
}

// BenchmarkTinyWritesCommit measures the many-tiny-scattered-writes-then-COMMIT
// burst that pays full per-record framing overhead before any record merge.
func BenchmarkTinyWritesCommit(b *testing.B) {
	s, dir := benchStoreDir(b, Config{})
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 512)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.WriteAt(ctx, "bench-tiny", int64(i)*int64(len(data)), data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
		if err := s.Commit(ctx, "bench-tiny"); err != nil {
			b.Fatalf("Commit: %v", err)
		}
	}
	b.StopTimer()
	reportWriteAmp(b, s, dir, "bench-tiny", int64(b.N)*int64(len(data)))
}

// BenchmarkReadWarm measures the warm-read path (index lookup + pread) over a
// pre-populated file.
func BenchmarkReadWarm(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	chunk := 64 << 10
	data := bytes.Repeat([]byte("y"), chunk)
	const spans = 256
	for i := 0; i < spans; i++ {
		if err := s.WriteAt(ctx, "warm", int64(i)*int64(chunk), data); err != nil {
			b.Fatalf("seed WriteAt: %v", err)
		}
	}

	dst := make([]byte, chunk)
	b.SetBytes(int64(chunk))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := int64(i%spans) * int64(chunk)
		if _, _, err := s.ReadAt(ctx, "warm", off, dst); err != nil {
			b.Fatalf("ReadAt: %v", err)
		}
	}
}

// BenchmarkCarve measures the FastCDC+BLAKE3+dedup+commit carve of an 8 MiB file.
// The deduper always misses and the sink is a no-op, so every pass does the full
// chunk-hash-pack work (the worst case) rather than short-circuiting on dedup.
func BenchmarkCarve(b *testing.B) {
	s, err := Open(b.TempDir(), Config{})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	s.SetCarveTargets(missDeduper{}, nopSink{})

	ctx := context.Background()
	data := bytes.Repeat([]byte("dittofs-journal-carve-"), (8<<20)/22)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		id := FileID("carve-" + string(rune('a'+i%26)) + time.Duration(i).String())
		if err := s.WriteAt(ctx, id, 0, data); err != nil {
			b.Fatalf("seed WriteAt: %v", err)
		}
		b.StartTimer()
		if _, err := s.Carve(ctx, CarveOptions{FileID: id, Force: true}); err != nil {
			b.Fatalf("Carve: %v", err)
		}
	}
}

// missDeduper reports every chunk novel, forcing the full pack path each pass.
type missDeduper struct{}

func (missDeduper) IsChunkDurable(context.Context, ChunkHash) (bool, error) { return false, nil }

// nopSink discards committed chunks — the benchmark measures journal's carve
// work, not a remote round-trip.
type nopSink struct{}

func (nopSink) CommitBlock(context.Context, []CarveChunk) error { return nil }
