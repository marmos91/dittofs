package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
	"github.com/spf13/cobra"
)

var (
	bsWorkload       string
	bsOps            int
	bsBlockSize      int
	bsWorkingSet     int
	bsSeed           uint64
	bsRemote         string
	bsGCGarbageRatio float64
)

var blockstoreCmd = &cobra.Command{
	Use:   "blockstore",
	Short: "Drive a workload against the blockstore engine",
	Long: `Composes FSStore local + remote (memory or s3) + in-memory metadata +
Syncer and drives one of:
  sequential-write | random-write | dedup-heavy | mixed-rw | flush-churn
  walk | delete | gc | raw-s3-put

Captures wall-clock, throughput, and CPU + heap pprof to
<profile-dir>/blockstore/<workload>-<timestamp>/.`,
	RunE: runBlockstore,
}

func init() {
	flags := blockstoreCmd.Flags()
	flags.StringVar(&bsWorkload, "workload", "", "workload name (see --help for the full list)")
	flags.IntVar(&bsOps, "ops", 10000, "operation count")
	flags.IntVar(&bsBlockSize, "block-size", 0, "per-op block size in bytes (default: 8 MiB for sequential/dedup, 4 KiB otherwise)")
	flags.IntVar(&bsWorkingSet, "working-set", 1, "number of files in the working set")
	flags.Uint64Var(&bsSeed, "seed", 1, "PRNG seed for randomized workloads")
	flags.StringVar(&bsRemote, "remote", bsbench.RemoteMemory, "remote backend: memory | s3")
	flags.Float64Var(&bsGCGarbageRatio, "gc-garbage-ratio", bsbench.DefaultGCGarbage, "fraction of seeded chunks left unreferenced for the gc workload")
	_ = blockstoreCmd.MarkFlagRequired("workload")
}

func runBlockstore(cmd *cobra.Command, _ []string) error {
	blockSize := resolveBlockSize(bsWorkload, bsBlockSize)
	opts := bsbench.Opts{
		Workload:   bsWorkload,
		Ops:        bsOps,
		BlockSize:  blockSize,
		WorkingSet: bsWorkingSet,
		Seed:       bsSeed,
		Remote:     bsRemote,
		ProfileDir: flagProfileDir,
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	tmpDir, err := os.MkdirTemp("", "bench-blockstore-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Each workload family wires its own backend (engine / bare local /
	// raw remote), wraps the timed region with the shared pprof
	// envelope, and reports through the same printResult formatter.
	switch opts.Workload {
	case bsbench.WorkloadWalk, bsbench.WorkloadDelete:
		return runLocalWorkload(ctx, cmd, opts, tmpDir)
	case bsbench.WorkloadGC:
		return runGCWorkload(ctx, cmd, opts)
	case bsbench.WorkloadRawS3Put:
		return runRawS3Workload(ctx, cmd, opts)
	default:
		// Engine-backed workloads (sequential-write, random-write, ...).
		return runEngineWorkload(ctx, cmd, opts, tmpDir)
	}
}

func runEngineWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts, tmpDir string) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	bs, engineClose, err := bsbench.NewEngine(tmpDir, remoteStore)
	if err != nil {
		// Engine never took ownership of the remote on construction
		// failure — close it here. The happy path defers engineClose,
		// which closes the remote via the engine.
		remoteClose()
		return err
	}
	defer engineClose()

	profDir, cpuStop, err := startProfiles(opts.ProfileDir, opts.Workload)
	if err != nil {
		return err
	}
	cpuStopped := false
	defer func() {
		if !cpuStopped {
			cpuStop()
		}
	}()

	res, err := bsbench.RunWorkload(ctx, bs, opts)
	cpuStop()
	cpuStopped = true
	if err != nil {
		return err
	}
	if err := writeHeapProfile(profDir); err != nil {
		return err
	}
	printResult(cmd, opts, res, profDir, true)
	return nil
}

func runLocalWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts, tmpDir string) error {
	local, closeFn, err := bsbench.NewLocalStore(tmpDir)
	if err != nil {
		return err
	}
	defer closeFn()

	profDir, cpuStop, err := startProfiles(opts.ProfileDir, opts.Workload)
	if err != nil {
		return err
	}
	cpuStopped := false
	defer func() {
		if !cpuStopped {
			cpuStop()
		}
	}()

	var res bsbench.Result
	switch opts.Workload {
	case bsbench.WorkloadWalk:
		res, err = bsbench.RunWalk(ctx, local, opts)
	case bsbench.WorkloadDelete:
		res, err = bsbench.RunDelete(ctx, local, opts)
	}
	cpuStop()
	cpuStopped = true
	if err != nil {
		return err
	}
	if err := writeHeapProfile(profDir); err != nil {
		return err
	}
	printResult(cmd, opts, res, profDir, false)
	return nil
}

func runGCWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	defer remoteClose()

	profDir, cpuStop, err := startProfiles(opts.ProfileDir, opts.Workload)
	if err != nil {
		return err
	}
	cpuStopped := false
	defer func() {
		if !cpuStopped {
			cpuStop()
		}
	}()

	res, err := bsbench.RunGC(ctx, remoteStore, opts, bsGCGarbageRatio)
	cpuStop()
	cpuStopped = true
	if err != nil {
		return err
	}
	if err := writeHeapProfile(profDir); err != nil {
		return err
	}
	printResult(cmd, opts, res, profDir, false)
	return nil
}

func runRawS3Workload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	defer remoteClose()

	profDir, cpuStop, err := startProfiles(opts.ProfileDir, opts.Workload)
	if err != nil {
		return err
	}
	cpuStopped := false
	defer func() {
		if !cpuStopped {
			cpuStop()
		}
	}()

	res, err := bsbench.RunRawS3Put(ctx, remoteStore, opts)
	cpuStop()
	cpuStopped = true
	if err != nil {
		return err
	}
	if err := writeHeapProfile(profDir); err != nil {
		return err
	}
	printResult(cmd, opts, res, profDir, false)
	return nil
}

// resolveBlockSize matches the legacy cmd/blockstore-perf default rule:
// 8 MiB for sequential-write and dedup-heavy, 4 KiB otherwise. An
// explicit --block-size always wins.
func resolveBlockSize(workload string, requested int) int {
	if requested > 0 {
		return requested
	}
	switch workload {
	case bsbench.WorkloadSequentialWrite, bsbench.WorkloadDedupHeavy:
		return bsbench.DefaultSeqBlockSize
	default:
		return bsbench.DefaultRandomBlockSize
	}
}

// startProfiles creates <root>/blockstore/<workload>-<ts>/ and begins
// CPU profiling. The returned closure stops the profile and closes
// the file; safe to call exactly once.
func startProfiles(rootDir, workload string) (string, func(), error) {
	ts := time.Now().UTC().Format("20060102T150405Z")
	dir := filepath.Join(rootDir, "blockstore", fmt.Sprintf("%s-%s", workload, ts))
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

// printResult emits the canonical one-line summary, optionally
// followed by the engine stats line. Matches the legacy
// cmd/blockstore-perf format byte for byte so downstream scrapers
// keep working: bytes_per_sec is omitted when res.Bytes == 0 (walk /
// delete / gc), and the stats line is emitted only for engine-backed
// workloads. The future --output-format=json switch will be added
// here.
func printResult(cmd *cobra.Command, opts bsbench.Opts, res bsbench.Result, profDir string, withStats bool) {
	w := cmd.OutOrStdout()
	durMs := float64(res.Duration.Microseconds()) / 1000.0
	opsPerSec := float64(res.Ops) / res.Duration.Seconds()
	if res.Bytes > 0 {
		_, _ = fmt.Fprintf(w,
			"workload=%s ops=%d dur=%.3fms ops_per_sec=%.2f bytes_per_sec=%.2f profiles=%s\n",
			opts.Workload, res.Ops, durMs, opsPerSec,
			float64(res.Bytes)/res.Duration.Seconds(), profDir,
		)
	} else {
		_, _ = fmt.Fprintf(w,
			"workload=%s ops=%d dur=%.3fms ops_per_sec=%.2f profiles=%s\n",
			opts.Workload, res.Ops, durMs, opsPerSec, profDir,
		)
	}
	if !withStats {
		return
	}
	_, _ = fmt.Fprintf(w,
		"stats before/after: files=%d/%d dirty=%d/%d disk=%d/%d pending=%d completed=%d\n",
		res.StatsBefore.FileCount, res.StatsAfter.FileCount,
		res.StatsBefore.BlocksDirty, res.StatsAfter.BlocksDirty,
		res.StatsBefore.LocalDiskUsed, res.StatsAfter.LocalDiskUsed,
		res.StatsAfter.PendingUploads, res.StatsAfter.CompletedSyncs,
	)
}
