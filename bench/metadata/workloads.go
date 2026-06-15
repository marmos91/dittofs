package metadatabench

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
	"github.com/marmos91/dittofs/bench/orchestrator"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Workload names. Each isolates one hot metadata read, plus a browse-like
// blend. The store is called directly, so the numbers reflect pure backend
// cost with no client attribute cache absorbing repeats.
const (
	WorkloadGetAttr = "getattr" // store.GetFile over the file working set
	WorkloadLookup  = "lookup"  // store.GetChild(dir, name)
	WorkloadReadDir = "readdir" // store.ListChildren, paginated to completion
	WorkloadMixed   = "mixed"   // browse blend: 45% lookup, 45% getattr, 10% readdir
)

// DefaultReaddirLimit is the page size for the readdir workload when unset.
const DefaultReaddirLimit = 256

// Opts configures one bench run.
type Opts struct {
	Backend      string
	Workload     string
	Ops          int
	Workers      int
	Seed         uint64
	Dirs         int
	FilesPerDir  int
	ReaddirLimit int
}

// Result is one run's aggregate. Latency is nil only when Ops is 0.
type Result struct {
	Backend  string
	Workload string
	Ops      int
	Duration time.Duration
	Latency  *orchestrator.Latency
	Errors   int64
}

// Validate fills defaults and rejects nonsensical opts.
func (o *Opts) Validate() error {
	switch o.Workload {
	case WorkloadGetAttr, WorkloadLookup, WorkloadReadDir, WorkloadMixed:
	default:
		return fmt.Errorf("unknown workload %q (want getattr|lookup|readdir|mixed)", o.Workload)
	}
	if o.Ops <= 0 {
		return fmt.Errorf("ops must be > 0")
	}
	if o.Workers < 1 {
		o.Workers = 1
	}
	if o.Dirs < 1 {
		o.Dirs = 1
	}
	if o.FilesPerDir < 1 {
		o.FilesPerDir = 1
	}
	if o.ReaddirLimit <= 0 {
		o.ReaddirLimit = DefaultReaddirLimit
	}
	return nil
}

// Seed populates the store with a Dirs × FilesPerDir tree and returns the
// working sets the hot loop draws from. It is separated from RunOnTree so a
// caller can wrap only the read loop in a pprof capture — seeding's write-path
// samples would otherwise pollute the profile.
func Seed(ctx context.Context, store metadata.Store, opts Opts) (*Tree, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	t, err := seedTree(ctx, store, opts.Dirs, opts.FilesPerDir)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	return t, nil
}

// Run seeds a tree then drives the read workload — convenience for tests. The
// CLI uses Seed + RunOnTree so the profile excludes seeding.
func Run(ctx context.Context, store metadata.Store, opts Opts) (Result, error) {
	t, err := Seed(ctx, store, opts)
	if err != nil {
		return Result{}, err
	}
	return RunOnTree(ctx, store, t, opts)
}

// RunOnTree drives Ops read operations across Workers goroutines against the
// shared store and the pre-seeded tree, recording per-op latency. The timed
// region covers only the hot loop.
func RunOnTree(ctx context.Context, store metadata.Store, t *Tree, opts Opts) (Result, error) {
	if err := opts.Validate(); err != nil {
		return Result{}, err
	}

	rec := bsbench.NewLatencyRecorder(opts.Ops)
	perWorker := opts.Ops / opts.Workers
	remainder := opts.Ops % opts.Workers

	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < opts.Workers; w++ {
		count := perWorker
		if w < remainder {
			count++
		}
		wg.Add(1)
		go func(workerID, n int) {
			defer wg.Done()
			// Per-worker PRNG keyed off the run seed so a worker's access
			// stream is deterministic and replayable, but workers don't
			// march in lockstep over the same indices.
			rng := rand.New(rand.NewSource(int64(opts.Seed) + int64(workerID)))
			for i := 0; i < n; i++ {
				op := pickOp(opts.Workload, rng)
				started := time.Now()
				err := runOp(ctx, store, op, t, rng, opts.ReaddirLimit)
				rec.Record(time.Since(started), err == nil)
			}
		}(w, count)
	}
	wg.Wait()
	dur := time.Since(start)

	_, failed := rec.Counts()
	return Result{
		Backend:  opts.Backend,
		Workload: opts.Workload,
		Ops:      opts.Ops,
		Duration: dur,
		Latency:  orchestrator.LatencyFromSamples(rec.Samples()),
		Errors:   failed,
	}, nil
}

// pickOp resolves the per-iteration operation. Single-op workloads return
// themselves; mixed rolls the browse blend.
func pickOp(workload string, rng *rand.Rand) string {
	if workload != WorkloadMixed {
		return workload
	}
	switch roll := rng.Intn(100); {
	case roll < 45:
		return WorkloadLookup
	case roll < 90:
		return WorkloadGetAttr
	default:
		return WorkloadReadDir
	}
}

// runOp executes one read against the store. readdir paginates the chosen
// directory to completion so the cost reflects a full listing.
func runOp(ctx context.Context, store metadata.Store, op string, t *Tree, rng *rand.Rand, limit int) error {
	switch op {
	case WorkloadGetAttr:
		h := t.fileHandles[rng.Intn(len(t.fileHandles))]
		_, err := store.GetFile(ctx, h)
		return err
	case WorkloadLookup:
		i := rng.Intn(len(t.lookupDir))
		_, err := store.GetChild(ctx, t.lookupDir[i], t.lookupName[i])
		return err
	case WorkloadReadDir:
		d := t.dirHandles[rng.Intn(len(t.dirHandles))]
		cursor := ""
		for {
			_, next, err := store.ListChildren(ctx, d, cursor, limit)
			if err != nil {
				return err
			}
			if next == "" {
				return nil
			}
			cursor = next
		}
	}
	return nil
}
