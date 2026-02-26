package runtime

import (
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/mounts"
)

// MountInfo is a type alias for mounts.MountInfo so that existing
// consumers can continue using runtime.MountInfo.
type MountInfo = mounts.MountInfo

// MountTracker is a type alias for mounts.Tracker so that existing
// consumers can continue using runtime.MountTracker.
type MountTracker = mounts.Tracker

// NewMountTracker creates a new MountTracker.
// This is a convenience function that delegates to mounts.NewTracker.
func NewMountTracker() *MountTracker {
	return mounts.NewTracker()
}
