// Package runtime provides runtime state management for the control plane.
//
// The runtime package manages ephemeral state that is NOT persisted to the database:
//   - Live metadata store instances
//   - Share runtime state (root handles)
//   - Active mounts from NFS clients
//
// This is the "in-memory" counterpart to the persistent store package.
package runtime

import (
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// Type aliases re-exported for backward compatibility.
type (
	Share           = shares.Share
	ShareConfig     = shares.ShareConfig
	LegacyMountInfo = shares.LegacyMountInfo
)
