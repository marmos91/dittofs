package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// Concurrent (a)–(d) workloads. Unlike the serial RunWorkload family these
// fan a disjoint slice of the working set across Opts.Workers goroutines via
// runWorkerPool, so a given (Seed, Workers) reproduces the same op multiset.
// Each worker owns its own files (payloadID = "<prefix>/w<worker>/<n>"), so
// there is no shared blockref state and no cross-worker lock — the pure
// scaling/contention picture comes from the engine's own internals, not from
// the harness serializing on a shared map.
const (
	WorkloadConcurrentSmallWrite = "concurrent-small-write"
	WorkloadConcurrentSmallRead  = "concurrent-small-read"
	WorkloadConcurrentBigWrite   = "concurrent-big-write"
	WorkloadConcurrentBigRead    = "concurrent-big-read"
)

// Concurrent workload sizing (per PLAN §Performance). Small ops are 4–64 KiB;
// big ops are 64–256 MiB. The per-op size is drawn from the worker PRNG so a
// seed reproduces the exact size sequence. Read workloads pre-seed a file per
// op outside the timed region.
const (
	concSmallMin = 4 * 1024
	concSmallMax = 64 * 1024
	concBigMin   = 64 * 1024 * 1024
	concBigMax   = 256 * 1024 * 1024
)

// IsConcurrentWorkload reports whether name routes through RunConcurrent.
func IsConcurrentWorkload(name string) bool {
	switch name {
	case WorkloadConcurrentSmallWrite, WorkloadConcurrentSmallRead,
		WorkloadConcurrentBigWrite, WorkloadConcurrentBigRead:
		return true
	default:
		return false
	}
}

// RunConcurrent drives one of the concurrent (a)–(d) workloads across
// Opts.Workers goroutines. Each worker writes (or reads) Opts.Ops/Workers ops
// against its own disjoint payloadID space. Read workloads seed one file per
// op before the timed region (seeding is excluded from Duration). Returns the
// fan-out wall-clock plus before/after engine stats.
func RunConcurrent(ctx context.Context, bs *engine.Store, opts Opts) (Result, error) {
	if bs == nil {
		return Result{}, fmt.Errorf("RunConcurrent: bs is nil")
	}
	if opts.Ops <= 0 {
		return Result{}, fmt.Errorf("RunConcurrent: ops must be > 0")
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}

	var (
		sizeMin, sizeMax int
		prefix           string
		isRead           bool
	)
	switch opts.Workload {
	case WorkloadConcurrentSmallWrite:
		sizeMin, sizeMax, prefix = concSmallMin, concSmallMax, "perf/conc/sw"
	case WorkloadConcurrentSmallRead:
		sizeMin, sizeMax, prefix, isRead = concSmallMin, concSmallMax, "perf/conc/sr", true
	case WorkloadConcurrentBigWrite:
		sizeMin, sizeMax, prefix = concBigMin, concBigMax, "perf/conc/bw"
	case WorkloadConcurrentBigRead:
		sizeMin, sizeMax, prefix, isRead = concBigMin, concBigMax, "perf/conc/br", true
	default:
		return Result{}, fmt.Errorf("RunConcurrent: not a concurrent workload %q", opts.Workload)
	}

	pid := func(worker, op int) string { return fmt.Sprintf("%s/w%d/%d", prefix, worker, op) }
	// opSize draws this op's size deterministically from the worker PRNG, so a
	// seed reproduces the exact size sequence per worker.
	opSize := func(rng *rand.Rand) int { return sizeMin + rng.IntN(sizeMax-sizeMin+1) }

	// Read workloads pre-seed one file per op (sized like its write would be)
	// outside the timed region so the timed fan-out is pure read latency.
	if isRead {
		if err := seedConcurrentReads(ctx, bs, opts, workers, pid, opSize); err != nil {
			return Result{}, err
		}
	}

	var moved atomic.Int64
	statsBefore := bs.GetStats()
	start := time.Now()
	err := runWorkerPool(ctx, workers, opts.Ops, opts.Seed, func(worker int, rng *rand.Rand, op int) error {
		size := opSize(rng)
		p := pid(worker, op)
		buf := make([]byte, size)
		if isRead {
			if _, err := bs.ReadAt(ctx, p, nil, buf, 0); err != nil {
				return fmt.Errorf("%s read %s: %w", opts.Workload, p, err)
			}
		} else {
			fillRandom(rng, buf)
			if _, err := bs.WriteAt(ctx, p, nil, buf, 0); err != nil {
				return fmt.Errorf("%s write %s: %w", opts.Workload, p, err)
			}
		}
		moved.Add(int64(size))
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	dur := time.Since(start)
	statsAfter := bs.GetStats()

	return Result{
		Duration:    dur,
		Ops:         opts.Ops,
		Bytes:       moved.Load(),
		StatsBefore: statsBefore,
		StatsAfter:  statsAfter,
	}, nil
}

// seedConcurrentReads writes one file per op (same payloadID and size the read
// pass will request) so every read hits a live file. The size draw replays the
// worker PRNG used in the timed loop, keeping payload IDs and sizes aligned.
func seedConcurrentReads(ctx context.Context, bs *engine.Store, opts Opts, workers int, pid func(int, int) string, opSize func(*rand.Rand) int) error {
	for worker := 0; worker < workers; worker++ {
		rng := workerRNG(opts.Seed, worker)
		for op := worker; op < opts.Ops; op += workers {
			size := opSize(rng)
			if err := seedPayload(ctx, bs, pid(worker, op), seededBytes(opts.Seed^uint64(op+1), size)); err != nil {
				return err
			}
		}
	}
	return nil
}
