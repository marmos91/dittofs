package fs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
)

// Healthcheck verifies that the on-disk local block store is operational
// and returns a structured [health.Report].
//
// The probe performs three checks in sequence:
//
//  1. The store hasn't been Closed (closedFlag).
//  2. The configured baseDir exists and is a directory (os.Stat).
//  3. The process can write to baseDir — verified by [verifyWritable],
//     which creates a temporary marker file directly inside baseDir and
//     immediately removes it. This catches read-only mounts and
//     permission regressions that a plain stat() would miss.
//
// A canceled caller context surfaces as [health.StatusUnknown] (the
// probe was indeterminate, not the store). Any failed check surfaces
// as [health.StatusUnhealthy] with a message identifying which one
// tripped. Success is [health.StatusHealthy] with the measured
// probe latency.
//
// The probe is intentionally light. It does not walk subdirectories,
// touch the fdPool, or interact with the in-memory block maps; the
// expected per-call cost is two filesystem syscalls plus an unlink.
// Cache the result via [health.CachedChecker] in callers that hit it
// from a hot /status route.
func (bs *FSStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	if err := ctx.Err(); err != nil {
		return health.NewUnknownReport(err.Error(), time.Since(start))
	}

	if bs.closedFlag.Load() {
		return health.NewUnhealthyReport("fs block store is closed", time.Since(start))
	}

	info, err := os.Stat(bs.baseDir)
	if err != nil {
		return health.NewUnhealthyReport(fmt.Sprintf("baseDir stat: %v", err), time.Since(start))
	}
	if !info.IsDir() {
		return health.NewUnhealthyReport(
			fmt.Sprintf("baseDir %q is not a directory", bs.baseDir),
			time.Since(start),
		)
	}

	if err := verifyWritable(bs.baseDir); err != nil {
		return health.NewUnhealthyReport(err.Error(), time.Since(start))
	}

	return health.NewHealthyReport(time.Since(start))
}

// verifyWritable confirms the process can create, close, and unlink a
// file inside dir. It is the operator-facing notion of "this directory
// is writable right now": catches read-only mounts, permission
// regressions, and full filesystems that a plain os.Stat would miss.
//
// The probe uses os.CreateTemp with a fixed ".dfs-health-probe-*" prefix
// so that unrelated leftovers from a crashed previous probe are still
// recognisable on disk; the random suffix prevents collisions between
// concurrent probes.
func verifyWritable(dir string) error {
	probePath := filepath.Join(dir, ".dfs-health-probe-*")
	f, err := os.CreateTemp(dir, ".dfs-health-probe-*")
	if err != nil {
		return fmt.Errorf("write probe (create %q): %w", probePath, err)
	}
	probeName := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(probeName) // best-effort cleanup
		return fmt.Errorf("write probe (close): %w", closeErr)
	}
	if removeErr := os.Remove(probeName); removeErr != nil {
		return fmt.Errorf("write probe (remove): %w", removeErr)
	}
	return nil
}
