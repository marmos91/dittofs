// blockstore-perf is a seeded workload harness for pkg/blockstore/engine.
// Composes FSStore local + memory remote + in-memory metadata + Syncer
// and drives one of: sequential-write, random-write, dedup-heavy,
// mixed-rw, flush-churn. Captures wall-clock, throughput, and CPU + heap
// pprof to <profile-dir>/<workload>-<timestamp>/.
//
// Example: go run ./cmd/blockstore-perf --workload random-write --ops 5000
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/marmos91/dittofs/pkg/blockstore/engine"
	"github.com/marmos91/dittofs/pkg/blockstore/local/fs"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

const (
	defaultSeqBlockSize    = 8 * 1024 * 1024
	defaultRandomBlockSize = 4 * 1024
	logBudget              = 256 * 1024 * 1024
	memBudget              = 64 * 1024 * 1024
)

type config struct {
	workload   string
	ops        int
	blockSize  int
	workingSet int
	profileDir string
	seed       uint64
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		log.Fatalf("blockstore-perf: %v", err)
	}
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.workload, "workload", "", "workload name (sequential-write|random-write|dedup-heavy|mixed-rw|flush-churn)")
	flag.IntVar(&c.ops, "ops", 10000, "operation count")
	bs := flag.Int("block-size", 0, "per-op block size in bytes (default: 8 MiB for sequential/dedup, 4 KiB for the rest)")
	flag.IntVar(&c.workingSet, "working-set", 1, "number of files in the working set")
	flag.StringVar(&c.profileDir, "profile-dir", "_profiles", "directory for pprof output")
	flag.Uint64Var(&c.seed, "seed", 1, "PRNG seed for randomized workloads")
	flag.Parse()
	if c.workload == "" {
		fmt.Fprintln(os.Stderr, "--workload is required")
		flag.Usage()
		os.Exit(2)
	}
	switch {
	case *bs > 0:
		c.blockSize = *bs
	case c.workload == "sequential-write" || c.workload == "dedup-heavy":
		c.blockSize = defaultSeqBlockSize
	default:
		c.blockSize = defaultRandomBlockSize
	}
	return c
}

func run(cfg config) error {
	tmpDir, err := os.MkdirTemp("", "blockstore-perf-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	bs, closeFn, err := newBlockStore(tmpDir)
	if err != nil {
		return err
	}
	defer closeFn()

	profDir, cpuStop, err := startProfiles(cfg)
	if err != nil {
		return err
	}
	cpuStopped := false
	defer func() {
		if !cpuStopped {
			cpuStop()
		}
	}()

	ctx := context.Background()
	// Pre-seed any working-set state BEFORE timing — seedAndOffsetCap
	// writes 32–64 MiB per file and must not pollute throughput numbers.
	workload, err := prepareWorkload(ctx, bs, cfg)
	if err != nil {
		return err
	}

	statsBefore := bs.GetStats()
	start := time.Now()
	bytesWritten, err := runLoop(cfg, workload)
	dur := time.Since(start)
	// Stop CPU profile right after the timed region — heap snapshot and
	// runtime.GC below must not appear in the workload CPU profile.
	cpuStop()
	cpuStopped = true
	if err != nil {
		return err
	}
	statsAfter := bs.GetStats()

	if err := writeHeapProfile(profDir); err != nil {
		return err
	}

	fmt.Printf("workload=%s ops=%d dur=%.3fms ops_per_sec=%.2f bytes_per_sec=%.2f profiles=%s\n",
		cfg.workload, cfg.ops, float64(dur.Microseconds())/1000.0,
		float64(cfg.ops)/dur.Seconds(), float64(bytesWritten)/dur.Seconds(), profDir)
	fmt.Printf("stats before/after: files=%d/%d dirty=%d/%d disk=%d/%d pending=%d completed=%d\n",
		statsBefore.FileCount, statsAfter.FileCount,
		statsBefore.BlocksDirty, statsAfter.BlocksDirty,
		statsBefore.LocalDiskUsed, statsAfter.LocalDiskUsed,
		statsAfter.PendingUploads, statsAfter.CompletedSyncs)
	return nil
}

// newBlockStore wires production-equivalent FSStore + memory remote +
// memory metadata + Syncer, sized for short-lived benchmark runs.
func newBlockStore(baseDir string) (*engine.BlockStore, func(), error) {
	ms := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	local, err := fs.NewWithOptions(baseDir, 0, memBudget, ms, fs.FSStoreOptions{
		MaxLogBytes:     logBudget,
		RollupWorkers:   2,
		StabilizationMS: 5,
		RollupStore:     ms,
		SyncedHashStore: ms,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("fs.NewWithOptions: %w", err)
	}
	if err := local.StartRollup(context.Background()); err != nil {
		return nil, nil, fmt.Errorf("StartRollup: %w", err)
	}
	remoteStore := remotememory.New()
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
	// engine.BlockStore.Close already closes the remote — no double close here.
	return bs, func() { _ = bs.Close() }, nil
}

// startProfiles creates the per-run profile dir and begins CPU
// profiling. The returned closure stops the profile and closes the file.
func startProfiles(cfg config) (string, func(), error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(cfg.profileDir, fmt.Sprintf("%s-%s", cfg.workload, ts))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("mkdir profiles: %w", err)
	}
	f, err := os.Create(filepath.Join(dir, "cpu.pprof"))
	if err != nil {
		return "", func() {}, fmt.Errorf("create cpu.pprof: %w", err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		return "", func() {}, fmt.Errorf("StartCPUProfile: %w", err)
	}
	return dir, func() { pprof.StopCPUProfile(); _ = f.Close() }, nil
}

func writeHeapProfile(dir string) error {
	runtime.GC()
	f, err := os.Create(filepath.Join(dir, "heap.pprof"))
	if err != nil {
		return fmt.Errorf("create heap.pprof: %w", err)
	}
	defer func() { _ = f.Close() }()
	if err := pprof.WriteHeapProfile(f); err != nil {
		return fmt.Errorf("WriteHeapProfile: %w", err)
	}
	return nil
}

// prepareWorkload seeds any pre-workload state (outside the timed
// region) and returns the per-op step function consumed by runLoop.
// Seeding is excluded from timing because seedAndOffsetCap writes
// 32–64 MiB per file that would otherwise pollute throughput numbers
// without being counted in ops/bytes.
func prepareWorkload(ctx context.Context, bs *engine.BlockStore, cfg config) (func(i int) (int, error), error) {
	files := max(cfg.workingSet, 1)
	buf := make([]byte, cfg.blockSize)
	// PRNG drives both per-op offset selection and per-op buffer fill.
	// Filling buf per-iteration (rng.Read) defeats CAS dedup that would
	// otherwise swallow most ops when only buf[0] varied across writes.
	rng := rand.New(rand.NewPCG(cfg.seed, 0))

	switch cfg.workload {
	case "sequential-write":
		// Per-file offsets — when working-set > 1, each payloadID must
		// track its own monotonic offset; a shared `off` would smear
		// writes across files at correlated positions.
		offs := make([]int64, files)
		return func(i int) (int, error) {
			fillRandom(rng, buf)
			idx := i % files
			pid := fmt.Sprintf("perf/seq/%d", idx)
			_, err := bs.WriteAt(ctx, pid, nil, buf, uint64(offs[idx]))
			offs[idx] += int64(len(buf))
			return len(buf), err
		}, nil
	case "random-write":
		const fileSize = 64 * 1024 * 1024
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/rand", cfg, fileSize)
		if err != nil {
			return nil, err
		}
		return func(i int) (int, error) {
			fillRandom(rng, buf)
			pid := fmt.Sprintf("perf/rand/%d", i%files)
			_, err := bs.WriteAt(ctx, pid, nil, buf, rng.Uint64N(maxOff+1))
			return len(buf), err
		}, nil
	case "dedup-heavy":
		// Same block bytes across distinct files exercises file-level
		// dedup; deliberately fill buf once before the loop.
		fillRandom(rng, buf)
		return func(i int) (int, error) {
			pid := fmt.Sprintf("perf/dedup/%d", i)
			if _, err := bs.WriteAt(ctx, pid, nil, buf, 0); err != nil {
				return 0, err
			}
			_, err := bs.Flush(ctx, pid)
			return len(buf), err
		}, nil
	case "mixed-rw":
		const fileSize = 32 * 1024 * 1024
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/mixed", cfg, fileSize)
		if err != nil {
			return nil, err
		}
		rbuf := make([]byte, cfg.blockSize)
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
	case "flush-churn":
		// Per-file offsets — same rationale as sequential-write.
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
		return nil, fmt.Errorf("unknown workload %q", cfg.workload)
	}
}

func runLoop(cfg config, step func(i int) (int, error)) (int64, error) {
	var total int64
	for i := 0; i < cfg.ops; i++ {
		n, err := step(i)
		if err != nil {
			return total, fmt.Errorf("%s op=%d: %w", cfg.workload, i, err)
		}
		total += int64(n)
	}
	return total, nil
}

// seedAndOffsetCap seeds N payloads of `size` bytes and returns the max
// legal write offset (size-blockSize). Errors when blockSize > size
// (would otherwise underflow when cast to uint64).
func seedAndOffsetCap(ctx context.Context, bs *engine.BlockStore, files int, prefix string, cfg config, size int) (uint64, error) {
	if cfg.blockSize > size {
		return 0, fmt.Errorf("%s: --block-size %d exceeds seeded file size %d", cfg.workload, cfg.blockSize, size)
	}
	data := seededBytes(cfg.seed^0x9E3779B97F4A7C15, size)
	for i := 0; i < files; i++ {
		pid := fmt.Sprintf("%s/%d", prefix, i)
		if _, err := bs.WriteAt(ctx, pid, nil, data, 0); err != nil {
			return 0, fmt.Errorf("seed %s: %w", pid, err)
		}
	}
	return uint64(size - cfg.blockSize), nil
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
// otherwise CAS dedup would short-circuit most writes and the workload
// would not reflect realistic block churn.
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
