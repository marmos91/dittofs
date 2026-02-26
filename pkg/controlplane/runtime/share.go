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

// Share is a type alias for shares.Share so that existing consumers can
// continue using runtime.Share without import changes.
type Share = shares.Share

// ShareConfig is a type alias for shares.ShareConfig so that existing
// consumers can continue using runtime.ShareConfig without import changes.
type ShareConfig = shares.ShareConfig

// LegacyMountInfo is a type alias for shares.LegacyMountInfo so that
// existing consumers can continue using runtime.LegacyMountInfo.
type LegacyMountInfo = shares.LegacyMountInfo
