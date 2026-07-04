package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/local"
	"github.com/marmos91/dittofs/pkg/block/local/fs"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metadata"
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

// EngineOpts tunes NewEngineWithOpts beyond the NewEngine defaults.
type EngineOpts struct {
	// Syncer overrides engine.DefaultConfig() — e.g. a pinned ParallelUploads
	// for concurrency-sweep runs. Nil keeps the default.
	Syncer *engine.SyncerConfig
	// Metrics is an optional recorder (*pkg/metrics.Metrics satisfies it) so
	// the datapath gauges — inflight, window, queue depth, goodput — are
	// observable during a run.
	Metrics local.MetricsRecorder
	// Metadata reuses an existing metadata store instead of creating a fresh
	// one. Pair it with a fresh baseDir to model a cold restart that lost the
	// local block cache: the manifest still resolves, every read goes through
	// the remote read-through path.
	Metadata *metadatamemory.MemoryMetadataStore
	// KeepRemoteOpen hands the engine a non-closing view of the remote so
	// engine.Close leaves it usable — for callers that run several engines
	// against one remote store (upload engine + cold-restart download engine)
	// and own the final Close themselves.
	KeepRemoteOpen bool
	// PackedBlocks wires the block-carve substrate like the production share
	// factory (#1414/#1493): rollup persists chunks to the log-blob substrate
	// (LocalChunkIndex) and uploads leave as ~16 MiB packed blocks, not legacy
	// standalone CAS objects. The parity harness turns this on. It stays
	// opt-in here because the churn-style legacy workloads (flush-churn,
	// mixed-ops-storm) currently trip ErrChunkLostBeforeMirror on the carve
	// reroute path when per-op Flush supersedes still-pending chunks — their
	// baselines keep the legacy mirror until that is resolved.
	PackedBlocks bool
}

// nonClosingRemote makes engine.Close a no-op on the shared remote, like the
// production wrapper in the shares service. The carve (write) substrate is
// wired from the raw store, but the syncer's cold-read path type-asserts the
// remote.ChunkReader capability on ITS remote — this wrapper — so ReadChunk
// must be forwarded or packed-chunk read-through breaks.
type nonClosingRemote struct{ remote.RemoteStore }

func (nonClosingRemote) Close() error { return nil }

func (n nonClosingRemote) ReadChunk(ctx context.Context, blockID string, offset, length int64, hash block.ContentHash) ([]byte, error) {
	cr, ok := n.RemoteStore.(remote.ChunkReader)
	if !ok {
		return nil, remote.ErrChunkReadUnsupported
	}
	return cr.ReadChunk(ctx, blockID, offset, length, hash)
}

// NewEngine wires production-equivalent FSStore + remote + memory
// metadata + Syncer for a single benchmark run. baseDir is the local
// fs root; remoteStore is the upload target (memory or s3). The
// returned cleanup closes the engine, which also closes the remote.
func NewEngine(baseDir string, remoteStore remote.RemoteStore) (*engine.Store, func(), error) {
	bs, _, cleanup, err := NewEngineWithOpts(baseDir, remoteStore, EngineOpts{})
	return bs, cleanup, err
}

// NewEngineWithOpts is NewEngine with explicit knobs (EngineOpts). It also
// returns the metadata store so a follow-up engine can reuse it. Matching the
// production share factory, it wires the block-carve substrate whenever the
// remote implements RemoteBlockStore, so benchmarks exercise the packed-blocks
// upload path (#1414/#1493), not the legacy standalone CAS mirror.
func NewEngineWithOpts(baseDir string, remoteStore remote.RemoteStore, opts EngineOpts) (*engine.Store, *metadatamemory.MemoryMetadataStore, func(), error) {
	cfg := engine.DefaultConfig()
	if opts.Syncer != nil {
		cfg = *opts.Syncer
	}
	ms := opts.Metadata
	if ms == nil {
		ms = metadatamemory.NewMemoryMetadataStoreWithDefaults()
	}
	fsOpts := fs.FSStoreOptions{
		MaxLogBytes:     LogBudget,
		RollupWorkers:   RollupWorkers,
		StabilizationMS: StabilizationMS,
		RollupStore:     ms,
		SyncedHashStore: ms,
	}
	// Wire the LocalChunkIndex like the production share factory (#1414 PR3
	// flip): rollup then persists chunks to the log-blob substrate so the
	// carver's GetLocalLocation→ReadLocalAt path resolves and uploads go out
	// as packed blocks, not legacy standalone CAS objects.
	if opts.PackedBlocks {
		if lci, ok := any(ms).(metadata.LocalChunkIndex); ok {
			fsOpts.LocalChunkIndex = lci
		}
	}
	localStore, err := fs.NewWithOptions(baseDir, 0, ms, fsOpts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	if err := localStore.StartRollup(context.Background()); err != nil {
		return nil, nil, nil, fmt.Errorf("StartRollup: %w", err)
	}
	engineRemote := remoteStore
	if opts.KeepRemoteOpen && remoteStore != nil {
		engineRemote = nonClosingRemote{remoteStore}
	}
	syncer := engine.NewSyncer(localStore, engineRemote, ms, cfg)
	// Flip the packed-blocks carve path on, exactly like the production share
	// factory (pkg/controlplane/runtime/shares): every shipped remote (memory,
	// s3) implements RemoteBlockStore. Assert on the RAW store, matching the
	// production wiring (the wrapper only forwards the read-side ReadChunk).
	if opts.PackedBlocks && remoteStore != nil {
		if rbs, ok := remoteStore.(remote.RemoteBlockStore); ok {
			syncer.SetRemoteBlockStore(rbs)
		}
	}
	bs, err := engine.New(engine.BlockStoreConfig{
		Local:           localStore,
		Remote:          engineRemote,
		Syncer:          syncer,
		FileChunkStore:  ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("engine.New: %w", err)
	}
	if opts.Metrics != nil {
		bs.SetMetrics(opts.Metrics)
	}
	if err := bs.Start(context.Background()); err != nil {
		return nil, nil, nil, fmt.Errorf("engine.Start: %w", err)
	}
	// engine.Store.Close also closes the remote — no double close here.
	return bs, ms, func() { _ = bs.Close() }, nil
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
