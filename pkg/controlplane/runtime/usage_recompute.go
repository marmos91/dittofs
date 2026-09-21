package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// UsageRecomputeResult reports what a share's used-bytes repair moved, or —
// for a dry run — what it would have moved.
type UsageRecomputeResult struct {
	// ShareName is the share the counter was read for.
	ShareName string `json:"share_name"`
	// BeforeBytes is the share's used bytes as reported before the rebuild.
	BeforeBytes int64 `json:"before_bytes"`
	// AfterBytes is what its live files actually add up to. On a dry run the
	// counters were not touched, so it equals BeforeBytes.
	AfterBytes int64 `json:"after_bytes"`
	// DurationMS is how long the rebuild took.
	DurationMS int64 `json:"duration_ms"`
	// DryRun reports whether the counters were left alone.
	DryRun bool `json:"dry_run"`
	// Drift lists the buckets whose counter disagrees with the file rows,
	// across every share the store serves. Only a dry run fills it: a repair
	// replaces the counters it would have been compared against.
	Drift []metadata.QuotaDrift `json:"drift,omitempty"`
}

// RecomputeShareUsage rebuilds the used-bytes counters from the metadata
// store's file rows and reports what the named share held before and after.
//
// The rebuild is a full scan of the store's file rows, which is why it runs
// only when asked. It covers every share served by that store instance, not
// only the named one: rebuilding one share's buckets alone costs the same
// scan.
//
// dryRun derives the same figures, changes nothing, and reports every usage
// bucket the counters and the file rows disagree on — the question an operator
// has before deciding to run the repair, which the repair itself destroys the
// evidence for. Run against a store taking writes it reports small transient
// deltas, because the scan and the counters are read at different instants.
//
// Returns ErrShareNotFound (wrapped) when the share is unknown.
func (r *Runtime) RecomputeShareUsage(ctx context.Context, shareName string, dryRun bool) (*UsageRecomputeResult, error) {
	mds, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	before, err := mds.GetUsedBytesForShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	drift, err := mds.RecomputeUsage(ctx, dryRun)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)

	after, err := mds.GetUsedBytesForShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	logger.Info("Share used-bytes recompute complete",
		"share", shareName,
		"beforeBytes", before,
		"afterBytes", after,
		"durationMs", elapsed.Milliseconds(),
		"dryRun", dryRun,
		"driftBuckets", len(drift),
	)

	return &UsageRecomputeResult{
		ShareName:   shareName,
		BeforeBytes: before,
		AfterBytes:  after,
		DurationMS:  elapsed.Milliseconds(),
		DryRun:      dryRun,
		Drift:       drift,
	}, nil
}
