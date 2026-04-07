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
//  3. The process can write to baseDir — verified by creating a temporary
//     marker file under a hidden subdirectory and immediately removing it.
//     This catches read-only mounts and permission regressions that a
//     plain stat() would miss.
//
// On any failure the report is [health.StatusUnhealthy] with a message
// describing which check tripped. On success it is [health.StatusHealthy]
// with the measured probe latency.
//
// The probe is intentionally light. It does not walk subdirectories,
// touch the fdPool, or interact with the in-memory block maps; the
// expected per-call cost is two filesystem syscalls plus an unlink.
// Cache the result via [health.CachedChecker] in callers that hit it
// from a hot /status route.
func (bs *FSStore) Healthcheck(ctx context.Context) health.Report {
	start := time.Now()

	makeReport := func(status health.Status, msg string) health.Report {
		return health.Report{
			Status:    status,
			Message:   msg,
			CheckedAt: time.Now().UTC(),
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	if err := ctx.Err(); err != nil {
		return makeReport(health.StatusUnhealthy, err.Error())
	}

	if bs.closedFlag.Load() {
		return makeReport(health.StatusUnhealthy, "fs block store is closed")
	}

	info, err := os.Stat(bs.baseDir)
	if err != nil {
		return makeReport(
			health.StatusUnhealthy,
			fmt.Sprintf("baseDir stat: %v", err),
		)
	}
	if !info.IsDir() {
		return makeReport(
			health.StatusUnhealthy,
			fmt.Sprintf("baseDir %q is not a directory", bs.baseDir),
		)
	}

	// Write probe: create a temp file under baseDir, then remove it.
	// We use a fixed prefix so an unrelated leftover from a crashed
	// previous probe is recognisable; CreateTemp generates a unique
	// suffix so multiple concurrent probes don't collide.
	probePath := filepath.Join(bs.baseDir, ".dfs-health-probe-*")
	f, err := os.CreateTemp(bs.baseDir, ".dfs-health-probe-*")
	if err != nil {
		return makeReport(
			health.StatusUnhealthy,
			fmt.Sprintf("write probe (create %q): %v", probePath, err),
		)
	}
	probeName := f.Name()
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(probeName) // best-effort cleanup
		return makeReport(
			health.StatusUnhealthy,
			fmt.Sprintf("write probe (close): %v", closeErr),
		)
	}
	if removeErr := os.Remove(probeName); removeErr != nil {
		return makeReport(
			health.StatusUnhealthy,
			fmt.Sprintf("write probe (remove): %v", removeErr),
		)
	}

	return makeReport(health.StatusHealthy, "")
}
