package shares

import (
	"context"
	"strings"
	"time"

	"github.com/marmos91/dittofs/pkg/health"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Healthcheck returns the share's overall health by combining the
// reports from its block store engine and metadata store. The result
// satisfies [health.Checker]'s contract semantically (the method
// itself doesn't satisfy the interface because it requires the meta
// store as an argument; the [Runtime.HealthcheckShare] convenience
// method on the runtime does the lookup and exposes a Checker-shaped
// surface).
//
// # Worst-of derivation
//
// The share is the union of two subsystems and is only as healthy as
// the weakest one:
//
//   - If the metadata store is [health.StatusUnhealthy] → share is
//     unhealthy. The share cannot do anything useful without working
//     metadata.
//   - If the block store engine is [health.StatusUnhealthy] (its
//     local store is broken) → share is unhealthy. Same reasoning.
//   - If either subsystem is [health.StatusDegraded] → share is
//     degraded. The most common case is a healthy local store with an
//     unreachable remote (engine reports degraded), which still lets
//     reads from the local cache succeed but queues writes.
//   - If either subsystem is [health.StatusUnknown] → share is
//     unknown. We can't make a definitive call until both subsystems
//     have produced a positive answer.
//   - Otherwise → [health.StatusHealthy].
//
// The combined message preserves the worst-status component's message,
// prefixed with the subsystem name (e.g. "metadata: …" or "block: …")
// so an operator can immediately see which side is at fault. When
// both sides are at the same severity the metadata store wins the tie
// because corrupt metadata is usually the more impactful failure.
//
// # Context handling
//
// A canceled or deadlined caller context surfaces as
// [health.StatusUnknown] before either sub-probe runs.
//
// # Local-only / metadata-only shares
//
// A share with no block store at all (BlockStore == nil — the
// "metadata-only" edge case) skips the engine check and reports
// purely on the metadata store's status.
//
// A share with a remote-less block store (local-only) is handled
// transparently because [engine.BlockStore.Healthcheck] already
// returns healthy when there is no remote configured.
func (s *Share) Healthcheck(ctx context.Context, metaStore metadata.MetadataStore) health.Report {
	start := time.Now()
	now := time.Now().UTC()

	if err := ctx.Err(); err != nil {
		return health.Report{
			Status:    health.StatusUnknown,
			Message:   err.Error(),
			CheckedAt: now,
			LatencyMs: time.Since(start).Milliseconds(),
		}
	}

	// Probe both subsystems regardless of intermediate results so the
	// reported latency reflects the worst-case wall time. This is a
	// deliberate choice: a /status caller wants the operator to see
	// "share check took 700ms because the remote is slow", not "share
	// check returned in 0.2ms because metadata was unhealthy".
	var metaRep health.Report
	if metaStore != nil {
		metaRep = metaStore.Healthcheck(ctx)
	}
	var blockRep health.Report
	if s.BlockStore != nil {
		blockRep = s.BlockStore.Healthcheck(ctx)
	}

	worst := combineShareReports(metaRep, blockRep, metaStore != nil, s.BlockStore != nil)
	worst.CheckedAt = now
	worst.LatencyMs = time.Since(start).Milliseconds()
	return worst
}

// combineShareReports computes the worst-of two [health.Report]s and
// returns a synthesised report carrying the worst component's status
// and a tagged message. Pure function — no I/O — so it can be unit
// tested without spinning up real stores.
//
// hasMeta and hasBlock indicate whether each subsystem was actually
// probed (the corresponding report is meaningless when its
// "has-side" is false).
func combineShareReports(metaRep, blockRep health.Report, hasMeta, hasBlock bool) health.Report {
	// If neither subsystem is even present, we have no signal.
	if !hasMeta && !hasBlock {
		return health.Report{
			Status:  health.StatusUnknown,
			Message: "share has neither metadata store nor block store",
		}
	}

	// Severity from worst to best. Tie-break favours metadata.
	severity := func(s health.Status) int {
		switch s {
		case health.StatusUnhealthy:
			return 4
		case health.StatusDegraded:
			return 3
		case health.StatusUnknown:
			return 2
		case health.StatusHealthy:
			return 1
		default:
			return 0
		}
	}

	type sideReport struct {
		tag string
		rep health.Report
	}
	var sides []sideReport
	if hasMeta {
		sides = append(sides, sideReport{tag: "metadata", rep: metaRep})
	}
	if hasBlock {
		sides = append(sides, sideReport{tag: "block", rep: blockRep})
	}

	worst := sides[0]
	for _, s := range sides[1:] {
		if severity(s.rep.Status) > severity(worst.rep.Status) {
			worst = s
		}
	}

	out := health.Report{Status: worst.rep.Status}
	if msg := worst.rep.Message; msg != "" {
		out.Message = worst.tag + ": " + msg
	} else if worst.rep.Status != health.StatusHealthy {
		// Surface the subsystem name even when the upstream report
		// has no message — operators still want to know which side
		// produced the non-healthy state.
		out.Message = worst.tag + ": " + strings.ToLower(string(worst.rep.Status))
	}
	return out
}
