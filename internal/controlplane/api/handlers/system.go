package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// DefaultDrainStallTimeout is the fallback inactivity bound for DrainUploads,
// used when the configured value is non-positive (e.g. a zero-valued test
// handler). Mirrors pkg/controlplane/api.APIConfig.DrainStallTimeout's default.
const DefaultDrainStallTimeout = 5 * time.Minute

// drainProgressInterval is how often the idle watchdog samples upload progress.
// Smaller than any sane DrainStallTimeout so the watchdog reacts promptly once
// the idle window elapses, without busy-polling.
const drainProgressInterval = time.Second

// drainRuntime is the narrow slice of the runtime that the system handler
// needs. Defining it here (rather than depending on the whole *runtime.Runtime)
// keeps the handler testable with a lightweight fake. *runtime.Runtime
// satisfies it.
type drainRuntime interface {
	// DrainAllUploads blocks until every pending block has reached the remote
	// (or ctx is cancelled / a block fails permanently).
	DrainAllUploads(ctx context.Context) error
	// UploadProgress returns a monotonic count of concluded mirror attempts.
	// Used purely as a liveness signal by the idle watchdog.
	UploadProgress() int64
}

// SystemHandler handles system-level endpoints.
type SystemHandler struct {
	runtime drainRuntime
	// drainStallTimeout bounds DrainUploads by inactivity, not wall-clock.
	drainStallTimeout time.Duration
}

// NewSystemHandler creates a new system handler. drainStallTimeout is the
// inactivity bound for the drain-uploads endpoint; a non-positive value falls
// back to DefaultDrainStallTimeout.
func NewSystemHandler(rt *runtime.Runtime, drainStallTimeout time.Duration) *SystemHandler {
	if drainStallTimeout <= 0 {
		drainStallTimeout = DefaultDrainStallTimeout
	}
	// A typed-nil *runtime.Runtime would make the interface non-nil, defeating
	// the nil check in DrainUploads, so store nil explicitly in that case.
	if rt == nil {
		return &SystemHandler{drainStallTimeout: drainStallTimeout}
	}
	return &SystemHandler{runtime: rt, drainStallTimeout: drainStallTimeout}
}

// DrainUploads handles POST /api/v1/system/drain-uploads.
//
// Blocks until every in-flight block store upload has reached the remote across
// all shares. Useful for benchmarking (clean boundaries between test workloads)
// and for operators forcing a flush before maintenance.
//
// Timeout model. A real flush of many GiB legitimately runs for minutes — far
// longer than the control plane's global request timeout (chi
// middleware.Timeout, ~30s) and the HTTP write_timeout. Two things follow:
//
//  1. The drain must not inherit the request deadline, or it would be cancelled
//     at ~30s with data still un-uploaded (issue #1432). So it runs on a
//     context detached from the request via context.WithoutCancel.
//
//  2. There is deliberately no total wall-clock budget. A correct flush of an
//     arbitrarily large backlog takes arbitrarily long; any fixed cap is just a
//     number that's wrong for some workload. Instead the drain is bounded by
//     *inactivity*: an idle watchdog samples UploadProgress and aborts only if
//     no upload completes within drainStallTimeout (the remote has stalled).
//     This mirrors rclone's --timeout (an idle, progress-resetting timeout) and
//     matches how juicefs/s3ql treat a forced flush — block until done, give up
//     only on a genuine stall. CAS chunks are at most a few MiB, so even on a
//     slow link a single chunk finishes well inside any sane idle window.
//
// What a trip actually means is narrower than "the remote is wedged": the
// watchdog observes only that no block reached the remote for the window. A
// stuck carve, a stuck remote, and a drain that never had anything to do are
// indistinguishable from here, so the 504 reports the observation and how much
// landed before it, and leaves the diagnosis to the reader.
//
// A genuine client disconnect (request context Canceled, as opposed to the
// deadline-exceeded fired by middleware.Timeout) still aborts the drain.
//
// Returns 200 OK when all uploads have drained, or 504 Gateway Timeout if the
// drain stalls for drainStallTimeout.
func (h *SystemHandler) DrainUploads(w http.ResponseWriter, r *http.Request) {
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}

	// Clear this connection's write deadline. A multi-GiB flush legitimately runs
	// for minutes, far longer than http.Server.WriteTimeout (10s by default, 120s
	// under pprof); without this the transport tears the connection down mid-drain
	// and the client sees a bare EOF — not the 200/504 this handler returns. The
	// request-deadline detachment below handles chi middleware.Timeout; this
	// handles the separate transport-level write deadline. Best-effort: the stdlib
	// server's ResponseWriter supports it; a wrapper that does not just keeps its
	// deadline (#1627 fixed the symmetric client-side timeout).
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		logger.Warn("drain-uploads: could not clear write deadline; a long drain may be cut off", "error", err)
	}

	// Detach from the request deadline (total <= 0 → no wall-clock cap); the
	// idle watchdog below is the only bound. A real client disconnect still
	// cancels via the helper. cancel doubles as the watchdog's abort handle.
	ctx, cancel := detachFromRequest(r, 0)
	defer cancel()

	logger.Info("Drain uploads requested", "idle_timeout", h.drainStallTimeout)
	start := time.Now()
	startProgress := h.runtime.UploadProgress()

	err, stalled := h.runWithIdleWatchdog(ctx, cancel)
	if err != nil {
		// Report what was observed, not what it implies. The watchdog knows only
		// that the block count stopped moving; whether the remote is wedged, the
		// carve is stuck, or nothing was ever queued reads identically from here,
		// and a message that picks one sends the reader after the wrong thing.
		// How much did land, and over how long, is what narrows it.
		committed := h.runtime.UploadProgress() - startProgress
		logger.Error("Drain uploads failed", "error", err, "duration", time.Since(start),
			"stalled", stalled, "blocks_committed", committed)
		if stalled {
			GatewayTimeout(w, fmt.Sprintf(
				"drain uploads: no block reached the remote for %s; %d committed over the preceding %s: %v",
				h.drainStallTimeout, committed, time.Since(start).Round(time.Second), err))
		} else {
			InternalServerError(w, "drain uploads failed: "+err.Error())
		}
		return
	}

	logger.Info("Drain uploads complete", "duration", time.Since(start))
	WriteJSONOK(w, map[string]any{
		"status":   "drained",
		"duration": time.Since(start).String(),
	})
}

// runWithIdleWatchdog runs DrainAllUploads in a goroutine and supervises it
// with an inactivity watchdog, blocking until the drain finishes or stalls. It
// returns the drain's error and whether the watchdog aborted it — true when
// UploadProgress stayed flat for drainStallTimeout, which the caller maps to a
// 504. cancel must cancel the same ctx passed in.
func (h *SystemHandler) runWithIdleWatchdog(ctx context.Context, cancel context.CancelFunc) (error, bool) {
	drainDone := make(chan error, 1)
	go func() { drainDone <- h.runtime.DrainAllUploads(ctx) }()

	// Sample at least as often as the idle window itself, so a small configured
	// timeout still trips after roughly that window (and tests don't have to
	// wait a full second).
	interval := drainProgressInterval
	if h.drainStallTimeout < interval {
		interval = h.drainStallTimeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastProgress := h.runtime.UploadProgress()
	idleFor := time.Duration(0)
	for {
		select {
		case err := <-drainDone:
			return err, false
		case <-ticker.C:
			cur := h.runtime.UploadProgress()
			if cur != lastProgress {
				lastProgress = cur
				idleFor = 0
				continue
			}
			idleFor += interval
			if idleFor >= h.drainStallTimeout {
				cancel()
				return <-drainDone, true // wait for the drain to unwind on ctx cancel
			}
		}
	}
}
