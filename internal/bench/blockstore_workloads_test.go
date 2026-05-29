// Native Go benchmarks for the blockstore engine, mirroring the per-op
// shapes of cmd/blockstore-perf so benchstat can A/B-compare commits
// without invoking the external harness. Engine wiring matches the
// harness 1:1 (FSStore local + memory remote + memory metadata +
// Syncer, 256 MiB log / 64 MiB mem / 2 rollup workers).
//
// The buffers are filled with PRNG bytes (not zeros) so CAS dedup
// doesn't swallow most writes — same lesson as the harness Copilot fix.
//
// Duplication with cmd/blockstore-perf is intentional at v0: a shared
// workloads package can be extracted later once the interface is clear.

package bench

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

const (
	workloadLogBudget       = 256 * 1024 * 1024
	workloadMemBudget       = 64 * 1024 * 1024
	workloadSeqBlockSize    = 8 * 1024 * 1024
	workloadRandomBlockSize = 4 * 1024
	workloadRandFileSize    = 64 * 1024 * 1024
	workloadMixedFileSize   = 32 * 1024 * 1024
)

// newWorkloadBlockStore wires production-equivalent FSStore + memory
// remote + memory metadata + Syncer for benchmarks. Mirrors
// cmd/blockstore-perf/main.go::newBlockStore.
func newWorkloadBlockStore(tb testing.TB) *engine.BlockStore {
	tb.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(tb.TempDir(), 0, workloadMemBudget, ms, fs.FSStoreOptions{
		MaxLogBytes:     workloadLogBudget,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		tb.Fatalf("fs.NewWithOptions: %v", err)
	}
	if err := local.StartRollup(context.Background()); err != nil {
		tb.Fatalf("StartRollup: %v", err)
	}
	remoteStore := remotememory.New()
	syncer := engine.NewSyncer(local, remoteStore, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          remoteStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		tb.Fatalf("engine.New: %v", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		tb.Fatalf("engine.Start: %v", err)
	}
	tb.Cleanup(func() { _ = bs.Close() })
	return bs
}

// fillRandom overwrites buf with deterministic PRNG bytes drawn 8 at
// a time. Defeats CAS dedup that would otherwise swallow most ops.
// Mirrors cmd/blockstore-perf/main.go::fillRandom.
func fillRandom(rng *rand.Rand, buf []byte) {
	i := 0
	for ; i+8 <= len(buf); i += 8 {
		v := rng.Uint64()
		buf[i] = byte(v)
		buf[i+1] = byte(v >> 8)
		buf[i+2] = byte(v >> 16)
		buf[i+3] = byte(v >> 24)
		buf[i+4] = byte(v >> 32)
		buf[i+5] = byte(v >> 40)
		buf[i+6] = byte(v >> 48)
		buf[i+7] = byte(v >> 56)
	}
	if i < len(buf) {
		v := rng.Uint64()
		for ; i < len(buf); i++ {
			buf[i] = byte(v)
			v >>= 8
		}
	}
}

// seedFile writes a single deterministic payload of `size` bytes at
// offset 0 against payloadID. Used by random-write / mixed-rw benches
// to establish a working set before the timed region.
func seedFile(tb testing.TB, bs *engine.BlockStore, payloadID string, size int) {
	tb.Helper()
	data := make([]byte, size)
	fillRandom(rand.New(rand.NewPCG(0x9E3779B97F4A7C15, 0)), data)
	if _, err := bs.WriteAt(context.Background(), payloadID, nil, data, 0); err != nil {
		tb.Fatalf("seed %s: %v", payloadID, err)
	}
}

// BenchmarkSequentialWrite8MB mirrors the harness sequential-write
// workload: monotonic offset, 8 MiB blocks, single payload.
func BenchmarkSequentialWrite8MB(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(workloadSeqBlockSize)
	bs := newWorkloadBlockStore(b)
	ctx := context.Background()
	buf := make([]byte, workloadSeqBlockSize)
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
	b.SetBytes(workloadRandomBlockSize)
	bs := newWorkloadBlockStore(b)
	ctx := context.Background()
	const pid = "bench/rand/0"
	seedFile(b, bs, pid, workloadRandFileSize)
	buf := make([]byte, workloadRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(workloadRandFileSize - workloadRandomBlockSize)
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
	b.SetBytes(workloadSeqBlockSize)
	bs := newWorkloadBlockStore(b)
	ctx := context.Background()
	buf := make([]byte, workloadSeqBlockSize)
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
	b.SetBytes(workloadRandomBlockSize)
	bs := newWorkloadBlockStore(b)
	ctx := context.Background()
	const pid = "bench/mixed/0"
	seedFile(b, bs, pid, workloadMixedFileSize)
	wbuf := make([]byte, workloadRandomBlockSize)
	rbuf := make([]byte, workloadRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(workloadMixedFileSize - workloadRandomBlockSize)
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
	b.SetBytes(workloadRandomBlockSize)
	bs := newWorkloadBlockStore(b)
	ctx := context.Background()
	buf := make([]byte, workloadRandomBlockSize)
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
