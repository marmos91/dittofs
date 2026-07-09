package run

import (
	"context"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/internal/dfsbench/fio"
	"github.com/marmos91/dittofs/internal/dfsbench/report"
)

// baselineScratchRoot is where the local-disk ceiling fio runs — plain dirs on
// the VM's scratch volume, no FS layer and no S3, so the numbers are the "is the
// bottleneck the filesystem or the hardware?" anchor every cell is read against.
// Each run gets its own subdir (below) so concurrent runs don't interfere and a
// cleanup only ever removes its own directory.
const baselineScratchRoot = "/var/tmp/dfsbench-baseline"

// measureLocalDiskCeiling runs fio straight against a scratch dir (no mount, no
// S3) to get the local-disk bandwidth + IOPS ceiling. size is the run's largest
// class, so the ceiling is measured at the same scale as the cells it anchors.
// Best-effort: a failure returns the error for the caller to warn-and-continue —
// a missing ceiling shouldn't abort the comparison.
//
// The raw-S3 PUT/GET ceiling (MinIO warp) is the other half of the plan's
// baseline; it's added once warp's flags/JSON are pinned on the VM.
func measureLocalDiskCeiling(ctx context.Context, size string, opts fio.LoadOpts) (report.Baseline, error) {
	if err := os.MkdirAll(baselineScratchRoot, 0o755); err != nil {
		return report.Baseline{}, err
	}
	// Per-run subdir: isolates concurrent runs and bounds the cleanup to our own
	// directory (never a recursive wipe of a shared, possibly-reused path).
	scratch, err := os.MkdirTemp(baselineScratchRoot, "run-*")
	if err != nil {
		return report.Baseline{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	// Lay the file down first so the read passes hit real bytes (mirrors the
	// matrix's layoutReadTarget), then measure sequential bandwidth and random
	// 4k IOPS — the two ceiling numbers the scorecard header shows.
	if _, err := fio.RunFio(ctx, "layout", scratch, withSize(opts, size)); err != nil {
		return report.Baseline{}, fmt.Errorf("baseline layout: %w", err)
	}
	seq, err := fio.RunFio(ctx, "seq-read", scratch, withSize(opts, size))
	if err != nil {
		return report.Baseline{}, fmt.Errorf("baseline seq-read: %w", err)
	}
	rnd, err := fio.RunFio(ctx, "rand-read-4k", scratch, withSize(opts, size))
	if err != nil {
		return report.Baseline{}, fmt.Errorf("baseline rand-read-4k: %w", err)
	}
	return report.Baseline{
		LocalDiskSeqMBps:  seq.ThroughputMBps,
		LocalDiskRandIOPS: rnd.IOPS,
	}, nil
}
