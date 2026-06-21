package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// Engine wiring constants. Sized for short-lived benchmark runs:
// 256 MiB rollup log budget, 64 MiB resident memory, 2 rollup workers.
const (
	LogBudget       = 256 * 1024 * 1024
	MemBudget       = 64 * 1024 * 1024
	RollupWorkers   = 2
	StabilizationMS = 5
)

// NewEngine wires production-equivalent FSStore + remote + memory
// metadata + Syncer for a single benchmark run. baseDir is the local
// fs root; remoteStore is the upload target (memory or s3). The
// returned cleanup closes the engine, which also closes the remote.
func NewEngine(baseDir string, remoteStore remote.RemoteStore) (*engine.Store, func(), error) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(baseDir, 0, ms, fs.FSStoreOptions{
		MaxLogBytes:     LogBudget,
		RollupWorkers:   RollupWorkers,
		StabilizationMS: StabilizationMS,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	if err := local.StartRollup(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("StartRollup: %w", err)
	}
	syncer := engine.NewSyncer(local, remoteStore, ms, engine.DefaultConfig())
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           local,
		Remote:          remoteStore,
		Syncer:          syncer,
		FileBlockStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("engine.New: %w", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("engine.Start: %w", err)
	}
	// engine.Store.Close also closes the remote — no double close here.
	return bs, func() { _ = bs.Close() }, nil
}

// workerRNG returns the per-worker PRNG for the concurrent workloads. Seeding
// the stream component with worker+1 (so worker 0 differs from the serial
// seed) gives each worker a deterministic-yet-distinct sequence: re-running
// with the same (Seed, Workers) reproduces every worker's stream exactly,
// while no two workers draw the same offsets/payload bytes.
func workerRNG(seed uint64, worker int) *rand.Rand {
	return rand.New(rand.NewPCG(seed, uint64(worker)+1))
}

// runWorkerPool fans an op stream of `ops` ops across `workers` errgroup
// goroutines and returns the wall-clock duration of the fan-out (the timed
// region). Op index i is owned by worker i%workers, so the set of ops is a
// pure function of (ops, workers) regardless of concurrency. fn receives the
// worker index, that worker's private PRNG (workerRNG), and the global op
// index; it must touch only worker-private or concurrency-safe state. The
// first non-nil error cancels the group and is returned. workers < 1 is
// treated as 1.
//
// Setup that must stay out of the timed region (seeding files, etc.) belongs
// before the call — runWorkerPool times only the fan-out.
func runWorkerPool(ctx context.Context, workers, ops int, seed uint64, fn func(worker int, rng *rand.Rand, op int) error) error {
	if workers < 1 {
		workers = 1
	}
	g, gctx := errgroup.WithContext(ctx)
	for w := 0; w < workers; w++ {
		worker := w
		g.Go(func() error {
			rng := workerRNG(seed, worker)
			for i := worker; i < ops; i += workers {
				if gctx.Err() != nil {
					return nil // another worker failed; stop quietly
				}
				if err := fn(worker, rng, i); err != nil {
					return err
				}
			}
			return nil
		})
	}
	return g.Wait()
}
