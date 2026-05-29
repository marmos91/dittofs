// BenchmarkRandWriteCAS_IdempotentBytes measures the in-memory hash
// dedup LRU's effectiveness on idempotent rewrites: K rollup passes
// of identical content under the SAME payloadID. On the first pass
// the LRU is cold; StoreChunk fires once per emitted chunk, and the
// LRU is seeded by PutMany after the ObjectIDPersister callback
// confirms the FileBlock row is durable (#669 ordering).
// On every subsequent pass the LRU hit path takes over: AddRef bumps
// RefCount on the existing FileBlock row and StoreChunk is skipped
// entirely.
//
// #669: the LRU is keyed by (hash, payloadID). This bench drives
// repeated rewrites against ONE payload so the steady-state hot path
// fires; cross-payload short-circuit is intentionally not supported.
//
// Reported metric: stores_per_chunk = StoreChunk invocations / total
// chunks emitted. Expected at K rewrites: 1/K (only the first pass
// stores). The bench REPORTS the ratio via b.ReportMetric but never
// gates (no t.Fatal on perf regression). The hard gate is aggregate
// (internal/bench/phase19_test.go).
//
// Skip-under-race honored via the raceEnabled constant
// (raceenabled_norace_test.go / raceenabled_race_test.go pair). The
// -race detector instruments mutex/atomic operations heavily, which
// collapses the perf ratio to noise and would false-fail this bench.

package fs

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore"
	memmeta "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// newBenchFSStoreWithLRU constructs an FSStore for benchmarks. Mirrors
// newFSStoreForRollupLRUTest's shape (programmableFBS wrapping a
// memory metadata store, identity used for both EngineFileBlockStore
// and RollupStore surfaces) but takes testing.TB so it can be called
// from both *testing.T and *testing.B contexts.
func newBenchFSStoreWithLRU(tb testing.TB) (*FSStore, *programmableFBS, *memmeta.MemoryMetadataStore) {
	tb.Helper()
	mem := memmeta.NewMemoryMetadataStoreWithDefaults()
	wrapped := newProgrammableFBS(mem)
	dir := tb.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, 1<<30, wrapped, FSStoreOptions{
		MaxLogBytes:     1 << 30,
		RollupWorkers:   2,
		StabilizationMS: 1,
		RollupStore:     mem,
	})
	if err != nil {
		tb.Fatalf("NewWithOptions: %v", err)
	}
	tb.Cleanup(func() { _ = bc.Close() })
	return bc, wrapped, mem
}

// runRollupOncePB drives one AppendWrite + stabilization + rollupFile
// pass for benchmarks. Bench-friendly variant of runRollupOnce that
// accepts *testing.B instead of *testing.T.
func runRollupOncePB(b *testing.B, bc *FSStore, payloadID string, payload []byte) {
	b.Helper()
	ctx := context.Background()
	if err := bc.AppendWrite(ctx, payloadID, payload, 0); err != nil {
		b.Fatalf("AppendWrite: %v", err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bc.EarliestStableForTest(payloadID) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bc.EarliestStableForTest(payloadID) {
		b.Fatal("dirty interval did not stabilize within 500 ms")
	}
	if err := bc.rollupFile(ctx, payloadID, false); err != nil {
		b.Fatalf("rollupFile: %v", err)
	}
}

// BenchmarkRandWriteCAS_IdempotentBytes — Opt 1 yellow-flag bench.
// Drives b.N rollup passes of identical content under the SAME
// payloadID — the supported LRU hit pattern post-#669 — and reports
// the stores_per_chunk ratio.
//
// Yellow-flag: this bench reports custom metrics via
// b.ReportMetric and never gates (no b.Fatal on perf regression). The
// hard quantitative merge gate is aggregate.
func BenchmarkRandWriteCAS_IdempotentBytes(b *testing.B) {
	if raceEnabled {
		b.Skip("Phase 19 D-17 yellow-flag — skip under -race to avoid detector overhead collapsing ratio to noise")
	}

	bc, wrapped, mem := newBenchFSStoreWithLRU(b)
	ctx := context.Background()
	_ = ctx

	// 256 KiB constant-byte payload: well under MinChunkSize (1 MiB), so
	// FastCDC emits exactly ONE chunk with final=true. Constant bytes
	// give every rewrite the same hash — the canonical idempotent-bytes
	// signature for VM-disk zero-fill / log rotation / config-rewrite
	// workloads Opt 1 targets per CONTEXT.md "in scope".
	payload := bytes.Repeat([]byte{0x5A}, 256*1024)
	contentHash := blake3ContentHash(payload)

	// Pre-seed a FileBlock row for the eventual hash so AddRef can
	// succeed on iteration 2+ without the StoreChunk fallback path.
	// In production, the first rollup pass's StoreChunk + downstream
	// engine-coordinator wiring materializes this row; here we seed it
	// once up-front to keep the bench's hot loop focused on the
	// AddRef-vs-StoreChunk arithmetic.
	if err := mem.Put(context.Background(), &blockstore.FileBlock{
		ID:       "bench-seed/block-0",
		Hash:     contentHash,
		State:    blockstore.BlockStateRemote,
		RefCount: 1,
		DataSize: uint32(len(payload)),
	}); err != nil {
		b.Fatalf("seed Put: %v", err)
	}
	// Also seed the LRU directly so iteration 1 already hits — the
	// hot loop measures the steady-state LRU-hit ratio rather than
	// including a one-iteration cold-start. (Cold-start behavior is
	// exercised by TestRollup_FirstChunk_PopulatesLRU.)
	//
	// #669: LRU key is now (hash, payloadID). Seed the same
	// payloadID the hot loop will reuse.
	const benchPID = "bench-idempotent"
	bc.dedupLRU.Put(contentHash, benchPID)

	startDisk := bc.diskUsed.Load()
	baseAddRef := wrapped.addRefCalls.Load()

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		runRollupOncePB(b, bc, benchPID, payload)
	}

	b.StopTimer()

	endDisk := bc.diskUsed.Load()
	endAddRef := wrapped.addRefCalls.Load()

	// One chunk per rollup pass (single-chunk payload at < MinChunkSize).
	totalChunks := int64(b.N)
	// chunksStored = number of NEW chunks the chunkstore actually wrote.
	// diskUsed bumps by len(payload) per stored chunk, so the delta /
	// payload size gives the count.
	chunksStored := (endDisk - startDisk) / int64(len(payload))
	addRefDelta := endAddRef - baseAddRef

	// Yellow-flag: report the ratio. With the LRU pre-seeded above
	// expected stores_per_chunk ≈ 0 (every emit hits LRU + AddRef).
	if totalChunks > 0 {
		ratio := float64(chunksStored) / float64(totalChunks)
		b.ReportMetric(ratio, "stores_per_chunk")
	}
	// Raw counters for post-bench inspection of the LRU hit path's
	// fingerprint.
	b.ReportMetric(float64(addRefDelta), "addref_calls_total")
	b.ReportMetric(float64(chunksStored), "stores_total")
}
