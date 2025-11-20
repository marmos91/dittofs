package config

import (
	"fmt"

	"github.com/marmos91/dittofs/pkg/adapter"
	"github.com/marmos91/dittofs/pkg/adapter/nfs"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// CreateAdapters creates all enabled protocol adapters from the configuration.
//
// This factory function centralizes adapter creation logic and makes it easy to:
//   - Add new protocol adapters
//   - Configure metrics for all adapters
//   - Handle adapter-specific initialization
//
// Parameters:
//   - cfg: The complete DittoFS configuration
//   - nfsMetrics: Optional NFS metrics collector (nil = no metrics)
//
// Returns:
//   - []adapter.Adapter: List of enabled adapters ready to be added to the server
//   - error: Any error during adapter creation
func CreateAdapters(cfg *Config, nfsMetrics metrics.NFSMetrics) ([]adapter.Adapter, error) {
	var adapters []adapter.Adapter

	// Create NFS adapter if enabled
	if cfg.Adapters.NFS.Enabled {
		nfsAdapter := nfs.New(cfg.Adapters.NFS, nfsMetrics)
		adapters = append(adapters, nfsAdapter)
	}

	// Future adapters can be added here:
	// if cfg.Adapters.SMB.Enabled {
	//     smbAdapter := smb.New(cfg.Adapters.SMB)
	//     adapters = append(adapters, smbAdapter)
	// }

	if len(adapters) == 0 {
		return nil, fmt.Errorf("no adapters enabled in configuration")
	}

	return adapters, nil
}
