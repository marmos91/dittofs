package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/health"
)

// StartScheduledIntegrityScan runs the structural manifest scan over every
// share on the given interval until ctx is cancelled. It returns
// immediately; the loop runs in its own goroutine and exits when ctx is
// done (server shutdown).
//
// Each tick scans the shares one at a time and synchronously, so runs never
// overlap and the walk never competes with itself for the metadata store. A
// long run simply delays the next tick. The scan writes nothing, so a tick
// skipped by shutdown or an error costs only freshness; the next tick
// catches up.
//
// ponytail: a plain ticker, so the schedule restarts from zero on every
// server start and a box restarted more often than the interval never
// completes a scan. Upgrade to a persisted per-share last-run time, scanning
// at startup when it is older than the interval, once operators report such
// boxes — it puts a full metadata walk on the startup path, which is the
// cost this scheduler exists to keep off it.
func (r *Runtime) StartScheduledIntegrityScan(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		logger.Info("integrity scan: scheduler started", "interval", interval)
		for {
			select {
			case <-ctx.Done():
				logger.Info("integrity scan: scheduler stopped")
				return
			case <-ticker.C:
				// A tick and ctx.Done() can be ready at the same time and
				// select picks at random. Re-check so shutdown never
				// triggers one last walk.
				if ctx.Err() != nil {
					logger.Info("integrity scan: scheduler stopped")
					return
				}
				r.runIntegrityScanOnce(ctx)
			}
		}
	}()
}

// runIntegrityScanOnce scans every registered share once, recording each
// outcome for the share's status report. Errors are logged and recorded,
// never fatal — one unscannable share must not stop the others or the
// scheduler.
func (r *Runtime) runIntegrityScanOnce(ctx context.Context) {
	for _, name := range r.ListShares() {
		if ctx.Err() != nil {
			return
		}
		r.scanShareIntegrity(ctx, name)
	}
}

// recordShareIntegrity stores a completed scan's outcome as the share's current
// integrity status. Called by every scan, scheduled or operator-run.
func (r *Runtime) recordShareIntegrity(share string, res *engine.ManifestCheckResult) {
	r.setShareIntegrity(share, &health.IntegrityStatus{
		LastRunAt:              res.CompletedAt,
		DurationMS:             res.DurationMS,
		FilesScanned:           res.FilesScanned,
		PayloadsWithFindings:   res.PayloadsWithFindings,
		DamagedPayloads:        res.DamagedPayloads,
		ClaimedUncoveredRanges: res.ClaimedUncoveredRanges,
		UnplaceableRows:        res.UnplaceableRows,
		UnknownHashRows:        res.UnknownHashRows,
	})
}

// scanShareIntegrity runs one read-only manifest scan. CheckManifests records
// the outcome; this adds the failure case and the operator-facing warning.
func (r *Runtime) scanShareIntegrity(ctx context.Context, share string) {
	res, err := r.CheckManifests(ctx, share, engine.ManifestCheckOptions{})
	if err != nil {
		// Context cancellation is shutdown, not a finding: recording it
		// would leave every share reporting a failed scan across a restart.
		if ctx.Err() != nil {
			return
		}
		// The share list is sampled once per tick, so a share removed since
		// then fails here. Recording that would put an entry back for a name
		// the removal just cleared, and describe a share that is gone.
		if errors.Is(err, shares.ErrShareNotFound) {
			return
		}
		logger.Error("integrity scan: share scan failed", logger.KeyShare, share, "error", err)
		r.setShareIntegrity(share, &health.IntegrityStatus{
			LastRunAt: time.Now().UTC(),
			Error:     err.Error(),
		})
		return
	}

	if res.DamagedPayloads > 0 {
		logger.Warn("integrity scan: share has damaged payloads",
			logger.KeyShare, share,
			"damaged_payloads", res.DamagedPayloads,
			"claimed_uncovered_ranges", res.ClaimedUncoveredRanges,
			"unplaceable_rows", res.UnplaceableRows,
			"unknown_hash_rows", res.UnknownHashRows,
		)
	}
}

// setShareIntegrity records a share's latest scan outcome.
func (r *Runtime) setShareIntegrity(share string, status *health.IntegrityStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shareIntegrity == nil {
		r.shareIntegrity = make(map[string]*health.IntegrityStatus)
	}
	r.shareIntegrity[share] = status
}

// ShareIntegrity returns the named share's most recent scan outcome, or nil
// when no scan has run for it in this process. The returned value is a copy,
// so a caller never holds a pointer into state the next scan replaces.
func (r *Runtime) ShareIntegrity(share string) *health.IntegrityStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	status := r.shareIntegrity[share]
	if status == nil {
		return nil
	}
	cp := *status
	return &cp
}

// ShareStatus returns the named share's health report together with the
// structural counters recorded alongside it. A share whose manifest scan
// found damaged payloads reads as degraded even when every subsystem probe
// comes back healthy, because that damage is invisible to the probes.
//
// A worse status from a subsystem probe always wins; the scan can only
// downgrade a healthy share, never upgrade an unhealthy one.
func (r *Runtime) ShareStatus(ctx context.Context, share string) health.ShareStatus {
	out := withIntegrity(r.ShareChecker(share).Healthcheck(ctx), r.ShareIntegrity(share))
	out.Offline = r.ShareOffline(share)
	return out
}

// withIntegrity joins a subsystem health report to a recorded scan outcome,
// downgrading a healthy share to degraded when the scan found damage. Pure
// function — no I/O — so the downgrade rule can be tested on its own.
func withIntegrity(rep health.Report, in *health.IntegrityStatus) health.ShareStatus {
	out := health.ShareStatus{Report: rep, Integrity: in}
	if in == nil || in.DamagedPayloads == 0 || rep.Status != health.StatusHealthy {
		return out
	}
	out.Status = health.StatusDegraded
	out.Message = fmt.Sprintf(
		"integrity: %d damaged payloads of %d with findings; run `dfsctl store check` for detail",
		in.DamagedPayloads, in.PayloadsWithFindings)
	return out
}
