package handlers

import (
	"context"
	"net"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// mountRuntime is the consumer-defined role interface for the MOUNT protocol
// handlers. It lists exactly the Runtime methods this package depends on — no
// more — so the handlers couple to the behaviour they use rather than to the
// concrete *runtime.Runtime god-object. *runtime.Runtime satisfies it
// implicitly (compile-time assertion below).
type mountRuntime interface {
	// Share + export resolution.
	GetShare(name string) (*runtime.Share, error)
	GetRootHandle(shareName string) (metadata.FileHandle, error)
	ListShares() []string
	ShareExists(name string) bool

	// Per-client export access control.
	CheckNetgroupAccess(ctx context.Context, shareName string, clientIP net.IP) (bool, error)

	// Mount tracking (DUMP / UMNT / UMNTALL).
	RecordMount(clientAddr, shareName string, mountTime int64)
	RemoveMount(clientAddr string) bool
	RemoveAllMounts() int
	ListMounts() []*runtime.LegacyMountInfo
}

var _ mountRuntime = (*runtime.Runtime)(nil)
