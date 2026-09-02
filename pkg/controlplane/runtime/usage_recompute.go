package runtime

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
)

// UsageRecomputeResult reports what a share's used-bytes repair moved.
type UsageRecomputeResult struct {
	// ShareName is the share the counter was read for.
	ShareName string `json:"share_name"`
	// BeforeBytes is the share's used bytes as reported before the rebuild.
	BeforeBytes int64 `json:"before_bytes"`
	// AfterBytes is what its live files actually add up to.
	AfterBytes int64 `json:"after_bytes"`
	// DurationMS is how long the rebuild took.
	DurationMS int64 `json:"duration_ms"`
}

// RecomputeShareUsage rebuilds the used-bytes counters from the metadata
// store's file rows and reports what the named share held before and after.
//
// The rebuild is a full scan of the store's file rows, which is why it runs
// only when asked. It covers every share served by that store instance, not
// only the named one: rebuilding one share's buckets alone costs the same
// scan.
//
// Returns ErrShareNotFound (wrapped) when the share is unknown.
func (r *Runtime) RecomputeShareUsage(ctx context.Context, shareName string) (*UsageRecomputeResult, error) {
	mds, err := r.GetMetadataStoreForShare(shareName)
	if err != nil {
		return nil, err
	}

	before, err := mds.GetUsedBytesForShare(ctx, shareName)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	if err := mds.RecomputeUsage(ctx); err != nil {
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
	)

	return &UsageRecomputeResult{
		ShareName:   shareName,
		BeforeBytes: before,
		AfterBytes:  after,
		DurationMS:  elapsed.Milliseconds(),
	}, nil
}
