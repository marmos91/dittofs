// Native Go benchmarks for the blockstore engine workloads. Each
// Benchmark* func reuses the exported engine fixture (NewEngine) and
// the per-op shape mirrors RunWorkload's step function 1:1, but with
// seeding hoisted out of the timed region (b.ResetTimer) so b.N
// measures only the per-op cost — same shape as the legacy
// internal/bench file this replaces.
//
// b.SetBytes is set to the per-op block size so benchstat / -benchmem
// throughput columns reflect the actual data path. The memory remote
// is used; S3 paths are exercised by the cmd CLI only.

package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
)

func newBenchEngine(tb testing.TB) *engine.Store {
	tb.Helper()
	bs, closeFn, err := NewEngine(tb.TempDir(), remotememory.New())
	if err != nil {
		tb.Fatalf("NewEngine: %v", err)
	}
	tb.Cleanup(closeFn)
	return bs
}

// seedFile writes a single deterministic payload of `size` bytes at
// offset 0 against payloadID. Used by random-write / mixed-rw benches
// to establish a working set before the timed region.
func seedFile(tb testing.TB, bs *engine.Store, payloadID string, size int) {
	tb.Helper()
	data := seededBytes(0x9E3779B97F4A7C15, size)
	if _, err := bs.WriteAt(context.Background(), payloadID, nil, data, 0); err != nil {
		tb.Fatalf("seed %s: %v", payloadID, err)
	}
}

// BenchmarkSequentialWrite8MB mirrors the harness sequential-write
// workload: monotonic offset, 8 MiB blocks, single payload.
func BenchmarkSequentialWrite8MB(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(DefaultSeqBlockSize)
	bs := newBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, DefaultSeqBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	const pid = "bench/seq/0"
	var off uint64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillRandom(rng, buf)
		if _, err := bs.WriteAt(ctx, pid, nil, buf, off); err != nil {
			b.Fatalf("WriteAt i=%d: %v", i, err)
		}
		off += uint64(len(buf))
	}
}

// BenchmarkRandomWrite4KB mirrors the harness random-write workload:
// seeded 64 MiB file, 4 KiB blocks at uniform-random offsets.
func BenchmarkRandomWrite4KB(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(DefaultRandomBlockSize)
	bs := newBenchEngine(b)
	ctx := context.Background()
	const pid = "bench/rand/0"
	seedFile(b, bs, pid, RandomWriteFileSize)
	buf := make([]byte, DefaultRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(RandomWriteFileSize - DefaultRandomBlockSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillRandom(rng, buf)
		if _, err := bs.WriteAt(ctx, pid, nil, buf, rng.Uint64N(maxOff+1)); err != nil {
			b.Fatalf("WriteAt i=%d: %v", i, err)
		}
	}
}

// BenchmarkDedupHeavy mirrors the harness dedup-heavy workload: same
// 8 MiB block bytes written across N distinct payloads to exercise
// file-level dedup. Flush per op so the rollup → CAS path runs.
func BenchmarkDedupHeavy(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(DefaultSeqBlockSize)
	bs := newBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, DefaultSeqBlockSize)
	fillRandom(rand.New(rand.NewPCG(1, 0)), buf)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pid := fmt.Sprintf("bench/dedup/%d", i)
		if _, err := bs.WriteAt(ctx, pid, nil, buf, 0); err != nil {
			b.Fatalf("WriteAt i=%d: %v", i, err)
		}
		if _, err := bs.Flush(ctx, pid); err != nil {
			b.Fatalf("Flush i=%d: %v", i, err)
		}
	}
}

// BenchmarkMixedRW mirrors the harness mixed-rw workload: 50/50
// random read/write over a seeded 32 MiB file, 4 KiB blocks.
func BenchmarkMixedRW(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(DefaultRandomBlockSize)
	bs := newBenchEngine(b)
	ctx := context.Background()
	const pid = "bench/mixed/0"
	seedFile(b, bs, pid, MixedRWFileSize)
	wbuf := make([]byte, DefaultRandomBlockSize)
	rbuf := make([]byte, DefaultRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(MixedRWFileSize - DefaultRandomBlockSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		off := rng.Uint64N(maxOff + 1)
		if i%2 == 0 {
			fillRandom(rng, wbuf)
			if _, err := bs.WriteAt(ctx, pid, nil, wbuf, off); err != nil {
				b.Fatalf("WriteAt i=%d: %v", i, err)
			}
			continue
		}
		if _, err := bs.ReadAt(ctx, pid, nil, rbuf, off); err != nil {
			b.Fatalf("ReadAt i=%d: %v", i, err)
		}
	}
}

// BenchmarkFlushChurn mirrors the harness flush-churn workload:
// write→flush→write tight loop with monotonic offset, single payload.
func BenchmarkFlushChurn(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(DefaultRandomBlockSize)
	bs := newBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, DefaultRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	const pid = "bench/churn/0"
	var off uint64
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		fillRandom(rng, buf)
		if _, err := bs.WriteAt(ctx, pid, nil, buf, off); err != nil {
			b.Fatalf("WriteAt i=%d: %v", i, err)
		}
		if _, err := bs.Flush(ctx, pid); err != nil {
			b.Fatalf("Flush i=%d: %v", i, err)
		}
		off += uint64(len(buf))
	}
}
