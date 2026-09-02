package apiclient

import (
	"fmt"
	"net/url"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// UsageRecomputeResult is the response body for RecomputeShareUsage. Mirrors
// the server-side handlers.UsageRecomputeResponse shape.
type UsageRecomputeResult struct {
	Result *runtime.UsageRecomputeResult `json:"result"`
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
