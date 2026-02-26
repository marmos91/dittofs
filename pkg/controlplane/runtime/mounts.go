package runtime

import (
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/mounts"
)

// Type aliases re-exported for backward compatibility.
type (
	MountInfo    = mounts.MountInfo
	MountTracker = mounts.Tracker
)

func NewMountTracker() *MountTracker {
	return mounts.NewTracker()
}
