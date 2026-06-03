package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bsbench "github.com/marmos91/dittofs/bench/blockstore"
	"github.com/marmos91/dittofs/bench/orchestrator"
	"github.com/spf13/cobra"
)

var (
	orchManifest      string
	orchOut           string
	orchSummary       bool
	orchTimestamp     string
	orchRunID         string
	orchGitSHA        string
	orchPhase         string
	orchFullProfiles  bool
	orchCompareBase   string
	orchCompareCand   string
	orchCompareThresh float64
)

var orchestrateCmd = &cobra.Command{
	Use:   "orchestrate",
	Short: "Run a manifest of blockstore workloads and emit a versioned result JSON",
	Long: `Runs each workload in a manifest (default: a fast in-process memory-remote
set; override with --manifest <file.json>) through the same bench/blockstore
primitives the 'blockstore' subcommand uses, capturing the pprof envelope per
workload, and emits a versioned result document (schema_version 1).

Run metadata is injected for reproducibility: --timestamp defaults to now in
RFC3339 but can be pinned; --git-sha defaults to the build's embedded VCS
revision. --phase nests profile capture under <profile-dir>/blockstore/<phase>/
so baseline vs post-fix runs sit side by side.

Compare mode diffs two result JSONs by workload and flags ns/op regressions:
  dfsbench orchestrate --compare-baseline base.json --compare-candidate new.json`,
	RunE: runOrchestrate,
}

func init() {
	flags := orchestrateCmd.Flags()
	flags.StringVar(&orchManifest, "manifest", "", "JSON manifest file (default: built-in fast manifest)")
	flags.StringVar(&orchOut, "out", "", "write result JSON to this path (default: stdout)")
	flags.BoolVar(&orchSummary, "summary", false, "also print a human-readable summary table to stderr")
	flags.StringVar(&orchTimestamp, "timestamp", "", "run timestamp in RFC3339 (default: now, UTC)")
	flags.StringVar(&orchRunID, "run-id", "", "run identifier (default: derived from the timestamp)")
	flags.StringVar(&orchGitSHA, "git-sha", "", "git SHA to stamp (default: build VCS revision)")
	flags.StringVar(&orchPhase, "phase", "", "profile capture phase subdir (e.g. baseline | post-fix)")
	flags.BoolVar(&orchFullProfiles, "full-profiles", false, "also capture mutex + block profiles per workload")
	flags.StringVar(&orchCompareBase, "compare-baseline", "", "compare mode: baseline result JSON")
	flags.StringVar(&orchCompareCand, "compare-candidate", "", "compare mode: candidate result JSON")
	flags.Float64Var(&orchCompareThresh, "compare-threshold", 10, "compare mode: ns/op regression threshold in percent")

	rootCmd.AddCommand(orchestrateCmd)
}

func runOrchestrate(cmd *cobra.Command, _ []string) error {
	if orchCompareBase != "" || orchCompareCand != "" {
		return runCompare(cmd)
	}

	manifest := orchestrator.DefaultManifest()
	if orchManifest != "" {
		f, err := os.Open(orchManifest)
		if err != nil {
			return fmt.Errorf("open manifest: %w", err)
		}
		defer func() { _ = f.Close() }()
		if manifest, err = orchestrator.LoadManifest(f); err != nil {
			return err
		}
	}

	ts := orchTimestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	ts, err := orchestrator.ParseTimestamp(ts)
	if err != nil {
		return err
	}
	runID := orchRunID
	if runID == "" {
		runID = "run-" + ts
	}
	gitSHA := orchGitSHA
	if gitSHA == "" {
		gitSHA = orchestrator.BuildGitSHA()
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	doc, err := orchestrator.Run(ctx, manifest, runID, ts, gitSHA, orchestrator.LocalSystem(), engineRunner)
	if err != nil {
		// A partial/aborted run still produced a document worth emitting; write
		// it before surfacing the error so failures are not silently lost.
		_ = emitDocument(cmd, doc)
		return err
	}

	if err := emitDocument(cmd, doc); err != nil {
		return err
	}
	if orchSummary {
		orchestrator.WriteSummary(cmd.ErrOrStderr(), doc)
	}
	if doc.Outcome != orchestrator.OutcomeCompleted {
		return fmt.Errorf("run outcome %s", doc.Outcome)
	}
	return nil
}

// emitDocument marshals the document and writes it to --out (and/or stdout).
func emitDocument(cmd *cobra.Command, doc *orchestrator.Document) error {
	b, err := doc.Marshal()
	if err != nil {
		return err
	}
	if orchOut != "" {
		if err := os.WriteFile(orchOut, b, 0o644); err != nil {
			return fmt.Errorf("write --out: %w", err)
		}
		return nil
	}
	_, err = cmd.OutOrStdout().Write(b)
	return err
}

// engineRunner is the real WorkloadRunner: it maps a manifest entry to
// blockstore.Opts, wires the matching backend, wraps the timed region in the
// shared pprof envelope, and returns the measured metrics plus profile paths.
func engineRunner(ctx context.Context, p orchestrator.WorkloadParams) (orchestrator.RunOutput, error) {
	opts := bsbench.Opts{
		Workload:   p.Workload,
		Ops:        p.Ops,
		BlockSize:  resolveBlockSize(p.Workload, p.BlockSize),
		WorkingSet: p.WorkingSet,
		Workers:    p.Workers,
		Seed:       p.Seed,
		Remote:     p.Remote,
		Mix:        p.Mix,
		ProfileDir: flagProfileDir,
	}

	tmpDir, err := os.MkdirTemp("", "bench-orch-")
	if err != nil {
		return orchestrator.RunOutput{}, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	res, profDir, err := runOneWorkload(ctx, opts, tmpDir)
	if err != nil {
		return orchestrator.RunOutput{}, err
	}

	metrics := orchestrator.MetricsFromRun(res.Duration.Nanoseconds(), int64(res.Ops), res.Bytes)
	// Fold in the per-op latency distribution + succeeded/failed breakdown the
	// runner recorded. These are additive schema fields (no version bump): a
	// runner that records no samples leaves them nil/zero.
	if res.Latency != nil {
		metrics.Latency = orchestrator.LatencyFromSamples(res.Latency.Samples())
		succeeded, failed := res.Latency.Counts()
		if succeeded+failed > 0 {
			metrics.OpCounts = orchestrator.OpCounts{
				Total:     succeeded + failed,
				Succeeded: succeeded,
				Failed:    failed,
			}
		}
	}

	out := orchestrator.RunOutput{Metrics: metrics}
	if profDir != "" {
		out.ProfilePaths = profileFiles(profDir, orchFullProfiles)
	}
	return out, nil
}

// runOneWorkload wires the backend for opts.Workload, captures the pprof
// envelope around the timed region, and returns the Result plus the profile
// directory. It mirrors runBlockstore's dispatch but returns metrics instead of
// printing them so the orchestrator can collect across many workloads.
func runOneWorkload(ctx context.Context, opts bsbench.Opts, tmpDir string) (bsbench.Result, string, error) {
	switch opts.Workload {
	case bsbench.WorkloadWalk, bsbench.WorkloadDelete:
		local, closeFn, err := bsbench.NewLocalStore(tmpDir)
		if err != nil {
			return bsbench.Result{}, "", err
		}
		defer closeFn()
		return capture(opts, func() (bsbench.Result, error) {
			if opts.Workload == bsbench.WorkloadWalk {
				return bsbench.RunWalk(ctx, local, opts)
			}
			return bsbench.RunDelete(ctx, local, opts)
		})
	case bsbench.WorkloadGC:
		remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
		if err != nil {
			return bsbench.Result{}, "", err
		}
		defer remoteClose()
		return capture(opts, func() (bsbench.Result, error) {
			return bsbench.RunGC(ctx, remoteStore, opts, bsbench.DefaultGCGarbage)
		})
	case bsbench.WorkloadRawS3Put:
		remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
		if err != nil {
			return bsbench.Result{}, "", err
		}
		defer remoteClose()
		return capture(opts, func() (bsbench.Result, error) {
			return bsbench.RunRawS3Put(ctx, remoteStore, opts)
		})
	default:
		remoteStore, remoteClose, err := bsbench.SetupRemote(ctx, opts)
		if err != nil {
			return bsbench.Result{}, "", err
		}
		bs, engineClose, err := bsbench.NewEngine(tmpDir, remoteStore)
		if err != nil {
			remoteClose()
			return bsbench.Result{}, "", err
		}
		defer engineClose()
		return capture(opts, func() (bsbench.Result, error) {
			switch {
			case opts.Workload == bsbench.WorkloadMixedOpStorm:
				return bsbench.RunStorm(ctx, bs, opts)
			case bsbench.IsConcurrentWorkload(opts.Workload):
				return bsbench.RunConcurrent(ctx, bs, opts)
			default:
				return bsbench.RunWorkload(ctx, bs, opts)
			}
		})
	}
}

// capture wraps fn in a profile session and returns the result + profile dir.
func capture(opts bsbench.Opts, fn func() (bsbench.Result, error)) (bsbench.Result, string, error) {
	sess, err := startProfileSession(opts.ProfileDir, orchPhase, opts.Workload, orchFullProfiles)
	if err != nil {
		return bsbench.Result{}, "", err
	}
	defer func() { _ = sess.stop() }()
	if err := sess.writeSeed(opts); err != nil {
		return bsbench.Result{}, "", err
	}
	res, err := fn()
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return bsbench.Result{}, "", err
	}
	return res, sess.dir, nil
}

// profileFiles lists the pprof files a session writes into dir.
func profileFiles(dir string, full bool) []string {
	names := []string{"cpu.pprof", "heap.pprof", "goroutine.pprof"}
	if full {
		names = append(names, "mutex.pprof", "block.pprof")
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = filepath.Join(dir, n)
	}
	return out
}

func runCompare(cmd *cobra.Command) error {
	if orchCompareBase == "" || orchCompareCand == "" {
		return fmt.Errorf("compare mode needs both --compare-baseline and --compare-candidate")
	}
	base, err := orchestrator.DecodeFile(orchCompareBase)
	if err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	cand, err := orchestrator.DecodeFile(orchCompareCand)
	if err != nil {
		return fmt.Errorf("candidate: %w", err)
	}
	cmps := orchestrator.Compare(base, cand, orchCompareThresh)
	orchestrator.WriteComparison(cmd.OutOrStdout(), cmps)
	if orchestrator.HasRegression(cmps) {
		return fmt.Errorf("regression detected beyond %.1f%% threshold", orchCompareThresh)
	}
	return nil
}
