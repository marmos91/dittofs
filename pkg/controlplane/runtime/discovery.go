package runtime

import (
	"context"
	"strings"

	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

// DiscoveryNameKey is the system-settings key holding the instance-wide name
// advertised on the LAN by the service-discovery advertisers (mDNS /
// WS-Discovery). Set it with `dfsctl settings set discovery.name "<name>"`.
const DiscoveryNameKey = "discovery.name"

// DiscoveryName returns the instance-wide discovery name: the operator override
// stored under DiscoveryNameKey, or hostinfo.DefaultDiscoveryName()
// ("DittoFS-<hostname>") when unset. It is a single server identity; each
// adapter formats it for its own protocol (WS-Discovery folds it to a
// NetBIOS-legal computer name via hostinfo.NetBIOSSafe, mDNS uses it verbatim).
func (r *Runtime) DiscoveryName() string {
	if r.store != nil {
		if v, err := r.store.GetSetting(context.Background(), DiscoveryNameKey); err == nil {
			if v = strings.TrimSpace(v); v != "" {
				return v
			}
		}
	}
	return hostinfo.DefaultDiscoveryName()
}
