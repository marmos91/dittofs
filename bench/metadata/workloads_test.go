package metadatabench

import (
	"context"
	"testing"
)

// TestRun_HermeticBackends smoke-tests every workload against the two backends
// that need no external service (memory + badger in a temp dir), so
// `go test ./bench/metadata/...` is self-contained. Postgres is exercised
// manually via `dfsbench metadata --backend postgres` against a live DB.
func TestRun_HermeticBackends(t *testing.T) {
	backends := []string{BackendMemory, BackendBadger}
	workloads := []string{WorkloadGetAttr, WorkloadLookup, WorkloadReadDir, WorkloadMixed}

	for _, backend := range backends {
		for _, workload := range workloads {
			name := backend + "/" + workload
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				store, cleanup, err := OpenStore(ctx, backend, PGConfig{})
				if err != nil {
					t.Fatalf("OpenStore(%s): %v", backend, err)
				}
				defer cleanup()

				res, err := Run(ctx, store, Opts{
					Backend:     backend,
					Workload:    workload,
					Ops:         500,
					Workers:     4,
					Seed:        1,
					Dirs:        4,
					FilesPerDir: 25,
				})
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				if res.Errors != 0 {
					t.Errorf("got %d op errors, want 0", res.Errors)
				}
				if res.Ops != 500 {
					t.Errorf("Ops = %d, want 500", res.Ops)
				}
				if res.Latency == nil {
					t.Fatal("Latency is nil, want populated percentiles")
				}
				// Percentiles must be non-negative and monotonic. A zero is
				// valid: an in-RAM op can be faster than the platform clock's
				// resolution (notably Windows' coarse timer), so p50/p95
				// legitimately read 0ns.
				if res.Latency.P50Ns < 0 || res.Latency.P95Ns < res.Latency.P50Ns || res.Latency.P99Ns < res.Latency.P95Ns {
					t.Errorf("bad latency block: p50=%d p95=%d p99=%d",
						res.Latency.P50Ns, res.Latency.P95Ns, res.Latency.P99Ns)
				}
			})
		}
	}
}

// TestRun_RerunIdempotent verifies a backend can be seeded twice in a row
// (the reset path for persistent stores; a no-op for memory/badger).
func TestRun_RerunIdempotent(t *testing.T) {
	ctx := context.Background()
	store, cleanup, err := OpenStore(ctx, BackendMemory, PGConfig{})
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer cleanup()

	for i := 0; i < 2; i++ {
		if _, err := Run(ctx, store, Opts{
			Backend: BackendMemory, Workload: WorkloadMixed,
			Ops: 100, Workers: 2, Dirs: 2, FilesPerDir: 10,
		}); err != nil {
			t.Fatalf("Run #%d: %v", i, err)
		}
	}
}
