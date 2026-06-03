package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/pkg/block/engine"
)

// Mixed-ops-storm tuning. The storm targets many small files to exercise
// metadata churn, dedup, and the local/remote sync path under concurrency
// rather than raw sequential throughput.
const (
	// StormFileSize is the fixed size every storm file is seeded to. Writes
	// overwrite a block-sized region at a random offset within this size;
	// reads draw a random offset within it. Fixed size keeps every read/write
	// offset in range so no op fails on a short file.
	StormFileSize = 1 * 1024 * 1024
	// stormCreateOdds is the 1-in-N chance a WRITE creates a brand-new churn
	// file (seeded to StormFileSize) instead of overwriting a stable one. Low
	// so most writes are cheap block overwrites.
	stormCreateOdds = 16
)

// Default op-type weights. WRITE/READ dominate; LIST and DELETE are rarer,
// matching a read-mostly mutating workload. Override via Opts.Mix.
const (
	stormWeightWrite  = 50
	stormWeightRead   = 30
	stormWeightList   = 15
	stormWeightDelete = 5
)

// stormWeights is the resolved WRITE/READ/LIST/DELETE op-type mix for a storm
// run. Total is their sum (the modulus for pickStormOp).
type stormWeights struct {
	write, read, list, delete int
}

func (w stormWeights) total() int { return w.write + w.read + w.list + w.delete }

func defaultStormWeights() stormWeights {
	return stormWeights{write: stormWeightWrite, read: stormWeightRead, list: stormWeightList, delete: stormWeightDelete}
}

// parseStormMix parses a "W,R,L,D" weight string (e.g. "50,30,15,5"). An empty
// string yields the default mix. All four weights are required, must be
// non-negative, and must not all be zero.
func parseStormMix(mix string) (stormWeights, error) {
	if strings.TrimSpace(mix) == "" {
		return defaultStormWeights(), nil
	}
	parts := strings.Split(mix, ",")
	if len(parts) != 4 {
		return stormWeights{}, fmt.Errorf("mix %q: want four comma-separated weights W,R,L,D", mix)
	}
	vals := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			return stormWeights{}, fmt.Errorf("mix %q: weight %d (%q) must be a non-negative integer", mix, i, p)
		}
		vals[i] = n
	}
	w := stormWeights{write: vals[0], read: vals[1], list: vals[2], delete: vals[3]}
	if w.total() == 0 {
		return stormWeights{}, fmt.Errorf("mix %q: weights must not all be zero", mix)
	}
	return w, nil
}

// churnPool is the storm's concurrency-safe pool of deletable payload IDs.
//
// The storm partitions its keyspace to keep ops race-free: a fixed "stable" set
// (pre-seeded, never deleted) backs all READs and WRITE-overwrites, while this
// pool holds only files created by WRITE-create and removed by DELETE. Because
// no payload is ever both a read/write target and a delete target, a worker can
// never write to a file another worker is concurrently deleting. popRandom is
// exclusive, so a churn file is deleted at most once.
//
// Only payload IDs are tracked, not block refs: the engine resolves a payload's
// blocks from its ID internally (WriteAt/ReadAt/Delete all accept nil refs).
type churnPool struct {
	mu  sync.Mutex
	ids []string
	seq atomic.Int64
}

// nextName returns a unique, not-yet-registered churn payload ID.
func (p *churnPool) nextName() string {
	return fmt.Sprintf("perf/storm/c%d", p.seq.Add(1))
}

// add registers a fully-seeded churn payload as deletable.
func (p *churnPool) add(pid string) {
	p.mu.Lock()
	p.ids = append(p.ids, pid)
	p.mu.Unlock()
}

// popRandom removes and returns a random churn payload. ok is false when empty.
func (p *churnPool) popRandom(rng *rand.Rand) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) == 0 {
		return "", false
	}
	i := rng.IntN(len(p.ids))
	pid := p.ids[i]
	p.ids[i] = p.ids[len(p.ids)-1]
	p.ids = p.ids[:len(p.ids)-1]
	return pid, true
}

// RunStorm drives the concurrent mixed-ops-storm: Opts.Workers goroutines each
// execute a share of Opts.Ops drawn from WRITE/READ/LIST/DELETE by the weights
// above. The stable working set is pre-seeded outside the timed region.
//
// Determinism: each worker owns a private PRNG seeded from (Opts.Seed, worker)
// so a given (seed, workers) pair reproduces the same multiset of operations.
// Goroutine interleaving is not deterministic — two runs with the same inputs
// issue the same ops but the engine may observe them in a different order. The
// seed reproduces the workload shape, which is what drives the captured
// contention profiles; it does not reproduce a byte-exact execution.
func RunStorm(ctx context.Context, bs *engine.Store, opts Opts) (Result, error) {
	if bs == nil {
		return Result{}, fmt.Errorf("RunStorm: bs is nil")
	}
	if opts.Ops <= 0 {
		return Result{}, fmt.Errorf("RunStorm: ops must be > 0")
	}
	if opts.BlockSize <= 0 || opts.BlockSize > StormFileSize {
		return Result{}, fmt.Errorf("RunStorm: block size must be in (0, %d]", StormFileSize)
	}
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	weights, err := parseStormMix(opts.Mix)
	if err != nil {
		return Result{}, err
	}

	// Stable set: at least four files per worker, so readers always have live
	// targets even at high concurrency.
	nFiles := max(opts.WorkingSet, workers*4)
	stable, err := seedStableFiles(ctx, bs, opts, nFiles)
	if err != nil {
		return Result{}, err
	}
	churn := &churnPool{}

	var counts StormCounts
	lat := NewLatencyRecorder(opts.Ops)
	statsBefore := bs.GetStats()
	start := time.Now()

	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			if err := runStormWorker(gctx, bs, opts, weights, stable, churn, worker, workers, &counts, lat); err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}(w)
	}
	wg.Wait()

	if firstErr != nil {
		return Result{}, firstErr
	}

	dur := time.Since(start)
	statsAfter := bs.GetStats()
	// Approximate throughput: every write/read moves one block. Churn-create
	// writes actually move StormFileSize, but at 1-in-stormCreateOdds they are
	// rare enough (<0.5% of bytes) that folding them in isn't worth a separate
	// counter — this drives the displayed bytes/sec, not any invariant.
	bytes := (counts.Writes + counts.Reads) * int64(opts.BlockSize)
	return Result{
		Duration:    dur,
		Ops:         opts.Ops,
		Bytes:       bytes,
		StatsBefore: statsBefore,
		StatsAfter:  statsAfter,
		Storm:       &counts,
		Latency:     lat,
	}, nil
}

// runStormWorker executes this worker's share of the op stream: global op
// indices i where i%workers == worker. The worker owns a private PRNG and
// scratch buffers; only the stable set, churn pool, and atomic counters are
// shared.
func runStormWorker(ctx context.Context, bs *engine.Store, opts Opts, weights stormWeights, stable []string, churn *churnPool, worker, workers int, counts *StormCounts, lat *LatencyRecorder) error {
	rng := workerRNG(opts.Seed, worker)
	wbuf := make([]byte, opts.BlockSize)
	rbuf := make([]byte, opts.BlockSize)
	maxOff := uint64(StormFileSize - opts.BlockSize)

	for i := worker; i < opts.Ops; i += workers {
		if ctx.Err() != nil {
			return nil // another worker failed; stop quietly
		}
		opStart := time.Now()
		switch pickStormOp(rng, weights) {
		case opWrite:
			if rng.IntN(stormCreateOdds) == 0 {
				// Create a new churn file: seed it fully, then publish it as
				// deletable. Registering only after seeding means DELETE never
				// races a half-written file.
				pid := churn.nextName()
				if err := seedPayload(ctx, bs, pid, seededBytes(rng.Uint64(), StormFileSize)); err != nil {
					lat.Record(time.Since(opStart), false)
					return err
				}
				churn.add(pid)
			} else {
				pid := stable[rng.IntN(len(stable))]
				fillRandom(rng, wbuf)
				if _, err := bs.WriteAt(ctx, pid, nil, wbuf, rng.Uint64N(maxOff+1)); err != nil {
					lat.Record(time.Since(opStart), false)
					return fmt.Errorf("storm write %s: %w", pid, err)
				}
			}
			lat.Record(time.Since(opStart), true)
			atomic.AddInt64(&counts.Writes, 1)
		case opRead:
			pid := stable[rng.IntN(len(stable))]
			if _, err := bs.ReadAt(ctx, pid, nil, rbuf, rng.Uint64N(maxOff+1)); err != nil {
				lat.Record(time.Since(opStart), false)
				return fmt.Errorf("storm read %s: %w", pid, err)
			}
			lat.Record(time.Since(opStart), true)
			atomic.AddInt64(&counts.Reads, 1)
		case opList:
			_ = bs.ListFiles()
			lat.Record(time.Since(opStart), true)
			atomic.AddInt64(&counts.Lists, 1)
		case opDelete:
			pid, ok := churn.popRandom(rng)
			if !ok {
				continue // nothing to delete yet
			}
			if err := bs.Delete(ctx, pid, nil); err != nil {
				lat.Record(time.Since(opStart), false)
				return fmt.Errorf("storm delete %s: %w", pid, err)
			}
			lat.Record(time.Since(opStart), true)
			atomic.AddInt64(&counts.Deletes, 1)
		}
	}
	return nil
}

// seedStableFiles seeds nFiles distinct StormFileSize payloads (outside the
// timed region) and returns their fixed payload-ID set.
func seedStableFiles(ctx context.Context, bs *engine.Store, opts Opts, nFiles int) ([]string, error) {
	ids := make([]string, nFiles)
	for i := 0; i < nFiles; i++ {
		pid := fmt.Sprintf("perf/storm/%d", i)
		// Distinct bytes per file so the seed isn't one giant dedup class.
		if err := seedPayload(ctx, bs, pid, seededBytes(opts.Seed^uint64(i+1), StormFileSize)); err != nil {
			return nil, err
		}
		ids[i] = pid
	}
	return ids, nil
}

type stormOp int

const (
	opWrite stormOp = iota
	opRead
	opList
	opDelete
)

// pickStormOp draws an op type by the configured weights.
func pickStormOp(rng *rand.Rand, w stormWeights) stormOp {
	r := rng.IntN(w.total())
	switch {
	case r < w.write:
		return opWrite
	case r < w.write+w.read:
		return opRead
	case r < w.write+w.read+w.list:
		return opList
	default:
		return opDelete
	}
}
