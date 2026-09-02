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

// ShareUsageRecompute reports what a share's used-bytes repair moved. Declared
// here rather than reused from the server so a client build does not pull in
// the control-plane runtime.
type ShareUsageRecompute struct {
	// ShareName is the share the counter was read for.
	ShareName string `json:"share_name"`
	// BeforeBytes is the share's used bytes as reported before the rebuild.
	BeforeBytes int64 `json:"before_bytes"`
	// AfterBytes is what its live files actually add up to.
	AfterBytes int64 `json:"after_bytes"`
	// DurationMS is how long the rebuild took.
	DurationMS int64 `json:"duration_ms"`
}

// RecomputeShareUsage rebuilds the metadata store's used-bytes counters from
// its file rows and returns the named share's figure before and after.
// The scan covers every file row in the store, so it is slow in proportion to
// the store's size and repairs every share that store serves, not only this one.
func (c *Client) RecomputeShareUsage(shareName string) (*UsageRecomputeResult, error) {
	return createResource[UsageRecomputeResult](
		c,
		fmt.Sprintf("/api/v1/shares/%s/usage/recompute", url.PathEscape(normalizeShareNameForAPI(shareName))),
		struct{}{},
	)
}
