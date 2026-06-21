// BenchmarkAppendWrite_GroupCommit measures the per-file fsync
// coordinator's coalescence under concurrent AppendWrite load. G=8
// goroutines perform b.N total AppendWrites against a single logFile;
// the coordinator's fsyncFn is wrapped at construction time with an
// atomic counter so we can observe how many actual fsync syscalls
// the coordinator issued vs how many AppendWrite calls landed.
//
// Reported metric: fsyncs_per_op = fsync invocations / total
// AppendWrites. Expected on a well-coalescing coordinator: < 1.0
// (concurrent writers piggyback on a single fsync window). Note that
// per-file mu serializes same-payload writers, so the AppendWrite
// hot-path does not exhibit dramatic coalescence — the metric is
// closer to 1.0 in practice (one fsync per writer arrival).
// Cross-payload fan-out across distinct logFiles would coalesce more
// aggressively but THIS bench keeps the production hot-path shape
// (one logFile, G writers) to track regressions in the depth-1
// inline bypass path and the coordinator's overhead in steady state.
//
// The bench REPORTS the ratio via b.ReportMetric but never gates (no
// b.Fatal on perf regression). The hard gate is aggregate
// (internal/bench/phase19_test.go).
//
// Skip-under-race honored via the raceEnabled constant
// (raceenabled_norace_test.go / raceenabled_race_test.go pair). The
// -race detector instruments mutex/atomic operations heavily, which
// collapses the perf ratio to noise.

package fs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// BenchmarkAppendWrite_GroupCommit — Opt 2 yellow-flag bench.
// Fans out G goroutines doing AppendWrite at distinct offsets within
// a single logFile; counts fsync invocations via a wrapped fsyncFn.
//
// Yellow-flag: reports custom metrics via b.ReportMetric and
// never gates (no b.Fatal on perf regression).
func BenchmarkAppendWrite_GroupCommit(b *testing.B) {
	if raceEnabled {
		b.Skip("Phase 19 D-17 yellow-flag — skip under -race to avoid detector overhead collapsing ratio to noise")
	}

	bc := newBenchFSStore(b, FSStoreOptions{MaxLogBytes: 1 << 30})
	ctx := context.Background()
	const payloadID = "gc-bench"

	// Seed the logFile and wrap its coordinator BEFORE the timed loop
	// so the wrap doesn't show up in benchmark allocations.
	if err := bc.AppendWrite(ctx, payloadID, []byte("seed"), 0); err != nil {
		b.Fatalf("seed AppendWrite: %v", err)
	}
	bcSh := bc.shardFor(payloadID)
	bcSh.mu.RLock()
	lf := bcSh.logFDs[payloadID]
	bcSh.mu.RUnlock()
	if lf == nil || lf.groupCommit == nil {
		b.Fatal("logFile/coordinator missing after seed AppendWrite")
	}

	var fsyncCalls atomic.Int64
	orig := lf.groupCommit.fsyncFn
	lf.groupCommit.fsyncFn = func() error {
		fsyncCalls.Add(1)
		return orig()
	}

	const goroutines = 8

	// Distinct offsets per goroutine; each goroutine performs
	// b.N/goroutines iterations. Using distinct offset windows avoids
	// "non-monotone offset" rejection from the per-file dirty-interval
	// tree.
	perGoroutine := b.N / goroutines
	if perGoroutine == 0 {
		perGoroutine = 1
	}
	totalOps := perGoroutine * goroutines

	// Each AppendWrite writes 64 bytes — small enough to keep the
	// bench focused on the coalesce path (one fsync per coord window)
	// rather than disk-bandwidth.
	payload := make([]byte, 64)
	for i := range payload {
		payload[i] = byte(i)
	}

	b.ResetTimer()
	b.ReportAllocs()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			// Carve a per-goroutine offset window: goroutine g writes at
			// offsets [base+g*step, base+g*step+perGoroutine*step). The
			// base accounts for the seed write at offset 0.
			//
			// Offsets must monotonically advance per goroutine within its
			// own window AND must be globally unique to avoid the
			// dirty-interval tree's overlap detection. We pick a step of
			// 4096 (block-aligned) per writer.
			const stepBytes = uint64(4096)
			base := uint64(len(payload)) // skip past the seed
			window := uint64(perGoroutine) * stepBytes
			start := base + uint64(g)*window
			for i := 0; i < perGoroutine; i++ {
				off := start + uint64(i)*stepBytes
				if err := bc.AppendWrite(ctx, payloadID, payload, off); err != nil {
					b.Errorf("AppendWrite g=%d i=%d off=%d: %v", g, i, off, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	b.StopTimer()

	calls := fsyncCalls.Load()
	if totalOps > 0 {
		ratio := float64(calls) / float64(totalOps)
		b.ReportMetric(ratio, "fsyncs_per_op")
	}
	b.ReportMetric(float64(calls), "fsync_calls_total")
	b.ReportMetric(float64(totalOps), "ops_total")
}

// newBenchFSStore mirrors newFSStoreForTest but accepts *testing.B so
// the benches can stand up an FSStore + nopFBS without
// refactoring the test-side helper.
func newBenchFSStore(b *testing.B, opts FSStoreOptions) *FSStore {
	b.Helper()
	dir := b.TempDir()
	bc, err := NewWithOptions(dir, 1<<30, nopFBS{}, opts)
	if err != nil {
		b.Fatalf("NewWithOptions: %v", err)
	}
	b.Cleanup(func() { _ = bc.Close() })
	return bc
}
