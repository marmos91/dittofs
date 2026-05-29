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
	defer cpuStop()

	statsBefore := bs.GetStats()
	ctx := context.Background()
	start := time.Now()
	bytesWritten, err := driveWorkload(ctx, bs, cfg)
	dur := time.Since(start)
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
	return bs, func() { _ = bs.Close(); _ = remoteStore.Close() }, nil
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

// driveWorkload runs the selected workload and returns bytes written.
func driveWorkload(ctx context.Context, bs *engine.BlockStore, cfg config) (int64, error) {
	files := max(cfg.workingSet, 1)
	buf := seededBytes(cfg.seed, cfg.blockSize)
	rng := rand.New(rand.NewPCG(cfg.seed, 0))

	switch cfg.workload {
	case "sequential-write":
		var off uint64
		return runLoop(cfg, func(i int) (int, error) {
			buf[0] = byte(i)
			pid := fmt.Sprintf("perf/seq/%d", i%files)
			_, err := bs.WriteAt(ctx, pid, nil, buf, off)
			off += uint64(len(buf))
			return len(buf), err
		})
	case "random-write":
		const fileSize = 64 * 1024 * 1024
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/rand", cfg, fileSize)
		if err != nil {
			return 0, err
		}
		return runLoop(cfg, func(i int) (int, error) {
			buf[0] = byte(i)
			pid := fmt.Sprintf("perf/rand/%d", i%files)
			_, err := bs.WriteAt(ctx, pid, nil, buf, rng.Uint64N(maxOff+1))
			return len(buf), err
		})
	case "dedup-heavy":
		// Same block bytes -> distinct file per op exercises file-level dedup.
		return runLoop(cfg, func(i int) (int, error) {
			pid := fmt.Sprintf("perf/dedup/%d", i)
			if _, err := bs.WriteAt(ctx, pid, nil, buf, 0); err != nil {
				return 0, err
			}
			_, err := bs.Flush(ctx, pid)
			return len(buf), err
		})
	case "mixed-rw":
		const fileSize = 32 * 1024 * 1024
		maxOff, err := seedAndOffsetCap(ctx, bs, files, "perf/mixed", cfg, fileSize)
		if err != nil {
			return 0, err
		}
		rbuf := make([]byte, cfg.blockSize)
		return runLoop(cfg, func(i int) (int, error) {
			off := rng.Uint64N(maxOff + 1)
			pid := fmt.Sprintf("perf/mixed/%d", i%files)
			if i%2 == 0 {
				buf[0] = byte(i)
				_, err := bs.WriteAt(ctx, pid, nil, buf, off)
				return len(buf), err
			}
			_, err := bs.ReadAt(ctx, pid, nil, rbuf, off)
			return 0, err
		})
	case "flush-churn":
		var off uint64
		return runLoop(cfg, func(i int) (int, error) {
			buf[0] = byte(i)
			pid := fmt.Sprintf("perf/churn/%d", i%files)
			if _, err := bs.WriteAt(ctx, pid, nil, buf, off); err != nil {
				return 0, err
			}
			if _, err := bs.Flush(ctx, pid); err != nil {
				return 0, err
			}
			off += uint64(len(buf))
			return len(buf), nil
		})
	default:
		return 0, fmt.Errorf("unknown workload %q", cfg.workload)
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
	for i := range out {
		out[i] = byte(rng.Uint32())
	}
	return out
}
