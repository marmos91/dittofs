package main

import (
	"context"
	"fmt"
	"os"
	"time"

	snapbench "github.com/marmos91/dittofs/bench/snapshots"
	remotememory "github.com/marmos91/dittofs/pkg/blockstore/remote/memory"
	"github.com/spf13/cobra"
)

var (
	snapEngine        string
	snapFiles         int
	snapBlocksPerFile int
	snapBlockSize     int
	snapDedup         int
)

var snapshotsCmd = &cobra.Command{
	Use:   "snapshots",
	Short: "Scale/perf workload for the snapshot create + verify pipeline",
	Long: `Seeds a synthetic share (--files regular files, each with
--blocks-per-file BlockRefs) into the chosen metadata engine, then times
the three snapshot cost centers in sequence:

  backup          metadata dump (streamed) + resident HashSet
  write-manifest  sorted hex-line manifest (streamed)
  verify          HEAD-probe every hash against an in-memory remote (conc 16)

Reports wall time and dump/manifest sizes per stage. Uses an in-memory
remote so there is no S3 request cost; multiply the verify time by a real
per-HEAD RTT for an S3 budget estimate.`,
	RunE: runSnapshots,
}

func init() {
	flags := snapshotsCmd.Flags()
	flags.StringVar(&snapEngine, "engine", snapbench.EngineMemory, "metadata engine: memory | badger")
	flags.IntVar(&snapFiles, "files", 100000, "number of synthetic files to seed")
	flags.IntVar(&snapBlocksPerFile, "blocks-per-file", 1, "BlockRefs per file")
	flags.IntVar(&snapBlockSize, "block-size", 1<<20, "logical block size in bytes (metadata only)")
	flags.IntVar(&snapDedup, "dedup", 1, "share every Nth block hash (>1 shrinks unique-hash count)")
}

func runSnapshots(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	w := cmd.OutOrStdout()

	opts := snapbench.SeedOpts{
		Engine:        snapEngine,
		Files:         snapFiles,
		BlocksPerFile: snapBlocksPerFile,
		BlockSize:     uint32(snapBlockSize),
		Dedup:         snapDedup,
	}
	if opts.Engine == snapbench.EngineBadger {
		dir, err := os.MkdirTemp("", "bench-snapshots-")
		if err != nil {
			return fmt.Errorf("temp dir: %w", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()
		opts.DBPath = dir
	}

	seedStart := time.Now()
	store, unique, cleanup, err := snapbench.NewStore(ctx, opts)
	if err != nil {
		return err
	}
	defer cleanup()
	seedDur := time.Since(seedStart)

	backupStart := time.Now()
	backup, err := snapbench.RunBackup(ctx, store)
	if err != nil {
		return err
	}
	backupDur := time.Since(backupStart)

	manifestStart := time.Now()
	manifestBytes, err := snapbench.RunWriteManifest(backup.HashSet)
	if err != nil {
		return err
	}
	manifestDur := time.Since(manifestStart)

	rs := remotememory.New()
	defer func() { _ = rs.Close() }()
	if _, err := snapbench.SeedRemote(ctx, rs, backup.HashSet); err != nil {
		return err
	}
	verifyStart := time.Now()
	probes, err := snapbench.RunVerify(ctx, rs, backup.HashSet)
	if err != nil {
		return err
	}
	verifyDur := time.Since(verifyStart)

	_, _ = fmt.Fprintf(w,
		"engine=%s files=%d blocks_per_file=%d unique_hashes=%d\n",
		opts.Engine, opts.Files, snapBlocksPerFile, unique,
	)
	_, _ = fmt.Fprintf(w,
		"seed=%s backup=%s write_manifest=%s verify=%s\n",
		seedDur.Round(time.Millisecond), backupDur.Round(time.Millisecond),
		manifestDur.Round(time.Millisecond), verifyDur.Round(time.Millisecond),
	)
	_, _ = fmt.Fprintf(w,
		"dump_bytes=%d manifest_bytes=%d manifest_hashes=%d verify_probes=%d\n",
		backup.DumpBytes, manifestBytes, backup.ManifestHashes, probes,
	)
	return nil
}
