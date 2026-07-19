// Native Go benchmarks for the blockstore engine write path. Each
// Benchmark* func wires a production-equivalent FSStore + memory remote
// + memory metadata + rollup Syncer via newWriteBenchEngine, and the
// per-op shape mirrors the legacy bench/blockstore RunWorkload step
// function 1:1 — but with seeding hoisted out of the timed region
// (b.ResetTimer) so b.N measures only the per-op cost.
//
// b.SetBytes is set to the per-op block size so benchstat / -benchmem
// throughput columns reflect the actual data path. The memory remote is
// used; S3 paths were exercised by the retired cmd CLI harness only.
package engine

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Engine wiring constant. Sized for short-lived benchmark runs:
// 256 MiB local cache budget.
const writeBenchLogBudget = 256 * 1024 * 1024

// Default block sizes — match the legacy bench/blockstore shape so
// historical results stay comparable. Sequential and dedup move 8 MiB
// per op; random/mixed/churn move 4 KiB.
const (
	writeBenchSeqBlockSize    = 8 * 1024 * 1024
	writeBenchRandomBlockSize = 4 * 1024
)

// Seeded working-set sizes for random-offset workloads.
const (
	writeBenchRandomFileSize = 64 * 1024 * 1024
	writeBenchMixedFileSize  = 32 * 1024 * 1024
)

// writeBenchSeedChunkSize bounds each seed WriteAt so a single record
// never exceeds the local store's per-record cap (~17 MiB, just above
// the chunker's 16 MiB hard ceiling). Real protocol callers already
// arrive in sub-MiB segments; only the bench seed would otherwise emit
// one multi-MiB record.
const writeBenchSeedChunkSize = 8 * 1024 * 1024

// newWriteBenchEngine wires a production-equivalent FSStore + memory
// remote + memory metadata + rollup Syncer for a single benchmark run,
// mirroring the block/shares production factory. Cleanup closes the
// engine (which also closes the remote).
func newWriteBenchEngine(tb testing.TB) *Store {
	tb.Helper()
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	localStore, err := fs.NewWithOptions(tb.TempDir(), 0, ms, fs.FSStoreOptions{
		MaxLogBytes: writeBenchLogBudget,
	})
	if err != nil {
		tb.Fatalf("fs.NewWithOptions: %v", err)
	}
	rem := remotememory.New()
	syncer := NewSyncer(localStore, rem, ms, DefaultConfig())
	bs, err := New(BlockStoreConfig{
		Local:           localStore,
		Remote:          rem,
		Syncer:          syncer,
		FileChunkStore:  ms,
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

// seedFile writes a single deterministic payload of `size` bytes at
// offset 0 against payloadID. Used by random-write / mixed-rw benches to
// establish a working set before the timed region.
func seedFile(tb testing.TB, bs *Store, payloadID string, size int) {
	tb.Helper()
	data := seededBytes(0x9E3779B97F4A7C15, size)
	if err := seedPayload(context.Background(), bs, payloadID, data); err != nil {
		tb.Fatalf("%v", err)
	}
}

// seedPayload writes data into payloadID at offset 0 in segments of at
// most writeBenchSeedChunkSize bytes so a multi-MiB working-set file
// stays under the local store's per-record cap.
func seedPayload(ctx context.Context, bs *Store, payloadID string, data []byte) error {
	for off := 0; off < len(data); off += writeBenchSeedChunkSize {
		end := off + writeBenchSeedChunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := bs.WriteAt(ctx, payloadID, nil, data[off:end], uint64(off)); err != nil {
			return fmt.Errorf("seed %s: %w", payloadID, err)
		}
	}
	return nil
}

// seededBytes returns `size` deterministic PRNG bytes for the given seed.
func seededBytes(seed uint64, size int) []byte {
	rng := rand.New(rand.NewPCG(seed, 0))
	out := make([]byte, size)
	fillRandom(rng, out)
	return out
}

// fillRandom overwrites buf with PRNG bytes drawn 8 at a time. Used
// per-op in the timed loop so payload bytes are unique across ops —
// otherwise dedup short-circuits most writes and the workload stops
// reflecting realistic block churn.
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

// BenchmarkSequentialWrite8MB mirrors the harness sequential-write
// workload: monotonic offset, 8 MiB blocks, single payload.
func BenchmarkSequentialWrite8MB(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(writeBenchSeqBlockSize)
	bs := newWriteBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, writeBenchSeqBlockSize)
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
	b.SetBytes(writeBenchRandomBlockSize)
	bs := newWriteBenchEngine(b)
	ctx := context.Background()
	const pid = "bench/rand/0"
	seedFile(b, bs, pid, writeBenchRandomFileSize)
	buf := make([]byte, writeBenchRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(writeBenchRandomFileSize - writeBenchRandomBlockSize)
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
	b.SetBytes(writeBenchSeqBlockSize)
	bs := newWriteBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, writeBenchSeqBlockSize)
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

// BenchmarkMixedRW mirrors the harness mixed-rw workload: 50/50 random
// read/write over a seeded 32 MiB file, 4 KiB blocks.
func BenchmarkMixedRW(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(writeBenchRandomBlockSize)
	bs := newWriteBenchEngine(b)
	ctx := context.Background()
	const pid = "bench/mixed/0"
	seedFile(b, bs, pid, writeBenchMixedFileSize)
	wbuf := make([]byte, writeBenchRandomBlockSize)
	rbuf := make([]byte, writeBenchRandomBlockSize)
	rng := rand.New(rand.NewPCG(1, 0))
	maxOff := uint64(writeBenchMixedFileSize - writeBenchRandomBlockSize)
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
	b.SetBytes(writeBenchRandomBlockSize)
	bs := newWriteBenchEngine(b)
	ctx := context.Background()
	buf := make([]byte, writeBenchRandomBlockSize)
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
