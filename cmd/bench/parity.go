package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/bench/parity"
)

var parityFlags struct {
	label      string
	conc       string
	quadrants  []string
	tools      []string
	profile    string
	seed       uint64
	outDir     string
	workDir    string
	rcloneBin  string
	sampleMs   int
	largeBytes int64
	largeCount int
	smallBytes int64
	smallCount int
	minChunk   int
	maxChunk   int
	keepRemote bool
}

var parityCmd = &cobra.Command{
	Use:   "parity",
	Short: "rclone-parity scorecard: dittofs engine vs rclone against the same S3 bucket (#1467)",
	Long: `parity runs the dittofs engine path (write -> rollup -> packed-block carve ->
drain) and an rclone baseline against the SAME bucket, apples-to-apples,
across upload/download x large/small-file quadrants plus a metadata-ops lane,
at several concurrency levels. Upload parallelism is pinned per cell so
dittofs ParallelUploads is comparable to rclone --transfers.

It emits a scorecard (JSON + CSV + markdown, dittofs-vs-rclone ratio per
quadrant per concurrency) into --out-dir, with per-cell datapath gauge
timelines (inflight, window, queue depth, goodput) embedded in the JSON.

The S3 target comes from environment variables ONLY (never files):
  required: AWS_S3_BUCKET, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
  optional: AWS_ENDPOINT_URL, AWS_S3_REGION, AWS_S3_PATH_STYLE, AWS_S3_KEY_PREFIX

Wrappers: bench/scripts/parity-smoke.sh (local MinIO, no cloud creds),
bench/scripts/parity-scw.sh (disposable Scaleway VM -> Cubbit / SCW S3).`,
	Example: `  # local smoke (MinIO): tiny sizes, two concurrency levels
  dfsbench parity --profile smoke

  # real WAN run (from the SCW VM): defaults = 4x1GiB + 2048x64KiB, conc 1/8/24/64
  dfsbench parity --label cubbit-fr-par

  # single quadrant at one concurrency
  dfsbench parity --quadrant upload-large --conc 64`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		opts := parity.Opts{
			Label:          parityFlags.label,
			Quadrants:      parityFlags.quadrants,
			Tools:          parityFlags.tools,
			Seed:           parityFlags.seed,
			OutDir:         parityFlags.outDir,
			WorkDir:        parityFlags.workDir,
			RcloneBin:      parityFlags.rcloneBin,
			Sample:         time.Duration(parityFlags.sampleMs) * time.Millisecond,
			LargeFileBytes: parityFlags.largeBytes,
			LargeFileCount: parityFlags.largeCount,
			SmallFileBytes: parityFlags.smallBytes,
			SmallFileCount: parityFlags.smallCount,
			MinChunk:       parityFlags.minChunk,
			MaxChunk:       parityFlags.maxChunk,
			KeepRemote:     parityFlags.keepRemote,
		}
		var err error
		if opts.Conc, err = parseConcList(parityFlags.conc); err != nil {
			return err
		}
		// --profile smoke shrinks the dataset so the whole harness (both
		// tools, all quadrants) validates in a couple of minutes against a
		// local MinIO. Explicit size flags win over the profile.
		if parityFlags.profile == "smoke" {
			if opts.LargeFileBytes <= 0 {
				opts.LargeFileBytes = 32 << 20
			}
			if opts.LargeFileCount <= 0 {
				opts.LargeFileCount = 2
			}
			if opts.SmallFileBytes <= 0 {
				opts.SmallFileBytes = 64 << 10
			}
			if opts.SmallFileCount <= 0 {
				opts.SmallFileCount = 128
			}
			if parityFlags.conc == "" {
				opts.Conc = []int{1, 4}
			}
		} else if parityFlags.profile != "" && parityFlags.profile != "wan" {
			return fmt.Errorf("unknown profile %q (want smoke or wan)", parityFlags.profile)
		}
		_, err = parity.Execute(cmd.Context(), opts)
		return err
	},
}

func parseConcList(s string) ([]int, error) {
	if s == "" {
		return nil, nil // Opts.Validate applies the default sweep
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("--conc: %q is not an integer", part)
		}
		out = append(out, n)
	}
	return out, nil
}

func init() {
	f := parityCmd.Flags()
	f.StringVar(&parityFlags.label, "label", "parity", "run label (artifact names + remote key prefix)")
	f.StringVar(&parityFlags.conc, "conc", "", "comma-separated concurrency levels (default 1,8,24,64; smoke profile: 1,4)")
	f.StringSliceVar(&parityFlags.quadrants, "quadrant", nil, "quadrants to run: upload-large,download-large,upload-small,download-small,meta (default all)")
	f.StringSliceVar(&parityFlags.tools, "tool", nil, "tools to run: dittofs,rclone (default both)")
	f.StringVar(&parityFlags.profile, "profile", "wan", "size preset: wan (default) | smoke (tiny, for local MinIO validation)")
	f.Uint64Var(&parityFlags.seed, "seed", 1467, "deterministic dataset seed")
	f.StringVar(&parityFlags.outDir, "out-dir", "bench/results", "artifact output directory")
	f.StringVar(&parityFlags.workDir, "work-dir", "", "scratch dir for datasets + engine state (default: temp dir, removed at exit)")
	f.StringVar(&parityFlags.rcloneBin, "rclone-bin", "rclone", "rclone binary")
	f.IntVar(&parityFlags.sampleMs, "sample-ms", 500, "gauge-timeline sample interval (ms)")
	f.Int64Var(&parityFlags.largeBytes, "large-file-bytes", 0, "large-file size (default 1GiB; smoke 32MiB)")
	f.IntVar(&parityFlags.largeCount, "large-file-count", 0, "large-file count (default 4; smoke 2)")
	f.Int64Var(&parityFlags.smallBytes, "small-file-bytes", 0, "small-file size (default 64KiB)")
	f.IntVar(&parityFlags.smallCount, "small-file-count", 0, "small-file count (default 2048; smoke 128)")
	f.IntVar(&parityFlags.minChunk, "min-chunk", 0, "dittofs FastCDC min chunk bytes (0=default ~1MiB; lower to cut cold-read amplification, #1569)")
	f.IntVar(&parityFlags.maxChunk, "max-chunk", 0, "dittofs FastCDC max chunk bytes ceiling (default 16MiB when --min-chunk set)")
	f.BoolVar(&parityFlags.keepRemote, "keep-remote", false, "skip remote cleanup + meta delete phase (debugging)")
	rootCmd.AddCommand(parityCmd)
}
