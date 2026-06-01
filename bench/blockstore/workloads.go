package blockstore

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
)

// Workload identifiers. To add an engine-backed workload: add a
// constant here, a case in prepareWorkload (workloads.go), and a
// per-op step function — RunWorkload dispatches through that switch.
// Workloads that bypass the engine (raw remote / local-only) instead
// expose their own exported Run* helper in workloads_extra.go.
const (
	WorkloadSequentialWrite = "sequential-write"
	WorkloadRandomWrite     = "random-write"
	WorkloadDedupHeavy      = "dedup-heavy"
	WorkloadMixedRW         = "mixed-rw"
	WorkloadFlushChurn      = "flush-churn"
)

// Default block sizes — match the legacy cmd/blockstore-perf shape so
// historical results stay comparable. Sequential and dedup move 8 MiB
// per op; random/mixed/churn move 4 KiB.
const (
	DefaultSeqBlockSize    = 8 * 1024 * 1024
	DefaultRandomBlockSize = 4 * 1024
)

// Seeded working-set sizes for random-offset workloads.
const (
	RandomWriteFileSize = 64 * 1024 * 1024
	MixedRWFileSize     = 32 * 1024 * 1024
)

// seedChunkSize bounds each seed WriteAt so a single record never
// exceeds the local store's per-record cap (maxRecordPayload, 17 MiB,
// just above the chunker's 16 MiB hard ceiling). Production protocol
// callers (NFS WRITE, SMB write) already arrive in sub-MiB segments;
// only the bench seed would otherwise emit one multi-MiB record. 8 MiB
// keeps the seed fast while staying well under the cap.
const seedChunkSize = 8 * 1024 * 1024

// Opts is the unified workload configuration. Shared by cmd/bench
// blockstore (CLI flags), Go Benchmark* tests, and any future caller.
// Zero values are valid only for fields explicitly documented as
// optional; required fields must be set by the caller (the cmd layer
// resolves CLI defaults in cmd/bench/blockstore.go).
type Opts struct {
	// Workload selects which Run* function dispatches. Required.
	Workload string

	// Ops is the operation count for the timed loop. Required.
	Ops int

	// BlockSize is the per-op block size in bytes. Required.
	BlockSize int

	// WorkingSet is the number of distinct files. Min 1.
	WorkingSet int

	// Seed is the PRNG seed for offsets and payload fill. 0 is valid.
	Seed uint64

	// Remote selects the upload backend: "memory" (default) or "s3".
	Remote string

	// ProfileDir is the parent dir for per-run pprof output. Optional
	// at the library level (only the cmd layer captures profiles).
	ProfileDir string
}

// Result is the post-run summary returned by RunWorkload.
type Result struct {
	// Duration is the wall-clock time spent in the timed loop only —
	// excludes engine wiring, seeding, and profile finalization.
	Duration time.Duration

	// Ops mirrors Opts.Ops on success (provided for symmetry).
	Ops int

	// Bytes is the sum of bytes returned by each per-op step. For
	// mixed-rw, read ops contribute 0 (writes carry the throughput).
	Bytes int64

	// StatsBefore and StatsAfter are engine snapshots taken just
	// before / after the timed region.
	StatsBefore engine.BlockStoreStats
	StatsAfter  engine.BlockStoreStats
}

// RunWorkload is the single dispatcher used by the cmd/bench CLI. It
// seeds working-set state outside the timed region, runs the per-op
// step Opts.Ops times, and returns wall-clock + stats. The engine and
// remote must already be wired by the caller (see NewEngine /
// SetupRemote in fixture.go and remote.go); this function does not
// own their lifecycle so that profiling envelopes can wrap the timed
// region precisely.
func RunWorkload(ctx context.Context, bs *engine.Store, opts Opts) (Result, error) {
	if bs == nil {
		return Result{}, fmt.Errorf("RunWorkload: bs is nil")
	}
	if opts.Ops <= 0 {
		return Result{}, fmt.Errorf("RunWorkload: ops must be > 0")
	}
	if opts.BlockSize <= 0 {
		return Result{}, fmt.Errorf("RunWorkload: block size must be > 0")
	}
	files := opts.WorkingSet
	if files < 1 {
		files = 1
	}

	step, err := prepareWorkload(ctx, bs, opts, files)
	if err != nil {
		return Result{}, err
	}

	statsBefore := bs.GetStats()
	start := time.Now()
	var total int64
	for i := 0; i < opts.Ops; i++ {
		n, err := step(i)
		if err != nil {
			return Result{}, fmt.Errorf("%s op=%d: %w", opts.Workload, i, err)
		}
		total += int64(n)
	}
	dur := time.Since(start)
	statsAfter := bs.GetStats()

	return Result{
		Duration:    dur,
		Ops:         opts.Ops,
		Bytes:       total,
		StatsBefore: statsBefore,
		StatsAfter:  statsAfter,
	}, nil
}

// prepareWorkload builds the per-op step function for opts.Workload,
// seeding any pre-workload state (excluded from timing).
func prepareWorkload(ctx context.Context, bs *engine.Store, opts Opts, files int) (func(i int) (int, error), error) {
	buf := make([]byte, opts.BlockSize)
	rng := rand.New(rand.NewPCG(opts.Seed, 0))

	switch opts.Workload {
	case WorkloadSequentialWrite:
		// Per-file offsets — a shared `off` would smear writes across
		// files at correlated positions when working-set > 1.
		offs := make([]int64, files)
		return func(i int) (int, error) {
			fillRandom(rng, buf)
			idx := i % files
			pid := fmt.Sprintf("perf/seq/%d", idx)
			_, err := bs.WriteAt(ctx, pid, nil, buf, uint64(offs[idx]))
			offs[idx] += int64(len(buf))
			return len(buf), err
		}, nil
	case WorkloadRandomWrite:
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/rand", opts, RandomWriteFileSize)
		if err != nil {
			return nil, err
		}
		return func(i int) (int, error) {
			fillRandom(rng, buf)
			pid := fmt.Sprintf("perf/rand/%d", i%files)
			_, err := bs.WriteAt(ctx, pid, nil, buf, rng.Uint64N(maxOff+1))
			return len(buf), err
		}, nil
	case WorkloadDedupHeavy:
		// Same bytes across distinct files exercises file-level dedup.
		fillRandom(rng, buf)
		return func(i int) (int, error) {
			pid := fmt.Sprintf("perf/dedup/%d", i)
			if _, err := bs.WriteAt(ctx, pid, nil, buf, 0); err != nil {
				return 0, err
			}
			_, err := bs.Flush(ctx, pid)
			return len(buf), err
		}, nil
	case WorkloadMixedRW:
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/mixed", opts, MixedRWFileSize)
		if err != nil {
			return nil, err
		}
		rbuf := make([]byte, opts.BlockSize)
		return func(i int) (int, error) {
			off := rng.Uint64N(maxOff + 1)
			pid := fmt.Sprintf("perf/mixed/%d", i%files)
			if i%2 == 0 {
				fillRandom(rng, buf)
				_, err := bs.WriteAt(ctx, pid, nil, buf, off)
				return len(buf), err
			}
			_, err := bs.ReadAt(ctx, pid, nil, rbuf, off)
			return 0, err
		}, nil
	case WorkloadFlushChurn:
		offs := make([]int64, files)
		return func(i int) (int, error) {
			fillRandom(rng, buf)
			idx := i % files
			pid := fmt.Sprintf("perf/churn/%d", idx)
			if _, err := bs.WriteAt(ctx, pid, nil, buf, uint64(offs[idx])); err != nil {
				return 0, err
			}
			if _, err := bs.Flush(ctx, pid); err != nil {
				return 0, err
			}
			offs[idx] += int64(len(buf))
			return len(buf), nil
		}, nil
	default:
		return nil, fmt.Errorf("unknown workload %q", opts.Workload)
	}
}

// seedAndOffsetCap seeds N payloads of `size` bytes and returns the
// max legal write offset (size-blockSize). Errors when blockSize >
// size (would otherwise underflow when cast to uint64).
func seedAndOffsetCap(ctx context.Context, bs *engine.Store, files int, prefix string, opts Opts, size int) (uint64, error) {
	if opts.BlockSize > size {
		return 0, fmt.Errorf("%s: block-size %d exceeds seeded file size %d", opts.Workload, opts.BlockSize, size)
	}
	data := seededBytes(opts.Seed^0x9E3779B97F4A7C15, size)
	for i := 0; i < files; i++ {
		pid := fmt.Sprintf("%s/%d", prefix, i)
		if err := seedPayload(ctx, bs, pid, data); err != nil {
			return 0, err
		}
	}
	return uint64(size - opts.BlockSize), nil
}

// seedPayload writes data into payloadID at offset 0 in segments of at
// most seedChunkSize bytes. A single WriteAt becomes one append-log
// record, so seeding a multi-MiB working-set file in one call would
// exceed the local store's per-record cap; splitting mirrors the
// bounded segments real protocol callers emit.
func seedPayload(ctx context.Context, bs *engine.Store, payloadID string, data []byte) error {
	for off := 0; off < len(data); off += seedChunkSize {
		end := off + seedChunkSize
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
// otherwise CAS dedup short-circuits most writes and the workload
// stops reflecting realistic block churn.
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
