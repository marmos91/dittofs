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
// The counters are maintained transactionally and are correct on a store that
// has only ever run code that maintains them. This repairs one that has not:
// a share carrying bytes from a version that never released them on unlink
// reports itself fuller than it is, and since that figure gates writes through
// the share quota, it can report itself full while holding nothing.
//
// The rebuild is a full scan of the store's file rows, so this is an
// operator-invoked repair, not something the server does at startup — a
// per-file walk on every boot is a cost every share pays forever to fix a
// number that is almost always already right.
//
// It covers every share served by the same metadata store instance, not only
// the named one: they share the scan, and rebuilding one share's buckets alone
// would cost exactly the same.
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
