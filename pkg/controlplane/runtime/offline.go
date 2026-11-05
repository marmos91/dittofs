package runtime

import (
	"context"

	"github.com/cubbit/dittofs/pkg/health"
)

// ShareOffline reports whether the named share could keep serving reads with
// its remote store unreachable. Always nil: only a content store can answer,
// and none is wired (block.ErrNoContentStore).
func (r *Runtime) ShareOffline(_ context.Context, _ string) *health.OfflineStatus {
	return nil
}
