package main

import (
	"context"
	"fmt"
	"os"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
	"github.com/spf13/cobra"
)

var (
	bsWorkload       string
	bsOps            int
	bsBlockSize      int
	bsWorkingSet     int
	bsWorkers        int
	bsSeed           uint64
	bsRemote         string
	bsGCGarbageRatio float64
	bsFullProfiles   bool
	bsPhase          string
	bsReplay         string
)

var blockstoreCmd = &cobra.Command{
	Use:   "blockstore",
	Short: "Drive a workload against the blockstore engine",
	Long: `Composes FSStore local + remote (memory or s3) + in-memory metadata +
Syncer and drives one of:
  sequential-write | random-write | dedup-heavy | mixed-rw | flush-churn
  mixed-ops-storm (concurrent WRITE/READ/LIST/DELETE; use --workers)
  walk | delete | gc | raw-s3-put

Captures wall-clock, throughput, and CPU + heap + goroutine pprof to
<profile-dir>/blockstore/[<phase>/]<workload>-<timestamp>/. Add
--full-profiles to also enable the runtime mutex + block profilers and emit
mutex.pprof + block.pprof (off by default — they add per-event accounting
overhead). --phase baseline|post-fix nests the capture so before/after runs
sit side by side. --replay <dir> reproduces a recorded run from its
seed.txt.`,
	RunE: runBlockstore,
}

func init() {
	flags := blockstoreCmd.Flags()
	flags.StringVar(&bsWorkload, "workload", "", "workload name (see --help for the full list)")
	flags.IntVar(&bsOps, "ops", 10000, "operation count")
	flags.IntVar(&bsBlockSize, "block-size", 0, "per-op block size in bytes (default: 8 MiB for sequential/dedup, 4 KiB otherwise)")
	flags.IntVar(&bsWorkingSet, "working-set", 1, "number of files in the working set")
	flags.Uint64Var(&bsSeed, "seed", 1, "PRNG seed for randomized workloads")
	flags.IntVar(&bsWorkers, "workers", 1, "concurrent worker goroutines (mixed-ops-storm only)")
	flags.StringVar(&bsRemote, "remote", bsbench.RemoteMemory, "remote backend: memory | s3")
	flags.Float64Var(&bsGCGarbageRatio, "gc-garbage-ratio", bsbench.DefaultGCGarbage, "fraction of seeded chunks left unreferenced for the gc workload")
	flags.BoolVar(&bsFullProfiles, "full-profiles", false, "also capture mutex + block profiles (enables runtime mutex/block profilers; adds overhead)")
	flags.StringVar(&bsPhase, "phase", "", "optional capture phase subdir under <profile-dir>/blockstore/ (e.g. baseline | post-fix)")
	flags.StringVar(&bsReplay, "replay", "", "replay a recorded run: load workload params from <dir>/seed.txt (overrides other flags except --phase/--profile-dir)")
	// --workload is required unless replaying, where it comes from seed.txt.
}

func runBlockstore(cmd *cobra.Command, _ []string) error {
	var opts bsbench.Opts
	if bsReplay != "" {
		// Reproduce a recorded run from its seed.txt. --profile-dir and --phase
		// still apply so the replay lands in a fresh capture dir.
		loaded, full, err := loadSeed(bsReplay)
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		opts = loaded
		opts.ProfileDir = flagProfileDir
		bsFullProfiles = full
	} else {
		if bsWorkload == "" {
			return fmt.Errorf("--workload is required (or use --replay <dir>)")
		}
		opts = bsbench.Opts{
			Workload:   bsWorkload,
			Ops:        bsOps,
			BlockSize:  resolveBlockSize(bsWorkload, bsBlockSize),
			WorkingSet: bsWorkingSet,
			Workers:    bsWorkers,
			Seed:       bsSeed,
			Remote:     bsRemote,
			ProfileDir: flagProfileDir,
		}
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
	case bsbench.WorkloadMixedOpStorm:
		return runStormWorkload(ctx, cmd, opts, tmpDir)
	default:
		// Engine-backed workloads (sequential-write, random-write, ...).
		return runEngineWorkload(ctx, cmd, opts, tmpDir)
	}
}

// runStormWorkload wires the engine like runEngineWorkload but drives the
// concurrent mixed-ops-storm via RunStorm and prints the per-op-type tallies.
func runStormWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts, tmpDir string) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	bs, engineClose, err := bsbench.NewEngine(tmpDir, remoteStore)
	if err != nil {
		remoteClose()
		return err
	}
	defer engineClose()

	sess, err := startProfileSession(opts.ProfileDir, bsPhase, opts.Workload, bsFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return err
	}

	res, err := bsbench.RunStorm(ctx, bs, opts)
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}
	printResult(cmd, opts, res, sess.dir, true)
	if res.Storm != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"storm ops: workers=%d writes=%d reads=%d lists=%d deletes=%d\n",
			opts.Workers, res.Storm.Writes, res.Storm.Reads, res.Storm.Lists, res.Storm.Deletes)
	}
	return nil
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

	sess, err := startProfileSession(opts.ProfileDir, bsPhase, opts.Workload, bsFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return err
	}

	res, err := bsbench.RunWorkload(ctx, bs, opts)
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}
	printResult(cmd, opts, res, sess.dir, true)
	return nil
}

func runLocalWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts, tmpDir string) error {
	local, closeFn, err := bsbench.NewLocalStore(tmpDir)
	if err != nil {
		return err
	}
	defer closeFn()

	sess, err := startProfileSession(opts.ProfileDir, bsPhase, opts.Workload, bsFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return err
	}

	var res bsbench.Result
	switch opts.Workload {
	case bsbench.WorkloadWalk:
		res, err = bsbench.RunWalk(ctx, local, opts)
	case bsbench.WorkloadDelete:
		res, err = bsbench.RunDelete(ctx, local, opts)
	}
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}
	printResult(cmd, opts, res, sess.dir, false)
	return nil
}

func runGCWorkload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	defer remoteClose()

	sess, err := startProfileSession(opts.ProfileDir, bsPhase, opts.Workload, bsFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return err
	}

	res, err := bsbench.RunGC(ctx, remoteStore, opts, bsGCGarbageRatio)
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}
	printResult(cmd, opts, res, sess.dir, false)
	return nil
}

func runRawS3Workload(ctx context.Context, cmd *cobra.Command, opts bsbench.Opts) error {
	remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
	if err != nil {
		return err
	}
	defer remoteClose()

	sess, err := startProfileSession(opts.ProfileDir, bsPhase, opts.Workload, bsFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return err
	}

	res, err := bsbench.RunRawS3Put(ctx, remoteStore, opts)
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}
	printResult(cmd, opts, res, sess.dir, false)
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
