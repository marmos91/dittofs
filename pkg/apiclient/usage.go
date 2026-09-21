package apiclient

import (
	"fmt"
	"net/url"
)

// UsageRecomputeResult is the response body for RecomputeShareUsage. Mirrors
// the server-side handlers.UsageRecomputeResponse shape.
type UsageRecomputeResult struct {
	Result *ShareUsageRecompute `json:"result"`
}

// ShareUsageRecompute reports what a share's used-bytes repair moved, or —
// for a dry run — what it would have moved. Declared here rather than reused
// from the server so a client build does not pull in the control-plane runtime.
type ShareUsageRecompute struct {
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
	// across every share the store serves. Only a dry run fills it.
	Drift []QuotaDrift `json:"drift,omitempty"`
}

// QuotaDrift is one usage bucket whose maintained counter disagrees with the
// value derived from the store's file rows. Both numbers travel so an operator
// can see how far apart they are and in which direction.
type QuotaDrift struct {
	// Share is the share the bucket belongs to.
	Share string `json:"share"`
	// Scope is "user" for a bucket keyed by owning uid, "group" for owning gid.
	Scope string `json:"scope"`
	// ID is the owning uid or gid.
	ID uint32 `json:"identity_id"`
	// Counter is what the maintained counter reports for the bucket.
	Counter UsageStat `json:"counter"`
	// Derived is what the store's file rows add up to for the bucket.
	Derived UsageStat `json:"derived"`
}

// UsageStat is the accounting pair every usage bucket holds: total logical
// bytes and inode count.
type UsageStat struct {
	Bytes int64 `json:"bytes"`
	Files int64 `json:"files"`
}

// RecomputeShareUsage rebuilds the metadata store's used-bytes counters from
// its file rows and returns the named share's figure before and after.
// The scan covers every file row in the store, so it is slow in proportion to
// the store's size and repairs every share that store serves, not only this one.
//
// With dryRun it changes nothing and reports the buckets whose counters
// disagree with the file rows instead.
func (c *Client) RecomputeShareUsage(shareName string, dryRun bool) (*UsageRecomputeResult, error) {
	path := fmt.Sprintf("/api/v1/shares/%s/usage/recompute", url.PathEscape(normalizeShareNameForAPI(shareName)))
	if dryRun {
		path += "?dry_run=true"
	}
	return createResource[UsageRecomputeResult](c, path, struct{}{})
}
