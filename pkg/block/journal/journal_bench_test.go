package journal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeRemote is an in-memory RemoteStore for tests and benchmarks. It buffers
// each block whole — fine for a fake, never used on a hot path.
type fakeRemote struct {
	mu     sync.Mutex
	blocks map[BlockID][]byte
}

func newFakeRemote() *fakeRemote { return &fakeRemote{blocks: make(map[BlockID][]byte)} }

func (f *fakeRemote) PutBlock(_ context.Context, id BlockID, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.blocks[id] = b
	f.mu.Unlock()
	return nil
}

func (f *fakeRemote) GetBlock(_ context.Context, id BlockID) (io.ReadCloser, error) {
	f.mu.Lock()
	b, ok := f.blocks[id]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("fakeRemote: block not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeRemote) GetRange(_ context.Context, id BlockID, off, length int64) (io.ReadCloser, error) {
	f.mu.Lock()
	b, ok := f.blocks[id]
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("fakeRemote: block not found")
	}
	if off < 0 || off > int64(len(b)) {
		return nil, errors.New("fakeRemote: range out of bounds")
	}
	end := off + length
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return io.NopCloser(bytes.NewReader(b[off:end])), nil
}

func benchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(b.TempDir(), Config{}, newFakeRemote(), SystemClock())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// BenchmarkWriteAt measures the dirty-write append path with a 64 KiB payload.
func BenchmarkWriteAt(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 64<<10)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.WriteAt(ctx, "bench-file", int64(i)*int64(len(data)), data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
	}
}

// BenchmarkTinyWritesCommit measures the many-tiny-scattered-writes-then-COMMIT
// burst that pays full per-record framing overhead before any record merge.
func BenchmarkTinyWritesCommit(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 512)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.WriteAt(ctx, "bench-tiny", int64(i)*int64(len(data)), data); err != nil {
			b.Fatalf("WriteAt: %v", err)
		}
		if err := s.Commit(ctx, "bench-tiny"); err != nil {
			b.Fatalf("Commit: %v", err)
		}
	}
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
	s, err := Open(b.TempDir(), Config{}, newFakeRemote(), newFakeClock())
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	s.SetCarveTargets(missDeduper{}, nopSink{})

	ctx := context.Background()
	data := bytes.Repeat([]byte("dittofs-journal-carve-"), (8<<20)/22)

	b.SetBytes(int64(len(data)))
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
