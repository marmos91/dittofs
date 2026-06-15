package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	mdbench "github.com/marmos91/dittofs/bench/metadata"
	"github.com/spf13/cobra"
)

var (
	mdBackend      string
	mdWorkload     string
	mdOps          int
	mdWorkers      int
	mdSeed         uint64
	mdDirs         int
	mdFilesPerDir  int
	mdReaddirLimit int
	mdPhase        string
	mdFullProfiles bool

	// Postgres connection. Defaults match the CI service
	// (.github/workflows/integration-tests.yml) so a bench reuses the same
	// database the conformance suite runs against.
	mdPGHost     string
	mdPGPort     int
	mdPGDatabase string
	mdPGUser     string
	mdPGPassword string
	mdPGSSLMode  string
	mdPGPrepare  bool
)

var metadataCmd = &cobra.Command{
	Use:   "metadata",
	Short: "Drive a read workload directly against a metadata store backend",
	Long: `Measures per-backend metadata read cost (GetFile / GetChild / ListChildren)
by calling the store directly — no NFS/SMB protocol, no client attribute cache
in the way. This is the decisive signal for whether a server-side metadata read
cache is worth building (issue #1169): the e2e mount path is masked by client
caching, this path is not.

Backends:
  memory     pure RAM — the zero-cost floor a perfect cache would approach
  badger     local embedded LSM in a temp dir
  postgres   connects to an existing database (--pg-*), auto-migrates, resets first

Workloads:
  getattr    store.GetFile over the seeded file working set
  lookup     store.GetChild(dir, name)
  readdir    store.ListChildren, paginated to completion
  mixed      browse blend: 45% lookup, 45% getattr, 10% readdir

Captures cpu + heap + goroutine pprof (plus mutex + block with --full-profiles)
to <profile-dir>/metadata/[<phase>/]<workload>-<timestamp>/.`,
	RunE: runMetadata,
}

func init() {
	f := metadataCmd.Flags()
	f.StringVar(&mdBackend, "backend", mdbench.BackendMemory, "backend: memory | badger | postgres")
	f.StringVar(&mdWorkload, "workload", mdbench.WorkloadMixed, "workload: getattr | lookup | readdir | mixed")
	f.IntVar(&mdOps, "ops", 100000, "number of read operations to time")
	f.IntVar(&mdWorkers, "workers", 1, "concurrent worker goroutines")
	f.Uint64Var(&mdSeed, "seed", 1, "PRNG seed for the access stream")
	f.IntVar(&mdDirs, "dirs", 16, "number of directories to seed")
	f.IntVar(&mdFilesPerDir, "files-per-dir", 256, "files seeded per directory")
	f.IntVar(&mdReaddirLimit, "readdir-limit", mdbench.DefaultReaddirLimit, "page size for the readdir workload")
	f.StringVar(&mdPhase, "phase", "", "optional capture phase subdir under <profile-dir>/metadata/ (e.g. baseline | post-fix)")
	f.BoolVar(&mdFullProfiles, "full-profiles", false, "also capture mutex + block profiles (adds runtime overhead)")

	f.StringVar(&mdPGHost, "pg-host", "localhost", "postgres host")
	f.IntVar(&mdPGPort, "pg-port", 5432, "postgres port")
	f.StringVar(&mdPGDatabase, "pg-dbname", "dittofs_test", "postgres database")
	f.StringVar(&mdPGUser, "pg-user", "postgres", "postgres user")
	f.StringVar(&mdPGPassword, "pg-password", "postgres", "postgres password")
	f.StringVar(&mdPGSSLMode, "pg-sslmode", "disable", "postgres sslmode")
	f.BoolVar(&mdPGPrepare, "pg-prepare", true, "use server-side prepared statements (production default)")
}

func runMetadata(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	opts := mdbench.Opts{
		Backend:      mdBackend,
		Workload:     mdWorkload,
		Ops:          mdOps,
		Workers:      mdWorkers,
		Seed:         mdSeed,
		Dirs:         mdDirs,
		FilesPerDir:  mdFilesPerDir,
		ReaddirLimit: mdReaddirLimit,
	}
	if err := opts.Validate(); err != nil {
		return err
	}

	store, cleanup, err := mdbench.OpenStore(ctx, mdBackend, mdbench.PGConfig{
		Host:     mdPGHost,
		Port:     mdPGPort,
		Database: mdPGDatabase,
		User:     mdPGUser,
		Password: mdPGPassword,
		SSLMode:  mdPGSSLMode,
		Prepare:  mdPGPrepare,
	})
	if err != nil {
		return fmt.Errorf("open %s store: %w", mdBackend, err)
	}
	defer cleanup()

	// Seed before starting the profile session so the capture reflects only the
	// read hot loop, not the write-path samples of building the fixture tree.
	tree, err := mdbench.Seed(ctx, store, opts)
	if err != nil {
		return err
	}

	sess, err := startProfileSession(flagProfileDir, "metadata", mdPhase, mdWorkload, mdFullProfiles)
	if err != nil {
		return err
	}
	defer func() { _ = sess.stop() }()
	if err := writeMetadataSeed(sess.dir, opts); err != nil {
		return err
	}

	res, err := mdbench.RunOnTree(ctx, store, tree, opts)
	if stopErr := sess.stop(); err == nil {
		err = stopErr
	}
	if err != nil {
		return err
	}

	printMetadataResult(cmd, res, sess.dir)
	return nil
}

// printMetadataResult emits one machine-parseable line. Latency percentiles are
// reported in microseconds for readability; raw ns live in the pprof captures.
func printMetadataResult(cmd *cobra.Command, res mdbench.Result, profDir string) {
	durMs := float64(res.Duration.Microseconds()) / 1000.0
	opsPerSec := float64(res.Ops) / res.Duration.Seconds()
	var p50, p95, p99 float64
	if res.Latency != nil {
		p50 = float64(res.Latency.P50Ns) / 1000.0
		p95 = float64(res.Latency.P95Ns) / 1000.0
		p99 = float64(res.Latency.P99Ns) / 1000.0
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"backend=%s workload=%s ops=%d dur=%.3fms ops_per_sec=%.2f p50_us=%.3f p95_us=%.3f p99_us=%.3f errors=%d profiles=%s\n",
		res.Backend, res.Workload, res.Ops, durMs, opsPerSec, p50, p95, p99, res.Errors, profDir,
	)
}

// writeMetadataSeed records the run parameters next to the profiles so a
// capture is self-describing. Mirrors the blockstore seed.txt convention.
func writeMetadataSeed(dir string, opts mdbench.Opts) error {
	body := fmt.Sprintf(
		"backend=%s\nworkload=%s\nops=%d\nworkers=%d\nseed=%d\ndirs=%d\nfiles_per_dir=%d\nreaddir_limit=%d\n",
		opts.Backend, opts.Workload, opts.Ops, opts.Workers, opts.Seed, opts.Dirs, opts.FilesPerDir, opts.ReaddirLimit,
	)
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte(body), 0o644); err != nil {
		return fmt.Errorf("write seed.txt: %w", err)
	}
	return nil
}
