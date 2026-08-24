package runtime

import (
	"context"

	"github.com/marmos91/dittofs/pkg/health"
)

// ShareOffline reports whether the named share could keep serving reads with
// its remote store unreachable. Returns nil when the share has no block store
// to ask — a metadata-only share, or one that is not currently served.
//
// The measurement is taken live rather than recorded by a scheduled pass: the
// answer changes with every eviction and every warm, and an operator running
// `dfsctl share warm` wants to watch the number fall, not learn about it
// tomorrow.
func (r *Runtime) ShareOffline(ctx context.Context, share string) *health.OfflineStatus {
	bs, err := r.sharesSvc.GetBlockStoreForShare(share)
	if err != nil || bs == nil {
		return nil
	}
	rd := bs.OfflineReadiness(ctx)
	return &health.OfflineStatus{
		Safe:             rd.Safe(),
		RemoteOnlyBytes:  rd.RemoteOnlyBytes,
		RemoteOnlyRanges: rd.RemoteOnlyRanges,
		Unknown:          rd.Reason,
	}
}
