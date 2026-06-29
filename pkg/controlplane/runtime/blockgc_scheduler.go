package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// StartScheduledGC runs background garbage collection on the given interval
// until ctx is cancelled, reclaiming orphaned blocks on both the local and
// remote tiers without operator action (#1433). It returns immediately; the
// loop runs in its own goroutine and exits when ctx is done (server shutdown).
//
// Each tick runs RunBlockGC synchronously, so runs never overlap — a long run
// simply delays the next tick. GC is idempotent, so a tick skipped by shutdown
// or an error is harmless; the next tick catches up.
func (r *Runtime) StartScheduledGC(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.Info("auto-GC: scheduler started", "interval", interval)
		for {
			select {
			case <-ctx.Done():
				logger.Info("auto-GC: scheduler stopped")
				return
			case <-ticker.C:
				// A tick and ctx.Done() can be ready at the same time; select
				// picks randomly. Re-check so shutdown never triggers one last run.
				if ctx.Err() != nil {
					logger.Info("auto-GC: scheduler stopped")
					return
				}
				r.runScheduledGCOnce(ctx)
			}
		}
	}()
}

// runScheduledGCOnce performs one background GC pass and logs the outcome.
// Errors are logged, never fatal — the scheduler keeps running.
func (r *Runtime) runScheduledGCOnce(ctx context.Context) {
	start := time.Now()
	stats, err := r.RunBlockGC(ctx, "", false)
	if err != nil {
		logger.Error("auto-GC: run failed", "err", err)
		return
	}
	logger.Info("auto-GC: run complete",
		"objectsSwept", stats.ObjectsSwept,
		"bytesFreed", stats.BytesFreed,
		"errors", stats.ErrorCount,
		"durationMs", time.Since(start).Milliseconds(),
	)
}
